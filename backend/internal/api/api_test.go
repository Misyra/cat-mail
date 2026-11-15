// api_test.go API 层集成测试：httptest + 临时 SQLite，零外部网络
// （mock 适配器本地模拟发送）。覆盖 SPEC §5 全部端点的关键路径与 §8 校验规则。
package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/store"
)

// ---------- 测试脚手架 ----------

// newTestEnv 每个用例独立的临时 SQLite + gin 引擎（种子含 admin/示例渠道）。
func newTestEnv(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	Register(r, db)
	// 与 cmd 层 registerStatic 一致的 404 分支（测试环境无静态资源；
	// P2-12：/emails/* 子路径同样返回统一 JSON 404，不落 fallback）
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if p == "/api" || strings.HasPrefix(p, "/api/") ||
			p == "/v1" || strings.HasPrefix(p, "/v1/") ||
			p == "/emails" || strings.HasPrefix(p, "/emails/") {
			NotFound(c)
			return
		}
		c.String(http.StatusNotFound, "not found")
	})
	return r, db
}

// doReq 发起 JSON 请求；cookie 与 bearer 可为空。
func doReq(r *gin.Engine, method, path, cookie, bearer, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// login 登录并返回会话 cookie 值。
func login(t *testing.T, r *gin.Engine, username, password string) string {
	t.Helper()
	w := doReq(r, "POST", "/api/auth/login", "", "",
		fmt.Sprintf(`{"username":%q,"password":%q}`, username, password))
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: status=%d body=%s", username, w.Code, w.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("login: session cookie missing")
	}
	return cookie.Value
}

// mustStatus 断言 HTTP 状态码并返回响应体。
func mustStatus(t *testing.T, w *httptest.ResponseRecorder, code int) string {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status: want %d got %d body=%s", code, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// decode 解析响应 JSON。
func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode json: %v body=%s", err, body)
	}
	return m
}

// createChannel 创建一个 mock 渠道，返回其 id。
func createChannel(t *testing.T, r *gin.Engine, cookie, body string) float64 {
	t.Helper()
	w := doReq(r, "POST", "/api/channels", cookie, "", body)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	return m["id"].(float64)
}

// createKey 创建 API Key，返回明文。
func createKey(t *testing.T, r *gin.Engine, cookie, name string) string {
	t.Helper()
	w := doReq(r, "POST", "/api/keys", cookie, "",
		fmt.Sprintf(`{"name":%q}`, name))
	m := decode(t, mustStatus(t, w, http.StatusOK))
	return m["key"].(string)
}

// sendBody 构造最小发送请求体。
func sendBody(from, to, subject string) string {
	return fmt.Sprintf(`{"from":%q,"to":[%q],"subject":%q,"text":"hi"}`, from, to, subject)
}

// ---------- 1. 登录 + 全链路发送 ----------

// TestSendFullFlow 登录 → 建 mock 渠道 → 建 key → /v1/send 成功，
// 断言响应字段与 SendLog 落库；并验证 /emails 与 /v1/send 同构。
func TestSendFullFlow(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 建 mock 渠道（priority 更小，确保轮换选中它而非种子示例渠道）
	chID := createChannel(t, r, cookie, `{"name":"mock主渠道","provider":"mock","priority":-10}`)
	key := createKey(t, r, cookie, "测试Key")
	if !regexp.MustCompile(`^cm-[0-9a-f]{32}$`).MatchString(key) {
		t.Fatalf("key format: %s", key)
	}

	// /v1/send 全链路
	w := doReq(r, "POST", "/v1/send", "", key, sendBody("CatMail <noreply@example.com>", "a@example.com", "Hello"))
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["accepted"] != true {
		t.Fatalf("accepted: %v", m["accepted"])
	}
	if m["channel"] != "mock主渠道" || m["provider"] != "mock" {
		t.Fatalf("channel/provider: %v / %v", m["channel"], m["provider"])
	}
	msgID, _ := m["id"].(string)
	if msgID == "" || !strings.HasPrefix(m["provider_message_id"].(string), "mock-") {
		t.Fatalf("id/provider_message_id: %v / %v", m["id"], m["provider_message_id"])
	}
	_ = chID

	// 日志落库断言
	var log model.SendLog
	if err := db.Where("message_id = ?", msgID).First(&log).Error; err != nil {
		t.Fatalf("send log missing: %v", err)
	}
	if log.Status != model.SendStatusSent || log.IsTest {
		t.Fatalf("log status/is_test: %d / %v", log.Status, log.IsTest)
	}
	if log.Subject != "Hello" || log.FromAddr != "noreply@example.com" {
		t.Fatalf("log subject/from: %s / %s", log.Subject, log.FromAddr)
	}
	if !strings.Contains(log.ToAddrs, "a@example.com") || log.ProviderMessageID == "" {
		t.Fatalf("log to_addrs/pmid: %s / %s", log.ToAddrs, log.ProviderMessageID)
	}
	if log.ChannelID == 0 || log.ApiKeyID == 0 {
		t.Fatalf("log channel_id/api_key_id: %d / %d", log.ChannelID, log.ApiKeyID)
	}

	// /emails 同构（同样的鉴权/请求/响应）
	w = doReq(r, "POST", "/emails", "", key, sendBody("noreply@example.com", "b@example.com", "ViaEmails"))
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["accepted"] != true || m["provider"] != "mock" {
		t.Fatalf("/emails resp: %v", m)
	}

	// 渠道额度记账：used_day 应为 2（受理 +1/次）
	var ch model.Channel
	if err := db.Where("name = ?", "mock主渠道").First(&ch).Error; err != nil {
		t.Fatalf("channel: %v", err)
	}
	if ch.UsedDay != 2 || ch.UsedMonth != 2 {
		t.Fatalf("channel used_day/used_month: %d / %d", ch.UsedDay, ch.UsedMonth)
	}
}

// ---------- 2. 未登录 401 ----------

// TestUnauthorized 未登录/坏 cookie/坏 bearer 一律 401 统一错误体；
// 且响应体只含错误对象——middleware 未 Abort 时 gin 会继续执行 handler
// 并把真实数据拼在 401 错误体之后（回归保护，见 R1 安全复核）。
func TestUnauthorized(t *testing.T) {
	r, _ := newTestEnv(t)

	w := doReq(r, "GET", "/api/channels", "", "", "")
	body := mustStatus(t, w, http.StatusUnauthorized)
	if !strings.Contains(body, `"type":"unauthorized"`) {
		t.Fatalf("error body: %s", body)
	}
	if strings.Contains(body, `"items"`) || strings.Contains(body, `"name"`) {
		t.Fatalf("401 leaks handler data after error body: %s", body)
	}
	// 整个响应体必须是单个 JSON 对象（无拼接的第二段数据）
	if !json.Valid([]byte(body)) {
		t.Fatalf("401 body is not a single JSON object: %s", body)
	}

	// 各敏感端点逐一确认：错误体之外无任何数据
	for _, path := range []string{"/api/users", "/api/keys", "/api/logs", "/api/settings", "/api/stats/overview"} {
		w := doReq(r, "GET", path, "", "", "")
		b := mustStatus(t, w, http.StatusUnauthorized)
		if !json.Valid([]byte(b)) || strings.Contains(b, `"items"`) {
			t.Fatalf("%s leaks data on 401: %s", path, b)
		}
	}

	if code := doReq(r, "GET", "/api/users", "tampered|token", "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie: %d", code)
	}
	if code := doReq(r, "POST", "/v1/send", "", "cm-invalid", `{}`).Code; code != http.StatusUnauthorized {
		t.Fatalf("bad bearer: %d", code)
	}
	if code := doReq(r, "POST", "/v1/send", "", "", `{}`).Code; code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", code)
	}
}

