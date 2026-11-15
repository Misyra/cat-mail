// keypolicy.go Key 级限速与日额度（B5）在 SMTP 入口的强制点：认证会话的每封
// 邮件发送前按命中 ApiKey 的 rate_per_sec / daily_quota 拦截，语义对齐 api 层
// enforceKeyPolicy——先限速后额度，被拒不发送、不落发送日志。Key 的日额度按
// SendLog（status=sent, api_key_id）聚合统计，SMTP 发送同样落库归因，故与
// /v1/send 共用同一额度池；未认证（loopback 免认证）会话无 Key，直接放行。
package smtpserver

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"catmail/internal/model"
)

// keyRateMaxWait 单次发送因 Key 限速等待的上限（对齐 api 层 B5 契约 3s）；等待
// 期间该 SMTP 命令挂起（客户端无感知阻塞，超时即 452 临时拒绝）。
var keyRateMaxWait = 3 * time.Second

// enforceKeyPolicy 校验当前会话 Key 的限速与日额度；ok=false 时返回应答码与文案。
// 读失败 fail-closed（451 临时失败）：额度口径宁可保守也不放行。
func (se *session) enforceKeyPolicy() (code int, text string, ok bool) {
	if se.apiKeyID == 0 {
		return 0, "", true
	}
	var ak model.ApiKey
	// 每封邮件重读 Key：会话可能存活数分钟，改配置（额度/限速）应即时生效。
	if err := se.srv.db.First(&ak, se.apiKeyID).Error; err != nil {
		log.Printf("[smtp] key policy load failed (key_id=%d): %v", se.apiKeyID, err)
		return 451, "4.3.0 Temporary failure, try again later", false
	}
	if ak.RatePerSec > 0 && !se.srv.limiter.wait(ak.ID, ak.RatePerSec) {
		return 452, "4.2.1 Request too frequent (rate_per_sec limited)", false
	}
	if ak.DailyQuota > 0 {
		var sent int64
		err := se.srv.db.Model(&model.SendLog{}).
			Where("api_key_id = ? AND status = ? AND created_at >= ?",
				ak.ID, model.SendStatusSent, midnight(time.Now())).
			Count(&sent).Error
		if err != nil {
			log.Printf("[smtp] key quota count failed (key_id=%d): %v", se.apiKeyID, err)
			return 451, "4.3.0 Temporary failure, try again later", false
		}
		if sent >= int64(ak.DailyQuota) {
			return 452, fmt.Sprintf("4.2.1 ApiKey 今日额度已用尽（daily_quota=%d）", ak.DailyQuota), false
		}
	}
	return 0, "", true
}

// midnight 本地时区当日零点（与 api 层统计/额度口径一致）。
func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// ---------- Key 级令牌桶 ----------

// keyBucket 单 ApiKey 令牌桶：rate 为 Key 配置快照（封/秒），配置变更后整桶重建。
type keyBucket struct {
	rate, burst, tokens float64
	last                time.Time
}

// keyLimiter 进程内按 ApiKey 的令牌桶（B5 rate_per_sec 在 SMTP 路径的实现）。
// 语义对齐 api/keylimit.go：桶容量 max(1, rate)、令牌按真实时间匀速补充、配置
// 变更整桶重建、等待超限即拒绝。独立小实现而非复用：api 包的桶键控在其 *Server
// 上且未导出，SMTP 侧拒绝语义是 452 应答而非 HTTP 429，键控生命周期也不同
// （smtpserver.Server 恒单例，桶随进程存亡）。
type keyLimiter struct {
	mu      sync.Mutex
	buckets map[uint]*keyBucket
}

// take 原子补充并尝试取走一个令牌；ok=false 时返回距下一枚可用令牌的等待时长。
func (l *keyLimiter) take(keyID uint, ratePerSec int, now time.Time) (waitFor time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = make(map[uint]*keyBucket)
	}
	b := l.buckets[keyID]
	if b == nil || b.rate != float64(ratePerSec) {
		burst := math.Max(1, float64(ratePerSec))
		b = &keyBucket{rate: float64(ratePerSec), burst: burst, tokens: burst, last: now}
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

// wait 阻塞直至拿到令牌或等待超 keyRateMaxWait。
func (l *keyLimiter) wait(keyID uint, ratePerSec int) bool {
	deadline := time.Now().Add(keyRateMaxWait)
	for {
		waitFor, ok := l.take(keyID, ratePerSec, time.Now())
		if ok {
			return true
		}
		now := time.Now()
		if now.Add(waitFor).After(deadline) {
			waitFor = deadline.Sub(now) // 截断到 deadline，做最后一次尝试
		}
		if waitFor <= 0 {
			_, ok = l.take(keyID, ratePerSec, time.Now())
			return ok
		}
		time.Sleep(waitFor)
	}
}
