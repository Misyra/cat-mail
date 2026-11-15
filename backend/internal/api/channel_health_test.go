// channel_health_test.go 渠道健康度（A3）集成测试：GET /api/channels 附带的
// health 块。覆盖：有日志渠道聚合、零日志渠道 health=null、纯失败渠道、
// 测试发送（is_test）不计入、失败原因 Top3 截断与排序、7 日窗口外日志不统计、
// 多渠道隔离。
package api

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// addLog 测试库直插 SendLog（GORM 显式 CreatedAt 原样落库）。
func addLog(t *testing.T, db *gorm.DB, chID uint, status int, isTest bool, errText string, at time.Time) {
	t.Helper()
	lg := &model.SendLog{
		MessageID:   fmt.Sprintf("health-%d-%d", chID, at.UnixNano()),
		ChannelID:   chID,
		ChannelName: "健康测试渠道",
		Provider:    "mock",
		IsTest:      isTest,
		Status:      status,
		Error:       errText,
		Attempts:    1,
		CreatedAt:   at,
	}
	if err := db.Create(lg).Error; err != nil {
		t.Fatalf("seed send log: %v", err)
	}
}

// channelHealth 拉取渠道列表并返回指定渠道的 health 块（零日志渠道返回 nil）。
func channelHealth(t *testing.T, r *gin.Engine, cookie string, chID float64) map[string]any {
	t.Helper()
	w := doReq(r, "GET", "/api/channels", cookie, "", "")
	body := mustStatus(t, w, 200)
	var resp struct {
		Items []struct {
			ID     float64        `json:"id"`
			Health map[string]any `json:"health"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode channels: %v body=%s", err, body)
	}
	for _, it := range resp.Items {
		if it.ID == chID {
			return it.Health
		}
	}
	t.Fatalf("channel %v not found in list", chID)
	return nil
}

// healthNum 取 health 块数值字段。
func healthNum(t *testing.T, h map[string]any, key string) float64 {
	t.Helper()
	v, ok := h[key].(float64)
	if !ok {
		t.Fatalf("health.%s = %#v, want number", key, h[key])
	}
	return v
}

// parseHealthTime 解析 last_success_at/last_failed_at（RFC3339）。
func parseHealthTime(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("time field = %#v, want string", v)
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return at
}

// absDur 绝对时长。
func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// TestChannelHealthWithLogs 有日志渠道：计数/成功率/最近成功与失败时间聚合正确；
// 窗口外（8 天前）日志不计入。
func TestChannelHealthWithLogs(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"健康渠道A","provider":"mock"}`)
	id := uint(chID)

	now := time.Now()
	okAt := now.Add(-2 * time.Hour)
	failAt := now.Add(-1 * time.Hour)
	oldAt := now.Add(-8 * 24 * time.Hour) // 窗口外
	addLog(t, db, id, model.SendStatusSent, false, "", okAt)
	addLog(t, db, id, model.SendStatusSent, false, "", now.Add(-3*time.Hour))
	addLog(t, db, id, model.SendStatusSent, false, "", now.Add(-4*time.Hour))
	addLog(t, db, id, model.SendStatusFailed, false, "send timeout", failAt)
	addLog(t, db, id, model.SendStatusFailed, false, "send timeout", now.Add(-90*time.Minute))
	addLog(t, db, id, model.SendStatusSent, false, "", oldAt) // 8 天前：不统计

	h := channelHealth(t, r, cookie, chID)
	if h == nil {
		t.Fatalf("health = nil, want object")
	}
	if got := healthNum(t, h, "window_days"); got != 7 {
		t.Errorf("window_days = %v, want 7", got)
	}
	if got := healthNum(t, h, "total"); got != 5 {
		t.Errorf("total = %v, want 5（窗口外 1 条不计入）", got)
	}
	if got := healthNum(t, h, "sent"); got != 3 {
		t.Errorf("sent = %v, want 3", got)
	}
	if got := healthNum(t, h, "failed"); got != 2 {
		t.Errorf("failed = %v, want 2", got)
	}
	if got := healthNum(t, h, "success_rate"); got != 60 {
		t.Errorf("success_rate = %v, want 60", got)
	}
	// 最近成功 = -2h（MAX），最近失败 = -1h（MAX）；允许 ±2min 往返误差
	if got, want := parseHealthTime(t, h["last_success_at"]), okAt; absDur(got.Sub(want)) > 2*time.Minute {
		t.Errorf("last_success_at = %v, want ≈ %v", got, want)
	}
	if got, want := parseHealthTime(t, h["last_failed_at"]), failAt; absDur(got.Sub(want)) > 2*time.Minute {
		t.Errorf("last_failed_at = %v, want ≈ %v", got, want)
	}
	errs, ok := h["top_errors"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("top_errors = %#v, want 1 项", h["top_errors"])
	}
	e := errs[0].(map[string]any)
	if e["reason"] != "send timeout" || healthNum(t, e, "count") != 2 {
		t.Errorf("top_errors[0] = %#v, want {send timeout, 2}", e)
	}
}

// TestChannelHealthZeroLogs 零日志渠道：列表 health=null。
func TestChannelHealthZeroLogs(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"零日志渠道B","provider":"mock"}`)
	if h := channelHealth(t, r, cookie, chID); h != nil {
		t.Errorf("health = %#v, want nil（零日志渠道）", h)
	}
}

// TestChannelHealthAllFailed 纯失败渠道：成功率 0、last_success_at 为 null、
// top_errors 有失败原因。
func TestChannelHealthAllFailed(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"纯失败渠道C","provider":"mock"}`)
	id := uint(chID)

	now := time.Now()
	addLog(t, db, id, model.SendStatusFailed, false, "auth failed", now.Add(-2*time.Hour))
	addLog(t, db, id, model.SendStatusFailed, false, "quota exceeded", now.Add(-1*time.Hour))

	h := channelHealth(t, r, cookie, chID)
	if h == nil {
		t.Fatalf("health = nil, want object")
	}
	if got := healthNum(t, h, "success_rate"); got != 0 {
		t.Errorf("success_rate = %v, want 0", got)
	}
	if got := healthNum(t, h, "failed"); got != 2 {
		t.Errorf("failed = %v, want 2", got)
	}
	if h["last_success_at"] != nil {
		t.Errorf("last_success_at = %#v, want nil", h["last_success_at"])
	}
	if h["last_failed_at"] == nil {
		t.Errorf("last_failed_at = nil, want 时间戳")
	}
}

