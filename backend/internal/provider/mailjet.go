// mailjet.go Mailjet 平台适配器：POST https://api.mailjet.com/v3.1/send，Basic api_key:api_secret，JSON（SPEC §3）。
package provider

import (
	"context"
	"encoding/json"
	"strconv"

	"catmail/internal/model"
)

// mailjetBaseURL 平台根地址，包级变量供测试注入 httptest 假服务覆盖。
var mailjetBaseURL = "https://api.mailjet.com"

// Mailjet Mailjet 平台适配器。
type Mailjet struct{}

func (Mailjet) Name() string { return "mailjet" }

type mailjetContact struct {
	Email string `json:"Email"`
	Name  string `json:"Name,omitempty"`
}

type mailjetAttachment struct {
	Filename      string `json:"Filename"`
	ContentType   string `json:"ContentType"`
	Base64Content string `json:"Base64Content"`
}

type mailjetMessage struct {
	From        mailjetContact      `json:"From"`
	To          []mailjetContact    `json:"To"`
	Cc          []mailjetContact    `json:"Cc,omitempty"`
	Bcc         []mailjetContact    `json:"Bcc,omitempty"`
	ReplyTo     *mailjetContact     `json:"ReplyTo,omitempty"` // SPEC §3：ReplyTo={Email}
	Subject     string              `json:"Subject"`
	TextPart    string              `json:"TextPart,omitempty"`
	HTMLPart    string              `json:"HTMLPart,omitempty"`
	Headers     map[string]string   `json:"Headers,omitempty"`
	Attachments []mailjetAttachment `json:"Attachments,omitempty"`
}

// mailjetContacts 地址列表转 [{Email}]：dto 保留 "Name <addr>" 原始串，官方 Email 字段仅收裸地址，先提取。
func mailjetContacts(addrs []string) []mailjetContact {
	out := make([]mailjetContact, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, mailjetContact{Email: extractBareAddress(a)})
	}
	return out
}

// Send 经 Mailjet v3.1 发送，成功取 Messages[0].To[0].MessageID（数字转字符串）。
func (Mailjet) Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError) {
	client, err := BuildClient(ch.ProxyURL)
	if err != nil {
		return nil, &ProviderError{Code: "proxy_error", Message: err.Error(), Retryable: true}
	}
	msg := mailjetMessage{
		From:     mailjetContact{Email: m.FromAddr, Name: m.FromName},
		To:       mailjetContacts(m.To),
		Cc:       mailjetContacts(m.Cc),
		Bcc:      mailjetContacts(m.Bcc),
		Subject:  m.Subject,
		TextPart: m.Text,
		HTMLPart: m.HTML,
		Headers:  m.Headers,
	}
	if len(m.ReplyTo) > 0 { // 官方为单对象 {Email}，email 仅收裸地址
		msg.ReplyTo = &mailjetContact{Email: extractBareAddress(m.ReplyTo[0])}
	}
	for _, a := range m.Attachments {
		msg.Attachments = append(msg.Attachments, mailjetAttachment{
			Filename:      a.Filename,
			ContentType:   guessContentType(a.Filename),
			Base64Content: a.ContentBase64,
		})
	}
	body, perr := PostJSON(ctx, client, mailjetBaseURL+"/v3.1/send", map[string]string{
		"Authorization": basicAuth(ch.APIKey, ch.APISecret),
	}, struct {
		Messages []mailjetMessage `json:"Messages"`
	}{Messages: []mailjetMessage{msg}})
	if perr != nil {
		return nil, perr
	}
	var out struct {
		Messages []struct {
			Status string `json:"Status"`
			To     []struct {
				Email     string `json:"Email"`
				MessageID int64  `json:"MessageID"`
			} `json:"To"`
		} `json:"Messages"`
	}
	if json.Unmarshal(body, &out) != nil || len(out.Messages) == 0 || len(out.Messages[0].To) == 0 || out.Messages[0].To[0].MessageID == 0 {
		return nil, &ProviderError{Code: "unexpected_response",
			Message: "mailjet: missing Messages[0].To[0].MessageID in response | body: " + truncate(string(body), maxErrorBody)}
	}
	return &Result{ProviderMessageID: strconv.FormatInt(out.Messages[0].To[0].MessageID, 10)}, nil
}

func init() {
	Register(Mailjet{})
}
