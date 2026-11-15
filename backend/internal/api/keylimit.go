// keylimit.go 调用方维度限速（B5）：进程内每 ApiKey 令牌桶，强制点在发送入口
// 鉴权（authAPIKey → enforceKeyPolicy），仅带 key 的发送路径触达。
//
// 语义（模式参考 rotator/ratelimit.go 的渠道级令牌桶，独立实现而非复用：键控
// 维度不同——渠道桶按 channelID 服务 failover 跳过，本桶按 apiKeyID 服务 429
// 拒绝；且 rotator 不对外暴露其内部类型）：
//   - rate_per_sec = 0 → 不限速：enforceKeyPolicy 直通，不触碰 limiter（零开销）；
//   - 桶容量 burst = max(1, rate)，令牌按真实时间匀速补充（rate 封/秒）；
//   - key 限速配置变更后（桶内 rate 快照与当前不同）下次使用时整桶重建，改
//     配置立即生效（重建即满桶，接受一次短暂的突发窗口）；
//   - 等待超过 keyRateMaxWait（3s）或请求 ctx 取消 → 拒绝 → 429 rate_limited；
//   - 进程重启丢状态可接受：只影响重启后的第一个突发窗口，不影响长期速率。
//
// 令牌与额度语义独立：令牌代表「这一秒的发送资格」，预留失败不退还（下个时间
// 窗口自然补充）；被限速拒绝的请求不进入发送流程、不落发送日志、不耗日额度。
package api

import (
	"context"
	"math"
	"sync"
	"time"
)

// keyRateMaxWait 单请求因限速等待的上限：超过即 429 拒绝（B5 契约 3s）。
// 等待期间请求未进入发送流程、不占任何额度与渠道资源；包级变量仅为测试可注入
// （缩短等待，对齐 rotator.rateLimitMaxWait 的做法）。
var keyRateMaxWait = 3 * time.Second

// keyTokenBucket 单 ApiKey 令牌桶。rate 为 key 配置快照（封/秒）；配置变更后
// takeToken 发现 rate 不一致即整桶重建。
type keyTokenBucket struct {
	rate   float64   // 令牌补充速率（封/秒）
	burst  float64   // 桶容量 = max(1, rate)
	tokens float64   // 当前令牌数
	last   time.Time // 上次补充时刻
}

// keyRateLimiter 进程内每 ApiKey 令牌桶集合。key 删除后的残留条目无害（与
// rotator.limiterBuckets 同取舍：条目数有限、随进程生命周期消亡；AutoIncrement
// 也不会复用 ID）。
type keyRateLimiter struct {
	mu      sync.Mutex
	buckets map[uint]*keyTokenBucket // apiKeyID → 桶
}

func newKeyRateLimiter() *keyRateLimiter {
	return &keyRateLimiter{buckets: make(map[uint]*keyTokenBucket)}
}

// keyLimiters 按 *Server 实例键控的 limiter 注册表：每个 Server 惰性持有独立
// 实例（首个带限速 key 的请求触发创建），测试环境每个 newTestEnv 独立 Register
// 即得全新 limiter，用例间零串扰。
//
// 选型说明：常规做法是 Server 加字段并在 Register 构造（对齐 loginLimiter /
// adminCIDRs 的装配风格），但 server.go 当前由并行任务占用（C4 2FA 代理在改），
// B5 不触碰该文件——以包级注册表等价实现「Server 持有 limiter 实例 + Register
// 内部惰性创建默认实例」的语义。终审波可收纳为 Server.keyRateLimiter 字段并在
// Register 中 newLoginLimiter() 旁构造一行（删除本注册表与 keyLimiter 方法），
// 本文件其余逻辑不变。生产进程仅 Register 一次，注册表恒 1 条；测试多条随进程
// 退出释放。
var keyLimiters sync.Map // *Server → *keyRateLimiter

// keyLimiter 返回该 Server 的调用方限速器（惰性创建，LoadOrStore 兜底并发首用）。
func (s *Server) keyLimiter() *keyRateLimiter {
	if v, ok := keyLimiters.Load(s); ok {
		return v.(*keyRateLimiter)
	}
	v, _ := keyLimiters.LoadOrStore(s, newKeyRateLimiter())
	return v.(*keyRateLimiter)
}

// takeToken 原子补充并尝试取走一个令牌（now 由调用方传入，测试可注虚拟时钟）。
// ok=false 时返回距下一个可用令牌的预计等待时长（按真实时间匀速补充，等待多久
// 补多少，不罚时不预支）。
func (l *keyRateLimiter) takeToken(keyID uint, ratePerSec int, now time.Time) (waitFor time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[keyID]
	if b == nil || b.rate != float64(ratePerSec) {
		// 懒创建；key 限速配置变更（rate 不同）→ 重建：改配置立即生效。
		burst := math.Max(1, float64(ratePerSec))
		b = &keyTokenBucket{rate: float64(ratePerSec), burst: burst, tokens: burst, last: now}
		l.buckets[keyID] = b
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

// wait 阻塞直至拿到令牌、等待超 keyRateMaxWait 或 ctx 取消：
//   - 拿到令牌 → true，调用方继续后续鉴权与发送；
//   - 等待超限/取消 → false，调用方返回 429 rate_limited。
func (l *keyRateLimiter) wait(ctx context.Context, keyID uint, ratePerSec int) bool {
	// 入口先查取消：客户端已离开的请求不再消耗令牌（即便桶里有现货），
	// 也避免「取消请求占走一枚令牌、挤压后续请求」的微小不公。
	if err := ctx.Err(); err != nil {
		return false
	}
	deadline := time.Now().Add(keyRateMaxWait)
	if cd, ok := ctx.Deadline(); ok && cd.Before(deadline) {
		deadline = cd // 请求自带更早的截止时间时以其为准
	}
	for {
		waitFor, ok := l.takeToken(keyID, ratePerSec, time.Now())
		if ok {
			return true
		}
		now := time.Now()
		if now.Add(waitFor).After(deadline) {
			waitFor = deadline.Sub(now) // 截断到 deadline，做最后一次尝试
		}
		if waitFor <= 0 {
			// 已到等待上限：最后再试一次（等待期间令牌可能已自然补充）。
			_, ok = l.takeToken(keyID, ratePerSec, time.Now())
			return ok
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(waitFor):
		}
	}
}
