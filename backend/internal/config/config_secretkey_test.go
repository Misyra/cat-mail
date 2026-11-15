// config_secretkey_test.go CATMAIL_SECRET_KEY 解析（安全加固三件套，A）：
// 未设置 = nil（明文兼容）；合法 base64 32B = 解码注入 Config；非法（长度≠32/
// 非 base64）走 log.Fatalf 快速失败——Fatal 路径在进程内不可测（会终止测试进程），
// 解析合法性与错误文案由 secretcrypt.ParseKey 单测锁定。
package config

import (
	"os"
	"testing"
)

// TestLoadSecretKey Load 注入口径：未设置 nil；合法值注入解码结果。
func TestLoadSecretKey(t *testing.T) {
	t.Setenv("CATMAIL_SECRET_KEY", "")
	if cfg := Load(); cfg.SecretKey != nil {
		t.Errorf("未设置 CATMAIL_SECRET_KEY 应为 nil（明文兼容），got %d 字节", len(cfg.SecretKey))
	}

	// 32 字节的 base64（openssl rand -base64 产物，仅测试使用）
	const good = "jP8bBi5J3wXyFOUYqXJBupzqQw3N5eWAWhI3Qfi1DDw="
	t.Setenv("CATMAIL_SECRET_KEY", good)
	cfg := Load()
	if cfg.SecretKey == nil || len(cfg.SecretKey) != 32 {
		t.Fatalf("SecretKey = %v, want 32 字节", cfg.SecretKey)
	}
	if os.Getenv("CATMAIL_SECRET_KEY") != good {
		t.Error("环境变量不应被修改")
	}
}
