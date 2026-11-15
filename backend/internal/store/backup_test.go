// backup_test.go A2 数据备份 store 层测试：VACUUM INTO 快照合法性、保留裁剪、
// 文件名白名单与列表/删除。覆盖了 CreateBackup 对 glebarez/sqlite VACUUM INTO
// 支持性的实测断言（生成物必须是可打开查询的合法 SQLite）。
package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// backupDirOf 经主库路径推出备份目录（与 backupDir 同一推导，供测试定位文件）。
func backupDirOf(t *testing.T, db *gorm.DB) string {
	t.Helper()
	dbPath, err := DBFilePath(db)
	if err != nil {
		t.Fatalf("DBFilePath: %v", err)
	}
	return filepath.Join(filepath.Dir(dbPath), "backups")
}

// countBackupFiles 统计备份目录内白名单命中的文件数。
func countBackupFiles(t *testing.T, db *gorm.DB) int {
	t.Helper()
	infos, err := ListBackups(db)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	return len(infos)
}

// TestDBFilePath 主库路径可从 *gorm.DB 解析（A2 备份定位数据目录的基石）。
func TestDBFilePath(t *testing.T) {
	db := openSeeded(t)
	path, err := DBFilePath(db)
	if err != nil {
		t.Fatalf("DBFilePath: %v", err)
	}
	if filepath.Base(path) != "catmail.db" {
		t.Errorf("主库路径异常: %s", path)
	}
}

// TestCreateBackupValidSQLite 备份文件必须是合法 SQLite：新连接打开后能查询到
// send_logs 表且行数与主库一致（VACUUM INTO 支持性实测断言）。
func TestCreateBackupValidSQLite(t *testing.T) {
	db := openSeeded(t)
	// 主库写入 3 条日志，备份后应原样可查
	logs := make([]model.SendLog, 3)
	for i := range logs {
		logs[i] = model.SendLog{MessageID: "m-1", Status: model.SendStatusSent}
	}
	if err := db.CreateInBatches(logs, 10).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}

	name, size, err := CreateBackup(db, time.Now())
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if !BackupFileNameRe.MatchString(name) {
		t.Errorf("备份名 %s 不符合白名单", name)
	}
	if size <= 0 {
		t.Errorf("备份文件大小异常: %d", size)
	}
	// 文件落在 <数据目录>/backups/ 下
	if _, err := ListBackups(db); err != nil {
		t.Fatalf("ListBackups: %v", err)
	}

	// 用全新连接打开备份文件（只做原生查询，不做迁移），验证其合法性
	bdb, err := gorm.Open(sqlite.Open(filepath.Join(backupDirOf(t, db), name)), &gorm.Config{})
	if err != nil {
		t.Fatalf("打开备份文件: %v", err)
	}
	defer func() {
		if sqlDB, err := bdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}()
	var n int64
	if err := bdb.Table("send_logs").Count(&n).Error; err != nil {
		t.Fatalf("备份内查询 send_logs: %v", err)
	}
	if n != 3 {
		t.Errorf("备份内 send_logs 行数 = %d，期望 3", n)
	}
	// 备份目录内除白名单文件外不应有 -wal/-shm 残留（VACUUM INTO 产出单文件快照）
	entries, err := os.ReadDir(backupDirOf(t, db))
	if err != nil {
		t.Fatalf("读备份目录: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-wal") || strings.HasSuffix(e.Name(), "-shm") {
			t.Errorf("备份目录残留 %s（快照应为单文件）", e.Name())
		}
	}
}

// TestCreateBackupTrimsToTen 保留最近 10 份：连造 12 份（逐秒推进时间戳），
// 最旧的 2 份应被裁剪。
func TestCreateBackupTrimsToTen(t *testing.T) {
	db := openSeeded(t)
	base := time.Date(2026, 1, 2, 15, 4, 5, 0, time.Local)
	for i := 0; i < 12; i++ {
		name, _, err := CreateBackup(db, base.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatalf("CreateBackup #%d: %v", i, err)
		}
		want := "catmail-backup-" + base.Add(time.Duration(i)*time.Second).Format("20060102-150405") + ".db"
		if name != want {
			t.Fatalf("第 %d 次备份名 = %s，期望 %s", i, name, want)
		}
	}
	infos, err := ListBackups(db)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(infos) != BackupKeep {
		t.Fatalf("备份份数 = %d，期望保留 %d", len(infos), BackupKeep)
	}
	// 最旧的两份（i=0,1）应被裁掉；现存最旧为 i=2
	oldest := infos[len(infos)-1].Name
	wantOldest := "catmail-backup-" + base.Add(2*time.Second).Format("20060102-150405") + ".db"
	if oldest != wantOldest {
		t.Errorf("现存最旧备份 = %s，期望 %s（旧档未按序裁剪）", oldest, wantOldest)
	}
	// 列表按文件名倒序 = 时间倒序
	for i := 1; i < len(infos); i++ {
		if infos[i-1].Name <= infos[i].Name {
			t.Errorf("列表未按倒序排列: %s 应在 %s 之前", infos[i-1].Name, infos[i].Name)
		}
	}
}

