// smtp2go.go SMTP2GO 平台适配器：POST https://api.smtp2go.com/v3/email/send，header X-Smtp2go-API-Key，JSON（SPEC §3）。
package provider

import (
	"context"
	"encoding/json"
	"strings"

	"catmail/internal/model"
)

// smtp2goBaseURL 平台根地址，包级变量供测试注入 httptest 假服务覆盖。
var smtp2goBaseURL = "https://api.smtp2go.com"

// SMTP2Go SMTP2GO 平台适配器。
type SMTP2Go struct{}

func (SMTP2Go) Name() string { return "smtp2go" }

type smtp2goHeader struct {
	Header string `json:"header"`
	Value  string `json:"value"`
}

type smtp2goAttachment struct {
	Filename string `json:"filename"`
	FileBlob string `json:"fileblob"` // 官方字段名为 fileblob
	Mimetype string `json:"mimetype"`
}

type smtp2goPayload struct {
	Sender        string              `json:"sender"`
	To            []string            `json:"to"`
	Cc            []string            `json:"cc,omitempty"`
	Bcc           []string            `json:"bcc,omitempty"`
	Subject       string              `json:"subject"`
	TextBody      string              `json:"text_body,omitempty"`
	HTMLBody      string              `json:"html_body,omitempty"`
	CustomHeaders []smtp2goHeader     `json:"custom_headers,omitempty"`
	Attachments   []smtp2goAttachment `json:"attachments,omitempty"`
}

// smtp2goForbiddenHeaders 官方禁止经 custom_headers 设置的结构性头（大小写不敏感）。
var smtp2goForbiddenHeaders = []string{"Content-Type", "Content-Transfer-Encoding", "MIME-Version"}

// smtp2goHeaderForbidden 判断自定义头是否落在禁设名单内。
func smtp2goHeaderForbidden(k string) bool {
	for _, f := range smtp2goForbiddenHeaders {
		if strings.EqualFold(k, f) {
			return true
		}
	}
	return false
}

// Send 经 SMTP2GO API 发送，成功取 data.email_id。
func (SMTP2Go) Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError) {
	client, err := BuildClient(ch.ProxyURL)
	if err != nil {
		return nil, &ProviderError{Code: "proxy_error", Message: err.Error(), Retryable: true}
	}
	payload := smtp2goPayload{
		Sender: FormatFrom(m), // 官方 sender 为 "Name <addr>" 全格式
		// 官方 to/cc/bcc 亦接受 "Name <addr>" 全格式（reference/send-standard-email），原样透传不提取
		To:       m.To,
		Cc:       m.Cc,
		Bcc:      m.Bcc,
		Subject:  m.Subject,
		TextBody: m.Text,
		HTMLBody: m.HTML,
	}
	// 官方请求不含 reply_to 字段（会被静默忽略）：Reply-To 按官方指定经 custom_headers 透传，多个时取第一个
	if len(m.ReplyTo) > 0 {
		payload.CustomHeaders = append(payload.CustomHeaders, smtp2goHeader{Header: "Reply-To", Value: m.ReplyTo[0]})
	}
	for k, v := range m.Headers { // 过滤官方禁止经 custom_headers 设置的结构性头
		if smtp2goHeaderForbidden(k) {
			continue
		}
		payload.CustomHeaders = append(payload.CustomHeaders, smtp2goHeader{Header: k, Value: v})
	}
	for _, a := range m.Attachments { // mimetype 按扩展名猜测，未知兜底 application/octet-stream
		payload.Attachments = append(payload.Attachments, smtp2goAttachment{
			Filename: a.Filename, FileBlob: a.ContentBase64, Mimetype: guessContentType(a.Filename),
		})
	}
	body, perr := PostJSON(ctx, client, smtp2goBaseURL+"/v3/email/send", map[string]string{
		"X-Smtp2go-API-Key": ch.APIKey,
	}, payload)
	if perr != nil {
		return nil, perr
	}
	var out struct {
		Data struct {
			EmailID string `json:"email_id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil || out.Data.EmailID == "" {
		return nil, &ProviderError{Code: "unexpected_response",
			Message: "smtp2go: missing data.email_id in response | body: " + truncate(string(body), maxErrorBody)}
	}
	return &Result{ProviderMessageID: out.Data.EmailID}, nil
}

func init() {
	Register(SMTP2Go{})
}
