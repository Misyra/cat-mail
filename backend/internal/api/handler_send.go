// handler_send.go 对外发送入口：/v1/send 与 /emails 共用同一 handler（SPEC §2）。
package api

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/provider"
	"catmail/internal/rotator"
)

// maxBodyChars html/text 正文各自的字符上限（1MB，P2-2 / SPEC §2）。
const maxBodyChars = 1048576

// reqErr 请求层错误：status + 统一错误体 type/message；internal 仅入日志。
type reqErr struct {
	status   int
	typ      string
	message  string
	internal error
}

// badReqErr 参数错误（400 invalid_request）。
func badReqErr(format string, a ...any) *reqErr {
	return &reqErr{status: http.StatusBadRequest, typ: TypeInvalidRequest, message: fmt.Sprintf(format, a...)}
}

// serverReqErr 内部错误（500，客户端仅见通用文案）。
func serverReqErr(err error) *reqErr {
	return &reqErr{status: http.StatusInternalServerError, typ: TypeServerError,
		message: "服务器内部错误", internal: err}
}

// send 统一发送入口（POST /v1/send 与 POST /emails）：
// Bearer ApiKey 校验 → 幂等闸门（可选 Idempotency-Key，E2）→ 请求解析校验
// → rotator.Send 轮换发送 → 统一响应/错误体。
func (s *Server) send(c *gin.Context) {
	apiKeyID, ok := s.authAPIKey(c)
	if !ok {
		return
	}
	// 幂等闸门在鉴权后、buildMessage 前生效：请求体解析/校验失败的 400 同样
	// 被存档重放（失败响应幂等，见 idemArchive）。无 key = 完全不启用去重，
	// 行为与启用前逐字节一致（零额外 DB 访问）。
	key, ok := idempotencyKeyOf(c)
	if !ok {
		return
	}
	if key == "" {
		s.sendExec(c, apiKeyID)
		return
	}
	recordID, ok := s.idemClaim(c, apiKeyID, key)
	if !ok {
		return
	}
	// 捕获包装：sendExec 的全部响应出口（成功/错误）统一在此截获，发送结束后
	// 存档状态码+响应体（方案取舍见 captureWriter）。send 为终端 handler，
	// 无需恢复 c.Writer。
	cw := &captureWriter{ResponseWriter: c.Writer}
	c.Writer = cw
	messageID := s.sendExec(c, apiKeyID)
	s.idemArchive(recordID, cw.Status(), cw.buf.Bytes(), messageID)
}

// sendExec 幂等包装内的原始发送流程（bindJSON → 模板渲染 hook → buildMessage →
// rotator.Send），返回成功响应的内部 message_id（错误路径为空串），供幂等档案
// 审计记录。
func (s *Server) sendExec(c *gin.Context, apiKeyID uint) string {
	var req sendRequest
	if !bindJSON(c, &req) {
		return ""
	}
	// 邮件模板渲染 hook（幂等检查之后、发送之前，语义见 send_template.go）：
	// template_id 存在则渲染结果替换 subject/html/text，渲染结果即发送快照。
	templateID, missingVars, rerr := s.applySendTemplate(&req)
	if rerr != nil {
		if rerr.internal != nil {
			log.Printf("api: send build failed: %v", rerr.internal)
		}
		writeErr(c, rerr.status, rerr.typ, rerr.message)
		return ""
	}
	msg, rerr := s.buildMessage(&req)
	if rerr != nil {
		if rerr.internal != nil {
			log.Printf("api: send build failed: %v", rerr.internal)
		}
		writeErr(c, rerr.status, rerr.typ, rerr.message)
		return ""
	}

	out, err := rotator.Send(c.Request.Context(), s.db, &rotator.Input{
		Message:     msg,
		ChannelName: strings.TrimSpace(req.Channel),
		APIKeyID:    apiKeyID,
		TemplateID:  templateID,
	})
	if err != nil {
		writeRotatorErr(c, err) // 抑制拦截等带 code 的错误透传 error.code（送达链路三件套）
		return ""
	}
	resp := gin.H{
		"id":                  out.MessageID,
		"channel":             out.ChannelName,
		"provider":            out.Provider,
		"provider_message_id": out.ProviderMessageID,
		"accepted":            true,
	}
	// 缺失变量清单（模板渲染留空策略的响应可见性）：仅使用模板且有缺失时附带，
	// 额外字段对 Resend SDK 兼容无影响（SDK 忽略未知字段）
	if len(missingVars) > 0 {
		resp["missing_variables"] = missingVars
	}
	c.JSON(http.StatusOK, resp)
	return out.MessageID
}

