// delivery_pipeline_test.go 送达链路三件套 API 集成测试：入站事件端点（token/
// Svix/Mailgun 三层鉴权与摄取落库、抑制自动入库、退信率告警）、抑制列表管理端点、
// 送达事件查询端点、渠道健康度送达侧指标、渠道 webhook token 端点、设置新字段、
// 发送入口抑制拦截（error.code=recipient_suppressed）。httptest + 临时 SQLite，零网络。
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/inbound"
	"catmail/internal/model"
	"catmail/internal/notify"
)

// doWebhook 发送带自定义头的 webhook 请求，返回响应记录器。
func doWebhook(r *gin.Engine, path, contentType string, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(string(body)))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// createChannelRow 测试库直插渠道（带 webhook token），返回渠道。
func createChannelRow(t *testing.T, db *gorm.DB, ch *model.Channel) *model.Channel {
	t.Helper()
	if err := db.Create(ch).Error; err != nil {
		t.Fatalf("create channel %q: %v", ch.Name, err)
	}
	return ch
}

// newTestHMAC 按官方 Svix 算法推导签名原始字节（独立造数，防「实现自证」）。
func newTestHMAC(secret, id string, ts time.Time, payload []byte) []byte {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		panic(err)
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.%s", id, ts.Unix(), payload)
	return mac.Sum(nil)
}

// testMailgunSig 按官方 Mailgun 算法推导签名（HMAC-SHA256(timestamp+token) hex）。
func testMailgunSig(key, ts, token string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(ts + token))
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------- 入站事件端点：token 鉴权与摄取 ----------

// TestWebhookIngestTokenAuth token 鉴权：无效 token 404 统一错误体；有效 token
// 摄取 Resend 事件（无签名，走共享 token 口径）→ 归一化落库 + SendLog 关联。
func TestWebhookIngestTokenAuth(t *testing.T) {
	r, db := newTestEnv(t)
	ch := createChannelRow(t, db, &model.Channel{Name: "resend主号", Provider: "resend", WebhookToken: "tok-123", WebhookSecret: ""})

	// 无效 token：404 不区分渠道不存在/token 错误
	w := doWebhook(r, "/webhooks/mail-events/no-such-token", "application/json", nil,
		[]byte(`{"type":"email.delivered"}`))
	body := mustStatus(t, w, http.StatusNotFound)
	if !strings.Contains(body, "invalid_request") {
		t.Errorf("404 错误体应为统一 invalid_request 形态: %s", body)
	}

	// 发送日志（供 provider_message_id 关联）
	lg := &model.SendLog{
		MessageID: "msg-uuid-1", ChannelID: ch.ID, ChannelName: ch.Name,
		Provider: ch.Provider, Status: model.SendStatusSent,
		ProviderMessageID: "e0b5-123", ToAddrs: `["a@example.com"]`,
	}
	if err := db.Create(lg).Error; err != nil {
		t.Fatalf("seed send log: %v", err)
	}

	w = doWebhook(r, "/webhooks/mail-events/tok-123", "application/json", nil,
		[]byte(`{"type":"email.delivered","created_at":"2026-11-14T09:00:00Z","data":{"email_id":"e0b5-123","to":["a@example.com"]}}`))
	mustStatus(t, w, http.StatusOK)

	var ev model.MailEvent
	if err := db.Where("channel_id = ?", ch.ID).First(&ev).Error; err != nil {
		t.Fatalf("mail_event 未落库: %v", err)
	}
	if ev.Event != model.MailEventDelivered || ev.Recipient != "a@example.com" ||
		ev.ProviderMessageID != "e0b5-123" || ev.MessageID != "msg-uuid-1" || ev.SendLogID != lg.ID {
		t.Errorf("归一化/关联不符: %+v", ev)
	}

	// 未知事件类型：raw 存档且仍 200（平台按非 2xx 重试，未知事件不能打回）
	w = doWebhook(r, "/webhooks/mail-events/tok-123", "application/json", nil,
		[]byte(`{"type":"email.opened","data":{"email_id":"e0b5-123"}}`))
	mustStatus(t, w, http.StatusOK)
	var n int64
	db.Model(&model.MailEvent{}).Where("event = ?", model.MailEventRaw).Count(&n)
	if n != 1 {
		t.Errorf("未知事件应 raw 存档, got %d", n)
	}
}

