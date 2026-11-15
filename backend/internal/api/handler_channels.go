// handler_channels.go 渠道管理：列表（脱敏）/创建/部分更新/删除/启停/直发测试（SPEC §5）。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/provider"
	"catmail/internal/render"
	"catmail/internal/rotator"
)

// listChannels 渠道列表：密钥/代理脱敏，附生效额度与用量；附带近 7 日健康块
// （A3：channelHealthMap 两条聚合 SQL 一次算齐全部渠道，零日志渠道 health=null）。
func (s *Server) listChannels(c *gin.Context) {
	var chs []model.Channel
	if err := s.db.Order("priority ASC, id ASC").Find(&chs).Error; err != nil {
		serverError(c, err)
		return
	}
	now := time.Now()
	health, err := s.channelHealthMap(now)
	if err != nil {
		serverError(c, err)
		return
	}
	items := make([]channelDTO, 0, len(chs))
	for i := range chs {
		dto := newChannelDTO(&chs[i], now)
		dto.Health = health[chs[i].ID] // 无日志渠道为 nil → JSON null
		items = append(items, dto)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// createChannel 创建渠道：name 唯一、provider 合法、from 校验（SPEC §5）。
func (s *Server) createChannel(c *gin.Context) {
	var in channelInput
	if !bindJSON(c, &in) {
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		badRequest(c, "渠道名称不能为空")
		return
	}
	if in.Provider == nil || *in.Provider == "" {
		badRequest(c, "平台（provider）不能为空")
		return
	}
	ch := &model.Channel{Status: model.ChannelEnabled}
	// 入站送达事件 URL token（送达链路三件套）：建渠道即生成（256bit 随机），
	// 前端渠道详情直接展示完整 webhook 地址；生成失败 500（低概率，无兜底必要）
	token, terr := randomHex(16)
	if terr != nil {
		serverError(c, terr)
		return
	}
	ch.WebhookToken = token
	if err := s.applyChannelInput(ch, &in); err != nil {
		badRequest(c, err.Error())
		return
	}
	if err := s.ensureChannelNameFree(ch.Name, 0); err != nil {
		badRequest(c, err.Error())
		return
	}
	if err := validateChannel(ch); err != nil {
		badRequest(c, err.Error())
		return
	}
	if err := s.db.Create(ch).Error; err != nil {
		// 并发重名兜底（P2-13）：唯一索引冲突映射 400
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			badRequest(c, errChannelNameTaken.Error())
			return
		}
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录
	s.audit(c, model.AuditChannelCreate,
		fmt.Sprintf("channel#%d name=%s", ch.ID, ch.Name),
		fmt.Sprintf("provider=%s priority=%d", ch.Provider, ch.Priority))
	c.JSON(http.StatusOK, newChannelDTO(ch, time.Now()))
}

// updateChannel 部分更新：省略字段 = 保留原值（P2-5，指针字段语义）；
// api_key/api_secret/proxy_url/smtp_pass 传空 = 保留原值；更新后 status 复位为 0、
// 连续失败/last_error/auto_disabled_reason 清空（SPEC §5）。
func (s *Server) updateChannel(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var ch model.Channel
	if err := s.db.First(&ch, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "渠道不存在")
			return
		}
		serverError(c, err)
		return
	}
	var in channelInput
	if !bindJSON(c, &in) {
		return
	}
	// clear_secrets 显式清除（先于指针字段合并）：清除覆盖指针字段的保留语义
	// （nil/空串=保留），同请求中同字段传非空新值时新值生效；含未知值 → 400。
	if err := clearChannelSecrets(&ch, in.ClearSecrets); err != nil {
		badRequest(c, err.Error())
		return
	}
	if err := s.applyChannelInput(&ch, &in); err != nil {
		badRequest(c, err.Error())
		return
	}
	if err := s.ensureChannelNameFree(ch.Name, ch.ID); err != nil {
		badRequest(c, err.Error())
		return
	}
	if err := validateChannel(&ch); err != nil {
		badRequest(c, err.Error())
		return
	}
	ch.Status = model.ChannelEnabled
	ch.ConsecutiveFailures = 0
	ch.AutoDisabledReason = ""
	ch.CooldownUntil = nil
	ch.LastError = "" // P2-6：编辑成功即清除残留最近错误（与状态复位语义一致）
	// P1-2 + P2-6：只写「表单可写集 + 复位字段」，不回写运行时计数——Save 全字段覆盖
	// 会把并发发送刚累计的 used_day/used_month 与周期锚点用编辑时刻的旧快照抹掉。
	// Select 强制写入零值字段（清空的字符串、nil 的 cooldown_until、空的 last_error）。
	if err := s.db.Model(&model.Channel{}).Where("id = ?", ch.ID).
		Select("Name", "Provider", "Priority", "APIKey", "APISecret", "ProxyURL", "SMTPPass",
			"Domain", "Region", "AllowedDomains", "FromEmail", "FromName",
			"SMTPHost", "SMTPPort", "SMTPUser", "SMTPTLS", "DailyLimitOverride", "MonthlyLimitOverride",
			"RateLimitPerSec", "WebhookSecret",
			"Status", "ConsecutiveFailures", "AutoDisabledReason", "CooldownUntil", "LastError").
		Updates(ch).Error; err != nil {
		// 并发改名兜底（P2-13）：唯一索引冲突映射 400
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			badRequest(c, errChannelNameTaken.Error())
			return
		}
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录；秘钥类字段值一律不入审计，仅记平台与优先级
	s.audit(c, model.AuditChannelUpdate,
		fmt.Sprintf("channel#%d name=%s", ch.ID, ch.Name),
		fmt.Sprintf("provider=%s priority=%d", ch.Provider, ch.Priority))
	c.JSON(http.StatusOK, newChannelDTO(&ch, time.Now()))
}

