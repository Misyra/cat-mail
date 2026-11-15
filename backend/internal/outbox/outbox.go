// Package outbox 实现 Webhook 投递 outbox（安全加固三件套，B）：通知 webhook 与
// 调用方回执统一先落库（webhook_deliveries 表）再由 scheduler 周期驱动投递——
// 重启不丢任务、失败按指数退避重试（1m→5m→30m→2h→8h→24h 封顶）、10 次失败转
// dead（保留最近 100 条供排查）。投递带 X-CatMail-Signature 签名（secret 引用
// 现取现签；secret 未配置不签名不发头，契约见 httpsig 包）。
//
// 与旧「内存队列 + 1s 重试 1 次」的取舍：内存队列重启即丢且无退避；outbox 把
// 「事件 → 投递」改为两段式，入队仅一行 INSERT（毫秒级），投递完全异步化，
// 与发送主链路解耦得更彻底。
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"catmail/internal/httpsig"
	"catmail/internal/model"
	"catmail/internal/provider"
)

const (
	// MaxAttempts 投递尝试上限：第 10 次失败转 dead（任务书口径）。
	MaxAttempts = 10
	// requestTimeout 单次投递超时（与旧 notify/receipt webhook 通道一致）。
	requestTimeout = 5 * time.Second
	// claimInterval 到期行认领占用时长：捞取时先原子改写 next_retry_at 占坑，
	// 并发 worker（或进程重叠）看到 next_retry_at 未到期即跳过，防重复投递；
	// 认领后崩溃的行最迟 claimInterval 后被重新捞起（自愈）。
	claimInterval = 2 * time.Minute
	// batchSize 单轮最多投递条数：防大量积压时单轮长时间占用（cron 下一轮继续）。
	batchSize = 100
	// DeadKeep dead 行保留条数（最近），超出由 PruneDead 清理。
	DeadKeep = 100
)

// SecretRef 签名密钥引用约定值（model.WebhookDelivery.SecretRef）。
const (
	// SecretRefNotify 通知 webhook：引用 Setting.NotifyWebhookSecret。
	SecretRefNotify = "settings:notify_webhook_secret"
	// SecretRefAPIKeyPrefix 调用方回执：引用 ApiKey.WebhookSecret，"api_key:<id>"。
	SecretRefAPIKeyPrefix = "api_key:"
)

// ErrPermanent 永久故障哨兵（errors.Is 判定）：URL 非法/签名密钥引用失效等重试
// 无意义的失败，直接转 dead 不退避。
var ErrPermanent = errors.New("outbox: 永久故障")

// backoffSteps 指数退避节奏：第 n 次失败后等待 steps[min(n-1, len-1)]（24h 封顶）。
var backoffSteps = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	8 * time.Hour,
	24 * time.Hour,
}

// BackoffDelay 第 n 次失败（1 起算）后的下次重试间隔（封顶 24h）。
func BackoffDelay(attempt int) time.Duration {
	i := attempt - 1
	if i < 0 {
		i = 0
	}
	if i >= len(backoffSteps) {
		i = len(backoffSteps) - 1
	}
	return backoffSteps[i]
}

// httpClient 直连出站客户端（webhook 走全局出站，不用渠道代理），懒构建一次。
var httpClient = sync.OnceValue(func() *http.Client {
	c, err := provider.BuildClient("")
	if err != nil { // 空代理串不会走到这里，防御兜底：请求仍受 ctx 5s 超时约束
		return &http.Client{}
	}
	return c
})

// Enqueue 落库一条投递任务：payload 序列化为 JSON 存储（投递时逐字节原样发送并
// 签名）。url/secretRef 由调用方快照传入。db 为 nil（未接线，测试等）静默 no-op，
// 对齐 notify/receipt 未接线语义。入队失败返回错误——通知/回执通道故障不阻塞
// 调用方主链路，调用方按需记日志。
func Enqueue(db *gorm.DB, eventType, url, secretRef string, payload any) error {
	if db == nil {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal payload: %w", err)
	}
	row := model.WebhookDelivery{
		EventType:   eventType,
		Payload:     string(raw),
		URL:         url,
		SecretRef:   secretRef,
		Status:      model.OutboxStatusPending,
		NextRetryAt: time.Now(),
	}
	if err := db.Create(&row).Error; err != nil {
		return fmt.Errorf("outbox: enqueue: %w", err)
	}
	return nil
}

