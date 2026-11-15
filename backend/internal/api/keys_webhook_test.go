// keys_webhook_test.go D1 调用方回执 API 测试：PUT /api/keys/:id 的 webhook_url /
// webhook_events 校验（http(s) 白名单、事件子集、空串清除、缺省保留）与
// POST /api/keys/:id/webhook/test handler（含经 Register 的正式路由与会话保护）。
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"catmail/internal/model"
)

// newJSONCaptureServer 起一个把请求体解析为 JSON 并记录的 httptest 服务，返回
// 服务与取快照函数（handler 与断言并发访问，加锁保护）。
func newJSONCaptureServer(t *testing.T, status int) (*httptest.Server, func() []map[string]any) {
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

// TestUpdateKeyWebhook PUT /api/keys/:id 回执配置：URL http(s) 校验、事件白名单
// 校验与规范化、空串清除、缺省保留、非法请求不产生部分写入。
func TestUpdateKeyWebhook(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 建 Key（走管理接口）
	body := mustStatus(t, doReq(r, "POST", "/api/keys", cookie, "", `{"name":"回执Key"}`), http.StatusOK)
	keyID := decode(t, body)["id"].(float64)
	path := fmt.Sprintf("/api/keys/%g", keyID)

	// 非法 URL：非 http(s) scheme / 无法解析 / 缺 host → 400
	for _, bad := range []string{"ftp://example.com/hook", "://broken", "http://", "https://"} {
		w := doReq(r, "PUT", path, cookie, "", fmt.Sprintf(`{"webhook_url":%q}`, bad))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("webhook_url=%q: status=%d body=%s, want 400", bad, w.Code, w.Body.String())
		}
	}
	// 未知事件 → 400
	w := doReq(r, "PUT", path, cookie, "", `{"webhook_events":"send_accepted,unknown"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知事件: status=%d body=%s, want 400", w.Code, w.Body.String())
	}
	// 非法请求整体拒绝：库里不应有部分写入
	var cnt int64
	db.Model(&model.ApiKey{}).Where("id = ? AND webhook_url <> ''", uint(keyID)).Count(&cnt)
	if cnt != 0 {
		t.Fatal("非法请求不应写入 webhook_url")
	}

	// 合法配置：URL + 事件（含空白容忍与去重规范化）
	body = mustStatus(t, doReq(r, "PUT", path, cookie, "",
		`{"webhook_url":"https://cb.example.com/hook","webhook_events":" send_accepted , send_failed , send_accepted "}`), http.StatusOK)
	resp := decode(t, body)
	if resp["webhook_url"] != "https://cb.example.com/hook" {
		t.Errorf("webhook_url = %v", resp["webhook_url"])
	}
	if resp["webhook_events"] != "send_accepted,send_failed" {
		t.Errorf("webhook_events = %v, want 规范化 csv", resp["webhook_events"])
	}
	// 列表接口同样回显
	items := decode(t, mustStatus(t, doReq(r, "GET", "/api/keys", cookie, "", ""), http.StatusOK))["items"].([]any)
	first := items[0].(map[string]any)
	if first["webhook_url"] != "https://cb.example.com/hook" ||
		first["webhook_events"] != "send_accepted,send_failed" {
		t.Errorf("列表回显不匹配: %v / %v", first["webhook_url"], first["webhook_events"])
	}

	// 缺省保留：只改 enabled，webhook 不动
	mustStatus(t, doReq(r, "PUT", path, cookie, "", `{"enabled":false}`), http.StatusOK)
	first = decode(t, mustStatus(t, doReq(r, "GET", "/api/keys", cookie, "", ""), http.StatusOK))["items"].([]any)[0].(map[string]any)
	if first["webhook_url"] != "https://cb.example.com/hook" {
		t.Errorf("缺省字段被清空: %v", first["webhook_url"])
	}

	// 空串清除（显式 opt-in 的反向操作）
	mustStatus(t, doReq(r, "PUT", path, cookie, "", `{"webhook_url":"","webhook_events":""}`), http.StatusOK)
	first = decode(t, mustStatus(t, doReq(r, "GET", "/api/keys", cookie, "", ""), http.StatusOK))["items"].([]any)[0].(map[string]any)
	if first["webhook_url"] != "" || first["webhook_events"] != "" {
		t.Errorf("空串未清除: %v / %v", first["webhook_url"], first["webhook_events"])
	}
}

// TestKeyWebhookTestHandler POST /api/keys/:id/webhook/test：未配置 URL → 400；
// 已配置 → 向该地址发测试回执（envelope event=test），成败经 ok 字段上报（恒 200，
// 与 testNotify 同风格）；Key 不存在 → 404。
// handler 语义直挂 gin engine 测试（便于构造裸 Server）；经 Register 的正式
// 路由接线与鉴权另见 TestKeyWebhookTestRoute。
func TestKeyWebhookTestHandler(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	srv, bodies := newJSONCaptureServer(t, http.StatusOK)

	// 直挂被测 handler（同包构造 Server；正式路由待终审波挂载到 Register）
	// 直挂被测 handler（同包构造 Server；正式路由接线由 TestKeyWebhookTestRoute 锁定）
	rr := gin.New()
	s := &Server{db: db}
	rr.POST("/api/keys/:id/webhook/test", s.testKeyWebhook)

	// 建两个 Key：一个带回执 URL，一个未配置
	body := mustStatus(t, doReq(r, "POST", "/api/keys", cookie, "", `{"name":"有回执"}`), http.StatusOK)
	withURL := decode(t, body)["id"].(float64)
	mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/keys/%g", withURL), cookie, "",
		fmt.Sprintf(`{"webhook_url":%q,"webhook_events":"send_accepted"}`, srv.URL)), http.StatusOK)
	body = mustStatus(t, doReq(r, "POST", "/api/keys", cookie, "", `{"name":"无回执"}`), http.StatusOK)
	noURL := decode(t, body)["id"].(float64)

	// 未配置 URL → 400
	w := doReq(rr, "POST", fmt.Sprintf("/api/keys/%g/webhook/test", noURL), "", "", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未配置 URL: status=%d body=%s, want 400", w.Code, w.Body.String())
	}
	// 已配置 → 推送测试回执成功
	w = doReq(rr, "POST", fmt.Sprintf("/api/keys/%g/webhook/test", withURL), "", "", "")
	if w.Code != http.StatusOK || decode(t, w.Body.String())["ok"] != true {
		t.Fatalf("测试推送: status=%d body=%s, want ok=true", w.Code, w.Body.String())
	}
	got := bodies()
	if len(got) != 1 || got[0]["event"] != "test" || got[0]["api_key_id"] != withURL {
		t.Fatalf("收到的回执 = %v, want 1 条 event=test api_key_id=%v", got, withURL)
	}
	// 目标 5xx → 恒 200 + ok=false
	failingSrv, _ := newJSONCaptureServer(t, http.StatusInternalServerError)
	mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/keys/%g", withURL), cookie, "",
		fmt.Sprintf(`{"webhook_url":%q}`, failingSrv.URL)), http.StatusOK)
	w = doReq(rr, "POST", fmt.Sprintf("/api/keys/%g/webhook/test", withURL), "", "", "")
	if w.Code != http.StatusOK || decode(t, w.Body.String())["ok"] != false {
		t.Fatalf("目标失败: status=%d body=%s, want ok=false", w.Code, w.Body.String())
	}
	// Key 不存在 → 404
	w = doReq(rr, "POST", "/api/keys/999999/webhook/test", "", "", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("Key 不存在: status=%d, want 404", w.Code)
	}
}

// TestKeyWebhookTestRoute 经 Register 的正式路由（D1 接线锁定）：端点已注册、
// 位于 /api 会话组内（未登录 401，登录后可实测成功）。
func TestKeyWebhookTestRoute(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	srv, bodies := newJSONCaptureServer(t, http.StatusOK)

	body := mustStatus(t, doReq(r, "POST", "/api/keys", cookie, "", `{"name":"路由Key"}`), http.StatusOK)
	id := decode(t, body)["id"].(float64)
	mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/keys/%g", id), cookie, "",
		fmt.Sprintf(`{"webhook_url":%q,"webhook_events":"send_accepted"}`, srv.URL)), http.StatusOK)

	// 未登录 → 401（/api 组 requireSession 拦截）
	if w := doReq(r, "POST", fmt.Sprintf("/api/keys/%g/webhook/test", id), "", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录: status=%d body=%s, want 401", w.Code, w.Body.String())
	}
	// 登录后实测成功，测试回执推送到目标
	body = mustStatus(t, doReq(r, "POST", fmt.Sprintf("/api/keys/%g/webhook/test", id), cookie, "", ""), http.StatusOK)
	if decode(t, body)["ok"] != true {
		t.Fatalf("实测推送: body=%s, want ok=true", body)
	}
	if got := bodies(); len(got) != 1 || got[0]["event"] != "test" {
		t.Fatalf("收到的回执 = %v, want 1 条 event=test", got)
	}
}
