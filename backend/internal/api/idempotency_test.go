// idempotency_test.go E2 幂等键防重发集成测试：同 key 重放逐字节一致且不重发、
// key 作用域按 ApiKey 隔离、并发 409、僵尸 pending 5min 回收、24h TTL 懒惰清理、
// 无 key 回归（现状不变）、非法 key 400、失败响应同样存档重放。
package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// idemReq 发起带自定义请求头的 JSON 请求（doReq 不支持额外请求头；
// 幂等测试仅需要 Idempotency-Key）。
func idemReq(r *gin.Engine, method, path, bearer, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// newIdemEnv 独立测试环境 + mock 主渠道（priority=-10 保证选中）+ 一个 ApiKey，
// 返回引擎、DB、key 明文与其 ApiKeyID。
func newIdemEnv(t *testing.T, keyName string) (*gin.Engine, *gorm.DB, string, uint) {
	t.Helper()
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mc","provider":"mock","priority":-10}`)
	key := createKey(t, r, cookie, keyName)
	var ak model.ApiKey
	if err := db.Where("key = ?", key).First(&ak).Error; err != nil {
		t.Fatalf("load api key: %v", err)
	}
	return r, db, key, ak.ID
}

// idemSendCount 该 ApiKeyID+key 的 SendLog 数量（重放不得新增）。
func idemSendCount(t *testing.T, db *gorm.DB, apiKeyID uint) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.SendLog{}).Where("api_key_id = ?", apiKeyID).Count(&n).Error; err != nil {
		t.Fatalf("count send logs: %v", err)
	}
	return n
}

// ---------- 1. 同 key 重放：逐字节一致且不重发 ----------

// TestIdempotencyReplaySameResponse 同 key 二次请求返回存档响应：
// 状态码与响应体逐字节一致、附 Idempotent-Replay: true、不新增 SendLog；
// 档案落库内容（status/ResponseStatus/ResponseBody/MessageID）与响应一致。
func TestIdempotencyReplaySameResponse(t *testing.T) {
	r, db, key, apiKeyID := newIdemEnv(t, "幂等Key")
	hdr := map[string]string{"Idempotency-Key": "order-20260101-001"}

	body := sendBody("a@b.com", "c@d.com", "幂等首投")
	w1 := idemReq(r, "POST", "/v1/send", key, body, hdr)
	first := mustStatus(t, w1, http.StatusOK)
	if got := w1.Header().Get("Idempotent-Replay"); got != "" {
		t.Fatalf("first send must not carry replay header: %q", got)
	}
	firstID := decode(t, first)["id"].(string)

	// 重放：同 key 同 body 再来一次
	w2 := idemReq(r, "POST", "/v1/send", key, body, hdr)
	replay := mustStatus(t, w2, http.StatusOK)
	if replay != first {
		t.Fatalf("replay body not byte-identical:\nfirst : %q\nreplay: %q", first, replay)
	}
	if got := w2.Header().Get("Idempotent-Replay"); got != "true" {
		t.Fatalf("replay header: %q", got)
	}
	if n := idemSendCount(t, db, apiKeyID); n != 1 {
		t.Fatalf("send logs after replay: %d, want 1（重放不重发）", n)
	}

	// 档案内容与响应一致
	var rec model.IdempotencyRecord
	if err := db.Where("api_key_id = ? AND key = ?", apiKeyID, "order-20260101-001").First(&rec).Error; err != nil {
		t.Fatalf("record missing: %v", err)
	}
	if rec.Status != model.IdemStatusDone || rec.ResponseStatus != http.StatusOK || rec.ResponseBody != first {
		t.Fatalf("record archived wrong: status=%q resp_status=%d body_match=%v",
			rec.Status, rec.ResponseStatus, rec.ResponseBody == first)
	}
	if rec.MessageID != firstID {
		t.Fatalf("record message_id: %q, want %q", rec.MessageID, firstID)
	}

	// /emails 与 /v1/send 共用同一 handler 与档案：跨路径同 key 同样重放
	w3 := idemReq(r, "POST", "/emails", key, body, hdr)
	if got := mustStatus(t, w3, http.StatusOK); got != first {
		t.Fatalf("/emails replay not identical: %q", got)
	}
}

// ---------- 2. key 作用域绑定 ApiKeyID ----------

