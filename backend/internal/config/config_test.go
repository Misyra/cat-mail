package config

import "testing"

// TestLoadDefaults 全部环境变量未设置/空串时取 SPEC §1 默认值
// （envOr 语义：空串=未设置，与 os.Getenv 缺失同走默认分支）。
func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{"CATMAIL_ADDR", "CATMAIL_DB", "CATMAIL_ADMIN_USER", "CATMAIL_ADMIN_PASS", "CATMAIL_COOKIE_SECURE"} {
		t.Setenv(k, "")
	}
	cfg := Load()
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.DBPath != "./data/catmail.db" {
		t.Errorf("DBPath = %q, want ./data/catmail.db", cfg.DBPath)
	}
	if cfg.AdminUser != "admin" {
		t.Errorf("AdminUser = %q, want admin", cfg.AdminUser)
	}
	if cfg.AdminPass != "admin123" {
		t.Errorf("AdminPass = %q, want admin123", cfg.AdminPass)
	}
	if cfg.CookieSecure {
		t.Error("CookieSecure = true, want false")
	}
}

// TestLoadOverrides 环境变量覆盖默认值；空串视为未设置回落默认（边界）。
func TestLoadOverrides(t *testing.T) {
	t.Setenv("CATMAIL_ADDR", ":9000")
	t.Setenv("CATMAIL_DB", "/tmp/sm.db")
	t.Setenv("CATMAIL_ADMIN_USER", "root")
	t.Setenv("CATMAIL_ADMIN_PASS", "secret")
	t.Setenv("CATMAIL_COOKIE_SECURE", "1")
	cfg := Load()
	if cfg.Addr != ":9000" || cfg.DBPath != "/tmp/sm.db" || cfg.AdminUser != "root" || cfg.AdminPass != "secret" {
		t.Errorf("override 未生效: %+v", cfg)
	}
	if !cfg.CookieSecure {
		t.Error("CookieSecure = false, want true")
	}

	// 空串=未设置：回落默认，不允许空密码/空地址等非法配置
	t.Setenv("CATMAIL_ADDR", "")
	t.Setenv("CATMAIL_ADMIN_PASS", "")
	cfg = Load()
	if cfg.Addr != ":8080" {
		t.Errorf("空串 Addr 应回落默认，got %q", cfg.Addr)
	}
	if cfg.AdminPass != "admin123" {
		t.Errorf("空串 AdminPass 应回落默认，got %q", cfg.AdminPass)
	}
}

// TestEnvBool 布尔环境变量解析："1"/"true"（不区分大小写）为真，其余（含非法值）为假。
func TestEnvBool(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"0", false},
		{"false", false},
		{"yes", false},   // 非法值按假处理
		{"on", false},    // 非法值按假处理
		{"", false},      // 未设置/空串
		{" true", false}, // 带空格不解析（fail-safe）
	} {
		t.Setenv("CATMAIL_COOKIE_SECURE", tc.val)
		if got := Load().CookieSecure; got != tc.want {
			t.Errorf("CATMAIL_COOKIE_SECURE=%q → %v, want %v", tc.val, got, tc.want)
		}
	}
}
