// session_epoch_test.go 按用户会话纪元（O10，SPEC §5）+ v3 会话 token（安全加固
// 三件套，C）：token v3 格式（v3|exp|username|user_epoch|session_id|sig）、按用户
// 隔离（重置只失效目标用户）、本人改密重签当前会话、旧版本 token（无 v3 前缀/
// 全局纪元格式）一律拒绝。
package api

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
)

// globalEpochToken 构造旧全局纪元格式（P2-8，v2 之前）的会话 token：
// "exp|username|epoch|sig"，签名合法但无 v3 前缀，升级后必须一律拒绝。
func globalEpochToken(secret, username string, epoch int, now time.Time) string {
	payload := fmt.Sprintf("%d|%s|%d", now.Add(sessionTTL).Unix(), username, epoch)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "|" + hex.EncodeToString(mac.Sum(nil))
}

// v2Token 构造 v2 格式 token（v2|exp|username|user_epoch|sig）：签名合法的存量
// 版本，v3 上线后按版本前缀机制一律拒绝（对齐 O10 先例，重新登录一次即可）。
func v2Token(secret, username string, epoch int, now time.Time) string {
	payload := fmt.Sprintf("v2|%d|%s|%d", now.Add(sessionTTL).Unix(), username, epoch)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "|" + hex.EncodeToString(mac.Sum(nil))
}

// mintSession 直接在注册表造一行活跃会话并签发对应 v3 token（构造「合法签名 +
// 注册表在行」的最小态，供纪元不匹配/过期等用例使用——正常路径一律走 login 写表）。
func mintSession(t *testing.T, db *gorm.DB, username string, epoch int, now time.Time) string {
	t.Helper()
	var st model.Setting
	if err := db.First(&st, 1).Error; err != nil {
		t.Fatalf("setting row: %v", err)
	}
	sid := newSessionIDForTest(t)
	row := model.Session{
		ID: sid, Username: username, IP: "127.0.0.1", UserAgent: "mint-test",
		CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(sessionTTL),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("create session row: %v", err)
	}
	return signSession(st.SessionSecret, username, epoch, sid, now)
}

// newSessionIDForTest 会话 ID 生成（与 createSession 同口径的 32 位 hex）。
func newSessionIDForTest(t *testing.T) string {
	t.Helper()
	sid, err := randomHex(16)
	if err != nil {
		t.Fatalf("random session id: %v", err)
	}
	return sid
}

// reSignedCookie 从改密响应中取出重签的会话 cookie（O10：当前会话保持登录的载体）。
func reSignedCookie(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var fresh *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			fresh = c
		}
	}
	if fresh == nil || fresh.Value == "" {
		t.Fatalf("password change must re-sign session cookie")
	}
	return fresh.Value
}

// rawCookieValue 会话 cookie 原始值（gin SetCookie 经 url.QueryEscape 写入，
// httptest 响应里读到的 value 是转义形态；结构断言前先还原）。
func rawCookieValue(t *testing.T, v string) string {
	t.Helper()
	out, err := url.QueryUnescape(v)
	if err != nil {
		t.Fatalf("unescape cookie: %v", err)
	}
	return out
}

// TestLoginWritesSessionRegistry 登录写注册表：cookie 为合法 v3 格式（5 段载荷 +
// 签名），且 sessions 表新增一行（属主一致、未吊销、未过期、IP/UA 快照）。
func TestLoginWritesSessionRegistry(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := rawCookieValue(t, login(t, r, "admin", "admin123"))

	// v3 格式：v3|exp|username|epoch|session_id|sig（6 段）
	parts := strings.Split(cookie, "|")
	if len(parts) != 6 || parts[0] != "v3" {
		t.Fatalf("cookie 应为 v3 六段格式，got %d 段: %q", len(parts), cookie)
	}
	if parts[2] != "admin" {
		t.Errorf("username 段 = %q, want admin", parts[2])
	}
	sid := parts[4]
	var row model.Session
	if err := db.First(&row, "id = ?", sid).Error; err != nil {
		t.Fatalf("sessions 表缺少登录会话行: %v", err)
	}
	if row.Username != "admin" || row.RevokedAt != nil {
		t.Errorf("注册表行不匹配: %+v", row)
	}
	if row.ExpiresAt.Before(time.Now()) {
		t.Error("expires_at 应在未来")
	}
	mustStatus(t, doReq(r, "GET", "/api/auth/me", cookie, "", ""), http.StatusOK)
}

// TestPerUserEpochIsolation 按用户隔离：管理员重置 B 的密码后，B 的旧会话 401、
// 操作者（管理员）会话不受影响、B 可用新密码重新登录。
func TestPerUserEpochIsolation(t *testing.T) {
	r, db := newTestEnv(t)
	adminCookie := login(t, r, "admin", "admin123")
	mustStatus(t, doReq(r, "POST", "/api/users", adminCookie, "",
		`{"username":"bob","password":"secret1"}`), http.StatusOK)
	bobCookie := login(t, r, "bob", "secret1")
	mustStatus(t, doReq(r, "GET", "/api/auth/me", bobCookie, "", ""), http.StatusOK)

	// 重置 bob（id=2）密码：仅 bob 的纪元 +1
	mustStatus(t, doReq(r, "PUT", "/api/users/2/password", adminCookie, "",
		`{"new_password":"newpass1"}`), http.StatusOK)
	var bob model.User
	if err := db.Where("username = ?", "bob").First(&bob).Error; err != nil {
		t.Fatalf("load bob: %v", err)
	}
	if bob.SessionEpoch != 1 {
		t.Fatalf("bob epoch must bump to 1, got %d", bob.SessionEpoch)
	}
	// B 旧会话 401
	if code := doReq(r, "GET", "/api/auth/me", bobCookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("bob old session after reset: %d", code)
	}
	// 操作者会话仍有效（全局纪元时代会被误踢）
	mustStatus(t, doReq(r, "GET", "/api/auth/me", adminCookie, "", ""), http.StatusOK)
	// B 用新密码重新登录可用
	bobCookie = login(t, r, "bob", "newpass1")
	mustStatus(t, doReq(r, "GET", "/api/auth/me", bobCookie, "", ""), http.StatusOK)
}

