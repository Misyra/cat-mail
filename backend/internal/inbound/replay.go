// replay.go 入站签名防重放的进程内去重缓存：Svix 按消息 id、Mailgun 按
// token（官方文档建议的 token 一次性缓存）。签名时间戳容差挡住窗口外重放，
// 本缓存挡住窗口内重放——两层合起来构成完整防重放。
//
// 进程内存态（map+互斥锁，风格对齐 api 层 resendGate）：重启即清零，清零窗口
// 内被截获的请求理论上可重放一次——webhook 属低危附加数据（事件已存档，
// 重复摄取同一条只多一行 mail_events），进程内缓存是复杂度/收益的合理折中；
// 若需跨重启强防重放，后续可落库唯一索引兜底（留作演进方向，不在本期）。
package inbound

import (
	"sync"
	"time"
)

// replayTTL 缓存条目存活时长：与最宽容差（Mailgun 15min）对齐再放宽一档，
// 保证「容差窗口内的重放」必落在缓存存活期内。
const replayTTL = 30 * time.Minute

// replayTTL 窗口内的条目上限（防御性）：同 key 重复摄取不刷新 TTL，极端风暴
// 下条目数 = 30 分钟内不同签名 id 数，远低于该上限，超限直接整体清空重来
// （防重放宁可短暂失效也不无界增长内存）。
const replayMaxEntries = 65536

// replayCache 签名防重放缓存（key → 首次记录时刻）。
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// Claim 记录 key 并返回是否为首次出现（false = 窗口内重复，调用方按重放拒绝）。
func (c *replayCache) Claim(key string) bool {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]time.Time)
	}
	if len(c.seen) >= replayMaxEntries {
		c.seen = make(map[string]time.Time) // 防御性整体重置（见 replayMaxEntries 注释）
	}
	if first, ok := c.seen[key]; ok && now.Sub(first) < replayTTL {
		return false // 窗口内重复：重放
	}
	c.seen[key] = now
	// 插入时顺手清理过期项（低频路径，O(n) 可接受，与 resendGate 同风格）
	for k, t := range c.seen {
		if now.Sub(t) >= replayTTL {
			delete(c.seen, k)
		}
	}
	return true
}
