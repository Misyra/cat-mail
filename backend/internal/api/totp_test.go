// totp_test.go 两步验证（TOTP，C4）：绑定→启用→两步登录全流程（测试内用
// pquerna/otp 生成有效码）、错码 401、恢复码单次使用后失效、票据过期/伪造/
// 当会话用被拒、会话 cookie 当票据用被拒、disable 密码错拒、enable 后旧会话
// 失效当前重签有效、限流生效，以及 secret/恢复码不进 GET 响应。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"

	"catmail/internal/model"
)

// totpCode 用密钥生成当前时刻的 6 位 TOTP 码（±1 窗口校验，窗口边界不抖）。
func totpCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	return code
}

// setupTotp 走 setup，返回 base32 secret 与 otpauth URL。
func setupTotp(t *testing.T, r *gin.Engine, cookie string) (string, string) {
	t.Helper()
	m := decode(t, mustStatus(t, doReq(r, "POST", "/api/auth/totp/setup", cookie, "", ""), http.StatusOK))
	secret, _ := m["secret"].(string)
	url, _ := m["otpauth_url"].(string)
	if secret == "" || url == "" {
		t.Fatalf("setup response: %v", m)
	}
	return secret, url
}

// enableTotp 用有效码完成 enable，返回恢复码明文列表与重签后的会话 cookie。
func enableTotp(t *testing.T, r *gin.Engine, cookie, secret string) ([]string, string) {
	t.Helper()
	w := doReq(r, "POST", "/api/auth/totp/enable", cookie, "",
		fmt.Sprintf(`{"code":%q}`, totpCode(t, secret)))
	m := decode(t, mustStatus(t, w, http.StatusOK))
	raw, _ := json.Marshal(m["recovery_codes"])
	var codes []string
	if err := json.Unmarshal(raw, &codes); err != nil || len(codes) != recoveryCodeCount {
		t.Fatalf("recovery codes: %v", m["recovery_codes"])
	}
	return codes, reSignedCookie(t, w)
}

// loginStep1 第一步登录（密码正确、已开两步验证）：断言不建会话、返回票据，
// 返回 ticket。
func loginStep1(t *testing.T, r *gin.Engine, username, password string) string {
	t.Helper()
	w := doReq(r, "POST", "/api/auth/login", "", "",
		fmt.Sprintf(`{"username":%q,"password":%q}`, username, password))
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["totp_required"] != true {
		t.Fatalf("step1 must require totp: %v", m)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Fatalf("step1 must not create session, got cookie")
		}
	}
	ticket, _ := m["ticket"].(string)
	if !strings.HasPrefix(ticket, totpTicketPrefix+"|") {
		t.Fatalf("ticket format: %q", ticket)
	}
	return ticket
}

