// handler_backup_test.go A2 数据管理 API 测试：备份创建/列表/下载/删除
// （白名单与未鉴权）与日志手动清理三模式（分批删除）。
package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/store"
)

// TestBackupsRequireAuth 未鉴权访问备份/清理接口一律 401（备份含全库数据，
// 泄露面等同主库）。
func TestBackupsRequireAuth(t *testing.T) {
	r, _ := newTestEnv(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/backups"},
		{"POST", "/api/backups"},
		{"GET", "/api/backups/catmail-backup-20260102-150405.db/download"},
		{"DELETE", "/api/backups/catmail-backup-20260102-150405.db"},
		{"POST", "/api/logs/purge"},
	} {
		w := doReq(r, tc.method, tc.path, "", "", "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 未鉴权应 401，实际 %d", tc.method, tc.path, w.Code)
		}
	}
}

// TestCreateListDownloadDeleteBackup 完整链路：创建 → 列表 → 下载 → 删除。
func TestCreateListDownloadDeleteBackup(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 创建
	w := doReq(r, "POST", "/api/backups", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	name, _ := m["name"].(string)
	size, _ := m["size"].(float64)
	if !store.BackupFileNameRe.MatchString(name) {
		t.Errorf("响应备份名 %q 不符合白名单", name)
	}
	if size <= 0 {
		t.Errorf("响应 size 异常: %v", size)
	}
	if got := backupTestCountFiles(t, db); got != 1 {
		t.Fatalf("备份后文件数 = %d，期望 1", got)
	}

	// 列表：name/size/created_at
	w = doReq(r, "GET", "/api/backups", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	items, _ := m["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("列表条数 = %d，期望 1", len(items))
	}
	item, _ := items[0].(map[string]any)
	if item["name"] != name {
		t.Errorf("列表 name = %v，期望 %s", item["name"], name)
	}
	if _, ok := item["created_at"].(string); !ok {
		t.Errorf("列表 created_at 缺失或非字符串: %v", item["created_at"])
	}

	// 下载：附件流（Content-Disposition）
	w = doReq(r, "GET", "/api/backups/"+name+"/download", cookie, "", "")
	mustStatus(t, w, http.StatusOK)
	if disp := w.Header().Get("Content-Disposition"); !strings.Contains(disp, "attachment") {
		t.Errorf("下载响应缺少 attachment 头: %q", disp)
	}
	if len(w.Body.Bytes()) == 0 {
		t.Error("下载响应体为空")
	}

	// 删除 → 文件消失
	w = doReq(r, "DELETE", "/api/backups/"+name, cookie, "", "")
	mustStatus(t, w, http.StatusOK)
	if got := backupTestCountFiles(t, db); got != 0 {
		t.Errorf("删除后备份文件数 = %d，期望 0", got)
	}

	// 不存在的备份：下载与删除均 404
	w = doReq(r, "GET", "/api/backups/"+name+"/download", cookie, "", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("下载不存在的备份应 404，实际 %d", w.Code)
	}
	w = doReq(r, "DELETE", "/api/backups/"+name, cookie, "", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("删除不存在的备份应 404，实际 %d", w.Code)
	}
}

// TestBackupNameWhitelist 下载与删除的 :name 严格白名单：任意名一律 400；
// 含穿越段（解码后含 /）的路径不落入参数路由——两者都不得以 200 触达文件。
func TestBackupNameWhitelist(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	// 先造一份合法备份，确认白名单拒绝不是「目录为空」的巧合
	if w := doReq(r, "POST", "/api/backups", cookie, "", ""); w.Code != http.StatusOK {
		t.Fatalf("seed backup: %d %s", w.Code, w.Body.String())
	}

	badNames := []string{
		"..",                                    // 纯穿越段
		"..\\catmail-backup-20260102-150405",    // Windows 分隔符
		"catmail-backup-20260102-150405.db.bak", // 附加后缀
		"foo.db",                                // 任意名
		"catmail-backup-202601-150405.db",       // 日期位数不足
	}
	for _, name := range badNames {
		w := doReq(r, "GET", "/api/backups/"+url.PathEscape(name)+"/download", cookie, "", "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("下载非法名 %q 应 400，实际 %d", name, w.Code)
		}
		w = doReq(r, "DELETE", "/api/backups/"+url.PathEscape(name), cookie, "", "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("删除非法名 %q 应 400，实际 %d", name, w.Code)
		}
	}

	// 显式穿越（解码后含 /）：不得 200（拒绝即通过，路径不触达文件）
	for _, raw := range []string{
		"/api/backups/..%2Fcatmail-backup-20260102-150405.db/download",
		"/api/backups/%2e%2e%2f%2e%2e%2fetc%2fpasswd/download",
	} {
		w := doReq(r, "GET", raw, cookie, "", "")
		if w.Code == http.StatusOK {
			t.Errorf("穿越路径 %s 不得 200", raw)
		}
	}
}

// TestLogsPurgeBeforeDays 按时间清理：只删早于 now-days 的日志（保留期内不动）。
func TestLogsPurgeBeforeDays(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	now := time.Now()
	purgeTestPutLogs(t, db, 5, now.AddDate(0, 0, -40))
	purgeTestPutLogs(t, db, 3, now.AddDate(0, 0, -1))

	w := doReq(r, "POST", "/api/logs/purge", cookie, "", `{"mode":"before_days","days":30}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if deleted, _ := m["deleted"].(float64); deleted != 5 {
		t.Errorf("deleted = %v，期望 5", m["deleted"])
	}
	if got := purgeTestCountLogs(t, db); got != 3 {
		t.Errorf("剩余日志 = %d，期望 3（保留期内不得误删）", got)
	}
}

// TestLogsPurgeBeforeDaysBatching >500 条走分批循环删到清完（1100 条过期 + 2 条保留）。
func TestLogsPurgeBeforeDaysBatching(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	now := time.Now()
	purgeTestPutLogs(t, db, 1100, now.AddDate(0, 0, -400))
	purgeTestPutLogs(t, db, 2, now)

	w := doReq(r, "POST", "/api/logs/purge", cookie, "", `{"mode":"before_days","days":365}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if deleted, _ := m["deleted"].(float64); deleted != 1100 {
		t.Errorf("deleted = %v，期望 1100（分批循环未删净）", m["deleted"])
	}
	if got := purgeTestCountLogs(t, db); got != 2 {
		t.Errorf("剩余日志 = %d，期望 2", got)
	}
}

// TestLogsPurgeChannel 按渠道清理：目标渠道日志清空，其他渠道日志不受影响；
// 已清空后再次执行幂等（deleted=0）。
func TestLogsPurgeChannel(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	now := time.Now()
	purgeTestPutLogs(t, db, 4, now)
	logs := make([]model.SendLog, 3)
	for i := range logs {
		logs[i] = model.SendLog{MessageID: fmt.Sprintf("ch2-%d", i), ChannelID: 2, Status: model.SendStatusSent, CreatedAt: now}
	}
	if err := db.CreateInBatches(logs, 10).Error; err != nil {
		t.Fatalf("seed channel2 logs: %v", err)
	}

	w := doReq(r, "POST", "/api/logs/purge", cookie, "", `{"mode":"channel","channel_id":2}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if deleted, _ := m["deleted"].(float64); deleted != 3 {
		t.Errorf("deleted = %v，期望 3", m["deleted"])
	}
	var remain int64
	if err := db.Model(&model.SendLog{}).Where("channel_id <> 2").Count(&remain).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if remain != 4 {
		t.Errorf("其他渠道日志被误删: 剩 %d，期望 4", remain)
	}
	w = doReq(r, "POST", "/api/logs/purge", cookie, "", `{"mode":"channel","channel_id":2}`)
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if deleted, _ := m["deleted"].(float64); deleted != 0 {
		t.Errorf("二次清理 deleted = %v，期望 0", m["deleted"])
	}
}

// TestLogsPurgeAll 清空模式：1250 条（>2 批）全部删除，表结构保留但数据为空。
func TestLogsPurgeAll(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	purgeTestPutLogs(t, db, 1250, time.Now().Add(-time.Hour))

	w := doReq(r, "POST", "/api/logs/purge", cookie, "", `{"mode":"all"}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if deleted, _ := m["deleted"].(float64); deleted != 1250 {
		t.Errorf("deleted = %v，期望 1250", m["deleted"])
	}
	if got := purgeTestCountLogs(t, db); got != 0 {
		t.Errorf("清空后剩 %d 条，期望 0", got)
	}
	// 表仍在（保留表结构，非 DROP）
	if !db.Migrator().HasTable(&model.SendLog{}) {
		t.Error("all 模式后 send_logs 表应保留")
	}
}

// TestLogsPurgeValidation 参数校验：mode 非法、days 越界/缺失、channel_id 缺失 → 400。
func TestLogsPurgeValidation(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	for _, body := range []string{
		`{}`,
		`{"mode":"nope"}`,
		`{"mode":"before_days"}`,             // 缺 days
		`{"mode":"before_days","days":0}`,    // 越界下
		`{"mode":"before_days","days":3651}`, // 越界上
		`{"mode":"channel"}`,                 // 缺 channel_id
		`{"mode":"channel","channel_id":0}`,
	} {
		w := doReq(r, "POST", "/api/logs/purge", cookie, "", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s 应 400，实际 %d（%s）", body, w.Code, w.Body.String())
		}
	}
}

// ---------- 测试小工具（命名带前缀，避免与并行用例的既有 helper 重名） ----------

// backupTestCountFiles 统计当前备份文件数（经 store.ListBackups）。
func backupTestCountFiles(t *testing.T, db *gorm.DB) int {
	t.Helper()
	infos, err := store.ListBackups(db)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	return len(infos)
}

// purgeTestPutLogs 批量写入 n 条指定创建时间的发送日志。
func purgeTestPutLogs(t *testing.T, db *gorm.DB, n int, createdAt time.Time) {
	t.Helper()
	logs := make([]model.SendLog, n)
	for i := range logs {
		logs[i] = model.SendLog{
			MessageID: fmt.Sprintf("purge-%d-%s", n, createdAt.Format("0102150405")),
			Status:    model.SendStatusSent,
			CreatedAt: createdAt,
		}
	}
	if err := db.CreateInBatches(logs, 100).Error; err != nil {
		t.Fatalf("create %d logs: %v", n, err)
	}
}

// purgeTestCountLogs 统计日志总数。
func purgeTestCountLogs(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.SendLog{}).Count(&n).Error; err != nil {
		t.Fatalf("count logs: %v", err)
	}
	return n
}
