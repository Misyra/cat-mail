// models_events.go 送达链路三件套的 GORM 模型（新模型放新文件，不动 models.go 中的
// 既有结构体）：mail_events（入站送达事件统一存档）与 suppressions（收件人抑制列表）。
// 两个新表均 SQLite | PostgreSQL 同构（无方言专属类型，AutoMigrate 直建）。
package model

import "time"

// 送达事件类型（MailEvent.Event）：归一化后的统一事件名；平台原始事件名进
// Detail/Raw 存档，识别不了的一律 raw 存档不猜测。
const (
	MailEventSent       = "sent"       // 平台受理（可选上报）
	MailEventDelivered  = "delivered"  // 送达
	MailEventBounced    = "bounced"    // 退信（hard/soft 记 BounceType）
	MailEventComplained = "complained" // 投诉（被标记为垃圾邮件）
	MailEventRaw        = "raw"        // 其他类型（opened/clicked/deferred/…）：raw 存档
)

// 退信类型（MailEvent.BounceType，仅 bounced 事件填写）。
const (
	BounceTypeHard = "hard"
	BounceTypeSoft = "soft"
)

// 抑制原因（Suppression.Reason）。
const (
	SuppressReasonBounce    = "bounce"    // hard bounce 事件自动入库
	SuppressReasonComplaint = "complaint" // complained 事件自动入库
	SuppressReasonManual    = "manual"    // 管理端手动添加
)

// 审计动作扩展（送达链路三件套，前缀着色沿用 model.AuditLog 的前缀口径）。
const (
	AuditChannelWebhookToken = "channel_webhook_token" // 渠道入站 webhook token 补生成/轮换
	AuditSuppressionCreate   = "suppression_create"    // 抑制列表手动添加
	AuditSuppressionDelete   = "suppression_delete"    // 抑制列表解除
)

// MailEvent 入站送达事件（公开 webhook 摄取，统一归一化后存档）。
//
// 关联口径：ProviderMessageID 存平台报文中的原始消息 ID；摄取时尽力按
// provider_message_id（去尖括号比对）反查 SendLog，命中则回填 MessageID
// （=SendLog.MessageID，贯穿日志页）与 SendLogID；查不到（日志已清理/测试发送/
// 平台未回传 ID）保持空串/0，事件照常存档。
//
// 索引取舍：channel_id + created_at 建索引——送达事件页按渠道筛选与 7 天健康度
// 聚合（窗口过滤 + GROUP BY channel_id）走索引；provider_message_id 建索引供
// 摄取时反查 SendLog；raw 大字段不索引。
type MailEvent struct {
	ID                uint      `gorm:"primaryKey"`
	ChannelID         uint      `gorm:"index"`
	ChannelName       string    // 冗余快照（已删渠道的历史事件仍可展示）
	Provider          string    // 冗余快照
	Event             string    // 归一化事件类型（MailEvent* 常量）
	BounceType        string    // hard|soft（仅 bounced 事件；其余空串）
	ProviderMessageID string    `gorm:"index;size:255"` // 平台消息 ID（原始形态）
	MessageID         string    `gorm:"size:64"`        // 关联 SendLog.MessageID（尽力解析，空=未解析）
	SendLogID         uint      // 关联 SendLog.ID（0=未解析）
	Recipient         string    `gorm:"size:320"` // 收件人（事件报文中通常为单个）
	Detail            string    // 事件补充：退信原因/诊断码/原始事件名等（截 500 字符）
	Raw               string    `gorm:"type:text"` // 原始报文 JSON（form 编码转 JSON 存档）
	OccurredAt        time.Time // 平台事件时间（报文缺省时取摄取时刻）
	CreatedAt         time.Time `gorm:"index"` // 摄取时间（7 天健康度窗口过滤口径）
}

// Suppression 收件人抑制列表：命中即拒绝发送（rotator 发送主链路统一拦截）。
// hard bounce 与 complained 事件自动入库（Setting.SuppressionAutoEnabled，默认开），
// 管理端可手动添加/解除。Email 全局唯一——抑制是收件人维度的（跨渠道生效）：
// 同一地址在任何渠道都退信过，就不再向它发送；首次入库的来源渠道记 Channel* 快照。
type Suppression struct {
	ID          uint   `gorm:"primaryKey"`
	Email       string `gorm:"uniqueIndex;size:320"` // 收件人（小写归一）
	Reason      string // bounce|complaint|manual（SuppressReason* 常量）
	ChannelID   uint   // 首次命中的来源渠道（0=手动添加未指定渠道）
	ChannelName string // 冗余快照
	Detail      string // 补充说明：退信原因/添加备注（截 500 字符）
	CreatedAt   time.Time
}
