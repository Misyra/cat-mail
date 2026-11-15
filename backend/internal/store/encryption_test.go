// encryption_test.go 渠道凭据加密的启动期检查与存量迁移单测（安全加固三件套，A）：
// 迁移幂等、明文→密文、密钥不匹配报错、未配密钥遇密文拒绝、模型 serializer 透明
// 加解密与 map 写绕行口径。
package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/secretcrypt"
)

// encTestKey 固定测试密钥。
var encTestKey = []byte("0123456789abcdef0123456789abcdef")

// withEncKey 进程级设置全局加密密钥并在测试结束还原（SetGlobalKey 全局态隔离）。
func withEncKey(t *testing.T, key []byte) {
	t.Helper()
	if key != nil {
		secretcrypt.SetGlobalKey(key)
	}
	t.Cleanup(func() { secretcrypt.SetGlobalKey(nil) })
}

// openEncDB 打开临时库（含种子）。
func openEncDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "enc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}
	return db
}

// seedChannelWithSecrets 直写含敏感字段的渠道（struct 写路径经 serializer：无全局
// 密钥时落明文，模拟存量明文库）。
func seedChannelWithSecrets(t *testing.T, db *gorm.DB) model.Channel {
	t.Helper()
	ch := model.Channel{
		Name: "存量渠道", Provider: "resend", Status: model.ChannelEnabled,
		APIKey:    "re_plain_api_key",
		APISecret: "re_plain_secret",
		SMTPPass:  "smtp_plain_pass",
		ProxyURL:  "http://user:pass@proxy:8080",
	}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	return ch
}

