// sessions_test.go 会话管理端点测试（安全加固三件套，C）：活跃会话列表（当前
// 标记）、踢指定会话（当前会话保护）、下线其余全部、登出吊销、密码重置联动
// 三件套（纪元 + 会话吊销 + Key 停用/启用清除标记）与审计落行。
package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gorm.io/gorm"

	"catmail/internal/model"
)

// assertAudit 断言审计表中存在指定动作、且 detail（或 target）包含指定子串的行。
func assertAudit(t *testing.T, db *gorm.DB, action, detailContains string) {
	t.Helper()
	var rows []model.AuditLog
	if err := db.Where("action = ?", action).Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("query audit %s: %v", action, err)
	}
	if len(rows) == 0 {
		t.Fatalf("审计缺少动作 %s 的记录", action)
	}
	if detailContains == "" {
		return
	}
	for _, row := range rows {
		if strings.Contains(row.Detail, detailContains) {
			return
		}
	}
	t.Fatalf("审计动作 %s 存在但 detail 均不含 %q: %+v", action, detailContains, rows)
}

// sessionsOf 查询某用户的活跃会话 ID 列表（排查断言用）。
func sessionsOf(t *testing.T, db *gorm.DB, username string) []string {
	t.Helper()
	var ids []string
	if err := db.Model(&model.Session{}).
		Where("username = ? AND revoked_at IS NULL", username).
		Order("id").Pluck("id", &ids).Error; err != nil {
		t.Fatalf("query sessions: %v", err)
	}
	return ids
}

// TestSessionListAndCurrentMark 会话列表：两个登录 → 两条活跃会话且恰一条
// current=true；字段齐全。
func TestSessionListAndCurrentMark(t *testing.T) {
	r, _ := newTestEnv(t)
	c1 := login(t, r, "admin", "admin123")
	c2 := login(t, r, "admin", "admin123")

	m := decode(t, mustStatus(t, doReq(r, "GET", "/api/sessions", c1, "", ""), http.StatusOK))
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2（两次登录各一条）", len(items))
	}
	currentCount := 0
	var currentID string
	for _, it := range items {
		row := it.(map[string]any)
		if row["current"].(bool) {
			currentCount++
			currentID = row["id"].(string)
		}
		if row["ip"] == nil || row["user_agent"] == nil || row["created_at"] == nil || row["last_seen_at"] == nil {
			t.Errorf("列表项字段缺失: %v", row)
		}
	}
	if currentCount != 1 {
		t.Fatalf("current 标记数 = %d, want 1", currentCount)
	}
	// c2 的列表当前标记指向 c2 自己（不同 cookie，同一用户名）
	m2 := decode(t, mustStatus(t, doReq(r, "GET", "/api/sessions", c2, "", ""), http.StatusOK))
	for _, it := range m2["items"].([]any) {
		row := it.(map[string]any)
		if row["current"].(bool) && row["id"].(string) == currentID {
			t.Error("c2 视角的当前会话不应是 c1 的")
		}
	}
}

// TestKickSession 当前会话保护与踢出：DELETE 当前 → 400；DELETE 其他 → 吊销生效
// （旧 cookie 401、列表不再含该行）；重复踢/踢不存在 → 404；不能踢他人会话（404
// 不探测存在性）。
func TestKickSession(t *testing.T) {
	r, db := newTestEnv(t)
	admin := login(t, r, "admin", "admin123")
	mustStatus(t, doReq(r, "POST", "/api/users", admin, "",
		`{"username":"bob","password":"secret1"}`), http.StatusOK)
	bob := login(t, r, "bob", "secret1")

	adminSessions := decode(t, mustStatus(t, doReq(r, "GET", "/api/sessions", admin, "", ""), http.StatusOK))
	items := adminSessions["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("admin sessions = %d, want 1", len(items))
	}
	currentID := items[0].(map[string]any)["id"].(string)

	// 不能踢当前会话
	w := doReq(r, "DELETE", "/api/sessions/"+currentID, admin, "", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("kick current: status=%d body=%s", w.Code, w.Body.String())
	}

	// bob 第二会话（另一浏览器）：从 bob 的列表视角，bob2 的会话即「其他会话」
	bob2 := login(t, r, "bob", "secret1")
	bobItems := decode(t, mustStatus(t, doReq(r, "GET", "/api/sessions", bob, "", ""), http.StatusOK))["items"].([]any)
	var otherID string
	for _, it := range bobItems {
		row := it.(map[string]any)
		if !row["current"].(bool) {
			otherID = row["id"].(string)
		}
	}
	if otherID == "" {
		t.Fatal("应存在 bob 的非当前会话")
	}
	// admin 不能踢 bob 的会话（不探测存在性，统一 404）
	if w := doReq(r, "DELETE", "/api/sessions/"+otherID, admin, "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("kick other user's session: status=%d, want 404", w.Code)
	}
	// bob 踢自己的其他会话（bob2 所属）：成功 → bob2 的 cookie 立即 401，
	// bob 的当前会话保持
	mustStatus(t, doReq(r, "DELETE", "/api/sessions/"+otherID, bob, "", ""), http.StatusOK)
	if code := doReq(r, "GET", "/api/auth/me", bob2, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("kicked session must be 401: %d", code)
	}
	mustStatus(t, doReq(r, "GET", "/api/auth/me", bob, "", ""), http.StatusOK)
	// 重复踢同一会话 → 404（已吊销）
	if w := doReq(r, "DELETE", "/api/sessions/"+otherID, bob, "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("re-kick revoked session: status=%d, want 404", w.Code)
	}
	// 踢不存在 → 404
	if w := doReq(r, "DELETE", "/api/sessions/no-such-id", bob, "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("kick missing session: status=%d, want 404", w.Code)
	}
	// bob 活跃会话只剩当前一条
	if got := sessionsOf(t, db, "bob"); len(got) != 1 {
		t.Fatalf("bob active sessions = %v, want 仅当前 1 条", got)
	}
	// 审计：sessions_revoke（action=kick）
	assertAudit(t, db, model.AuditSessionRevoke, "action=kick")
}

