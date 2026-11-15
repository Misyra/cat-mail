// log_body_test.go A4 日志正文隐私开关集成测试：
//   - log_body_enabled=false：/v1/send 与渠道测试直发落库的 SendLog 正文
//     （HTMLBody/TextBody）存空串，元数据（to/subject/from/状态/错误/耗时等）
//     照常完整；且只影响新日志，关闭前已落正文的历史行原样保留；
//   - 默认（true）：回归现状，正文照常落库；经 PUT 重新开启后恢复记录；
//   - PUT /api/settings 对 log_body_enabled 的校验往返（GET→PUT→GET 一致）。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gorm.io/gorm"

	"catmail/internal/model"
)

// setLogBodyEnabled 直改设置行的 log_body_enabled（测试铺底，绕过 PUT 的全量语义）。
func setLogBodyEnabled(t *testing.T, db *gorm.DB, enabled bool) {
	t.Helper()
	if err := db.Model(&model.Setting{}).Where("id = ?", 1).
		Update("log_body_enabled", enabled).Error; err != nil {
		t.Fatalf("set log_body_enabled: %v", err)
	}
}

// fetchSendLog 按 message_id 取日志行。
func fetchSendLog(t *testing.T, db *gorm.DB, messageID string) model.SendLog {
	t.Helper()
	var lg model.SendLog
	if err := db.Where("message_id = ?", messageID).First(&lg).Error; err != nil {
		t.Fatalf("send log missing (message_id=%s): %v", messageID, err)
	}
	return lg
}

// htmlTextSendBody 带 html+text 双正文的发送请求体。
func htmlTextSendBody() string {
	return `{"from":"CatMail <noreply@example.com>","to":["a@example.com"],` +
		`"subject":"Hello","html":"<p>Hi HTML</p>","text":"Hi TEXT"}`
}

// TestSendLogBodyDisabled 开关 off：新日志正文为空、元数据完整、历史行保留；测试日志同样。
func TestSendLogBodyDisabled(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	// from_email 必填：测试直发（/api/channels/:id/test）要求渠道配置默认发件人
	chID := createChannel(t, r, cookie,
		`{"name":"mock主渠道","provider":"mock","priority":-10,"from_email":"noreply@example.com","from_name":"CatMail"}`)
	key := createKey(t, r, cookie, "测试Key")

	// 开关开启时先落一条带正文的历史日志
	w := doReq(r, "POST", "/v1/send", "", key, htmlTextSendBody())
	m := decode(t, mustStatus(t, w, http.StatusOK))
	histMsgID, _ := m["id"].(string)
	hist := fetchSendLog(t, db, histMsgID)
	if hist.HTMLBody != "<p>Hi HTML</p>" || hist.TextBody != "Hi TEXT" {
		t.Fatalf("pre-toggle log body: html=%q text=%q", hist.HTMLBody, hist.TextBody)
	}

	// 关闭开关后再发：新日志正文应为空
	setLogBodyEnabled(t, db, false)
	w = doReq(r, "POST", "/v1/send", "", key, htmlTextSendBody())
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["accepted"] != true {
		t.Fatalf("accepted: %v", m)
	}
	msgID, _ := m["id"].(string)
	lg := fetchSendLog(t, db, msgID)
	if lg.HTMLBody != "" || lg.TextBody != "" {
		t.Fatalf("disabled log body not blanked: html=%q text=%q", lg.HTMLBody, lg.TextBody)
	}
	// 元数据照常完整
	if lg.Subject != "Hello" || lg.FromAddr != "noreply@example.com" {
		t.Fatalf("disabled log subject/from: %q / %q", lg.Subject, lg.FromAddr)
	}
	if !strings.Contains(lg.ToAddrs, "a@example.com") {
		t.Fatalf("disabled log to_addrs: %s", lg.ToAddrs)
	}
	if lg.Status != model.SendStatusSent || lg.IsTest {
		t.Fatalf("disabled log status/is_test: %d / %v", lg.Status, lg.IsTest)
	}
	if lg.ChannelID == 0 || lg.ApiKeyID == 0 || lg.MessageID == "" || lg.ProviderMessageID == "" {
		t.Fatalf("disabled log metadata incomplete: ch=%d key=%d mid=%q pmid=%q",
			lg.ChannelID, lg.ApiKeyID, lg.MessageID, lg.ProviderMessageID)
	}
	if lg.CreatedAt.IsZero() || lg.LatencyMs < 0 {
		t.Fatalf("disabled log time/latency: %v / %d", lg.CreatedAt, lg.LatencyMs)
	}
	// 只影响新日志：历史行原样保留（刻意取舍：关开关不承诺抹除历史）
	histAfter := fetchSendLog(t, db, histMsgID)
	if histAfter.HTMLBody != "<p>Hi HTML</p>" || histAfter.TextBody != "Hi TEXT" {
		t.Fatalf("historical log body altered: html=%q text=%q", histAfter.HTMLBody, histAfter.TextBody)
	}

	// 测试直发日志同样置空正文（元数据照常）
	w = doReq(r, "POST", fmt.Sprintf("/api/channels/%g/test", chID), cookie, "", `{"to":"dest@example.com"}`)
	mustStatus(t, w, http.StatusOK)
	var testLog model.SendLog
	if err := db.Where("is_test = ?", true).Order("id DESC").First(&testLog).Error; err != nil {
		t.Fatalf("test log missing: %v", err)
	}
	if testLog.HTMLBody != "" || testLog.TextBody != "" {
		t.Fatalf("disabled test log body not blanked: html=%q text=%q", testLog.HTMLBody, testLog.TextBody)
	}
	if testLog.Status != model.SendStatusSent || testLog.ToAddrs != `["dest@example.com"]` ||
		testLog.Subject != "CatMail 渠道测试" {
		t.Fatalf("disabled test log metadata: status=%d to=%s subject=%q",
			testLog.Status, testLog.ToAddrs, testLog.Subject)
	}
}

