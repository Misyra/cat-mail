// rotator_quota_export_test.go 导出额度记账入口的契约测试（外部包视角）：
// 渠道测试直发等绕过轮换的发送路径复用 rotator.ReserveQuota/ReleaseQuota，
// 必须与轮换路径保持同一套 SQL 口径——原子预留、额度耗尽拒绝（fail-closed）、
// 惰性周期重置、释放防御减为负（SPEC §5 渠道测试端点）。
package rotator_test

import (
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/rotator"
	"catmail/internal/store"
)

// newQuotaDB 打开临时 SQLite（仅迁移，渠道由用例自建）。
func newQuotaDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// reload 从库重读渠道快照（预留/释放均为 SQL 直改，必须重读才能断言）。
func reload(t *testing.T, db *gorm.DB, id uint) model.Channel {
	t.Helper()
	var ch model.Channel
	if err := db.First(&ch, id).Error; err != nil {
		t.Fatalf("reload channel: %v", err)
	}
	return ch
}

// TestReserveQuotaReleaseExported 预留/释放基础语义：成功日/月各 +1、超限拒绝且
// fail-closed 不动计数、释放回收 1 且防御减为负。
func TestReserveQuotaReleaseExported(t *testing.T) {
	db := newQuotaDB(t)
	now := time.Now()
	ch := model.Channel{Name: "q", Provider: "resend", DailyLimitOverride: 2, MonthlyLimitOverride: 5}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// 预留两次成功：0→1→2，日/月同步 +1，anchor 推进到当前周期
	if !rotator.ReserveQuota(db, &ch, now) {
		t.Fatal("reserve #1 should succeed")
	}
	if !rotator.ReserveQuota(db, &ch, now) {
		t.Fatal("reserve #2 should succeed")
	}
	got := reload(t, db, ch.ID)
	if got.UsedDay != 2 || got.UsedMonth != 2 {
		t.Fatalf("after 2 reserves: used_day=%d used_month=%d, want 2/2", got.UsedDay, got.UsedMonth)
	}
	if got.DayAnchor != now.Format("2006-01-02") || got.MonthAnchor != now.Format("2006-01") {
		t.Fatalf("anchors not advanced: day=%q month=%q", got.DayAnchor, got.MonthAnchor)
	}

	// 超日额度：拒绝且 fail-closed（计数保持 2/2，不被失败预留改动）
	if rotator.ReserveQuota(db, &ch, now) {
		t.Fatal("reserve beyond daily limit should fail")
	}
	if got := reload(t, db, ch.ID); got.UsedDay != 2 || got.UsedMonth != 2 {
		t.Fatalf("failed reserve mutated counters: %d/%d", got.UsedDay, got.UsedMonth)
	}

	// 释放三次：2→1→0→0（防御减为负；锚点匹配当前周期，照常回收）
	rotator.ReleaseQuota(db, &ch, now)
	rotator.ReleaseQuota(db, &ch, now)
	rotator.ReleaseQuota(db, &ch, now)
	got = reload(t, db, ch.ID)
	if got.UsedDay != 0 || got.UsedMonth != 0 {
		t.Fatalf("after 3 releases: used_day=%d used_month=%d, want 0/0", got.UsedDay, got.UsedMonth)
	}
}

// TestReserveQuotaMonthlyLimit 月维度上限独立生效（日额度未超也应被月上限拦截）。
func TestReserveQuotaMonthlyLimit(t *testing.T) {
	db := newQuotaDB(t)
	now := time.Now()
	ch := model.Channel{Name: "m", Provider: "resend", MonthlyLimitOverride: 1} // 日额度走 resend 默认 100
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if !rotator.ReserveQuota(db, &ch, now) {
		t.Fatal("first reserve should succeed")
	}
	if rotator.ReserveQuota(db, &ch, now) {
		t.Fatal("reserve beyond monthly limit should fail")
	}
	if got := reload(t, db, ch.ID); got.UsedMonth != 1 {
		t.Fatalf("failed reserve mutated used_month: %d", got.UsedMonth)
	}
}

