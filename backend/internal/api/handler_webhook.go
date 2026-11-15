// handler_webhook.go 送达事件入站端点（送达链路三件套）：公开无会话鉴权，POST
// /webhooks/mail-events/:token。鉴权分层（两层独立生效，签名优先）：
//  1. URL token 定位渠道（每渠道独立 WebhookToken，唯一索引精确匹配）——对无
//     签名机制的平台（Brevo/SMTP2GO/Mailjet/Elastic Email/Mailtrap/aliyun 等）
//     即共享 secret 校验（任务书「URL 带 token」取舍，平台 webhook 不支持自定义
//     请求头，详见 inbound 包注释）；
//  2. 平台真实签名（有则必须验真，验败一律拒绝且不回退 token）：Resend 按 Svix
//     规范（whsec_ 密钥存渠道 WebhookSecret），Mailgun 按 timestamp+token HMAC。
//
// 摄取流程：定位渠道 → 鉴权 → 读原始报文 → 归一化 → 尽力反查 SendLog 关联
// → 落 mail_events → hard bounce/complaint 自动入抑制列表（设置开关，默认开）
// → bounced 事件触发退信率告警判定（notify warn，1h/渠道去重）→ 200。
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/inbound"
	"catmail/internal/model"
)

// mailEventDetailMax 事件 Detail 截断上限（与审计明细同风格，防超长刷库）。
const mailEventDetailMax = 500

