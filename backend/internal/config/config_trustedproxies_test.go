// config_trustedproxies_test.go CATMAIL_TRUSTED_PROXIES 解析测试（B4 接线附带）：
// 复用 ParseAdminCIDRs 校验语义，Load 交接归一化 CIDR 字符串列表。非法条目走
// Load 内 mustTrustedProxies 的 log.Fatalf 终止进程，与 AdminCIDR 同策略，
// 进程内仅覆盖合法分支（错误分支由 ParseAdminCIDRs 测试覆盖）。
package config

import "testing"

// TestLoadTrustedProxies 未设置返回 nil（保持 gin 默认行为）；多 CIDR 归一化、
// 空白与结尾逗号容忍、IPv6 原样。
func TestLoadTrustedProxies(t *testing.T) {
	t.Setenv("CATMAIL_TRUSTED_PROXIES", "")
	if cfg := Load(); cfg.TrustedProxies != nil {
		t.Errorf("未设置时 TrustedProxies 应为 nil（保持 gin 默认行为）, got %v", cfg.TrustedProxies)
	}

	t.Setenv("CATMAIL_TRUSTED_PROXIES", " 10.0.0.0/8 , 192.168.1.5/32, ")
	cfg := Load()
	want := []string{"10.0.0.0/8", "192.168.1.5/32"}
	if len(cfg.TrustedProxies) != len(want) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, want)
	}
	for i := range want {
		if cfg.TrustedProxies[i] != want[i] {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, cfg.TrustedProxies[i], want[i])
		}
	}

	// IPv6 CIDR
	t.Setenv("CATMAIL_TRUSTED_PROXIES", "2001:db8::/32")
	if cfg := Load(); len(cfg.TrustedProxies) != 1 || cfg.TrustedProxies[0] != "2001:db8::/32" {
		t.Errorf("IPv6 TrustedProxies = %v, want [2001:db8::/32]", cfg.TrustedProxies)
	}
}
