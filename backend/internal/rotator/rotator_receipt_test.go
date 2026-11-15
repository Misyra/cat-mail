// rotator_receipt_test.go D1 调用方回执集成测试：受理成功/终态失败经 mock 适配器
// 触发，httptest webhook 逐字段断言 envelope 与 data；另覆盖事件过滤、未配置
// 零请求与参数类 400 不推。包级 receipt.Emit* 走默认发射器：用例内 SetDefault
// 接线测试库，结束解除（包级状态，串行测试安全）。
package rotator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/outbox"
	"catmail/internal/provider"
	"catmail/internal/receipt"
)

// newCaptureServer 起一个把请求体解析为 JSON 并记录的 httptest 服务（同 receipt 包测试）。
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

// addKey 创建带回执配置的 ApiKey 行（key 列唯一索引：按 id 派生保证不冲突）。
func addKey(t *testing.T, db *gorm.DB, id uint, webhookURL, events string) {
	t.Helper()
	if err := db.Create(&model.ApiKey{
		ID: id, Name: "调用方Key", Key: keyForID(id),
		Enabled: true, WebhookURL: webhookURL, WebhookEvents: events,
	}).Error; err != nil {
		t.Fatalf("create api key: %v", err)
	}
}

// keyForID 生成唯一的测试 Key 明文（api_keys.key 唯一索引）。
func keyForID(id uint) string {
	return "cm-rt" + string(rune('a'+id%26)) + "000000000000000000000000000"
}

// wireReceipt 接线默认回执发射器指向测试库，测试结束解除（回执经 outbox 落库，
// 无进程内 worker；投递由 flushOutbox 驱动，对齐生产 scheduler 路径）。
func wireReceipt(t *testing.T, db *gorm.DB) *receipt.Dispatcher {
	t.Helper()
	d := receipt.NewDispatcher(db)
	receipt.SetDefault(d)
	t.Cleanup(func() { receipt.SetDefault(nil) })
	return d
}

// flushOutbox 驱动一轮 outbox 投递后再断言（安全加固三件套，B：替代旧队列 flush）。
func flushOutbox(t *testing.T, db *gorm.DB) {
	t.Helper()
	if st := outbox.ProcessDue(db); st.Sent+st.Failed+st.Dead+st.Skipped > 0 {
		t.Logf("outbox flush: %+v", st)
	}
}

// TestReceiptAccepted 受理成功：send_accepted 回执逐字段（envelope + data 全量），
// message_id 与发送结果/SendLog 贯穿，accepted_at 为合法 RFC3339。
func TestReceiptAccepted(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "回执渠道", Provider: "fake", APIKey: "ok"})
	srv, bodies := newCaptureServer(t, http.StatusOK)
	addKey(t, db, 21, srv.URL, receipt.EventSendAccepted+","+receipt.EventSendFailed)
	wireReceipt(t, db)

	in := &Input{
		Message: &provider.Message{
			FromAddr: "noreply@example.com",
			To:       []string{"a@example.com", "b@example.com"},
			Subject:  "回执断言主题",
			Text:     "hello",
		},
		APIKeyID: 21,
		Resolver: newResolver(tr),
	}
	out, rerr := Send(context.Background(), db, in)
	if out == nil || rerr != nil {
		t.Fatalf("send: out=%v rerr=%+v", out, rerr)
	}
	flushOutbox(t, db)

	got := bodies()
	if len(got) != 1 {
		t.Fatalf("收到 %d 个回执，want 1", len(got))
	}
	b := got[0]
	if b["source"] != "catmail" || b["event"] != "send_accepted" {
		t.Errorf("source/event = %v/%v", b["source"], b["event"])
	}
	if b["api_key_id"] != float64(21) {
		t.Errorf("api_key_id = %v, want 21", b["api_key_id"])
	}
	data := b["data"].(map[string]any)
	if data["message_id"] != out.MessageID {
		t.Errorf("message_id = %v, want %s", data["message_id"], out.MessageID)
	}
	if data["provider"] != "fake" || data["channel"] != "回执渠道" {
		t.Errorf("provider/channel = %v/%v", data["provider"], data["channel"])
	}
	if data["provider_message_id"] != "fake-回执渠道" {
		t.Errorf("provider_message_id = %v", data["provider_message_id"])
	}
	toList := data["to"].([]any)
	if len(toList) != 2 || toList[0] != "a@example.com" || toList[1] != "b@example.com" {
		t.Errorf("to = %v", data["to"])
	}
	if data["subject"] != "回执断言主题" {
		t.Errorf("subject = %v", data["subject"])
	}
	if ms, ok := data["latency_ms"].(float64); !ok || ms < 0 {
		t.Errorf("latency_ms = %v", data["latency_ms"])
	}
	at, err := time.Parse(time.RFC3339, data["accepted_at"].(string))
	if err != nil {
		t.Errorf("accepted_at 非法 RFC3339: %v", err)
	} else if dd := time.Since(at); dd < -time.Minute || dd > time.Minute {
		t.Errorf("accepted_at 偏离当前时间: %v", dd)
	}
	// message_id 贯穿：落库 SendLog 也有同 uuid 的成功日志
	var lg model.SendLog
	if err := db.Where("message_id = ? AND status = ?", out.MessageID, model.SendStatusSent).
		First(&lg).Error; err != nil {
		t.Errorf("message_id 没有对应成功 SendLog: %v", err)
	}
}