// svixTestHeaders 生成合法 Svix 签名头（独立按官方算法推导）。
func svixTestHeaders(t *testing.T, secret, id string, payload []byte) (map[string]string, string) {
	t.Helper()
	now := time.Now()
	mac := newTestHMAC(secret, id, now, payload)
	sig := "v1," + base64.StdEncoding.EncodeToString(mac)
	return map[string]string{
		"svix-id":        id,
		"svix-timestamp": fmt.Sprintf("%d", now.Unix()),
		"svix-signature": sig,
	}, id
}

// TestWebhookIngestSvixSignature Svix 分层鉴权：签名头存在 → 必须验真——
// 渠道未配密钥拒绝、验签通过摄取、篡改 body 拒绝、同 svix-id 重放拒绝。
func TestWebhookIngestSvixSignature(t *testing.T) {
	r, db := newTestEnv(t)
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	ch := createChannelRow(t, db, &model.Channel{Name: "resend签", Provider: "resend", WebhookToken: "tok-svix", WebhookSecret: secret})
	payload := []byte(`{"type":"email.delivered","data":{"email_id":"d-1","to":["a@example.com"]}}`)

	// 渠道未配置签名密钥 → 拒绝（claimed 签名不验即过 = 绕过签名）
	chNoSecret := createChannelRow(t, db, &model.Channel{Name: "resend无密钥", Provider: "resend", WebhookToken: "tok-nosecret"})
	headers, id := svixTestHeaders(t, secret, "msg-1", payload)
	w := doWebhook(r, "/webhooks/mail-events/"+chNoSecret.WebhookToken, "application/json", headers, payload)
	mustStatus(t, w, http.StatusUnauthorized)

	// 合法签名：摄取成功
	w = doWebhook(r, "/webhooks/mail-events/"+ch.WebhookToken, "application/json", headers, payload)
	mustStatus(t, w, http.StatusOK)
	var n int64
	db.Model(&model.MailEvent{}).Where("channel_id = ?", ch.ID).Count(&n)
	if n != 1 {
		t.Fatalf("合法 Svix 签名应摄取成功, got %d 条", n)
	}

	// 篡改 body（签名与报文不匹配）→ 401
	tampered := []byte(`{"type":"email.delivered","data":{"email_id":"d-1","to":["evil@example.com"]}}`)
	w = doWebhook(r, "/webhooks/mail-events/"+ch.WebhookToken, "application/json", headers, tampered)
	mustStatus(t, w, http.StatusUnauthorized)

	// 同 svix-id 重放（容差窗口内）→ 401
	w = doWebhook(r, "/webhooks/mail-events/"+ch.WebhookToken, "application/json", headers, payload)
	mustStatus(t, w, http.StatusUnauthorized)
	_ = id
}

// TestWebhookIngestMailgunSignature Mailgun 分层鉴权（legacy form 编码）：
// 签名齐备 → 验真；hard bounce 自动入抑制列表；签名错误拒绝。
func TestWebhookIngestMailgunSignature(t *testing.T) {
	r, db := newTestEnv(t)
	key := "mg-signing-key-test"
	ch := createChannelRow(t, db, &model.Channel{Name: "mg渠道", Provider: "mailgun", WebhookToken: "tok-mg", WebhookSecret: key})
	now := time.Now()
	ts := fmt.Sprintf("%d", now.Unix())
	token := "e0b5477167110d68991efc6b9f89f0a11066af27834600e123"
	sig := testMailgunSig(key, ts, token)
	form := fmt.Sprintf(
		"event=bounced&recipient=Bounced@Example.com&timestamp=%s&token=%s&signature=%s&message-headers=%s",
		ts, token, sig,
		"[["+quoteKV("Message-Id")+","+quoteKV("<mg-mid@x>")+"]]")

	// 合法签名：摄取 + hard bounce 自动抑制（默认开关开）
	w := doWebhook(r, "/webhooks/mail-events/"+ch.WebhookToken,
		"application/x-www-form-urlencoded", nil, []byte(form))
	mustStatus(t, w, http.StatusOK)
	var ev model.MailEvent
	if err := db.Where("channel_id = ?", ch.ID).First(&ev).Error; err != nil {
		t.Fatalf("mailgun 事件未落库: %v", err)
	}
	if ev.Event != model.MailEventBounced || ev.BounceType != model.BounceTypeHard ||
		ev.Recipient != "bounced@example.com" || ev.ProviderMessageID != "<mg-mid@x>" {
		t.Errorf("mailgun 归一化不符: %+v", ev)
	}
	var sp model.Suppression
	if err := db.Where("email = ?", "bounced@example.com").First(&sp).Error; err != nil {
		t.Fatalf("hard bounce 应自动入抑制列表: %v", err)
	}
	if sp.Reason != model.SuppressReasonBounce || sp.ChannelID != ch.ID {
		t.Errorf("抑制行不符: %+v", sp)
	}

	// 签名错误：401 且不落库
	w = doWebhook(r, "/webhooks/mail-events/"+ch.WebhookToken,
		"application/x-www-form-urlencoded", nil,
		[]byte(fmt.Sprintf("event=bounced&recipient=x@y.com&timestamp=%s&token=%s&signature=deadbeef", ts, token)))
	mustStatus(t, w, http.StatusUnauthorized)
}