// TestTotpBindAndTwoStepLogin 绑定→开→两步登录全流程：
// setup → 错码 401 → 正确码 enable（恢复码一次性返回、库存 sha256、epoch +1、
// 旧会话失效当前重签有效）→ 登录第一步不建会话 → 错码 401 → TOTP 码登录成功
// → 恢复码单次有效（命中即删、复用 401、其余码可用、小写输入归一化）。
func TestTotpBindAndTwoStepLogin(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 未 setup 直接 enable → 400
	if code := doReq(r, "POST", "/api/auth/totp/enable", cookie, "", `{"code":"123456"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("enable before setup: %d", code)
	}

	secret, url := setupTotp(t, r, cookie)
	if !regexp.MustCompile(`^[A-Z2-7]+=*$`).MatchString(secret) {
		t.Fatalf("secret not base32: %q", secret)
	}
	if !strings.Contains(url, "issuer=CatMail") || !strings.Contains(url, "CatMail%3Aadmin") && !strings.Contains(url, "CatMail:admin") {
		t.Fatalf("otpauth url: %s", url)
	}
	// setup 后 enabled=false（未生效）
	var u model.User
	if err := db.Where("username = ?", "admin").First(&u).Error; err != nil {
		t.Fatalf("load admin: %v", err)
	}
	if u.TOTPEnabled || u.TOTPSecret != secret {
		t.Fatalf("after setup: enabled=%v secret_match=%v", u.TOTPEnabled, u.TOTPSecret == secret)
	}

	// enable 错码 → 401，且不生效
	wrong := doReq(r, "POST", "/api/auth/totp/enable", cookie, "", `{"code":"000000"}`)
	if wrong.Code != http.StatusUnauthorized || !strings.Contains(wrong.Body.String(), "验证码错误") {
		t.Fatalf("enable wrong code: %d %s", wrong.Code, wrong.Body.String())
	}
	if err := db.Where("username = ?", "admin").First(&u).Error; err != nil || u.TOTPEnabled {
		t.Fatalf("wrong code must not enable: %v", err)
	}

	// 正确码 enable：恢复码一次性返回、纪元 +1、旧会话失效、重签 cookie 有效
	codes, fresh := enableTotp(t, r, cookie, secret)
	for _, rc := range codes {
		if !regexp.MustCompile(`^[A-Z2-7]{10}$`).MatchString(rc) {
			t.Fatalf("recovery code format: %q", rc)
		}
	}
	if err := db.Where("username = ?", "admin").First(&u).Error; err != nil {
		t.Fatalf("reload admin: %v", err)
	}
	if !u.TOTPEnabled || u.SessionEpoch != 1 {
		t.Fatalf("after enable: enabled=%v epoch=%d", u.TOTPEnabled, u.SessionEpoch)
	}
	// 库存为 sha256 hex、不含任何明文
	var stored []string
	if err := json.Unmarshal([]byte(u.RecoveryCodes), &stored); err != nil || len(stored) != recoveryCodeCount {
		t.Fatalf("stored recovery codes: %q err=%v", u.RecoveryCodes, err)
	}
	for _, rc := range codes {
		for _, h := range stored {
			if h == rc || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h) {
				t.Fatalf("stored codes must be sha256 hex without plaintext: %q vs %q", h, rc)
			}
		}
	}
	// 旧纪元会话失效、重签 cookie 保持登录
	if code := doReq(r, "GET", "/api/auth/me", cookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("old session after enable: %d", code)
	}
	mustStatus(t, doReq(r, "GET", "/api/auth/me", fresh, "", ""), http.StatusOK)

	// 已启用再 setup / enable → 400
	if code := doReq(r, "POST", "/api/auth/totp/setup", fresh, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("setup when enabled: %d", code)
	}
	if code := doReq(r, "POST", "/api/auth/totp/enable", fresh, "", fmt.Sprintf(`{"code":%q}`, totpCode(t, secret))).Code; code != http.StatusBadRequest {
		t.Fatalf("enable when enabled: %d", code)
	}

	// 登录第一步：密码正确 → 不建会话，返回 t| 票据
	ticket := loginStep1(t, r, "admin", "admin123")

	// 第二步错码 → 401，文案与登录失败一致（防枚举）
	w := doReq(r, "POST", "/api/auth/login/totp", "", "",
		fmt.Sprintf(`{"ticket":%q,"code":"000000"}`, ticket))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "用户名或密码错误") {
		t.Fatalf("step2 wrong code: %d %s", w.Code, w.Body.String())
	}

	// 第二步 TOTP 码正确 → 会话 cookie，可访问 me
	code := totpCode(t, secret)
	w = doReq(r, "POST", "/api/auth/login/totp", "", "",
		fmt.Sprintf(`{"ticket":%q,"code":%q}`, ticket, code))
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["username"] != "admin" {
		t.Fatalf("step2 login resp: %v", m)
	}
	var sessionCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatalf("step2 success must issue session cookie")
	}
	mustStatus(t, doReq(r, "GET", "/api/auth/me", sessionCookie.Value, "", ""), http.StatusOK)

	// 恢复码：小写输入归一化 → 登录成功；同码复用 401（库存已删）；其余码可用
	ticket = loginStep1(t, r, "admin", "admin123")
	w = doReq(r, "POST", "/api/auth/login/totp", "", "",
		fmt.Sprintf(`{"ticket":%q,"code":%q}`, ticket, strings.ToLower(codes[0])))
	mustStatus(t, w, http.StatusOK)
	if err := db.Where("username = ?", "admin").First(&u).Error; err != nil {
		t.Fatalf("reload admin: %v", err)
	}
	var after []string
	if err := json.Unmarshal([]byte(u.RecoveryCodes), &after); err != nil || len(after) != recoveryCodeCount-1 {
		t.Fatalf("recovery code must be consumed: %q err=%v", u.RecoveryCodes, err)
	}
	ticket = loginStep1(t, r, "admin", "admin123")
	if w := doReq(r, "POST", "/api/auth/login/totp", "", "",
		fmt.Sprintf(`{"ticket":%q,"code":%q}`, ticket, codes[0])); w.Code != http.StatusUnauthorized {
		t.Fatalf("reused recovery code must fail: %d", w.Code)
	}
	ticket = loginStep1(t, r, "admin", "admin123")
	w = doReq(r, "POST", "/api/auth/login/totp", "", "",
		fmt.Sprintf(`{"ticket":%q,"code":%q}`, ticket, codes[1]))
	mustStatus(t, w, http.StatusOK)

	// secret 与恢复码明文不进任何 GET 响应
	for _, path := range []string{"/api/users", "/api/auth/me", "/api/settings", "/api/logs"} {
		w := doReq(r, "GET", path, sessionCookie.Value, "", "")
		mustStatus(t, w, http.StatusOK)
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("%s leaks totp secret: %s", path, w.Body.String())
		}
		for _, rc := range codes {
			if strings.Contains(w.Body.String(), rc) {
				t.Fatalf("%s leaks recovery code: %s", path, w.Body.String())
			}
		}
	}
}

// TestTotpTicketSecurity 票据与会话双向隔离：过期/伪造/会话 cookie 当票据用
// 一律 401（统一登录失败文案）；票据当会话 cookie 用同样 401。
func TestTotpTicketSecurity(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	secret, _ := setupTotp(t, r, cookie)
	_, fresh := enableTotp(t, r, cookie, secret)
	ticket := loginStep1(t, r, "admin", "admin123")

	var st model.Setting
	if err := db.First(&st, 1).Error; err != nil {
		t.Fatalf("setting row: %v", err)
	}
	code := totpCode(t, secret)
	attempt := func(tkt string) int {
		return doReq(r, "POST", "/api/auth/login/totp", "", "",
			fmt.Sprintf(`{"ticket":%q,"code":%q}`, tkt, code)).Code
	}

	// 过期票据（签名合法、超 5 分钟）
	expired := signTotpTicket(st.SessionSecret, "admin", time.Now().Add(-2*totpTicketTTL))
	if got := attempt(expired); got != http.StatusUnauthorized {
		t.Fatalf("expired ticket: %d", got)
	}
	// 伪造票据（格式正确、签名错误）
	forged := strings.Replace(ticket, ticket[strings.LastIndex(ticket, "|")+1:], "deadbeef", 1)
	if got := attempt(forged); got != http.StatusUnauthorized {
		t.Fatalf("forged ticket: %d", got)
	}
	// 会话 cookie 当票据用（签名合法的 v2 会话）→ 拒绝
	if got := attempt(fresh); got != http.StatusUnauthorized {
		t.Fatalf("session cookie as ticket: %d", got)
	}
	// 票据当会话 cookie 用（GET /api/auth/me）→ 拒绝
	if got := doReq(r, "GET", "/api/auth/me", ticket, "", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("ticket as session: %d", got)
	}
	// 缺字段
	if got := doReq(r, "POST", "/api/auth/login/totp", "", "", `{"ticket":"","code":"123456"}`).Code; got != http.StatusUnauthorized {
		t.Fatalf("empty ticket: %d", got)
	}
	if got := doReq(r, "POST", "/api/auth/login/totp", "", "",
		fmt.Sprintf(`{"ticket":%q,"code":""}`, ticket)).Code; got != http.StatusUnauthorized {
		t.Fatalf("empty code: %d", got)
	}
}

// TestTotpDisable 解绑：密码错 400；密码对 → 三字段清空、纪元 +1、旧会话失效、
// 重签 cookie 有效；解绑后登录直接建会话（不再要求两步）。
func TestTotpDisable(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	secret, _ := setupTotp(t, r, cookie)
	_, fresh := enableTotp(t, r, cookie, secret)

	// 未启用账号解绑 → 400（bob 未绑定）
	mustStatus(t, doReq(r, "POST", "/api/users", fresh, "", `{"username":"bob","password":"secret1"}`), http.StatusOK)
	bobCookie := login(t, r, "bob", "secret1")
	if code := doReq(r, "POST", "/api/auth/totp/disable", bobCookie, "", `{"password":"secret1"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("disable when not enabled: %d", code)
	}

	// 密码错 → 400
	if w := doReq(r, "POST", "/api/auth/totp/disable", fresh, "", `{"password":"wrongpw"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "密码错误") {
		t.Fatalf("disable wrong password: %d %s", w.Code, w.Body.String())
	}

	// 密码对 → 200；字段清空、纪元 +1、旧 cookie 失效、重签 cookie 有效
	w := doReq(r, "POST", "/api/auth/totp/disable", fresh, "", `{"password":"admin123"}`)
	mustStatus(t, w, http.StatusOK)
	newer := reSignedCookie(t, w)
	var u model.User
	if err := db.Where("username = ?", "admin").First(&u).Error; err != nil {
		t.Fatalf("reload admin: %v", err)
	}
	if u.TOTPEnabled || u.TOTPSecret != "" || u.RecoveryCodes != "" || u.SessionEpoch != 2 {
		t.Fatalf("after disable: enabled=%v secret=%q codes=%q epoch=%d",
			u.TOTPEnabled, u.TOTPSecret, u.RecoveryCodes, u.SessionEpoch)
	}
	if code := doReq(r, "GET", "/api/auth/me", fresh, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("pre-disable session after disable: %d", code)
	}
	mustStatus(t, doReq(r, "GET", "/api/auth/me", newer, "", ""), http.StatusOK)

	// 解绑后登录直接建会话
	w = doReq(r, "POST", "/api/auth/login", "", "", `{"username":"admin","password":"admin123"}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["totp_required"] == true || m["username"] != "admin" {
		t.Fatalf("login after disable: %v", m)
	}
}

// TestTotpLoginRateLimit 第二步复用登录限流：同 用户名+IP 连续 5 次错码后锁定，
// 第 6 次（即使码正确）一律 429。
func TestTotpLoginRateLimit(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	secret, _ := setupTotp(t, r, cookie)
	enableTotp(t, r, cookie, secret)
	ticket := loginStep1(t, r, "admin", "admin123")

	for i := 0; i < 5; i++ {
		w := doReq(r, "POST", "/api/auth/login/totp", "", "",
			fmt.Sprintf(`{"ticket":%q,"code":"000000"}`, ticket))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("step2 fail attempt %d: want 401 got %d", i+1, w.Code)
		}
	}
	// 第 6 次：锁定期内即使码正确也 429
	w := doReq(r, "POST", "/api/auth/login/totp", "", "",
		fmt.Sprintf(`{"ticket":%q,"code":%q}`, ticket, totpCode(t, secret)))
	body := mustStatus(t, w, http.StatusTooManyRequests)
	if !strings.Contains(body, "尝试次数过多") {
		t.Fatalf("step2 rate limited body: %s", body)
	}
}

// TestTotpSessionEpoch isolates: enable/disable 只失效本人会话（对齐 O10 按用户纪元），
// 其他用户会话不受影响。
func TestTotpEpochIsolation(t *testing.T) {
	r, _ := newTestEnv(t)
	adminCookie := login(t, r, "admin", "admin123")
	mustStatus(t, doReq(r, "POST", "/api/users", adminCookie, "", `{"username":"bob","password":"secret1"}`), http.StatusOK)
	bobCookie := login(t, r, "bob", "secret1")

	secret, _ := setupTotp(t, r, adminCookie)
	_, fresh := enableTotp(t, r, adminCookie, secret)

	// admin 纪元 +1（旧失效、重签有效）；bob 会话不受影响
	if code := doReq(r, "GET", "/api/auth/me", adminCookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("admin old session after enable: %d", code)
	}
	mustStatus(t, doReq(r, "GET", "/api/auth/me", fresh, "", ""), http.StatusOK)
	mustStatus(t, doReq(r, "GET", "/api/auth/me", bobCookie, "", ""), http.StatusOK)

	mustStatus(t, doReq(r, "POST", "/api/auth/totp/disable", fresh, "", `{"password":"admin123"}`), http.StatusOK)
	mustStatus(t, doReq(r, "GET", "/api/auth/me", bobCookie, "", ""), http.StatusOK)
}