// TestSendLogBodyEnabled 默认/重新开启：正文照常落库（回归现状）。
func TestSendLogBodyEnabled(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mock主渠道","provider":"mock","priority":-10}`)
	key := createKey(t, r, cookie, "测试Key")

	// 默认（种子开关开启）：现状回归，双正文完整落库
	w := doReq(r, "POST", "/v1/send", "", key, htmlTextSendBody())
	m := decode(t, mustStatus(t, w, http.StatusOK))
	msgID, _ := m["id"].(string)
	lg := fetchSendLog(t, db, msgID)
	if lg.HTMLBody != "<p>Hi HTML</p>" || lg.TextBody != "Hi TEXT" {
		t.Fatalf("default log body: html=%q text=%q", lg.HTMLBody, lg.TextBody)
	}

	// off → 经 PUT 重新开启 → 新日志恢复记录正文
	setLogBodyEnabled(t, db, false)
	w = doReq(r, "POST", "/v1/send", "", key, htmlTextSendBody())
	m = decode(t, mustStatus(t, w, http.StatusOK))
	offMsgID, _ := m["id"].(string)
	if lg := fetchSendLog(t, db, offMsgID); lg.HTMLBody != "" || lg.TextBody != "" {
		t.Fatalf("off log body: html=%q text=%q", lg.HTMLBody, lg.TextBody)
	}

	w = doReq(r, "PUT", "/api/settings", cookie, "",
		`{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,`+
			`"consecutive_failure_threshold":5,"log_retention_days":90,`+
			`"notify_inapp_enabled":false,"notify_webhook_enabled":false,`+
			`"notify_webhook_url":"","notify_events":"","log_body_enabled":true}`)
	mustStatus(t, w, http.StatusOK)

	w = doReq(r, "POST", "/v1/send", "", key, htmlTextSendBody())
	m = decode(t, mustStatus(t, w, http.StatusOK))
	reMsgID, _ := m["id"].(string)
	if lg := fetchSendLog(t, db, reMsgID); lg.HTMLBody != "<p>Hi HTML</p>" || lg.TextBody != "Hi TEXT" {
		t.Fatalf("re-enabled log body: html=%q text=%q", lg.HTMLBody, lg.TextBody)
	}
}

// TestSettingsLogBodyRoundtrip PUT /api/settings 对 log_body_enabled 的往返：
// GET 默认 true（种子行为）→ PUT 翻转 → GET 一致 → 再翻转回 true。
func TestSettingsLogBodyRoundtrip(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 种子设置行 log_body_enabled 默认 true（gorm default:true，存量库升级行为不变）
	var st model.Setting
	if err := db.First(&st, 1).Error; err != nil {
		t.Fatalf("seed setting missing: %v", err)
	}
	if !st.LogBodyEnabled {
		t.Fatalf("seed log_body_enabled: want true, got false")
	}

	w := doReq(r, "GET", "/api/settings", cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["log_body_enabled"] != true {
		t.Fatalf("GET default log_body_enabled: %v", m["log_body_enabled"])
	}

	// PUT 关闭：响应回显且落库
	putBody := `{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,` +
		`"consecutive_failure_threshold":5,"log_retention_days":90,` +
		`"notify_inapp_enabled":false,"notify_webhook_enabled":false,` +
		`"notify_webhook_url":"","notify_events":"","log_body_enabled":false}`
	w = doReq(r, "PUT", "/api/settings", cookie, "", putBody)
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["log_body_enabled"] != false {
		t.Fatalf("PUT echo log_body_enabled: %v", m["log_body_enabled"])
	}
	var st2 model.Setting
	if err := db.First(&st2, 1).Error; err != nil {
		t.Fatalf("reload setting: %v", err)
	}
	if st2.LogBodyEnabled {
		t.Fatalf("PUT log_body_enabled not persisted: got true")
	}
	w = doReq(r, "GET", "/api/settings", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["log_body_enabled"] != false {
		t.Fatalf("GET after PUT log_body_enabled: %v", m["log_body_enabled"])
	}

	// PUT 重新开启：往返一致
	w = doReq(r, "PUT", "/api/settings", cookie, "",
		strings.Replace(putBody, `"log_body_enabled":false`, `"log_body_enabled":true`, 1))
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["log_body_enabled"] != true {
		t.Fatalf("PUT re-enable echo: %v", m["log_body_enabled"])
	}
	w = doReq(r, "GET", "/api/settings", cookie, "", "")
	m = decode(t, mustStatus(t, w, http.StatusOK))
	if m["log_body_enabled"] != true {
		t.Fatalf("GET after re-enable: %v", m["log_body_enabled"])
	}

	// JSON 形态冒烟：log_body_enabled 必须出现在 GET 响应键中（前端类型契约）
	keys := make(map[string]bool)
	for k := range m {
		keys[k] = true
	}
	if !keys["log_body_enabled"] {
		b, _ := json.Marshal(m)
		t.Fatalf("GET response missing log_body_enabled key: %s", b)
	}
}
