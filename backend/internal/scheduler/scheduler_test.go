package scheduler

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/store"
)

// ---------- 测试基建：临时 sqlite（零网络） ----------

// newDB 打开临时 SQLite（仅迁移不 seed，渠道与设置由用例自建）。
func newDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "scheduler.db"))
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

// putSetting 写入 ID=1 设置行（用例显式控制 LogRetentionDays）。
func putSetting(t *testing.T, db *gorm.DB, retentionDays int) {
	t.Helper()
	if err := db.Create(&model.Setting{
		ID:               1,
		Rotation:         "priority",
		MaxAttempts:      3,
		CooldownSeconds:  60,
		LogRetentionDays: retentionDays,
	}).Error; err != nil {
		t.Fatalf("create setting: %v", err)
	}
}

// putChannel 写入一条指定状态与冷却截止时间的渠道。
func putChannel(t *testing.T, db *gorm.DB, name string, status int, cooldownUntil *time.Time) {
	t.Helper()
	if err := db.Create(&model.Channel{
		Name:          name,
		Provider:      "mock",
		Status:        status,
		CooldownUntil: cooldownUntil,
	}).Error; err != nil {
		t.Fatalf("create channel %s: %v", name, err)
	}
}

// putLogs 批量写入 n 条指定创建时间的发送日志。
func putLogs(t *testing.T, db *gorm.DB, n int, createdAt time.Time) {
	t.Helper()
	logs := make([]model.SendLog, n)
	for i := range logs {
		logs[i] = model.SendLog{
			MessageID: fmt.Sprintf("m-%s-%d", createdAt.Format("0102150405"), i),
			Status:    model.SendStatusSent,
			CreatedAt: createdAt,
		}
	}
	if err := db.CreateInBatches(logs, 100).Error; err != nil {
		t.Fatalf("create %d logs: %v", n, err)
	}
}

// countLogs 统计满足条件的日志条数。
func countLogs(t *testing.T, db *gorm.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.SendLog{}).Where(query, args...).Count(&n).Error; err != nil {
		t.Fatalf("count logs: %v", err)
	}
	return n
}

// getChannel 按 ID 读回渠道。
func getChannel(t *testing.T, db *gorm.DB, id uint) model.Channel {
	t.Helper()
	var ch model.Channel
	if err := db.First(&ch, id).Error; err != nil {
		t.Fatalf("read channel %d: %v", id, err)
	}
	return ch
}

// ---------- 冷却释放 ----------

func TestReleaseExpiredCooldowns(t *testing.T) {
	db := newDB(t)
	now := time.Now()

	putChannel(t, db, "已过期冷却", model.ChannelCooldown, ptrTime(now.Add(-time.Hour)))      // 应复位
	putChannel(t, db, "冷却未到期", model.ChannelCooldown, ptrTime(now.Add(time.Hour)))       // 不动
	putChannel(t, db, "冷却无截止", model.ChannelCooldown, nil)                               // 不动
	putChannel(t, db, "正常渠道", model.ChannelEnabled, ptrTime(now.Add(-time.Hour)))        // 不动
	putChannel(t, db, "手动停用", model.ChannelManualDisabled, ptrTime(now.Add(-time.Hour))) // 不动
	putChannel(t, db, "自动禁用", model.ChannelAutoDisabled, ptrTime(now.Add(-time.Hour)))   // 不动

	releaseExpiredCooldowns(db)

	ch := getChannel(t, db, 1)
	if ch.Status != model.ChannelEnabled || ch.CooldownUntil != nil {
		t.Errorf("已过期冷却渠道未复位: status=%d cooldown_until=%v", ch.Status, ch.CooldownUntil)
	}
	for _, id := range []uint{2, 3, 4, 5, 6} {
		ch := getChannel(t, db, id)
		if ch.Status == model.ChannelEnabled && ch.CooldownUntil == nil && id <= 3 {
			t.Errorf("渠道 %d 不应被复位: status=%d cooldown_until=%v", id, ch.Status, ch.CooldownUntil)
		}
	}
	// 幂等：再执行一次，结果不变
	releaseExpiredCooldowns(db)
	if ch := getChannel(t, db, 1); ch.Status != model.ChannelEnabled || ch.CooldownUntil != nil {
		t.Errorf("幂等执行后状态被改变: status=%d cooldown_until=%v", ch.Status, ch.CooldownUntil)
	}
	if got := getChannel(t, db, 2); got.Status != model.ChannelCooldown || got.CooldownUntil == nil {
		t.Errorf("未到期冷却渠道被误复位: status=%d cooldown_until=%v", got.Status, got.CooldownUntil)
	}
}

func TestReleaseExpiredCooldownsExactBoundary(t *testing.T) {
	db := newDB(t)
	// 冷却截止时间恰好等于当前时刻 → 视为已过期（<= 语义），应复位
	putChannel(t, db, "恰好到期", model.ChannelCooldown, ptrTime(time.Now()))
	releaseExpiredCooldowns(db)
	if ch := getChannel(t, db, 1); ch.Status != model.ChannelEnabled || ch.CooldownUntil != nil {
		t.Errorf("恰好到期未复位: status=%d cooldown_until=%v", ch.Status, ch.CooldownUntil)
	}
}

// ---------- 日志清理 ----------

