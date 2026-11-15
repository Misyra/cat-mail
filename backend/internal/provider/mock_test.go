// mock_test.go Mock 适配器单测：成功返回与 APIKey 魔法值触发的模拟失败（供本地测试轮换器路径）。
package provider

import (
	"context"
	"strings"
	"testing"

	"catmail/internal/model"
)

// TestMockSendSuccess 默认成功：返回 mock-<uuid>。
func TestMockSendSuccess(t *testing.T) {
	res, perr := Mock{}.Send(context.Background(), &model.Channel{}, testMessage())
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if !strings.HasPrefix(res.ProviderMessageID, "mock-") {
		t.Fatalf("provider id = %q, want mock-<uuid>", res.ProviderMessageID)
	}
}

// TestMockSendFailures 修复锁定：渠道 APIKey 设为魔法值可模拟三类失败
// fail→5xx 可重试 / fail_rate_limit→429 重试+冷却 / fail_auth→401 禁用渠道。
func TestMockSendFailures(t *testing.T) {
	cases := []struct {
		apiKey                       string
		wantStatus                   int
		retryable, disable, cooldown bool
	}{
		{"fail", 500, true, false, false},
		{"fail_rate_limit", 429, true, false, true},
		{"fail_auth", 401, false, true, false},
	}
	for _, tc := range cases {
		_, perr := Mock{}.Send(context.Background(), &model.Channel{APIKey: tc.apiKey}, testMessage())
		if perr == nil {
			t.Fatalf("apiKey=%q: expected error, got nil", tc.apiKey)
		}
		if perr.HTTPStatus != tc.wantStatus || perr.Retryable != tc.retryable || perr.DisableChannel != tc.disable || perr.Cooldown != tc.cooldown {
			t.Fatalf("apiKey=%q: got %+v, want status=%d retry=%v disable=%v cooldown=%v",
				tc.apiKey, perr, tc.wantStatus, tc.retryable, tc.disable, tc.cooldown)
		}
	}
}