// TestIdempotencyScopePerApiKey 不同调用方（ApiKey）同 key 互不干扰：
// 各自首次真实发送，各自重放各自的存档；总发送数 = 2。
func TestIdempotencyScopePerApiKey(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mc","provider":"mock","priority":-10}`)
	key1 := createKey(t, r, cookie, "调用方A")
	key2 := createKey(t, r, cookie, "调用方B")
	var ak1, ak2 model.ApiKey
	if err := db.Where("key = ?", key1).First(&ak1).Error; err != nil {
		t.Fatalf("load key1: %v", err)
	}
	if err := db.Where("key = ?", key2).First(&ak2).Error; err != nil {
		t.Fatalf("load key2: %v", err)
	}

	hdrA := map[string]string{"Idempotency-Key": "shared-key"}
	hdrB := map[string]string{"Idempotency-Key": "shared-key"}

	firstA := mustStatus(t, idemReq(r, "POST", "/v1/send", key1, sendBody("a@b.com", "a1@c.com", "A首投"), hdrA), http.StatusOK)
	firstB := mustStatus(t, idemReq(r, "POST", "/v1/send", key2, sendBody("a@b.com", "b1@c.com", "B首投"), hdrB), http.StatusOK)
	if firstA == firstB {
		t.Fatalf("different callers must send independently (bodies identical): %q", firstA)
	}

	replayA := mustStatus(t, idemReq(r, "POST", "/v1/send", key1, sendBody("a@b.com", "a1@c.com", "A首投"), hdrA), http.StatusOK)
	replayB := mustStatus(t, idemReq(r, "POST", "/v1/send", key2, sendBody("a@b.com", "b1@c.com", "B首投"), hdrB), http.StatusOK)
	if replayA != firstA || replayB != firstB {
		t.Fatalf("per-caller replay mismatch:\nA: %q vs %q\nB: %q vs %q", replayA, firstA, replayB, firstB)
	}
	if replayA == replayB {
		t.Fatal("A 的存档不应串到 B（作用域必须按 ApiKeyID 隔离）")
	}

	if n := idemSendCount(t, db, ak1.ID); n != 1 {
		t.Fatalf("caller A send logs: %d, want 1", n)
	}
	if n := idemSendCount(t, db, ak2.ID); n != 1 {
		t.Fatalf("caller B send logs: %d, want 1", n)
	}
}

// ---------- 3. 并发 409（手动造 pending 行） ----------

// TestIdempotencyConflictPending 同 key 上次仍在进行中（pending 且新鲜）→
// 409 invalid_request 统一错误体，不执行发送、不新增档案。
func TestIdempotencyConflictPending(t *testing.T) {
	r, db, key, apiKeyID := newIdemEnv(t, "并发Key")

	db.Create(&model.IdempotencyRecord{
		ApiKeyID:  apiKeyID,
		Key:       "in-flight",
		Status:    model.IdemStatusPending,
		CreatedAt: time.Now(),
	})

	w := idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", "不该发出去"), map[string]string{"Idempotency-Key": "in-flight"})
	body := mustStatus(t, w, http.StatusConflict)
	if !strings.Contains(body, `"type":"invalid_request"`) || !strings.Contains(body, "相同的 Idempotency-Key 请求正在处理中") {
		t.Fatalf("409 body: %s", body)
	}
	if n := idemSendCount(t, db, apiKeyID); n != 0 {
		t.Fatalf("send logs after 409: %d, want 0", n)
	}
	var cnt int64
	db.Model(&model.IdempotencyRecord{}).Where("api_key_id = ? AND key = ?", apiKeyID, "in-flight").Count(&cnt)
	if cnt != 1 {
		t.Fatalf("records after 409: %d, want 1（不得重复建档）", cnt)
	}
}

// ---------- 4. 僵尸 pending 超 5min 重新认领 ----------

// TestIdempotencyStalePendingReclaimed pending 超过 5 分钟（进程崩溃残留）→
// 新请求原子认领成功并发送、随后存档置 done。
func TestIdempotencyStalePendingReclaimed(t *testing.T) {
	r, db, key, apiKeyID := newIdemEnv(t, "僵尸Key")

	db.Create(&model.IdempotencyRecord{
		ApiKeyID:  apiKeyID,
		Key:       "crashed",
		Status:    model.IdemStatusPending,
		CreatedAt: time.Now().Add(-6 * time.Minute), // 恰好越过 5min 判定期
	})

	w := idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", "接管重发"), map[string]string{"Idempotency-Key": "crashed"})
	resp := mustStatus(t, w, http.StatusOK)
	if n := idemSendCount(t, db, apiKeyID); n != 1 {
		t.Fatalf("send logs after reclaim: %d, want 1", n)
	}
	var rec model.IdempotencyRecord
	if err := db.Where("api_key_id = ? AND key = ?", apiKeyID, "crashed").First(&rec).Error; err != nil {
		t.Fatalf("record missing: %v", err)
	}
	if rec.Status != model.IdemStatusDone || rec.ResponseStatus != http.StatusOK || rec.ResponseBody != resp {
		t.Fatalf("reclaimed record not archived: status=%q resp=%d body_match=%v",
			rec.Status, rec.ResponseStatus, rec.ResponseBody == resp)
	}
}