// TestWebhookIngestAutoSuppressToggle 抑制自动入库开关：设置关闭后 complained
// 事件不再入抑制列表；开关打开时 complained → complaint 原因入库；soft bounce
// 不入抑制列表（保守口径）。
func TestWebhookIngestAutoSuppressToggle(t *testing.T) {
	r, db := newTestEnv(t)
	ch := createChannelRow(t, db, &model.Channel{Name: "c", Provider: "brevo", WebhookToken: "tok-sup"})

	ingest := func(event string) {
		body := fmt.Sprintf(`{"event":%q,"email":"x@example.com"}`, event)
		w := doWebhook(r, "/webhooks/mail-events/"+ch.WebhookToken, "application/json", nil, []byte(body))
		mustStatus(t, w, http.StatusOK)
	}

	// soft bounce：不入抑制
	ingest("soft_bounce")
	var n int64
	db.Model(&model.Suppression{}).Count(&n)
	if n != 0 {
		t.Fatalf("soft bounce 不应自动抑制, got %d 行", n)
	}
	// complained：入库（reason=complaint）
	ingest("spam")
	var sp model.Suppression
	if err := db.Where("email = ?", "x@example.com").First(&sp).Error; err != nil {
		t.Fatalf("complained 应自动入库: %v", err)
	}
	if sp.Reason != model.SuppressReasonComplaint {
		t.Errorf("reason = %q, want complaint", sp.Reason)
	}
	// 关闭开关：不再入库
	if err := db.Model(&model.Setting{}).Where("id = 1").Update("suppression_auto_enabled", false).Error; err != nil {
		t.Fatalf("toggle setting: %v", err)
	}
	db.Where("email = ?", "x@example.com").Delete(&model.Suppression{})
	ingest("spam")
	db.Model(&model.Suppression{}).Count(&n)
	if n != 0 {
		t.Errorf("开关关闭后不应自动入库, got %d 行", n)
	}
}