// rawRowFor 读取指定渠道的原始敏感列值（影子结构直读，不经 serializer）。
func rawRowFor(t *testing.T, db *gorm.DB, id uint) channelSecretRow {
	t.Helper()
	rows, err := loadChannelSecretRows(db)
	if err != nil {
		t.Fatalf("load raw rows: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("渠道 #%d 原始行不存在", id)
	return channelSecretRow{}
}

// TestEnsureChannelEncryptionMigrates 配置密钥后启动迁移：明文→密文（单事务），
// 模型读回为明文（serializer 透明解密），原始列值为密文格式；幂等（重复执行零变更）。
func TestEnsureChannelEncryptionMigrates(t *testing.T) {
	db := openEncDB(t)
	ch := seedChannelWithSecrets(t, db)

	// 未配置密钥 + 明文库：明文兼容，无迁移、无报错
	withEncKey(t, nil)
	status, migrated, err := EnsureChannelEncryption(db, nil)
	if err != nil || migrated != 0 {
		t.Fatalf("明文库无密钥: migrated=%d err=%v", migrated, err)
	}
	if status.PlaintextRows != 1 {
		t.Errorf("status.PlaintextRows = %d, want 1", status.PlaintextRows)
	}

	// 配置密钥：一次性迁移
	withEncKey(t, encTestKey)
	_, migrated, err = EnsureChannelEncryption(db, encTestKey)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if migrated != 1 {
		t.Fatalf("migrated = %d, want 1", migrated)
	}
	// 原始列值已为密文（影子结构直读原始列）
	raw := rawRowFor(t, db, ch.ID)
	for col, v := range map[string]string{
		"api_key": raw.APIKey, "api_secret": raw.APISecret,
		"smtp_pass": raw.SMTPPass, "proxy_url": raw.ProxyURL,
	} {
		if !strings.HasPrefix(v, secretcrypt.Prefix) {
			t.Errorf("%s 原始列应为密文, got %q", col, v)
		}
	}
	// 模型读回：serializer 透明解密为明文
	var got model.Channel
	if err := db.First(&got, ch.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.APIKey != "re_plain_api_key" || got.APISecret != "re_plain_secret" ||
		got.SMTPPass != "smtp_plain_pass" || got.ProxyURL != "http://user:pass@proxy:8080" {
		t.Errorf("模型读回应为明文: %+v", got)
	}
	// 幂等：重复执行零迁移
	_, migrated2, err := EnsureChannelEncryption(db, encTestKey)
	if err != nil || migrated2 != 0 {
		t.Errorf("重复迁移: migrated=%d err=%v, want 0/nil", migrated2, err)
	}
}

// TestEnsureChannelEncryptionNoKeyCiphertext 未配密钥但库里有密文 → 拒绝启动
// （ErrCiphertextNeedsKey）。
func TestEnsureChannelEncryptionNoKeyCiphertext(t *testing.T) {
	db := openEncDB(t)
	// 先在密钥上下文里造一条密文库（模拟「曾配置密钥已迁移，后移除环境变量」）
	withEncKey(t, encTestKey)
	seedChannelWithSecrets(t, db)
	if _, _, err := EnsureChannelEncryption(db, encTestKey); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	withEncKey(t, nil)
	if _, _, err := EnsureChannelEncryption(db, nil); !errIs(err, ErrCiphertextNeedsKey) {
		t.Fatalf("密文无密钥应返回 ErrCiphertextNeedsKey, got %v", err)
	}
}

// TestEnsureChannelEncryptionKeyMismatch 密钥不匹配（换了一把密钥）→ 迁移失败
// （ErrKeyMismatch），数据库保持原状（事务回滚）。
func TestEnsureChannelEncryptionKeyMismatch(t *testing.T) {
	db := openEncDB(t)
	withEncKey(t, encTestKey)
	ch := seedChannelWithSecrets(t, db)
	if _, _, err := EnsureChannelEncryption(db, encTestKey); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 换新密钥（库里密文与新密钥不匹配）→ 整体报错
	newKey := []byte("ffffffffffffffffffffffffffffffff")
	if _, _, err := EnsureChannelEncryption(db, newKey); !errIs(err, ErrKeyMismatch) {
		t.Fatalf("密钥不匹配应返回 ErrKeyMismatch, got %v", err)
	}
	// 回滚验证：原密钥仍能正常解密（数据未被破坏）
	var got model.Channel
	if err := db.First(&got, ch.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.APIKey != "re_plain_api_key" {
		t.Errorf("不匹配迁移失败后数据应原样: %q", got.APIKey)
	}
}

// TestSerializerModelRoundTrip serializer 透明加解密与写路径：struct Create /
// Select+Updates（业务写路径）加密落库；空串与清空语义不受影响。
func TestSerializerModelRoundTrip(t *testing.T) {
	db := openEncDB(t)
	withEncKey(t, encTestKey)

	ch := model.Channel{Name: "round-trip", Provider: "mock", Status: model.ChannelEnabled, APIKey: "mock-secret-1"}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if raw := rawRowFor(t, db, ch.ID); !strings.HasPrefix(raw.APIKey, secretcrypt.Prefix) {
		t.Errorf("Create 写路径应加密落库, got %q", raw.APIKey)
	}
	// 更新（Select+Updates struct，业务写路径口径）
	ch.APIKey = "mock-secret-2"
	if err := db.Model(&model.Channel{}).Where("id = ?", ch.ID).
		Select("APIKey").Updates(ch).Error; err != nil {
		t.Fatalf("update: %v", err)
	}
	if raw := rawRowFor(t, db, ch.ID); !strings.HasPrefix(raw.APIKey, secretcrypt.Prefix) {
		t.Errorf("Updates 写路径应加密落库, got %q", raw.APIKey)
	}
	var got model.Channel
	if err := db.First(&got, ch.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.APIKey != "mock-secret-2" {
		t.Errorf("读回应为明文 %q, got %q", "mock-secret-2", got.APIKey)
	}
	// 清空（清凭据语义）：空串写路径 → 落空串（非密文）
	ch.APIKey = ""
	if err := db.Model(&model.Channel{}).Where("id = ?", ch.ID).
		Select("APIKey").Updates(ch).Error; err != nil {
		t.Fatalf("clear: %v", err)
	}
	if raw := rawRowFor(t, db, ch.ID); raw.APIKey != "" {
		t.Errorf("空串应原样落库, got %q", raw.APIKey)
	}
}

// errIs errors.Is 的简短别名（包内多处判定用）。
func errIs(err, target error) bool { return errors.Is(err, target) }
