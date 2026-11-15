// dto.go 请求/响应 DTO、string-or-array 兼容解析、敏感字段脱敏与地址解析工具
// （SPEC §2/§5/§8：响应一律脱敏，输入兼容 Resend 风格扁平结构）。
package api

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"catmail/internal/model"
	"catmail/internal/provider"
)

// stringOrList 兼容 "string" 与 "string[]" 两种 JSON 形态（SPEC §2：to/cc/bcc/reply_to）。
type stringOrList []string

func (s *stringOrList) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*s = nil
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = []string{one}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	*s = list
	return nil
}

// ---------- 发送请求（/v1/send 与 /emails 共用） ----------

// sendRequest 对外发送请求体（Resend 风格扁平结构，replyTo 为 camelCase 别名）。
type sendRequest struct {
	From         string            `json:"from"`
	To           stringOrList      `json:"to"`
	Subject      string            `json:"subject"`
	HTML         string            `json:"html"`
	Text         string            `json:"text"`
	CC           stringOrList      `json:"cc"`
	BCC          stringOrList      `json:"bcc"`
	ReplyTo      stringOrList      `json:"reply_to"`
	ReplyToAlias stringOrList      `json:"replyTo"` // camelCase 别名兼容
	Headers      map[string]string `json:"headers"`
	Attachments  []attachmentDTO   `json:"attachments"`
	Tags         []tagDTO          `json:"tags"`
	Channel      string            `json:"channel"`
	// 邮件模板扩展字段（邮件模板功能包）：template_id 引用管理端模板，渲染结果
	// 替换 subject/html/text（冲突策略见 send_template.go）；variables 为渲染变量
	// 对象（JSON 键值，支持嵌套对象点路径取值）。两者均为可选扩展：SDK 不发送时
	// 保持零值，行为与启用前完全一致（Resend SDK 同构不受影响）。
	TemplateID uint           `json:"template_id"`
	Variables  map[string]any `json:"variables"`
}

// attachmentDTO 附件：filename + base64 内容（content 为 content_base64 别名）。
type attachmentDTO struct {
	Filename      string `json:"filename"`
	ContentBase64 string `json:"content_base64"`
	Content       string `json:"content"`
}

// tagDTO 标签 {name,value}，适配器尽力透传。
type tagDTO struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ---------- 渠道 ----------

// channelInput 渠道创建/更新请求体。全部字段用指针：nil = 省略 = 保留原值
// （P2-5：PUT 省略字段不再被清空；创建时未提供的字段保持零值；前端全量提交
// 不受影响）；秘钥类字段（api_key/api_secret/proxy_url/smtp_pass）传空串 = 保留
// 原值，防止前端回显掩码误清凭据（SPEC §5）。
type channelInput struct {
	Name                 *string `json:"name"`
	Provider             *string `json:"provider"`
	Priority             *int    `json:"priority"`
	APIKey               *string `json:"api_key"`
	APISecret            *string `json:"api_secret"`
	Domain               *string `json:"domain"`
	Region               *string `json:"region"`
	ProxyURL             *string `json:"proxy_url"`
	AllowedDomains       *string `json:"allowed_domains"`
	FromEmail            *string `json:"from_email"`
	FromName             *string `json:"from_name"`
	SMTPHost             *string `json:"smtp_host"`
	SMTPPort             *int    `json:"smtp_port"`
	SMTPUser             *string `json:"smtp_user"`
	SMTPPass             *string `json:"smtp_pass"`
	SMTPTLS              *string `json:"smtp_tls"`
	DailyLimitOverride   *int    `json:"daily_limit_override"`
	MonthlyLimitOverride *int    `json:"monthly_limit_override"`
	RateLimitPerSec      *int    `json:"rate_limit_per_sec"` // 出站限速（封/秒）；nil=保留，0=不限
	// WebhookSecret 平台签名密钥（送达链路三件套）：Resend 的 whsec_ 签名串 /
	// Mailgun 的 Webhook Signing Key；传空 = 保留原值（秘钥类字段同款语义），
	// 清除走 clear_secrets。空串 = 该渠道仅按 URL token 校验。
	WebhookSecret *string `json:"webhook_secret"`
	// ClearSecrets 显式清除的秘钥字段名（仅 PUT 生效）：合法值 ⊆
	// {api_key, api_secret, proxy_url, smtp_pass, webhook_secret}；去重、空白项忽略、
	// 含未知值 → 400。
	// 优先级：显式清除 > 指针字段保留语义（nil/空串=保留）；同字段同时传非空新值时新值生效。
	ClearSecrets []string `json:"clear_secrets"`
}

