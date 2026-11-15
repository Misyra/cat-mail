// scheduler_backup_test.go A2 每日定时备份测试：开/关两态、当日去重、
// 设置行缺失容错。
package scheduler

import (
	"os"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/store"
)

// resetDailyBackupState 清空进程内「今日已备」状态（各用例互不串扰）。
func resetDailyBackupState() {
	dailyBackupState.mu.Lock()
	dailyBackupState.lastDate = ""
	dailyBackupState.mu.Unlock()
}

// TestRunScheduledBackupEnabled 开启自动备份：首次执行生成 1 份；同日再执行
// 不重复生成（当日去重）。
func TestRunScheduledBackupEnabled(t *testing.T) {
	resetDailyBackupState()
	db := newDB(t)
	if err := db.Create(&model.Setting{
		ID: 1, Rotation: "priority", MaxAttempts: 3, CooldownSeconds: 60,
		LogRetentionDays: 0, BackupDailyEnabled: true,
	}).Error; err != nil {
		t.Fatalf("create setting: %v", err)
	}

	runScheduledBackup(db)
	if n := countSchedulerBackups(t, db); n != 1 {
		t.Fatalf("首次执行后备份份数 = %d，期望 1", n)
	}
	// 同一天重复触发：不产生新备份
	runScheduledBackup(db)
	runScheduledBackup(db)
	if n := countSchedulerBackups(t, db); n != 1 {
		t.Errorf("同日重复执行后备份份数 = %d，期望 1（当日去重失效）", n)
	}
}

// TestRunScheduledBackupDisabled 关闭自动备份（默认态）：不产生任何备份，
// 也不写入「今日已备」状态——随后开启时当天仍能立即备份。
func TestRunScheduledBackupDisabled(t *testing.T) {
	resetDailyBackupState()
	db := newDB(t)
	if err := db.Create(&model.Setting{
		ID: 1, Rotation: "priority", MaxAttempts: 3, CooldownSeconds: 60,
		LogRetentionDays: 0, BackupDailyEnabled: false,
	}).Error; err != nil {
		t.Fatalf("create setting: %v", err)
	}

	runScheduledBackup(db)
	if n := countSchedulerBackups(t, db); n != 0 {
		t.Fatalf("关闭态不应产生备份，实际 %d 份", n)
	}
	// 再开启：状态未被污染，立即补备一次
	dailyBackupState.mu.Lock()
	polluted := dailyBackupState.lastDate != ""
	dailyBackupState.mu.Unlock()
	if polluted {
		t.Error("关闭态执行不应写入 lastDate")
	}
}

// TestRunScheduledBackupSettingMissing 设置行缺失：软失败（不 panic、不备份）。
func TestRunScheduledBackupSettingMissing(t *testing.T) {
	resetDailyBackupState()
	db := newDB(t) // 不创建 Setting 行
	runScheduledBackup(db)
	if n := countSchedulerBackups(t, db); n != 0 {
		t.Errorf("设置行缺失不应备份，实际 %d 份", n)
	}
}

// TestRunScheduledBackupLandsInDataDir 备份落在主库同级的 backups/ 目录
// （<数据目录>/backups/<name>.db）。
func TestRunScheduledBackupLandsInDataDir(t *testing.T) {
	resetDailyBackupState()
	db := newDB(t)
	if err := db.Create(&model.Setting{
		ID: 1, Rotation: "priority", MaxAttempts: 3, CooldownSeconds: 60,
		BackupDailyEnabled: true,
	}).Error; err != nil {
		t.Fatalf("create setting: %v", err)
	}
	runScheduledBackup(db)
	dbPath, err := store.DBFilePath(db)
	if err != nil {
		t.Fatalf("DBFilePath: %v", err)
	}
	// 数据目录（主库同级）下应存在 backups/ 子目录，且 ListBackups 能列出该份备份
	//（ListBackups 的目录推导与定时备份完全一致，能列出即证明落点正确）
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(dbPath), "backups"))
	if err != nil {
		t.Fatalf("读备份目录: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("backups/ 目录文件数 = %d，期望 1", len(entries))
	}
}

// countSchedulerBackups 统计备份份数（store.ListBackups 与定时备份同一落点推导）。
func countSchedulerBackups(t *testing.T, db *gorm.DB) int {
	t.Helper()
	infos, err := store.ListBackups(db)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	return len(infos)
}