// TestCreateBackupSameSecondCollision 同一秒重复备份：目标已存在时按秒推进重试，
// 两次都应成功且文件名不同。
func TestCreateBackupSameSecondCollision(t *testing.T) {
	db := openSeeded(t)
	now := time.Date(2026, 6, 1, 8, 0, 30, 0, time.Local)
	name1, _, err := CreateBackup(db, now)
	if err != nil {
		t.Fatalf("第一次备份: %v", err)
	}
	name2, _, err := CreateBackup(db, now) // 同一时间戳：首个名字已占用
	if err != nil {
		t.Fatalf("同一秒第二次备份: %v", err)
	}
	if name1 == name2 {
		t.Errorf("两次备份文件名相同: %s", name1)
	}
	if got := countBackupFiles(t, db); got != 2 {
		t.Errorf("备份份数 = %d，期望 2", got)
	}
}

// TestBackupFileNameRe 白名单严格性：仅放行 catmail-backup-YYYYMMDD-HHMMSS.db，
// 拒绝 ../、反斜杠、任意名与后缀变换（杜绝路径穿越）。
func TestBackupFileNameRe(t *testing.T) {
	valid := "catmail-backup-20260102-150405.db"
	if !BackupFileNameRe.MatchString(valid) {
		t.Errorf("合法名被拒绝: %s", valid)
	}
	for _, name := range []string{
		"../" + valid,                           // 相对路径穿越
		"..\\catmail-backup-20260102-150405.db", // Windows 分隔符
		valid + ".bak",                          // 附加后缀
		valid + "-x",                            // 附加尾巴
		"catmail-backup-202601-150405.db",       // 日期位数不足
		"catmail-backup-20260102-1504.db",       // 时间位数不足
		"catmail-backup-20260102-150405",        // 缺扩展名
		"foo.db",                                // 任意名
		"catmail-backup-20260102-150405.db/",    // 目录分隔符
		"catmail-backup-20260102-150405.DB",     // 大小写
		"",                                      // 空名
		"..",                                    // 纯穿越段
		"catmail-backup-20260102-150405.db%00",  // 截断符
	} {
		if BackupFileNameRe.MatchString(name) {
			t.Errorf("非法名被放行: %q", name)
		}
	}
}

// TestListBackupsEmptyWhenNoDir 尚无备份目录（从未备份）时列表为空且不报错。
func TestListBackupsEmptyWhenNoDir(t *testing.T) {
	db := openSeeded(t)
	infos, err := ListBackups(db)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(infos) != 0 {
		t.Errorf("期望空列表，实际 %d 条", len(infos))
	}
}

// TestDeleteBackup 删除：合法名删除成功且文件消失；非法名返回
// ErrInvalidBackupName 且绝不解析出路径；不存在的合法名返回文件不存在错误。
func TestDeleteBackup(t *testing.T) {
	db := openSeeded(t)
	name, _, err := CreateBackup(db, time.Now())
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := DeleteBackup(db, name); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if got := countBackupFiles(t, db); got != 0 {
		t.Errorf("删除后备份份数 = %d，期望 0", got)
	}

	// 非法名：白名单拒绝，绝不触达文件系统
	if err := DeleteBackup(db, "../evil.db"); !errors.Is(err, ErrInvalidBackupName) {
		t.Errorf("非法名应返回 ErrInvalidBackupName，实际: %v", err)
	}
	if path, err := BackupFilePath(db, "../evil.db"); err == nil || path != "" {
		t.Errorf("非法名不应解析出路径: path=%q err=%v", path, err)
	}
	if err := DeleteBackup(db, "catmail-backup-20260102-150405.db"); !os.IsNotExist(err) {
		t.Errorf("不存在的备份应返回文件不存在错误，实际: %v", err)
	}
}
