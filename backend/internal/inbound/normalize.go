// normalize.go 入站送达事件的归一化：各平台 webhook 报文 → 统一 Event
// （sent/delivered/bounced[hard|soft]/complained/raw，SPEC「送达链路三件套」口径）。
//
// 事件名与字段以官方文档为准（已核对：Resend webhook events / Svix、Mailgun
// webhook payloads、Brevo transactional webhooks、Mailjet event API）；拿不准的
// 平台（SMTP2GO/Elastic Email/Mailtrap/aliyun 等）走 generic 映射，识别不了的
// 事件一律 raw 存档不猜测——宁缺勿滥，宁可损失一个事件的统计口径也不污染
// 送达率/退信率/投诉率。
package inbound

import (
	"encoding/json"
	"math"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"catmail/internal/model"
)

// Event 归一化后的统一事件（ingest handler 据此落 MailEvent 行）。
type Event struct {
	Type              string    // MailEvent* 常量
	BounceType        string    // hard|soft（仅 bounced；其余空串）
	Recipient         string    // 收件人裸地址（报文带显示名时提取 addr）
	ProviderMessageID string    // 平台消息 ID（原始形态，供反查 SendLog）
	OccurredAt        time.Time // 平台事件时间（缺省由调用方补摄取时刻）
	Detail            string    // 补充：原始事件名/退信原因/诊断码
}

// Normalize 入口：JSON 报文或 form 编码（Mailgun legacy webhook）二选一传入
// （form 非 nil 时优先，body 忽略）。无法识别的报文格式返回 raw 事件存档。
func Normalize(provider string, body []byte, form url.Values) Event {
	if form != nil {
		m := make(map[string]any, len(form))
		for k, vs := range form {
			if len(vs) == 1 {
				m[k] = vs[0]
			} else {
				m[k] = vs // 可重复字段（罕见）：原样保留数组
			}
		}
		return byProvider(provider, m)
	}
	var m map[string]any
	if json.Unmarshal(body, &m) == nil && m != nil {
		return byProvider(provider, m)
	}
	return Event{Type: model.MailEventRaw, Detail: "未识别的报文格式", OccurredAt: time.Now()}
}

// byProvider 按平台分发；未知平台走 generic 兜底。
func byProvider(provider string, m map[string]any) Event {
	switch provider {
	case "resend":
		return normalizeResend(m)
	case "mailgun":
		return normalizeMailgun(m)
	case "brevo":
		return normalizeBrevo(m)
	case "mailjet":
		return normalizeMailjet(m)
	default:
		return normalizeGeneric(m)
	}
}

// ---------- 通用取值助手（JSON map 与 form 转换 map 双形态兼容） ----------

// nestedMap 逐层下探嵌套对象（每层必须为 map[string]any），取不到返回 nil。
func nestedMap(m map[string]any, keys ...string) map[string]any {
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// asString 宽容取字符串：string 原样；float64（JSON 数字）为整数时去小数点；
// 数组取首项。空值返回空串。
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == math.Trunc(t) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		if len(t) > 0 {
			return asString(t[0])
		}
	case json.Number:
		return t.String()
	}
	return ""
}

// firstString 依次取候选键的第一个非空字符串值。
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := asString(m[k]); s != "" {
			return s
		}
	}
	return ""
}

// anyTime 宽容解析事件时间：unix 秒（JSON 数字或字符串）/ RFC3339 / 常见
// 日期串；全部失败返回零值（调用方兜底摄取时刻）。
func anyTime(v any) time.Time {
	switch t := v.(type) {
	case float64:
		return time.Unix(int64(t), 0)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return time.Time{}
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return time.Unix(int64(n), 0)
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts
			}
		}
	case json.Number:
		if n, err := t.Float64(); err == nil {
			return time.Unix(int64(n), 0)
		}
	}
	return time.Time{}
}

// eventTime 依次尝试候选键取事件时间，全取不到回退 now。
func eventTime(m map[string]any, keys ...string) time.Time {
	for _, k := range keys {
		if ts := anyTime(m[k]); !ts.IsZero() {
			return ts
		}
	}
	return time.Now()
}