// TestRevokeOtherSessions revoke-others：其余全部吊销、当前保持；重复调用幂等
// （revoked=0）；不影响 API Key。
func TestRevokeOtherSessions(t *testing.T) {
	r, db := newTestEnv(t)
	c1 := login(t, r, "admin", "admin123")
	c2 := login(t, r, "admin", "admin123")
	c3 := login(t, r, "admin", "admin123")

	// admin 建一个 Key：下线会话不影响 Key（联动仅密码重置触发）
	mustStatus(t, doReq(r, "POST", "/api/keys", c1, "", `{"name":"集成Key"}`), http.StatusOK)

	m := decode(t, mustStatus(t, doReq(r, "POST", "/api/sessions/revoke-others", c2, "", ""), http.StatusOK))
	if got := int(m["revoked"].(float64)); got != 2 {
		t.Fatalf("revoked = %d, want 2", got)
	}
	// c1/c3 401，c2 保持
	if code := doReq(r, "GET", "/api/auth/me", c1, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("c1 after revoke-others: %d", code)
	}
	if code := doReq(r, "GET", "/api/auth/me", c3, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("c3 after revoke-others: %d", code)
	}
	mustStatus(t, doReq(r, "GET", "/api/auth/me", c2, "", ""), http.StatusOK)
	// 幂等：再调 revoked=0
	m = decode(t, mustStatus(t, doReq(r, "POST", "/api/sessions/revoke-others", c2, "", ""), http.StatusOK))
	if got := int(m["revoked"].(float64)); got != 0 {
		t.Fatalf("repeat revoked = %d, want 0", got)
	}
	// API Key 不受影响
	keys := decode(t, mustStatus(t, doReq(r, "GET", "/api/keys", c2, "", ""), http.StatusOK))
	item := keys["items"].([]any)[0].(map[string]any)
	if !item["enabled"].(bool) {
		t.Error("revoke-others 不应停用 API Key")
	}
	// 审计：sessions_revoke（action=revoke_others）
	assertAudit(t, db, model.AuditSessionRevoke, "action=revoke_others")
}

// TestLogoutRevokesSession 登出吊销注册表行：登出后旧 cookie 401（服务端即时失效）。
func TestLogoutRevokesSession(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	mustStatus(t, doReq(r, "GET", "/api/auth/me", cookie, "", ""), http.StatusOK)
	mustStatus(t, doReq(r, "POST", "/api/auth/logout", cookie, "", ""), http.StatusOK)
	if code := doReq(r, "GET", "/api/auth/me", cookie, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("cookie after logout: %d, want 401", code)
	}
	if got := sessionsOf(t, db, "admin"); len(got) != 0 {
		t.Fatalf("登出后活跃会话 = %v, want 空", got)
	}
}