// TestChangeOwnPasswordResignsSession 本人改密：当前会话经响应重签的新 cookie 保持
// 登录（注册表行保留原 session_id 并顺延 expires_at）；同用户其他会话（旧纪元
// token）与改密前的旧 cookie 全部失效；纪元确实 +1；API Key 不动（联动只发生在
// 管理员重置他人密码）。
func TestChangeOwnPasswordResignsSession(t *testing.T) {
	r, db := newTestEnv(t)
	adminCookie := login(t, r, "admin", "admin123")
	otherCookie := login(t, r, "admin", "admin123") // 同用户第二会话
	mustStatus(t, doReq(r, "GET", "/api/auth/me", otherCookie, "", ""), http.StatusOK)

	w := doReq(r, "PUT", "/api/auth/password", adminCookie, "",
		`{"old_password":"admin123","new_password":"newpass1"}`)
	mustStatus(t, w, http.StatusOK)
	fresh := rawCookieValue(t, reSignedCookie(t, w))

	// 重签的新 cookie 立即可用（当前会话保持登录，文案承诺的行为）
	m := decode(t, mustStatus(t, doReq(r, "GET", "/api/auth/me", fresh, "", ""), http.StatusOK))
	if m["username"] != "admin" {
		t.Fatalf("re-signed session username: %v", m["username"])
	}
	// 重签保留原 session_id（注册表行仍是同一行）
	freshParts := strings.Split(fresh, "|")
	oldParts := strings.Split(rawCookieValue(t, adminCookie), "|")
	if freshParts[4] != oldParts[4] {
		t.Fatalf("重签必须保留原 session_id: %q vs %q", freshParts[4], oldParts[4])
	}
	var row model.Session
	if err := db.First(&row, "id = ?", freshParts[4]).Error; err != nil {
		t.Fatalf("registry row missing: %v", err)
	}
	if row.RevokedAt != nil {
		t.Error("当前会话注册表行不应被吊销")
	}
	if row.ExpiresAt.Before(time.Now()) {
		t.Error("重签后注册表 expires_at 应顺延到未来")
	}
	// 同用户其他会话（旧纪元）401
	if code := doReq(r, "GET", "/api/auth/me", otherCookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("other session of same user after change: %d", code)
	}
	// 改密前携带的旧 cookie 同样 401
	if code := doReq(r, "GET", "/api/auth/me", adminCookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("pre-change session after change: %d", code)
	}
	// 本人纪元 +1
	var admin model.User
	if err := db.Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatalf("load admin: %v", err)
	}
	if admin.SessionEpoch != 1 {
		t.Fatalf("admin epoch must bump to 1, got %d", admin.SessionEpoch)
	}
}

// TestLegacyTokenFormatsRejected 旧版本 token 一律拒绝：无 v3 前缀的三种历史格式
// （v2 的 "v2|exp|username|user_epoch|sig"、P2-8 之前的 "exp|username|sig" 与全局
// 纪元 "exp|username|epoch|sig"）即使签名合法也无效；v3 token 的纪元不匹配或
// 已过期同样 401。
func TestLegacyTokenFormatsRejected(t *testing.T) {
	r, db := newTestEnv(t)
	var st model.Setting
	if err := db.First(&st, 1).Error; err != nil {
		t.Fatalf("setting row: %v", err)
	}
	cookie := login(t, r, "admin", "admin123")
	mustStatus(t, doReq(r, "GET", "/api/auth/me", cookie, "", ""), http.StatusOK)

	now := time.Now()
	// v3 畸形：session_id 在注册表不存在（伪造 ID 段）→ 401
	forged := signSession(st.SessionSecret, "admin", 0, "no-such-session-id", now)
	for name, tok := range map[string]string{
		// v2 存量格式（签名合法）：版本前缀机制一律拒绝（安全加固三件套，C）
		"v2 format": v2Token(st.SessionSecret, "admin", 0, now),
		// P2-8 之前：无 epoch 段
		"pre-p2-8 format": legacySessionToken(st.SessionSecret, "admin"),
		// 全局纪元格式：纪元取旧实现种子的首值 1（Setting.SessionEpoch 已删）；
		// 拒绝与否与取值无关——缺 v3 前缀即一律无效
		"global epoch format": globalEpochToken(st.SessionSecret, "admin", 1, now),
		// v3 但注册表无行（伪造 session_id）
		"v3 with unknown session": forged,
	} {
		if code := doReq(r, "GET", "/api/auth/me", tok, "", "").Code; code != http.StatusUnauthorized {
			t.Fatalf("%s token must be rejected: %d", name, code)
		}
	}

	var admin model.User
	if err := db.Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatalf("load admin: %v", err)
	}
	// v3 token（注册表在行）：纪元不匹配（如改密/重置后仍持旧 token）→ 401
	stale := mintSession(t, db, "admin", admin.SessionEpoch+1, now)
	if code := doReq(r, "GET", "/api/auth/me", stale, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("stale epoch v3 token must be rejected: %d", code)
	}
	// v3 token（注册表在行）：token 已过期 → 401
	expired := mintSession(t, db, "admin", admin.SessionEpoch, now.Add(-2*sessionTTL))
	if code := doReq(r, "GET", "/api/auth/me", expired, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("expired v3 token must be rejected: %d", code)
	}
}