// ingestMailEvent 入站事件摄取 handler（路由与鉴权分层见文件头注释）。
func (s *Server) ingestMailEvent(c *gin.Context) {
	token := c.Param("token")
	if token == "" {
		badRequest(c, "无效的 webhook 地址")
		return
	}
	var ch model.Channel
	if err := s.db.Where("webhook_token = ?", token).First(&ch).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 404 + 统一错误体：不区分「渠道不存在/token 错误」，不泄露渠道枚举面
			writeErr(c, http.StatusNotFound, TypeInvalidRequest, "webhook 地址无效或已轮换")
			return
		}
		serverError(c, err)
		return
	}

	// 读原始报文（签名校验与 raw 存档共用同一份字节，绝不 re-marshal）。
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		badRequest(c, "请求体读取失败")
		return
	}

	var form url.Values // form 编码报文（Mailgun legacy webhook）非 nil 时优先
	contentType := c.GetHeader("Content-Type")
	if !isJSONBody(body, contentType) {
		if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
			if q, perr := url.ParseQuery(string(body)); perr == nil {
				form = q
			}
		} else if strings.HasPrefix(contentType, "multipart/form-data") {
			// 兼容 multipart 提交的 legacy webhook：仅取值字段转 form map
			if c.Request.ParseMultipartForm(maxAdminBodyBytes) == nil && c.Request.MultipartForm != nil {
				form = c.Request.MultipartForm.Value
			}
		}
	}

	if !s.authorizeInbound(c, &ch, body, form) {
		return
	}

	ev := inbound.Normalize(ch.Provider, body, form)
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now()
	}
	if len(ev.Detail) > mailEventDetailMax {
		ev.Detail = ev.Detail[:mailEventDetailMax]
	}

	row := s.persistMailEvent(&ch, ev, rawArchive(body, form))
	if row == nil {
		// 落库失败：事件平台会按非 2xx 重试，此处如实 500（幂等性由平台侧重试
		// 语义承担；重复投递的重复行由后续可能的 svix-id/token 去重与人工兜底）
		serverError(c, errors.New("mail event persist failed"))
		return
	}

	// 抑制列表自动入库（设置开关，默认开）：hard bounce 与 complained 命中即拦。
	s.autoSuppress(&ch, ev)

	// 退信率告警（bounced 触发；阈值/去重/白名单见 inbound.EmitBounceRateWarn）。
	if ev.Type == model.MailEventBounced {
		inbound.EmitBounceRateWarn(s.db, &ch)
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// authorizeInbound 入站鉴权分层（三层取或，签名请求验败必拒）：
//   - Svix 签名头存在（svix-* / webhook-* 三头齐备）→ 必须验真：渠道未配置
//     WebhookSecret 也拒绝（claimed 签名不验即过 = 绕过签名）；
//   - Mailgun 签名字段存在（timestamp+token+signature 齐备）→ 必须验真；
//   - 否则按 URL token 放行（token 已在路由匹配中精确定位渠道）。
//
// 返回 false 时已写出 401。
func (s *Server) authorizeInbound(c *gin.Context, ch *model.Channel, body []byte, form url.Values) bool {
	now := time.Now()

	// 1) Svix（Resend）：签名头齐备即走 Svix 校验
	if id, ts, sigs, ok := inbound.SvixHeaders(c.GetHeader); ok {
		if err := inbound.VerifySvix(ch.WebhookSecret, body, id, ts, sigs, now); err != nil {
			log.Printf("api: svix verify failed (channel=%d): %v", ch.ID, err)
			unauthorized(c, "webhook 签名校验失败")
			return false
		}
		if !svixReplayCache.Claim("svix:" + id) {
			// 容差窗口内的重放（同 svix-id 已消费过）：拒绝（防重放第二层闸门）
			unauthorized(c, "webhook 事件重复")
			return false
		}
		return true
	}

	// 2) Mailgun：签名字段齐备即走 Mailgun 校验（JSON signature 信封与 form
	//    平铺两种形态统一按键名提取）
	sigFields := mailgunSignatureFields(body, form)
	if sigFields != nil {
		if err := inbound.VerifyMailgun(ch.WebhookSecret, sigFields.timestamp, sigFields.token, sigFields.signature, now); err != nil {
			log.Printf("api: mailgun verify failed (channel=%d): %v", ch.ID, err)
			unauthorized(c, "webhook 签名校验失败")
			return false
		}
		if !svixReplayCache.Claim("mailgun:" + sigFields.token) {
			unauthorized(c, "webhook 事件重复")
			return false
		}
		return true
	}

	// 3) 无签名标记：URL token 即凭据（路由已按 token 定位渠道），放行。
	return true
}

// mailgunSig 签名三元组（mailgunSignatureFields 提取结果）。
type mailgunSig struct{ timestamp, token, signature string }

// mailgunSignatureFields 从 JSON body（signature 信封对象或顶层平铺）与 form
// 报文中提取 Mailgun 签名三元组；三者齐备才返回非 nil（齐备即视为签名请求，
// 验败必拒，不回退 token）。
func mailgunSignatureFields(body []byte, form url.Values) *mailgunSig {
	grab := func(m map[string]any) *mailgunSig {
		extract := func(src map[string]any) *mailgunSig {
			ts, tok, sig := inbound.SigTriple(src)
			if ts != "" && tok != "" && sig != "" {
				return &mailgunSig{timestamp: ts, token: tok, signature: sig}
			}
			return nil
		}
		if sig := extract(inbound.NestedOrSelf(m, "signature")); sig != nil {
			return sig
		}
		return extract(m)
	}
	if form == nil {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil && m != nil {
			return grab(m)
		}
		return nil
	}
	get := func(k string) string {
		if vs := form[k]; len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	ts, tok, sig := get("timestamp"), get("token"), get("signature")
	if ts != "" && tok != "" && sig != "" {
		return &mailgunSig{timestamp: ts, token: tok, signature: sig}
	}
	return nil
}

// svixReplayCache 签名防重放缓存（Svix 按 svix-id、Mailgun 按 token 复用同一条
// 缓存；实现与 TTL 见 inbound/replay.go）。
var svixReplayCache = &inbound.ReplayCache{}

// isJSONBody 判断报文是否按 JSON 处理：Content-Type 含 json 或 body 以对象/数组
// 开头（部分平台不发标准 Content-Type）。
func isJSONBody(body []byte, contentType string) bool {
	if strings.Contains(contentType, "json") {
		return true
	}
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}

// rawArchive 构造 raw 存档串：JSON 报文原样；form 报文转 JSON 对象存档
// （mail_events.raw 列统一为 JSON 形态，前端详情页可格式化展示）。
func rawArchive(body []byte, form url.Values) string {
	if form != nil {
		m := make(map[string]any, len(form))
		for k, vs := range form {
			if len(vs) == 1 {
				m[k] = vs[0]
			} else {
				m[k] = vs
			}
		}
		if b, err := json.Marshal(m); err == nil {
			return string(b)
		}
		return "{}"
	}
	if json.Valid(body) {
		return string(body)
	}
	// 非 JSON（极端情况）：base64 兜底保真存档，避免二进制报文写坏 JSON 列语义
	return `{"encoding":"base64","data":"` + base64.StdEncoding.EncodeToString(body) + `"}`
}

// persistMailEvent 落库一条 MailEvent：尽力按 provider_message_id（去尖括号比对）
// 反查本渠道的 SendLog 关联，命中回填 MessageID/SendLogID。落库失败返回 nil
// （调用方 500，平台侧重试）。
func (s *Server) persistMailEvent(ch *model.Channel, ev inbound.Event, raw string) *model.MailEvent {
	row := &model.MailEvent{
		ChannelID:         ch.ID,
		ChannelName:       ch.Name,
		Provider:          ch.Provider,
		Event:             ev.Type,
		BounceType:        ev.BounceType,
		ProviderMessageID: ev.ProviderMessageID,
		Recipient:         ev.Recipient,
		Detail:            ev.Detail,
		Raw:               raw,
		OccurredAt:        ev.OccurredAt,
	}
	// 关联 SendLog：平台消息 ID 去尖括号/空白后等值（Mailgun 报文带 <>、API 返回
	// 亦带 <>；Resend/其他为裸 ID），限定本渠道并取最新一条（重发/多次发送场景）。
	if ev.ProviderMessageID != "" {
		trimmed := trimMessageID(ev.ProviderMessageID)
		var lg model.SendLog
		err := s.db.Where("channel_id = ? AND (provider_message_id = ? OR REPLACE(REPLACE(REPLACE(provider_message_id, '<', ''), '>', ''), ' ', '') = ?)",
			ch.ID, ev.ProviderMessageID, trimmed).
			Order("id DESC").First(&lg).Error
		if err == nil {
			row.MessageID, row.SendLogID = lg.MessageID, lg.ID
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("api: mail event sendlog lookup failed (channel=%d): %v", ch.ID, err)
		}
	}
	if err := s.db.Create(row).Error; err != nil {
		log.Printf("api: mail event persist failed (channel=%d event=%s): %v", ch.ID, ev.Type, err)
		return nil
	}
	return row
}

// trimMessageID 去掉消息 ID 两端的尖括号与空白（关联比对口径）。
func trimMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">")
	return strings.TrimSpace(id)
}