// authAPIKey 校验 Authorization: Bearer <ApiKey>（enabled），命中后更新 LastUsedAt；
// 失败 401（SPEC §5）。
func (s *Server) authAPIKey(c *gin.Context) (uint, bool) {
	const prefix = "bearer "
	h := c.GetHeader("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		unauthorized(c, "缺少 Bearer ApiKey")
		return 0, false
	}
	key := strings.TrimSpace(h[len(prefix):])
	if key == "" {
		unauthorized(c, "缺少 Bearer ApiKey")
		return 0, false
	}
	var ak model.ApiKey
	err := s.db.Where("key = ?", key).First(&ak).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		unauthorized(c, "ApiKey 无效或已停用")
		return 0, false
	}
	if err != nil {
		serverError(c, err)
		return 0, false
	}
	if !ak.Enabled {
		unauthorized(c, "ApiKey 无效或已停用")
		return 0, false
	}
	// B5 调用方维度限速与日额度：强制点在鉴权——请求被拒在门外（429 统一错误体），
	// 不进入发送流程、不落发送日志；被拒请求亦不刷新 last_used_at（拒绝洪峰不产生
	// 无谓写放大）。实现与顺序（先限速后额度）详见 middleware.go enforceKeyPolicy。
	if !s.enforceKeyPolicy(c, &ak) {
		return 0, false
	}
	// 记账失败不阻断发送（last_used_at 仅供展示与轮换排序），但不能静默：记入服务端日志
	if err := s.db.Model(&model.ApiKey{}).Where("id = ?", ak.ID).Update("last_used_at", time.Now()).Error; err != nil {
		log.Printf("api: authAPIKey update last_used_at failed (key_id=%d): %v", ak.ID, err)
	}
	return ak.ID, true
}

