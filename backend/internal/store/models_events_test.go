// models_events_test.go 送达链路三件套新表（mail_events/suppressions）迁移与
// 约束测试：AutoMigrate 建表/补列、唯一约束冲突翻译（双方言同口径）、新设置列
// 默认值（存量行补列取 gorm default）；live PG 集成按 CATMAIL_TEST_PG_DSN 跳过。
package store

import (
	"errors"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
)

// TestOpenMigratesEventTables 两个新表建表齐全 + 关键列/索引存在。
func TestOpenMigratesEventTables(t *testing.T) {
	db := openSeeded(t)
	m := db.Migrator()
	for _, tbl := range []any{&model.MailEvent{}, &model.Suppression{}} {
		if !m.HasTable(tbl) {
			t.Fatalf("table %T missing", tbl)
		}
	}
	for _, col := range []string{"bounce_type", "provider_message_id", "send_log_id", "occurred_at", "raw"} {
		if !m.HasColumn(&model.MailEvent{}, col) {
			t.Errorf("mail_events 缺少列 %s", col)
		}
	}
	for _, col := range []string{"email", "reason", "channel_id", "detail"} {
		if !m.HasColumn(&model.Suppression{}, col) {
			t.Errorf("suppressions 缺少列 %s", col)
		}
	}
	// 抑制列表 email 唯一索引（拦截查询与并发首投兜底的基础）
	if !m.HasIndex(&model.Suppression{}, "idx_suppressions_email") &&
		!m.HasIndex(&model.Suppression{}, "email") {
		t.Error("suppressions.email 应建唯一索引")
	}
}

// TestSuppressionEmailUniqueDialect email 唯一约束冲突双方言翻译为
// gorm.ErrDuplicatedKey（与渠道重名同口径，上层兜底 400）。
func TestSuppressionEmailUniqueDialect(t *testing.T) {
	db := openSeeded(t)
	row := model.Suppression{Email: "dup@example.com", Reason: model.SuppressReasonBounce}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("first insert: %v", err)
	}
	dup := model.Suppression{Email: "dup@example.com", Reason: model.SuppressReasonManual}
	if err := db.Create(&dup).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Errorf("重复 email 应翻译为 gorm.ErrDuplicatedKey, got %v", err)
	}
	// 拦截查询：IN 命中（rotator 发送前统一拦截的数据面）
	var hits []model.Suppression
	if err := db.Where("email IN ?", []string{"other@example.com", "dup@example.com"}).Find(&hits).Error; err != nil {
		t.Fatalf("IN query: %v", err)
	}
	if len(hits) != 1 || hits[0].Email != "dup@example.com" {
		t.Errorf("IN 查询应精确命中 1 条, got %v", hits)
	}
}

// TestEventSettingColumnsDefaults 新设置列带默认值：存量行（本例为先建行的再迁移
// 场景以 AutoMigrate 补列覆盖）补列后读取为任务书口径——抑制自动开、阈值 5。
// 直接断言 seed 行（EnsureSeed 建行时未显式赋值，gorm default 生效）。
func TestEventSettingColumnsDefaults(t *testing.T) {
	db := openSeeded(t)
	var s model.Setting
	if err := db.First(&s, 1).Error; err != nil {
		t.Fatalf("setting row: %v", err)
	}
	if !s.SuppressionAutoEnabled {
		t.Error("suppression_auto_enabled 默认应为 true（gorm default 对建行生效）")
	}
	if s.BounceRateThreshold != 5 {
		t.Errorf("bounce_rate_threshold 默认应为 5, got %v", s.BounceRateThreshold)
	}
	// 渠道 webhook token 列存在（建渠道自动生成在 api 层，store 层只验列）
	if !db.Migrator().HasColumn(&model.Channel{}, "webhook_token") ||
		!db.Migrator().HasColumn(&model.Channel{}, "webhook_secret") {
		t.Error("channels 缺少 webhook_token/webhook_secret 列")
	}
}

// TestEventTablesLivePostgres live PostgreSQL 集成（D2 同构回归，样式沿用
// store_pg_test.go）：设 CATMAIL_TEST_PG_DSN 才运行，未设置时 Skip。
// 用例：两新表建表、唯一约束翻译、IN 拦截查询与 SQLite 同口径。
func TestEventTablesLivePostgres(t *testing.T) {
	dsn := os.Getenv("CATMAIL_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("CATMAIL_TEST_PG_DSN 未设置：跳过 live PostgreSQL 集成测试（本机无 PG）")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open(pg): %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if !IsPostgres(db) {
		t.Fatal("Open(pg DSN) 应回到 postgres 方言")
	}

	// 建表齐全
	m := db.Migrator()
	if !m.HasTable(&model.MailEvent{}) || !m.HasTable(&model.Suppression{}) {
		t.Fatal("PG 上 mail_events/suppressions 建表缺失")
	}

	// 唯一约束 + IN 查询（email 含 PG 保留字样式地址，验证无方言坑）
	email := pgUniqueName("dup") + "@example.com"
	row := model.Suppression{Email: email, Reason: model.SuppressReasonBounce}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("insert suppression(pg): %v", err)
	}
	if err := db.Create(&model.Suppression{Email: email, Reason: model.SuppressReasonManual}).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Errorf("PG 唯一冲突应翻译为 gorm.ErrDuplicatedKey, got %v", err)
	}
	var hits []model.Suppression
	if err := db.Where("email IN ?", []string{email}).Find(&hits).Error; err != nil || len(hits) != 1 {
		t.Errorf("PG IN 拦截查询: n=%d err=%v", len(hits), err)
	}

	// mail_events 送达侧条件聚合（与 api 层 channelHealthMap 同款 SQL 的方言回归）
	ch := model.Channel{Name: pgUniqueName("ev-ch"), Provider: "mock", Status: model.ChannelEnabled}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("create channel(pg): %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := db.Create(&model.MailEvent{
			ChannelID: ch.ID, Event: model.MailEventDelivered,
			OccurredAt: time.Now(), CreatedAt: time.Now(),
		}).Error; err != nil {
			t.Fatalf("insert mail event(pg): %v", err)
		}
	}
	var row2 struct {
		Delivered int64
		Bounced   int64
	}
	if err := db.Model(&model.MailEvent{}).
		Select("COALESCE(SUM(CASE WHEN event = ? THEN 1 ELSE 0 END), 0) AS delivered, "+
			"COALESCE(SUM(CASE WHEN event = ? THEN 1 ELSE 0 END), 0) AS bounced",
			model.MailEventDelivered, model.MailEventBounced).
		Where("channel_id = ?", ch.ID).
		Group("channel_id").Scan(&row2).Error; err != nil {
		t.Fatalf("条件聚合(pg): %v", err)
	}
	if row2.Delivered != 3 || row2.Bounced != 0 {
		t.Errorf("PG 条件聚合 = %+v, want delivered=3 bounced=0", row2)
	}
}
