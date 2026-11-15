// brevo.go Brevo 平台适配器：POST https://api.brevo.com/v3/smtp/email，header api-key，JSON（SPEC §3）。
package provider

import (
	"context"
	"encoding/json"

	"catmail/internal/model"
)

// brevoBaseURL 平台根地址，包级变量供测试注入 httptest 假服务覆盖。
var brevoBaseURL = "https://api.brevo.com"

// Brevo Brevo 平台适配器。
type Brevo struct{}

func (Brevo) Name() string { return "brevo" }

type brevoContact struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type brevoAttachment struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type brevoPayload struct {
	Sender     brevoContact      `json:"sender"`
	To         []brevoContact    `json:"to"`
	Cc         []brevoContact    `json:"cc,omitempty"`
	Bcc        []brevoContact    `json:"bcc,omitempty"`
	ReplyTo    *brevoContact     `json:"replyTo,omitempty"` // 官方 schema：replyTo 为单对象 {email,name}
	Subject    string            `json:"subject"`
	HTML       string            `json:"htmlContent,omitempty"`
	Text       string            `json:"textContent,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Attachment []brevoAttachment `json:"attachment,omitempty"`
	Tags       []string          `json:"tags,omitempty"` // 取 tag.value
}

// brevoContacts 地址列表转 [{email}]：dto 保留 "Name <addr>" 原始串，官方 email 字段仅收裸地址，先提取。
func brevoContacts(addrs []string) []brevoContact {
	out := make([]brevoContact, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, brevoContact{Email: extractBareAddress(a)})
	}
	return out
}

// Send 经 Brevo API 发送，成功取响应 messageId。
func (Brevo) Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError) {
	client, err := BuildClient(ch.ProxyURL)
	if err != nil {
		return nil, &ProviderError{Code: "proxy_error", Message: err.Error(), Retryable: true}
	}
	payload := brevoPayload{
		Sender:  brevoContact{Name: m.FromName, Email: m.FromAddr},
		To:      brevoContacts(m.To),
		Cc:      brevoContacts(m.Cc),
		Bcc:     brevoContacts(m.Bcc),
		Subject: m.Subject,
		HTML:    m.HTML,
		Text:    m.Text,
		Headers: m.Headers,
	}
	if len(m.ReplyTo) > 0 { // 官方 schema 为单对象 {email,name}，多个 Reply-To 时取第一个；email 仅收裸地址
		payload.ReplyTo = &brevoContact{Email: extractBareAddress(m.ReplyTo[0])}
	}
	for _, a := range m.Attachments {
		payload.Attachment = append(payload.Attachment, brevoAttachment{Name: a.Filename, Content: a.ContentBase64})
	}
	for _, t := range m.Tags {
		payload.Tags = append(payload.Tags, t.Value)
	}
	body, perr := PostJSON(ctx, client, brevoBaseURL+"/v3/smtp/email", map[string]string{
		"api-key": ch.APIKey,
	}, payload)
	if perr != nil {
		return nil, perr
	}
	var out struct {
		MessageID string `json:"messageId"`
	}
	if json.Unmarshal(body, &out) != nil || out.MessageID == "" {
		return nil, &ProviderError{Code: "unexpected_response",
			Message: "brevo: missing messageId in response | body: " + truncate(string(body), maxErrorBody)}
	}
	return &Result{ProviderMessageID: out.MessageID}, nil
}

func init() {
	Register(Brevo{})
}