// buildMessage 解析并校验发送请求为规范 provider.Message（SPEC §2 字段表 + §8 校验）。
func (s *Server) buildMessage(req *sendRequest) (*provider.Message, *reqErr) {
	msg := &provider.Message{
		Subject: strings.TrimSpace(req.Subject),
	}
	req.Channel = strings.TrimSpace(req.Channel)
	if utf8.RuneCountInString(req.Channel) > 128 {
		return nil, badReqErr("channel 过长（≤128 字符）")
	}

	// headers：数量与键值长度上限（透传适配器，防超长）
	if len(req.Headers) > 50 {
		return nil, badReqErr("headers 数量超限（≤50）")
	}
	for k, v := range req.Headers {
		// P2-7：键/值拒绝 CRLF——防 SMTP/MIME 头注入（注入头可伪造收件人/主题等）
		if strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
			return nil, badReqErr("headers 键值不得包含换行符")
		}
		if strings.TrimSpace(k) == "" || utf8.RuneCountInString(k) > 256 || utf8.RuneCountInString(v) > 4096 {
			return nil, badReqErr("headers 键值无效（键非空 ≤256 字符、值 ≤4096 字符）")
		}
	}
	msg.Headers = req.Headers

	// from：请求未提供时——
	//   指定渠道：用其默认发件人（渠道也未配置 → 400）；
	//   未指定渠道：留空放行，交给 rotator 按各候选渠道的默认发件人逐候选回退
	//   （P1-4：回退后的域名天然匹配该渠道绑定域名；无任何候选配置默认时，
	//   由平台的参数类 4xx 终止 → 400 invalid_request，不换渠道）。
	fromRaw := strings.TrimSpace(req.From)
	if fromRaw == "" && req.Channel != "" {
		var ch model.Channel
		e := s.db.Where("name = ?", req.Channel).First(&ch).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return nil, badReqErr("指定渠道不存在：%s", req.Channel)
		} else if e != nil {
			return nil, serverReqErr(e)
		}
		if ch.FromEmail == "" {
			return nil, badReqErr("缺少 from，且渠道「%s」未配置默认发件人", req.Channel)
		}
		fromRaw = ch.FromEmail
		if ch.FromName != "" {
			fromRaw = fmt.Sprintf("%s <%s>", ch.FromName, ch.FromEmail)
		}
	}
	name, addr := "", ""
	if fromRaw != "" {
		if utf8.RuneCountInString(fromRaw) > 1024 {
			return nil, badReqErr("from 过长（≤1024 字符）")
		}
		var err error
		name, addr, err = parseFrom(fromRaw)
		if err != nil {
			return nil, badReqErr(`from 格式无效，应为 "Name <addr>" 或纯地址`)
		}
	}
	msg.FromName, msg.FromAddr = name, addr

	// 收件人：to 必填；cc/bcc/reply_to 可选；replyTo 别名兜底
	var e error
	if msg.To, e = collectAddrs(req.To, "to", true); e != nil {
		return nil, badReqErr("%s", e.Error())
	}
	if msg.Cc, e = collectAddrs(req.CC, "cc", false); e != nil {
		return nil, badReqErr("%s", e.Error())
	}
	if msg.Bcc, e = collectAddrs(req.BCC, "bcc", false); e != nil {
		return nil, badReqErr("%s", e.Error())
	}
	reply := req.ReplyTo
	if len(reply) == 0 {
		reply = req.ReplyToAlias
	}
	if msg.ReplyTo, e = collectAddrs(reply, "reply_to", false); e != nil {
		return nil, badReqErr("%s", e.Error())
	}
	if len(msg.To)+len(msg.Cc)+len(msg.Bcc) > 100 {
		return nil, badReqErr("收件人总数超限（≤100）")
	}

	// subject 必填且限长（RFC 单头行 998 字符）
	if msg.Subject == "" {
		return nil, badReqErr("subject 不能为空")
	}
	if utf8.RuneCountInString(msg.Subject) > 998 {
		return nil, badReqErr("subject 过长（≤998 字符）")
	}

	// html/text 至少一个非空；各限 1MB 字符（P2-2，SPEC §2）
	if strings.TrimSpace(req.HTML) == "" && strings.TrimSpace(req.Text) == "" {
		return nil, badReqErr("html 与 text 至少提供一个")
	}
	if utf8.RuneCountInString(req.HTML) > maxBodyChars || utf8.RuneCountInString(req.Text) > maxBodyChars {
		return nil, badReqErr("正文过大：html/text 各不超过 %d 字符", maxBodyChars)
	}
	msg.HTML, msg.Text = req.HTML, req.Text

	// 附件：filename 必填、base64 可解码（容忍换行）
	if len(req.Attachments) > 10 {
		return nil, badReqErr("附件数量超限（≤10）")
	}
	for _, a := range req.Attachments {
		fn := strings.TrimSpace(a.Filename)
		if fn == "" || utf8.RuneCountInString(fn) > 255 {
			return nil, badReqErr("附件 filename 不能为空且不超过 255 字符")
		}
		b64 := a.ContentBase64
		if b64 == "" {
			b64 = a.Content // content 为 content_base64 别名
		}
		if b64 == "" {
			return nil, badReqErr("附件 %s 缺少 content/content_base64", fn)
		}
		clean := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' {
				return -1
			}
			return r
		}, b64)
		if _, err := base64.StdEncoding.DecodeString(clean); err != nil {
			return nil, badReqErr("附件 %s 的 content_base64 不是合法 base64", fn)
		}
		msg.Attachments = append(msg.Attachments, provider.Attachment{Filename: fn, ContentBase64: clean})
	}

	// tags：{name,value}[]，name 必填
	if len(req.Tags) > 20 {
		return nil, badReqErr("tags 数量超限（≤20）")
	}
	for _, t := range req.Tags {
		if strings.TrimSpace(t.Name) == "" {
			return nil, badReqErr("tag.name 不能为空")
		}
		msg.Tags = append(msg.Tags, provider.Tag{Name: t.Name, Value: t.Value})
	}
	return msg, nil
}