// TestSessionCookieFlags 会话 Cookie 属性：HttpOnly + SameSite=Lax + Path=/（SPEC §8）。
func TestSessionCookieFlags(t *testing.T) {
	r, _ := newTestEnv(t)
	w := doReq(r, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"admin123"}`)
	mustStatus(t, w, http.StatusOK)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("cookie missing")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("cookie flags: httponly=%v samesite=%v path=%s", cookie.HttpOnly, cookie.SameSite, cookie.Path)
	}
	if cookie.MaxAge != int((7 * 24 * time.Hour).Seconds()) {
		t.Fatalf("cookie max-age: %d", cookie.MaxAge)
	}
}

// ---------- 3. 账号规则 ----------

// TestUsersRules 重名/用户名与密码校验/删自己/上限。
func TestUsersRules(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 正常创建
	mustStatus(t, doReq(r, "POST", "/api/users", cookie, "", `{"username":"bob","password":"secret1"}`), http.StatusOK)
	// 重名
	if code := doReq(r, "POST", "/api/users", cookie, "", `{"username":"bob","password":"secret1"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("duplicate username: %d", code)
	}
	// 用户名过短 / 密码过短
	if code := doReq(r, "POST", "/api/users", cookie, "", `{"username":"a","password":"secret1"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("short username: %d", code)
	}
	if code := doReq(r, "POST", "/api/users", cookie, "", `{"username":"carol","password":"12345"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("short password: %d", code)
	}
	// 列表不含密码字段
	w := doReq(r, "GET", "/api/users", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if raw, _ := json.Marshal(m["items"]); strings.Contains(string(raw), "password") {
		t.Fatalf("users list leaks password: %s", raw)
	}
	// 删自己 → 400（admin id=1）
	if code := doReq(r, "DELETE", "/api/users/1", cookie, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("delete self: %d", code)
	}

	// 上限 10：已有 admin+bob=2，再建 8 个到 10，第 11 个 → 400
	for i := 2; i <= 9; i++ {
		body := fmt.Sprintf(`{"username":"user%d","password":"secret1"}`, i)
		mustStatus(t, doReq(r, "POST", "/api/users", cookie, "", body), http.StatusOK)
	}
	if code := doReq(r, "POST", "/api/users", cookie, "", `{"username":"overflow","password":"secret1"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("user limit: %d", code)
	}

	// 重置密码 + 新密码可登录；O10 按用户纪元：重置仅失效目标用户（bob）的会话，
	// 操作者（admin）既有会话不受影响
	mustStatus(t, doReq(r, "PUT", "/api/users/2/password", cookie, "", `{"new_password":"newpass1"}`), http.StatusOK)
	if code := doReq(r, "GET", "/api/auth/me", cookie, "", "").Code; code != http.StatusOK {
		t.Fatalf("admin session should survive resetting another user: %d", code)
	}
	login(t, r, "bob", "newpass1")
	cookie = login(t, r, "admin", "admin123")
	if code := doReq(r, "PUT", "/api/users/999/password", cookie, "", `{"new_password":"newpass1"}`).Code; code != http.StatusNotFound {
		t.Fatalf("reset missing user: %d", code)
	}

	// 删除其他账号 → 200；最后只剩 admin，再删任何账号都会命中保护规则
	mustStatus(t, doReq(r, "DELETE", "/api/users/2", cookie, "", ""), http.StatusOK)
	if code := doReq(r, "DELETE", "/api/users/1", cookie, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("delete last account: %d", code)
	}
}

// TestPasswordChange 本人改密：旧密码校验、新密码生效；
// O10 按用户纪元：改密后本人旧纪元会话立即失效（当前会话由响应重签的新 cookie
// 保持登录，重签行为由 session_epoch_test.go 覆盖）。
func TestPasswordChange(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	if code := doReq(r, "PUT", "/api/auth/password", cookie, "", `{"old_password":"wrong","new_password":"newpass1"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("wrong old password: %d", code)
	}
	mustStatus(t, doReq(r, "PUT", "/api/auth/password", cookie, "", `{"old_password":"admin123","new_password":"newpass1"}`), http.StatusOK)

	// 旧密码失效、新密码可登录
	if code := doReq(r, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"admin123"}`).Code; code != http.StatusUnauthorized {
		t.Fatalf("old password still valid: %d", code)
	}
	// 改密后本人旧纪元会话失效（O10；当前会话由响应重签的新 cookie 保持）
	if code := doReq(r, "GET", "/api/auth/me", cookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("old session after password change: %d", code)
	}
	cookie = login(t, r, "admin", "newpass1")

	// me / logout（用新会话）；me 返回 {id, username}（admin 为首个账号 id=1）
	w := doReq(r, "GET", "/api/auth/me", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["username"] != "admin" {
		t.Fatalf("me username: %v", m["username"])
	}
	if id, ok := m["id"].(float64); !ok || id != 1 {
		t.Fatalf("me id: %v", m["id"])
	}
	w = doReq(r, "POST", "/api/auth/logout", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["ok"] != true {
		t.Fatalf("logout: %v", m)
	}
}

// ---------- 4. 渠道：脱敏 / 部分更新 / 启停 / 测试 ----------

// TestChannelMasking 响应脱敏：api_key/api_secret/smtp_pass 前4后4掩码、
// proxy_url 凭据打码；PUT 传空保留原值。
func TestChannelMasking(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	body := `{"name":"resend主号","provider":"resend","api_key":"sk-1234567890abcdef","api_secret":"sec-9876543210abcdefgh",` +
		`"proxy_url":"http://user:secret@proxy.example.com:8080"}`
	chID := createChannel(t, r, cookie, body)

	smtpBody := `{"name":"smtp备线","provider":"smtp","smtp_host":"smtp.example.com","smtp_port":587,` +
		`"smtp_user":"mailer","smtp_pass":"password123","smtp_tls":"starttls","from_email":"noreply@example.com"}`
	createChannel(t, r, cookie, smtpBody)

	w := doReq(r, "GET", "/api/channels", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	items := m["items"].([]any)
	var resendCh, smtpCh map[string]any
	for _, it := range items {
		item := it.(map[string]any)
		switch item["name"] {
		case "resend主号":
			resendCh = item
		case "smtp备线":
			smtpCh = item
		}
	}
	if resendCh == nil || smtpCh == nil {
		t.Fatalf("channels missing: %v", m)
	}
	if resendCh["api_key"] != "sk-1…cdef" || resendCh["api_secret"] != "sec-…efgh" {
		t.Fatalf("mask: %v / %v", resendCh["api_key"], resendCh["api_secret"])
	}
	if smtpCh["smtp_pass"] != "pass…d123" {
		t.Fatalf("smtp_pass mask: %v", smtpCh["smtp_pass"])
	}
	if got := resendCh["proxy_url"].(string); got != "http://***@proxy.example.com:8080" {
		t.Fatalf("proxy mask: %s", got)
	}
	// 完整明文不得出现在任何响应里
	for _, secret := range []string{"sk-1234567890", "sec-9876543210", "password123", "user:secret"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("response leaks secret %s: %s", secret, w.Body.String())
		}
	}
	// 生效额度：resend 平台默认 100/3000
	if resendCh["effective_daily_limit"].(float64) != 100 || resendCh["effective_monthly_limit"].(float64) != 3000 {
		t.Fatalf("effective limits: %v / %v", resendCh["effective_daily_limit"], resendCh["effective_monthly_limit"])
	}

	// PUT：api_key 传空 = 保留原值；其余字段更新 + 状态复位
	w = doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", chID), cookie, "", `{"api_key":"","priority":5,"daily_limit_override":50}`)
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["api_key"] != "sk-1…cdef" {
		t.Fatalf("empty api_key should keep original: %v", m["api_key"])
	}
	if m["priority"].(float64) != 5 || m["daily_limit_override"].(float64) != 50 || m["effective_daily_limit"].(float64) != 50 {
		t.Fatalf("update fields: %v", m)
	}
	// 更换 api_key → 掩码随之更新
	w = doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", chID), cookie, "", `{"api_key":"sk-newvalue12345678"}`)
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["api_key"] != "sk-n…5678" {
		t.Fatalf("new api_key mask: %v", m["api_key"])
	}
}

// TestChannelClearSecrets PUT clear_secrets 显式清除秘钥字段：清除覆盖指针字段
// 的保留语义（nil/空串=保留），同字段同请求传非空新值时新值生效；去重、空白项
// 忽略、未知值 400；清除后 validateChannel 照常执行（不绕过校验）。
func TestChannelClearSecrets(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	chID := createChannel(t, r, cookie,
		`{"name":"resend主号","provider":"resend","api_key":"sk-1234567890abcdef","api_secret":"sec-9876543210abcdefgh",`+
			`"proxy_url":"http://user:secret@proxy.example.com:8080"}`)
	put := func(body string) map[string]any {
		w := doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", chID), cookie, "", body)
		return decode(t, mustStatus(t, w, http.StatusOK))
	}

	// 清除 proxy_url → GET 返回空
	put(`{"clear_secrets":["proxy_url"]}`)
	w := doReq(r, "GET", "/api/channels", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	var proxy any
	for _, it := range m["items"].([]any) {
		item := it.(map[string]any)
		if item["id"].(float64) == chID {
			proxy = item["proxy_url"]
		}
	}
	if proxy != "" {
		t.Fatalf("proxy_url should be cleared, got %q", proxy)
	}

	// 清除与同时传新值：新值生效
	m = put(`{"proxy_url":"socks5://u:p@h:1080","clear_secrets":["proxy_url"]}`)
	if m["proxy_url"] != "socks5://***@h:1080" {
		t.Fatalf("new proxy value should win over clear: %v", m["proxy_url"])
	}

	// 清除 api_key/api_secret：去重 + 空白项忽略
	m = put(`{"clear_secrets":["api_key","","api_secret","api_key"]}`)
	if m["api_key"] != "" || m["api_secret"] != "" {
		t.Fatalf("api_key/api_secret should be cleared: %v / %v", m["api_key"], m["api_secret"])
	}

	// 未知值 → 400 invalid_request
	body := mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", chID), cookie, "", `{"clear_secrets":["bogus"]}`), http.StatusBadRequest)
	if !strings.Contains(body, `"type":"invalid_request"`) {
		t.Fatalf("unknown clear_secrets: %s", body)
	}

	// smtp 渠道：清除秘钥不绕过 validateChannel——smtp_host 显式清空仍 400
	smtpID := createChannel(t, r, cookie,
		`{"name":"smtp线","provider":"smtp","smtp_host":"smtp.example.com","smtp_port":587,"from_email":"noreply@example.com","smtp_pass":"pw123456"}`)
	if code := doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", smtpID), cookie, "",
		`{"clear_secrets":["smtp_pass"],"smtp_host":""}`).Code; code != http.StatusBadRequest {
		t.Fatalf("clear_secrets must not bypass validateChannel: %d", code)
	}
	// 仅清除 smtp_pass → 200 且响应掩码变空
	m = decode(t, mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", smtpID), cookie, "",
		`{"clear_secrets":["smtp_pass"]}`), http.StatusOK))
	if m["smtp_pass"] != "" {
		t.Fatalf("smtp_pass should be cleared: %v", m["smtp_pass"])
	}
}

// TestChannelStatusAndUpdate 启停/删除/校验分支。
func TestChannelStatusAndUpdate(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"mc","provider":"mock"}`)

	// 非法 provider
	if code := doReq(r, "POST", "/api/channels", cookie, "", `{"name":"x","provider":"nope"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("invalid provider: %d", code)
	}
	// smtp 渠道缺 from_email
	if code := doReq(r, "POST", "/api/channels", cookie, "", `{"name":"s","provider":"smtp","smtp_host":"h","smtp_port":25}`).Code; code != http.StatusBadRequest {
		t.Fatalf("smtp without from_email: %d", code)
	}
	// 重名
	if code := doReq(r, "POST", "/api/channels", cookie, "", `{"name":"mc","provider":"mock"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("duplicate channel: %d", code)
	}

	// disable → status=1；enable → status=0 且清冷却/失败计数
	db.Model(&model.Channel{}).Where("id = ?", uint(chID)).Updates(map[string]any{
		"consecutive_failures": 3, "auto_disabled_reason": "连续失败",
	})
	m := decode(t, mustStatus(t, doReq(r, "POST", "/api/channels/1/status", cookie, "", `{"action":"disable"}`), http.StatusOK))
	if m["status"].(float64) != float64(model.ChannelManualDisabled) {
		t.Fatalf("disable: %v", m["status"])
	}
	m = decode(t, mustStatus(t, doReq(r, "POST", "/api/channels/1/status", cookie, "", `{"action":"enable"}`), http.StatusOK))
	if m["status"].(float64) != float64(model.ChannelEnabled) || m["consecutive_failures"].(float64) != 0 {
		t.Fatalf("enable: %v", m)
	}
	if code := doReq(r, "POST", "/api/channels/1/status", cookie, "", `{"action":"pause"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("invalid action: %d", code)
	}

	// 删除 + 404
	mustStatus(t, doReq(r, "DELETE", fmt.Sprintf("/api/channels/%g", chID), cookie, "", ""), http.StatusOK)
	if code := doReq(r, "DELETE", "/api/channels/9999", cookie, "", "").Code; code != http.StatusNotFound {
		t.Fatalf("delete missing: %d", code)
	}
}

// TestChannelTest 直发测试：绕过轮换、记 IsTest 日志；额度与轮换路径同口径——
// 发送前预留，受理成功保留扣减（used_day +1），失败释放。
func TestChannelTest(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie,
		`{"name":"mc","provider":"mock","from_email":"noreply@example.com","from_name":"CatMail"}`)

	w := doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID), cookie, "", `{"to":"dest@example.com"}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	// O8：成功体只含 ok/latency_ms，不再带恒为空的 "error":"" 字段
	if m["ok"] != true {
		t.Fatalf("test resp: %v", m)
	}
	if _, exists := m["error"]; exists {
		t.Fatalf("success body should not carry error field: %v", m)
	}
	if _, ok := m["latency_ms"]; !ok {
		t.Fatalf("latency_ms missing: %v", m)
	}

	var log model.SendLog
	if err := db.Where("is_test = ?", true).First(&log).Error; err != nil {
		t.Fatalf("test log missing: %v", err)
	}
	if log.Status != model.SendStatusSent || log.ToAddrs != `["dest@example.com"]` {
		t.Fatalf("test log: status=%d to=%s", log.Status, log.ToAddrs)
	}
	var ch model.Channel
	db.First(&ch, uint(chID))
	if ch.UsedDay != 1 { // 测试直发成功：预留转最终记账（与轮换路径同口径）
		t.Fatalf("test quota not reserved: %d", ch.UsedDay)
	}

	// 缺 to → 400
	if code := doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID), cookie, "", `{}`).Code; code != http.StatusBadRequest {
		t.Fatalf("missing to: %d", code)
	}
}

// ---------- 5. 发送校验分支 ----------

// TestSendValidation 参数错误 → 400 invalid_request；指定渠道语义由 rotator 保证。
func TestSendValidation(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mc","provider":"mock"}`)
	key := createKey(t, r, cookie, "k")

	cases := []struct{ name, body string }{
		{"missing text/html", `{"from":"a@b.com","to":["c@d.com"],"subject":"s"}`},
		{"missing to", `{"from":"a@b.com","subject":"s","text":"t"}`},
		{"missing subject", `{"from":"a@b.com","to":["c@d.com"],"text":"t"}`},
		{"bad from", `{"from":"not-an-email","to":["c@d.com"],"subject":"s","text":"t"}`},
		{"bad to email", `{"from":"a@b.com","to":["not-an-email"],"subject":"s","text":"t"}`},
		// 缺少 from：指定渠道 mc 未配置默认发件人 → 400（未指定渠道时改由
		// rotator 按候选渠道默认发件人回退，见 TestSendDefaultFromRotation）
		{"missing from", `{"to":["c@d.com"],"subject":"s","text":"t","channel":"mc"}`},
		{"bad attachment b64", `{"from":"a@b.com","to":["c@d.com"],"subject":"s","text":"t","attachments":[{"filename":"a.txt","content_base64":"!!!not-base64!!!"}]}`},
	}
	for _, tc := range cases {
		w := doReq(r, "POST", "/v1/send", "", key, tc.body)
		body := mustStatus(t, w, http.StatusBadRequest)
		if !strings.Contains(body, `"type":"invalid_request"`) {
			t.Fatalf("%s: %s", tc.name, body)
		}
	}

	// to 为字符串形态也接受（SPEC §2 string or array）
	w := doReq(r, "POST", "/v1/send", "", key, `{"from":"a@b.com","to":"c@d.com","subject":"s","text":"t"}`)
	mustStatus(t, w, http.StatusOK)

	// replyTo camelCase 别名
	w = doReq(r, "POST", "/v1/send", "", key, `{"from":"a@b.com","to":["c@d.com"],"subject":"s","text":"t","replyTo":"r@e.com"}`)
	mustStatus(t, w, http.StatusOK)

	// 指定不存在的渠道 → 400（rotator 语义）
	w = doReq(r, "POST", "/v1/send", "", key, `{"from":"a@b.com","to":["c@d.com"],"subject":"s","text":"t","channel":"ghost"}`)
	body := mustStatus(t, w, http.StatusBadRequest)
	if !strings.Contains(body, "ghost") {
		t.Fatalf("specified channel 404-ish: %s", body)
	}

	// body 非法 JSON
	if code := doReq(r, "POST", "/v1/send", "", key, `{bad`).Code; code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", code)
	}
	_ = cookie
}

// TestSendDefaultFromRotation P1-4：不带 from 且不指定 channel → 轮换路径用
// 选中渠道的默认发件人回退成功，日志记录实际发送的 from（SPEC §2；README 同口径）。
func TestSendDefaultFromRotation(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie,
		`{"name":"默认发件渠道","provider":"mock","priority":-10,"from_email":"noreply@example.com","from_name":"Snow"}`)
	key := createKey(t, r, cookie, "k")

	// 请求不带 from：轮换路径按选中渠道默认发件人回退
	w := doReq(r, "POST", "/v1/send", "", key, `{"to":["a@example.com"],"subject":"s","text":"hi"}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["accepted"] != true || m["channel"] != "默认发件渠道" {
		t.Fatalf("resp: %v", m)
	}
	var log model.SendLog
	if err := db.Where("channel_name = ? AND is_test = ?", "默认发件渠道", false).First(&log).Error; err != nil {
		t.Fatalf("log missing: %v", err)
	}
	if log.FromAddr != "noreply@example.com" {
		t.Errorf("log from = %q, want 渠道默认 noreply@example.com", log.FromAddr)
	}
}

// TestChannelUpdatePreservesCounters P1-2 + P2-6：PUT 编辑渠道只写表单可写集——
// 运行时计数（used_day/used_month/周期锚点）不被编辑时刻的旧快照抹掉；
// status 复位 0、连续失败清零、last_error 清空的复位语义生效。
func TestChannelUpdatePreservesCounters(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"mc","provider":"mock"}`)

	// 模拟并发发送累计出的运行态计数
	today := time.Now().Format("2006-01-02")
	month := time.Now().Format("2006-01")
	if err := db.Model(&model.Channel{}).Where("id = ?", uint(chID)).Updates(map[string]any{
		"used_day": 5, "day_anchor": today,
		"used_month": 7, "month_anchor": month,
		"last_error":           "boom",
		"last_used_at":         time.Now(),
		"consecutive_failures": 3,
	}).Error; err != nil {
		t.Fatalf("seed counters: %v", err)
	}

	// PUT 编辑 name/priority
	w := doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", chID), cookie, "", `{"name":"mc2","priority":5}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["name"] != "mc2" {
		t.Fatalf("name not applied: %v", m["name"])
	}

	var ch model.Channel
	if err := db.First(&ch, uint(chID)).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if ch.Name != "mc2" || ch.Priority != 5 {
		t.Fatalf("edit not applied: name=%q priority=%d", ch.Name, ch.Priority)
	}
	if ch.UsedDay != 5 || ch.UsedMonth != 7 {
		t.Errorf("used_day/used_month = %d/%d, want 5/7（编辑不得回写运行时计数）", ch.UsedDay, ch.UsedMonth)
	}
	if ch.DayAnchor != today || ch.MonthAnchor != month {
		t.Errorf("anchors = %q/%q, want %q/%q", ch.DayAnchor, ch.MonthAnchor, today, month)
	}
	if ch.LastError != "" {
		t.Errorf("last_error = %q, want 空（P2-6：编辑成功清除残留最近错误）", ch.LastError)
	}
	// 复位语义保留（表单可写集）：status 复位 0、连续失败清零
	if ch.Status != model.ChannelEnabled || ch.ConsecutiveFailures != 0 {
		t.Errorf("status/cf = %d/%d, want 0/0（复位语义保留）", ch.Status, ch.ConsecutiveFailures)
	}
}

