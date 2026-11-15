// mailgun.go Mailgun 平台适配器：region=eu 用 EU 节点，Basic api:<api_key>，multipart/form-data（SPEC §3）。
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"strings"

	"catmail/internal/model"
)

var (
	mailgunBaseUS = "https://api.mailgun.net"
	mailgunBaseEU = "https://api.eu.mailgun.net"
)

// Mailgun Mailgun 平台适配器。
type Mailgun struct{}

func (Mailgun) Name() string { return "mailgun" }

// mailgunEndpoint region 为 eu 时用 EU 节点，否则 US；路径 /v3/{domain}/messages。
func mailgunEndpoint(region, domain string) string {
	base := mailgunBaseUS
	if strings.EqualFold(strings.TrimSpace(region), "eu") {
		base = mailgunBaseEU
	}
	return base + "/v3/" + strings.TrimSpace(domain) + "/messages"
}

var formQuoteEscaper = strings.NewReplacer("\\", "\\\\", "\"", "\\\"")

// writeFormFile 写入一个 file part，filename 原样，Content-Type 按扩展名猜测。
func writeFormFile(w *multipart.Writer, field, filename string, data []byte) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="%s"; filename="%s"`, formQuoteEscaper.Replace(field), formQuoteEscaper.Replace(filename)))
	h.Set("Content-Type", guessContentType(filename))
	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = part.Write(data)
	return err
}

// Send 经 Mailgun API 发送（multipart/form-data），成功取响应 id。
func (Mailgun) Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError) {
	if strings.TrimSpace(ch.Domain) == "" {
		return nil, &ProviderError{Code: "channel_config", Message: "mailgun: 发送域名未配置", Retryable: true}
	}
	client, err := BuildClient(ch.ProxyURL)
	if err != nil {
		return nil, &ProviderError{Code: "proxy_error", Message: err.Error(), Retryable: true}
	}
	body, perr := PostMultipart(ctx, client, mailgunEndpoint(ch.Region, ch.Domain), map[string]string{
		"Authorization": basicAuth("api", ch.APIKey),
	}, func(w *multipart.Writer) error {
		if err := w.WriteField("from", FormatFrom(m)); err != nil {
			return err
		}
		for _, a := range m.To { // to/cc/bcc 可重复字段，每地址一个 part；官方接受 "Name <addr>" 全格式，原样透传不提取
			if err := w.WriteField("to", a); err != nil {
				return err
			}
		}
		for _, a := range m.Cc {
			if err := w.WriteField("cc", a); err != nil {
				return err
			}
		}
		for _, a := range m.Bcc {
			if err := w.WriteField("bcc", a); err != nil {
				return err
			}
		}
		if err := w.WriteField("subject", m.Subject); err != nil {
			return err
		}
		if m.Text != "" {
			if err := w.WriteField("text", m.Text); err != nil {
				return err
			}
		}
		if m.HTML != "" {
			if err := w.WriteField("html", m.HTML); err != nil {
				return err
			}
		}
		for k, v := range m.Headers {
			if v == "" || strings.EqualFold(k, "Reply-To") { // Reply-To 由下方 ReplyTo 循环统一写，避免重复发头
				continue
			}
			if err := w.WriteField("h:"+k, v); err != nil {
				return err
			}
		}
		for _, r := range m.ReplyTo {
			if err := w.WriteField("h:Reply-To", r); err != nil {
				return err
			}
		}
		for _, t := range m.Tags { // 官方支持重复 o:tag part，每个 tag 写一个
			if err := w.WriteField("o:tag", t.Value); err != nil {
				return err
			}
		}
		for _, a := range m.Attachments { // 多附件发多个 file part；base64 先剔除空白再解码
			data, derr := decodeBase64(a.ContentBase64)
			if derr != nil {
				return fmt.Errorf("attachment %q: %w", a.Filename, derr)
			}
			if err := writeFormFile(w, "attachment", a.Filename, data); err != nil {
				return err
			}
		}
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &out) != nil || out.ID == "" {
		return nil, &ProviderError{Code: "unexpected_response",
			Message: "mailgun: missing id in response | body: " + truncate(string(body), maxErrorBody)}
	}
	return &Result{ProviderMessageID: out.ID}, nil
}

func init() {
	Register(Mailgun{})
}
