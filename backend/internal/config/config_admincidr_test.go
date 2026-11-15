// config_admincidr_test.go CATMAIL_ADMIN_CIDR 解析测试（E5）：
// ParseAdminCIDRs 的空值/IPv4/IPv6/多 CIDR/非法条目各分支，以及 Load 的
// 类型安全交接（Config.AdminCIDRs）。非法配置经 Load 的 log.Fatalf 快速失败
// 路径会终止测试进程，故以 ParseAdminCIDRs 的错误分支覆盖。
package config

import (
	"net"
	"strings"
	"testing"
)

// TestParseAdminCIDRs 解析语义：空=不限制；IPv4/IPv6 CIDR；空白与结尾逗号容忍；
// 裸 IP 等非法条目报错（宁窄勿宽）。
func TestParseAdminCIDRs(t *testing.T) {
	contains := func(n *net.IPNet, ip string) bool { return n.Contains(net.ParseIP(ip)) }

	// 空/未设置 = 不限制（nil）
	for _, raw := range []string{"", "   "} {
		cidrs, err := ParseAdminCIDRs(raw)
		if err != nil || cidrs != nil {
			t.Errorf("ParseAdminCIDRs(%q) = %v, %v; want nil, nil", raw, cidrs, err)
		}
	}

	// 单条 IPv4
	cidrs, err := ParseAdminCIDRs("10.0.0.0/8")
	if err != nil || len(cidrs) != 1 {
		t.Fatalf("单条 IPv4: got %v, %v", cidrs, err)
	}
	if !contains(cidrs[0], "10.1.2.3") || contains(cidrs[0], "11.0.0.1") {
		t.Errorf("10.0.0.0/8 命中判定错误")
	}

	// 多条：容忍条目两侧空白与结尾逗号
	cidrs, err = ParseAdminCIDRs(" 192.168.1.0/24 , 10.0.0.5/32, ")
	if err != nil || len(cidrs) != 2 {
		t.Fatalf("多条解析: got %v, %v", cidrs, err)
	}
	if !contains(cidrs[0], "192.168.1.77") || !contains(cidrs[1], "10.0.0.5") {
		t.Errorf("多条命中判定错误")
	}
	if contains(cidrs[1], "10.0.0.6") {
		t.Errorf("/32 不应命中同网段其他主机")
	}

	// IPv6
	cidrs, err = ParseAdminCIDRs("2001:db8::/32")
	if err != nil || len(cidrs) != 1 {
		t.Fatalf("IPv6 解析: got %v, %v", cidrs, err)
	}
	if !contains(cidrs[0], "2001:db8::1") || contains(cidrs[0], "2001:db9::1") {
		t.Errorf("2001:db8::/32 命中判定错误")
	}

	// 非法条目：裸 IP、越界前缀、乱码、混合列表中的坏条目，均报错且写明条目
	for _, raw := range []string{"10.0.0.5", "10.0.0.5/33", "banana", "192.168.1.0/24,oops"} {
		cidrs, err := ParseAdminCIDRs(raw)
		if err == nil {
			t.Errorf("ParseAdminCIDRs(%q) 应报错, got %v", raw, cidrs)
			continue
		}
		if !strings.Contains(err.Error(), "不是合法 CIDR") {
			t.Errorf("ParseAdminCIDRs(%q) 错误信息应写明原因, got %q", raw, err)
		}
	}
}

// TestLoadAdminCIDRs Load 将解析结果以类型安全的 []*net.IPNet 交给 Config。
func TestLoadAdminCIDRs(t *testing.T) {
	t.Setenv("CATMAIL_ADMIN_CIDR", "")
	if cfg := Load(); len(cfg.AdminCIDRs) != 0 {
		t.Errorf("未配置时 AdminCIDRs 应为空（不限制）, got %v", cfg.AdminCIDRs)
	}

	t.Setenv("CATMAIL_ADMIN_CIDR", "192.168.1.0/24,10.0.0.5/32")
	cfg := Load()
	if len(cfg.AdminCIDRs) != 2 {
		t.Fatalf("AdminCIDRs 长度 = %d, want 2", len(cfg.AdminCIDRs))
	}
	if !cfg.AdminCIDRs[0].Contains(net.ParseIP("192.168.1.7")) ||
		!cfg.AdminCIDRs[1].Contains(net.ParseIP("10.0.0.5")) {
		t.Errorf("Load 后的 CIDR 命中判定错误: %v", cfg.AdminCIDRs)
	}
}