// TestWebhookIngestBounceRateNotify 退信率告警：阈值内不打扰；超阈值（默认 5%）
// 经 notify 站内通道广播 bounce_rate_exceeded（warn），1 小时内同渠道去重。
func TestWebhookIngestBounceRateNotify(t *testing.T) {
	r, db := newTestEnv(t)
	inbound.ResetBounceRateGateForTest()
	// 接线 notify 默认发射器（main 启动路径同款），开启站内通道
	notifier := notify.NewNotifier(db)
	notify.SetDefault(notifier)
	t.Cleanup(func() { notify.SetDefault(nil) })
	if err := db.Model(&model.Setting{}).Where("id = 1").
		Updates(map[string]any{"notify_inapp_enabled": true, "bounce_rate_threshold": 5.0}).Error; err != nil {
		t.Fatalf("settings: %v", err)
	}

	ch := createChannelRow(t, db, &model.Channel{Name: "告警渠道", Provider: "brevo", WebhookToken: "tok-warn"})
	ingest := func(event string, email string) {
		body := fmt.Sprintf(`{"event":%q,"email":%q}`, event, email)
		w := doWebhook(r, "/webhooks/mail-events/"+ch.WebhookToken, "application/json", nil, []byte(body))
		mustStatus(t, w, http.StatusOK)
	}

	// 100 delivered + 1 bounced = 0.99%：阈值内不告警
	for i := 0; i < 100; i++ {
		ingest("delivered", fmt.Sprintf("d%d@example.com", i))
	}
	ingest("hard_bounce", "edge@example.com")
	var n int64
	db.Model(&model.Notification{}).Where("event = ?", notify.EventBounceRate).Count(&n)
	if n != 0 {
		t.Fatalf("退信率 1%%（阈值 5%%）不应告警, got %d 条", n)
	}

	// 再退 4 封 → 5/106 ≈ 4.7%…继续退到超过 5%：11/111 ≈ 9.9% → 告警一条
	for i := 0; i < 10; i++ {
		ingest("hard_bounce", fmt.Sprintf("b%d@example.com", i))
	}
	db.Model(&model.Notification{}).Where("event = ?", notify.EventBounceRate).Count(&n)
	if n != 1 {
		t.Fatalf("退信率超阈值应告警一条, got %d 条", n)
	}
	var note model.Notification
	if err := db.Where("event = ?", notify.EventBounceRate).First(&note).Error; err != nil {
		t.Fatalf("load notification: %v", err)
	}
	if note.Level != notify.LevelWarn || note.ChannelID == nil || *note.ChannelID != ch.ID {
		t.Errorf("告警级别/渠道不符: %+v", note)
	}

	// 同渠道 1 小时去重：后续退信不再重复告警
	ingest("hard_bounce", "more@example.com")
	db.Model(&model.Notification{}).Where("event = ?", notify.EventBounceRate).Count(&n)
	if n != 1 {
		t.Errorf("1 小时内应去重, got %d 条", n)
	}
}

// ---------- 抑制列表管理端点 ----------

