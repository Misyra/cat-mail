// receipt_test.go 调用方回执发射器单测：事件白名单、outbox 入队投递、事件过滤
// 与非法 URL 容忍；建库方式同 notify_test.go。
package receipt

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/httpsig"
	"catmail/internal/model"
	"catmail/internal/outbox"
	"catmail/internal/store"
)

// openDB 在临时目录打开 DB 并完成首启初始化；测试结束自动清理。
func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "catmail.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := store.EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}
	return db
}

// addKey 创建带回执配置的 ApiKey 行（key 列唯一索引：按 id 派生保证不冲突）。
func addKey(t *testing.T, db *gorm.DB, id uint, webhookURL, events string) {
	t.Helper()
	addKeyWithSecret(t, db, id, webhookURL, events, "")
}

// addKeyWithSecret 同 addKey，另指定回执签名密钥（空串=不签名口径）。
func addKeyWithSecret(t *testing.T, db *gorm.DB, id uint, webhookURL, events, secret string) {
	t.Helper()
	if err := db.Create(&model.ApiKey{
		ID: id, Name: "回执Key", Key: fmt.Sprintf("cm-test%032d", id),
		Enabled: true, WebhookURL: webhookURL, WebhookEvents: events, WebhookSecret: secret,
	}).Error; err != nil {
		t.Fatalf("create api key: %v", err)
	}
}

// newCaptureServer 起一个把请求体解析为 JSON 并记录的 httptest 服务（同 notify 测试）。
func newCaptureServer(t *testing.T, status int) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return bodies
	}
}

// flushOutbox 驱动一轮 outbox 投递（对齐生产 scheduler 路径，替代旧队列 flush）。
func flushOutbox(t *testing.T, db *gorm.DB) outbox.Stats {
	t.Helper()
	return outbox.ProcessDue(db)
}

// pendingCount 统计 pending 任务条数。
func pendingCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var c int64
	if err := db.Model(&model.WebhookDelivery{}).Where("status = ?", model.OutboxStatusPending).Count(&c).Error; err != nil {
		t.Fatalf("count pending: %v", err)
	}
	return c
}

// TestValidEvent 事件名合法性判定。
func TestValidEvent(t *testing.T) {
	if !ValidEvent(EventSendAccepted) || !ValidEvent(EventSendFailed) {
		t.Error("内置事件应合法")
	}
	if ValidEvent("test") || ValidEvent("") || ValidEvent("unknown") {
		t.Error("外部传入事件名不应通过 ValidEvent")
	}
}

// TestKeyEventEnabled 调用方事件白名单：与 notify 不同，空串=不推送（逐项 opt-in）。
func TestKeyEventEnabled(t *testing.T) {
	cases := []struct {
		csv, event string
		want       bool
	}{
		{"", EventSendAccepted, false},                                     // 空=未 opt-in 任何事件
		{"   ", EventSendAccepted, false},                                  // 仅空白同空串
		{EventSendAccepted, EventSendAccepted, true},                       // 单事件命中
		{EventSendFailed + "," + EventSendAccepted, EventSendFailed, true}, // 子集命中
		{EventSendAccepted, EventSendFailed, false},                        // 未列入
		{" " + EventSendFailed + " ", EventSendFailed, true},               // 项内空白容忍
		{",", EventSendAccepted, false},                                    // 空项不误匹配
	}
	for _, tc := range cases {
		if got := eventEnabled(tc.csv, tc.event); got != tc.want {
			t.Errorf("eventEnabled(%q, %q) = %v, want %v", tc.csv, tc.event, got, tc.want)
		}
	}
}

