// channel_rate_limit_test.go 渠道出站限速字段（A1）的 API 语义：
// 创建/更新持久化（rate_limit_per_sec）、响应 DTO 回显、负数 400、
// PUT 省略字段保留原值（P2-5 指针语义）。
package api

import (
	"fmt"
	"net/http"
	"testing"

	"catmail/internal/model"
)

// TestChannelRateLimitFieldRoundTrip 创建带 rate_limit_per_sec 的渠道 → DTO 回显；
// PUT 修改生效；PUT 省略该字段 → 保留原值；负数 → 400。
func TestChannelRateLimitFieldRoundTrip(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 创建：rate_limit_per_sec=10 持久化并在响应中回显。
	chID := createChannel(t, r, cookie,
		`{"name":"rl1","provider":"mock","from_email":"noreply@example.com","rate_limit_per_sec":10}`)
	var ch model.Channel
	if err := db.First(&ch, uint(chID)).Error; err != nil {
		t.Fatalf("load channel: %v", err)
	}
	if ch.RateLimitPerSec != 10 {
		t.Fatalf("db rate_limit_per_sec = %d, want 10", ch.RateLimitPerSec)
	}

	// PUT 修改为 5：生效。
	w := doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", chID), cookie, "", `{"rate_limit_per_sec":5}`)
	mustStatus(t, w, http.StatusOK)
	if m := decode(t, w.Body.String()); m["rate_limit_per_sec"].(float64) != 5 {
		t.Fatalf("PUT 响应 rate_limit_per_sec = %v, want 5", m["rate_limit_per_sec"])
	}

	// PUT 省略该字段（仅改名称）：限速保留原值，不被清零。
	w = doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", chID), cookie, "", `{"name":"rl1-renamed"}`)
	mustStatus(t, w, http.StatusOK)
	var ch2 model.Channel
	if err := db.First(&ch2, uint(chID)).Error; err != nil {
		t.Fatalf("reload channel: %v", err)
	}
	if ch2.RateLimitPerSec != 5 || ch2.Name != "rl1-renamed" {
		t.Fatalf("省略字段后 rate_limit_per_sec=%d name=%q, want 5/rl1-renamed", ch2.RateLimitPerSec, ch2.Name)
	}

	// PUT 负数 → 400 invalid_request，值不变。
	w = doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", chID), cookie, "", `{"rate_limit_per_sec":-1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT -1: status = %d, want 400 body=%s", w.Code, w.Body.String())
	}
	var ch3 model.Channel
	db.First(&ch3, uint(chID))
	if ch3.RateLimitPerSec != 5 {
		t.Fatalf("400 后 rate_limit_per_sec = %d, want 5（校验失败不落库）", ch3.RateLimitPerSec)
	}
}

// TestChannelRateLimitDefaultZero 创建时省略 rate_limit_per_sec → 0（不限），
// 列表响应回显 0；存量库语义（AutoMigrate 补列旧行取默认 0 = 不限速）一致。
func TestChannelRateLimitDefaultZero(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	createChannel(t, r, cookie,
		`{"name":"rl0","provider":"mock","from_email":"noreply@example.com"}`)
	w := doReq(r, "GET", "/api/channels", cookie, "", "")
	mustStatus(t, w, http.StatusOK)
	items, _ := decode(t, w.Body.String())["items"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		if m["name"] == "rl0" && m["rate_limit_per_sec"].(float64) != 0 {
			t.Fatalf("默认 rate_limit_per_sec = %v, want 0（不限）", m["rate_limit_per_sec"])
		}
	}
}
