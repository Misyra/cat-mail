// Package provider 定义发送适配器契约（SPEC §3）并维护注册表。
package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"catmail/internal/model"
)

// Attachment 附件：filename + base64 内容。
type Attachment struct {
	Filename      string
	ContentBase64 string
}

// decodeBase64 容错解码附件 base64：先剔除全部空白字符再解码（部分来源会在编码中间插入换行/空格）。
func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
}

// Tag 标签，适配器尽力透传。
type Tag struct {
	Name  string
	Value string
}

// Message 统一发送消息（Resend 风格扁平结构解析后的规范形态）。
type Message struct {
	FromName, FromAddr   string
	To, Cc, Bcc, ReplyTo []string // ReplyTo 可能来自 string 或 string[]
	Subject, Text, HTML  string
	Headers              map[string]string
	Attachments          []Attachment
	Tags                 []Tag
}

// Result 成功结果：平台返回的消息 ID。
type Result struct {
	ProviderMessageID string
}

// ProviderError 统一的适配器错误，字段供轮换器决定重试/禁用/冷却。
type ProviderError struct {
	HTTPStatus     int // 平台返回的 HTTP 状态码，网络错误为 0
	Code, Message  string
	Retryable      bool // 网络错误/429/5xx/402 = true
	DisableChannel bool // 401/403 = true
	Cooldown       bool // 429 = true
}

// Error 使 ProviderError 可作为 error 使用。
func (e *ProviderError) Error() string {
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("%s (http %d)", e.Message, e.HTTPStatus)
	}
	return e.Message
}

// Transport 平台适配器契约。
type Transport interface {
	Name() string
	Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError)
}

// registry 适配器注册表："resend" "brevo" "mailjet" "smtp2go" "mailgun" "smtp" "mock"。
var registry = map[string]Transport{}

// Register 注册（或覆盖）一个适配器。
func Register(t Transport) {
	registry[t.Name()] = t
}

// Get 按平台名获取适配器；未注册时 ok=false。
func Get(name string) (Transport, bool) {
	t, ok := registry[name]
	return t, ok
}

// Names 返回已注册的平台名（排序后）。
func Names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// notImplemented 本波次占位适配器：调用一律返回未实现错误，后续波次填充。
type notImplemented struct{ name string }

func (p *notImplemented) Name() string { return p.name }

func (p *notImplemented) Send(_ context.Context, _ *model.Channel, _ *Message) (*Result, *ProviderError) {
	return nil, &ProviderError{Code: "not_implemented", Message: "provider not implemented yet"}
}

func init() {
	// 已有真实实现的平台优先（后续波次的适配器文件会注册同名覆盖占位）。
	Register(Mock{})
	for _, name := range []string{"resend", "brevo", "mailjet", "smtp2go", "mailgun", "smtp"} {
		if _, ok := registry[name]; !ok {
			Register(&notImplemented{name: name})
		}
	}
}
