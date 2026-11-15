// stats_time_dialect_test.go D2 时间口径测试：channelHealthMap 的 MAX(CASE…)
// 表达式列按方言分支接收——SQLite 读回字符串（glebarez 落库格式，parseDriverTimeString
// 解析）、PostgreSQL 直接按 *time.Time 接收（分支见 handler_stats.go channelHealthMap，
// live PG 回归设 CATMAIL_TEST_PG_DSN 后由 rotator/store 的 live 用例覆盖）。
// 本文件锁死 SQLite 侧解析行为不被方言改造波及。
package api

import (
	"strings"
	"testing"
	"time"
)

// TestParseDriverTimeString SQLite 方言的字符串时间解析（D2 改造后行为不变）：
// 带时区偏移的格式（glebarez 实际落库取首项）断言瞬时相等；无偏移的兜底格式按
// UTC 解析，仅断言可解析与墙钟往返一致（既有行为，非本次 D2 改动面）。
func TestParseDriverTimeString(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 30, 0, 123456789, time.FixedZone("CST", 8*3600))

	for _, layout := range driverTimeFormats {
		s := now.Format(layout)
		got := parseDriverTimeString(&s)
		if got == nil {
			t.Errorf("格式 %q 字符串应可解析: %q", layout, s)
			continue
		}
		if strings.Contains(layout, "-07:00") {
			if !got.Equal(now) {
				t.Errorf("格式 %q 解析结果不等: got %v want %v", layout, got, now)
			}
		} else if got.Format(layout) != s {
			t.Errorf("格式 %q 墙钟往返不一致: got %q want %q", layout, got.Format(layout), s)
		}
	}

	// 边界：nil 指针/空串 → nil（零日志渠道最近时间为 null）
	if got := parseDriverTimeString(nil); got != nil {
		t.Errorf("nil 输入应返回 nil, got %v", got)
	}
	empty := ""
	if got := parseDriverTimeString(&empty); got != nil {
		t.Errorf("空串输入应返回 nil, got %v", got)
	}
}