// ---------- 6. 日志 ----------

// TestLogsListAndDetail 分页/筛选/详情。
func TestLogsListAndDetail(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mc","provider":"mock"}`)
	key := createKey(t, r, cookie, "k")
	var ak model.ApiKey
	if err := db.Where("key = ?", key).First(&ak).Error; err != nil {
		t.Fatalf("created key missing: %v", err)
	}

	for i, subject := range []string{"欢迎注册", "订单确认", "密码重置"} {
		w := doReq(r, "POST", "/v1/send", "", key,
			fmt.Sprintf(`{"from":"a@b.com","to":["u%d@c.com"],"subject":%q,"text":"t"}`, i, subject))
		mustStatus(t, w, http.StatusOK)
	}

	// 列表：created_at 倒序
	w := doReq(r, "GET", "/api/logs", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["total"].(float64) != 3 {
		t.Fatalf("total: %v", m["total"])
	}
	items := m["items"].([]any)
	if items[0].(map[string]any)["subject"] != "密码重置" {
		t.Fatalf("order: %v", items[0])
	}
	// api_key_id 为创建 Key 的 id（SPEC §4 SendLog.ApiKeyID 契约）
	if got := items[0].(map[string]any)["api_key_id"].(float64); got != float64(ak.ID) {
		t.Fatalf("list api_key_id: got %v want %d", got, ak.ID)
	}
	// to_addrs 为数组形态
	first := items[0].(map[string]any)
	if _, ok := first["to_addrs"].([]any); !ok {
		t.Fatalf("to_addrs not array: %v", first["to_addrs"])
	}

	// q 模糊（命中 subject 与 to_addrs）
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/logs?q=订单", cookie, "", ""), http.StatusOK))
	if m["total"].(float64) != 1 {
		t.Fatalf("q subject: %v", m["total"])
	}
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/logs?q=u1%40c.com", cookie, "", ""), http.StatusOK))
	if m["total"].(float64) != 1 {
		t.Fatalf("q to_addr: %v", m["total"])
	}
	// % 通配符不生效（escape 转义）
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/logs?q=%25", cookie, "", ""), http.StatusOK))
	if m["total"].(float64) != 0 {
		t.Fatalf("q escape: %v", m["total"])
	}

	// status 字符串筛选
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/logs?status=sent", cookie, "", ""), http.StatusOK))
	if m["total"].(float64) != 3 {
		t.Fatalf("status=sent: %v", m["total"])
	}
	if code := doReq(r, "GET", "/api/logs?status=999", cookie, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("status invalid: %d", code)
	}

	// 分页边界
	if code := doReq(r, "GET", "/api/logs?page=0", cookie, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("page=0: %d", code)
	}
	if code := doReq(r, "GET", "/api/logs?page_size=101", cookie, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("page_size=101: %d", code)
	}
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/logs?page=2&page_size=2", cookie, "", ""), http.StatusOK))
	if len(m["items"].([]any)) != 1 {
		t.Fatalf("page 2 size 2: %v", m)
	}

	// 详情：全字段含正文
	var row model.SendLog
	db.Order("id ASC").First(&row)
	w = doReq(r, "GET", fmt.Sprintf("/api/logs/%d", row.ID), cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["html_body"] == nil || m["text_body"] != "t" || m["message_id"] != row.MessageID {
		t.Fatalf("detail: %v", m)
	}
	if got := m["api_key_id"].(float64); got != float64(ak.ID) {
		t.Fatalf("detail api_key_id: got %v want %d", got, ak.ID)
	}
	if code := doReq(r, "GET", "/api/logs/99999", cookie, "", "").Code; code != http.StatusNotFound {
		t.Fatalf("detail missing: %d", code)
	}
}

// TestLogsSort sort 参数：newest（默认，created_at 倒序）/ oldest（正序）/
// latency_desc（耗时降序，同耗时按 created_at 倒序）；非法值 400。
func TestLogsSort(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mc","provider":"mock"}`)

	// 直接落库 4 条：created_at 递增，latency 与时间序刻意错开（最早条耗时最大）
	rows := []model.SendLog{
		{MessageID: "m1", ChannelID: 1, ChannelName: "mc", Provider: "mock", Status: model.SendStatusSent,
			ToAddrs: `["a@c.com"]`, Subject: "最早", LatencyMs: 1000, CreatedAt: time.Now().Add(-4 * time.Hour)},
		{MessageID: "m2", ChannelID: 1, ChannelName: "mc", Provider: "mock", Status: model.SendStatusSent,
			ToAddrs: `["b@c.com"]`, Subject: "次早", LatencyMs: 300, CreatedAt: time.Now().Add(-3 * time.Hour)},
		{MessageID: "m3", ChannelID: 1, ChannelName: "mc", Provider: "mock", Status: model.SendStatusSent,
			ToAddrs: `["c@c.com"]`, Subject: "再次", LatencyMs: 200, CreatedAt: time.Now().Add(-2 * time.Hour)},
		{MessageID: "m4", ChannelID: 1, ChannelName: "mc", Provider: "mock", Status: model.SendStatusSent,
			ToAddrs: `["d@c.com"]`, Subject: "最新", LatencyMs: 300, CreatedAt: time.Now().Add(-1 * time.Hour)},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}

	subjects := func(query string) []string {
		m := decode(t, mustStatus(t, doReq(r, "GET", query, cookie, "", ""), http.StatusOK))
		var out []string
		for _, it := range m["items"].([]any) {
			out = append(out, it.(map[string]any)["subject"].(string))
		}
		return out
	}

	// 默认（无 sort）与 sort=newest：created_at 倒序
	wantNewest := []string{"最新", "再次", "次早", "最早"}
	if got := subjects("/api/logs"); !reflect.DeepEqual(got, wantNewest) {
		t.Fatalf("default order: %v", got)
	}
	if got := subjects("/api/logs?sort=newest"); !reflect.DeepEqual(got, wantNewest) {
		t.Fatalf("sort=newest: %v", got)
	}
	// oldest：正序
	if got := subjects("/api/logs?sort=oldest"); !reflect.DeepEqual(got, []string{"最早", "次早", "再次", "最新"}) {
		t.Fatalf("sort=oldest: %v", got)
	}
	// latency_desc：1000 → 300 组内 created_at 倒序（最新在前）→ 200
	if got := subjects("/api/logs?sort=latency_desc"); !reflect.DeepEqual(got, []string{"最早", "最新", "次早", "再次"}) {
		t.Fatalf("sort=latency_desc: %v", got)
	}
	// 非法值 → 400
	if code := doReq(r, "GET", "/api/logs?sort=bogus", cookie, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("invalid sort: %d", code)
	}
}

// exportCSV 调用导出接口并解析响应（去掉 UTF-8 BOM 后按 CSV 解析）。
func exportCSV(t *testing.T, r *gin.Engine, cookie, query string) [][]string {
	t.Helper()
	w := doReq(r, "GET", "/api/logs/export"+query, cookie, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export status: %d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("content-type: %s", ct)
	}
	if !regexp.MustCompile(`^attachment; filename="catmail-logs-\d{8}-\d{6}\.csv"$`).MatchString(w.Header().Get("Content-Disposition")) {
		t.Fatalf("content-disposition: %s", w.Header().Get("Content-Disposition"))
	}
	raw := w.Body.Bytes()
	if len(raw) < 3 || raw[0] != 0xEF || raw[1] != 0xBB || raw[2] != 0xBF {
		t.Fatalf("missing UTF-8 BOM: % x", raw[:min(3, len(raw))])
	}
	records, err := csv.NewReader(bytes.NewReader(raw[3:])).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	return records
}

// TestLogsExport CSV 导出：响应头/BOM、列与行内容、收件人 "; " 连接、
// CSV 注入转义（= + - @ 前置单引号）、status/sort 生效且 page/page_size 被忽略。
func TestLogsExport(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mc","provider":"mock"}`)
	key := createKey(t, r, cookie, "k")

	// 两封经 API 的成功发送 + 一条注入样本（失败、测试、特殊字符开头）
	mustStatus(t, doReq(r, "POST", "/v1/send", "", key,
		`{"from":"a@b.com","to":["u1@c.com","u2@c.com"],"subject":"订单确认","text":"t"}`), http.StatusOK)
	mustStatus(t, doReq(r, "POST", "/v1/send", "", key,
		`{"from":"a@b.com","to":["u3@c.com"],"subject":"=公式注入","text":"t"}`), http.StatusOK)
	db.Create(&model.SendLog{MessageID: "m-inj", ProviderMessageID: "-dash-id", ChannelID: 1, ChannelName: "mc", Provider: "mock",
		Status: model.SendStatusFailed, FromAddr: "@at-start", ToAddrs: `["+plus@c.com"]`,
		Subject: "@at", Error: "+900", IsTest: true, LatencyMs: 5, CreatedAt: time.Now().Add(-24 * time.Hour)})

	// 列表序（newest）：=公式注入 → 订单确认 → 注入样本（24h 前最旧）
	records := exportCSV(t, r, cookie, "")
	wantHeader := []string{"时间", "状态", "渠道", "平台", "发件人", "收件人", "主题", "耗时ms", "测试", "错误信息", "消息ID", "备注"}
	if !reflect.DeepEqual(records[0], wantHeader) {
		t.Fatalf("header: %v", records[0])
	}
	if len(records) != 4 { // 表头 + 3 行
		t.Fatalf("rows: %d", len(records)-1)
	}
	// 时间列：本地 RFC3339 可解析
	if _, err := time.Parse(time.RFC3339, records[1][0]); err != nil {
		t.Fatalf("created_at not RFC3339: %v (%s)", err, records[1][0])
	}
	// = 公式注入转义
	if records[1][6] != "'=公式注入" || records[1][1] != "sent" || records[1][8] != "no" {
		t.Fatalf("row1: %v", records[1])
	}
	// 收件人多地址 "; " 连接
	if records[2][5] != "u1@c.com; u2@c.com" {
		t.Fatalf("to join: %q", records[2][5])
	}
	// 注入样本：@ + - 开头单元格全部前置单引号；测试/失败标记正确
	inj := records[3]
	if inj[1] != "failed" || inj[8] != "yes" {
		t.Fatalf("row3 status/test: %v", inj)
	}
	if inj[4] != "'@at-start" || inj[5] != "'+plus@c.com" || inj[6] != "'@at" || inj[9] != "'+900" || inj[10] != "'-dash-id" {
		t.Fatalf("injection escape: %v", inj)
	}
	// 备注列默认为空
	if inj[11] != "" {
		t.Fatalf("remark column should be empty: %q", inj[11])
	}

	// status/sort 生效；page/page_size 被忽略
	records = exportCSV(t, r, cookie, "?status=failed&sort=oldest&page=2&page_size=1")
	if len(records) != 2 { // 表头 + 1 条 failed
		t.Fatalf("filtered rows: %d", len(records)-1)
	}
	if records[1][10] != "'-dash-id" {
		t.Fatalf("oldest failed row: %v", records[1])
	}

	// 非法 sort → 400（CSV 头写出前即可返回 JSON 错误体）
	body := mustStatus(t, doReq(r, "GET", "/api/logs/export?sort=bogus", cookie, "", ""), http.StatusBadRequest)
	if !strings.Contains(body, `"type":"invalid_request"`) {
		t.Fatalf("invalid sort on export: %s", body)
	}
}

// TestLogsExportCap 导出行数上限 10000：达到上限时在「备注」列追加提示行。
func TestLogsExportCap(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	rows := make([]model.SendLog, logExportCap+1)
	for i := range rows {
		rows[i] = model.SendLog{MessageID: fmt.Sprintf("m-%d", i), ChannelID: 1, ChannelName: "mc",
			Provider: "mock", Status: model.SendStatusSent, ToAddrs: `["a@c.com"]`, Subject: "s"}
	}
	if err := db.CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatalf("seed %d logs: %v", len(rows), err)
	}

	records := exportCSV(t, r, cookie, "")
	if len(records) != logExportCap+2 { // 表头 + 10000 行 + 提示行
		t.Fatalf("records: %d (want %d)", len(records)-1, logExportCap+1)
	}
	hint := records[len(records)-1]
	for i := 0; i < len(hint)-1; i++ {
		if hint[i] != "" {
			t.Fatalf("hint row cell %d non-empty: %q", i, hint[i])
		}
	}
	if hint[len(hint)-1] != "已达导出上限，仅含前 10000 条" {
		t.Fatalf("hint: %v", hint)
	}
}

// ---------- 7. Keys ----------

// TestKeysFlow 创建一次性明文、列表掩码、启停后 401、删除。
func TestKeysFlow(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mc","provider":"mock"}`)

	key := createKey(t, r, cookie, "主 Key")
	if !regexp.MustCompile(`^cm-[0-9a-f]{32}$`).MatchString(key) {
		t.Fatalf("key: %s", key)
	}

	// 列表掩码 cm-xxxx…xxxx 且不含明文
	w := doReq(r, "GET", "/api/keys", cookie, "", "")
	body := mustStatus(t, w, http.StatusOK)
	if strings.Contains(body, key) {
		t.Fatalf("list leaks plaintext key: %s", body)
	}
	m := decode(t, body)
	item := m["items"].([]any)[0].(map[string]any)
	if item["masked_key"] != key[:7]+"…"+key[len(key)-4:] {
		t.Fatalf("masked key: %v", item["masked_key"])
	}

	// 停用后发送 → 401
	keyID := item["id"].(float64)
	mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/keys/%g", keyID), cookie, "", `{"enabled":false}`), http.StatusOK)
	if code := doReq(r, "POST", "/v1/send", "", key, sendBody("a@b.com", "c@d.com", "s")).Code; code != http.StatusUnauthorized {
		t.Fatalf("disabled key send: %d", code)
	}
	// 重新启用 → 恢复
	mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/keys/%g", keyID), cookie, "", `{"enabled":true,"name":"改名"}`), http.StatusOK)
	mustStatus(t, doReq(r, "POST", "/v1/send", "", key, sendBody("a@b.com", "c@d.com", "s")), http.StatusOK)

	// 删除后再用 → 401
	mustStatus(t, doReq(r, "DELETE", fmt.Sprintf("/api/keys/%g", keyID), cookie, "", ""), http.StatusOK)
	if code := doReq(r, "POST", "/v1/send", "", key, sendBody("a@b.com", "c@d.com", "s")).Code; code != http.StatusUnauthorized {
		t.Fatalf("deleted key send: %d", code)
	}
	// 空名称
	if code := doReq(r, "POST", "/api/keys", cookie, "", `{"name":"  "}`).Code; code != http.StatusBadRequest {
		t.Fatalf("empty name: %d", code)
	}
}

// TestKeysTotalSent GET /api/keys 的 items 携带 total_sent：该 key 的 SendLog
// 中 status=sent 的总数（含测试发送，聚合不做 is_test 过滤；失败不计入；
// 单条 GROUP BY 聚合）。
func TestKeysTotalSent(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	// 渠道配默认发件人，供下方「测试直发」使用
	chID := createChannel(t, r, cookie, `{"name":"mc","provider":"mock","from_email":"noreply@example.com"}`)
	key := createKey(t, r, cookie, "主Key")

	// 经该 key 成功发送 2 封
	mustStatus(t, doReq(r, "POST", "/v1/send", "", key, sendBody("a@b.com", "c@d.com", "s1")), http.StatusOK)
	mustStatus(t, doReq(r, "POST", "/v1/send", "", key, sendBody("a@b.com", "c@d.com", "s2")), http.StatusOK)

	// 渠道测试直发（is_test=true、无 key 归属）不计入任何 key
	mustStatus(t, doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID), cookie, "", `{"to":"t@e.com"}`), http.StatusOK)

	totalSentOf := func() map[string]float64 {
		m := decode(t, mustStatus(t, doReq(r, "GET", "/api/keys", cookie, "", ""), http.StatusOK))
		out := map[string]float64{}
		for _, it := range m["items"].([]any) {
			item := it.(map[string]any)
			out[item["name"].(string)] = item["total_sent"].(float64)
		}
		return out
	}

	if totals := totalSentOf(); totals["主Key"] != 2 {
		t.Fatalf("total_sent after 2 sends: %v", totals)
	}

	var ak model.ApiKey
	if err := db.Where("name = ?", "主Key").First(&ak).Error; err != nil {
		t.Fatalf("load key: %v", err)
	}
	// 失败日志不计入
	db.Create(&model.SendLog{MessageID: "m-f", ApiKeyID: ak.ID, ChannelID: 1, ChannelName: "mc", Provider: "mock",
		Status: model.SendStatusFailed, ToAddrs: `["c@d.com"]`, Subject: "f"})
	if totals := totalSentOf(); totals["主Key"] != 2 {
		t.Fatalf("total_sent must exclude failed: %v", totals)
	}

	// is_test=true 但归属该 key 的成功日志同样计入（聚合不做 is_test 过滤）
	db.Create(&model.SendLog{MessageID: "m-t", ApiKeyID: ak.ID, ChannelID: 1, ChannelName: "mc", Provider: "mock",
		Status: model.SendStatusSent, IsTest: true, ToAddrs: `["c@d.com"]`, Subject: "t"})
	if totals := totalSentOf(); totals["主Key"] != 3 {
		t.Fatalf("total_sent must include test send of the key: %v", totals)
	}

	// 无发送记录的新 Key：0
	createKey(t, r, cookie, "新Key")
	if totals := totalSentOf(); totals["新Key"] != 0 {
		t.Fatalf("new key total_sent: %v", totals)
	}
}

