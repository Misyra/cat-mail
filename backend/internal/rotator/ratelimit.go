// ratelimit.go 渠道级出站速率限制（A1）：进程内令牌桶，按渠道懒创建。
//
// 语义（SPEC §2 轮换算法配套）：
//   - rate_limit_per_sec = 0 → 不限速，取令牌直通，零额外延迟；
//   - 桶容量 burst = max(1, rate)，令牌按真实时间匀速补充（rate 封/秒）；
//   - 渠道限速配置变更后（桶内记录的 rate 与当前不同）下次使用时整桶重建，
//     改配置立即生效（重建即满桶，接受一次短暂的突发窗口）；
//   - 进程重启丢状态可接受：只影响重启后的第一个突发窗口，不影响长期速率。
//
// 令牌与额度语义独立：令牌代表「这一秒的出站资格」，预留失败或发送失败都不
// 退还（下个时间窗口自然补充）——失败重试同样受限于出站速率，不会穿透平台限速。
package rotator

import (
	"context"
	"math"
	"sync"
	"time"

	"catmail/internal/model"
)

// rateLimitMaxWait 单个候选因限速等待的上限：超过即跳过该候选继续 failover。
// 等待期间不占额度、不计尝试、不累计失败（限速视作「临时不可用」，是刻意的
// failover 语义）。包级变量仅为测试可注入（缩短等待）。
var rateLimitMaxWait = 5 * time.Second

// tokenBucket 单渠道令牌桶。rate 为渠道配置快照（封/秒）；配置变更后
// takeToken 发现 rate 不一致即整桶重建。
type tokenBucket struct {
	rate   float64   // 令牌补充速率（封/秒）
	burst  float64   // 桶容量 = max(1, rate)
	tokens float64   // 当前令牌数
	last   time.Time // 上次补充时刻
}

var (
	limiterMu      sync.Mutex
	limiterBuckets = map[uint]*tokenBucket{} // channelID → 桶；渠道删除后的残留条目无害（数量有限）
)

// takeToken 原子补充并尝试取走一个令牌。ok=false 时返回距下一个可用令牌的
// 预计等待时长（按真实时间匀速补充，等待多久补多少，不罚时不预支）。
func takeToken(channelID uint, ratePerSec int) (waitFor time.Duration, ok bool) {
	limiterMu.Lock()
	defer limiterMu.Unlock()

	now := time.Now()
	b := limiterBuckets[channelID]
	if b == nil || b.rate != float64(ratePerSec) {
		// 懒创建；渠道限速配置变更（rate 不同）→ 重建：改配置立即生效。
		burst := math.Max(1, float64(ratePerSec))
		b = &tokenBucket{rate: float64(ratePerSec), burst: burst, tokens: burst, last: now}
		limiterBuckets[channelID] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}
	return time.Duration((1 - b.tokens) / b.rate * float64(time.Second)), false
}

// tryAcquire 为候选渠道获取一个出站令牌（rotator.Send 逐候选调用）：
//   - rate_limit_per_sec ≤ 0 → 不限速，直通；
//   - 拿到令牌 → true，调用方继续原子预留额度并发送（顺序：先令牌、再预留、
//     再发送——避免等待期间占用已预留的额度）；
//   - 等待超过 rateLimitMaxWait 或 ctx 取消 → false，调用方跳过该候选
//     （不计 tried、不失败计数、不预留），继续下一候选。
func tryAcquire(ctx context.Context, ch *model.Channel) bool {
	if ch.RateLimitPerSec <= 0 {
		return true // 0=不限：零延迟直通
	}
	deadline := time.Now().Add(rateLimitMaxWait)
	if cd, ok := ctx.Deadline(); ok && cd.Before(deadline) {
		deadline = cd // ctx 自带更早的截止时间时以其为准
	}
	for {
		waitFor, ok := takeToken(ch.ID, ch.RateLimitPerSec)
		if ok {
			return true
		}
		now := time.Now()
		if now.Add(waitFor).After(deadline) {
			waitFor = deadline.Sub(now) // 截断到 deadline，做最后一次尝试
		}
		if waitFor <= 0 {
			// 已到等待上限：最后再试一次（等待期间令牌可能已自然补充）。
			_, ok = takeToken(ch.ID, ch.RateLimitPerSec)
			return ok
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(waitFor):
		}
	}
}
