// webhook.go 通用 Webhook 通道：按 URL host 嗅探目标机器人并适配 payload，
// 未知域名发通用 JSON。复用 provider 的 HTTP 传输栈（BuildClient/PostJSON），
// 不重复实现 HTTP 细节；真实事件投递走 outbox（退避重试/签名见 internal/outbox），
// 本文件只剩 payload 构造与设置页「测试」按钮的单次直发。
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"catmail/internal/httpsig"
	"catmail/internal/provider"
)

const (
	// webhookTimeout 测试发送单次超时（outbox 真实投递的超时在 outbox 包内）。
	webhookTimeout = 5 * time.Second
)

// webhookClient 直连出站客户端（webhook 走全局出站，不用渠道代理），懒构建一次。
var webhookClient = sync.OnceValue(func() *http.Client {
	c, err := provider.BuildClient("")
	if err != nil { // 空代理串不会走到这里，防御兜底：请求仍受 ctx 5s 超时约束
		return &http.Client{}
	}
	return c
})

// webhookText 统一文本格式：机器人 payload 与通用 payload 共用。
func webhookText(title, body string) string {
	return "[CatMail] " + title + "\n" + body
}

// genericPayload 通用 Webhook JSON（自定义接收端），字段名按契约固定。
type genericPayload struct {
	Source    string `json:"source"`
	Event     string `json:"event"`
	Level     string `json:"level"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	ChannelID *uint  `json:"channel_id"` // 与具体渠道无关的事件为 null
	At        string `json:"at"`         // RFC3339
}

// buildPayload 按目标 host 适配 payload（域名固定可嗅探）：
// 飞书/钉钉/企业微信/Slack 机器人各取所需，其余一律通用 JSON。
// URL 畸形返回错误（调用方兜底记日志丢弃）。
func buildPayload(rawURL, event, level, title, body string, channelID *uint) (any, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	text := webhookText(title, body)
	switch target.Hostname() {
	case "open.feishu.cn":
		return map[string]any{"msg_type": "text", "content": map[string]any{"text": text}}, nil
	case "oapi.dingtalk.com", "qyapi.weixin.qq.com": // 钉钉与企业微信同为 {"msgtype":"text","text":{"content":…}}
		return map[string]any{"msgtype": "text", "text": map[string]any{"content": text}}, nil
	case "hooks.slack.com":
		return map[string]any{"text": text}, nil
	default:
		return genericPayload{
			Source: "catmail", Event: event, Level: level,
			Title: title, Body: body, ChannelID: channelID,
			At: time.Now().Format(time.RFC3339),
		}, nil
	}
}

// TestWebhook 向 endpoint 发送一条测试通知（走与真实事件相同的 host 适配逻辑，
// 并按 secret 签名——与 outbox 真实投递同一签名契约），返回错误供设置页「测试」
// 按钮直接展示；单次尝试不重试，5s 超时。secret 为空 = 不签名不发头（契约口径：
// 配置 secret 后才有签名头）。
func TestWebhook(endpoint, secret string) error {
	payload, err := buildPayload(endpoint, "test", LevelInfo, "CatMail 测试通知", "收到即表示 Webhook 配置成功。", nil)
	if err != nil {
		return fmt.Errorf("invalid webhook url: %w", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode test payload: %w", err)
	}
	headers := map[string]string{}
	if h := httpsig.Sign(secret, raw, time.Now()); h != "" {
		headers[httpsig.HeaderName] = h
	}
	ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
	defer cancel()
	_, perr := provider.PostJSON(ctx, webhookClient(), endpoint, headers, json.RawMessage(raw))
	if perr == nil {
		return nil
	}
	return perr
}
