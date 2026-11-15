// handler_logs_resend_test.go E3 日志重发集成测试：
//   - 成功重发：按快照原样（from/to/subject/html/text）走轮换落一条新日志
//     （新 message_id、is_test=false、status=sent、走 mock 渠道），响应为发送成功体；
//   - 正文缺失（隐私开关落空正文/直接落库空行）→ 400「该日志未记录正文，无法重发」；
//   - 防滥用：同一日志 id 30 秒内二次重发 → 429 统一错误体；
//   - 未鉴权 401、日志不存在 404、非法 id 400。
package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// resetResendGate 清空进程内重发节流状态：各用例独立建库但日志 id 均从 1 起编号，
// 跨用例会命中同一 id 的 30s 节流窗口，故命中重发端点的用例先清空再断言。
func resetResendGate() {
	resendGate.Lock()
	resendGate.last = make(map[uint]time.Time)
	resendGate.Unlock()
}

// seedResendLog 登录 + mock 渠道 + key + 发一封带双正文的邮件，返回会话、内部
// message_id 与落库日志行（供重发断言快照一致性）。
func seedResendLog(t *testing.T, r *gin.Engine, db *gorm.DB) (cookie, msgID string, row model.SendLog) {
	t.Helper()
	cookie = login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mock重发渠道","provider":"mock","priority":-10,`+
		`"from_email":"noreply@example.com","from_name":"CatMail"}`)
	key := createKey(t, r, cookie, "重发Key")
	w := doReq(r, "POST", "/v1/send", "", key, htmlTextSendBody())
	m := decode(t, mustStatus(t, w, http.StatusOK))
	msgID, _ = m["id"].(string)
	if msgID == "" {
		t.Fatalf("send response missing id: %v", m)
	}
	row = fetchSendLog(t, db, msgID)
	return cookie, msgID, row
}

// TestLogsResendSuccess 成功重发：响应为发送成功体且 message_id 全新；
// 新日志按快照原样落库（正文/收件人/主题一致）、is_test=false、status=sent、
// 渠道为 mock（正常轮换），原日志行不受影响。
func TestLogsResendSuccess(t *testing.T) {
	resetResendGate()
	r, db := newTestEnv(t)
	cookie, msgID, orig := seedResendLog(t, r, db)

	w := doReq(r, "POST", fmt.Sprintf("/api/logs/%d/resend", orig.ID), cookie, "", "")
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["accepted"] != true || m["provider"] != "mock" || m["channel"] != "mock重发渠道" {
		t.Fatalf("resend resp: %v", m)
	}
	newMsgID, _ := m["id"].(string)
	if newMsgID == "" || newMsgID == msgID {
		t.Fatalf("resend must use a fresh message_id: orig=%s new=%q", msgID, newMsgID)
	}
	if pmid, _ := m["provider_message_id"].(string); !strings.HasPrefix(pmid, "mock-") {
		t.Fatalf("provider_message_id: %v", m["provider_message_id"])
	}

	var re model.SendLog
	if err := db.Where("message_id = ?", newMsgID).First(&re).Error; err != nil {
		t.Fatalf("resend log missing: %v", err)
	}
	if re.ID == orig.ID {
		t.Fatalf("resend must create a new log row, got same id %d", re.ID)
	}
	if re.IsTest || re.Status != model.SendStatusSent {
		t.Fatalf("resend log is_test/status: %v / %d", re.IsTest, re.Status)
	}
	// 快照原样：from/to/subject/html/text 与原日志一致
	if re.FromAddr != orig.FromAddr || re.Subject != orig.Subject {
		t.Fatalf("resend log from/subject: %q / %q", re.FromAddr, re.Subject)
	}
	if re.ToAddrs != orig.ToAddrs {
		t.Fatalf("resend log to_addrs: %q want %q", re.ToAddrs, orig.ToAddrs)
	}
	if re.HTMLBody != "<p>Hi HTML</p>" || re.TextBody != "Hi TEXT" {
		t.Fatalf("resend log body: html=%q text=%q", re.HTMLBody, re.TextBody)
	}
	if re.ChannelID != orig.ChannelID || re.ChannelName != orig.ChannelName {
		t.Fatalf("resend log channel: %d/%s want %d/%s",
			re.ChannelID, re.ChannelName, orig.ChannelID, orig.ChannelName)
	}
	// 原日志行不受影响
	after := fetchSendLog(t, db, msgID)
	if after != orig {
		t.Fatalf("original log altered: %+v want %+v", after, orig)
	}
}