// channelTopErrorDTO 渠道 7 日失败原因聚合项（reason 为 error 前 120 字符归并）。
type channelTopErrorDTO struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// channelHealthDTO 渠道近 7 日健康度（A3 + 送达链路三件套升级）：受理侧由
// send_logs 聚合（is_test=false，与 stats 口径一致），success_rate 为 0-100 整数
// （四舍五入，分母 total 含发送中的瞬态 pending 行）；top_errors 为失败原因 Top3。
// 送达侧由 mail_events 聚合：delivered/bounced/complained 为 7 天窗口内事件计数；
// 三个 rate（0-100，一位小数）= 计数 / 终态事件合计（delivered+bounced+complained），
// 窗口内无终态事件的渠道为 null（JSON null）——「无事件数据」与「0%」语义不同，
// 前端据此展示灰色「—」。口径详见 handler_stats.go channelHealthMap。
type channelHealthDTO struct {
	WindowDays    int                  `json:"window_days"`
	Total         int64                `json:"total"`
	Sent          int64                `json:"sent"`
	Failed        int64                `json:"failed"`
	SuccessRate   int                  `json:"success_rate"`
	LastSuccessAt *time.Time           `json:"last_success_at"`
	LastFailedAt  *time.Time           `json:"last_failed_at"`
	TopErrors     []channelTopErrorDTO `json:"top_errors"`
	// 送达侧（送达链路三件套升级）
	Delivered     int64    `json:"delivered"`      // delivered 事件数
	Bounced       int64    `json:"bounced"`        // bounced 事件数（hard+soft）
	Complained    int64    `json:"complained"`     // complained 事件数
	DeliveryRate  *float64 `json:"delivery_rate"`  // 送达率（%），null=无终态事件数据
	BounceRate    *float64 `json:"bounce_rate"`    // 退信率（%），null=无终态事件数据
	ComplaintRate *float64 `json:"complaint_rate"` // 投诉率（%），null=无终态事件数据
}

// channelDTO 渠道响应体：密钥/代理一律脱敏，附生效额度与用量（SPEC §5）。
// Health 仅渠道列表（GET /api/channels）填充：窗口聚合一并算出，零额外往返；
// 创建/更新/启停/测试等单渠道响应保持 null（前端详情面板读列表行数据，
// 不依赖单渠道响应携带健康块）。
type channelDTO struct {
	ID                   uint   `json:"id"`
	Name                 string `json:"name"`
	Provider             string `json:"provider"`
	Status               int    `json:"status"`
	Priority             int    `json:"priority"`
	APIKey               string `json:"api_key"`
	APISecret            string `json:"api_secret"`
	Domain               string `json:"domain"`
	Region               string `json:"region"`
	ProxyURL             string `json:"proxy_url"`
	AllowedDomains       string `json:"allowed_domains"`
	FromEmail            string `json:"from_email"`
	FromName             string `json:"from_name"`
	SMTPHost             string `json:"smtp_host"`
	SMTPPort             int    `json:"smtp_port"`
	SMTPUser             string `json:"smtp_user"`
	SMTPPass             string `json:"smtp_pass"`
	SMTPTLS              string `json:"smtp_tls"`
	DailyLimitOverride   int    `json:"daily_limit_override"`
	MonthlyLimitOverride int    `json:"monthly_limit_override"`
	RateLimitPerSec      int    `json:"rate_limit_per_sec"` // 出站限速（封/秒，令牌桶）；0=不限
	// WebhookToken 入站送达事件 URL token（送达链路三件套）：明文回显——运营者
	// 需将其拼入平台 webhook 地址（/webhooks/mail-events/<token>），轮换走
	// POST /api/channels/:id/webhook-token；WebhookSecret 为平台侧签名密钥，
	// 属凭据，掩码回显（同 api_key 口径）。
	WebhookToken          string     `json:"webhook_token"`
	WebhookSecret         string     `json:"webhook_secret"`
	EffectiveDailyLimit   int        `json:"effective_daily_limit"`
	EffectiveMonthlyLimit int        `json:"effective_monthly_limit"`
	UsedDay               int        `json:"used_day"`
	UsedMonth             int        `json:"used_month"`
	CooldownUntil         *time.Time `json:"cooldown_until"`
	ConsecutiveFailures   int        `json:"consecutive_failures"`
	LastError             string     `json:"last_error"`
	AutoDisabledReason    string     `json:"auto_disabled_reason"`
	LastUsedAt            *time.Time `json:"last_used_at"`
	CreatedAt             time.Time  `json:"created_at"`
	// Health 近 7 日健康度；窗口内零日志的渠道为 null（而非全零块）：null 与
	// 「0% 成功率」语义不同，前端据此展示灰色「—」，避免把无数据误读成全失败。
	Health *channelHealthDTO `json:"health"`
}

