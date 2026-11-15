// webhook.go 回执测试推送通道：固定通用 JSON envelope（不做飞书/钉钉等机器人
// payload 适配——回执目标是调用方自己的自动化端点）。复用 provider 的 HTTP 传输栈
// （BuildClient/PostJSON），单次 5 秒超时；真实回执投递走 outbox（退避重试/签名见
// internal/outbox），本文件只剩 Key 编辑弹窗「测试」按钮的单次直发。
package receipt

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"catmail/internal/httpsig"
	"catmail/internal/provider"
)

const (
	// pushTimeout 测试推送单次超时（outbox 真实投递的超时在 outbox 包内）。
	pushTimeout = 5 * time.Second
)

// httpClient 直连出站客户端（回执走全局出站，不用渠道代理），懒构建一次。
var httpClient = sync.OnceValue(func() *http.Client {
	c, err := provider.BuildClient("")
	if err != nil { // 空代理串不会走到这里，防御兜底：请求仍受 ctx 5s 超时约束
		return &http.Client{}
	}
	return c
})

// TestWebhook 向 endpoint 发送一条测试回执（与真实回执相同的 envelope 格式，
// event="test"，并按 secret 签名——与 outbox 真实投递同一签名契约），返回错误供
// Key 编辑弹窗「测试」按钮直接展示；单次尝试不重试，5s 超时。secret 为空 = 不签名
// 不发头（契约口径：配置 secret 后才有签名头）。handler 见 api 层 testKeyWebhook。
func TestWebhook(apiKeyID uint, endpoint, secret string) error {
	payload := envelope{
		Source: Source, Event: "test", APIKeyID: apiKeyID,
		Data: map[string]string{"note": "收到即表示回执 Webhook 配置成功。"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	headers := map[string]string{}
	if h := httpsig.Sign(secret, raw, time.Now()); h != "" {
		headers[httpsig.HeaderName] = h
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	_, perr := provider.PostJSON(ctx, httpClient(), endpoint, headers, json.RawMessage(raw))
	if perr == nil {
		return nil
	}
	return perr
}
