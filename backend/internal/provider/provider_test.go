// provider_test.go 适配器注册表完整性与统一错误格式测试。
package provider

import (
	"strings"
	"testing"
)

// TestDefaultRegistryIntegrity 修复锁定：init 后全部默认平台可解析且 Name() 自洽——
// 轮换器 resolveTransport 与 API 层平台枚举都依赖注册表完整性，若某适配器 init
// 未注册（或被占位实现覆盖），渠道将静默走 notImplemented/失败路径。
func TestDefaultRegistryIntegrity(t *testing.T) {
	want := []string{"aliyun", "brevo", "elasticemail", "mailgun", "mailjet", "mailtrap", "mock", "resend", "smtp", "smtp2go"}
	got := Names()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Names() = %v, want %v（排序后）", got, want)
	}
	for _, name := range want {
		tr, ok := Get(name)
		if !ok || tr == nil {
			t.Errorf("平台 %q 未注册", name)
			continue
		}
		if tr.Name() != name {
			t.Errorf("注册名 %q 与适配器 Name() %q 不一致", name, tr.Name())
		}
	}
	if _, ok := Get("not-exist"); ok {
		t.Error("未注册平台不应解析成功")
	}
}

// TestProviderErrorMessage 统一错误格式（写进 SendLog.last_error 与统一错误体）：
// 带 HTTP 状态时追加 "(http N)"，网络错误（状态 0）仅报文本。
func TestProviderErrorMessage(t *testing.T) {
	e := &ProviderError{HTTPStatus: 429, Code: "rate_limited", Message: "too many"}
	if got := e.Error(); got != "too many (http 429)" {
		t.Errorf("Error() = %q, want %q", got, "too many (http 429)")
	}
	e = &ProviderError{Code: "network_error", Message: "dial timeout"}
	if got := e.Error(); got != "dial timeout" {
		t.Errorf("Error() = %q, want %q", got, "dial timeout")
	}
}