// TestEmitAcceptedDelivery send_accepted 全链路：outbox 入队 → 驱动投递 →
// envelope 逐字段 + accepted_at RFC3339。
func TestEmitAcceptedDelivery(t *testing.T) {
	db := openDB(t)
	srv, bodies := newCaptureServer(t, http.StatusOK)
	addKey(t, db, 5, srv.URL, EventSendAccepted+","+EventSendFailed)

	d := NewDispatcher(db)
	SetDefault(d)
	t.Cleanup(func() { SetDefault(nil) })
	EmitAccepted(5, AcceptedData{
		MessageID: "msg-1", Provider: "resend", Channel: "主渠道",
		ProviderMessageID: "prov-9", To: []string{"a@example.com", "b@example.com"},
		Subject: "你好", LatencyMs: 42,
	})
	if got := pendingCount(t, db); got != 1 {
		t.Fatalf("Emit 后 pending 任务 = %d, want 1（入队即持久化）", got)
	}
	flushOutbox(t, db)

	got := bodies()
	if len(got) != 1 {
		t.Fatalf("收到 %d 个回执，want 1", len(got))
	}
	b := got[0]
	if b["source"] != Source || b["event"] != EventSendAccepted {
		t.Errorf("source/event = %v/%v, want %s/%s", b["source"], b["event"], Source, EventSendAccepted)
	}
	if b["api_key_id"] != float64(5) {
		t.Errorf("api_key_id = %v, want 5", b["api_key_id"])
	}
	data, ok := b["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 非对象: %v", b["data"])
	}
	if data["message_id"] != "msg-1" || data["provider"] != "resend" ||
		data["channel"] != "主渠道" || data["provider_message_id"] != "prov-9" ||
		data["subject"] != "你好" || data["latency_ms"] != float64(42) {
		t.Errorf("data 字段不匹配: %v", data)
	}
	to, ok := data["to"].([]any)
	if !ok || len(to) != 2 || to[0] != "a@example.com" || to[1] != "b@example.com" {
		t.Errorf("to = %v", data["to"])
	}
	at, err := time.Parse(time.RFC3339, data["accepted_at"].(string))
	if err != nil {
		t.Errorf("accepted_at 非法 RFC3339: %v", err)
	} else if dd := time.Since(at); dd < -time.Minute || dd > time.Minute {
		t.Errorf("accepted_at 偏离当前时间: %v", dd)
	}
	// 投递成功后任务置 sent
	var row model.WebhookDelivery
	if err := db.Where("event_type = ?", EventSendAccepted).First(&row).Error; err != nil {
		t.Fatalf("load delivery: %v", err)
	}
	if row.Status != model.OutboxStatusSent {
		t.Errorf("投递成功后 status = %s, want sent", row.Status)
	}
	if row.SecretRef != outbox.SecretRefAPIKeyPrefix+"5" {
		t.Errorf("secret_ref = %q, want api_key:5", row.SecretRef)
	}
}

// TestEmitEventFilter 事件过滤：白名单外事件零请求；URL 与事件缺一不可。
func TestEmitEventFilter(t *testing.T) {
	db := openDB(t)
	srv, bodies := newCaptureServer(t, http.StatusOK)
	// 只 opt-in send_failed
	addKey(t, db, 6, srv.URL, EventSendFailed)

	d := NewDispatcher(db)
	SetDefault(d)
	t.Cleanup(func() { SetDefault(nil) })
	EmitAccepted(6, AcceptedData{MessageID: "m"}) // 白名单外：不推
	flushOutbox(t, db)
	if n := len(bodies()); n != 0 {
		t.Fatalf("白名单外事件不应推送，收到 %d 个", n)
	}
	EmitFailed(6, FailedData{MessageID: "m", Error: "x", Tried: 1})
	flushOutbox(t, db)
	if n := len(bodies()); n != 1 {
		t.Fatalf("白名单内事件应推送，收到 %d 个", n)
	}

	// URL 为空：零请求
	db2 := openDB(t)
	addKey(t, db2, 7, "", EventSendAccepted)
	d2 := NewDispatcher(db2)
	SetDefault(d2)
	EmitAccepted(7, AcceptedData{MessageID: "m"})
	// 事件为空（未 opt-in）：零请求
	addKey(t, db2, 8, srv.URL, "")
	EmitAccepted(8, AcceptedData{MessageID: "m"})
	flushOutbox(t, db2)
}

// TestEmitPersistedAndFlushed 入队即持久化：连续 10 次发射落 10 条任务，一轮驱动
// 全部送达（旧内存队列的 flush 语义由 outbox 持久化 + scheduler 驱动替代）。
func TestEmitPersistedAndFlushed(t *testing.T) {
	db := openDB(t)
	srv, bodies := newCaptureServer(t, http.StatusOK)
	addKey(t, db, 9, srv.URL, EventSendAccepted)

	d := NewDispatcher(db)
	SetDefault(d)
	t.Cleanup(func() { SetDefault(nil) })
	for i := 0; i < 10; i++ {
		EmitAccepted(9, AcceptedData{MessageID: "m"})
	}
	if got := pendingCount(t, db); got != 10 {
		t.Fatalf("pending = %d, want 10", got)
	}
	flushOutbox(t, db)
	if got := len(bodies()); got != 10 {
		t.Fatalf("收到 %d 个回执，want 10", got)
	}
}

// TestNilDefaultEmitNoop 未 SetDefault 时包级 Emit* 与 nil 接收者均 no-op，不 panic。
func TestNilDefaultEmitNoop(t *testing.T) {
	SetDefault(nil)
	EmitAccepted(1, AcceptedData{MessageID: "m"}) // 不应 panic
	EmitFailed(1, FailedData{MessageID: "m"})
	var d *Dispatcher
	d.emit(1, EventSendAccepted, AcceptedData{})
}

