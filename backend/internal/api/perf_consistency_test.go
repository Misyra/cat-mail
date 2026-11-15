// perf_consistency_test.go 列表/统计端点的 SQL 往返条数与多写原子性回归测试。
// 计数方式：GORM Query 回调按 Statement.Table 分类计数 SELECT；Update 回调注入
// 失败验证事务回滚。各用例断言值的依据见注释（requireSession 每请求 1 条
// users 查询确认账号存在，middleware.go）。
package api

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// queryCounter 按表名计数 SELECT（Statement.Table 在回调执行前已由
// processor.Execute 从 Model/Dest 解析填充）。SELECT 走两条回调链：
// Find/First/Count 走 Query 处理器（gorm:query），聚合 Scan（如
// keySentTotals/statsOverview 的 GROUP BY ... Scan）走 Row 处理器（gorm:row，
// finisher_api.go Scan→Rows()），两条都要挂才能计全。
type queryCounter struct {
	mu   sync.Mutex
	data map[string]int
}

// newQueryCounter 在 db 上注册 SELECT 计数回调并返回计数器。
func newQueryCounter(t *testing.T, db *gorm.DB) *queryCounter {
	t.Helper()
	qc := &queryCounter{data: map[string]int{}}
	count := func(tx *gorm.DB) {
		qc.mu.Lock()
		qc.data[tx.Statement.Table]++
		qc.mu.Unlock()
	}
	if err := db.Callback().Query().After("gorm:query").Register("test:count_queries", count); err != nil {
		t.Fatalf("register query counter: %v", err)
	}
	if err := db.Callback().Row().After("gorm:row").Register("test:count_row_queries", count); err != nil {
		t.Fatalf("register row counter: %v", err)
	}
	return qc
}

// reset 清零计数（种子数据落库不计入请求断言）。
func (qc *queryCounter) reset() {
	qc.mu.Lock()
	qc.data = map[string]int{}
	qc.mu.Unlock()
}

// snapshot 返回按表计数的快照。
func (qc *queryCounter) snapshot() map[string]int {
	qc.mu.Lock()
	defer qc.mu.Unlock()
	out := make(map[string]int, len(qc.data))
	for k, v := range qc.data {
		out[k] = v
	}
	return out
}

// assertTables 断言各表 SELECT 条数与期望完全一致（未列出的表必须为 0）。
func assertTables(t *testing.T, qc *queryCounter, want map[string]int) {
	t.Helper()
	got := qc.snapshot()
	for table, n := range want {
		if got[table] != n {
			t.Fatalf("table %s: want %d SELECT got %d (all: %v)", table, n, got[table], got)
		}
		delete(got, table)
	}
	for table, n := range got {
		if n != 0 {
			t.Fatalf("table %s: want 0 SELECT got %d (all: %v)", table, n, got)
		}
	}
}

// seedLog 直插一条发送日志（CreatedAt 显式指定，GORM 保留非零值）。
func seedLog(t *testing.T, db *gorm.DB, status int, isTest bool, createdAt time.Time) {
	t.Helper()
	if err := db.Create(&model.SendLog{
		MessageID: fmt.Sprintf("m-%d-%d", createdAt.UnixNano(), status),
		Status:    status,
		IsTest:    isTest,
		CreatedAt: createdAt,
	}).Error; err != nil {
		t.Fatalf("seed send log: %v", err)
	}
}