// applyChannelInput 将请求体合并进渠道记录（创建与更新统一入口）。
// 指针字段 nil = 保留原值（P2-5：PUT 省略字段不再被清空）；秘钥类字段传空串 =
// 保留原值（防前端回显掩码误清凭据）；其余非秘钥字符串字段传空串 = 显式清空。
func (s *Server) applyChannelInput(ch *model.Channel, in *channelInput) error {
	if in.Name != nil {
		if name := strings.TrimSpace(*in.Name); name != "" {
			ch.Name = name // 名称不允许为空：显式空串视作未修改
		}
	}
	if in.Provider != nil && *in.Provider != "" {
		ch.Provider = *in.Provider // 平台不允许为空：显式空串视作未修改
	}
	if in.Priority != nil {
		ch.Priority = *in.Priority
	}
	// 秘钥类：传空 = 保留原值
	if in.APIKey != nil && *in.APIKey != "" {
		ch.APIKey = *in.APIKey
	}
	if in.APISecret != nil && *in.APISecret != "" {
		ch.APISecret = *in.APISecret
	}
	if in.WebhookSecret != nil && *in.WebhookSecret != "" {
		ch.WebhookSecret = strings.TrimSpace(*in.WebhookSecret) // 平台签名密钥（Resend whsec_/Mailgun signing key）
	}
	if in.ProxyURL != nil && *in.ProxyURL != "" {
		ch.ProxyURL = strings.TrimSpace(*in.ProxyURL)
	}
	if in.SMTPPass != nil && *in.SMTPPass != "" {
		ch.SMTPPass = *in.SMTPPass
	}
	// 非秘钥字符串字段：nil = 保留，显式值（含空串）= 覆盖/清空
	if in.Domain != nil {
		ch.Domain = strings.TrimSpace(*in.Domain)
	}
	if in.Region != nil {
		ch.Region = strings.TrimSpace(*in.Region)
	}
	if in.AllowedDomains != nil {
		ch.AllowedDomains = strings.TrimSpace(*in.AllowedDomains)
	}
	if in.FromEmail != nil {
		ch.FromEmail = strings.TrimSpace(*in.FromEmail)
	}
	if in.FromName != nil {
		ch.FromName = strings.TrimSpace(*in.FromName)
	}
	if in.SMTPHost != nil {
		ch.SMTPHost = strings.TrimSpace(*in.SMTPHost)
	}
	if in.SMTPUser != nil {
		ch.SMTPUser = strings.TrimSpace(*in.SMTPUser)
	}
	if in.SMTPTLS != nil {
		ch.SMTPTLS = strings.ToLower(strings.TrimSpace(*in.SMTPTLS))
	}
	if in.SMTPPort != nil {
		ch.SMTPPort = *in.SMTPPort
	}
	if in.DailyLimitOverride != nil {
		ch.DailyLimitOverride = *in.DailyLimitOverride
	}
	if in.MonthlyLimitOverride != nil {
		ch.MonthlyLimitOverride = *in.MonthlyLimitOverride
	}
	if in.RateLimitPerSec != nil {
		ch.RateLimitPerSec = *in.RateLimitPerSec
	}
	return nil
}