// TestReceiptFailed502 候选穷尽：send_failed 回执（message_id 贯穿失败 SendLog、
// error、tried=1、failed_at RFC3339）。
func TestReceiptFailed502(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 1, 60, 5) // threshold=5：单次失败不禁用
	tr := &fakeTransport{name: "fake", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		return nil, &provider.ProviderError{Code: "boom", Message: "simulated failure", Retryable: true}
	}}
	addChannel(t, db, &model.Channel{Name: "失败渠道", Provider: "fake"})
	srv, bodies := newCaptureServer(t, http.StatusOK)
	addKey(t, db, 22, srv.URL, receipt.EventSendFailed)
	wireReceipt(t, db)

	in := &Input{
		Message: &provider.Message{
			FromAddr: "noreply@example.com",
			To:       []string{"a@example.com"},
			Subject:  "失败回执主题",
			Text:     "hello",
		},
		APIKeyID: 22,
		Resolver: newResolver(tr),
	}
	out, rerr := Send(context.Background(), db, in)
	if out != nil || rerr == nil || rerr.HTTPStatus != 502 {
		t.Fatalf("send: out=%v rerr=%+v", out, rerr)
	}
	flushOutbox(t, db)

	got := bodies()
	if len(got) != 1 {
		t.Fatalf("收到 %d 个回执，want 1", len(got))
	}
	b := got[0]
	if b["event"] != "send_failed" || b["api_key_id"] != float64(22) {
		t.Errorf("event/api_key_id = %v/%v", b["event"], b["api_key_id"])
	}
	data := b["data"].(map[string]any)
	if !strings.Contains(data["error"].(string), "simulated failure") {
		t.Errorf("error = %v", data["error"])
	}
	if data["tried"] != float64(1) {
		t.Errorf("tried = %v, want 1", data["tried"])
	}
	if _, err := time.Parse(time.RFC3339, data["failed_at"].(string)); err != nil {
		t.Errorf("failed_at 非法 RFC3339: %v", err)
	}
	var lg model.SendLog
	if err := db.Where("message_id = ? AND status = ?", data["message_id"], model.SendStatusFailed).
		First(&lg).Error; err != nil {
		t.Errorf("message_id %v 没有对应失败 SendLog: %v", data["message_id"], err)
	}
}

