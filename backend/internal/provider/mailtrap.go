// mailtrap.go Mailtrap 平台适配器：POST https://send.api.mailtrap.io/api/send，header Api-Token，JSON
// （官方 docs.mailtrap.io sending API；body 与 Resend 同构但 from 为 {email,name} 对象，SPEC §3）。
package provider

import (
	"context"
	"encoding/json"

	"catmail/internal/model"
)

// mailtrapBaseURL 平台根地址，包级变量供测试注入 httptest 假服务覆盖。
var mailtrapBaseURL = "https://send.api.mailtrap.io"

// Mailtrap Mailtrap 平台适配器。
type Mailtrap struct{}

func (Mailtrap) Name() string { return "mailtrap" }

type mailtrapContact struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type mailtrapAttachment struct {
	Filename    string `json:"filename"`
	Content     string `json:"content"` // base64 编码内容
	Type        string `json:"type,omitempty"`
	Disposition string `json:"disposition,omitempty"` // attachment|inline，默认 attachment
}

type mailtrapPayload struct {
	From        mailtrapContact      `json:"from"`
	To          []mailtrapContact    `json:"to"` // 官方每数组上限 1000
	Cc          []mailtrapContact    `json:"cc,omitempty"`
	Bcc         []mailtrapContact    `json:"bcc,omitempty"`
	ReplyTo     *mailtrapContact     `json:"reply_to,omitempty"` // 官方为单对象 {email,name}
	Subject     string               `json:"subject"`
	Text        string               `json:"text,omitempty"`
	HTML        string               `json:"html,omitempty"`
	Attachments []mailtrapAttachment `json:"attachments,omitempty"`
	Headers     map[string]string    `json:"headers,omitempty"`
}

// mailtrapContacts 地址列表转 [{email}]：dto 层保留 "Name <addr>" 原始串，官方 email 字段仅收邮箱格式，先提取裸地址。
func mailtrapContacts(addrs []string) []mailtrapContact {
	out := make([]mailtrapContact, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, mailtrapContact{Email: extractBareAddress(a)})
	}
	return out
}

// Send 经 Mailtrap sending API 发送，成功取响应 message_ids[0]（官方每收件人一个 id）。
func (Mailtrap) Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError) {
	client, err := BuildClient(ch.ProxyURL)
	if err != nil {
		return nil, &ProviderError{Code: "proxy_error", Message: err.Error(), Retryable: true}
	}
	payload := mailtrapPayload{
		From:    mailtrapContact{Name: m.FromName, Email: m.FromAddr},
		To:      mailtrapContacts(m.To),
		Cc:      mailtrapContacts(m.Cc),
		Bcc:     mailtrapContacts(m.Bcc),
		Subject: m.Subject,
		Text:    m.Text,
		HTML:    m.HTML,
		Headers: m.Headers,
	}
	if len(m.ReplyTo) > 0 { // 官方 schema 为单对象，多个 Reply-To 时取第一个
		payload.ReplyTo = &mailtrapContact{Email: extractBareAddress(m.ReplyTo[0])}
	}
	for _, a := range m.Attachments {
		payload.Attachments = append(payload.Attachments, mailtrapAttachment{
			Filename:    a.Filename,
			Content:     a.ContentBase64,
			Type:        guessContentType(a.Filename),
			Disposition: "attachment",
		})
	}
	body, perr := PostJSON(ctx, client, mailtrapBaseURL+"/api/send", map[string]string{
		"Api-Token": ch.APIKey,
	}, payload)
	if perr != nil {
		return nil, perr
	}
	var out struct {
		Success    bool     `json:"success"`
		MessageIDs []string `json:"message_ids"`
	}
	if json.Unmarshal(body, &out) != nil || !out.Success || len(out.MessageIDs) == 0 {
		return nil, &ProviderError{Code: "unexpected_response",
			Message: "mailtrap: missing message_ids in response | body: " + truncate(string(body), maxErrorBody)}
	}
	return &Result{ProviderMessageID: out.MessageIDs[0]}, nil
}

func init() {
	Register(Mailtrap{})
}