// clearChannelSecretNames PUT clear_secrets 允许显式清除的秘钥字段集合。
var clearChannelSecretNames = map[string]bool{
	"api_key":        true,
	"api_secret":     true,
	"proxy_url":      true,
	"smtp_pass":      true,
	"webhook_secret": true, // 平台签名密钥（送达链路三件套）：清除后仅按 URL token 校验
}

// clearChannelSecrets 处理 PUT 的 clear_secrets：去重、空白项忽略；
// 含未知值 → 错误（调用方输出 400 invalid_request）。先于 applyChannelInput
// 执行——指针字段的保留语义（nil/空串=保留）不覆盖已清除的零值，而同字段
// 传入的非空新值会生效；清除后的合并结果仍经 validateChannel 校验（不绕过）。
func clearChannelSecrets(ch *model.Channel, names []string) error {
	seen := make(map[string]bool, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue // 空白项忽略
		}
		if !clearChannelSecretNames[name] {
			return fmt.Errorf("clear_secrets 含未知字段：%s", name)
		}
		if seen[name] {
			continue // 去重
		}
		seen[name] = true
		switch name {
		case "api_key":
			ch.APIKey = ""
		case "api_secret":
			ch.APISecret = ""
		case "proxy_url":
			ch.ProxyURL = ""
		case "smtp_pass":
			ch.SMTPPass = ""
		case "webhook_secret":
			ch.WebhookSecret = ""
		}
	}
	return nil
}

// ensureChannelNameFree 校验渠道名唯一（excludeID 用于更新时排除自身）。
func (s *Server) ensureChannelNameFree(name string, excludeID uint) error {
	var n int64
	q := s.db.Model(&model.Channel{}).Where("name = ?", name)
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	if err := q.Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return errChannelNameTaken
	}
	return nil
}

// errChannelNameTaken 渠道重名。
var errChannelNameTaken = errors.New("渠道名称已存在")