// TestLogsResendNoBody 正文缺失（隐私开关落空正文/直接落库空行）→ 400 专用文案。
func TestLogsResendNoBody(t *testing.T) {
	resetResendGate()
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mock主渠道","provider":"mock","priority":-10}`)
	key := createKey(t, r, cookie, "k")

	// 关闭正文开关后发送：新日志正文为空（隐私开关历史形态）
	setLogBodyEnabled(t, db, false)
	w := doReq(r, "POST", "/v1/send", "", key, htmlTextSendBody())
	m := decode(t, mustStatus(t, w, http.StatusOK))
	blank := fetchSendLog(t, db, m["id"].(string))
	if blank.HTMLBody != "" || blank.TextBody != "" {
		t.Fatalf("blank log should have empty body: %+v", blank)
	}
	body := mustStatus(t, doReq(r, "POST", fmt.Sprintf("/api/logs/%d/resend", blank.ID), cookie, "", ""),
		http.StatusBadRequest)
	if !strings.Contains(body, "该日志未记录正文，无法重发") {
		t.Fatalf("no-body 400: %s", body)
	}

	// 直接落库的空正文行同样 400
	if err := db.Create(&model.SendLog{MessageID: "m-bare", ChannelID: 1, ChannelName: "mock主渠道",
		Provider: "mock", Status: model.SendStatusSent, ToAddrs: `["a@c.com"]`, Subject: "s"}).Error; err != nil {
		t.Fatalf("seed bare log: %v", err)
	}
	var bare model.SendLog
	db.Where("message_id = ?", "m-bare").First(&bare)
	body = mustStatus(t, doReq(r, "POST", fmt.Sprintf("/api/logs/%d/resend", bare.ID), cookie, "", ""),
		http.StatusBadRequest)
	if !strings.Contains(body, "该日志未记录正文，无法重发") {
		t.Fatalf("bare no-body 400: %s", body)
	}
}

// TestLogsResendRateLimit 防滥用：同一日志 id 30 秒内二次重发 → 429 统一错误体；
// 被拒的第二次不落新日志。
func TestLogsResendRateLimit(t *testing.T) {
	resetResendGate()
	r, db := newTestEnv(t)
	cookie, _, orig := seedResendLog(t, r, db)

	mustStatus(t, doReq(r, "POST", fmt.Sprintf("/api/logs/%d/resend", orig.ID), cookie, "", ""), http.StatusOK)
	body := mustStatus(t, doReq(r, "POST", fmt.Sprintf("/api/logs/%d/resend", orig.ID), cookie, "", ""),
		http.StatusTooManyRequests)
	if !strings.Contains(body, "30 秒") || !strings.Contains(body, `"type":"quota_exhausted"`) {
		t.Fatalf("second resend within 30s: %s", body)
	}
	var n int64
	db.Model(&model.SendLog{}).Count(&n)
	if n != 2 { // 原始 1 条 + 第一次重发 1 条
		t.Fatalf("log rows after rate-limited resend: %d want 2", n)
	}
}

// TestLogsResendAuthAndNotFound 未鉴权 401；日志不存在 404；非法 id 400。
func TestLogsResendAuthAndNotFound(t *testing.T) {
	resetResendGate()
	r, _ := newTestEnv(t)

	w := doReq(r, "POST", "/api/logs/1/resend", "", "", "")
	body := mustStatus(t, w, http.StatusUnauthorized)
	if !strings.Contains(body, `"type":"unauthorized"`) {
		t.Fatalf("unauth resend: %s", body)
	}
	cookie := login(t, r, "admin", "admin123")
	if code := doReq(r, "POST", "/api/logs/99999/resend", cookie, "", "").Code; code != http.StatusNotFound {
		t.Fatalf("missing log resend: %d", code)
	}
	if code := doReq(r, "POST", "/api/logs/abc/resend", cookie, "", "").Code; code != http.StatusBadRequest {
		t.Fatalf("invalid id: %d", code)
	}
}