// ---------- 8. 统计 ----------

// TestStatsOverviewAndDaily 总览计数/成功率/渠道用量；daily 补零与边界。
func TestStatsOverviewAndDaily(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"mc","provider":"mock","priority":-10}`)
	key := createKey(t, r, cookie, "k")

	mustStatus(t, doReq(r, "POST", "/v1/send", "", key, sendBody("a@b.com", "c@d.com", "s1")), http.StatusOK)
	mustStatus(t, doReq(r, "POST", "/v1/send", "", key, sendBody("a@b.com", "c@d.com", "s2")), http.StatusOK)

	// overview
	w := doReq(r, "GET", "/api/stats/overview", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["today_sent"].(float64) != 2 || m["month_sent"].(float64) != 2 || m["success_rate"].(float64) != 100 {
		t.Fatalf("overview: %v", m)
	}
	var found bool
	for _, it := range m["channels"].([]any) {
		ch := it.(map[string]any)
		if ch["name"] == "mc" {
			found = true
			if ch["used_day"].(float64) != 2 {
				t.Fatalf("overview channel used_day: %v", ch["used_day"])
			}
			if ch["last_error"] == nil {
				t.Fatalf("overview channel last_error missing")
			}
		}
	}
	if !found {
		t.Fatalf("overview channels missing mc")
	}

	// daily：7 天含今天，末位 sent=2，缺数据日补 0
	w = doReq(r, "GET", "/api/stats/daily?days=7", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	items := m["items"].([]any)
	if len(items) != 7 {
		t.Fatalf("daily items: %d", len(items))
	}
	last := items[6].(map[string]any)
	today := time.Now().Format("01-02")
	if last["date"] != today || last["sent"].(float64) != 2 {
		t.Fatalf("daily today: %v", last)
	}
	first := items[0].(map[string]any)
	if first["sent"].(float64) != 0 || first["failed"].(float64) != 0 {
		t.Fatalf("daily zero fill: %v", first)
	}

	// channel_id 过滤 + 边界
	m = decode(t, mustStatus(t, doReq(r, "GET", fmt.Sprintf("/api/stats/daily?days=3&channel_id=%g", chID), cookie, "", ""), http.StatusOK))
	if m["items"].([]any)[2].(map[string]any)["sent"].(float64) != 2 {
		t.Fatalf("daily channel filter: %v", m)
	}
	if code := doReq(r, "GET", "/api/stats/daily?days=0", cookie, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("days=0: %d", code)
	}
	if code := doReq(r, "GET", "/api/stats/daily?days=91", cookie, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("days=91: %d", code)
	}
	// failed 计数：指定不存在渠道发送 → 400 invalid_request，不落日志不影响统计
	if code := doReq(r, "GET", "/api/stats/daily?channel_id=abc", cookie, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("channel_id invalid: %d", code)
	}

	// today_failed：制造一条失败日志
	db.Create(&model.SendLog{MessageID: "m-fail", ChannelID: 1, ChannelName: "mc", Provider: "mock",
		Status: model.SendStatusFailed, FromAddr: "a@b.com", ToAddrs: `["c@d.com"]`, Subject: "f"})
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/stats/overview", cookie, "", ""), http.StatusOK))
	if m["today_failed"].(float64) != 1 || m["success_rate"].(float64) != 67 {
		t.Fatalf("overview failed/rate: %v / %v", m["today_failed"], m["success_rate"])
	}
}

// ---------- 9. 设置 ----------

// TestSettingsValidation 设置范围校验与读写回环。
func TestSettingsValidation(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	w := doReq(r, "GET", "/api/settings", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["rotation"] != "priority" || m["max_attempts"].(float64) != 3 || m["log_retention_days"].(float64) != 90 {
		t.Fatalf("defaults: %v", m)
	}
	// 会话签名密钥绝不外泄（notify_webhook_secret 属管理页需展示/复制的回执签名
	// 密钥，明文回显是安全加固三件套 B 的既定契约，不在保密范围）
	if _, leaked := m["session_secret"]; leaked || strings.Contains(w.Body.String(), "session_secret") {
		t.Fatalf("settings leaks secret: %s", w.Body.String())
	}

	for _, body := range []string{
		`{"rotation":"random","max_attempts":3,"cooldown_seconds":60,"consecutive_failure_threshold":5,"log_retention_days":90}`,
		`{"rotation":"priority","max_attempts":0,"cooldown_seconds":60,"consecutive_failure_threshold":5,"log_retention_days":90}`,
		`{"rotation":"priority","max_attempts":11,"cooldown_seconds":60,"consecutive_failure_threshold":5,"log_retention_days":90}`,
		`{"rotation":"priority","max_attempts":3,"cooldown_seconds":0,"consecutive_failure_threshold":5,"log_retention_days":90}`,
		`{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,"consecutive_failure_threshold":2,"log_retention_days":90}`,
		`{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,"consecutive_failure_threshold":21,"log_retention_days":90}`,
		`{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,"consecutive_failure_threshold":5,"log_retention_days":-1}`,
		`{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,"consecutive_failure_threshold":5,"log_retention_days":366}`,
	} {
		if code := doReq(r, "PUT", "/api/settings", cookie, "", body).Code; code != http.StatusBadRequest {
			t.Fatalf("invalid settings accepted: %s", body)
		}
	}

	valid := `{"rotation":"round_robin","max_attempts":5,"cooldown_seconds":120,"consecutive_failure_threshold":8,"log_retention_days":30}`
	m = decode(t, mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", valid), http.StatusOK))
	if m["rotation"] != "round_robin" || m["max_attempts"].(float64) != 5 {
		t.Fatalf("put: %v", m)
	}
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/settings", cookie, "", ""), http.StatusOK))
	if m["cooldown_seconds"].(float64) != 120 || m["consecutive_failure_threshold"].(float64) != 8 || m["log_retention_days"].(float64) != 30 {
		t.Fatalf("roundtrip: %v", m)
	}
}

// ---------- 10. API 404 统一错误体 ----------

// TestNotFoundJSON 未知 /api、/v1 与 /emails 子路径返回统一错误体
// （P2-12：/emails/* 不落 SPA fallback，与 /v1 同构的 JSON 404）。
func TestNotFoundJSON(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	w := doReq(r, "GET", "/api/unknown", cookie, "", "")
	body := mustStatus(t, w, http.StatusNotFound)
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("api 404: %s", body)
	}
	w = doReq(r, "GET", "/v1/nope", cookie, "", "")
	if code := w.Code; code != http.StatusNotFound {
		t.Fatalf("v1 404: %d", code)
	}
	w = doReq(r, "GET", "/emails/assets/app.js", cookie, "", "")
	body = mustStatus(t, w, http.StatusNotFound)
	if !strings.Contains(body, `"type":"invalid_request"`) {
		t.Fatalf("emails subpath 404: %s", body)
	}
}

// ---------- 11. 健康检查与输入上限（R1 质量波） ----------

// TestHealth GET /api/health 公开无鉴权，返回 {ok:true}；
// 携带无效 Cookie 也必须 200（不得要求会话）。
func TestHealth(t *testing.T) {
	r, _ := newTestEnv(t)

	w := doReq(r, "GET", "/api/health", "", "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["ok"] != true {
		t.Fatalf("health: %v", m)
	}
	w = doReq(r, "GET", "/api/health", "garbage-cookie", "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["ok"] != true {
		t.Fatalf("health with cookie: %v", m)
	}
}

// TestSendInputCaps /v1/send 超长 from/channel/headers 一律 400 invalid_request。
func TestSendInputCaps(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mc","provider":"mock"}`)
	key := createKey(t, r, cookie, "k")

	longFrom := strings.Repeat("a", 1100) + "@example.com"
	manyHeaders := make([]string, 51)
	for i := range manyHeaders {
		manyHeaders[i] = fmt.Sprintf(`"X-H%d":"v"`, i)
	}
	cases := []struct{ name, body string }{
		{"from too long", fmt.Sprintf(`{"from":%q,"to":["c@d.com"],"subject":"s","text":"t"}`, longFrom)},
		{"channel too long", fmt.Sprintf(`{"from":"a@b.com","to":["c@d.com"],"subject":"s","text":"t","channel":%q}`, strings.Repeat("ch", 100))},
		{"headers too many", `{"from":"a@b.com","to":["c@d.com"],"subject":"s","text":"t","headers":{` +
			strings.Join(manyHeaders, ",") + `}}`},
		{"header value too long", `{"from":"a@b.com","to":["c@d.com"],"subject":"s","text":"t","headers":{"X-A":"` + strings.Repeat("v", 5000) + `"}}`},
		{"email too long", `{"from":"a@b.com","to":["` + strings.Repeat("u", 400) + "@example.com" + `"],"subject":"s","text":"t"}`},
	}
	for _, tc := range cases {
		w := doReq(r, "POST", "/v1/send", "", key, tc.body)
		body := mustStatus(t, w, http.StatusBadRequest)
		if !strings.Contains(body, `"type":"invalid_request"`) {
			t.Fatalf("%s: %s", tc.name, body)
		}
	}

	// 上限内正常通过（headers 50 项、from 1024 内）
	fiftyHeaders := make([]string, 50)
	for i := range fiftyHeaders {
		fiftyHeaders[i] = fmt.Sprintf(`"X-N%d":"v"`, i)
	}
	w := doReq(r, "POST", "/v1/send", "", key,
		`{"from":"a@b.com","to":["c@d.com"],"subject":"s","text":"t","headers":{`+strings.Join(fiftyHeaders, ",")+`}}`)
	mustStatus(t, w, http.StatusOK)

	// P2-7：headers 键/值含 CRLF → 400（防 SMTP/MIME 头注入；JSON 内以 \r\n
	// 转义序列传输，解码后即为真实换行符）
	for _, tc := range []struct{ name, body string }{
		{"header value CRLF", `{"from":"a@b.com","to":["c@d.com"],"subject":"s","text":"t","headers":{"X-A":"v\r\nInjected: 1"}}`},
		{"header key CRLF", `{"from":"a@b.com","to":["c@d.com"],"subject":"s","text":"t","headers":{"X-A\r\nB":"v"}}`},
	} {
		w := doReq(r, "POST", "/v1/send", "", key, tc.body)
		body := mustStatus(t, w, http.StatusBadRequest)
		if !strings.Contains(body, "换行符") {
			t.Fatalf("%s: %s", tc.name, body)
		}
	}

	// P2-2：html/text 各限 1MB 字符 → 400 正文过大
	bigHTML := fmt.Sprintf(`{"from":"a@b.com","to":["c@d.com"],"subject":"s","html":"%s"}`, strings.Repeat("a", 1048577))
	w = doReq(r, "POST", "/v1/send", "", key, bigHTML)
	body := mustStatus(t, w, http.StatusBadRequest)
	if !strings.Contains(body, "正文过大") {
		t.Fatalf("html too long: %s", body)
	}
}