// Stats 单轮投递结果计数（scheduler 日志与测试断言用）。
type Stats struct {
	Sent    int
	Failed  int
	Dead    int
	Skipped int // 被并发轮认领走的行数
}

// dueRow 到期行最小快照（捞取 → 认领 → 发送）。
type dueRow struct {
	ID        uint
	Payload   string
	URL       string
	SecretRef string
}

// mu 单轮互斥：cron 每分钟触发 + robfig/cron 默认允许任务重叠，防两轮并发对同一
// 批行竞争（认领 UPDATE 已兜底正确性，这里避免无谓的锁竞争与重复扫描）。
var mu sync.Mutex

// ProcessDue 投递一轮到期任务：捞取 status=pending 且 next_retry_at<=now 的行
// （按 batchSize 截断）→ 逐条原子认领 → 发送 → 按结果更新。失败按 BackoffDelay
// 推进 next_retry_at，达 MaxAttempts 或永久故障转 dead；成功置 sent。
// DB 错误记日志返回零统计（投递通道故障不影响调用方主链路，对齐 notify 口径）。
func ProcessDue(db *gorm.DB) Stats {
	var st Stats
	if db == nil {
		return st
	}
	if !mu.TryLock() {
		return st // 上一轮仍在执行：本轮跳过（认领间隔自愈，不积压并发）
	}
	defer mu.Unlock()

	now := time.Now()
	var due []dueRow
	if err := db.Model(&model.WebhookDelivery{}).
		Where("status = ? AND next_retry_at <= ?", model.OutboxStatusPending, now).
		Order("id ASC").Limit(batchSize).
		Select("id", "payload", "url", "secret_ref").
		Find(&due).Error; err != nil {
		log.Printf("[outbox] 捞取到期任务失败: %v", err)
		return st
	}
	for _, row := range due {
		if !claim(db, row.ID, now) {
			st.Skipped++ // 被并发轮认领
			continue
		}
		if err := deliver(db, row); err != nil {
			st.Failed++
			if markFailure(db, row.ID, err) {
				st.Dead++
			}
			continue
		}
		st.Sent++
		markSent(db, row.ID)
	}
	if st.Sent+st.Failed > 0 {
		log.Printf("[outbox] 本轮投递: sent=%d failed=%d dead=%d skipped=%d",
			st.Sent, st.Failed, st.Dead, st.Skipped)
	}
	return st
}

// claim 原子认领：条件 UPDATE 抢占（next_retry_at 推进 claimInterval 占坑），
// RowsAffected==1 才视为本进程持有该行——与并发轮/重启后的下一轮天然互斥。
func claim(db *gorm.DB, id uint, now time.Time) bool {
	res := db.Model(&model.WebhookDelivery{}).
		Where("id = ? AND status = ? AND next_retry_at <= ?", id, model.OutboxStatusPending, now).
		Update("next_retry_at", now.Add(claimInterval))
	return res.Error == nil && res.RowsAffected == 1
}

