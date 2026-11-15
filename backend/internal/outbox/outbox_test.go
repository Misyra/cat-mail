// outbox_test.go 投递引擎单测（安全加固三件套，B）：退避节奏/dead 转移/重投成功/
// 签名头契约/secret 引用解析/dead 裁剪。全部经 httptest 真实收发断言。
package outbox

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/httpsig"
	"catmail/internal/model"
	"catmail/internal/store"
)

// jsonCompact 序列化为紧凑 JSON 串（与 Enqueue 的落库形态一致）。
func jsonCompact(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

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

// captureServer 记录请求头与原始体的 httptest 服务。
type captureServer struct {
	*httptest.Server
	mu      sync.Mutex
	raws    [][]byte
	headers []http.Header
	status  int
}

// newCaptureServer 起一个按固定状态码响应并记录请求的服务（status=0 时按调用
// 计数先 500 后 200，供重投成功用例）。
func newCaptureServer(t *testing.T, status int) *captureServer {
	t.Helper()
	cs := &captureServer{status: status}
	var hits int
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.raws = append(cs.raws, raw)
		cs.headers = append(cs.headers, r.Header)
		hits++
		respStatus := status
		if status == 0 { // flaky 模式：首 500 后 200
			if hits == 1 {
				respStatus = http.StatusInternalServerError
			} else {
				respStatus = http.StatusOK
			}
		}
		cs.mu.Unlock()
		w.WriteHeader(respStatus)
	}))
	t.Cleanup(cs.Server.Close)
	return cs
}

// count 请求计数。
func (cs *captureServer) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.raws)
}

// TestEnqueueAndDeliver 入队即落库 → 一轮驱动投递成功置 sent；payload 原样发送。
func TestEnqueueAndDeliver(t *testing.T) {
	db := openDB(t)
	cs := newCaptureServer(t, http.StatusOK)
	payload := map[string]any{"source": "catmail", "event": "send_failed"}

	if err := Enqueue(db, "send_failed", cs.URL, "", payload); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	st := ProcessDue(db)
	if st.Sent != 1 {
		t.Fatalf("sent = %d, want 1（stats=%+v）", st.Sent, st)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.raws) != 1 {
		t.Fatalf("request count = %d, want 1", len(cs.raws))
	}
	// payload 逐字节一致（紧凑 JSON）
	want, _ := jsonCompact(payload)
	if string(cs.raws[0]) != want {
		t.Errorf("body = %q, want %q", cs.raws[0], want)
	}
	var row model.WebhookDelivery
	if err := db.First(&row, 1).Error; err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.Status != model.OutboxStatusSent || row.LastError != "" {
		t.Errorf("成功后 status=%q last_error=%q, want sent/空", row.Status, row.LastError)
	}
}