// TestReceiptFailed429Quota 额度竞态终态：全部候选输在预留环节 → 429，
// send_failed 回执 tried=0。用 BEFORE UPDATE 触发器让额度预留必败（同 notify 用法）。
func TestReceiptFailed429Quota(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	addChannel(t, db, &model.Channel{Name: "满额渠道", Provider: "fake"})
	if err := db.Exec(`CREATE TRIGGER block_quota_before_update BEFORE UPDATE ON channels
		WHEN NEW.used_day > OLD.used_day BEGIN SELECT RAISE(ABORT, 'blocked'); END`).Error; err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	srv, bodies := newCaptureServer(t, http.StatusOK)
	addKey(t, db, 23, srv.URL, receipt.EventSendFailed)
	wireReceipt(t, db)

	in := &Input{
		Message: &provider.Message{
			FromAddr: "noreply@example.com",
			To:       []string{"a@example.com"},
			Subject:  "s",
			Text:     "t",
		},
		APIKeyID: 23,
		Resolver: newResolver(&fakeTransport{name: "fake"}),
	}
	out, rerr := Send(context.Background(), db, in)
	if out != nil || rerr == nil || rerr.HTTPStatus != 429 {
		t.Fatalf("send: out=%v rerr=%+v", out, rerr)
	}
	flushOutbox(t, db)

	got := bodies()
	if len(got) != 1 {
		t.Fatalf("收到 %d 个回执，want 1", len(got))
	}
	data := got[0]["data"].(map[string]any)
	if data["tried"] != float64(0) {
		t.Errorf("tried = %v, want 0", data["tried"])
	}
	if !strings.Contains(data["error"].(string), "额度已耗尽") {
		t.Errorf("error = %v", data["error"])
	}
}

// TestReceiptFilterAndOptOut 组合：事件过滤（只 opt-in accepted 时 failed 零推送）、
// 无 URL 零请求、未 opt-in 事件零请求、参数类 400 不推。
func TestReceiptFilterAndOptOut(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	srv, bodies := newCaptureServer(t, http.StatusOK)

	// ① 只 opt-in send_accepted：受理 → 1 条；失败 → 仍 1 条（failed 被过滤）
	addChannel(t, db, &model.Channel{Name: "成功渠道", Provider: "fake", APIKey: "ok"})
	addKey(t, db, 24, srv.URL, receipt.EventSendAccepted)
	wireReceipt(t, db)
	in := &Input{
		Message: &provider.Message{
			FromAddr: "noreply@example.com", To: []string{"a@example.com"},
			Subject: "s", Text: "t",
		},
		APIKeyID: 24, Resolver: newResolver(&fakeTransport{name: "fake"}),
	}
	if out, rerr := Send(context.Background(), db, in); out == nil || rerr != nil {
		t.Fatalf("send#1: out=%v rerr=%+v", out, rerr)
	}
	// ② 参数类 400（指定渠道不存在）：不推
	inBad := &Input{Message: in.Message, APIKeyID: 24, ChannelName: "不存在的渠道", Resolver: newResolver()}
	if out, rerr := Send(context.Background(), db, inBad); out != nil || rerr == nil || rerr.HTTPStatus != 400 {
		t.Fatalf("send#2: out=%v rerr=%+v", out, rerr)
	}
	flushOutbox(t, db)
	if got := len(bodies()); got != 1 {
		t.Fatalf("① ②后收到 %d 个回执，want 1（accepted 推送、400 不推）", got)
	}

	// ③ 同 key 改为只 opt-in send_failed：再次受理 → 零新增
	if err := db.Model(&model.ApiKey{}).Where("id = ?", 24).
		Update("webhook_events", receipt.EventSendFailed).Error; err != nil {
		t.Fatalf("update events: %v", err)
	}
	d2 := receipt.NewDispatcher(db)
	receipt.SetDefault(d2)
	if out, rerr := Send(context.Background(), db, in); out == nil || rerr != nil {
		t.Fatalf("send#3: out=%v rerr=%+v", out, rerr)
	}
	// ④ 无 URL 的 key：零请求
	addKey(t, db, 25, "", receipt.EventSendAccepted)
	inNoURL := &Input{Message: in.Message, APIKeyID: 25, Resolver: newResolver(&fakeTransport{name: "fake"})}
	if out, rerr := Send(context.Background(), db, inNoURL); out == nil || rerr != nil {
		t.Fatalf("send#4: out=%v rerr=%+v", out, rerr)
	}
	// ⑤ 未 opt-in 任何事件（URL 有、events 空）：零请求
	addKey(t, db, 26, srv.URL, "")
	inNoEvents := &Input{Message: in.Message, APIKeyID: 26, Resolver: newResolver(&fakeTransport{name: "fake"})}
	if out, rerr := Send(context.Background(), db, inNoEvents); out == nil || rerr != nil {
		t.Fatalf("send#5: out=%v rerr=%+v", out, rerr)
	}
	flushOutbox(t, db)
	if got := len(bodies()); got != 1 {
		t.Fatalf("③④⑤后收到 %d 个回执，want 仍为 1（过滤/未配置均零推送）", got)
	}
}
