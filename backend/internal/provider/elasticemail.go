// elasticemail.go Elastic Email 平台适配器：POST https://api.elasticemail.com/v4/emails/transactional，
// header X-ElasticEmail-ApiKey，JSON（官方 OpenAPI 规范 api.elasticemail.com/v4，SPEC §3）。
package provider

import (
	"context"
	"encoding/json"

	"catmail/internal/model"
)

// elasticEmailBaseURL 平台 v4 根地址（官方 OpenAPI servers 字段），包级变量供测试注入 httptest 假服务覆盖。
var elasticEmailBaseURL = "https://api.elasticemail.com/v4"

// ElasticEmail Elastic Email 平台适配器。
type ElasticEmail struct{}

func (ElasticEmail) Name() string { return "elasticemail" }

// elasticEmailBodyPart 正文片段：ContentType 取官方枚举 HTML/PlainText（另有 AMP/CSS）。
type elasticEmailBodyPart struct {
	ContentType string `json:"ContentType"`
	Content     string `json:"Content"`
}

type elasticEmailAttachment struct {
	BinaryContent string `json:"BinaryContent"` // base64 字符串（官方：byte array or a Base64 string）
	Name          string `json:"Name"`
	ContentType   string `json:"ContentType,omitempty"`
}

type elasticEmailRecipients struct {
	To  []string `json:"To"` // 官方必填；元素为收件人串，接受 "Name <addr>" 全格式
	CC  []string `json:"CC,omitempty"`
	BCC []string `json:"BCC,omitempty"`
}

type elasticEmailContent struct {
	From        string                   `json:"From"` // "Name <addr>" 全格式
	ReplyTo     string                   `json:"ReplyTo,omitempty"`
	Subject     string                   `json:"Subject,omitempty"`
	Body        []elasticEmailBodyPart   `json:"Body,omitempty"`
	Attachments []elasticEmailAttachment `json:"Attachments,omitempty"`
	Headers     map[string]string        `json:"Headers,omitempty"`
}

type elasticEmailPayload struct {
	Recipients elasticEmailRecipients `json:"Recipients"`
	Content    elasticEmailContent    `json:"Content"`
}

// Send 经 Elastic Email v4 API 发送，成功取响应 MessageID（缺失时回退 TransactionID）。
func (ElasticEmail) Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError) {
	client, err := BuildClient(ch.ProxyURL)
	if err != nil {
		return nil, &ProviderError{Code: "proxy_error", Message: err.Error(), Retryable: true}
	}
	content := elasticEmailContent{
		From:    FormatFrom(m),
		Subject: m.Subject,
		Headers: m.Headers,
	}
	if m.HTML != "" {
		content.Body = append(content.Body, elasticEmailBodyPart{ContentType: "HTML", Content: m.HTML})
	}
	if m.Text != "" {
		content.Body = append(content.Body, elasticEmailBodyPart{ContentType: "PlainText", Content: m.Text})
	}
	if len(m.ReplyTo) > 0 { // 官方为单个字符串，多个回信地址逗号分隔
		content.ReplyTo = joinNonEmpty(m.ReplyTo, ",")
	}
	for _, a := range m.Attachments {
		content.Attachments = append(content.Attachments, elasticEmailAttachment{
			BinaryContent: a.ContentBase64,
			Name:          a.Filename,
			ContentType:   guessContentType(a.Filename),
		})
	}
	payload := elasticEmailPayload{
		Recipients: elasticEmailRecipients{To: m.To, CC: m.Cc, BCC: m.Bcc},
		Content:    content,
	}
	body, perr := PostJSON(ctx, client, elasticEmailBaseURL+"/emails/transactional", map[string]string{
		"X-ElasticEmail-ApiKey": ch.APIKey,
	}, payload)
	if perr != nil {
		return nil, perr
	}
	var out struct {
		MessageID     string `json:"MessageID"`
		TransactionID string `json:"TransactionID"`
	}
	if json.Unmarshal(body, &out) != nil || (out.MessageID == "" && out.TransactionID == "") {
		return nil, &ProviderError{Code: "unexpected_response",
			Message: "elasticemail: missing MessageID in response | body: " + truncate(string(body), maxErrorBody)}
	}
	id := out.MessageID
	if id == "" {
		id = out.TransactionID
	}
	return &Result{ProviderMessageID: id}, nil
}

// joinNonEmpty 以 sep 连接非空元素。
func joinNonEmpty(items []string, sep string) string {
	out := ""
	for _, s := range items {
		if s == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += s
	}
	return out
}

func init() {
	Register(ElasticEmail{})
}
