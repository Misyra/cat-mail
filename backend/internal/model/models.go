// Package model 定义全部 GORM 数据模型（SPEC §4）。
package model

import "time"

// 渠道状态（Channel.Status）。
const (
	ChannelEnabled        = 0 // 正常参与轮换
	ChannelManualDisabled = 1 // 手动停用
	ChannelAutoDisabled   = 2 // 连续失败/平台 401/403 自动禁用
	ChannelCooldown       = 3 // 429 触发冷却
)

// 发送日志状态（SendLog.Status）。
const (
	SendStatusPending = 0
	SendStatusSent    = 1
	SendStatusFailed  = 2
)

// User 管理后台账号（登录用户均为管理员角色）。
type User struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"uniqueIndex;size:64"`
	PasswordHash string // bcrypt
	// session_epoch 按用户会话纪元（O10，取代原 Setting.SessionEpoch 全局纪元）：
	// 本人改密/被重置密码成功后同事务 +1，该用户携带旧纪元的全部会话立即失效；
	// 当前会话由 changeOwnPassword 重签 cookie 保持登录。存量库 AutoMigrate 补列
	// 时旧行取默认 0，与升级后首轮签发 token 的纪元一致，无需回填。
	SessionEpoch int `gorm:"default:0"`
	// 两步验证（C4，SPEC §5）：totp_secret 为 base32 密钥（setup 生成，enable
	// 校验通过前置位）；recovery_codes 为 JSON 数组，存每个恢复码的 sha256 hex
	// （明文仅 enable 响应一次性返回，不落明文、不进任何 GET 响应与日志）。
	// 全部带 gorm 默认值：存量库 AutoMigrate 补列时旧行取零值（空串/false），
	// 语义即「未启用两步验证」，属安全默认，无需回填。
	TOTPSecret    string `gorm:"default:''"`
	TOTPEnabled   bool   `gorm:"default:false"`
	RecoveryCodes string `gorm:"default:''"`
	CreatedAt     time.Time
}

// Setting 全局设置，单行（ID 恒为 1）。
type Setting struct {
	ID                          uint   `gorm:"primaryKey"`
	Rotation                    string // priority | round_robin
	MaxAttempts                 int
	CooldownSeconds             int
	ConsecutiveFailureThreshold int
	LogRetentionDays            int    // 0=永久保留
	SessionSecret               string // 首启随机生成 32B hex
	// 通知设置（发送失败/渠道异常双通道：站内通知中心 + 通用 Webhook）。
	// 全部带 gorm 默认值：存量库 AutoMigrate 补列时旧行取默认零值（false/空），
	// 语义即「通知关闭/未配置」，属安全默认，无需回填。
	NotifyInappEnabled   bool   `gorm:"default:false"` // 站内通知中心开关
	NotifyWebhookEnabled bool   `gorm:"default:false"` // Webhook 通知开关
	NotifyWebhookURL     string `gorm:"default:''"`    // Webhook 地址（空=未配置）
	NotifyEvents         string `gorm:"default:''"`    // 事件白名单，逗号分隔；空=全部事件
	// 记录邮件正文开关（A4）：false 时新落库的 SendLog（含测试日志）HTMLBody/TextBody
	// 存空串，to/subject/from/错误/耗时等元数据照常记录。只影响新日志：存量已落
	// 正文的历史行原样保留——刻意取舍，关闭开关不承诺抹除历史。默认 true=保持
	// 现状：存量库 AutoMigrate 补列时旧行取默认 true，升级后行为不变，无需回填。
	LogBodyEnabled bool `gorm:"default:true"`
	// 每日自动备份开关（A2）：true 时 scheduler 每小时任务检查「今天尚未备份」
	// 并自动执行一次 VACUUM INTO 整库备份（保留最近 10 份）。带 gorm 默认值：
	// 存量库 AutoMigrate 补列时旧行取 false，语义即「保持现状不自动备份」，
	// 属安全默认，无需回填。
	BackupDailyEnabled bool `gorm:"default:false"`
	// 抑制列表自动入库开关（送达链路三件套）：true 时 hard bounce 与 complained
	// 事件自动写入 suppressions。按任务书默认开——gorm default 对存量库新增列
	// 同样生效（旧行补列取 true），升级即获得抑制防护，无需回填。
	SuppressionAutoEnabled bool `gorm:"default:true"`
	// 退信率告警阈值（百分比 0-100）：渠道近 7 天退信率超阈值经 notify 广播
	// bounce_rate_exceeded warn 事件（事件开关走 notify_events 白名单）；0=关闭
	// 告警。默认 5（任务书口径：默认 5%）。
	BounceRateThreshold float64 `gorm:"default:5"`
	// 通知 Webhook 签名密钥（安全加固三件套，B）：32 字节随机数的 hex，通知
	// webhook 投递时据此生成 X-CatMail-Signature（HMAC-SHA256）；空串 = 未配置
	// → 不签名、不发签名头（调用方校验示例须注明「配置 secret 后才有签名头」）。
	// 由设置页「通知」卡片生成/轮换（POST /api/settings/notify/webhook-secret），
	// PUT /api/settings 不受理该字段（防误清）。带 gorm 默认值：存量库补列旧行
	// 取空串，语义即「未配置不签名」，属安全默认，无需回填。
	NotifyWebhookSecret string `gorm:"default:''"`
	UpdatedAt           time.Time
}

// ApiKey 对外发送 API 的 Bearer Key，形如 "cm-" + 32 位随机串。
type ApiKey struct {
	ID      uint `gorm:"primaryKey"`
	Name    string
	Key     string `gorm:"uniqueIndex;size:64"`
	Enabled bool
	// 调用方 Webhook 回执（D1）：显式 opt-in，默认零值（空）= 不推送。
	// WebhookURL 为 http(s):// 地址；WebhookEvents 为事件白名单 csv 子集
	// （send_accepted|send_failed，空=不推送）。两者均带 gorm 默认值：存量库
	// AutoMigrate 补列时旧行取空串，语义即「未配置不推送」，属安全默认，无需回填。
	// 与 notify 包（管理员侧失败告警）完全独立，互不复用。
	WebhookURL    string `gorm:"default:''"`
	WebhookEvents string `gorm:"default:''"`
	// 调用方维度配额与限速（B5）：daily_quota 为该 Key 每日发送成功条数上限
	// （按本地日历日零点起算，status=sent 口径，与统计聚合一致；强制点在发送
	// 入口鉴权，见 api 层 enforceKeyPolicy）；rate_per_sec 为发送接口令牌桶
	// 限速（封/秒，进程内按真实时间补充）。两者均 0=不限，带 gorm 默认值：
	// 存量库 AutoMigrate 补列时旧行取 0，语义即「不限额不限速」，升级后行为
	// 不变，无需回填。
	DailyQuota int `gorm:"default:0"`
	RatePerSec int `gorm:"default:0"`
	// 回执签名密钥（安全加固三件套，B）：创建 Key 自动生成（32 字节随机 hex），
	// 管理页可查看/复制、可经 POST /api/keys/:id/webhook-secret 轮换；回执请求
	// 据此生成 X-CatMail-Signature（HMAC-SHA256）。空串 = 不签名不发签名头
	// （与通知 webhook secret 同口径）。带 gorm 默认值：存量库补列旧行取空串，
	// 语义即「不签名」，属安全默认，无需回填。
	WebhookSecret string `gorm:"default:''"`
	// 创建者用户名（安全加固三件套，C，吊销联动归属）：createKey 时记录操作者。
	// 管理员重置某用户密码时，联动停用 created_by=该用户的全部 Key（enabled=false
	// + disabled_reason=password_reset）。存量库补列旧行取空串 = 无主 Key，不受
	// 联动影响（升级前创建的 Key 归属未知，宁窄勿宽不误伤），可在管理页手动启用。
	CreatedBy string `gorm:"size:64;default:''"`
	// 联动停用原因（安全加固三件套，C）：password_reset = 密码重置联动停用
	// （管理页据此展示「密码重置后停用，可手动启用」徽标）；手动启用时清空；
	// 空串 = 常规停用（无联动提示）。带 gorm 默认值：存量库补列旧行取空串，无需回填。
	DisabledReason string `gorm:"default:''"`
	LastUsedAt     *time.Time
	CreatedAt      time.Time
}

// Channel 发送渠道（一个平台账号一条记录）。
//
// 凭据静态加密（安全加固三件套）：APIKey/APISecret/SMTPPass/ProxyURL/WebhookSecret
// 五个敏感字段挂 gorm serializer "encsecret"（AES-256-GCM，密钥 CATMAIL_SECRET_KEY），
// 读写路径在模型访问层透明加解密（secretcrypt 包），业务代码无感知。明文兼容：未配置
// 密钥时原样读写；已配置时存量明文由启动期一次性迁移转密文（store.EnsureChannelEncryption，
// 幂等）。ProxyURL 虽非纯凭据但可内嵌 user:pass@ 代理账号，一并加密（逐字段核对口径）。
type Channel struct {
	ID                   uint   `gorm:"primaryKey"`
	Name                 string `gorm:"uniqueIndex;size:128"`
	Provider             string // resend|brevo|mailjet|smtp2go|mailgun|smtp|mock
	Status               int    // 0=enabled 1=manual_disabled 2=auto_disabled 3=cooldown
	Priority             int    // 越小越优先
	APIKey               string `gorm:"serializer:encsecret"` // brevo/mailjet(public)/mailgun/resend/smtp2go 共用
	APISecret            string `gorm:"serializer:encsecret"` // mailjet private key
	Domain               string // mailgun 发送域名
	Region               string // mailgun: "us"(默认)|"eu"
	ProxyURL             string `gorm:"serializer:encsecret"` // 出站代理 http:// 或 socks5://（smtp 仅 socks5）；空=直连（可内嵌代理凭据）
	AllowedDomains       string // 允许的发件域名，逗号分隔；空=不限制
	FromEmail            string // 渠道默认发件人（smtp 渠道必填）
	FromName             string
	SMTPHost             string
	SMTPPort             int
	SMTPUser             string
	SMTPPass             string `gorm:"serializer:encsecret"` // SMTP 密码
	SMTPTLS              string // none|starttls|tls
	DailyLimitOverride   int    // 0=用 provider 默认
	MonthlyLimitOverride int    // 0=用 provider 默认
	// 出站限速（封/秒，渠道级令牌桶，桶容量=max(1,rate)，进程内按真实时间补充）；
	// 0=不限。默认 0：存量库 AutoMigrate 补列时旧行取 0，语义即不限速，无需回填。
	RateLimitPerSec     int `gorm:"default:0"`
	UsedDay             int
	DayAnchor           string // YYYY-MM-DD
	UsedMonth           int
	MonthAnchor         string // YYYY-MM
	CooldownUntil       *time.Time
	ConsecutiveFailures int
	LastError           string
	AutoDisabledReason  string
	LastUsedAt          *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	// 送达事件入站 webhook（送达链路三件套）：WebhookToken 为每渠道独立的 URL
	// token（公开入站端点 /webhooks/mail-events/:token 据此定位渠道；对无签名的
	// 平台兼作共享 secret），创建/复制渠道时自动生成，可经管理端点轮换；
	// WebhookSecret 存平台侧签名密钥（Resend 的 whsec_ 签名串 / Mailgun 的
	// Webhook Signing Key），空串=该渠道仅按 URL token 校验。两者带 gorm 默认值：
	// 存量库补列旧行取空串（token 为空 → 入站端点查不到渠道，需在管理端轮换
	// 生成），属安全默认。token 用普通索引而非唯一索引：存量库多行空串会令
	// unique 迁移失败；唯一性由生成侧保证（256bit 随机，碰撞不可行）。
	WebhookToken  string `gorm:"index;size:64;default:''"`
	WebhookSecret string `gorm:"serializer:encsecret;default:''"` // 平台侧签名密钥（Resend whsec_/Mailgun signing key），同属凭据
}

// SendLog 每次发送请求的日志（含正文快照）。
// created_at/channel_id/status/api_key_id 建索引：日志列表筛选、统计聚合与
// Key 累计发送（total_sent 按 api_key_id 聚合）走索引（SPEC §8）；
// (latency_ms, created_at, id) 组合索引供耗时排序免全表扫描（取舍见字段注释）。
type SendLog struct {
	ID          uint   `gorm:"primaryKey;index:idx_send_logs_latency,priority:3"`
	MessageID   string `gorm:"index;size:64"` // 内部 uuid，贯穿一次请求
	ApiKeyID    uint   `gorm:"index"`
	ChannelID   uint   `gorm:"index"`
	ChannelName string // 冗余快照
	Provider    string // 冗余快照
	// 所用邮件模板 ID（邮件模板功能包）：发送请求携带 template_id 时记录（渲染
	// 结果替换 subject/html/text 后随本日志快照落库），0 = 未使用模板。模板删除后
	// 历史日志仍保留该 ID 供追溯；名称不冗余存储，前端经模板列表接口解析展示。
	TemplateID        uint `gorm:"default:0"`
	IsTest            bool
	Status            int `gorm:"index"` // 0=pending 1=sent 2=failed
	FromAddr          string
	ToAddrs           string // JSON 数组
	Subject           string
	HTMLBody          string // 正文落库（用户已确认）
	TextBody          string
	ProviderMessageID string
	Error             string
	Attempts          int
	// (latency_ms, created_at, id) 组合索引（L5 性能采纳）：日志页「按耗时排序」
	// （慢请求排查）此前无索引可用，排序需全表物化——L5 实测 10 万行量级扫描行差
	// 约 7 倍，建索引后排序直接走索引。写放大可接受：发送日志为低频写入路径，每行
	// 仅多维护一个索引条目；created_at 次序键提供同耗时的确定性 tie-break，尾随
	// id 让「耗时 + 时间」排序查询可覆盖扫描免回表。created_at 单列索引保留：
	// 保留期清理按 created_at 范围圈定，组合索引（latency 在前）服务不了该路径。
	LatencyMs int64     `gorm:"index:idx_send_logs_latency,priority:1"`
	CreatedAt time.Time `gorm:"index;index:idx_send_logs_latency,priority:2"`
}

// 幂等档案状态（IdempotencyRecord.Status）。
const (
	IdemStatusPending = "pending" // 已认领、发送进行中（或进程崩溃残留）
	IdemStatusDone    = "done"    // 响应已存档，同 key 重放直接返回存档
)

// IdempotencyRecord 幂等键防重发档案（E2）：可选请求头 Idempotency-Key 驱动的
// 发送去重，作用域绑定 ApiKeyID——不同调用方同 key 互不干扰。语义与状态机
// 见 api 层 handler_idempotency.go；失败响应（400/429/502）同样存档重放，
// 调用方带同 key 重试拿到同一错误而非触发二次发送（幂等语义的明示取舍）。
//
// 单表量级：每行对应「一个调用方的一次带 key 发送」，24h TTL 懒惰清理
// （仅带 key 的请求顺手 DELETE 过期行，不依赖 scheduler），稳态行数≈24h 内
// 带 key 发送量，远小于 SendLog，无需额外索引分区或定期任务。
type IdempotencyRecord struct {
	ID uint `gorm:"primaryKey"`
	// (api_key_id, key) 唯一：同一调用方同 key 仅一条档案；并发首投由唯一索引
	// 兜底（Create 冲突 → 重读走重放/409 分支）。
	ApiKeyID uint   `gorm:"uniqueIndex:idx_idempotency_records_api_key_id_key,priority:1"`
	Key      string `gorm:"uniqueIndex:idx_idempotency_records_api_key_id_key,priority:2;size:128"`
	Status   string // pending | done（常量 IdemStatus*）
	// 响应存档（done 后重放逐字节一致）：status=0/body 为空即尚未存档。
	ResponseStatus int
	ResponseBody   string
	MessageID      string // 成功响应的内部 uuid（错误路径为空），仅审计/排查用
	// 认领基准时间：插入或僵尸回收时刷新，存档不改写——TTL 清理、僵尸 pending
	// （>5min 视为崩溃残留）判定均以此为锚，建索引供清理 DELETE 走索引。
	CreatedAt time.Time `gorm:"index"`
}

// 审计动作枚举（AuditLog.Action，E4）：前缀标识资源类别（channel/key/user/
// settings/purge/backup），后缀标识动作；前端按前缀着色（见 AuditActionBadge）。
const (
	AuditChannelCreate     = "channel_create"
	AuditChannelUpdate     = "channel_update"
	AuditChannelDelete     = "channel_delete"
	AuditChannelStatus     = "channel_status"
	AuditChannelDuplicate  = "channel_duplicate"
	AuditKeyCreate         = "key_create"
	AuditKeyUpdate         = "key_update"
	AuditKeyDelete         = "key_delete"
	AuditUserCreate        = "user_create"
	AuditUserResetPassword = "user_reset_password"
	AuditUserDelete        = "user_delete"
	AuditSettingsUpdate    = "settings_update"
	AuditPurgeLogs         = "purge_logs"
	AuditBackupCreate      = "backup_create"
	AuditBackupDelete      = "backup_delete"
)

// AuditLog 管理操作审计日志（E4）：管理面写操作成功后追加一条「谁在何时从哪个
// IP 对什么目标做了什么」。只增不改不删（应用层；裁剪由 scheduler 按条数上限）。
//
// 索引取舍：created_at 建索引——审计页主路径是「created_at 倒序分页」（无筛选时
// 排序直走索引）；username 建索引——按操作者筛选是审计的第二常用维度（等值匹配，
// 区分度尚可）。action 不建索引：动作枚举基数极低（≤15 个取值）单独索引区分度差，
// 且按动作筛选属低频运维操作；本表有 5000 条硬上限（scheduler 裁剪），全表扫描
// 的绝对代价有界，宁缺勿滥。
//
// Detail 截断至 500 字符（api 层落库前截断），且只记字段名/计数等非敏感摘要——
// 密码、API Key 明文、渠道秘钥等敏感值绝不入审计（详见 api 层 audit helper 注释）。
type AuditLog struct {
	ID        uint      `gorm:"primaryKey"`
	Username  string    `gorm:"index;size:64"` // 操作者（会话用户名；scheduler 等系统行为不产生审计行）
	Action    string    // 动作枚举（Audit* 常量）
	Target    string    // 目标摘要，形如 "channel#3 name=示例渠道"
	Detail    string    // 补充摘要（≤500 字符截断；仅字段名/计数，不含敏感值）
	IP        string    `gorm:"size:64"` // 操作来源 c.ClientIP()（反代部署依赖可信代理配置）
	CreatedAt time.Time `gorm:"index"`
}

// Notification 站内通知中心的消息（发送失败/渠道异常事件落库，事件类型常量见 notify 包）。
// 索引取舍：通知中心主路径是「按已读过滤 + created_at 倒序分页/未读计数」，
// 单列 read 区分度极低（绝大多数行为未读），故建 (read, created_at) 组合索引同时
// 覆盖过滤与排序；全量时间线与 PruneOld（按 id 倒序，自增与 created_at 单调一致）
// 在本表量级（仅异常事件，远小于 SendLog）下无需再单独建 created_at 索引。
type Notification struct {
	ID        uint   `gorm:"primaryKey"`
	Event     string // 事件类型：channel_disabled|channel_cooldown|channel_recovered|send_failed
	Level     string // info|warn|error
	Title     string
	Body      string
	ChannelID *uint     `gorm:"index"` // 关联渠道；与具体渠道无关的事件为空
	Read      bool      `gorm:"index:idx_notifications_read_created_at,priority:1"`
	CreatedAt time.Time `gorm:"index:idx_notifications_read_created_at,priority:2"`
}
