// channel_test_quota_test.go 渠道测试直发的额度语义（端到端，SPEC §5 渠道测试端点）：
// 发送前原子预留、受理成功保留扣减（含惰性周期重置）、发送失败释放、额度耗尽
// 429 且不落发送日志；停用渠道仍可测试；失败不触发连续失败累计/状态机。
package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"catmail/internal/model"
	"catmail/internal/provider"
)

// TestChannelTestQuotaExhausted429 日/月额度用尽时测试不执行：429 + 固定文案，
// 未实际发送故不落发送日志，计数不动（fail-closed）。
func TestChannelTestQuotaExhausted429(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	today, month := time.Now().Format(dayLayout), time.Now().Format(monthLayout)

	// 当日额度用尽：daily_limit_override=1 且 used_day=1（anchor=今天）
	chID := createChannel(t, r, cookie,
		`{"name":"q1","provider":"mock","from_email":"noreply@example.com","daily_limit_override":1}`)
	db.Model(&model.Channel{}).Where("id = ?", uint(chID)).
		Updates(map[string]any{"used_day": 1, "day_anchor": today})

	w := doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID), cookie, "", `{"to":"dest@example.com"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status: want 429 got %d body=%s", w.Code, w.Body.String())
	}
	// O8：429 走 SPEC §2 统一错误体 {error:{type,message}}（不再是扁平 {ok:false,error}）
	m := decode(t, w.Body.String())
	errObj, _ := m["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "quota_exhausted" ||
		errObj["message"] != "渠道当日/当月额度已用尽，测试发送未执行" {
		t.Fatalf("429 body: %v", m)
	}
	var n int64
	db.Model(&model.SendLog{}).Where("is_test = ?", true).Count(&n)
	if n != 0 {
		t.Fatalf("429 must not write send log, got %d", n)
	}
	var ch model.Channel
	db.First(&ch, uint(chID))
	if ch.UsedDay != 1 {
		t.Fatalf("used_day mutated on 429: %d", ch.UsedDay)
	}

	// 当月额度用尽：monthly_limit_override=1 且 used_month=1（日额度未超）
	chID2 := createChannel(t, r, cookie,
		`{"name":"q2","provider":"mock","from_email":"noreply@example.com","monthly_limit_override":1}`)
	db.Model(&model.Channel{}).Where("id = ?", uint(chID2)).
		Updates(map[string]any{"used_month": 1, "month_anchor": month})
	w = doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID2), cookie, "", `{"to":"dest@example.com"}`)
	mustStatus(t, w, http.StatusTooManyRequests)
}

// TestChannelTestQuotaReservedOnSuccess 成功：预留转最终记账（日/月 +1，anchor 推进）；
// 过期 anchor 的旧计数（哪怕远超上限）先惰性重置再起算，而非误判超额。
func TestChannelTestQuotaReservedOnSuccess(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie,
		`{"name":"q3","provider":"mock","from_email":"noreply@example.com","daily_limit_override":2,"monthly_limit_override":2}`)

	w := doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID), cookie, "", `{"to":"dest@example.com"}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["ok"] != true {
		t.Fatalf("resp: %v", m)
	}
	now := time.Now()
	var ch model.Channel
	db.First(&ch, uint(chID))
	if ch.UsedDay != 1 || ch.UsedMonth != 1 ||
		ch.DayAnchor != now.Format(dayLayout) || ch.MonthAnchor != now.Format(monthLayout) {
		t.Fatalf("quota not reserved: used=%d/%d anchor=%q/%q",
			ch.UsedDay, ch.UsedMonth, ch.DayAnchor, ch.MonthAnchor)
	}

	db.Model(&model.Channel{}).Where("id = ?", uint(chID)).Updates(map[string]any{
		"used_day": 999, "day_anchor": "2000-01-01", "used_month": 999, "month_anchor": "2000-01"})
	w = doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID), cookie, "", `{"to":"dest@example.com"}`)
	mustStatus(t, w, http.StatusOK)
	db.First(&ch, uint(chID))
	if ch.UsedDay != 1 || ch.UsedMonth != 1 {
		t.Fatalf("stale anchor not reset: used=%d/%d", ch.UsedDay, ch.UsedMonth)
	}
}