// TestReserveQuotaLazyAnchorReset 惰性周期重置口径：anchor 非当前周期时旧计数
// （哪怕已远超上限）不参与判定，预留成功即以新周期起算 1。
func TestReserveQuotaLazyAnchorReset(t *testing.T) {
	db := newQuotaDB(t)
	now := time.Now()
	ch := model.Channel{Name: "stale", Provider: "resend",
		DailyLimitOverride: 2, UsedDay: 999, DayAnchor: "2000-01-01",
		MonthlyLimitOverride: 2, UsedMonth: 999, MonthAnchor: "2000-01"}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if !rotator.ReserveQuota(db, &ch, now) {
		t.Fatal("stale-anchor reserve should succeed (reset first)")
	}
	got := reload(t, db, ch.ID)
	if got.UsedDay != 1 || got.UsedMonth != 1 {
		t.Fatalf("after stale-anchor reserve: used_day=%d used_month=%d, want 1/1", got.UsedDay, got.UsedMonth)
	}
	if got.DayAnchor != now.Format("2006-01-02") || got.MonthAnchor != now.Format("2006-01") {
		t.Fatalf("anchors not rolled: day=%q month=%q", got.DayAnchor, got.MonthAnchor)
	}
}

// TestReleaseQuotaCrossPeriodAnchor 跨周期释放口径（O8）：预留发生在旧周期、
// 释放在锚点翻页后执行时，不得把新周期计数多减 1——仅当渠道当前锚点仍等于
// 本次预留的周期锚点（且计数 > 0）才回收。用固定日期构造，不依赖运行时刻。
func TestReleaseQuotaCrossPeriodAnchor(t *testing.T) {
	db := newQuotaDB(t)
	// 固定基准：2026-03-31 23:59:59 预留，日/月锚点均为旧周期
	base := time.Date(2026, 3, 31, 23, 59, 59, 0, time.Local)
	ch := model.Channel{Name: "cross", Provider: "resend", DailyLimitOverride: 5, MonthlyLimitOverride: 5}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if !rotator.ReserveQuota(db, &ch, base) {
		t.Fatal("reserve at base should succeed")
	}

	// 场景一：仅日锚翻页（次日另一笔预留），月锚仍属预留当月。
	// 以旧预留时刻释放：日计数（新周期）不动，月计数（锚点匹配）回收。
	db.Model(&model.Channel{}).Where("id = ?", ch.ID).Updates(map[string]any{
		"used_day": 1, "day_anchor": "2026-04-01",
	})
	rotator.ReleaseQuota(db, &ch, base)
	got := reload(t, db, ch.ID)
	if got.UsedDay != 1 || got.UsedMonth != 0 {
		t.Fatalf("day-crossed release: used_day=%d used_month=%d, want 1/0", got.UsedDay, got.UsedMonth)
	}

	// 场景二：日/月锚点均已翻页（23:59:59 预留、00:00 后释放的完整场景），
	// 新周期各有一笔独立预留 used=1/1：旧预留释放两个维度都不得多减。
	db.Model(&model.Channel{}).Where("id = ?", ch.ID).Updates(map[string]any{
		"used_day": 1, "day_anchor": "2026-04-01",
		"used_month": 1, "month_anchor": "2026-04",
	})
	rotator.ReleaseQuota(db, &ch, base)
	got = reload(t, db, ch.ID)
	if got.UsedDay != 1 || got.UsedMonth != 1 {
		t.Fatalf("cross-period release mutated new-period counters: used=%d/%d, want 1/1",
			got.UsedDay, got.UsedMonth)
	}

	// 锚点匹配时释放照常回收（与预留同周期）：日/月各 -1
	rotator.ReleaseQuota(db, &ch, time.Date(2026, 4, 1, 0, 0, 30, 0, time.Local))
	got = reload(t, db, ch.ID)
	if got.UsedDay != 0 || got.UsedMonth != 0 {
		t.Fatalf("matched-anchor release: used=%d/%d, want 0/0", got.UsedDay, got.UsedMonth)
	}
}