// validateChannel 校验渠道合并后的完整配置（创建与更新统一入口，SPEC §8 输入校验）。
func validateChannel(ch *model.Channel) error {
	if n := utf8.RuneCountInString(ch.Name); n < 1 || n > 128 {
		return errors.New("渠道名称长度须为 1-128 个字符")
	}
	valid := false
	for _, name := range provider.Names() {
		if name == ch.Provider {
			valid = true
			break
		}
	}
	if !valid {
		return errors.New("未知平台：" + ch.Provider)
	}
	// 字符串字段长度上限（防超长写库，SPEC §8）
	for _, c := range []struct {
		name  string
		value string
		max   int
	}{
		{"api_key", ch.APIKey, 1024},
		{"api_secret", ch.APISecret, 1024},
		{"smtp_pass", ch.SMTPPass, 1024},
		{"proxy_url", ch.ProxyURL, 1024},
		{"domain", ch.Domain, 512},
		{"allowed_domains", ch.AllowedDomains, 2048},
		{"from_email", ch.FromEmail, 512},
		{"from_name", ch.FromName, 512},
		{"smtp_host", ch.SMTPHost, 512},
		{"smtp_user", ch.SMTPUser, 512},
		{"webhook_secret", ch.WebhookSecret, 1024}, // 平台签名密钥（whsec_ 串最长约百字符，留余量）
	} {
		if utf8.RuneCountInString(c.value) > c.max {
			return fmt.Errorf("%s 过长（≤%d 字符）", c.name, c.max)
		}
	}
	if ch.ProxyURL != "" {
		u, err := url.Parse(ch.ProxyURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "socks5") {
			return errors.New("代理地址仅支持 http:// 与 socks5://")
		}
	}
	if ch.Region != "" && ch.Region != "us" && ch.Region != "eu" {
		return errors.New("region 仅支持 us/eu")
	}
	if ch.FromEmail != "" && !validEmail(ch.FromEmail) {
		return errors.New("from_email 格式无效")
	}
	if ch.SMTPTLS != "" && ch.SMTPTLS != "none" && ch.SMTPTLS != "starttls" && ch.SMTPTLS != "tls" {
		return errors.New("smtp_tls 仅支持 none/starttls/tls")
	}
	if ch.DailyLimitOverride < 0 || ch.MonthlyLimitOverride < 0 {
		return errors.New("额度覆盖值不能为负数")
	}
	if ch.RateLimitPerSec < 0 {
		return errors.New("出站限速不能为负数")
	}
	switch ch.Provider {
	case "smtp":
		if ch.FromEmail == "" {
			return errors.New("SMTP 渠道必须配置默认发件人 from_email")
		}
		if ch.SMTPHost == "" {
			return errors.New("SMTP 渠道必须配置 smtp_host")
		}
		if ch.SMTPPort < 1 || ch.SMTPPort > 65535 {
			return errors.New("SMTP 渠道必须配置有效端口（1-65535）")
		}
	case "mailgun":
		if ch.Domain == "" {
			return errors.New("Mailgun 渠道必须配置发送域名 domain")
		}
	}
	return nil
}

// deleteChannel 删除渠道（日志保留冗余快照，不受影响）。删除前先读取渠道：
// 一并承担 404 判定，并为审计 target 提供渠道名（E4）。
func (s *Server) deleteChannel(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var ch model.Channel
	if err := s.db.First(&ch, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "渠道不存在")
			return
		}
		serverError(c, err)
		return
	}
	if err := s.db.Delete(&model.Channel{}, id).Error; err != nil {
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录
	s.audit(c, model.AuditChannelDelete,
		fmt.Sprintf("channel#%d name=%s", ch.ID, ch.Name), "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// channelStatusReq 启停请求体。
type channelStatusReq struct {
	Action string `json:"action"` // enable | disable
}

// channelStatus 手动启停：disable→1；enable→0 且清冷却/失败计数（SPEC §5）。
func (s *Server) channelStatus(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req channelStatusReq
	if !bindJSON(c, &req) {
		return
	}
	switch req.Action {
	case "disable":
		if err := s.db.Model(&model.Channel{}).Where("id = ?", id).
			Update("status", model.ChannelManualDisabled).Error; err != nil {
			serverError(c, err)
			return
		}
	case "enable":
		if err := s.db.Model(&model.Channel{}).Where("id = ?", id).Updates(map[string]any{
			"status":               model.ChannelEnabled,
			"cooldown_until":       nil,
			"consecutive_failures": 0,
		}).Error; err != nil {
			serverError(c, err)
			return
		}
	default:
		badRequest(c, "action 仅支持 enable/disable")
		return
	}
	var ch model.Channel
	if err := s.db.First(&ch, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "渠道不存在")
			return
		}
		serverError(c, err)
		return
	}
	// 审计（E4）：启停成功后记录（渠道不存在时上面已 404，不产生审计行）
	s.audit(c, model.AuditChannelStatus,
		fmt.Sprintf("channel#%d name=%s", ch.ID, ch.Name),
		"action="+req.Action)
	c.JSON(http.StatusOK, newChannelDTO(&ch, time.Now()))
}