// TestChannelFieldCaps 渠道字段长度上限与 region 枚举校验 → 400。
func TestChannelFieldCaps(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	cases := []struct{ name, body string }{
		{"api_key too long", `{"name":"c1","provider":"resend","api_key":"` + strings.Repeat("k", 2000) + `"}`},
		{"allowed_domains too long", `{"name":"c2","provider":"mock","allowed_domains":"` + strings.Repeat("d.com,", 500) + `"}`},
		{"from_name too long", `{"name":"c3","provider":"mock","from_email":"a@b.com","from_name":"` + strings.Repeat("名", 600) + `"}`},
		{"region invalid", `{"name":"c4","provider":"mailgun","domain":"mg.example.com","region":"cn"}`},
		{"smtp_user too long", `{"name":"c5","provider":"smtp","smtp_host":"h","smtp_port":25,"from_email":"a@b.com","smtp_user":"` + strings.Repeat("u", 600) + `"}`},
	}
	for _, tc := range cases {
		if code := doReq(r, "POST", "/api/channels", cookie, "", tc.body).Code; code != http.StatusBadRequest {
			t.Fatalf("%s: want 400 got %d", tc.name, code)
		}
	}

	// 合法 region=eu 通过
	mustStatus(t, doReq(r, "POST", "/api/channels", cookie, "",
		`{"name":"c6","provider":"mailgun","domain":"mg.example.com","region":"eu","api_key":"key-1234567890"}`), http.StatusOK)
}