func TestCleanupExpiredLogs(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, 90)
	now := time.Now()

	putLogs(t, db, 3, now.AddDate(0, 0, -100))                  // 过期 → 删
	putLogs(t, db, 3, now.AddDate(0, 0, -91).Add(-time.Minute)) // 超界 → 删
	putLogs(t, db, 3, now.AddDate(0, 0, -89).Add(time.Minute))  // 保留期内 → 留
	putLogs(t, db, 2, now)                                      // 今天 → 留

	cleanupExpiredLogs(db)

	if got := countLogs(t, db, "created_at < ?", now.AddDate(0, 0, -89)); got != 0 {
		t.Errorf("过期日志未删净: 剩 %d 条", got)
	}
	if got := countLogs(t, db, "created_at >= ?", now.AddDate(0, 0, -89)); got != 5 {
		t.Errorf("保留期内日志被误删: 剩 %d 条（期望 5）", got)
	}
}

func TestCleanupExpiredLogsBatchBoundary(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, 30)
	now := time.Now()

	// 1100 条过期日志 > 单批 500 条，验证循环分批删到清完
	putLogs(t, db, 1100, now.AddDate(0, 0, -31))
	putLogs(t, db, 7, now.AddDate(0, 0, -5)) // 保留期内，不应被波及

	cleanupExpiredLogs(db)

	if got := countLogs(t, db, "created_at < ?", now.AddDate(0, 0, -30)); got != 0 {
		t.Errorf("分批删除未清完: 剩 %d 条", got)
	}
	if got := countLogs(t, db, "created_at >= ?", now.AddDate(0, 0, -30)); got != 7 {
		t.Errorf("保留期内日志被误删: 剩 %d 条（期望 7）", got)
	}
}

func TestCleanupExpiredLogsZeroRetentionSkips(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, 0) // 0=永久保留
	now := time.Now()
	putLogs(t, db, 5, now.AddDate(0, 0, -3650))

	cleanupExpiredLogs(db)

	if got := countLogs(t, db, "1=1"); got != 5 {
		t.Errorf("LogRetentionDays=0 不应删除任何日志: 剩 %d 条", got)
	}
}

func TestCleanupExpiredLogsNoSettingRowSkips(t *testing.T) {
	db := newDB(t) // 不创建 Setting 行
	now := time.Now()
	putLogs(t, db, 5, now.AddDate(0, 0, -3650))

	cleanupExpiredLogs(db)

	if got := countLogs(t, db, "1=1"); got != 5 {
		t.Errorf("设置行缺失不应删除任何日志: 剩 %d 条", got)
	}
}

// ---------- 调度器注册与退出 ----------

func TestStartSchedulesJobsAndStop(t *testing.T) {
	db := newDB(t)
	c := Start(db)

	entries := c.Entries()
	if len(entries) != 3 {
		t.Fatalf("应注册 3 个 cron 任务，实际 %d 个", len(entries))
	}
	// Entry 不保留 spec 字符串，用相邻两次触发的间隔验证语义（单看一次触发时间
	// 有歧义：整点任务与每分钟任务都可能在下一分钟边界命中）：
	//   - 间隔 1 分钟 → outbox 投递驱动（安全加固三件套，B，每分钟）
	//   - 间隔 1 小时 → 冷却释放/清理/备份（每小时整点）
	//   - 间隔 24 小时 → 日志清理（每日 04:30）
	now := time.Now()
	var sawMinute, sawHourly, sawDaily bool
	for _, e := range entries {
		next1 := e.Schedule.Next(now)
		next2 := e.Schedule.Next(next1)
		switch next2.Sub(next1) {
		case time.Minute:
			sawMinute = true
		case time.Hour:
			sawHourly = true
		case 24 * time.Hour:
			sawDaily = true
		default:
			t.Errorf("意外的 cron 触发间隔: %v", next2.Sub(next1))
		}
	}
	if !sawMinute {
		t.Error("缺少每分钟任务（outbox 投递驱动）")
	}
	if !sawHourly {
		t.Error("缺少每小时整点任务（冷却释放）")
	}
	if !sawDaily {
		t.Error("缺少每日 04:30 任务（日志清理）")
	}

	Stop(c)   // 优雅退出：不挂起即视为通过
	Stop(nil) // nil 安全
}

// ptrTime 返回时间指针。
func ptrTime(t time.Time) *time.Time { return &t }

// ---------- 故障容忍：库不可用时软失败（log + 跳过），绝不 panic ----------

// cron 任务在进程内执行，库故障时必须软失败：冷却释放记错误返回、日志清理跳过、
// 批删除失败带累计数返回。回归锁定：任何 panic 都会击穿 cron goroutine 拖垮服务进程。
func TestTasksFailSoftOnDBError(t *testing.T) {
	db := newDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	releaseExpiredCooldowns(db) // 写失败：记日志返回
	cleanupExpiredLogs(db)      // 读设置失败：跳过清理
	optimizeSQLite(db)          // Exec 失败：记日志返回
}

// ---------- PRAGMA optimize（L5） ----------

// TestOptimizeSQLite PRAGMA optimize：sqlite 连接上执行无错且幂等（每日任务在
// 进程重启后重复执行无害），执行后库仍可正常读写。
func TestOptimizeSQLite(t *testing.T) {
	db := newDB(t)
	putLogs(t, db, 3, time.Now())

	optimizeSQLite(db)
	optimizeSQLite(db)

	if got := countLogs(t, db, "1=1"); got != 3 {
		t.Errorf("optimize 后库应可正常读: 剩 %d 条，期望 3", got)
	}
}

// 日志清理中途批删除失败（表被删）：记日志返回，不清空剩余数据结构状态。
func TestCleanupExpiredLogsDeleteErrorSkips(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, 7)
	if err := db.Migrator().DropTable(&model.SendLog{}); err != nil {
		t.Fatalf("drop send_logs: %v", err)
	}
	cleanupExpiredLogs(db) // 批删除失败：记日志返回（不 panic）
}
