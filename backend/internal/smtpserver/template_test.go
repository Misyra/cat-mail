// template_test.go SMTP 入口的邮件模板支持测试：X-CatMail-Template 指令头
// 渲染替换 subject/html/text、变量 JSON/base64 两种形态、unknown_template 与
// body_conflict 的 501 映射、SendLog.template_id 落库。
package smtpserver

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"

	"gorm.io/gorm"

	"catmail/internal/model"
)

// createTemplate 建一个模板，返回其 ID。
func createTemplate(t *testing.T, db *gorm.DB, subject, html, text string) uint {
	t.Helper()
	tpl := model.Template{Name: fmt.Sprintf("tpl-%d", idSeq.Add(1)), Subject: subject, HTML: html, Text: text}
	if err := db.Create(&tpl).Error; err != nil {
		t.Fatalf("create template: %v", err)
	}
	return tpl.ID
}

// idSeq 模板名递增序号（避免重名）。
var idSeq = counter{}

// counter 极简原子递增（复用包内既有的并发原语依赖为零）。
type counter struct{ n int64 }

func (c *counter) Add(delta int64) int64 {
	c.n += delta
	return c.n
}

// sendRawData 完整 MAIL/RCPT/DATA 流程发送自定义 DATA 载荷（loopback 免认证），
// 返回最终应答错误（net/smtp.Data 的 Close 错误承载应答状态码）。
func sendRawData(t *testing.T, addr, from string, to []string, data string) error {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	c, err := smtp.NewClient(conn, "127.0.0.1")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Hello("client.local"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if err := c.Mail(from); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			t.Fatalf("RCPT %s: %v", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatalf("write data: %v", err)
	}
	return w.Close()
}

// smtpTemplateEnv 搭建「capture 渠道 + 模板」环境，返回 SMTP 地址与模板 ID。
func smtpTemplateEnv(t *testing.T, subject, html, text string) (string, uint, *gorm.DB) {
	t.Helper()
	resetCapture()
	db := newTestDB(t)
	if err := db.Create(&model.Channel{Name: "cap", Provider: "capture", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	id := createTemplate(t, db, subject, html, text)
	s := startServer(t, db, 0)
	return s.Addr(), id, db
}

// TestSMTPWithTemplate 指令头 + JSON 变量：渲染替换 subject/html/text（HTML
// 变量转义）、SendLog 落 template_id；报文自身主题/正文被整体替换。
func TestSMTPWithTemplate(t *testing.T) {
	addr, tplID, db := smtpTemplateEnv(t,
		"验证码通知 {{code}}",
		"<p>您好 {{name}}，验证码 {{code}}</p>",
		"您好 {{name}}，验证码 {{code}}")

	// 正文为空报文（模板与自带正文互斥，自带正文 → body_conflict，见冲突用例）
	err := sendRawData(t, addr, "sender@example.com", []string{"rcpt@example.com"},
		"From: sender@example.com\r\nTo: rcpt@example.com\r\nSubject: 占位草稿主题\r\n"+
			"X-CatMail-Template: "+fmt.Sprint(tplID)+"\r\n"+
			"X-CatMail-Template-Vars: {\"name\":\"张<b>三\",\"code\":\"123456\"}\r\n\r\n")
	if err != nil {
		t.Fatalf("DATA close: %v", err)
	}
	msg := lastCapture(t)
	if msg.Subject != "验证码通知 123456" {
		t.Fatalf("subject: %q", msg.Subject)
	}
	if msg.HTML != "<p>您好 张&lt;b&gt;三，验证码 123456</p>" {
		t.Fatalf("html: %q", msg.HTML)
	}
	if msg.Text != "您好 张<b>三，验证码 123456" {
		t.Fatalf("text: %q", msg.Text)
	}
	// SendLog 落渲染快照与模板 ID
	var lg model.SendLog
	if err := db.Model(&model.SendLog{}).Order("id DESC").First(&lg).Error; err != nil {
		t.Fatalf("send log: %v", err)
	}
	if lg.TemplateID != tplID || lg.Subject != "验证码通知 123456" {
		t.Fatalf("log: tpl=%d subject=%q", lg.TemplateID, lg.Subject)
	}
}

// TestSMTPTemplateVarsBase64 变量以 base64: 前缀携带（UTF-8 值的稳妥形态）。
func TestSMTPTemplateVarsBase64(t *testing.T) {
	addr, tplID, _ := smtpTemplateEnv(t, "hi {{v}}", "", "{{v}}")
	vars := base64.StdEncoding.EncodeToString([]byte(`{"v":"文本&值"}`))
	err := sendRawData(t, addr, "sender@example.com", []string{"rcpt@example.com"},
		"From: sender@example.com\r\nSubject: x\r\n"+
			"X-CatMail-Template: "+fmt.Sprint(tplID)+"\r\n"+
			"X-CatMail-Template-Vars: base64:"+vars+"\r\n\r\n")
	if err != nil {
		t.Fatalf("DATA close: %v", err)
	}
	msg := lastCapture(t)
	if msg.Subject != "hi 文本&值" || msg.Text != "文本&值" {
		t.Fatalf("subject=%q text=%q", msg.Subject, msg.Text)
	}
}

// TestSMTPTemplateMissingVars 未提供的变量：占位符留空、不阻断发送。
func TestSMTPTemplateMissingVars(t *testing.T) {
	addr, tplID, _ := smtpTemplateEnv(t, "s{{a}}{{b}}", "", "{{a}}")
	err := sendRawData(t, addr, "sender@example.com", []string{"rcpt@example.com"},
		"From: sender@example.com\r\nSubject: x\r\n"+
			"X-CatMail-Template: "+fmt.Sprint(tplID)+"\r\n"+
			"X-CatMail-Template-Vars: {\"a\":\"1\"}\r\n\r\n")
	if err != nil {
		t.Fatalf("DATA close: %v", err)
	}
	msg := lastCapture(t)
	if msg.Subject != "s1" || msg.Text != "1" {
		t.Fatalf("subject=%q text=%q", msg.Subject, msg.Text)
	}
}

// TestSMTPTemplateUnknown 不存在的模板 ID → 501（unknown_template 永久失败），不发送。
func TestSMTPTemplateUnknown(t *testing.T) {
	addr, _, _ := smtpTemplateEnv(t, "s", "", "t")
	err := sendRawData(t, addr, "sender@example.com", []string{"rcpt@example.com"},
		"From: sender@example.com\r\nSubject: x\r\nX-CatMail-Template: 999\r\n\r\n")
	var tpErr *textproto.Error
	if !errors.As(err, &tpErr) || tpErr.Code != 501 || !strings.Contains(tpErr.Msg, "unknown_template") {
		t.Fatalf("want 501 unknown_template, got %v", err)
	}
	capture.mu.Lock()
	n := len(capture.msgs)
	capture.mu.Unlock()
	if n != 0 {
		t.Fatalf("unknown template must not send, captured %d", n)
	}
}

// TestSMTPTemplateBodyConflict 报文自带非空正文时指定模板 → 501 body_conflict。
func TestSMTPTemplateBodyConflict(t *testing.T) {
	addr, tplID, _ := smtpTemplateEnv(t, "s", "", "t")
	err := sendRawData(t, addr, "sender@example.com", []string{"rcpt@example.com"},
		"From: sender@example.com\r\nSubject: x\r\n"+
			"X-CatMail-Template: "+fmt.Sprint(tplID)+"\r\n"+
			"Content-Type: text/plain\r\n\r\n自带正文\r\n")
	var tpErr *textproto.Error
	if !errors.As(err, &tpErr) || tpErr.Code != 501 || !strings.Contains(tpErr.Msg, "body_conflict") {
		t.Fatalf("want 501 body_conflict, got %v", err)
	}
}

// TestSMTPTemplateDirectiveInvalid 指令头取值非法（ID 非数字 / 变量非 JSON）→ 501。
func TestSMTPTemplateDirectiveInvalid(t *testing.T) {
	addr, _, _ := smtpTemplateEnv(t, "s", "", "t")
	cases := []string{
		"From: a@b.com\r\nSubject: x\r\nX-CatMail-Template: abc\r\n\r\n",
		"From: a@b.com\r\nSubject: x\r\nX-CatMail-Template: 0\r\n\r\n",
		"From: a@b.com\r\nSubject: x\r\nX-CatMail-Template: 1\r\nX-CatMail-Template-Vars: [1,2]\r\n\r\n",
		"From: a@b.com\r\nSubject: x\r\nX-CatMail-Template: 1\r\nX-CatMail-Template-Vars: base64:!!!\r\n\r\n",
	}
	for i, data := range cases {
		err := sendRawData(t, addr, "sender@example.com", []string{"rcpt@example.com"}, data)
		var tpErr *textproto.Error
		if !errors.As(err, &tpErr) || tpErr.Code != 501 {
			t.Fatalf("case %d: want 501, got %v", i, err)
		}
	}
	// 未携带指令头：行为与启用前一致（正文照发、不查模板）
	err := sendRawData(t, addr, "sender@example.com", []string{"rcpt@example.com"},
		"From: sender@example.com\r\nSubject: 原样\r\nContent-Type: text/plain\r\n\r\n原样正文\r\n")
	if err != nil {
		t.Fatalf("no directive: %v", err)
	}
	msg := lastCapture(t)
	if msg.Subject != "原样" || msg.Text != "原样正文\r\n" {
		t.Fatalf("no-directive msg: subject=%q text=%q", msg.Subject, msg.Text)
	}
}
