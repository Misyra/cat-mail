// api_notifications_test.go 站内通知中心四端点（O9）：列表/未读数/标记已读行为、
// 失败通知设置四字段的读写与校验、通知通道连通性测试（站内 + httptest 假 webhook）。
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"catmail/internal/model"
	"catmail/internal/notify"
)

// settingsBody 构造带通知字段的 PUT /api/settings 完整请求体（基础五字段必须合法）。
func settingsBody(notifyFields string) string {
	return fmt.Sprintf(`{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,`+
		`"consecutive_failure_threshold":5,"log_retention_days":90%s}`, notifyFields)
}

// TestNotificationsEndpoints 列表（created_at/id 倒序、分页边界、字段契约）、
// 未读数、按 id / 全部标记已读（幂等，已读不重复计数）。
func TestNotificationsEndpoints(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 5 条：created_at 递增；仅最早一条已读
	cid := uint(7)
	rows := []model.Notification{
		{Event: notify.EventSendFailed, Level: notify.LevelError, Title: "最早-已读",
			Read: true, CreatedAt: time.Now().Add(-4 * time.Hour)},
		{Event: notify.EventChannelDisabled, Level: notify.LevelError, Title: "次早-带渠道",
			ChannelID: &cid, CreatedAt: time.Now().Add(-3 * time.Hour)},
		{Event: notify.EventChannelCooldown, Level: notify.LevelWarn, Title: "中间-冷却",
			CreatedAt: time.Now().Add(-2 * time.Hour)},
		{Event: notify.EventChannelRecovered, Level: notify.LevelInfo, Title: "次新-恢复",
			CreatedAt: time.Now().Add(-1 * time.Hour)},
		{Event: notify.EventSendFailed, Level: notify.LevelError, Title: "最新-失败"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed notifications: %v", err)
	}

	// 列表第 1 页：倒序 + total + unread + 字段契约
	w := doReq(r, "GET", "/api/notifications?page=1&page_size=2", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["total"].(float64) != 5 || m["unread"].(float64) != 4 {
		t.Fatalf("total/unread: %v / %v", m["total"], m["unread"])
	}
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items: %d", len(items))
	}
	first := items[0].(map[string]any)
	second := items[1].(map[string]any)
	if first["title"] != "最新-失败" || second["title"] != "次新-恢复" {
		t.Fatalf("order: %v / %v", first["title"], second["title"])
	}
	if first["event"] != notify.EventSendFailed || first["level"] != notify.LevelError {
		t.Fatalf("item event/level: %v / %v", first["event"], first["level"])
	}
	if first["channel_id"] != nil {
		t.Fatalf("无渠道事件 channel_id = %v, want null", first["channel_id"])
	}
	if first["read"] != false || first["created_at"] == nil || first["id"] == nil {
		t.Fatalf("item fields: %v", first)
	}
	// 第 2 页续排 + channel_id 关联
	w = doReq(r, "GET", "/api/notifications?page=2&page_size=2", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	items = m["items"].([]any)
	if items[0].(map[string]any)["title"] != "中间-冷却" ||
		items[1].(map[string]any)["title"] != "次早-带渠道" {
		t.Fatalf("page 2 order: %v", m)
	}
	if got := items[1].(map[string]any)["channel_id"].(float64); got != float64(cid) {
		t.Fatalf("channel_id: %v, want %d", got, cid)
	}

	// 分页边界
	for _, q := range []string{"page=0", "page=abc", "page_size=0", "page_size=101"} {
		if code := doReq(r, "GET", "/api/notifications?"+q, cookie, "", "").Code; code != http.StatusBadRequest {
			t.Fatalf("%s: want 400 got %d", q, code)
		}
	}

	// 未读数
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/notifications/unread_count", cookie, "", ""), http.StatusOK))
	if m["count"].(float64) != 4 {
		t.Fatalf("unread count: %v", m["count"])
	}

	// 按 id 标记已读：更新数与未读数联动
	idOf := func(title string) float64 {
		m := decode(t, mustStatus(t, doReq(r, "GET", "/api/notifications?page_size=100", cookie, "", ""), http.StatusOK))
		for _, it := range m["items"].([]any) {
			item := it.(map[string]any)
			if item["title"] == title {
				return item["id"].(float64)
			}
		}
		t.Fatalf("notification %q not found", title)
		return 0
	}
	recoverID := idOf("次新-恢复")
	m = decode(t, mustStatus(t, doReq(r, "POST", "/api/notifications/read", cookie, "",
		fmt.Sprintf(`{"ids":[%g]}`, recoverID)), http.StatusOK))
	if m["ok"] != true || m["updated"].(float64) != 1 {
		t.Fatalf("mark read: %v", m)
	}
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/notifications/unread_count", cookie, "", ""), http.StatusOK))
	if m["count"].(float64) != 3 {
		t.Fatalf("unread after one read: %v", m["count"])
	}
	// 重复标记幂等：已读行不再计数
	m = decode(t, mustStatus(t, doReq(r, "POST", "/api/notifications/read", cookie, "",
		fmt.Sprintf(`{"ids":[%g]}`, recoverID)), http.StatusOK))
	if m["updated"].(float64) != 0 {
		t.Fatalf("idempotent re-read: %v", m)
	}

	// ids=null 全部已读
	m = decode(t, mustStatus(t, doReq(r, "POST", "/api/notifications/read", cookie, "", `{"ids":null}`), http.StatusOK))
	if m["ok"] != true || m["updated"].(float64) != 3 {
		t.Fatalf("mark all read: %v", m)
	}
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/notifications/unread_count", cookie, "", ""), http.StatusOK))
	if m["count"].(float64) != 0 {
		t.Fatalf("unread after read-all: %v", m["count"])
	}

	// 空数组 no-op；缺省 ids 同 null（全部已读，此时已无可读 → 0）
	m = decode(t, mustStatus(t, doReq(r, "POST", "/api/notifications/read", cookie, "", `{"ids":[]}`), http.StatusOK))
	if m["ok"] != true || m["updated"].(float64) != 0 {
		t.Fatalf("empty ids: %v", m)
	}

	// 未登录 401
	if code := doReq(r, "GET", "/api/notifications", "", "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: %d", code)
	}
}

// TestNotificationsFromEmit 集成：经 notify.Emit（默认发射器接线测试库）落库的
// 事件出现在通知列表，未读计数联动。
func TestNotificationsFromEmit(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	if err := db.Model(&model.Setting{}).Where("id = ?", 1).
		Update("notify_inapp_enabled", true).Error; err != nil {
		t.Fatalf("enable inapp: %v", err)
	}
	notify.SetDefault(notify.NewNotifier(db))
	t.Cleanup(func() { notify.SetDefault(nil) })

	cid := uint(3)
	notify.Emit(notify.EventChannelDisabled, notify.LevelError, "渠道「x」已自动禁用", "原因：测试", &cid)
	notify.Emit(notify.EventChannelCooldown, notify.LevelWarn, "渠道「x」进入冷却", "平台限流，冷却 60 秒", nil)

	m := decode(t, mustStatus(t, doReq(r, "GET", "/api/notifications", cookie, "", ""), http.StatusOK))
	if m["total"].(float64) != 2 || m["unread"].(float64) != 2 {
		t.Fatalf("total/unread: %v / %v", m["total"], m["unread"])
	}
	items := m["items"].([]any)
	if items[0].(map[string]any)["event"] != notify.EventChannelCooldown {
		t.Fatalf("newest first: %v", items[0])
	}
	if got := items[1].(map[string]any)["channel_id"].(float64); got != 3 {
		t.Fatalf("channel_id: %v", got)
	}
}

// TestNotifySettingsValidation 设置四字段：GET 回显、PUT 校验（webhook 启用时
// URL 必须 http(s):// 且非空；notify_events 仅允许五事件 token 子集）、
// 空串规范化为全事件五 token 串。
func TestNotifySettingsValidation(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// GET：四字段齐备且默认关闭
	w := doReq(r, "GET", "/api/settings", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["notify_inapp_enabled"] != false || m["notify_webhook_enabled"] != false ||
		m["notify_webhook_url"] != "" || m["notify_events"] != "" {
		t.Fatalf("defaults: %v", m)
	}

	// 非法 webhook URL（启用时）：非 http(s)、纯文本、空串 → 400
	for _, bad := range []string{"ftp://example.com/hook", "example.com/hook", ""} {
		body := settingsBody(fmt.Sprintf(`,"notify_webhook_enabled":true,"notify_webhook_url":%q`, bad))
		if code := doReq(r, "PUT", "/api/settings", cookie, "", body).Code; code != http.StatusBadRequest {
			t.Fatalf("bad webhook url %q accepted", bad)
		}
	}
	// 未启用时 URL 不校验（允许先关着占位）
	body := settingsBody(`,"notify_webhook_enabled":false,"notify_webhook_url":"notaurl"`)
	mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", body), http.StatusOK)

	// notify_events 未知 token → 400
	body = settingsBody(`,"notify_events":"channel_cooldown,bogus"`)
	if code := doReq(r, "PUT", "/api/settings", cookie, "", body).Code; code != http.StatusBadRequest {
		t.Fatalf("unknown event token accepted")
	}
	// 空 token（连续逗号）→ 400
	body = settingsBody(`,"notify_events":"channel_cooldown,,"`)
	if code := doReq(r, "PUT", "/api/settings", cookie, "", body).Code; code != http.StatusBadRequest {
		t.Fatalf("empty event token accepted")
	}

	// 合法子集（含项内空白）→ 保存并按修剪后回显
	body = settingsBody(`,"notify_events":" channel_cooldown , send_failed "`)
	m = decode(t, mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", body), http.StatusOK))
	if m["notify_events"] != "channel_cooldown,send_failed" {
		t.Fatalf("PUT echo events: %v", m["notify_events"])
	}
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/settings", cookie, "", ""), http.StatusOK))
	if m["notify_events"] != "channel_cooldown,send_failed" {
		t.Fatalf("GET events: %v", m["notify_events"])
	}

	// 空串/缺省 → 规范化为全事件 token 串（契约：空=全部；E8 新增 update_available，
	// 送达链路三件套新增 bounce_rate_exceeded）
	allEvents := "channel_disabled,channel_cooldown,channel_recovered,send_failed,update_available,bounce_rate_exceeded"
	body = settingsBody(`,"notify_events":""`)
	m = decode(t, mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", body), http.StatusOK))
	if m["notify_events"] != allEvents {
		t.Fatalf("empty events normalized: %v", m["notify_events"])
	}
	body = settingsBody("") // 缺省字段
	m = decode(t, mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", body), http.StatusOK))
	if m["notify_events"] != allEvents {
		t.Fatalf("missing events normalized: %v", m["notify_events"])
	}

	// 四字段整体回环
	body = settingsBody(`,"notify_inapp_enabled":true,"notify_webhook_enabled":true,` +
		`"notify_webhook_url":"https://hooks.example.com/xyz","notify_events":"send_failed"`)
	_ = decode(t, mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", body), http.StatusOK))
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/settings", cookie, "", ""), http.StatusOK))
	if m["notify_inapp_enabled"] != true || m["notify_webhook_enabled"] != true ||
		m["notify_webhook_url"] != "https://hooks.example.com/xyz" || m["notify_events"] != "send_failed" {
		t.Fatalf("roundtrip: %v", m)
	}
}

// newNotifyCaptureServer 起一个记录 JSON 请求体的假 webhook（可指定响应码）。
func newNotifyCaptureServer(t *testing.T, status int) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var m map[string]any
		raw, _ := io.ReadAll(req.Body)
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

// TestNotifyTestEndpoint POST /api/settings/notify/test：站内恒落一条测试通知；
// webhook 未启用视为通过；启用后实测地址（成功/失败/未配置地址三态）。
func TestNotifyTestEndpoint(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 默认设置（webhook 未启用）：inapp_ok=true 且落测试通知；webhook_ok=true、error 空
	m := decode(t, mustStatus(t, doReq(r, "POST", "/api/settings/notify/test", cookie, "", ""), http.StatusOK))
	if m["inapp_ok"] != true || m["webhook_ok"] != true || m["webhook_error"] != "" {
		t.Fatalf("default test resp: %v", m)
	}
	var row model.Notification
	if err := db.Where("event = ?", "test").First(&row).Error; err != nil {
		t.Fatalf("test notification missing: %v", err)
	}
	if row.Title != "CatMail 测试通知" || row.Level != notify.LevelInfo || row.Read {
		t.Fatalf("test notification: %+v", row)
	}

	// webhook 启用 + 假接收端 200：payload 到达且 webhook_ok=true
	srv, bodies := newNotifyCaptureServer(t, http.StatusOK)
	body := settingsBody(fmt.Sprintf(`,"notify_webhook_enabled":true,"notify_webhook_url":%q`, srv.URL))
	mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", body), http.StatusOK)
	m = decode(t, mustStatus(t, doReq(r, "POST", "/api/settings/notify/test", cookie, "", ""), http.StatusOK))
	if m["inapp_ok"] != true || m["webhook_ok"] != true || m["webhook_error"] != "" {
		t.Fatalf("webhook ok resp: %v", m)
	}
	got := bodies()
	if len(got) != 1 || got[0]["title"] != "CatMail 测试通知" {
		t.Fatalf("captured webhook: %v", got)
	}

	// webhook 启用 + 接收端 500：实测失败（TestWebhook 失败重试 1 次），error 非空
	failSrv, _ := newNotifyCaptureServer(t, http.StatusInternalServerError)
	body = settingsBody(fmt.Sprintf(`,"notify_webhook_enabled":true,"notify_webhook_url":%q`, failSrv.URL))
	mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", body), http.StatusOK)
	m = decode(t, mustStatus(t, doReq(r, "POST", "/api/settings/notify/test", cookie, "", ""), http.StatusOK))
	if m["inapp_ok"] != true || m["webhook_ok"] != false || m["webhook_error"] == "" {
		t.Fatalf("webhook fail resp: %v", m)
	}

	// 启用但未配置地址：webhook_ok=false（PUT 校验会拦这种形态，这里直改库构造）
	if err := db.Model(&model.Setting{}).Where("id = ?", 1).Updates(map[string]any{
		"notify_webhook_enabled": true, "notify_webhook_url": "",
	}).Error; err != nil {
		t.Fatalf("force setting: %v", err)
	}
	m = decode(t, mustStatus(t, doReq(r, "POST", "/api/settings/notify/test", cookie, "", ""), http.StatusOK))
	if m["webhook_ok"] != false || m["webhook_error"] == "" {
		t.Fatalf("webhook unconfigured resp: %v", m)
	}
}
