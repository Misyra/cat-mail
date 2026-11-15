// ratelimit_test.go 渠道级出站速率限制（A1）语义：
// rate=0 零延迟直通、大 rate 直通、小 rate 令牌节流（真实时间下限）、
// 等待超时跳过→failover、全候选限速→429 限速文案、改配置桶重建立即生效。
package rotator

import (
	"context"
	"strings"
	"testing"
	"time"

	"catmail/internal/model"
)

// resetRateLimiters 清空全局桶表：桶按 channelID 全局键控，而各用例的临时库
// 自增 ID 会重叠，不清会跨用例串状态。
func resetRateLimiters() {
	limiterMu.Lock()
	limiterBuckets = map[uint]*tokenBucket{}
	limiterMu.Unlock()
}

// shortenMaxWait 缩短限速等待上限（用例免等 5s），返回恢复函数。
func shortenMaxWait(d time.Duration) func() {
	old := rateLimitMaxWait
	rateLimitMaxWait = d
	return func() { rateLimitMaxWait = old }
}

// rate=0（默认零值）完全不限速：连续 5 封每封耗时 <100ms（零额外延迟直通）。
func TestRateLimitZeroNoDelay(t *testing.T) {
	resetRateLimiters()
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "c1"}) // RateLimitPerSec 零值 = 不限

	for i := 0; i < 5; i++ {
		start := time.Now()
		out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
		wantNoErr(t, rerr)
		if out.ChannelName != "c1" {
			t.Fatalf("send #%d: channel = %q, want c1", i+1, out.ChannelName)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Fatalf("send #%d 耗时 %v, want <100ms（rate=0 不引入延迟）", i+1, elapsed)
		}
	}
}

// rate 为大值：burst=max(1,rate) 一次性覆盖整批，10 封直通无等待。
func TestRateLimitLargeRatePassesThrough(t *testing.T) {
	resetRateLimiters()
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "c1", RateLimitPerSec: 1000})

	start := time.Now()
	for i := 0; i < 10; i++ {
		if _, rerr := Send(context.Background(), db, sendInput("user@example.com", tr)); rerr != nil {
			t.Fatalf("send #%d: %v", i+1, rerr)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("10 封耗时 %v, want <1s（rate=1000 全部直通）", elapsed)
	}
}

// rate=2 小值节流：前 2 封吃满 burst 直通，后 4 封各等一个新令牌（≈0.5s/个）
// → 总耗时真实下限 ≈2s。用真实时间断言下限（容忍 ±20%）：限速被绕过时 6 封
// 会瞬间完成；上限防实现死等。
func TestRateLimitThrottlesSmallRate(t *testing.T) {
	resetRateLimiters()
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "c1", RateLimitPerSec: 2}) // burst=2

	const n = 6
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, rerr := Send(context.Background(), db, sendInput("user@example.com", tr)); rerr != nil {
			t.Fatalf("send #%d: %v（超出限速应排队等待而非报错）", i+1, rerr)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 1600*time.Millisecond {
		t.Errorf("%d 封耗时 %v, want ≥1.6s（rate=2：前 2 封吃 burst、后 4 封按 0.5s/令牌节流）", n, elapsed)
	}
	if elapsed > 8*time.Second {
		t.Errorf("%d 封耗时 %v, want ≤8s（远超理论值，实现存在死等）", n, elapsed)
	}
}