// TestChannelHealthExcludesTestLogs 测试发送不计入：is_test=true 的成功日志
// 与失败日志均被排除（与 stats 口径一致）。
func TestChannelHealthExcludesTestLogs(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"测试隔离渠道D","provider":"mock"}`)
	id := uint(chID)

	now := time.Now()
	// 正式：1 成功 1 失败
	addLog(t, db, id, model.SendStatusSent, false, "", now.Add(-2*time.Hour))
	addLog(t, db, id, model.SendStatusFailed, false, "boom", now.Add(-1*time.Hour))
	// 测试发送：成功+失败各一条，均不计入
	addLog(t, db, id, model.SendStatusSent, true, "", now.Add(-30*time.Minute))
	addLog(t, db, id, model.SendStatusFailed, true, "test boom", now.Add(-20*time.Minute))

	h := channelHealth(t, r, cookie, chID)
	if h == nil {
		t.Fatalf("health = nil, want object")
	}
	if got := healthNum(t, h, "total"); got != 2 {
		t.Errorf("total = %v, want 2（is_test 两条排除）", got)
	}
	if got := healthNum(t, h, "sent"); got != 1 {
		t.Errorf("sent = %v, want 1", got)
	}
	if got := healthNum(t, h, "failed"); got != 1 {
		t.Errorf("failed = %v, want 1", got)
	}
	if got := healthNum(t, h, "success_rate"); got != 50 {
		t.Errorf("success_rate = %v, want 50", got)
	}
	// 测试失败原因也不进 top_errors
	if errs, _ := h["top_errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["reason"] != "boom" {
		t.Errorf("top_errors = %#v, want 仅 [boom]", h["top_errors"])
	}
}

// TestChannelHealthTopErrorsTruncated 失败原因 Top3：按次数降序截断，第 4 名丢弃。
func TestChannelHealthTopErrorsTruncated(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"Top原因渠道E","provider":"mock"}`)
	id := uint(chID)

	now := time.Now()
	seed := []struct {
		err   string
		count int
	}{
		{"quota exceeded (5xx)", 5},
		{"connection timeout", 3},
		{"tls handshake failed", 2},
		{"dns lookup failed", 1}, // 第 4 名：应被截断
	}
	for _, s := range seed {
		for i := 0; i < s.count; i++ {
			addLog(t, db, id, model.SendStatusFailed, false, s.err, now.Add(-time.Duration(i+1)*time.Hour))
		}
	}

	h := channelHealth(t, r, cookie, chID)
	if h == nil {
		t.Fatalf("health = nil, want object")
	}
	errs, ok := h["top_errors"].([]any)
	if !ok || len(errs) != 3 {
		t.Fatalf("top_errors = %#v, want 恰好 3 项", h["top_errors"])
	}
	want := []struct {
		reason string
		count  float64
	}{
		{"quota exceeded (5xx)", 5},
		{"connection timeout", 3},
		{"tls handshake failed", 2},
	}
	for i, w := range want {
		e := errs[i].(map[string]any)
		if e["reason"] != w.reason || healthNum(t, e, "count") != w.count {
			t.Errorf("top_errors[%d] = {%v, %v}, want {%s, %v}", i, e["reason"], e["count"], w.reason, w.count)
		}
	}
	if got := healthNum(t, h, "failed"); got != 11 {
		t.Errorf("failed = %v, want 11", got)
	}
}

// TestChannelHealthPerChannelIsolation 多渠道互不串扰：各自聚合各自的窗口数据。
func TestChannelHealthPerChannelIsolation(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chA := createChannel(t, r, cookie, `{"name":"隔离渠道A","provider":"mock"}`)
	chB := createChannel(t, r, cookie, `{"name":"隔离渠道B","provider":"mock"}`)

	now := time.Now()
	addLog(t, db, uint(chA), model.SendStatusSent, false, "", now.Add(-1*time.Hour))
	addLog(t, db, uint(chB), model.SendStatusFailed, false, "b failed", now.Add(-1*time.Hour))

	hA := channelHealth(t, r, cookie, chA)
	hB := channelHealth(t, r, cookie, chB)
	if got := healthNum(t, hA, "sent"); got != 1 {
		t.Errorf("A sent = %v, want 1", got)
	}
	if got := healthNum(t, hA, "failed"); got != 0 {
		t.Errorf("A failed = %v, want 0", got)
	}
	if got := healthNum(t, hB, "failed"); got != 1 {
		t.Errorf("B failed = %v, want 1", got)
	}
	if got := healthNum(t, hB, "success_rate"); got != 0 {
		t.Errorf("B success_rate = %v, want 0", got)
	}
}