// bareAddr 宽容提取裸地址："Name <a@b>" 取 a@b；解析失败原样返回（trim 后）。
func bareAddr(s string) string {
	s = strings.TrimSpace(s)
	if a, err := mail.ParseAddress(s); err == nil && a.Address != "" {
		return a.Address
	}
	return s
}

// recipientOf 收件人候选键提取（数组取首项、剥显示名），小写归一。
func recipientOf(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := asString(m[k]); s != "" {
			return strings.ToLower(bareAddr(s))
		}
	}
	return ""
}

// messageIDKey 常见平台消息 ID 字段名（generic 兜底用；按出现优先级排序）。
var messageIDKeys = []string{"message_id", "message-id", "messageId", "email_id", "emailId", "MessageID", "id"}

// classifyEvent 共享事件名归类：返回 (统一类型, 退信类型, 是否识别)。
// 裸 bounce/bounced 在 generic 口径按 soft 处理——hard 误判会自动进抑制列表
// （不可逆地拦掉合法收件人），soft 误判只是少统计一次退信，宁可保守。
func classifyEvent(name string) (string, string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, "-", "_")
	switch {
	case n == "":
		return "", "", false
	case n == "sent", n == "request", n == "accepted", n == "queued":
		return model.MailEventSent, "", true
	case n == "delivered":
		return model.MailEventDelivered, "", true
	case n == "complained", n == "spam", n == "complaint":
		return model.MailEventComplained, "", true
	case strings.Contains(n, "bounce"):
		switch {
		case strings.Contains(n, "hard") || strings.Contains(n, "permanent"):
			return model.MailEventBounced, model.BounceTypeHard, true
		case strings.Contains(n, "soft") || strings.Contains(n, "temporary"):
			return model.MailEventBounced, model.BounceTypeSoft, true
		default:
			return model.MailEventBounced, model.BounceTypeSoft, true
		}
	default:
		return model.MailEventRaw, "", true
	}
}

// ---------- 各平台归一化 ----------

// normalizeResend Resend（Svix 信封）：{"type":"email.delivered","created_at",
// "data":{email_id, message_id(RFC Message-ID), to[], bounce{message,diagnosticCode[]}}}
// （官方 webhook events 文档）。email_id 与发送 API 返回的 id 同源，优先反查。
// email.bounced 官方语义为「永久拒绝」→ hard。
func normalizeResend(m map[string]any) Event {
	rawType := asString(m["type"])
	data := nestedMap(m, "data")
	if data == nil {
		data = map[string]any{}
	}
	ev := Event{
		OccurredAt:        eventTime(m, "created_at"),
		ProviderMessageID: firstString(data, "email_id", "message_id"),
		Recipient:         recipientOf(data, "to", "email", "recipient"),
	}
	ev.Type, ev.BounceType, _ = classifyEvent(strings.TrimPrefix(rawType, "email."))
	// Resend 自有映射（不依赖共享 classifyEvent：email.bounced 官方语义固定
	// hard；delivered/sent/complained 之外的事件一律 raw 存档）。
	switch rawType {
	case "email.bounced":
		ev.Type, ev.BounceType = model.MailEventBounced, model.BounceTypeHard
	case "email.delivered":
		ev.Type = model.MailEventDelivered
	case "email.sent":
		ev.Type = model.MailEventSent
	case "email.complained":
		ev.Type = model.MailEventComplained
	default: // delivery_delayed/failed/opened/clicked/未知事件/缺事件名 → raw 存档
		ev.Type = model.MailEventRaw
	}
	if rawType != "" {
		ev.Detail = "原始事件: " + rawType
	}
	if ev.Type == model.MailEventBounced {
		b := nestedMap(m, "data", "bounce")
		if b != nil {
			msg := asString(b["message"])
			diag := asString(b["diagnosticCode"])
			switch {
			case msg != "" && diag != "":
				ev.Detail += " | " + msg + " | " + diag
			case msg != "":
				ev.Detail += " | " + msg
			case diag != "":
				ev.Detail += " | " + diag
			}
		}
	}
	return ev
}