// TestBodyLimit 管理 API 请求体超过 1MB → 400 请求体过大。
func TestBodyLimit(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	big := `{"name":"` + strings.Repeat("x", 2<<20) + `"}`
	w := doReq(r, "POST", "/api/channels", cookie, "", big)
	body := mustStatus(t, w, http.StatusBadRequest)
	if !strings.Contains(body, "请求体过大") {
		t.Fatalf("body limit: %s", body)
	}

	// 登录入口同样受限
	w = doReq(r, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"`+strings.Repeat("p", 2<<20)+`"}`)
	if !strings.Contains(mustStatus(t, w, http.StatusBadRequest), "请求体过大") {
		t.Fatalf("login body limit: %s", w.Body.String())
	}
}

// TestLoginCredentialCaps 登录超长凭据与错误凭据同样 401（不泄露校验细节、防 bcrypt 放大）。
func TestLoginCredentialCaps(t *testing.T) {
	r, _ := newTestEnv(t)

	w := doReq(r, "POST", "/api/auth/login", "", "",
		`{"username":"admin","password":"`+strings.Repeat("p", 500)+`"}`)
	body := mustStatus(t, w, http.StatusUnauthorized)
	if !strings.Contains(body, `"type":"unauthorized"`) {
		t.Fatalf("long password login: %s", body)
	}
	if code := doReq(r, "POST", "/api/auth/login", "", "",
		`{"username":"`+strings.Repeat("a", 500)+`","password":"x"}`).Code; code != http.StatusUnauthorized {
		t.Fatalf("long username login: %d", code)
	}
}

// ---------- 12. P2 加固回归 ----------

// TestLoginRateLimit P2-1：同 用户名+IP 在窗口内连续失败达阈值后锁定，
// 第 6 次（即使密码正确）一律 429「尝试次数过多」；成功登录清零计数。
// 限流器为 Server 内存态，newTestEnv 每用例独立实例，测试间天然隔离。
func TestLoginRateLimit(t *testing.T) {
	r, _ := newTestEnv(t)
	for i := 0; i < 5; i++ {
		w := doReq(r, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"wrong"}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("fail attempt %d: want 401 got %d body=%s", i+1, w.Code, w.Body.String())
		}
	}
	// 第 6 次：锁定期内即使密码正确也 429
	w := doReq(r, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"admin123"}`)
	body := mustStatus(t, w, http.StatusTooManyRequests)
	if !strings.Contains(body, "尝试次数过多") || !strings.Contains(body, `"type":"quota_exhausted"`) {
		t.Fatalf("rate limited body: %s", body)
	}

	// 成功登录清零计数：失败 3 次 → 成功 → 再失败 3 次仍未达阈值（401 而非 429）
	r2, _ := newTestEnv(t)
	for i := 0; i < 3; i++ {
		doReq(r2, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"wrong"}`)
	}
	login(t, r2, "admin", "admin123")
	for i := 0; i < 3; i++ {
		w := doReq(r2, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"wrong"}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("after reset attempt %d: want 401 got %d", i+1, w.Code)
		}
	}
}

// legacySessionToken 构造 P2-8 之前的旧格式会话 token（"exp|username|sig"，签名合法），
// 用于验证无 v2 前缀的旧 token 一律无效。
func legacySessionToken(secret, username string) string {
	payload := fmt.Sprintf("%d|%s", time.Now().Add(sessionTTL).Unix(), username)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "|" + hex.EncodeToString(mac.Sum(nil))
}

// TestSessionEpochInvalidation 按用户会话纪元（O10）：本人改密 → 旧纪元会话失效、
// 响应重签的新 cookie 保持当前会话；管理员重置他人密码 → 仅目标用户旧会话失效，
// 操作者会话不受影响；旧格式（无 v2 前缀）token 一律无效。
func TestSessionEpochInvalidation(t *testing.T) {
	r, db := newTestEnv(t)
	var st model.Setting
	if err := db.First(&st, 1).Error; err != nil {
		t.Fatalf("setting row: %v", err)
	}
	cookie := login(t, r, "admin", "admin123")
	mustStatus(t, doReq(r, "GET", "/api/auth/me", cookie, "", ""), http.StatusOK)

	// 旧格式 token（签名合法但无 v2 前缀）→ 401
	legacy := legacySessionToken(st.SessionSecret, "admin")
	if code := doReq(r, "GET", "/api/auth/me", legacy, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("legacy token without v2 prefix: %d", code)
	}

	// 本人改密 → 旧 cookie 失效，响应重签的新 cookie 保持当前会话
	w := doReq(r, "PUT", "/api/auth/password", cookie, "", `{"old_password":"admin123","new_password":"newpass1"}`)
	mustStatus(t, w, http.StatusOK)
	if code := doReq(r, "GET", "/api/auth/me", cookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("old session after change: %d", code)
	}
	var fresh *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			fresh = c
		}
	}
	if fresh == nil || fresh.Value == "" {
		t.Fatalf("change password must re-sign session cookie")
	}
	mustStatus(t, doReq(r, "GET", "/api/auth/me", fresh.Value, "", ""), http.StatusOK)

	// 管理员重置他人密码 → 仅目标用户旧会话失效，操作者会话不受影响（按用户纪元）
	mustStatus(t, doReq(r, "POST", "/api/users", fresh.Value, "", `{"username":"bob","password":"secret1"}`), http.StatusOK)
	bobCookie := login(t, r, "bob", "secret1")
	mustStatus(t, doReq(r, "GET", "/api/auth/me", bobCookie, "", ""), http.StatusOK)
	mustStatus(t, doReq(r, "PUT", "/api/users/2/password", fresh.Value, "", `{"new_password":"newpass2"}`), http.StatusOK)
	if code := doReq(r, "GET", "/api/auth/me", bobCookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("bob session after reset: %d", code)
	}
	mustStatus(t, doReq(r, "GET", "/api/auth/me", fresh.Value, "", ""), http.StatusOK)
	login(t, r, "bob", "newpass2") // 新密码可重新登录
}

// TestDuplicateGuards P2-13：重名（用户/渠道）统一 400 文案；
// P2-5：渠道 PUT 省略字段 = 保留原值、非秘钥字段显式空串 = 清空。
func TestDuplicateGuards(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 重名用户（预检路径）
	mustStatus(t, doReq(r, "POST", "/api/users", cookie, "", `{"username":"bob","password":"secret1"}`), http.StatusOK)
	if body := mustStatus(t, doReq(r, "POST", "/api/users", cookie, "", `{"username":"bob","password":"secret1"}`), http.StatusBadRequest); !strings.Contains(body, "用户名已存在") {
		t.Fatalf("dup user: %s", body)
	}

	// 重名渠道（预检路径）
	createChannel(t, r, cookie, `{"name":"mc","provider":"mock"}`)
	if body := mustStatus(t, doReq(r, "POST", "/api/channels", cookie, "", `{"name":"mc","provider":"mock"}`), http.StatusBadRequest); !strings.Contains(body, "渠道名称已存在") {
		t.Fatalf("dup channel: %s", body)
	}

	// P2-5：PUT 仅改 priority，其余字段（含 smtp 配置与秘钥）全部保留
	smtpID := createChannel(t, r, cookie,
		`{"name":"smtp线","provider":"smtp","smtp_host":"smtp.example.com","smtp_port":587,"smtp_user":"mailer","smtp_pass":"password123","smtp_tls":"starttls","from_email":"noreply@example.com"}`)
	m := decode(t, mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", smtpID), cookie, "", `{"priority":3}`), http.StatusOK))
	if m["name"] != "smtp线" || m["smtp_host"] != "smtp.example.com" || m["smtp_tls"] != "starttls" || m["smtp_port"].(float64) != 587 {
		t.Fatalf("PUT omit should keep fields: %v", m)
	}
	if m["smtp_pass"] != "pass…d123" {
		t.Fatalf("PUT omit should keep smtp_pass: %v", m["smtp_pass"])
	}
	// 非秘钥字段显式空串 = 清空
	m = decode(t, mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/channels/%g", smtpID), cookie, "", `{"smtp_user":""}`), http.StatusOK))
	if m["smtp_user"] != "" {
		t.Fatalf("PUT empty string should clear smtp_user: %v", m["smtp_user"])
	}
}

// TestCookieSecureOption P2-9：WithCookieSecure(true) 时会话 Cookie 附加 Secure 属性。
func TestCookieSecureOption(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	Register(r, db, WithCookieSecure(true))

	w := doReq(r, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"admin123"}`)
	mustStatus(t, w, http.StatusOK)
	var cookie *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == sessionCookieName {
			cookie = ck
		}
	}
	if cookie == nil {
		t.Fatal("cookie missing")
	}
	if !cookie.Secure {
		t.Fatal("cookie Secure flag not set with WithCookieSecure(true)")
	}
}