// ---------- 5. 存档 TTL 24h 懒惰清理 ----------

// TestIdempotencyArchiveTTL 超 24h 档案被懒惰清理：同 key 视为无行重新发送并
// 重建档案；未超 24h（23h）的档案仍正常重放（TTL 边界内不清）。
func TestIdempotencyArchiveTTL(t *testing.T) {
	r, db, key, apiKeyID := newIdemEnv(t, "TTLKey")

	// 25h 前的过期档案（done）
	db.Create(&model.IdempotencyRecord{
		ApiKeyID: apiKeyID, Key: "expired", Status: model.IdemStatusDone,
		ResponseStatus: http.StatusOK, ResponseBody: `{"stale":true}`,
		MessageID: "old-msg", CreatedAt: time.Now().Add(-25 * time.Hour),
	})
	w := idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", "过期重发"), map[string]string{"Idempotency-Key": "expired"})
	resp := mustStatus(t, w, http.StatusOK)
	if resp == `{"stale":true}` || w.Header().Get("Idempotent-Replay") != "" {
		t.Fatalf("expired archive must be re-sent, got replay: %s", resp)
	}
	var cnt int64
	db.Model(&model.IdempotencyRecord{}).Where("api_key_id = ? AND key = ?", apiKeyID, "expired").Count(&cnt)
	if cnt != 1 {
		t.Fatalf("records after ttl cleanup: %d, want 1（旧行被清、新档重建）", cnt)
	}
	var rec model.IdempotencyRecord
	db.Where("api_key_id = ? AND key = ?", apiKeyID, "expired").First(&rec)
	if rec.MessageID == "old-msg" || rec.ResponseBody != resp {
		t.Fatalf("fresh archive not rebuilt: message_id=%q body_match=%v", rec.MessageID, rec.ResponseBody == resp)
	}

	// 23h 前的档案仍在 TTL 内：照常重放
	db.Create(&model.IdempotencyRecord{
		ApiKeyID: apiKeyID, Key: "fresh-done", Status: model.IdemStatusDone,
		ResponseStatus: http.StatusOK, ResponseBody: `{"archived":23h}`,
		MessageID: "m-23h", CreatedAt: time.Now().Add(-23 * time.Hour),
	})
	w = idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", "23h 重放"), map[string]string{"Idempotency-Key": "fresh-done"})
	if got := mustStatus(t, w, http.StatusOK); got != `{"archived":23h}` {
		t.Fatalf("23h archive should replay: %q", got)
	}
	if w.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("23h replay header missing")
	}
	if n := idemSendCount(t, db, apiKeyID); n != 1 {
		t.Fatalf("send logs (only the expired re-send): %d, want 1", n)
	}
}

// ---------- 6. 无 key 回归：行为与现状完全一致 ----------

// TestIdempotencyNoKeyRegression 不带 Idempotency-Key：每次都真实发送
// （响应各异）、无重放头、不落任何幂等档案（现状零变化）。
func TestIdempotencyNoKeyRegression(t *testing.T) {
	r, db, key, apiKeyID := newIdemEnv(t, "回归Key")

	w1 := idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", "无键1"), nil)
	b1 := mustStatus(t, w1, http.StatusOK)
	w2 := idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", "无键1"), nil)
	b2 := mustStatus(t, w2, http.StatusOK)
	if b1 == b2 {
		t.Fatalf("no-key sends must both go through (bodies identical): %q", b1)
	}
	for _, w := range []*httptest.ResponseRecorder{w1, w2} {
		if got := w.Header().Get("Idempotent-Replay"); got != "" {
			t.Fatalf("no-key must not carry replay header: %q", got)
		}
	}
	if n := idemSendCount(t, db, apiKeyID); n != 2 {
		t.Fatalf("send logs without key: %d, want 2", n)
	}
	var cnt int64
	db.Model(&model.IdempotencyRecord{}).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("no-key must not create idempotency records: %d", cnt)
	}
}

// ---------- 7. 非法 key 校验与去首尾空格 ----------