// normalizeMailgun Mailgun：现行 webhook 为 JSON（signature 信封 + event 顶层
// 字段），legacy 为 form 编码（Normalize 已转 map，同一套键名）。字段见官方
// webhook payloads：event/timestamp/recipient/message.headers.message-id/
// delivery-status{code,message,description,bounce-type}/severity/reason。
func normalizeMailgun(m map[string]any) Event {
	rawType := asString(m["event"])
	ev := Event{
		Recipient:  recipientOf(m, "recipient", "email"),
		OccurredAt: eventTime(m, "timestamp", "ts"),
	}
	// 消息 ID：message.headers.message-id（新 JSON）；legacy form 为
	// message-headers（form 值为 JSON 数组字符串 [["key","value"],...]），
	// 兜底 message-id。
	if headers := nestedMap(m, "message", "headers"); headers != nil {
		ev.ProviderMessageID = asString(headers["message-id"])
	}
	if ev.ProviderMessageID == "" {
		ev.ProviderMessageID = messageIDFromHeaderList(m["message-headers"])
	}
	if ev.ProviderMessageID == "" {
		ev.ProviderMessageID = firstString(m, "message-id", "message_id")
	}

	switch rawType {
	case "accepted":
		ev.Type = model.MailEventSent
	case "delivered":
		ev.Type = model.MailEventDelivered
	case "failed":
		// failed 按 severity/bounce-type 分永久/临时：permanent 或 bounce-type
		// hard → hard（自动进抑制列表）；temporary → soft。
		ev.Type = model.MailEventBounced
		if strings.EqualFold(asString(m["severity"]), "permanent") ||
			strings.EqualFold(firstString(nestedMap(m, "delivery-status"), "bounce-type"), "hard") {
			ev.BounceType = model.BounceTypeHard
		} else {
			ev.BounceType = model.BounceTypeSoft
		}
	case "bounced": // legacy form/JSON 事件名：官方定义为永久退信
		ev.Type, ev.BounceType = model.MailEventBounced, model.BounceTypeHard
	case "soft-bounced", "soft_bounced": // legacy 临时退信事件名
		ev.Type, ev.BounceType = model.MailEventBounced, model.BounceTypeSoft
	case "complained":
		ev.Type = model.MailEventComplained
	default:
		ev.Type = model.MailEventRaw
	}
	ev.Detail = "原始事件: " + rawType
	if rawType == "failed" || strings.Contains(rawType, "bounce") {
		ds := nestedMap(m, "delivery-status")
		parts := make([]string, 0, 3)
		if code := asString(ds["code"]); code != "" && code != "null" {
			parts = append(parts, "code="+code)
		}
		if msg := asString(ds["message"]); msg != "" && msg != "null" {
			parts = append(parts, msg)
		}
		if reason := asString(m["reason"]); reason != "" {
			parts = append(parts, "reason="+reason)
		}
		if len(parts) > 0 {
			ev.Detail += " | " + strings.Join(parts, " | ")
		}
	}
	return ev
}

// messageIDFromHeaderList legacy Mailgun form 的 message-headers 字段：
// JSON 数组 [["From",".."],["Message-Id","<..>"],...]（form 值为 JSON 字符串，
// 先解码再提取），提取 Message-Id 值。
func messageIDFromHeaderList(v any) string {
	switch t := v.(type) {
	case string:
		var arr []any
		if json.Unmarshal([]byte(t), &arr) != nil {
			return ""
		}
		return headerListMessageID(arr)
	case []any:
		return headerListMessageID(t)
	}
	return ""
}

// headerListMessageID 从 [["key","value"],...] 数组提取 message-id 值。
func headerListMessageID(arr []any) string {
	for _, item := range arr {
		pair, ok := item.([]any)
		if !ok || len(pair) != 2 {
			continue
		}
		if strings.EqualFold(asString(pair[0]), "message-id") {
			return asString(pair[1])
		}
	}
	return ""
}