// TestEmitUnknownKeyTolerated Key 行不存在/非法事件名：静默丢弃不 panic。
func TestEmitUnknownKeyTolerated(t *testing.T) {
	db := openDB(t)
	d := NewDispatcher(db)
	SetDefault(d)
	t.Cleanup(func() { SetDefault(nil) })
	EmitAccepted(999, AcceptedData{MessageID: "m"}) // Key 不存在
	d.emit(1, "not_an_event", AcceptedData{})       // 非法事件名（防御，包级入口已保证合法）
	if got := pendingCount(t, db); got != 0 {
		t.Errorf("无效发射不应产生 outbox 任务，当前 %d 条", got)
	}
}

// TestEmitIllegalURLTolerated 非法 URL 容忍：直写库的畸形配置不 panic、不阻塞；
// 入队后由 outbox 投递判定为永久故障转 dead（重试无意义）。
func TestEmitIllegalURLTolerated(t *testing.T) {
	db := openDB(t)
	addKey(t, db, 11, "://bad url with spaces", EventSendAccepted)

	d := NewDispatcher(db)
	SetDefault(d)
	t.Cleanup(func() { SetDefault(nil) })
	done := make(chan struct{})
	go func() {
		EmitAccepted(11, AcceptedData{MessageID: "m"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("emit 在非法 URL 下阻塞")
	}
	st := flushOutbox(t, db)
	if st.Dead == 0 {
		t.Errorf("非法 URL 应转 dead（stats=%+v）", st)
	}
	var row model.WebhookDelivery
	if err := db.Where("status = ?", model.OutboxStatusDead).First(&row).Error; err != nil {
		t.Fatalf("load dead row: %v", err)
	}
	if !strings.Contains(row.LastError, "永久故障") {
		t.Errorf("dead 行 last_error 应标注永久故障: %q", row.LastError)
	}
}

// TestReceiptSignatureHeader 回执签名契约（安全加固三件套，B）：Key 无 secret →
// 不发头；有 secret → X-CatMail-Signature 可按 httpsig 契约校验（HMAC 覆盖原始
// 请求体）。
func TestReceiptSignatureHeader(t *testing.T) {
	db := openDB(t)
	const secret = "whsec-key-test"
	var mu sync.Mutex
	var raws [][]byte
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		raws = append(raws, raw)
		headers = append(headers, r.Header)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	addKeyWithSecret(t, db, 12, srv.URL, EventSendAccepted, "")
	addKeyWithSecret(t, db, 13, srv.URL, EventSendAccepted, secret)
	d := NewDispatcher(db)
	SetDefault(d)
	t.Cleanup(func() { SetDefault(nil) })

	EmitAccepted(12, AcceptedData{MessageID: "m"}) // 无 secret：不签名
	EmitAccepted(13, AcceptedData{MessageID: "m"}) // 有 secret：签名
	flushOutbox(t, db)

	mu.Lock()
	defer mu.Unlock()
	if len(raws) != 2 {
		t.Fatalf("request count = %d, want 2", len(raws))
	}
	if headers[0].Get(httpsig.HeaderName) != "" {
		t.Error("secret 为空的 Key 不应携带签名头")
	}
	sig := headers[1].Get(httpsig.HeaderName)
	if sig == "" {
		t.Fatal("secret 非空的 Key 应携带签名头")
	}
	if err := httpsig.Verify(secret, raws[1], sig, time.Now()); err != nil {
		t.Errorf("签名校验失败: %v", err)
	}
}

// TestTestWebhook 导出的 TestWebhook：2xx 返回 nil 且 event=test；非 2xx 返回错误；
// 非法 URL 报错。
func TestTestWebhook(t *testing.T) {
	srv, bodies := newCaptureServer(t, http.StatusOK)
	if err := TestWebhook(3, srv.URL, ""); err != nil {
		t.Fatalf("TestWebhook(200): %v", err)
	}
	got := bodies()
	if len(got) != 1 {
		t.Fatalf("request count = %d, want 1", len(got))
	}
	if got[0]["source"] != Source || got[0]["event"] != "test" || got[0]["api_key_id"] != float64(3) {
		t.Errorf("payload = %v, want source=catmail event=test api_key_id=3", got[0])
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(fail.Close)
	if err := TestWebhook(3, fail.URL, ""); err == nil {
		t.Error("TestWebhook(403) 应返回错误")
	}
	if err := TestWebhook(3, "://bad url", ""); err == nil {
		t.Error("非法 URL 应返回错误")
	}
}