// newChannelDTO 由渠道记录构造脱敏响应体；用量按 anchor 归一化到今天/本月展示。
func newChannelDTO(ch *model.Channel, now time.Time) channelDTO {
	quota := provider.DefaultQuotaFor(ch.Provider)
	dto := channelDTO{
		ID:                    ch.ID,
		Name:                  ch.Name,
		Provider:              ch.Provider,
		Status:                ch.Status,
		Priority:              ch.Priority,
		APIKey:                maskSecret(ch.APIKey),
		APISecret:             maskSecret(ch.APISecret),
		Domain:                ch.Domain,
		Region:                ch.Region,
		ProxyURL:              maskProxyURL(ch.ProxyURL),
		AllowedDomains:        ch.AllowedDomains,
		FromEmail:             ch.FromEmail,
		FromName:              ch.FromName,
		SMTPHost:              ch.SMTPHost,
		SMTPPort:              ch.SMTPPort,
		SMTPUser:              ch.SMTPUser,
		SMTPPass:              maskSecret(ch.SMTPPass),
		SMTPTLS:               ch.SMTPTLS,
		DailyLimitOverride:    ch.DailyLimitOverride,
		MonthlyLimitOverride:  ch.MonthlyLimitOverride,
		RateLimitPerSec:       ch.RateLimitPerSec,
		WebhookToken:          ch.WebhookToken,
		WebhookSecret:         maskSecret(ch.WebhookSecret),
		EffectiveDailyLimit:   effectiveLimitOf(ch.DailyLimitOverride, quota.Daily),
		EffectiveMonthlyLimit: effectiveLimitOf(ch.MonthlyLimitOverride, quota.Monthly),
		UsedDay:               ch.UsedDay,
		UsedMonth:             ch.UsedMonth,
		CooldownUntil:         ch.CooldownUntil,
		ConsecutiveFailures:   ch.ConsecutiveFailures,
		LastError:             ch.LastError,
		AutoDisabledReason:    ch.AutoDisabledReason,
		LastUsedAt:            ch.LastUsedAt,
		CreatedAt:             ch.CreatedAt,
	}
	if ch.DayAnchor != now.Format(dayLayout) {
		dto.UsedDay = 0 // anchor 未滚动到今天：展示为 0，不动库
	}
	if ch.MonthAnchor != now.Format(monthLayout) {
		dto.UsedMonth = 0
	}
	return dto
}

// ---------- 用户 / 日志 / Key / 设置 ----------

