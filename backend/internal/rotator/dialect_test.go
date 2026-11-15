// dialect_test.go D2 方言分支单测：锁死 lazyResetQuota WHERE 子句的 NULL 安全
// 不等片段在两种方言下的展开——SQLite 用 IS NOT（原行为不变）、PostgreSQL 用
// IS DISTINCT FROM（SQLite 的 `IS NOT ?` 在 PG 是语法错误）。另含 live PG
// 集成回归（CATMAIL_TEST_PG_DSN 未设置时 Skip，见 store/store_pg_test.go
// 的 docker 自测命令）。
package rotator

import (
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/store"
)

// newPostgresDialectorDB 构造挂 postgres 方言器的 *gorm.DB（不建立真实连接，
// 同 store/store_pg_test.go：驱动 Initialize 离线 + DisableAutomaticPing）。
func newPostgresDialectorDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialector := postgres.New(postgres.Config{
		DSN: "postgres://catmail:catmail@127.0.0.1:5432/catmail_test?sslmode=disable",
	})
	db, err := gorm.Open(dialector, &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open postgres dialector: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestNullSafeNotEqualFrag 两分支展开锁死：sqlite → `col IS NOT ?`、
// postgres → `col IS DISTINCT FROM ?`。改写任一分支须显式改本测试。
func TestNullSafeNotEqualFrag(t *testing.T) {
	sq := newDB(t)
	if got := nullSafeNotEqualFrag(sq, "day_anchor"); got != "day_anchor IS NOT ?" {
		t.Errorf("sqlite 片段 = %q, want %q", got, "day_anchor IS NOT ?")
	}
	pg := newPostgresDialectorDB(t)
	if got := nullSafeNotEqualFrag(pg, "day_anchor"); got != "day_anchor IS DISTINCT FROM ?" {
		t.Errorf("postgres 片段 = %q, want %q", got, "day_anchor IS DISTINCT FROM ?")
	}
	if got := nullSafeNotEqualFrag(pg, "month_anchor"); got != "month_anchor IS DISTINCT FROM ?" {
		t.Errorf("postgres 片段 = %q, want %q", got, "month_anchor IS DISTINCT FROM ?")
	}
}

// TestLazyResetQuotaSQLiteRegression SQLite 全链路回归：D2 改造（WHERE 改经
// helper 拼接、参数顺序不变）后惰性重置行为与原实现一致——过期 anchor 清零、
// 当前周期 anchor 不动、NULL anchor 视为不等照常清零。
func TestLazyResetQuotaSQLiteRegression(t *testing.T) {
	db := newDB(t)
	now := time.Now()
	today, month := now.Format(dayLayout), now.Format(monthLayout)

	mk := func(name string) model.Channel {
		ch := model.Channel{Name: name, Provider: "resend"}
		if err := db.Create(&ch).Error; err != nil {
			t.Fatalf("create channel: %v", err)
		}
		return ch
	}
	stale := mk("dialect-stale") // 过期日/月 anchor → 双双清零
	if err := db.Model(&stale).Updates(map[string]any{
		"used_day": 7, "day_anchor": "2000-01-01", "used_month": 9, "month_anchor": "2000-01",
	}).Error; err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	current := mk("dialect-current") // 已是当前周期 → 不动
	if err := db.Model(&current).Updates(map[string]any{
		"used_day": 3, "day_anchor": today, "used_month": 5, "month_anchor": month,
	}).Error; err != nil {
		t.Fatalf("seed current: %v", err)
	}
	null := mk("dialect-null") // anchor NULL → 等值不成立，按不等分支清零
	if err := db.Model(&null).Updates(map[string]any{
		"used_day": 4, "day_anchor": nil,
	}).Error; err != nil {
		t.Fatalf("seed null: %v", err)
	}

	for _, ch := range []*model.Channel{&stale, &current, &null} {
		lazyResetQuota(db, ch, today, month)
	}

	reload := func(ch *model.Channel) model.Channel {
		var got model.Channel
		if err := db.First(&got, ch.ID).Error; err != nil {
			t.Fatalf("reload: %v", err)
		}
		return got
	}
	if g := reload(&stale); g.UsedDay != 0 || g.DayAnchor != today || g.UsedMonth != 0 || g.MonthAnchor != month {
		t.Errorf("stale 渠道重置错误: %+v", g)
	}
	if g := reload(&current); g.UsedDay != 3 || g.UsedMonth != 5 {
		t.Errorf("current 渠道不应被重置: %+v", g)
	}
	if g := reload(&null); g.UsedDay != 0 || g.DayAnchor != today {
		t.Errorf("NULL anchor 渠道应被清零: %+v", g)
	}
}

// TestPostgresQuotaDialectLive live PG 上锁死 D2 修复：lazyResetQuota（IS DISTINCT
// FROM 分支在真实 PG 执行）+ reserveQuota/releaseQuota 的额度语义与 SQLite 一致。
// 未设 CATMAIL_TEST_PG_DSN 时 Skip（本机无 PG；docker 自测命令见
// store/store_pg_test.go 的 TestPostgresLiveIntegration 注释）。
func TestPostgresQuotaDialectLive(t *testing.T) {
	dsn := os.Getenv("CATMAIL_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("CATMAIL_TEST_PG_DSN 未设置：跳过 live PostgreSQL 方言回归（本机无 PG）")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	now := time.Now()
	today, month := now.Format(dayLayout), now.Format(monthLayout)

	ch := model.Channel{
		Name:     "pg-dialect-" + now.Format("20060102-150405.000000000"),
		Provider: "resend", DailyLimitOverride: 1, MonthlyLimitOverride: 3,
	}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	// 预置过期 anchor 与脏计数：惰性重置必须走 IS DISTINCT FROM 分支清零
	if err := db.Model(&ch).Updates(map[string]any{
		"used_day": 99, "day_anchor": "2000-01-01", "used_month": 99, "month_anchor": "2000-01",
	}).Error; err != nil {
		t.Fatalf("seed stale anchors: %v", err)
	}
	ch.UsedDay, ch.DayAnchor, ch.UsedMonth, ch.MonthAnchor = 99, "2000-01-01", 99, "2000-01"
	lazyResetQuota(db, &ch, today, month)

	var got model.Channel
	if err := db.First(&got, ch.ID).Error; err != nil {
		t.Fatalf("reload after lazy reset: %v", err)
	}
	if got.UsedDay != 0 || got.DayAnchor != today || got.UsedMonth != 0 || got.MonthAnchor != month {
		t.Fatalf("PG 惰性重置未生效（IS DISTINCT FROM 分支）: %+v", got)
	}

	// 原子预留：日限 1 → 第一次成功（used=1）、第二次 fail-closed 拒绝
	if !reserveQuota(db, &got, now) {
		t.Fatal("reserve #1 should succeed on pg")
	}
	if reserveQuota(db, &got, now) {
		t.Fatal("reserve #2 should be rejected by daily limit on pg")
	}
	// 释放回收 1（防御减为负的口径与 sqlite 相同）
	releaseQuota(db, &got, now)
	var after model.Channel
	if err := db.First(&after, ch.ID).Error; err != nil {
		t.Fatalf("reload after release: %v", err)
	}
	if after.UsedDay != 0 || after.UsedMonth != 1 {
		t.Errorf("release 后 used_day=0/used_month=1, got day=%d month=%d", after.UsedDay, after.UsedMonth)
	}
}
