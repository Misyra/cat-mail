// models_outbox.go Webhook 投递 outbox（安全加固三件套，B）：通知 webhook 与调用方
// 回执统一经 outbox 落库后异步投递——重启不丢、失败指数退避重试、10 次失败转 dead。
// SQLite | PostgreSQL 同构（无方言专属类型，AutoMigrate 直建）。
package model

import "time"

// 审计动作扩展（安全加固三件套，B）：webhook 签名密钥的生成/轮换（对齐既有
// 「资源_动作」命名；secret 值绝不入审计 detail）。
const (
	AuditSettingsWebhookSecret = "settings_webhook_secret" // 通知 Webhook 签名密钥生成/轮换
	AuditKeyWebhookSecret      = "key_webhook_secret"      // 调用方回执签名密钥轮换
)

// 投递状态（WebhookDelivery.Status）。
const (
	OutboxStatusPending = "pending" // 待投递（含等待退避重试）
	OutboxStatusSent    = "sent"    // 已成功投递（2xx）
	OutboxStatusDead    = "dead"    // 重试穷尽（10 次失败），保留供排查
)

// WebhookDelivery 一条待投递/已投递的 webhook 请求档案：入队时快照 URL 与 payload，
// 投递引擎（internal/outbox）按 next_retry_at 到期捞取发送。
//
// SecretRef 存 secret 引用而非值本身（投递时按引用现取当前值签名）：
//   - "settings:notify_webhook_secret" → Setting.NotifyWebhookSecret（通知 webhook）
//   - "api_key:<id>"                    → ApiKey.WebhookSecret（调用方回执）
//
// 引用侧语义：secret 被轮换后，未投递的存量任务按新值签名（接收方持有新值则仍可
// 校验通过；持旧值则校验失败属可接受的一致性窗口）；引用失效（如 Key 被删除）→
// 置 dead 并记 last_error。空引用 = 不签名（secret 未配置，明文兼容口径）。
//
// 索引取舍：status + next_retry_at 组合索引覆盖投递引擎的到期捞取（等值 + 范围）；
// event_type 建索引供排查页按类型筛选；payload/url 不索引（大字段无检索路径）。
type WebhookDelivery struct {
	ID uint `gorm:"primaryKey"`
	// EventType 业务事件名快照（通知侧 notify.* 事件；回执侧 send_accepted/
	// send_failed；测试发送 test）。
	EventType string `gorm:"index;size:64"`
	// Payload 原始 JSON 请求体（投递时逐字节原样发送，签名也基于它）。
	Payload string `gorm:"type:text"`
	// URL 投递目标（入队时快照；管理页排查展示）。
	URL string `gorm:"size:1024"`
	// SecretRef 签名密钥引用（见结构体注释），空串 = 不签名。
	SecretRef string `gorm:"size:128;default:''"`
	// Attempts 已失败尝试次数（成功投递后不再递增；达 MaxAttempts 转 dead）。
	Attempts int
	// NextRetryAt 下次投递时间（pending 行按此到期捞取；退避节奏 1m→5m→30m→2h→8h→24h 封顶）。
	NextRetryAt time.Time `gorm:"index:idx_webhook_deliveries_status_next,priority:2"`
	Status      string    `gorm:"size:16;index:idx_webhook_deliveries_status_next,priority:1"`
	LastError   string    // 最近一次失败原因（网络错误/非 2xx/secret 引用失效）
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