// TestBackoffSchedule 退避节奏：1m→5m→30m→2h→8h→24h 封顶（10 次失败在 24h 档）。
func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{
		time.Minute, 5 * time.Minute, 30 * time.Minute,
		2 * time.Hour, 8 * time.Hour, 24 * time.Hour,
	}
	for i, w := range want {
		if got := BackoffDelay(i + 1); got != w {
			t.Errorf("BackoffDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
	for _, attempt := range []int{7, 10, 20} { // 封顶
		if got := BackoffDelay(attempt); got != 24*time.Hour {
			t.Errorf("BackoffDelay(%d) = %v, want 24h 封顶", attempt, got)
		}
	}
	if BackoffDelay(0) != time.Minute { // 防御：非法入参取最小档
		t.Errorf("BackoffDelay(0) = %v, want 1m", BackoffDelay(0))
	}
}

// TestRetryThenSent 首败后退避 + 重投成功：失败行 attempts=1、next_retry_at≈+1m、
// 仍 pending；把 next_retry_at 拨回过去后再次驱动 → sent。
func TestRetryThenSent(t *testing.T) {
	db := openDB(t)
	cs := newCaptureServer(t, 0) // 首败后成功

	if err := Enqueue(db, "send_failed", cs.URL, "", map[string]any{"x": 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	st := ProcessDue(db)
	if st.Failed != 1 || st.Sent != 0 {
		t.Fatalf("首轮 stats=%+v, want failed=1", st)
	}
	var row model.WebhookDelivery
	if err := db.First(&row, 1).Error; err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.Status != model.OutboxStatusPending || row.Attempts != 1 {
		t.Fatalf("失败后 status=%s attempts=%d, want pending/1", row.Status, row.Attempts)
	}
	if d := time.Until(row.NextRetryAt); d < 30*time.Second || d > 90*time.Second {
		t.Errorf("next_retry_at 距今 %v, want ≈1m", d)
	}
	if row.LastError == "" {
		t.Error("失败应记录 last_error")
	}
	// 未到期不投（退避等待）
	if st := ProcessDue(db); st.Sent+st.Failed != 0 {
		t.Errorf("退避期内不应投递（stats=%+v）", st)
	}
	// 时间拨回 → 重投成功
	if err := db.Model(&model.WebhookDelivery{}).Where("id = ?", 1).
		Update("next_retry_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("rewind: %v", err)
	}
	st = ProcessDue(db)
	if st.Sent != 1 {
		t.Fatalf("重投 stats=%+v, want sent=1", st)
	}
	if cs.count() != 2 {
		t.Errorf("请求次数 = %d, want 2（首败 + 重投成功）", cs.count())
	}
	var fresh model.WebhookDelivery
	if err := db.First(&fresh, 1).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if fresh.Status != model.OutboxStatusSent {
		t.Errorf("重投后 status = %s, want sent", fresh.Status)
	}
}

// TestDeadTransition 10 次失败转 dead：达上限后 status=dead、不再投递；
// dead 行保留供排查（PruneDead 裁剪见 TestPruneDead）。
func TestDeadTransition(t *testing.T) {
	db := openDB(t)
	cs := newCaptureServer(t, http.StatusInternalServerError)
	if err := Enqueue(db, "send_failed", cs.URL, "", map[string]any{"x": 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// 驱动 10 轮失败（每轮把 next_retry_at 拨回过去，模拟时间流逝）
	for i := 1; i <= MaxAttempts; i++ {
		if err := db.Model(&model.WebhookDelivery{}).Where("id = ?", 1).
			Update("next_retry_at", time.Now().Add(-time.Minute)).Error; err != nil {
			t.Fatalf("rewind: %v", err)
		}
		st := ProcessDue(db)
		if st.Failed != 1 {
			t.Fatalf("第 %d 轮 stats=%+v, want failed=1", i, st)
		}
		var row model.WebhookDelivery
		if err := db.First(&row, 1).Error; err != nil {
			t.Fatalf("load row: %v", err)
		}
		if i < MaxAttempts && row.Status != model.OutboxStatusPending {
			t.Fatalf("第 %d 次失败应仍 pending, got %s", i, row.Status)
		}
		if i == MaxAttempts && row.Status != model.OutboxStatusDead {
			t.Fatalf("第 %d 次失败应转 dead, got %s", i, row.Status)
		}
	}
	// dead 后不再投递
	if st := ProcessDue(db); st.Sent+st.Failed != 0 {
		t.Errorf("dead 行不应再投（stats=%+v）", st)
	}
	if cs.count() != MaxAttempts {
		t.Errorf("请求次数 = %d, want %d", cs.count(), MaxAttempts)
	}
}

// TestPermanentFailureDead 永久故障（URL 为空/引用失效）直接转 dead 不退避。
func TestPermanentFailureDead(t *testing.T) {
	db := openDB(t)
	// URL 为空
	if err := Enqueue(db, "evt", "", "", map[string]any{"x": 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	st := ProcessDue(db)
	if st.Dead != 1 {
		t.Fatalf("空 URL stats=%+v, want dead=1", st)
	}
	// secret 引用失效（ApiKey 不存在）
	if err := Enqueue(db, "evt", "https://example.com/hook", "api_key:999", map[string]any{"x": 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	st = ProcessDue(db)
	if st.Dead != 1 {
		t.Fatalf("引用失效 stats=%+v, want dead=1（本轮）", st)
	}
	var deadCount int64
	if err := db.Model(&model.WebhookDelivery{}).Where("status = ?", model.OutboxStatusDead).Count(&deadCount).Error; err != nil {
		t.Fatalf("count dead: %v", err)
	}
	if deadCount != 2 {
		t.Fatalf("dead 总数 = %d, want 2（空 URL + 引用失效）", deadCount)
	}
	var row model.WebhookDelivery
	if err := db.Where("secret_ref = ?", "api_key:999").First(&row).Error; err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.Status != model.OutboxStatusDead {
		t.Errorf("引用失效应转 dead, got %s", row.Status)
	}
}

// TestSignatureHeaderContract 签名头契约：secretRef 解析出 secret → 头存在且可校验
// （HMAC 覆盖原始请求体）；secret 为空 → 不发头。
func TestSignatureHeaderContract(t *testing.T) {
	db := openDB(t)
	const secret = "whsec-outbox-test"
	// 通知引用：Setting.notify_webhook_secret
	if err := db.Model(&model.Setting{}).Where("id = ?", 1).
		Update("notify_webhook_secret", secret).Error; err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	cs := newCaptureServer(t, http.StatusOK)
	if err := Enqueue(db, "evt_signed", cs.URL, SecretRefNotify, map[string]any{"x": 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// 空引用：无签名头
	if err := Enqueue(db, "evt_plain", cs.URL, "", map[string]any{"x": 2}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ProcessDue(db)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.headers) != 2 {
		t.Fatalf("request count = %d, want 2", len(cs.headers))
	}
	if cs.headers[0].Get(httpsig.HeaderName) == "" {
		t.Error("secret 引用有效应携带签名头")
	}
	if err := httpsig.Verify(secret, cs.raws[0], cs.headers[0].Get(httpsig.HeaderName), time.Now()); err != nil {
		t.Errorf("签名校验失败: %v", err)
	}
	if cs.headers[1].Get(httpsig.HeaderName) != "" {
		t.Error("空 secret 引用不应携带签名头")
	}
}

// TestResolveSecret 引用解析：通知引用 → Setting 值；api_key 引用 → Key 值；
// 未知引用/失效引用报错；空引用返回空。
func TestResolveSecret(t *testing.T) {
	db := openDB(t)
	if err := db.Model(&model.Setting{}).Where("id = ?", 1).
		Update("notify_webhook_secret", "s-notif").Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Create(&model.ApiKey{ID: 7, Name: "k", Key: "cm-resolve-test", Enabled: true, WebhookSecret: "s-key"}).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}
	cases := []struct {
		ref  string
		want string
		fail bool
	}{
		{"", "", false},
		{SecretRefNotify, "s-notif", false},
		{SecretRefAPIKeyPrefix + "7", "s-key", false},
		{SecretRefAPIKeyPrefix + "999", "", true}, // Key 不存在
		{"unknown:ref", "", true},                 // 引用非法
	}
	for _, tc := range cases {
		got, err := ResolveSecret(db, tc.ref)
		if tc.fail && err == nil {
			t.Errorf("ResolveSecret(%q) 应失败", tc.ref)
		}
		if !tc.fail && (err != nil || got != tc.want) {
			t.Errorf("ResolveSecret(%q) = %q %v, want %q", tc.ref, got, err, tc.want)
		}
	}
}

// TestPruneDead dead 行保留最近 keep 条，超出删旧；pending/sent 不受影响。
func TestPruneDead(t *testing.T) {
	db := openDB(t)
	for i := 0; i < 5; i++ {
		if err := db.Create(&model.WebhookDelivery{
			EventType: "e", Payload: "{}", URL: "https://example.com",
			Status: model.OutboxStatusDead, NextRetryAt: time.Now(),
		}).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := db.Create(&model.WebhookDelivery{
		EventType: "e", Payload: "{}", URL: "https://example.com",
		Status: model.OutboxStatusPending, NextRetryAt: time.Now(),
	}).Error; err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	deleted, err := PruneDead(db, 3)
	if err != nil {
		t.Fatalf("PruneDead: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	var deadCount, pendingCount int64
	if err := db.Model(&model.WebhookDelivery{}).Where("status = ?", model.OutboxStatusDead).Count(&deadCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.WebhookDelivery{}).Where("status = ?", model.OutboxStatusPending).Count(&pendingCount).Error; err != nil {
		t.Fatal(err)
	}
	if deadCount != 3 || pendingCount != 1 {
		t.Fatalf("dead=%d pending=%d, want 3/1（pending 不受裁剪影响）", deadCount, pendingCount)
	}
	if deleted, _ := PruneDead(db, 10); deleted != 0 {
		t.Errorf("不足 keep 条时 deleted = %d, want 0", deleted)
	}
	if deleted, _ := PruneDead(db, 0); deleted != 0 {
		t.Errorf("keep=0 时 deleted = %d, want 0", deleted)
	}
}

// TestEnqueueNilDB 未接线（db=nil）静默 no-op 不 panic（对齐 notify/receipt 未接线语义）。
func TestEnqueueNilDB(t *testing.T) {
	if err := Enqueue(nil, "e", "https://example.com", "", map[string]any{}); err != nil {
		t.Errorf("nil db Enqueue 应 no-op, got %v", err)
	}
	ProcessDue(nil) // 不应 panic
}
