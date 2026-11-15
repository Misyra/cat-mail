// key_policy_test.go 调用方维度配额与限速（B5）API 级测试：真实 HTTP 层
// （doReq 直调 gin 引擎）覆盖——日额度耗尽 429 且不落日志、跨日重置（昨日日志
// 不计入今日）、quota=0 不限、每 key 令牌桶限速（注入短等待上限 + 虚拟时钟
// 单测双覆盖）、429 统一错误体 type/message 断言、默认零值 key 与无 key 路径
// 行为不变、PUT 指针更新语义（未提供=保留、显式 0=不限、负数 400）。
package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// createLimitedKey 创建带 daily_quota/rate_per_sec 的 Key（B5），返回 (明文, id)。
func createLimitedKey(t *testing.T, r *gin.Engine, cookie, body string) (string, uint) {
	t.Helper()
	w := doReq(r, "POST", "/api/keys", cookie, "", body)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	id, ok := m["id"].(float64)
	if !ok {
		t.Fatalf("create key: id missing: %s", w.Body.String())
	}
	plain, ok := m["key"].(string)
	if !ok || plain == "" {
		t.Fatalf("create key: key missing: %s", w.Body.String())
	}
	return plain, uint(id)
}

// sendWithKey 以指定 bearer POST /v1/send 一封最小邮件（走种子 mock 渠道，
// 默认额度 0/0=不限，不干扰调用方维度断言）。
func sendWithKey(r *gin.Engine, bearer string) *httptest.ResponseRecorder {
	return doReq(r, "POST", "/v1/send", "", bearer, sendBody("noreply@example.com", "rcpt@example.com", "B5 调用方策略"))
}

// errBodyOf 解析统一错误体，返回 (type, message)。
func errBodyOf(t *testing.T, body string) (string, string) {
	t.Helper()
	m := decode(t, body)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应非统一错误体：%s", body)
	}
	typ, _ := e["type"].(string)
	msg, _ := e["message"].(string)
	return typ, msg
}

// countKeyLogs 统计该 key 名下的发送日志总条数。
func countKeyLogs(t *testing.T, db *gorm.DB, keyID uint) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.SendLog{}).Where("api_key_id = ?", keyID).Count(&n).Error; err != nil {
		t.Fatalf("count key logs: %v", err)
	}
	return n
}

// TestApiKeyDailyQuotaBlocksSixthSend quota=5：连发 6 次，前 5 次 200、第 6 次
// 429 quota_exhausted（文案含 daily_quota=5），且被拒请求不落日志——强制点在
// 鉴权，请求被拒在门外（与渠道额度 429 语义对齐）。
func TestApiKeyDailyQuotaBlocksSixthSend(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	key, keyID := createLimitedKey(t, r, cookie, `{"name":"额度Key","daily_quota":5}`)

	for i := 1; i <= 5; i++ {
		if w := sendWithKey(r, key); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次：status=%d body=%s，want 200", i, w.Code, w.Body.String())
		}
	}
	w := sendWithKey(r, key)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 6 次：status=%d body=%s，want 429", w.Code, w.Body.String())
	}
	typ, msg := errBodyOf(t, w.Body.String())
	if typ != TypeQuotaExhausted {
		t.Fatalf("429 type=%q，want %q", typ, TypeQuotaExhausted)
	}
	if want := "ApiKey 今日额度已用尽（daily_quota=5）"; msg != want {
		t.Fatalf("429 message=%q，want %q", msg, want)
	}
	if n := countKeyLogs(t, db, keyID); n != 5 {
		t.Fatalf("key 日志 = %d，want 5（第 6 次被拒不进入发送流程、不落日志）", n)
	}
}

// TestApiKeyDailyQuotaCountsTodayOnly 跨日重置：注入昨日 status=sent 日志后，
// 今日计数从 0 起算——quota=2 时昨日 1 条不占今日额度，今日前 2 次照发、第 3
// 次才 429（本地日历日口径，与 stats 统计一致）。
func TestApiKeyDailyQuotaCountsTodayOnly(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	key, keyID := createLimitedKey(t, r, cookie, `{"name":"跨日Key","daily_quota":2}`)

	if err := db.Create(&model.SendLog{
		ApiKeyID:  keyID,
		Status:    model.SendStatusSent,
		ToAddrs:   `["yesterday@example.com"]`,
		Subject:   "昨日日志",
		CreatedAt: time.Now().AddDate(0, 0, -1),
	}).Error; err != nil {
		t.Fatalf("seed yesterday log: %v", err)
	}

	for i := 1; i <= 2; i++ {
		if w := sendWithKey(r, key); w.Code != http.StatusOK {
			t.Fatalf("今日第 %d 次：status=%d body=%s，want 200（昨日日志不计入今日额度）", i, w.Code, w.Body.String())
		}
	}
	if w := sendWithKey(r, key); w.Code != http.StatusTooManyRequests {
		t.Fatalf("今日第 3 次：status=%d body=%s，want 429", w.Code, w.Body.String())
	}
	// 昨日 1 条 + 今日 2 条 = 3；今日被拒的第 3 次不落日志
	if n := countKeyLogs(t, db, keyID); n != 3 {
		t.Fatalf("key 日志 = %d，want 3（昨日 1 + 今日 2，被拒不落）", n)
	}
}

