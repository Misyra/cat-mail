// mock.go 本地模拟适配器：无网络请求，直接成功，延迟 50ms（SPEC §3）。
// 渠道 APIKey 设为魔法值可模拟失败，便于本地验证轮换器的重试/冷却/禁用路径：
//
//	fail            → 5xx 平台错误（可重试）
//	fail_rate_limit → 429 限流（可重试+冷却）
//	fail_auth       → 401 认证失败（禁用渠道）
package provider

import (
	"context"
	"time"

	"github.com/google/uuid"

	"catmail/internal/model"
)

// Mock 模拟发送适配器。
type Mock struct{}

func (Mock) Name() string { return "mock" }

// mockFailures 渠道 APIKey 魔法值 → 模拟的 ProviderError（其余值含空一律成功）。
var mockFailures = map[string]*ProviderError{
	"fail":            {HTTPStatus: 500, Code: "provider_error", Message: "mock: simulated provider failure", Retryable: true},
	"fail_rate_limit": {HTTPStatus: 429, Code: "rate_limited", Message: "mock: simulated rate limit", Retryable: true, Cooldown: true},
	"fail_auth":       {HTTPStatus: 401, Code: "unauthorized", Message: "mock: simulated auth failure", DisableChannel: true},
}

// Send 返回 mock-<uuid>；APIKey 命中魔法值时立即模拟对应失败。
func (Mock) Send(ctx context.Context, ch *model.Channel, _ *Message) (*Result, *ProviderError) {
	if pe, ok := mockFailures[ch.APIKey]; ok {
		return nil, pe
	}
	select {
	case <-ctx.Done():
		return nil, &ProviderError{Code: "context_canceled", Message: "context canceled", Retryable: true}
	case <-time.After(50 * time.Millisecond):
	}
	return &Result{ProviderMessageID: "mock-" + uuid.NewString()}, nil
}

func init() {
	Register(Mock{})
}