// deliver 单条投递：secret 引用现取现签（空 secret 不签名不发头），payload 原始
// 串逐字节发送（签名与请求体一致）。URL 非法/引用失效属永久故障 → ErrPermanent。
func deliver(db *gorm.DB, row dueRow) error {
	if row.URL == "" {
		return fmt.Errorf("%w: 投递地址为空", ErrPermanent)
	}
	secret, err := ResolveSecret(db, row.SecretRef)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	headers := map[string]string{}
	if h := httpsig.Sign(secret, []byte(row.Payload), time.Now()); h != "" {
		headers[httpsig.HeaderName] = h
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	_, perr := provider.PostJSON(ctx, httpClient(), row.URL, headers, json.RawMessage(row.Payload))
	if perr == nil {
		return nil
	}
	if perr.Code == "build_request_failed" || perr.Code == "encode_failed" {
		return fmt.Errorf("%w: %s", ErrPermanent, perr.Error())
	}
	return fmt.Errorf("投递失败: %s", perr.Error())
}

// markSent 投递成功：status=sent。
func markSent(db *gorm.DB, id uint) {
	res := db.Model(&model.WebhookDelivery{}).Where("id = ?", id).Updates(map[string]any{
		"status":     model.OutboxStatusSent,
		"last_error": "",
	})
	if res.Error != nil {
		log.Printf("[outbox] 标记 sent 失败（id=%d）: %v", id, res.Error)
	}
}

// markFailure 投递失败：attempts+1；达上限或永久故障转 dead（返回 true），
// 否则按退避推进 next_retry_at 并记录 last_error（返回 false）。
func markFailure(db *gorm.DB, id uint, derr error) bool {
	var row model.WebhookDelivery
	if err := db.Select("attempts").First(&row, id).Error; err != nil {
		log.Printf("[outbox] 读取失败任务（id=%d）: %v", id, err)
		return false
	}
	attempts := row.Attempts + 1
	updates := map[string]any{
		"attempts":   attempts,
		"last_error": derr.Error(),
	}
	dead := errors.Is(derr, ErrPermanent) || attempts >= MaxAttempts
	if dead {
		updates["status"] = model.OutboxStatusDead
		updates["next_retry_at"] = time.Now()
	} else {
		updates["next_retry_at"] = time.Now().Add(BackoffDelay(attempts))
	}
	if err := db.Model(&model.WebhookDelivery{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		log.Printf("[outbox] 标记失败任务（id=%d）: %v", id, err)
	}
	if dead {
		log.Printf("[outbox] 投递转 dead（id=%d attempts=%d）: %v", id, attempts, derr)
	}
	return dead
}

// ResolveSecret 按引用解析当前签名密钥值：空引用返回空串（不签名口径）；
// 引用失效（回执 Key 已删/引用格式非法）返回错误。
func ResolveSecret(db *gorm.DB, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	switch {
	case ref == SecretRefNotify:
		var st model.Setting
		// Find（而非 First）：设置行缺失按「未配置」处理（不签名），与通知通道
		// 其余设置读取口径一致
		if err := db.Limit(1).Find(&st, 1).Error; err != nil {
			return "", fmt.Errorf("解析签名密钥引用 %s 失败: %w", ref, err)
		}
		return st.NotifyWebhookSecret, nil
	case strings.HasPrefix(ref, SecretRefAPIKeyPrefix) && len(ref) > len(SecretRefAPIKeyPrefix):
		var k model.ApiKey
		if err := db.Select("webhook_secret").First(&k, ref[len(SecretRefAPIKeyPrefix):]).Error; err != nil {
			return "", fmt.Errorf("签名密钥引用 %s 已失效（ApiKey 不存在）", ref)
		}
		return k.WebhookSecret, nil
	default:
		return "", fmt.Errorf("签名密钥引用非法: %s", ref)
	}
}

// PruneDead dead 行保留最近 keep 条（超出删旧，排查窗口有界）。keep<=0 或不足
// keep 条时不动数据；返回删除条数。id 圈界法与 notify.PruneOld 同款。
func PruneDead(db *gorm.DB, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	var boundary []uint
	if err := db.Model(&model.WebhookDelivery{}).
		Where("status = ?", model.OutboxStatusDead).
		Order("id DESC").Limit(1).Offset(keep-1).Pluck("id", &boundary).Error; err != nil {
		return 0, err
	}
	if len(boundary) == 0 {
		return 0, nil
	}
	res := db.Where("id < ? AND status = ?", boundary[0], model.OutboxStatusDead).Delete(&model.WebhookDelivery{})
	return res.RowsAffected, res.Error
}
