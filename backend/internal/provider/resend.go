// resend.go Resend 平台适配器：POST https://api.resend.com/emails，Bearer api_key，JSON（SPEC §3）。
package provider

import (
	"context"
	"encoding/json"

	"catmail/internal/model"
)

// resendBaseURL 平台根地址，包级变量供测试注入 httptest 假服务覆盖。
var resendBaseURL = "https://api.resend.com"

// Resend Resend 平台适配器。
type Resend struct{}

func (Resend) Name() string { return "resend" }

type resendAttachment struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

type resendTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resendPayload struct {
	From        string             `json:"from"`
	To          []string           `json:"to"`
	Cc          []string           `json:"cc,omitempty"`
	Bcc         []string           `json:"bcc,omitempty"`
	ReplyTo     []string           `json:"reply_to,omitempty"`
	Subject     string             `json:"subject"`
	HTML        string             `json:"html,omitempty"`
	Text        string             `json:"text,omitempty"`
	Headers     map[string]string  `json:"headers,omitempty"`
	Attachments []resendAttachment `json:"attachments,omitempty"`
	Tags        []resendTag        `json:"tags,omitempty"`
}

// Send 经 Resend API 发送，成功取响应 id。
func (Resend) Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError) {
	client, err := BuildClient(ch.ProxyURL)
	if err != nil {
		return nil, &ProviderError{Code: "proxy_error", Message: err.Error(), Retryable: true}
	}
	payload := resendPayload{
		From:    FormatFrom(m),
		To:      m.To,
		Cc:      m.Cc,
		Bcc:     m.Bcc,
		ReplyTo: m.ReplyTo,
		Subject: m.Subject,
		HTML:    m.HTML,
		Text:    m.Text,
		Headers: m.Headers,
	}
	for _, a := range m.Attachments {
		payload.Attachments = append(payload.Attachments, resendAttachment{Filename: a.Filename, Content: a.ContentBase64})
	}
	for _, t := range m.Tags {
		payload.Tags = append(payload.Tags, resendTag(t))
	}
	body, perr := PostJSON(ctx, client, resendBaseURL+"/emails", map[string]string{
		"Authorization": "Bearer " + ch.APIKey,
	}, payload)
	if perr != nil {
		return nil, perr
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &out) != nil || out.ID == "" {
		return nil, &ProviderError{Code: "unexpected_response",
			Message: "resend: missing id in response | body: " + truncate(string(body), maxErrorBody)}
	}
	return &Result{ProviderMessageID: out.ID}, nil
}

func init() {
	Register(Resend{})
}