// TestApiKeyZeroLimitsUnchanged 默认零值 key（quota=0、rate=0=均不限）：连发
// 8 次全部 200——零值走直通分支（不触 limiter、不做 COUNT），行为与 B5 之前
// 完全一致；无 key 请求照旧 401（策略检查在鉴权之后，未鉴权路径零影响）。
func TestApiKeyZeroLimitsUnchanged(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	key, keyID := createLimitedKey(t, r, cookie, `{"name":"默认Key"}`)

	for i := 1; i <= 8; i++ {
		if w := sendWithKey(r, key); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次：status=%d body=%s，want 200（零值=不限）", i, w.Code, w.Body.String())
		}
	}
	if n := countKeyLogs(t, db, keyID); n != 8 {
		t.Fatalf("key 日志 = %d，want 8", n)
	}
	if w := doReq(r, "POST", "/v1/send", "", "", sendBody("noreply@example.com", "x@example.com", "no key")); w.Code != http.StatusUnauthorized {
		t.Fatalf("无 key 请求：status=%d，want 401（未鉴权路径不受策略影响）", w.Code)
	}
}

// TestApiKeyRateLimitPerKey rate=2：四连发第 3/4 次被限（前两枚突发令牌耗尽，
// 2/s 的补充速率在毫秒级间隔内追不上）。注入 keyRateMaxWait=50ms 使等待超限
// 立即 429（小率直测 + 缩短等待上限，对齐 rotator 测试手法），耗时断言守护
// fail-fast（不走满 3s）；被限请求不落日志；限速按 key 隔离——同刻另一
// rate=0 的 key 正常发送。
func TestApiKeyRateLimitPerKey(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	key, keyID := createLimitedKey(t, r, cookie, `{"name":"限速Key","rate_per_sec":2}`)
	other, _ := createLimitedKey(t, r, cookie, `{"name":"旁路Key"}`)

	oldMaxWait := keyRateMaxWait
	keyRateMaxWait = 50 * time.Millisecond
	t.Cleanup(func() { keyRateMaxWait = oldMaxWait })

	for i := 1; i <= 2; i++ {
		if w := sendWithKey(r, key); w.Code != http.StatusOK {
			t.Fatalf("突发第 %d 次：status=%d body=%s，want 200（桶容量 max(1,2)=2）", i, w.Code, w.Body.String())
		}
	}
	start := time.Now()
	for i := 3; i <= 4; i++ {
		w := sendWithKey(r, key)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("第 %d 次：status=%d body=%s，want 429", i, w.Code, w.Body.String())
		}
		typ, msg := errBodyOf(t, w.Body.String())
		if typ != TypeRateLimited {
			t.Fatalf("429 type=%q，want %q", typ, TypeRateLimited)
		}
		if want := "请求过于频繁（rate_per_sec 限制）"; msg != want {
			t.Fatalf("429 message=%q，want %q", msg, want)
		}
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("两次限速拒绝耗时 %v，want <2s（等待超 50ms 即拒，不得走满 3s）", el)
	}
	if n := countKeyLogs(t, db, keyID); n != 2 {
		t.Fatalf("限速 key 日志 = %d，want 2（被限请求不进入发送流程）", n)
	}
	// 按 key 隔离：另一 rate=0 的 key 不受影响，正常发送
	if w := sendWithKey(r, other); w.Code != http.StatusOK {
		t.Fatalf("旁路 key：status=%d body=%s，want 200（限速按 key 隔离，0=不限直通）", w.Code, w.Body.String())
	}
}