// TestListEndpointsQueryCount 列表/统计端点 SQL 条数回归（防 N+1 与往返回潮）。
// 每请求基准：requireSession 确认账号存在 1 条 users 查询（middleware.go:70）。
func TestListEndpointsQueryCount(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	for i := 0; i < 3; i++ {
		if err := db.Create(&model.ApiKey{Name: fmt.Sprintf("k%d", i), Key: fmt.Sprintf("cm-key%d", i), Enabled: true}).Error; err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}
	seedLog(t, db, model.SendStatusSent, false, time.Now())
	qc := newQueryCounter(t, db)

	// GET /api/keys：keys Find 1 + total_sent 单条 GROUP BY 1（keySentTotals）+
	// requireSession 1 = 3。keys Find 与 Key 行数无关（3 个 Key 不产生额外查询）。
	qc.reset()
	mustStatus(t, doReq(r, "GET", "/api/keys", cookie, "", ""), http.StatusOK)
	assertTables(t, qc, map[string]int{"users": 1, "sessions": 1, "api_keys": 1, "send_logs": 1})

	// GET /api/channels：列表 Find 1（DTO 逐行构造，无逐行查库）+ 近 7 日健康块
	// 三条固定聚合 SQL（A3 + 送达链路三件套：计数/最近成功失败 1 + 失败原因 Top3
	// 窗口函数 1 + mail_events 送达侧条件聚合 1，均与渠道数无关，非 N+1）+
	// requireSession 1 = 5。
	if err := db.Create(&model.Channel{Name: "ch", Provider: "mock", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	qc.reset()
	mustStatus(t, doReq(r, "GET", "/api/channels", cookie, "", ""), http.StatusOK)
	assertTables(t, qc, map[string]int{"users": 1, "sessions": 1, "channels": 1, "send_logs": 2, "mail_events": 1})

	// GET /api/users：列表 Find 1 + requireSession 1 = 2。
	qc.reset()
	mustStatus(t, doReq(r, "GET", "/api/users", cookie, "", ""), http.StatusOK)
	assertTables(t, qc, map[string]int{"users": 2, "sessions": 1})

	// GET /api/logs：COUNT 1 + 页查询 1（SPEC 要求返回 total，两条为下限；
	// 禁止三条以上）+ requireSession 1 = 3。
	qc.reset()
	mustStatus(t, doReq(r, "GET", "/api/logs?page_size=20", cookie, "", ""), http.StatusOK)
	assertTables(t, qc, map[string]int{"users": 1, "sessions": 1, "send_logs": 2})

	// GET /api/stats/overview：今日+本月合并聚合 1（status × is_today 单条
	// GROUP BY，原两条）+ 渠道用量按需选列 1 + requireSession 1 = 3（原 4）。
	qc.reset()
	mustStatus(t, doReq(r, "GET", "/api/stats/overview", cookie, "", ""), http.StatusOK)
	assertTables(t, qc, map[string]int{"users": 1, "sessions": 1, "send_logs": 1, "channels": 1})
}

// TestStatsOverviewMergedAggregation 合并聚合口径不变：今日数取 is_today=1 桶、
// 本月成功数取 status=sent 全桶，测试发送与本月之前的数据均排除。
func TestStatsOverviewMergedAggregation(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	now := time.Now()
	todayStart := midnight(now)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	// 今日：成功 2 / 失败 1 / 测试成功 1（排除）
	seedLog(t, db, model.SendStatusSent, false, todayStart.Add(time.Hour))
	seedLog(t, db, model.SendStatusSent, false, todayStart.Add(2*time.Hour))
	seedLog(t, db, model.SendStatusFailed, false, todayStart.Add(3*time.Hour))
	seedLog(t, db, model.SendStatusSent, true, todayStart.Add(4*time.Hour))

	// 本月早于今天：成功 1（计入 month_sent；今天为 1 号时该桶不存在，跳过）
	if todayStart.After(monthStart) {
		seedLog(t, db, model.SendStatusSent, false, todayStart.AddDate(0, 0, -1))
	}
	// 上月：成功 1（今日/本月均排除）
	seedLog(t, db, model.SendStatusSent, false, monthStart.AddDate(0, 0, -45))

	wantMonth := int64(2) // 今日成功计入本月
	if todayStart.After(monthStart) {
		wantMonth = 3
	}
	m := decode(t, mustStatus(t, doReq(r, "GET", "/api/stats/overview", cookie, "", ""), http.StatusOK))
	if got := int64(m["today_sent"].(float64)); got != 2 {
		t.Fatalf("today_sent: want 2 got %v", m["today_sent"])
	}
	if got := int64(m["today_failed"].(float64)); got != 1 {
		t.Fatalf("today_failed: want 1 got %v", m["today_failed"])
	}
	if got := int64(m["month_sent"].(float64)); got != wantMonth {
		t.Fatalf("month_sent: want %d got %v", wantMonth, m["month_sent"])
	}
	if got := int(m["success_rate"].(float64)); got != 67 { // round(2/3*100)
		t.Fatalf("success_rate: want 67 got %v", m["success_rate"])
	}
}

// TestResetPasswordAtomic 重置密码与目标用户会话纪元递增同事务（按用户纪元，O10）：
// 纪元写入失败时改密整体回滚（不留「密码已改、旧会话仍有效」的中间态）；
// 成功路径仅目标用户纪元 +1 且其旧会话失效，操作者会话不受影响。
func TestResetPasswordAtomic(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	mustStatus(t, doReq(r, "POST", "/api/users", cookie, "",
		`{"username":"bob","password":"bobpass1"}`), http.StatusOK)
	var bob model.User
	if err := db.Where("username = ?", "bob").First(&bob).Error; err != nil {
		t.Fatalf("load bob: %v", err)
	}
	bobCookie := login(t, r, "bob", "bobpass1") // 目标用户改密前的既有会话

	// 注入 session_epoch 更新失败（仅当 failEpoch 置位且为 users 表携带
	// session_epoch 的更新；改密与纪元递增同为 users 表更新，按 Dest 区分）
	var failEpoch atomic.Bool
	if err := db.Callback().Update().Before("gorm:update").Register("test:fail_epoch_update", func(tx *gorm.DB) {
		if !failEpoch.Load() {
			return
		}
		if _, ok := tx.Statement.Model.(*model.User); !ok {
			return
		}
		if m, ok := tx.Statement.Dest.(map[string]any); ok {
			if _, has := m["session_epoch"]; has {
				tx.AddError(errors.New("injected: session epoch update failure"))
			}
		}
	}); err != nil {
		t.Fatalf("register failure injector: %v", err)
	}

	// 纪元写入失败 → 事务回滚：密码不变、纪元不变、响应 500
	failEpoch.Store(true)
	w := doReq(r, "PUT", fmt.Sprintf("/api/users/%d/password", bob.ID), cookie, "",
		`{"new_password":"newpass1"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("injected failure: want 500 got %d body=%s", w.Code, w.Body.String())
	}
	var bobAfter model.User
	if err := db.First(&bobAfter, bob.ID).Error; err != nil {
		t.Fatalf("reload bob: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(bobAfter.PasswordHash), []byte("bobpass1")); err != nil {
		t.Fatalf("password must be rolled back: %v", err)
	}
	if bobAfter.SessionEpoch != 0 {
		t.Fatalf("epoch must stay 0 on rollback, got %d", bobAfter.SessionEpoch)
	}
	// 回滚后目标用户既有会话不受影响（纪元未动）
	mustStatus(t, doReq(r, "GET", "/api/auth/me", bobCookie, "", ""), http.StatusOK)

	// 注入解除 → 重试成功：目标用户纪元 +1、其旧会话因纪元失效被拒；操作者会话不受影响
	failEpoch.Store(false)
	mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/users/%d/password", bob.ID), cookie, "",
		`{"new_password":"newpass1"}`), http.StatusOK)
	var bobFinal model.User
	if err := db.First(&bobFinal, bob.ID).Error; err != nil {
		t.Fatalf("reload bob: %v", err)
	}
	if bobFinal.SessionEpoch != 1 {
		t.Fatalf("epoch must bump to 1, got %d", bobFinal.SessionEpoch)
	}
	if code := doReq(r, "GET", "/api/auth/me", bobCookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("bob old session should be invalidated after successful reset: %d", code)
	}
	if code := doReq(r, "GET", "/api/users", cookie, "", "").Code; code != http.StatusOK {
		t.Fatalf("operator session must survive resetting another user: %d", code)
	}
	login(t, r, "bob", "newpass1") // 新密码可重新登录
}

// TestChangeOwnPasswordAtomic 本人改密与会话纪元递增同事务（按用户纪元，O10，
// 与 resetUserPassword 口径对齐）：纪元写入失败时改密整体回滚（不留「密码已改、
// 旧会话仍有效」的中间态）；成功路径纪元 +1、本人旧会话失效，响应重签的新 cookie
// 保持当前会话可用。
func TestChangeOwnPasswordAtomic(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 注入 session_epoch 更新失败（仅当 failEpoch 置位且为 users 表携带
	// session_epoch 的更新；改密与纪元递增同为 users 表更新，按 Dest 区分）
	var failEpoch atomic.Bool
	if err := db.Callback().Update().Before("gorm:update").Register("test:fail_epoch_update_own", func(tx *gorm.DB) {
		if !failEpoch.Load() {
			return
		}
		if _, ok := tx.Statement.Model.(*model.User); !ok {
			return
		}
		if m, ok := tx.Statement.Dest.(map[string]any); ok {
			if _, has := m["session_epoch"]; has {
				tx.AddError(errors.New("injected: session epoch update failure"))
			}
		}
	}); err != nil {
		t.Fatalf("register failure injector: %v", err)
	}

	// 纪元写入失败 → 事务回滚：密码不变（旧密码仍可登录）、纪元不变、响应 500
	failEpoch.Store(true)
	if w := doReq(r, "PUT", "/api/auth/password", cookie, "",
		`{"old_password":"admin123","new_password":"newpass1"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("injected failure: want 500 got %d body=%s", w.Code, w.Body.String())
	}
	var admin model.User
	if err := db.Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatalf("load admin: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte("admin123")); err != nil {
		t.Fatalf("password must be rolled back: %v", err)
	}
	if admin.SessionEpoch != 0 {
		t.Fatalf("epoch must stay 0 on rollback, got %d", admin.SessionEpoch)
	}
	// 回滚后当前会话不受影响（纪元未动）
	mustStatus(t, doReq(r, "GET", "/api/auth/me", cookie, "", ""), http.StatusOK)

	// 注入解除 → 重试成功：纪元 +1，本人旧会话（旧纪元 cookie）失效被拒；
	// 响应重签的新 cookie 保持当前会话
	failEpoch.Store(false)
	w := doReq(r, "PUT", "/api/auth/password", cookie, "",
		`{"old_password":"admin123","new_password":"newpass1"}`)
	mustStatus(t, w, http.StatusOK)
	var fresh *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			fresh = c
		}
	}
	if fresh == nil || fresh.Value == "" {
		t.Fatalf("password change must re-sign session cookie")
	}
	if err := db.Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatalf("reload admin: %v", err)
	}
	if admin.SessionEpoch != 1 {
		t.Fatalf("epoch must bump to 1, got %d", admin.SessionEpoch)
	}
	if code := doReq(r, "GET", "/api/auth/me", cookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("own old session should be invalidated after change: %d", code)
	}
	mustStatus(t, doReq(r, "GET", "/api/auth/me", fresh.Value, "", ""), http.StatusOK)
	login(t, r, "admin", "newpass1") // 新密码可重新登录
}
