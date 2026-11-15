// Package receipt 实现调用方 Webhook 回执（D1）：发送受理成功（send_accepted）与
// 终态失败（send_failed）时，按调用方 ApiKey 上显式配置的 webhook_url 推送固定
// 格式 JSON 回执（{"source","event","api_key_id","data"}），供调用方无人值守的
// 自动化归档——同步 API 响应已含结果，回执仅为补充，推送失败只记日志，绝不影响
// 发送主链路。与 notify 包（管理员侧失败/渠道告警）完全独立：实例、事件、payload
// 格式均不复用。
//
// 投递通道（安全加固三件套，B）：原「内存队列 + 失败 1s 重试 1 次」替换为 outbox
// 落库投递（internal/outbox）——回执入队即落 webhook_deliveries 表，由 scheduler
// 周期驱动异步投递，重启不丢、指数退避、10 次失败转 dead；带 X-CatMail-Signature
// 签名（ApiKey.WebhookSecret，创建 Key 自动生成；为空不签名不发头）。
//
// 使用方式：NewDispatcher(db) → SetDefault；rotator 在受理成功/终态失败处经包级
// receipt.EmitAccepted / EmitFailed 触发。未 SetDefault 时为 no-op，不接线也不 panic。
package receipt

import (
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/outbox"
)

// Source 回执 payload 的固定来源标识（envelope.source）。
const Source = "catmail"

// 事件类型（ApiKey.WebhookEvents 白名单 token）。
const (
	EventSendAccepted = "send_accepted" // 平台受理成功（终态，不会改判失败）
	EventSendFailed   = "send_failed"   // 终态失败（429 额度/限速、502 候选穷尽）
)

// Events 全部合法回执事件名（API 层校验 webhook_events 白名单与前端复选框共用）。
var Events = []string{EventSendAccepted, EventSendFailed}

// ValidEvent 判断 event 是否为合法回执事件名。
func ValidEvent(event string) bool {
	for _, e := range Events {
		if e == event {
			return true
		}
	}
	return false
}

// AcceptedData send_accepted 回执 data：平台受理成功快照（accepted_at RFC3339）。
type AcceptedData struct {
	MessageID         string   `json:"message_id"`
	Provider          string   `json:"provider"`
	Channel           string   `json:"channel"`
	ProviderMessageID string   `json:"provider_message_id"`
	To                []string `json:"to"`
	Subject           string   `json:"subject"`
	LatencyMs         int64    `json:"latency_ms"`
	AcceptedAt        string   `json:"accepted_at"` // RFC3339
}

// FailedData send_failed 回执 data：终态失败快照。参数类 400 不推——调用方在
// 同步响应里当场已见错误，无需异步补发。
type FailedData struct {
	MessageID string `json:"message_id"`
	Error     string `json:"error"`
	Tried     int    `json:"tried"`
	FailedAt  string `json:"failed_at"` // RFC3339
}

// envelope 回执统一外层格式（字段名按契约固定，调用方据此解析）。
// 注：这是调用方自动化归档用途——同步 API 响应已含结果，回执方便无人值守管道。
type envelope struct {
	Source   string `json:"source"` // 恒为 "catmail"
	Event    string `json:"event"`  // send_accepted | send_failed | test
	APIKeyID uint   `json:"api_key_id"`
	Data     any    `json:"data"`
}

// Dispatcher 调用方回执发射器：emit 内同步读 ApiKey 判定 opt-in 与事件白名单
// （单行主键查询，毫秒级，与 notify.Emit 读 Setting 同风格），命中则经 outbox
// 落库投递（签名与退避见 internal/outbox）。
type Dispatcher struct {
	db *gorm.DB
}

// NewDispatcher 创建回执发射器。
func NewDispatcher(db *gorm.DB) *Dispatcher {
	return &Dispatcher{db: db}
}

// defaultDispatcher 进程级默认发射器（rotator 经包级 Emit* 使用）。
var defaultDispatcher atomic.Pointer[Dispatcher]

// SetDefault 设置进程级默认发射器；传 nil 可解除，包级 Emit* 随即回到 no-op。
func SetDefault(d *Dispatcher) { defaultDispatcher.Store(d) }

// Default 取进程级默认发射器；未设置时返回 nil。
func Default() *Dispatcher { return defaultDispatcher.Load() }

// EmitAccepted 包级便捷入口：推送 send_accepted 回执，未 SetDefault 时 no-op。
// AcceptedAt 为空时取当前时刻（RFC3339）。
func EmitAccepted(apiKeyID uint, data AcceptedData) {
	if data.AcceptedAt == "" {
		data.AcceptedAt = time.Now().Format(time.RFC3339)
	}
	Default().emit(apiKeyID, EventSendAccepted, data)
}

// EmitFailed 包级便捷入口：推送 send_failed 回执，未 SetDefault 时 no-op。
// FailedAt 为空时取当前时刻（RFC3339）。
func EmitFailed(apiKeyID uint, data FailedData) {
	if data.FailedAt == "" {
		data.FailedAt = time.Now().Format(time.RFC3339)
	}
	Default().emit(apiKeyID, EventSendFailed, data)
}

// emit 单实例发射：读 ApiKey 判定回执 opt-in（WebhookURL 非空）与事件白名单
// （WebhookEvents 显式列出该事件；空串=未 opt-in 任何事件=不推送），命中才经
// outbox 落库（secret 引用指向该 Key 的 WebhookSecret，投递时现取现签）。
// 绝不 panic、毫秒级返回：读库失败/Key 不存在只记日志，回执故障不外泄、不阻塞
// 发送主链路。
func (d *Dispatcher) emit(apiKeyID uint, event string, data any) {
	defer func() { // 兜底：任何 panic（驱动异常等）都不外泄到发送主链路
		if r := recover(); r != nil {
			log.Printf("[receipt] emit panic（已忽略）: %v", r)
		}
	}()
	if d == nil || d.db == nil || apiKeyID == 0 {
		return
	}
	if !ValidEvent(event) {
		return
	}
	var k model.ApiKey
	// Key 行不存在/读库失败：回执是尽力而为的附加能力，放弃并记日志
	if err := d.db.First(&k, apiKeyID).Error; err != nil {
		log.Printf("[receipt] 读取 ApiKey %d 失败，丢弃回执 %s: %v", apiKeyID, event, err)
		return
	}
	if k.WebhookURL == "" || !eventEnabled(k.WebhookEvents, event) {
		return
	}
	payload := envelope{Source: Source, Event: event, APIKeyID: apiKeyID, Data: data}
	if err := outbox.Enqueue(d.db, event, k.WebhookURL,
		outbox.SecretRefAPIKeyPrefix+strconv.FormatUint(uint64(apiKeyID), 10), payload); err != nil {
		log.Printf("[receipt] 回执任务入队失败（key=%d event=%s）: %v", apiKeyID, event, err)
	}
}

// eventEnabled 调用方事件白名单判定：csv 子集逐项匹配（项内空白容忍）。
// 与 notify 的全局白名单语义不同（空=全部放行），Key 的 WebhookEvents
// 空=未 opt-in 任何事件=不推送。
func eventEnabled(eventsCSV, event string) bool {
	for _, e := range strings.Split(eventsCSV, ",") {
		if strings.TrimSpace(e) == event {
			return true
		}
	}
	return false
}