// TestChannelTestFailureReleasesQuota 失败：释放预留（used 回 0）、落失败日志，
// 且不改渠道状态机（status 仍 0、consecutive_failures 仍 0）。
// smtp 指向本机保留端口 1：立即 ECONNREFUSED，零外网。
func TestChannelTestFailureReleasesQuota(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie,
		`{"name":"sf","provider":"smtp","from_email":"noreply@example.com","smtp_host":"127.0.0.1","smtp_port":1}`)

	w := doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID), cookie, "", `{"to":"dest@example.com"}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["ok"] != false || m["error"] == "" {
		t.Fatalf("resp: %v", m)
	}
	var ch model.Channel
	db.First(&ch, uint(chID))
	if ch.UsedDay != 0 || ch.UsedMonth != 0 {
		t.Fatalf("quota not released on failure: used=%d/%d", ch.UsedDay, ch.UsedMonth)
	}
	if ch.Status != model.ChannelEnabled || ch.ConsecutiveFailures != 0 {
		t.Fatalf("channel state machine changed: status=%d failures=%d", ch.Status, ch.ConsecutiveFailures)
	}
	var lg model.SendLog
	if err := db.Where("is_test = ? AND channel_id = ?", true, uint(chID)).First(&lg).Error; err != nil {
		t.Fatalf("failed test log missing: %v", err)
	}
	if lg.Status != model.SendStatusFailed {
		t.Fatalf("log status: %d", lg.Status)
	}
}

// TestChannelTestDisabledChannelAllowed 停用渠道仍可测试（现状保留）：成功照常预留扣减。
func TestChannelTestDisabledChannelAllowed(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie,
		`{"name":"q4","provider":"mock","from_email":"noreply@example.com"}`)
	mustStatus(t, doReq(r, "POST", fmt.Sprintf("/api/channels/%g/status", chID), cookie, "",
		`{"action":"disable"}`), http.StatusOK)

	w := doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID), cookie, "", `{"to":"dest@example.com"}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["ok"] != true {
		t.Fatalf("disabled channel should be testable: %v", m)
	}
	var ch model.Channel
	db.First(&ch, uint(chID))
	if ch.UsedDay != 1 {
		t.Fatalf("quota not reserved on disabled channel test: %d", ch.UsedDay)
	}
}

// panicTransport Send 必 panic 的假适配器（panic 配对释放回归测试专用）。
type panicTransport struct{}

func (panicTransport) Name() string { return "panicmock" }

func (panicTransport) Send(context.Context, *model.Channel, *provider.Message) (*provider.Result, *provider.ProviderError) {
	panic("mock: 模拟适配器 panic")
}

// TestChannelTestPanicReleasesQuota panic 安全（O8）：transport.Send panic 时
// （gin Recovery 兜底为 500、进程不崩）预留额度被 defer 配对释放、不静默多计，
// 且不落发送日志（未实际发送）。
func TestChannelTestPanicReleasesQuota(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	provider.Register(panicTransport{})
	chID := createChannel(t, r, cookie,
		`{"name":"pk","provider":"panicmock","from_email":"noreply@example.com"}`)

	// panic 在 handler 内被 gin.Recovery 兜底：客户端收 500，进程不崩
	w := doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID), cookie, "", `{"to":"dest@example.com"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("panic should surface as 500 via Recovery, got %d body=%s", w.Code, w.Body.String())
	}
	var ch model.Channel
	db.First(&ch, uint(chID))
	if ch.UsedDay != 0 || ch.UsedMonth != 0 {
		t.Fatalf("quota not released after panic: used_day=%d used_month=%d, want 0/0",
			ch.UsedDay, ch.UsedMonth)
	}
	var n int64
	db.Model(&model.SendLog{}).Where("is_test = ?", true).Count(&n)
	if n != 0 {
		t.Fatalf("panic must not write send log, got %d", n)
	}
}