func TestSuppressionsAPI(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	ch := createChannelRow(t, db, &model.Channel{Name: "渠道A", Provider: "mock"})
	if err := db.Create(&model.Suppression{Email: "auto@example.com", Reason: model.SuppressReasonBounce, ChannelID: ch.ID, ChannelName: ch.Name}).Error; err != nil {
		t.Fatal(err)
	}

	// 手动添加
	w := doReq(r, "POST", "/api/suppressions", cookie, "",
		`{"email":"Manual@Example.com","detail":"客户要求"}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["reason"] != model.SuppressReasonManual || m["email"] != "manual@example.com" {
		t.Errorf("手动添加响应不符: %v", m)
	}
	// 重复添加 → 400
	w = doReq(r, "POST", "/api/suppressions", cookie, "", `{"email":"manual@example.com"}`)
	mustStatus(t, w, http.StatusBadRequest)
	// 非法邮箱 → 400
	w = doReq(r, "POST", "/api/suppressions", cookie, "", `{"email":"not-an-email"}`)
	mustStatus(t, w, http.StatusBadRequest)

	// 列表 + reason 筛选
	w = doReq(r, "GET", "/api/suppressions?page=1&page_size=10&reason=manual", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if items := m["items"].([]any); len(items) != 1 {
		t.Errorf("manual 筛选应命中 1 条, got %d", len(items))
	}
	// channel_id 筛选
	w = doReq(r, "GET", fmt.Sprintf("/api/suppressions?channel_id=%v", ch.ID), cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if items := m["items"].([]any); len(items) != 1 || totalOf(m) != 1 {
		t.Errorf("channel 筛选应命中自动入库行: %v", m)
	}
	// q 模糊
	w = doReq(r, "GET", "/api/suppressions?q=auto", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if totalOf(m) != 1 {
		t.Errorf("q 筛选应命中 auto@example.com: %v", m)
	}
	// 非法 reason → 400
	w = doReq(r, "GET", "/api/suppressions?reason=bogus", cookie, "", "")
	mustStatus(t, w, http.StatusBadRequest)

	// 解除（删除）
	id := int(m["items"].([]any)[0].(map[string]any)["id"].(float64))
	w = doReq(r, "DELETE", fmt.Sprintf("/api/suppressions/%d", int(id)), cookie, "", "")
	mustStatus(t, w, http.StatusOK)
	var deleted int64
	db.Model(&model.Suppression{}).Where("email = ?", "auto@example.com").Count(&deleted)
	if deleted != 0 {
		t.Errorf("解除后应删除, got %d", deleted)
	}
	// 不存在 → 404
	w = doReq(r, "DELETE", "/api/suppressions/9999", cookie, "", "")
	mustStatus(t, w, http.StatusNotFound)
	// 未登录 → 401
	w = doReq(r, "GET", "/api/suppressions", "", "", "")
	mustStatus(t, w, http.StatusUnauthorized)
}

// ---------- 送达事件查询端点 ----------

func TestMailEventsAPI(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	ch := createChannelRow(t, db, &model.Channel{Name: "渠道B", Provider: "resend"})
	rows := []model.MailEvent{
		{ChannelID: ch.ID, ChannelName: ch.Name, Provider: "resend", Event: model.MailEventDelivered, Recipient: "a@example.com", OccurredAt: time.Now()},
		{ChannelID: ch.ID, ChannelName: ch.Name, Provider: "resend", Event: model.MailEventBounced, BounceType: model.BounceTypeHard, Recipient: "b@example.com", Detail: "550", Raw: `{"type":"email.bounced"}`, OccurredAt: time.Now()},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	// 列表（倒序，不含 raw）
	w := doReq(r, "GET", "/api/mail-events?page=1&page_size=10", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if totalOf(m) != 2 {
		t.Fatalf("total = %v, want 2", m["total"])
	}
	first := m["items"].([]any)[0].(map[string]any)
	if _, hasRaw := first["raw"]; hasRaw {
		t.Error("列表项不应携带 raw 大字段")
	}
	// event 筛选
	w = doReq(r, "GET", "/api/mail-events?event=bounced", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if totalOf(m) != 1 {
		t.Errorf("event 筛选应命中 1 条: %v", m["total"])
	}
	// bounce_type 筛选
	w = doReq(r, "GET", "/api/mail-events?bounce_type=hard", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if totalOf(m) != 1 {
		t.Errorf("bounce_type 筛选应命中 1 条: %v", m["total"])
	}
	// q 模糊
	w = doReq(r, "GET", "/api/mail-events?q=a@ex", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if totalOf(m) != 1 {
		t.Errorf("q 筛选应命中 1 条: %v", m["total"])
	}
	// 非法枚举 → 400
	w = doReq(r, "GET", "/api/mail-events?event=bogus", cookie, "", "")
	mustStatus(t, w, http.StatusBadRequest)

	// 详情含 raw（JSON 透传）
	detailID := int(rows[1].ID)
	w = doReq(r, "GET", fmt.Sprintf("/api/mail-events/%d", detailID), cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	raw, ok := m["raw"].(map[string]any)
	if !ok || raw["type"] != "email.bounced" {
		t.Errorf("详情 raw 应为 JSON 对象透传: %v", m["raw"])
	}
	if _, ok := m["bounce_type"]; !ok {
		t.Error("详情应含 bounce_type 字段")
	}
	// 不存在 → 404
	w = doReq(r, "GET", "/api/mail-events/9999", cookie, "", "")
	mustStatus(t, w, http.StatusNotFound)
}

// ---------- 渠道健康度送达侧指标 ----------

func TestChannelHealthDeliveryMetrics(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	ch := createChannelRow(t, db, &model.Channel{Name: "健康渠道", Provider: "mock"})
	chNoEvents := createChannelRow(t, db, &model.Channel{Name: "无事件渠道", Provider: "mock"})
	now := time.Now()

	// 受理侧：2 sent 1 failed；送达侧：4 delivered 2 bounced(hard+soft) 1 complained
	addLog(t, db, ch.ID, model.SendStatusSent, false, "", now)
	addLog(t, db, ch.ID, model.SendStatusSent, false, "", now)
	addLog(t, db, ch.ID, model.SendStatusFailed, false, "boom", now)
	addEvent := func(event, bt string) {
		if err := db.Create(&model.MailEvent{
			ChannelID: ch.ID, ChannelName: ch.Name, Provider: "mock",
			Event: event, BounceType: bt, OccurredAt: now, CreatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		addEvent(model.MailEventDelivered, "")
	}
	addEvent(model.MailEventBounced, model.BounceTypeHard)
	addEvent(model.MailEventBounced, model.BounceTypeSoft)
	addEvent(model.MailEventComplained, "")

	h := channelHealth(t, r, cookie, float64(ch.ID))
	if h == nil {
		t.Fatal("有日志渠道 health 不应为 null")
	}
	if got := healthNum(t, h, "delivered"); got != 4 {
		t.Errorf("delivered = %v, want 4", got)
	}
	if got := healthNum(t, h, "bounced"); got != 2 {
		t.Errorf("bounced = %v, want 2", got)
	}
	if got := healthNum(t, h, "complained"); got != 1 {
		t.Errorf("complained = %v, want 1", got)
	}
	// 终态 7：delivery ≈ 57.1、bounce ≈ 28.6、complaint ≈ 14.3
	assertRate(t, h, "delivery_rate", 57.1)
	assertRate(t, h, "bounce_rate", 28.6)
	assertRate(t, h, "complaint_rate", 14.3)

	// 无日志且无事件的渠道：health 为 null（受理侧口径不变，送达侧聚合不产生空健康块）
	h2 := channelHealth(t, r, cookie, float64(chNoEvents.ID))
	if h2 != nil {
		t.Errorf("零日志且无事件渠道 health 应为 null, got %v", h2)
	}
	// 单独验证「只有 sent 事件（无终态）→ 比率 null」：
	chSentOnly := createChannelRow(t, db, &model.Channel{Name: "仅sent渠道", Provider: "mock"})
	if err := db.Create(&model.MailEvent{
		ChannelID: chSentOnly.ID, ChannelName: chSentOnly.Name, Provider: "mock",
		Event: model.MailEventSent, OccurredAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	h3 := channelHealth(t, r, cookie, float64(chSentOnly.ID))
	if h3 == nil {
		t.Fatal("有事件的渠道 health 不应为 null")
	}
	for _, key := range []string{"delivery_rate", "bounce_rate", "complaint_rate"} {
		if v, ok := h3[key]; ok && v != nil {
			t.Errorf("仅 sent 事件时 %s 应为 null, got %v", key, v)
		}
	}
}

// assertRate 断言健康块比率字段（一位小数容忍）。
func assertRate(t *testing.T, h map[string]any, key string, want float64) {
	t.Helper()
	v, ok := h[key].(float64)
	if !ok {
		t.Fatalf("health.%s = %#v, want number", key, h[key])
	}
	if v < want-0.05 || v > want+0.05 {
		t.Errorf("health.%s = %v, want ≈ %v", key, v, want)
	}
}

// ---------- 渠道 webhook token 端点 ----------

func TestChannelWebhookTokenEndpoint(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"token渠道","provider":"mock"}`)

	// 创建即生成 token（32 hex）
	w := doReq(r, "GET", "/api/channels", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	items := m["items"].([]any)
	var tok string
	for _, it := range items {
		row := it.(map[string]any)
		if row["id"].(float64) == chID {
			tok = row["webhook_token"].(string)
		}
	}
	if len(tok) != 32 {
		t.Fatalf("建渠道应自动生成 32 位 token, got %q", tok)
	}

	// 补生成端点（token 已有 → 不变）
	w = doReq(r, "POST", fmt.Sprintf("/api/channels/%v/webhook-token", chID), cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["webhook_token"] != tok {
		t.Errorf("无 rotate 请求不应换 token: %v", m["webhook_token"])
	}
	if m["webhook_url"] != "/webhooks/mail-events/"+tok {
		t.Errorf("webhook_url = %v", m["webhook_url"])
	}

	// rotate=true 强制轮换
	w = doReq(r, "POST", fmt.Sprintf("/api/channels/%v/webhook-token", chID), cookie, "",
		`{"rotate":true}`)
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["webhook_token"] == tok {
		t.Error("rotate 应更换 token")
	}
	if len(m["webhook_token"].(string)) != 32 {
		t.Errorf("新 token 应为 32 位 hex: %v", m["webhook_token"])
	}

	// 复制渠道：token 必须重新生成（不与原渠道相同）
	w = doReq(r, "POST", fmt.Sprintf("/api/channels/%v/duplicate", chID), cookie, "", "")
	mustStatus(t, w, http.StatusOK)
	var dup model.Channel
	if err := db.Where("name = ?", "token渠道-副本").First(&dup).Error; err != nil {
		t.Fatalf("副本未找到: %v", err)
	}
	var src model.Channel
	db.First(&src, uint(chID))
	if dup.WebhookToken == src.WebhookToken || dup.WebhookToken == "" {
		t.Errorf("副本 token 应独立生成: dup=%q src=%q", dup.WebhookToken, src.WebhookToken)
	}
	// 404
	w = doReq(r, "POST", "/api/channels/9999/webhook-token", cookie, "", "")
	mustStatus(t, w, http.StatusNotFound)
}

// ---------- 设置新字段 + 发送入口抑制拦截 ----------

func TestSettingsDeliveryFields(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// GET 默认值：抑制自动开、阈值 5
	w := doReq(r, "GET", "/api/settings", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["suppression_auto_enabled"] != true {
		t.Errorf("suppression_auto_enabled 默认应为 true, got %v", m["suppression_auto_enabled"])
	}
	if m["bounce_rate_threshold"] != 5.0 {
		t.Errorf("bounce_rate_threshold 默认应为 5, got %v", m["bounce_rate_threshold"])
	}

	// PUT 持久化新字段
	body := `{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,"consecutive_failure_threshold":5,` +
		`"log_retention_days":90,"notify_inapp_enabled":false,"notify_webhook_enabled":false,"notify_webhook_url":"",` +
		`"notify_events":"","log_body_enabled":true,"backup_daily_enabled":false,` +
		`"suppression_auto_enabled":false,"bounce_rate_threshold":12.5}`
	w = doReq(r, "PUT", "/api/settings", cookie, "", body)
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["bounce_rate_threshold"] != 12.5 || m["suppression_auto_enabled"] != false {
		t.Errorf("PUT 响应不符: %v", m)
	}
	w = doReq(r, "GET", "/api/settings", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["bounce_rate_threshold"] != 12.5 {
		t.Errorf("阈值应持久化, got %v", m["bounce_rate_threshold"])
	}
	// 越界 → 400
	for _, bad := range []string{`-1`, `101`} {
		b := strings.Replace(body, `"bounce_rate_threshold":12.5`, `"bounce_rate_threshold":`+bad, 1)
		w = doReq(r, "PUT", "/api/settings", cookie, "", b)
		mustStatus(t, w, http.StatusBadRequest)
	}
	// notify_events 白名单含 bounce_rate_exceeded（未知 token 拒绝，新 token 放行）
	b := strings.Replace(body, `"notify_events":""`,
		`"notify_events":"send_failed,bounce_rate_exceeded"`, 1)
	w = doReq(r, "PUT", "/api/settings", cookie, "", b)
	mustStatus(t, w, http.StatusOK)
}

func TestSendSuppressionBlockedAPI(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"拦截渠道","provider":"mock","priority":-10}`)
	key := createKey(t, r, cookie, "k1")
	if err := db.Create(&model.Suppression{Email: "bad@example.com", Reason: model.SuppressReasonBounce}).Error; err != nil {
		t.Fatal(err)
	}

	w := doReq(r, "POST", "/v1/send", "", key,
		`{"from":"sender@example.com","to":["bad@example.com"],"subject":"hi","text":"hi"}`)
	body := mustStatus(t, w, http.StatusBadRequest)
	var resp struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if resp.Error.Type != "invalid_request" || resp.Error.Code != "recipient_suppressed" {
		t.Errorf("错误体不符: %s", body)
	}
	if !strings.Contains(resp.Error.Message, "bad@example.com") {
		t.Errorf("错误信息应列出命中收件人: %s", body)
	}
	// 幂等重放路径同样拿到 code（失败响应存档重放形态一致）
	w = doReq(r, "POST", "/v1/send", "", key,
		`{"from":"sender@example.com","to":["good@example.com"],"subject":"hi","text":"hi","channel":"不存在渠道"}`)
	body = mustStatus(t, w, http.StatusBadRequest)
	if !strings.Contains(body, "指定渠道不存在") {
		t.Errorf("无抑制命中的 400 不应带 code: %s", body)
	}
	if strings.Contains(body, "recipient_suppressed") {
		t.Errorf("普通 400 不应携带 recipient_suppressed code: %s", body)
	}
}

// ---------- 测试工具 ----------

// totalOf 取响应 total 字段。
func totalOf(m map[string]any) int {
	v, _ := m["total"].(float64)
	return int(v)
}

// quoteKV form message-headers 数组项的 JSON 引号包装。
func quoteKV(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