// userDTO 用户响应体（绝不含密码字段）。
type userDTO struct {
	ID        uint      `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// toAddrsField 收件人字段：落库为 JSON 串，读取后反序列化为数组返回；
// 解析失败时原样返回字符串（前端两种形态均兼容）。
type toAddrsField any

// newToAddrs 由落库 JSON 串构造 to_addrs 响应值。
func newToAddrs(raw string) toAddrsField {
	var out []string
	if raw == "" {
		return []string{}
	}
	if json.Unmarshal([]byte(raw), &out) == nil {
		return out
	}
	return raw
}

// logItemDTO 日志列表项（不含正文大字段）。
type logItemDTO struct {
	ID                uint         `json:"id"`
	MessageID         string       `json:"message_id"`
	ApiKeyID          uint         `json:"api_key_id"`
	ChannelID         uint         `json:"channel_id"`
	ChannelName       string       `json:"channel_name"`
	Provider          string       `json:"provider"`
	TemplateID        uint         `json:"template_id"` // 所用邮件模板（0=未使用模板）
	IsTest            bool         `json:"is_test"`
	Status            int          `json:"status"`
	FromAddr          string       `json:"from_addr"`
	ToAddrs           toAddrsField `json:"to_addrs"`
	Subject           string       `json:"subject"`
	ProviderMessageID string       `json:"provider_message_id"`
	Error             string       `json:"error"`
	Attempts          int          `json:"attempts"`
	LatencyMs         int64        `json:"latency_ms"`
	CreatedAt         time.Time    `json:"created_at"`
}

// logDetailDTO 日志详情：全字段，含 html_body/text_body（SPEC §5）。
type logDetailDTO struct {
	logItemDTO
	HTMLBody string `json:"html_body"`
	TextBody string `json:"text_body"`
}

// keyDTO API Key 响应体：key 一律掩码 cm-abcd…wxyz（SPEC §5）；
// total_sent 为该 key 的 SendLog 中 status=sent 的总数（含测试发送）；
// webhook_* 为调用方回执配置（D1）——URL 非凭据，明文回显供编辑弹窗回填；
// webhook_secret 为回执签名密钥（安全加固三件套，B），明文回显供管理页查看/复制
// （/api 会话保护，口径同 notify_webhook_secret）；
// disabled_reason 为密码重置联动停用标记（安全加固三件套，C，前端据此展示徽标）；
// daily_quota/rate_per_sec 为调用方维度配额与限速（B5），0=不限。
type keyDTO struct {
	ID             uint       `json:"id"`
	Name           string     `json:"name"`
	MaskedKey      string     `json:"masked_key"`
	Enabled        bool       `json:"enabled"`
	WebhookURL     string     `json:"webhook_url"`
	WebhookEvents  string     `json:"webhook_events"`
	WebhookSecret  string     `json:"webhook_secret"`
	DailyQuota     int        `json:"daily_quota"`
	RatePerSec     int        `json:"rate_per_sec"`
	DisabledReason string     `json:"disabled_reason"`
	TotalSent      int64      `json:"total_sent"`
	LastUsedAt     *time.Time `json:"last_used_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// settingsDTO 系统设置请求/响应体（SessionSecret 不对外暴露）。
// notify_* 四字段为失败通知配置（O9）：notify_events 为逗号分隔的事件白名单，
// 空串 = 全部事件（PUT 收到空串/缺省时规范化为全事件四 token 串）。
// log_body_enabled 为日志正文隐私开关（A4）：false 时新落库日志不再保存正文
// （只影响新日志，历史行原样保留），默认 true=保持现状。
// backup_daily_enabled 为每日自动备份开关（A2）：true 时 scheduler 每小时检查
// 「今天尚未备份」并自动执行一次整库备份（保留最近 10 份），默认 false。
type settingsDTO struct {
	Rotation                    string `json:"rotation"`
	MaxAttempts                 int    `json:"max_attempts"`
	CooldownSeconds             int    `json:"cooldown_seconds"`
	ConsecutiveFailureThreshold int    `json:"consecutive_failure_threshold"`
	LogRetentionDays            int    `json:"log_retention_days"`
	NotifyInappEnabled          bool   `json:"notify_inapp_enabled"`
	NotifyWebhookEnabled        bool   `json:"notify_webhook_enabled"`
	NotifyWebhookURL            string `json:"notify_webhook_url"`
	// NotifyWebhookSecret 通知 Webhook 签名密钥（安全加固三件套，B）：GET 回显
	// 明文（设置卡片展示/复制用，/api 会话保护）；PUT 整体忽略该字段——变更仅走
	// POST /api/settings/notify/webhook-secret（生成/轮换），防整表单保存误清。
	NotifyWebhookSecret string `json:"notify_webhook_secret"`
	NotifyEvents        string `json:"notify_events"`
	LogBodyEnabled      bool   `json:"log_body_enabled"`
	BackupDailyEnabled  bool   `json:"backup_daily_enabled"`
	// 抑制列表自动入库开关（送达链路三件套）：hard bounce 与 complained 事件
	// 自动写入 suppressions，默认开。
	SuppressionAutoEnabled bool `json:"suppression_auto_enabled"`
	// 退信率告警阈值（百分比 0-100，0=关闭告警）：渠道近 7 天退信率超阈值经
	// notify 广播 bounce_rate_exceeded warn 事件，默认 5。
	BounceRateThreshold float64 `json:"bounce_rate_threshold"`
}

// dailyStatDTO 每日发送统计（本地时区 MM-DD，缺数据日补 0）。
type dailyStatDTO struct {
	Date   string `json:"date"`
	Sent   int64  `json:"sent"`
	Failed int64  `json:"failed"`
}

// ---------- 脱敏与解析工具 ----------

// maskSecret 前4后4掩码；过短（≤8）全文打码；空值返回空（SPEC §8 响应脱敏）。
func maskSecret(s string) string {
	switch {
	case s == "":
		return ""
	case len(s) <= 8:
		return strings.Repeat("*", len(s))
	default:
		return s[:4] + "…" + s[len(s)-4:]
	}
}

// maskedAPIKey 形如 cm-abcd…wxyz 的 Key 掩码（SPEC §5）。
func maskedAPIKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:7] + "…" + key[len(key)-4:]
}

