// config_smtp_test.go SMTP 入口配置（E1）的默认值与启用语义。
package config

import "testing"

func TestSMTPConfigDefaults(t *testing.T) {
	t.Setenv("CATMAIL_SMTP_ADDR", "")        // 空/未设置 = 功能关闭
	t.Setenv("CATMAIL_SMTP_MAX_SIZE_MB", "") // 默认 25
	cfg := Load()
	if cfg.SMTPAddr != "" {
		t.Errorf("SMTPAddr = %q, want empty (功能关闭)", cfg.SMTPAddr)
	}
	if cfg.SMTPMaxSizeMB != 25 {
		t.Errorf("SMTPMaxSizeMB = %d, want default 25", cfg.SMTPMaxSizeMB)
	}
}

func TestSMTPConfigEnabled(t *testing.T) {
	t.Setenv("CATMAIL_SMTP_ADDR", ":2525")
	t.Setenv("CATMAIL_SMTP_MAX_SIZE_MB", "40")
	cfg := Load()
	if cfg.SMTPAddr != ":2525" {
		t.Errorf("SMTPAddr = %q, want :2525", cfg.SMTPAddr)
	}
	if cfg.SMTPMaxSizeMB != 40 {
		t.Errorf("SMTPMaxSizeMB = %d, want 40", cfg.SMTPMaxSizeMB)
	}
}