// TestIdempotencyKeyValidation 超 128 字符 / 含换行 / 含非 ASCII → 400；
// 首尾空格被 trim 后按有效 key 正常去重。
func TestIdempotencyKeyValidation(t *testing.T) {
	r, db, key, apiKeyID := newIdemEnv(t, "校验Key")

	for _, tc := range []struct{ name, key string }{
		{"too long", strings.Repeat("k", 129)},
		{"contains newline", "abc\ndef"},
		{"non-ascii", "键值-1"},
		{"control char", "abc\x01def"},
	} {
		w := idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", "非法键"), map[string]string{"Idempotency-Key": tc.key})
		body := mustStatus(t, w, http.StatusBadRequest)
		if !strings.Contains(body, `"type":"invalid_request"`) || !strings.Contains(body, "Idempotency-Key") {
			t.Fatalf("%s: %s", tc.name, body)
		}
	}
	if n := idemSendCount(t, db, apiKeyID); n != 0 {
		t.Fatalf("invalid keys must not send: %d logs", n)
	}

	// 首尾空格 trim：两次分别带不同空白，仍命中同一档案（重放）
	padded := " " + strings.Repeat("k", 128) + " "
	b1 := mustStatus(t, idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", "空白键"), map[string]string{"Idempotency-Key": padded}), http.StatusOK)
	b2 := mustStatus(t, idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", "空白键"), map[string]string{"Idempotency-Key": strings.Repeat("k", 128)}), http.StatusOK)
	if b1 != b2 {
		t.Fatalf("trimmed key should hit same archive:\n%q\n%q", b1, b2)
	}
	var cnt int64
	db.Model(&model.IdempotencyRecord{}).Where("api_key_id = ?", apiKeyID).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("trimmed key records: %d, want 1", cnt)
	}

	// 缺省：头缺失/纯空白 = 不启用，正常发送
	if code := idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", "缺省"), map[string]string{"Idempotency-Key": "   "}).Code; code != http.StatusOK {
		t.Fatalf("blank key should be treated as absent: %d", code)
	}
}

// ---------- 8. 失败响应同样存档重放 ----------

// TestIdempotencyFailedResponseReplay 400（参数校验失败）同样幂等：
// 首投存档错误响应，同 key 重试拿到同一错误体（不重发、无新日志）。
func TestIdempotencyFailedResponseReplay(t *testing.T) {
	r, db, key, apiKeyID := newIdemEnv(t, "失败键")

	badBody := `{"from":"a@b.com","to":["c@d.com"],"text":"缺 subject"}` // 校验必失败
	hdr := map[string]string{"Idempotency-Key": "will-400"}
	w1 := idemReq(r, "POST", "/v1/send", key, badBody, hdr)
	first := mustStatus(t, w1, http.StatusBadRequest)
	if !strings.Contains(first, `"type":"invalid_request"`) {
		t.Fatalf("first 400 body: %s", first)
	}

	w2 := idemReq(r, "POST", "/v1/send", key, badBody, hdr)
	replay := mustStatus(t, w2, http.StatusBadRequest)
	if replay != first {
		t.Fatalf("failed response replay not byte-identical:\n%q\n%q", first, replay)
	}
	if w2.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("failed replay header missing")
	}
	if n := idemSendCount(t, db, apiKeyID); n != 0 {
		t.Fatalf("failed sends must not create logs: %d", n)
	}
	var rec model.IdempotencyRecord
	if err := db.Where("api_key_id = ? AND key = ?", apiKeyID, "will-400").First(&rec).Error; err != nil {
		t.Fatalf("record missing: %v", err)
	}
	if rec.Status != model.IdemStatusDone || rec.ResponseStatus != http.StatusBadRequest || rec.ResponseBody != first {
		t.Fatalf("failed response not archived: status=%q resp_status=%d", rec.Status, rec.ResponseStatus)
	}
	if rec.MessageID != "" {
		t.Fatalf("failed record message_id should be empty: %q", rec.MessageID)
	}
}

// ---------- 9. 真实并发：同 key 并行首投恰好一个成功 ----------

// TestIdempotencyConcurrentSameKey 并行 8 个同 key 首投：全部 200（重放）或
// 409（进行中），成功响应逐字节一致，SendLog 恰好 1 条（首次成功，其余重放）。
func TestIdempotencyConcurrentSameKey(t *testing.T) {
	r, db, key, apiKeyID := newIdemEnv(t, "并发回归Key")

	const n = 8
	results := make(chan *httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		go func() {
			results <- idemReq(r, "POST", "/v1/send", key, sendBody("a@b.com", "c@d.com", fmt.Sprintf("并发-%d", i)),
				map[string]string{"Idempotency-Key": "race-1"})
		}()
	}
	var firstBody string
	okCount, conflictCount := 0, 0
	for i := 0; i < n; i++ {
		w := <-results
		switch w.Code {
		case http.StatusOK:
			okCount++
			if firstBody == "" {
				firstBody = w.Body.String()
			} else if w.Body.String() != firstBody {
				t.Fatalf("concurrent ok bodies diverge: %q vs %q", firstBody, w.Body.String())
			}
		case http.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
		}
	}
	if okCount < 1 {
		t.Fatalf("at least one request must succeed: ok=%d conflict=%d", okCount, conflictCount)
	}
	if n := idemSendCount(t, db, apiKeyID); n != 1 {
		t.Fatalf("send logs after concurrent same-key: %d, want 1（重放/409 均不重发）", n)
	}
}