// TestPasswordResetLinkage 管理员重置他人密码联动三件套（安全加固三件套，C）：
// 同事务内纪元 +1、全部活跃会话吊销、该用户创建的全部 API Key 停用并带
// disabled_reason=password_reset；手动启用清除标记；审计落 password_reset_linkage；
// 操作者会话与「非目标用户创建」的 Key 不受影响。
func TestPasswordResetLinkage(t *testing.T) {
	r, db := newTestEnv(t)
	admin := login(t, r, "admin", "admin123")
	mustStatus(t, doReq(r, "POST", "/api/users", admin, "",
		`{"username":"bob","password":"secret1"}`), http.StatusOK)
	bob := login(t, r, "bob", "secret1")
	mustStatus(t, doReq(r, "GET", "/api/auth/me", bob, "", ""), http.StatusOK)

	// bob 创建两个 Key（应被联动停用）；admin 创建一个 Key（不应被联动停用）
	mustStatus(t, doReq(r, "POST", "/api/keys", bob, "", `{"name":"bob-key-1"}`), http.StatusOK)
	mustStatus(t, doReq(r, "POST", "/api/keys", bob, "", `{"name":"bob-key-2"}`), http.StatusOK)
	mustStatus(t, doReq(r, "POST", "/api/keys", admin, "", `{"name":"admin-key"}`), http.StatusOK)

	mustStatus(t, doReq(r, "PUT", "/api/users/2/password", admin, "",
		`{"new_password":"newpass1"}`), http.StatusOK)

	// 联动一：纪元 +1
	var bobUser model.User
	if err := db.Where("username = ?", "bob").First(&bobUser).Error; err != nil {
		t.Fatalf("load bob: %v", err)
	}
	if bobUser.SessionEpoch != 1 {
		t.Fatalf("bob epoch = %d, want 1", bobUser.SessionEpoch)
	}
	// 联动二：全部活跃会话吊销（旧 cookie 401）
	if code := doReq(r, "GET", "/api/auth/me", bob, "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("bob session after reset: %d, want 401", code)
	}
	if got := sessionsOf(t, db, "bob"); len(got) != 0 {
		t.Fatalf("bob active sessions = %v, want 空（联动吊销）", got)
	}
	// 联动三：bob 的 Key 全部停用 + disabled_reason=password_reset；admin 的 Key 不动
	keys := decode(t, mustStatus(t, doReq(r, "GET", "/api/keys", admin, "", ""), http.StatusOK))
	disabled, enabled, marked := 0, 0, 0
	for _, it := range keys["items"].([]any) {
		row := it.(map[string]any)
		name := row["name"].(string)
		switch name {
		case "bob-key-1", "bob-key-2":
			if row["enabled"].(bool) {
				t.Errorf("%s 应被联动停用", name)
			}
			disabled++
			if row["disabled_reason"] != model.ApiKeyDisabledByPasswordReset {
				t.Errorf("%s disabled_reason = %v, want password_reset", name, row["disabled_reason"])
			} else {
				marked++
			}
		case "admin-key":
			if !row["enabled"].(bool) {
				t.Errorf("admin-key 不应被联动停用")
			}
			enabled++
		}
	}
	if disabled != 2 || enabled != 1 || marked != 2 {
		t.Fatalf("联动结果: disabled=%d enabled=%d marked=%d, want 2/1/2", disabled, enabled, marked)
	}

	// 被停用 Key 的发送 401（enabled=false 鉴权拒绝）
	var k model.ApiKey
	if err := db.Where("name = ?", "bob-key-1").First(&k).Error; err != nil {
		t.Fatalf("load bob key: %v", err)
	}
	if code := doReq(r, "POST", "/v1/send", "", k.Key,
		`{"from":"noreply@example.com","to":["a@example.com"],"text":"x"}`).Code; code != http.StatusUnauthorized {
		t.Fatalf("disabled key send: %d, want 401", code)
	}

	// 手动启用清除标记（管理页「密码重置后停用，可手动启用」后的行为）
	mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/keys/%d", k.ID), admin, "",
		`{"enabled":true}`), http.StatusOK)
	var fresh model.ApiKey
	if err := db.First(&fresh, k.ID).Error; err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if !fresh.Enabled || fresh.DisabledReason != "" {
		t.Errorf("手动启用后 enabled=%v disabled_reason=%q, want true/空", fresh.Enabled, fresh.DisabledReason)
	}

	// 审计：user_reset_password + password_reset_linkage 各一条
	assertAudit(t, db, model.AuditUserResetPassword, "")
	assertAudit(t, db, model.AuditPasswordResetLinkage, "sessions_revoked=1 api_keys_disabled=2")
}

// TestPasswordResetLinkageSelfResetNoLinkage 本人改密不受联动影响（口径：当前会话
// 保持、Key 不动）——resetUserPassword 直接对操作者自己时联动仍会触发（API 允许、
// 前端不提供入口），此处锁定 changeOwnPassword 路径的「Key 不动」承诺。
func TestPasswordResetLinkageSelfResetNoLinkage(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	mustStatus(t, doReq(r, "POST", "/api/keys", cookie, "", `{"name":"self-key"}`), http.StatusOK)

	w := doReq(r, "PUT", "/api/auth/password", cookie, "",
		`{"old_password":"admin123","new_password":"newpass1"}`)
	mustStatus(t, w, http.StatusOK)
	// 重签 cookie 可用（当前会话保持）
	mustStatus(t, doReq(r, "GET", "/api/auth/me", reSignedCookie(t, w), "", ""), http.StatusOK)
	// Key 未被联动停用
	var k model.ApiKey
	if err := db.Where("name = ?", "self-key").First(&k).Error; err != nil {
		t.Fatalf("load key: %v", err)
	}
	if !k.Enabled || k.DisabledReason != "" {
		t.Errorf("本人改密不应停用 Key: enabled=%v reason=%q", k.Enabled, k.DisabledReason)
	}
}