// channelTestReq 渠道测试请求体。template_id/variables 为邮件模板扩展
// （邮件模板功能包）：提供模板时用渲染结果替换默认测试主题/正文。
type channelTestReq struct {
	To         string         `json:"to"`
	TemplateID uint           `json:"template_id"`
	Variables  map[string]any `json:"variables"`
}

// msgTestQuotaExhausted 测试直发额度用尽的统一错误文案（429 quota_exhausted）。
const msgTestQuotaExhausted = "渠道当日/当月额度已用尽，测试发送未执行"

// testChannel 直发测试：绕过轮换（不走候选排序/failover/渠道状态机），用渠道自身
// Transport 发送一封测试邮件并记 IsTest 日志（SPEC §5）。额度口径与轮换路径完全
// 一致：发送前先原子预留（rotator.ReserveQuota，同一套条件 UPDATE，含惰性周期
// 重置），平台受理成功保留扣减、发送失败释放（额度净增只发生在受理成功时）；
// 测试是人工诊断行为，失败不触发禁用/冷却/连续失败累计；停用/冷却渠道仍允许
// 测试（现状保留），但额度已用尽时不执行发送 → 429 统一错误体（quota_exhausted）
// 且不落发送日志（未实际发送）。管理端直发诊断接口（非发送入口）：失败仍以
// HTTP 200 + {ok:false,error} 返回平台错误文案。
func (s *Server) testChannel(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req channelTestReq
	if !bindJSON(c, &req) {
		return
	}
	to := strings.TrimSpace(req.To)
	if to == "" || utf8.RuneCountInString(to) > 320 || !validEmail(to) {
		badRequest(c, "缺少有效收件邮箱 to")
		return
	}
	var ch model.Channel
	if err := s.db.First(&ch, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "渠道不存在")
			return
		}
		serverError(c, err)
		return
	}
	transport, ok := provider.Get(ch.Provider)
	if !ok {
		badRequest(c, "平台适配器未注册："+ch.Provider)
		return
	}
	if ch.FromEmail == "" {
		badRequest(c, "渠道未配置默认发件人 from_email，无法测试")
		return
	}
	// 邮件模板（可选）：提供 template_id 时用渲染结果替换默认测试主题/正文。
	// unknown_template/变量校验失败在预留额度前拒绝（不白占额度）；测试直发的
	// 默认内容由服务端生成，无调用方正文，不涉及 body_conflict。
	var missingVars []string
	testSubject := "CatMail 渠道测试"
	testHTML := "<p>这是一封来自 CatMail 的渠道测试邮件，收到即说明渠道配置有效。</p>"
	testText := "这是一封来自 CatMail 的渠道测试邮件，收到即说明渠道配置有效。"
	if req.TemplateID != 0 {
		if rerr := validateVariables(req.Variables); rerr != nil {
			writeErr(c, rerr.status, rerr.typ, rerr.message)
			return
		}
		tpl, rerr := s.loadTemplate(req.TemplateID)
		if rerr != nil {
			writeErr(c, rerr.status, rerr.typ, rerr.message)
			return
		}
		rendered, missing := render.ApplyContent(render.Content{
			Subject: tpl.Subject,
			HTML:    tpl.HTML,
			Text:    tpl.Text,
		}, req.Variables)
		testSubject, testHTML, testText = rendered.Subject, rendered.HTML, rendered.Text
		if len(missing) > 0 {
			missingVars = missing
			log.Printf("api: channel test template missing variables (template_id=%d): %v", tpl.ID, missing)
		}
	}
	// 发送前原子预留额度（与轮换路径同口径）：抢占失败即当日/当月额度已用尽，
	// 测试不执行 → 429 统一错误体，未实际发送故不落发送日志。
	now := time.Now()
	if !rotator.ReserveQuota(s.db, &ch, now) {
		writeErr(c, http.StatusTooManyRequests, TypeQuotaExhausted, msgTestQuotaExhausted)
		return
	}
	// panic 安全（O8）：预留成功后 defer 配对兜底——transport.Send panic 时
	//（gin Recovery 兜底进程不崩）预留不再静默多计。quotaSettled 标记预留是否
	// 已按正常路径结算（受理成功保留 / 发送失败已显式释放），仅 panic 未结算时
	// defer 补释放，正常路径不双重释放。
	quotaSettled := false
	defer func() {
		if !quotaSettled {
			rotator.ReleaseQuota(s.db, &ch, now)
		}
	}()
	msg := &provider.Message{
		FromName: ch.FromName,
		FromAddr: ch.FromEmail,
		To:       []string{to},
		Subject:  testSubject,
		HTML:     testHTML,
		Text:     testText,
	}
	start := time.Now()
	res, perr := transport.Send(c.Request.Context(), &ch, msg)
	latency := time.Since(start).Milliseconds()
	if perr != nil {
		// 发送失败：释放预留额度（不改渠道状态机：不禁用、不冷却、不累计连续失败）。
		rotator.ReleaseQuota(s.db, &ch, now)
	}
	quotaSettled = true // 正常路径均已结算：成功保留扣减 / 失败已显式释放
	s.writeTestLog(&ch, msg, req.TemplateID, res, perr, latency)

	resp := gin.H{"ok": perr == nil, "latency_ms": latency}
	if perr != nil {
		resp["error"] = perr.Error()
	}
	// 缺失变量清单（模板渲染留空策略的响应可见性，与 /v1/send 口径一致）
	if len(missingVars) > 0 {
		resp["missing_variables"] = missingVars
	}
	c.JSON(http.StatusOK, resp)
}