// normalizeBrevo Brevo（Sendinblue）transactional webhooks：JSON 平铺
// {event, email, message-id, ts_event/ts/ts_epoch/date, reason?}。事件枚举
// （官方 transactional-webhooks）：request/delivered/soft_bounce/hard_bounce/
// spam/opened/unique_opened/click/deferred/invalid_email/blocked/error/unsubscribed。
func normalizeBrevo(m map[string]any) Event {
	rawType := asString(m["event"])
	ev := Event{
		Recipient:         recipientOf(m, "email", "recipient"),
		ProviderMessageID: firstString(m, "message-id", "message_id"),
		OccurredAt:        eventTime(m, "ts_event", "ts", "ts_epoch", "date"),
	}
	switch rawType {
	case "request":
		ev.Type = model.MailEventSent
	case "delivered":
		ev.Type = model.MailEventDelivered
	case "hard_bounce", "invalid_email": // invalid_email=地址永久无效，官方语义同硬退信
		ev.Type, ev.BounceType = model.MailEventBounced, model.BounceTypeHard
	case "soft_bounce":
		ev.Type, ev.BounceType = model.MailEventBounced, model.BounceTypeSoft
	case "spam":
		ev.Type = model.MailEventComplained
	default: // opened/click/deferred/blocked/error/unsubscribed 等 → raw
		ev.Type = model.MailEventRaw
	}
	ev.Detail = "原始事件: " + rawType
	if ev.Type == model.MailEventBounced {
		if reason := asString(m["reason"]); reason != "" {
			ev.Detail += " | " + reason
		}
	}
	return ev
}

// normalizeMailjet Mailjet event API：JSON 平铺 {event, message_id(数字), email,
// time/ts_event, hard_bounce(bool)?, error?}。事件枚举（官方 webhooks overview）：
// sent/open/click/bounce/spam/blocked/unsub。bounce 的 hard/soft 由 hard_bounce
// 布尔区分（缺省按 soft，保守不误进抑制列表）。
func normalizeMailjet(m map[string]any) Event {
	rawType := asString(m["event"])
	ev := Event{
		Recipient:         recipientOf(m, "email", "recipient"),
		ProviderMessageID: firstString(m, "message_id", "message-id", "messageId"),
		OccurredAt:        eventTime(m, "ts_event", "time", "ts"),
	}
	switch rawType {
	case "sent":
		ev.Type = model.MailEventSent
	case "bounce":
		ev.Type = model.MailEventBounced
		if b, ok := m["hard_bounce"].(bool); ok && b {
			ev.BounceType = model.BounceTypeHard
		} else {
			ev.BounceType = model.BounceTypeSoft
		}
	case "spam":
		ev.Type = model.MailEventComplained
	default: // open/click/blocked/unsub 等 → raw
		ev.Type = model.MailEventRaw
	}
	ev.Detail = "原始事件: " + rawType
	if ev.Type == model.MailEventBounced {
		if reason := asString(m["error"]); reason != "" {
			ev.Detail += " | " + reason
		}
	}
	return ev
}

// normalizeGeneric generic 兜底（SMTP2GO/Elastic Email/Mailtrap/aliyun/smtp/mock
// 及未来新平台）：按常见字段名提取事件名/收件人/消息 ID/时间，classifyEvent
// 归类；识别不了的事件名落 raw 存档。
func normalizeGeneric(m map[string]any) Event {
	rawType := firstString(m, "event", "type", "event_type", "EventType")
	ev := Event{
		Recipient:         recipientOf(m, "recipient", "email", "to"),
		ProviderMessageID: firstString(m, messageIDKeys...),
		OccurredAt:        eventTime(m, "timestamp", "ts_event", "ts", "time", "occurred_at", "date"),
	}
	var ok bool
	ev.Type, ev.BounceType, ok = classifyEvent(rawType)
	if !ok { // 连事件名都取不到：整条 raw 存档
		ev.Type = model.MailEventRaw
	}
	if rawType != "" {
		ev.Detail = "原始事件: " + rawType
	}
	return ev
}