// TestKeyPolicyUpdatePointerSemantics PUT 指针语义：未提供 = 保留；显式 0 =
// 改为不限；负数整体拒绝（400，库中值不变）；创建时负数同样 400；列表 DTO
// 暴露 daily_quota/rate_per_sec。
func TestKeyPolicyUpdatePointerSemantics(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	key, keyID := createLimitedKey(t, r, cookie, `{"name":"策略Key","daily_quota":5,"rate_per_sec":2}`)

	// 未提供 = 保留
	w := doReq(r, "PUT", fmt.Sprintf("/api/keys/%d", keyID), cookie, "", `{"enabled":true}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["daily_quota"].(float64) != 5 || m["rate_per_sec"].(float64) != 2 {
		t.Fatalf("空 PUT 后 quota/rate = %v/%v，want 5/2（nil=保留）", m["daily_quota"], m["rate_per_sec"])
	}
	// 显式更新单字段，另一字段保留
	w = doReq(r, "PUT", fmt.Sprintf("/api/keys/%d", keyID), cookie, "", `{"daily_quota":3}`)
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["daily_quota"].(float64) != 3 || m["rate_per_sec"].(float64) != 2 {
		t.Fatalf("更新后 quota/rate = %v/%v，want 3/2", m["daily_quota"], m["rate_per_sec"])
	}
	// 显式 0 = 改为不限
	w = doReq(r, "PUT", fmt.Sprintf("/api/keys/%d", keyID), cookie, "", `{"daily_quota":0,"rate_per_sec":0}`)
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["daily_quota"].(float64) != 0 || m["rate_per_sec"].(float64) != 0 {
		t.Fatalf("显式 0 后 = %v/%v，want 0/0（0=不限）", m["daily_quota"], m["rate_per_sec"])
	}
	// 负数整体拒绝，库中值保持不变（不做部分写入）
	for _, body := range []string{`{"daily_quota":-1}`, `{"rate_per_sec":-2}`} {
		if w := doReq(r, "PUT", fmt.Sprintf("/api/keys/%d", keyID), cookie, "", body); w.Code != http.StatusBadRequest {
			t.Fatalf("负数 PUT %s：status=%d body=%s，want 400", body, w.Code, w.Body.String())
		}
	}
	var k model.ApiKey
	if err := db.First(&k, keyID).Error; err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if k.DailyQuota != 0 || k.RatePerSec != 0 {
		t.Fatalf("负数拒绝后库值 = %d/%d，want 0/0（不做部分写入）", k.DailyQuota, k.RatePerSec)
	}
	// 创建时负数同样 400
	for _, body := range []string{`{"name":"负额度","daily_quota":-5}`, `{"name":"负限速","rate_per_sec":-1}`} {
		if w := doReq(r, "POST", "/api/keys", cookie, "", body); w.Code != http.StatusBadRequest {
			t.Fatalf("负数创建 %s：status=%d body=%s，want 400", body, w.Code, w.Body.String())
		}
	}
	// 列表 DTO 暴露两字段（前端弹窗回填依赖）
	w = doReq(r, "GET", "/api/keys", cookie, "", "")
	list := decode(t, mustStatus(t, w, http.StatusOK))
	items, ok := list["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("列表 items 异常：%s", w.Body.String())
	}
	first := items[0].(map[string]any)
	for _, f := range []string{"daily_quota", "rate_per_sec"} {
		if _, ok := first[f]; !ok {
			t.Fatalf("列表 DTO 缺字段 %q", f)
		}
	}
	_ = key // 明文仅断言用
}

// TestKeyRateLimiterVirtualClock 令牌桶数学（虚拟时钟，零睡眠）：burst=
// max(1,rate)、按真实时间比例匀速补充、限速配置变更整桶重建。
func TestKeyRateLimiterVirtualClock(t *testing.T) {
	l := newKeyRateLimiter()
	base := time.Unix(1700000000, 0)
	// rate=2：满桶 2 枚，第 3 枚需等 (1-0)/2=500ms
	if _, ok := l.takeToken(1, 2, base); !ok {
		t.Fatal("第 1 枚应立即可取")
	}
	if _, ok := l.takeToken(1, 2, base); !ok {
		t.Fatal("第 2 枚应立即可取（突发=桶容量）")
	}
	wait, ok := l.takeToken(1, 2, base)
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("第 3 枚 ok=%v wait=%v，want false/500ms", ok, wait)
	}
	// 250ms 后补了 0.5 枚：还差 0.25s
	wait, ok = l.takeToken(1, 2, base.Add(250*time.Millisecond))
	if ok || wait != 250*time.Millisecond {
		t.Fatalf("半程 ok=%v wait=%v，want false/250ms", ok, wait)
	}
	// 500ms 后补满 1 枚，可取
	if _, ok = l.takeToken(1, 2, base.Add(500*time.Millisecond)); !ok {
		t.Fatal("500ms 后应可取（2/s 匀速补充）")
	}
	// 配置变更（rate 2→4）→ 整桶重建：burst=max(1,4)=4，满桶立取
	for i := 0; i < 4; i++ {
		if _, ok = l.takeToken(1, 4, base.Add(600*time.Millisecond)); !ok {
			t.Fatalf("改 rate=4 后第 %d 枚应立即可取（重建即满桶）", i+1)
		}
	}
	if _, ok = l.takeToken(1, 4, base.Add(600*time.Millisecond)); ok {
		t.Fatal("重建桶取尽后第 5 枚应不可取")
	}
}

// TestKeyRateLimiterWaitRefuses wait 的拒绝路径：ctx 已取消 → 立即 false；
// 等待预算（注入 20ms）内补不满令牌 → false。首枚令牌恒直通。
func TestKeyRateLimiterWaitRefuses(t *testing.T) {
	l := newKeyRateLimiter()
	oldMaxWait := keyRateMaxWait
	keyRateMaxWait = 20 * time.Millisecond
	t.Cleanup(func() { keyRateMaxWait = oldMaxWait })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l.wait(ctx, 7, 1) {
		t.Fatal("已取消的 ctx 应立即拒绝")
	}
	bg := context.Background()
	if !l.wait(bg, 7, 1) {
		t.Fatal("首枚令牌应直通（新桶满桶）")
	}
	if l.wait(bg, 7, 1) {
		t.Fatal("20ms 预算内补不满 1 枚（1/s），应拒绝")
	}
}