// duplicateChannel POST /api/channels/:id/duplicate（E6）：读原渠道全字段新建副本。
//   - 副本名 = 原名-副本，重名依次追加 -2/-3 直到不冲突（并发窗口由渠道名唯一
//     索引兜底，冲突映射 400）；名称超长无法生成副本名时 400。
//   - status=manual_disabled：副本默认停用，防止与原渠道并存时意外参与轮换上线；
//   - secrets（api_key/api_secret/smtp_pass/proxy_url）服务端原样复制，响应仍按
//     统一规则脱敏回显（掩码与原渠道一致）；
//   - 运行态归零：used_day/used_month=0（连带周期锚点清空，惰性重置按新周期起算）、
//     last_error 清空、consecutive_failures=0；cooldown_until/auto_disabled_reason/
//     last_used_at 同属原渠道运行态，一并清空。
//   - 响应新渠道 DTO（结构同创建接口）。
func (s *Server) duplicateChannel(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var src model.Channel
	if err := s.db.First(&src, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "渠道不存在")
			return
		}
		serverError(c, err)
		return
	}
	name := src.Name + "-副本"
	for i := 2; ; i++ {
		var n int64
		if err := s.db.Model(&model.Channel{}).Where("name = ?", name).Count(&n).Error; err != nil {
			serverError(c, err)
			return
		}
		if n == 0 {
			break
		}
		name = fmt.Sprintf("%s-副本-%d", src.Name, i)
	}
	dup := src // 全字段复制（含 secrets），以下仅覆写身份与运行态字段
	dup.ID = 0
	dup.CreatedAt, dup.UpdatedAt = time.Time{}, time.Time{}
	dup.Name = name
	// 入站 webhook token 必须重生成（原渠道同 URL 会串渠道事件）：256bit 随机，
	// 与 createChannel 同口径；生成失败 500（低概率）。
	dupToken, terr := randomHex(16)
	if terr != nil {
		serverError(c, terr)
		return
	}
	dup.WebhookToken = dupToken
	dup.Status = model.ChannelManualDisabled
	dup.UsedDay, dup.DayAnchor = 0, ""
	dup.UsedMonth, dup.MonthAnchor = 0, ""
	dup.LastError = ""
	dup.ConsecutiveFailures = 0
	dup.AutoDisabledReason = ""
	dup.CooldownUntil = nil
	dup.LastUsedAt = nil
	// 原渠道本就合法，此处仅可能因副本名超长（原名接近 128 字符）而拒绝
	if err := validateChannel(&dup); err != nil {
		badRequest(c, err.Error())
		return
	}
	if err := s.db.Create(&dup).Error; err != nil {
		// 并发重名兜底：唯一索引冲突映射 400
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			badRequest(c, errChannelNameTaken.Error())
			return
		}
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录，target 为新副本，detail 指向复制来源
	s.audit(c, model.AuditChannelDuplicate,
		fmt.Sprintf("channel#%d name=%s", dup.ID, dup.Name),
		fmt.Sprintf("from=channel#%d name=%s", src.ID, src.Name))
	c.JSON(http.StatusOK, newChannelDTO(&dup, time.Now()))
}

