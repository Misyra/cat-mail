package store

import (
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// openSeeded 在临时目录打开 DB 并完成首启初始化；测试结束自动清理。
func openSeeded(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "catmail.db"))
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

func TestOpenMigratesAllTables(t *testing.T) {
	db := openSeeded(t)
	m := db.Migrator()
	for _, tbl := range []any{&model.User{}, &model.Setting{}, &model.ApiKey{}, &model.Channel{}, &model.SendLog{}, &model.Notification{}} {
		if !m.HasTable(tbl) {
			t.Errorf("table %T missing", tbl)
		}
	}
	for _, col := range []string{"notify_inapp_enabled", "notify_webhook_enabled", "notify_webhook_url", "notify_events"} {
		if !m.HasColumn(&model.Setting{}, col) {
			t.Errorf("settings 缺少列 %s", col)
		}
	}
}

func TestEnsureSeedAdminUser(t *testing.T) {
	db := openSeeded(t)
	var u model.User
	if err := db.Where("username = ?", "admin").First(&u).Error; err != nil {
		t.Fatalf("admin user not found: %v", err)
	}
	if u.PasswordHash == "" {
		t.Fatal("password hash empty")
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("admin123")) != nil {
		t.Error("bcrypt password mismatch for admin123")
	}
}

func TestEnsureSeedSettingDefaults(t *testing.T) {
	db := openSeeded(t)
	var s model.Setting
	if err := db.First(&s, 1).Error; err != nil {
		t.Fatalf("setting row not found: %v", err)
	}
	if s.Rotation != "priority" {
		t.Errorf("Rotation = %q, want priority", s.Rotation)
	}
	if s.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", s.MaxAttempts)
	}
	if s.CooldownSeconds != 60 {
		t.Errorf("CooldownSeconds = %d, want 60", s.CooldownSeconds)
	}
	if s.ConsecutiveFailureThreshold != 5 {
		t.Errorf("ConsecutiveFailureThreshold = %d, want 5", s.ConsecutiveFailureThreshold)
	}
	if s.LogRetentionDays != 90 {
		t.Errorf("LogRetentionDays = %d, want 90", s.LogRetentionDays)
	}
	if len(s.SessionSecret) != 64 {
		t.Errorf("SessionSecret len = %d, want 64 (32B hex)", len(s.SessionSecret))
	}
}

func TestEnsureSeedSampleChannel(t *testing.T) {
	db := openSeeded(t)
	var ch model.Channel
	if err := db.Where("name = ?", "示例渠道").First(&ch).Error; err != nil {
		t.Fatalf("sample channel not found: %v", err)
	}
	if ch.Provider != "mock" {
		t.Errorf("Provider = %q, want mock", ch.Provider)
	}
	if ch.Status != model.ChannelEnabled {
		t.Errorf("Status = %d, want enabled(%d)", ch.Status, model.ChannelEnabled)
	}
	if ch.FromEmail != "sender@example.com" {
		t.Errorf("FromEmail = %q, want sender@example.com", ch.FromEmail)
	}
	if ch.FromName != "CatMail 示例" {
		t.Errorf("FromName = %q, want CatMail 示例", ch.FromName)
	}
}

func TestEnsureSeedIdempotent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "catmail.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("EnsureSeed #1: %v", err)
	}
	if err := EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("EnsureSeed #2: %v", err)
	}
	var users, settings, channels int64
	if err := db.Model(&model.User{}).Count(&users).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Setting{}).Count(&settings).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Channel{}).Count(&channels).Error; err != nil {
		t.Fatal(err)
	}
	if users != 1 || settings != 1 || channels != 1 {
		t.Errorf("counts users=%d settings=%d channels=%d, want 1/1/1", users, settings, channels)
	}
}