// maskProxyURL 代理地址含 userinfo（user:pass@host）时打码凭据部分。
func maskProxyURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return "***"
	}
	if u.User != nil {
		return u.Scheme + "://***@" + u.Host
	}
	return u.String()
}

// effectiveLimitOf 生效额度：override>0 时用 override，否则用平台默认（0=不限）。
func effectiveLimitOf(override, def int) int {
	if override > 0 {
		return override
	}
	return def
}

// parseFrom 解析 "Name <addr>" 或纯地址形式的发件人（SPEC §2）。
func parseFrom(raw string) (name, addr string, err error) {
	pa, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil {
		return "", "", err
	}
	return pa.Name, pa.Address, nil
}

// validEmail 校验单个邮箱地址格式（允许 "Name <addr>" 形态）。
func validEmail(s string) bool {
	if s == "" {
		return false
	}
	pa, err := mail.ParseAddress(s)
	return err == nil && pa.Address != ""
}

// collectAddrs 校验并收集邮箱列表；required 时至少需要一项。
// 单项长度 ≤320 字符（RFC 邮箱上限量级），错误信息只回显序号不回显原始值。
func collectAddrs(list stringOrList, field string, required bool) ([]string, error) {
	out := make([]string, 0, len(list))
	for i, raw := range list {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if utf8.RuneCountInString(s) > 320 || !validEmail(s) {
			return nil, fmt.Errorf("%s 第 %d 项不是有效邮箱地址", field, i+1)
		}
		out = append(out, s)
	}
	if required && len(out) == 0 {
		return nil, fmt.Errorf("%s 不能为空", field)
	}
	return out, nil
}

// validateUsername 用户名规则：去空白后 2-32 字符（SPEC 任务 B5）。
func validateUsername(name string) error {
	n := utf8.RuneCountInString(name)
	if n < 2 || n > 32 {
		return fmt.Errorf("用户名长度须为 2-32 个字符")
	}
	return nil
}

// validatePassword 密码规则：6-72 字符（72 为 bcrypt 输入上限）。
func validatePassword(p string) error {
	switch n := len(p); {
	case n < 6:
		return fmt.Errorf("密码至少 6 个字符")
	case n > 72:
		return fmt.Errorf("密码最长 72 个字符")
	}
	return nil
}