// logBodyEnabled 读取「记录邮件正文」开关（A4，settings.log_body_enabled）。
// 与 handler_settings 同款单行读取（First(ID=1)）：测试直发低频，每次落日志附带
// 一次主键单行读开销可接受；读取失败或设置行缺失时按开启处理（保持既有行为，
// 正文漏记不可恢复）。
func (s *Server) logBodyEnabled() bool {
	var st model.Setting
	if err := s.db.First(&st, 1).Error; err != nil {
		return true
	}
	return st.LogBodyEnabled
}

// writeTestLog 测试直发日志落库（IsTest=true）。额度由调用方在发送前预留：受理
// 成功保留扣减、失败已释放（见 testChannel）；此处与轮换路径一致不改渠道状态机。
// A4 隐私开关：log_body_enabled=false 时新日志正文存空串（元数据照常）；只影响
// 新日志，存量已落正文的历史行原样保留——刻意取舍，关闭开关不承诺抹除历史。
func (s *Server) writeTestLog(ch *model.Channel, m *provider.Message, templateID uint,
	res *provider.Result, perr *provider.ProviderError, latency int64) {
	status := model.SendStatusSent
	errText := ""
	providerMsgID := ""
	if perr != nil {
		status = model.SendStatusFailed
		errText = perr.Error()
	} else if res != nil {
		providerMsgID = res.ProviderMessageID
	}
	toAddrs, _ := json.Marshal(m.To)
	htmlBody, textBody := m.HTML, m.Text
	if !s.logBodyEnabled() {
		htmlBody, textBody = "", ""
	}
	if err := s.db.Create(&model.SendLog{
		MessageID:         uuid.NewString(),
		ChannelID:         ch.ID,
		ChannelName:       ch.Name,
		Provider:          ch.Provider,
		TemplateID:        templateID,
		IsTest:            true,
		Status:            status,
		FromAddr:          m.FromAddr,
		ToAddrs:           string(toAddrs),
		Subject:           m.Subject,
		HTMLBody:          htmlBody,
		TextBody:          textBody,
		ProviderMessageID: providerMsgID,
		Error:             errText,
		Attempts:          1,
		LatencyMs:         latency,
	}).Error; err != nil {
		// 与 writeSendLog 的 P2-3 同口径：落库失败不影响发送结果，但不静默，记入服务端日志
		log.Printf("api: writeTestLog failed (channel=%s): %v", ch.Name, err)
	}
}
