// Package notify 实现发送失败/渠道异常通知的基础设施（双通道）：
// 站内通知中心（Notification 落库）与通用 Webhook（按域名适配
// 飞书/钉钉/企业微信/Slack 机器人，其余发通用 JSON，见 webhook.go）。
//
// Webhook 通道（安全加固三件套，B）：原「内存队列 + 失败 1s 重试 1 次」替换为
// outbox 落库投递（internal/outbox）——事件入队即落 webhook_deliveries 表，
// 由 scheduler 周期驱动异步投递，重启不丢、指数退避、10 次失败转 dead；带
// X-CatMail-Signature 签名（Setting.NotifyWebhookSecret，未配置不签名不发头）。
//
// 使用方式：NewNotifier(db) → SetDefault；rotator/scheduler 等经
// 包级 notify.Emit(...) 发射事件。未 SetDefault 时 Emit 为 no-op，未接线也不 panic。
package notify

import (
	"log"
	"strings"
	"sync/atomic"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/outbox"
)

// 事件类型（Notification.Event）。
const (
	EventChannelDisabled  = "channel_disabled"  // 渠道连续失败/平台 401/403 自动禁用
	EventChannelCooldown  = "channel_cooldown"  // 渠道 429 触发冷却
	EventChannelRecovered = "channel_recovered" // 渠道恢复参与轮换（手动启用/冷却释放）
	EventSendFailed       = "send_failed"       // 一次请求重试穷尽后仍失败
	EventUpdateAvailable  = "update_available"  // 自动更新启动检查发现新版本（E8，info 级）
	// 渠道退信率超阈值（送达链路三件套，warn 级）：近 7 天退信率超过
	// Setting.BounceRateThreshold（默认 5%）时由入站事件摄取处广播；
	// 开关走 notify_events 事件白名单（设置页可关）。
	EventBounceRate = "bounce_rate_exceeded"
)

// 通知级别（Notification.Level）。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// EventEnabled 判断 event 是否被事件白名单放行：eventsCSV 为 Setting.NotifyEvents
// 的逗号分隔事件子集，空串（或仅空白）=全部事件开启；不在列表内的事件一律关闭。
func EventEnabled(eventsCSV, event string) bool {
	eventsCSV = strings.TrimSpace(eventsCSV)
	if eventsCSV == "" {
		return true
	}
	for _, e := range strings.Split(eventsCSV, ",") {
		if strings.TrimSpace(e) == event {
			return true
		}
	}
	return false
}

// Notifier 通知发射器：Emit 内同步站内落库 + webhook 通道 outbox 落库投递
// （投递由 scheduler 周期驱动，见 internal/outbox）。
type Notifier struct {
	db *gorm.DB
}

// NewNotifier 创建通知发射器。
func NewNotifier(db *gorm.DB) *Notifier {
	return &Notifier{db: db}
}

// defaultNotifier 进程级默认发射器（rotator/scheduler 等经包级 Emit 使用）。
var defaultNotifier atomic.Pointer[Notifier]

// SetDefault 设置进程级默认发射器；传 nil 可解除，包级 Emit 随即回到 no-op。
func SetDefault(n *Notifier) { defaultNotifier.Store(n) }

// Default 取进程级默认发射器；未设置时返回 nil。
func Default() *Notifier { return defaultNotifier.Load() }

// Emit 包级便捷入口：经默认发射器发射通知，未 SetDefault 时为 no-op。
func Emit(event, level, title, body string, channelID *uint) {
	Default().Emit(event, level, title, body, channelID)
}

// Emit 发射一条通知事件：立即按当前 Setting 判定事件白名单与通道开关——
// 站内通道同步写 Notification 行；webhook 通道构造 payload 后经 outbox 落库
// （签名与退避投递见 internal/outbox）。绝不 panic、毫秒级返回：DB 错误只记
// 日志，通知故障不阻塞发送主链路。nil 接收者（未 SetDefault 的包级 Emit）为 no-op。
func (n *Notifier) Emit(event, level, title, body string, channelID *uint) {
	defer func() { // 兜底：任何 panic（驱动异常等）都不外泄到发送主链路
		if r := recover(); r != nil {
			log.Printf("[notify] Emit panic（已忽略）: %v", r)
		}
	}()
	if n == nil || n.db == nil {
		return
	}
	var s model.Setting
	// Find（而非 First）：设置行缺失不报错，零值开关=通知关闭，直接返回
	if err := n.db.Limit(1).Find(&s, 1).Error; err != nil {
		log.Printf("[notify] 读取设置失败，丢弃事件 %s: %v", event, err)
		return
	}
	if !EventEnabled(s.NotifyEvents, event) {
		return
	}
	if s.NotifyInappEnabled {
		row := model.Notification{Event: event, Level: level, Title: title, Body: body, ChannelID: channelID}
		if err := n.db.Create(&row).Error; err != nil {
			log.Printf("[notify] 站内通知落库失败: %v", err)
		}
	}
	if s.NotifyWebhookEnabled && s.NotifyWebhookURL != "" {
		if err := enqueueWebhook(n.db, s.NotifyWebhookURL, event, level, title, body, channelID); err != nil {
			log.Printf("[notify] webhook 任务入队失败（事件 %s）: %v", event, err)
		}
	}
}

// enqueueWebhook 构造 host 适配 payload 并经 outbox 落库（secret 引用指向
// Setting.NotifyWebhookSecret，投递时现取现签；未配置则不签名）。URL 非法属
// 配置错误：记日志丢弃（与旧 deliver 对畸形 URL 的口径一致）。
func enqueueWebhook(db *gorm.DB, targetURL, event, level, title, body string, channelID *uint) error {
	payload, err := buildPayload(targetURL, event, level, title, body, channelID)
	if err != nil {
		log.Printf("[notify] webhook 地址非法 %q，丢弃事件 %s: %v", targetURL, event, err)
		return nil // URL 畸形已在设置写入时校验拦截，此处为兜底，不重试不落 outbox
	}
	return outbox.Enqueue(db, event, targetURL, outbox.SecretRefNotify, payload)
}

// PruneOld 清理历史通知，仅保留最近 keep 条，供定时任务调用。
// keep<=0 或通知不足 keep 条时不动任何数据；返回删除条数。
// 保留边界按 id 倒序圈定：自增 id 与 created_at 单调一致，无需 created_at 索引。
func PruneOld(db *gorm.DB, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	var boundary []uint // 第 keep 条新通知的 id：其之前的全部删除，恰好保留 keep 条
	if err := db.Model(&model.Notification{}).
		Order("id DESC").Limit(1).Offset(keep-1).Pluck("id", &boundary).Error; err != nil {
		return 0, err
	}
	if len(boundary) == 0 {
		return 0, nil // 不足 keep 条
	}
	res := db.Where("id < ?", boundary[0]).Delete(&model.Notification{})
	return res.RowsAffected, res.Error
}