// autoSuppress 抑制列表自动入库：hard bounce 与 complained 事件（设置开关
// suppression_auto_enabled，默认开）按 email 全局唯一 upsert（DoNothing：手动
// 添加与既有记录不覆盖，首次原因与来源渠道留档）。失败仅记日志，不影响摄取响应。
func (s *Server) autoSuppress(ch *model.Channel, ev inbound.Event) {
	if ev.Recipient == "" {
		return
	}
	isHardBounce := ev.Type == model.MailEventBounced && ev.BounceType == model.BounceTypeHard
	if !isHardBounce && ev.Type != model.MailEventComplained {
		return
	}
	var setting model.Setting
	if err := s.db.Limit(1).Find(&setting, 1).Error; err != nil {
		log.Printf("api: 抑制自动入库读取设置失败: %v", err)
		return
	}
	if !setting.SuppressionAutoEnabled {
		return
	}
	reason := model.SuppressReasonBounce
	if ev.Type == model.MailEventComplained {
		reason = model.SuppressReasonComplaint
	}
	row := model.Suppression{
		Email:       strings.ToLower(strings.TrimSpace(ev.Recipient)),
		Reason:      reason,
		ChannelID:   ch.ID,
		ChannelName: ch.Name,
		Detail:      ev.Detail,
	}
	if err := s.db.Create(&row).Error; err != nil && !errors.Is(err, gorm.ErrDuplicatedKey) {
		log.Printf("api: 抑制自动入库失败（%s）: %v", row.Email, err)
	}
}
