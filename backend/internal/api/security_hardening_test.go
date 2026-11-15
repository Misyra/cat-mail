// security_hardening_test.go 安全加固专项测试：/api 端点鉴权覆盖、登录限流用户名维度、
// 登录响应一致性（防用户名枚举）、弱口令提示、输入上限、排序白名单、CSV 注入防护、
// mass assignment 与删除账号后的会话失效（httptest 走真实中间件链）。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"catmail/internal/model"
)

// doReqXFF 携带伪造 X-Forwarded-For 的 JSON 请求（模拟攻击者轮换客户端 IP）。
func doReqXFF(r *gin.Engine, method, path, xff, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---------- 鉴权覆盖：除 health/login 外全部 /api 端点必须过会话中间件 ----------

// TestAuthzAllEndpointsRequireSession 对照 server.go 路由表逐条核验：
// 未携带会话 Cookie 访问任意 /api 端点一律 401 统一错误体（不得 500/404/200）。
func TestAuthzAllEndpointsRequireSession(t *testing.T) {
	r, _ := newTestEnv(t)
	cases := []struct {
		method, path string
	}{
		{"POST", "/api/auth/logout"},
		{"GET", "/api/auth/me"},
		{"PUT", "/api/auth/password"},
		{"GET", "/api/users"},
		{"POST", "/api/users"},
		{"PUT", "/api/users/1/password"},
		{"DELETE", "/api/users/1"},
		{"GET", "/api/channels"},
		{"POST", "/api/channels"},
		{"PUT", "/api/channels/1"},
		{"DELETE", "/api/channels/1"},
		{"POST", "/api/channels/1/status"},
		{"POST", "/api/channels/1/test"},
		{"GET", "/api/logs"},
		{"GET", "/api/logs/export"},
		{"GET", "/api/logs/1"},
		{"GET", "/api/keys"},
		{"POST", "/api/keys"},
		{"PUT", "/api/keys/1"},
		{"DELETE", "/api/keys/1"},
		{"GET", "/api/stats/overview"},
		{"GET", "/api/stats/daily"},
		{"GET", "/api/settings"},
		{"PUT", "/api/settings"},
	}
	for _, tc := range cases {
		w := doReq(r, tc.method, tc.path, "", "", `{}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: want 401 got %d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `"type":"unauthorized"`) {
			t.Fatalf("%s %s: unauthorized body: %s", tc.method, tc.path, body)
		}
	}
}

// ---------- 登录限流：用户名维度独立计数 ----------

// TestLoginLockoutUsernameDimension 组合键（用户名+IP）可被伪造 X-Forwarded-For 轮换
// IP 绕过：同一用户名从不同 IP 各失败一次，累计达阈值后仍须 429（用户名维度锁定）。
func TestLoginLockoutUsernameDimension(t *testing.T) {
	r, _ := newTestEnv(t)
	for i := 0; i < loginFailLimit; i++ {
		xff := fmt.Sprintf("203.0.113.%d", i+1) // 每次不同伪造成源 IP
		w := doReqXFF(r, "POST", "/api/auth/login", xff, `{"username":"admin","password":"wrong"}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401 got %d", i+1, w.Code)
		}
	}
	// 阈值已满：换任意 IP、即使密码正确也 429
	w := doReqXFF(r, "POST", "/api/auth/login", "198.51.100.1", `{"username":"admin","password":"admin123"}`)
	body := mustStatus(t, w, http.StatusTooManyRequests)
	if !strings.Contains(body, "尝试次数过多") {
		t.Fatalf("username-dimension lockout: %s", body)
	}
}

// TestLoginNoUserEnumeration 「用户不存在」与「密码错误」响应体完全一致（无枚举差异）。
func TestLoginNoUserEnumeration(t *testing.T) {
	r, _ := newTestEnv(t)
	w1 := doReq(r, "POST", "/api/auth/login", "", "", `{"username":"ghost","password":"whatever1"}`)
	w2 := doReq(r, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"wrongpass"}`)
	if w1.Code != http.StatusUnauthorized || w2.Code != http.StatusUnauthorized {
		t.Fatalf("want 401/401 got %d/%d", w1.Code, w2.Code)
	}
	if w1.Body.String() != w2.Body.String() {
		t.Fatalf("response mismatch (enumeration): %s vs %s", w1.Body.String(), w2.Body.String())
	}
}

// ---------- 弱口令提示（GET /api/settings 附加 weak_admin_password） ----------

// TestSettingsWeakAdminPasswordHint 首启默认口令（admin123）→ 提示为 true；
// 改密为强口令并重新登录后 → 提示消失。
func TestSettingsWeakAdminPasswordHint(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	m := decode(t, mustStatus(t, doReq(r, "GET", "/api/settings", cookie, "", ""), http.StatusOK))
	if m["weak_admin_password"] != true {
		t.Fatalf("seeded weak password should be flagged: %v", m["weak_admin_password"])
	}

	// 改密成功 → 旧会话失效 → 重新登录
	mustStatus(t, doReq(r, "PUT", "/api/auth/password", cookie, "",
		`{"old_password":"admin123","new_password":"Str0ng-Passw0rd!"}`), http.StatusOK)
	cookie = login(t, r, "admin", "Str0ng-Passw0rd!")
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/settings", cookie, "", ""), http.StatusOK))
	if m["weak_admin_password"] != false {
		t.Fatalf("strong password should not be flagged: %v", m["weak_admin_password"])
	}
}

// ---------- 输入上限与排序白名单 ----------

// TestLogsKeywordLengthCap q 关键字超长（>128 字符）→ 400；列表与导出共用同一校验。
func TestLogsKeywordLengthCap(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	long := strings.Repeat("a", 129)
	ok := strings.Repeat("a", 128)
	if w := doReq(r, "GET", "/api/logs?q="+long, cookie, "", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("list long q: want 400 got %d", w.Code)
	}
	if w := doReq(r, "GET", "/api/logs?q="+ok, cookie, "", ""); w.Code != http.StatusOK {
		t.Fatalf("list 128-char q: want 200 got %d", w.Code)
	}
	if w := doReq(r, "GET", "/api/logs/export?q="+long, cookie, "", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("export long q: want 400 got %d", w.Code)
	}
}

// TestLogsSortAndStatusWhitelist sort/status 为枚举白名单，注入形态一律 400
// （ORDER BY 子句只能来自 logOrderClause 的固定分支）。
func TestLogsSortAndStatusWhitelist(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	for _, tc := range []struct{ key, val string }{
		{"sort", "created_at;DROP TABLE users"},
		{"sort", "latency_ms--"},
		{"status", "1 OR 1=1"},
		{"channel_id", "1 OR 1=1"},
	} {
		target := "/api/logs?" + url.Values{tc.key: []string{tc.val}}.Encode()
		if w := doReq(r, "GET", target, cookie, "", ""); w.Code != http.StatusBadRequest {
			t.Fatalf("%s=%s: want 400 got %d body=%s", tc.key, tc.val, w.Code, w.Body.String())
		}
	}
}

// TestChangePasswordOverlongOld 旧密码超长（>72，超出 bcrypt 输入上限）→ 400「旧密码错误」。
func TestChangePasswordOverlongOld(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	body := fmt.Sprintf(`{"old_password":%q,"new_password":"newpass1"}`, strings.Repeat("x", 300))
	w := doReq(r, "PUT", "/api/auth/password", cookie, "", body)
	resp := mustStatus(t, w, http.StatusBadRequest)
	if !strings.Contains(resp, "旧密码错误") {
		t.Fatalf("overlong old password: %s", resp)
	}
}

// ---------- CSV 公式注入防护：所有动态单元格 ----------

// TestCSVInjectionSanitize 落库内容以 = + - @ 开头时，导出 CSV 的对应单元格
// 必须前置单引号（覆盖 is_test/status/error/channel_name/subject 等）。
func TestCSVInjectionSanitize(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	if err := db.Create(&model.SendLog{
		MessageID:   "msg-csv-1",
		ChannelName: "@evil-channel",
		Provider:    "=1+1",
		Status:      model.SendStatusFailed,
		FromAddr:    "+86sender@example.com",
		ToAddrs:     `["-cmd@example.com"]`,
		Subject:     "=HYPERLINK(evil.example)",
		Error:       "@denied",
		IsTest:      true,
	}).Error; err != nil {
		t.Fatalf("seed log: %v", err)
	}

	body := mustStatus(t, doReq(r, "GET", "/api/logs/export", cookie, "", ""), http.StatusOK)
	for _, want := range []string{
		"'@evil-channel", "'=1+1", "'+86sender@example.com",
		"'-cmd@example.com", "'=HYPERLINK(evil.example)", "'@denied",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("csv cell not sanitized, want %q in:\n%s", want, body)
		}
	}
}

// ---------- mass assignment：字段白名单绑定 ----------

// TestMassAssignmentIgnored 用户/设置接口只绑定白名单字段：请求携带 id、
// password_hash、session_secret 等一律被忽略。
func TestMassAssignmentIgnored(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	var st model.Setting
	if err := db.First(&st, 1).Error; err != nil {
		t.Fatalf("setting row: %v", err)
	}
	origSecret := st.SessionSecret

	// 建号携带越权字段：id 不得被指定，password_hash 不得被写入
	m := decode(t, mustStatus(t, doReq(r, "POST", "/api/users", cookie, "",
		`{"username":"eve","password":"secret1","id":999,"password_hash":"$2a$10$evil","created_at":"2000-01-01T00:00:00Z"}`),
		http.StatusOK))
	if m["id"].(float64) == 999 {
		t.Fatalf("client-controlled id accepted: %v", m["id"])
	}
	if _, ok := m["password_hash"]; ok {
		t.Fatalf("create response leaks password_hash: %v", m)
	}

	// 设置携带 session_secret：settingsDTO 白名单外字段被忽略
	mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "",
		`{"rotation":"round_robin","max_attempts":5,"cooldown_seconds":60,"consecutive_failure_threshold":5,"log_retention_days":30,"session_secret":"evil-secret"}`),
		http.StatusOK)
	if err := db.First(&st, 1).Error; err != nil {
		t.Fatalf("setting reload: %v", err)
	}
	if st.SessionSecret != origSecret {
		t.Fatalf("session secret overwritten via mass assignment")
	}
}

// ---------- 删除账号路径的会话失效 ----------

// TestDeletedUserSessionInvalidated 删除账号后被删用户的既有会话立即失效
// （requireSession 存在性校验路径；密码重置路径的纪元失效由 TestSessionEpochInvalidation 覆盖）。
func TestDeletedUserSessionInvalidated(t *testing.T) {
	r, _ := newTestEnv(t)
	adminCookie := login(t, r, "admin", "admin123")
	m := decode(t, mustStatus(t, doReq(r, "POST", "/api/users", adminCookie, "",
		`{"username":"bob","password":"secret1"}`), http.StatusOK))
	bobID := fmt.Sprintf("%g", m["id"].(float64))

	bobCookie := login(t, r, "bob", "secret1")
	mustStatus(t, doReq(r, "GET", "/api/auth/me", bobCookie, "", ""), http.StatusOK)

	mustStatus(t, doReq(r, "DELETE", "/api/users/"+bobID, adminCookie, "", ""), http.StatusOK)
	if code := doReq(r, "GET", "/api/auth/me", bobCookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("deleted user session should be rejected: %d", code)
	}
}

// ---------- Cookie 属性（HttpOnly/SameSite/Path/MaxAge） ----------

// TestSessionCookieHardening 登录签发的会话 Cookie 必须 HttpOnly、SameSite=Lax、
// Path=/、Max-Age=7 天；登出清除时属性一致且 Max-Age<0。
func TestSessionCookieHardening(t *testing.T) {
	r, _ := newTestEnv(t)
	w := doReq(r, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"admin123"}`)
	mustStatus(t, w, http.StatusOK)
	var sc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			sc = c
		}
	}
	if sc == nil {
		t.Fatalf("session cookie missing")
	}
	if !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || sc.Path != "/" {
		t.Fatalf("cookie flags: httponly=%v samesite=%v path=%q", sc.HttpOnly, sc.SameSite, sc.Path)
	}
	if sc.MaxAge != int(sessionTTL.Seconds()) {
		t.Fatalf("cookie max-age: %d", sc.MaxAge)
	}

	cookie := sc.Value
	w = doReq(r, "POST", "/api/auth/logout", cookie, "", "")
	mustStatus(t, w, http.StatusOK)
	var cc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			cc = c
		}
	}
	if cc == nil || cc.MaxAge >= 0 || cc.Value != "" {
		t.Fatalf("logout should clear cookie: %+v", cc)
	}
}

// ---------- 响应不泄露敏感字段（Key 列表只回掩码） ----------

// TestKeysNeverReturnPlaintext 列表/更新响应只含 masked_key，不含明文 key 字段。
func TestKeysNeverReturnPlaintext(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 明文仅创建响应返回一次
	w := doReq(r, "POST", "/api/keys", cookie, "", `{"name":"plain-check"}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	plain, _ := m["key"].(string)
	if plain == "" || !strings.HasPrefix(plain, "cm-") {
		t.Fatalf("create should return plaintext once: %v", m)
	}

	w = doReq(r, "GET", "/api/keys", cookie, "", "")
	body := mustStatus(t, w, http.StatusOK)
	if strings.Contains(body, plain) {
		t.Fatalf("list leaks plaintext key: %s", body)
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items: %v", resp.Items)
	}
	if _, ok := resp.Items[0]["key"]; ok {
		t.Fatalf("list response contains raw key field")
	}
	if mk, _ := resp.Items[0]["masked_key"].(string); !strings.Contains(mk, "…") {
		t.Fatalf("masked_key missing: %v", resp.Items[0]["masked_key"])
	}
}