// 限速等待超时跳过 → failover：a（rate=1，burst 已被上一封取走）在等待上限内
// 拿不到令牌被跳过——不计 tried、不失败计数、不预留额度——换不限速的 b 成功。
func TestRateLimitWaitTimeoutFailsOver(t *testing.T) {
	resetRateLimiters()
	defer shortenMaxWait(50 * time.Millisecond)()
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "a", Priority: -1, RateLimitPerSec: 1}) // burst=1，恒排最前
	addChannel(t, db, &model.Channel{Name: "b"})                                   // 不限速

	// 第一次：a 取走唯一令牌成功。
	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	wantNoErr(t, rerr)
	if out.ChannelName != "a" {
		t.Fatalf("第一次 channel = %q, want a", out.ChannelName)
	}
	// 第二次：a 的令牌未补充（等待 50ms 仅补 0.05 个）→ 等待超时跳过 → b 成功。
	out, rerr = Send(context.Background(), db, sendInput("user@example.com", tr))
	wantNoErr(t, rerr)
	if out.ChannelName != "b" {
		t.Fatalf("第二次 channel = %q, want b（a 限速等待超时跳过后 failover）", out.ChannelName)
	}
	if strings.Join(tr.calls, ",") != "a,b" {
		t.Errorf("calls = %v, want [a,b]", tr.calls)
	}
	// 限速跳过不是失败：a 保持 enabled、连续失败 0、额度未被预留。
	a := reloadChannel(t, db, "a")
	if a.Status != model.ChannelEnabled || a.ConsecutiveFailures != 0 || a.UsedDay != 1 {
		t.Errorf("a: status=%d cf=%d used_day=%d, want enabled/0/1（仅第一次成功计 1）",
			a.Status, a.ConsecutiveFailures, a.UsedDay)
	}
}

// 全部候选都因限速等待跳过（tried==0）→ 429 quota_exhausted + 限速文案，
// 不调用适配器、不落发送日志。
func TestRateLimitAllCandidatesSkippedReturns429(t *testing.T) {
	resetRateLimiters()
	defer shortenMaxWait(50 * time.Millisecond)()
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "a", RateLimitPerSec: 1}) // 唯一候选，burst=1

	// 第一次：取走唯一令牌成功。
	if _, rerr := Send(context.Background(), db, sendInput("user@example.com", tr)); rerr != nil {
		t.Fatalf("第一次: %v", rerr)
	}
	// 第二次：唯一候选限速等待超时跳过 → 429 出站限速文案。
	_, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	if rerr == nil || rerr.HTTPStatus != 429 || rerr.Type != ErrQuotaExhausted {
		t.Fatalf("err = %+v, want 429 quota_exhausted", rerr)
	}
	if !strings.Contains(rerr.Message, "出站限速") {
		t.Errorf("message = %q, want 含「出站限速」", rerr.Message)
	}
	if len(tr.calls) != 1 {
		t.Errorf("calls = %d, want 1（第二次未实际发送）", len(tr.calls))
	}
	var logs int64
	db.Model(&model.SendLog{}).Count(&logs)
	if logs != 1 {
		t.Errorf("send logs = %d, want 1（限速跳过不落日志）", logs)
	}
}

// 渠道限速配置变更 → 下次使用时桶按新 rate 重建（满桶）→ 立即放行（改配置立即生效）。
func TestRateLimitRebuildOnConfigChange(t *testing.T) {
	resetRateLimiters()
	defer shortenMaxWait(200 * time.Millisecond)()
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "c1", RateLimitPerSec: 1}) // burst=1

	// 第一次：取走唯一令牌。
	if _, rerr := Send(context.Background(), db, sendInput("user@example.com", tr)); rerr != nil {
		t.Fatalf("第一次: %v", rerr)
	}
	// 放宽限速到 1000/s：旧桶（rate=1，令牌已耗尽）与新配置不符 → 重建满桶 → 直通。
	if err := db.Model(&model.Channel{}).Where("name = ?", "c1").
		Update("rate_limit_per_sec", 1000).Error; err != nil {
		t.Fatalf("update channel: %v", err)
	}
	start := time.Now()
	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	wantNoErr(t, rerr)
	if out.ChannelName != "c1" {
		t.Fatalf("channel = %q, want c1", out.ChannelName)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("改配置后发送耗时 %v, want <1s（桶按新 rate 重建立即放行）", elapsed)
	}
}
