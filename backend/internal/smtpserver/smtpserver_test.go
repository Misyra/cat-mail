// smtpserver_test.go SMTP 入口集成测试：真回环端口 + net/smtp.Client / 原始
// textproto 客户端打全会话，覆盖无认证 loopback 全流程、AUTH 两机制、防开放
// 中继策略（forceAuth 注入）、SIZE/RCPT 上限、rotator 错误映射与优雅关停。
package smtpserver

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/provider"
	"catmail/internal/store"
)

// ---------- 测试基建 ----------

// newTestDB 打开临时 SQLite（AutoMigrate 全模型，行由用例自建）。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "smtp.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// startServer 起一个 127.0.0.1 随机端口的 SMTP 服务（loopback，默认免认证），
// maxBytes 默认 25MB，用例可传 0 或显式值覆盖。
func startServer(t *testing.T, db *gorm.DB, maxBytes int64) *Server {
	t.Helper()
	if maxBytes <= 0 {
		maxBytes = 25 << 20
	}
	s := New(db, "127.0.0.1:0", maxBytes)
	if err := s.Start(); err != nil {
		t.Fatalf("start smtp server: %v", err)
	}
	t.Cleanup(s.Stop)
	return s
}

// createKey 建一个 enabled ApiKey，返回明文 key。
func createKey(t *testing.T, db *gorm.DB, mutate func(*model.ApiKey)) string {
	t.Helper()
	ak := model.ApiKey{Name: "smtp-test", Key: "cm-" + strings.Repeat("k", 32), Enabled: true}
	if mutate != nil {
		mutate(&ak)
	}
	if err := db.Create(&ak).Error; err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return ak.Key
}

// mustDial 拨通测试端口。
func mustDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

// ---------- 捕获适配器：断言 rotator 实际收到的 provider.Message ----------

var capture struct {
	mu   sync.Mutex
	msgs []*provider.Message
}

type captureTransport struct{}

func (captureTransport) Name() string { return "capture" }

func (captureTransport) Send(_ context.Context, _ *model.Channel, m *provider.Message) (*provider.Result, *provider.ProviderError) {
	capture.mu.Lock()
	capture.msgs = append(capture.msgs, m)
	capture.mu.Unlock()
	return &provider.Result{ProviderMessageID: "cap-" + fmt.Sprint(len(capture.msgs))}, nil
}

func resetCapture() {
	capture.mu.Lock()
	capture.msgs = nil
	capture.mu.Unlock()
}

func lastCapture(t *testing.T) *provider.Message {
	t.Helper()
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.msgs) == 0 {
		t.Fatal("capture transport: no message sent")
	}
	return capture.msgs[len(capture.msgs)-1]
}

func init() { provider.Register(captureTransport{}) }

// ---------- 无认证 loopback 全流程 ----------

// multipart 报文：RFC 2047 中文主题/发件人/附件名、alternative 文本+HTML、
// mixed 里的 base64 附件（PDF 魔数），收件人头部与 envelope 交错去重。
const testMailRaw = "From: =?utf-8?B?5byg5LiJ?= <zhang@example.com>\r\n" +
	"To: =?utf-8?B?6K+76ICF?= <one@example.com>, rcpt1@example.com, rcpt2@example.com\r\n" +
	"Cc: cc@example.com\r\n" +
	"Reply-To: reply@example.com\r\n" +
	"Subject: =?utf-8?B?5rWL6K+V5Lit5paH5Li76aKY?= \r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"mixed-b\"\r\n" +
	"\r\n" +
	"--mixed-b\r\n" +
	"Content-Type: multipart/alternative; boundary=\"alt-b\"\r\n" +
	"\r\n" +
	"--alt-b\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"你好，世界\r\n" +
	"--alt-b\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>你好，世界</p>\r\n" +
	"--alt-b--\r\n" +
	"--mixed-b\r\n" +
	"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"Content-Disposition: attachment; filename=\"=?utf-8?B?5oql5ZGK?= report.pdf\"\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--mixed-b--\r\n"

func TestLoopbackFullFlow(t *testing.T) {
	resetCapture()
	db := newTestDB(t)
	if err := db.Create(&model.Channel{Name: "cap", Provider: "capture", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	s := startServer(t, db, 0)

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c, err := smtp.NewClient(conn, "127.0.0.1")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Hello("client.local"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if ok, ext := c.Extension("AUTH"); !ok || !strings.Contains(ext, "PLAIN") || !strings.Contains(ext, "LOGIN") {
		t.Fatalf("EHLO should advertise AUTH PLAIN LOGIN, got %q (ok=%v)", ext, ok)
	}
	if ok, _ := c.Extension("PIPELINING"); !ok {
		t.Fatal("EHLO should advertise PIPELINING")
	}
	if ok, _ := c.Extension("8BITMIME"); !ok {
		t.Fatal("EHLO should advertise 8BITMIME")
	}
	if err := c.Mail("sender@example.com"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	for _, rcpt := range []string{"rcpt1@example.com", "rcpt2@example.com", "secret@example.com"} {
		if err := c.Rcpt(rcpt); err != nil {
			t.Fatalf("RCPT %s: %v", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	if _, err := w.Write([]byte(testMailRaw)); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("end data: %v", err)
	}
	if err := c.Quit(); err != nil {
		t.Fatalf("QUIT: %v", err)
	}

	// capture 适配器收到的规范消息：头部/envelope 交错下的字段口径。
	msg := lastCapture(t)
	if msg.Subject != "测试中文主题" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if msg.FromName != "张三" || msg.FromAddr != "zhang@example.com" {
		t.Errorf("from = %q <%q>", msg.FromName, msg.FromAddr)
	}
	wantTo := map[string]bool{
		"rcpt1@example.com": false, "rcpt2@example.com": false, // envelope
		"one@example.com": false, // To 头（rcpt1 同时出现在两处：并集去重）
	}
	for _, a := range msg.To {
		if seen, ok := wantTo[a]; !ok {
			t.Errorf("unexpected To %q", a)
		} else if seen {
			t.Errorf("duplicated To %q", a)
		}
		wantTo[a] = true
	}
	for a, seen := range wantTo {
		if !seen {
			t.Errorf("missing To %q", a)
		}
	}
	if len(msg.Cc) != 1 || msg.Cc[0] != "cc@example.com" {
		t.Errorf("cc = %v", msg.Cc)
	}
	// secret@example.com 只在 envelope：按 BCC 语义还原，不混入 To/Cc。
	if len(msg.Bcc) != 1 || msg.Bcc[0] != "secret@example.com" {
		t.Errorf("bcc = %v", msg.Bcc)
	}
	if len(msg.ReplyTo) != 1 || msg.ReplyTo[0] != "reply@example.com" {
		t.Errorf("reply_to = %v", msg.ReplyTo)
	}
	if msg.Text != "你好，世界" {
		t.Errorf("text = %q", msg.Text)
	}
	if msg.HTML != "<p>你好，世界</p>" {
		t.Errorf("html = %q", msg.HTML)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.Filename != "报告 report.pdf" {
		t.Errorf("attachment filename = %q", att.Filename)
	}
	if att.ContentBase64 != "JVBERi0xLjQK" { // %PDF-1.4\n
		t.Errorf("attachment content = %q", att.ContentBase64)
	}

	// SendLog 落库：归属/状态/快照字段。
	var logs []model.SendLog
	if err := db.Find(&logs).Error; err != nil {
		t.Fatalf("query send logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("send logs = %d, want 1", len(logs))
	}
	lg := logs[0]
	if lg.Status != model.SendStatusSent || lg.IsTest || lg.ApiKeyID != 0 {
		t.Errorf("log status/is_test/api_key_id = %d/%v/%d", lg.Status, lg.IsTest, lg.ApiKeyID)
	}
	if lg.ChannelName != "cap" || lg.Provider != "capture" {
		t.Errorf("log channel = %s/%s", lg.ChannelName, lg.Provider)
	}
	if lg.Subject != "测试中文主题" || lg.FromAddr != "zhang@example.com" {
		t.Errorf("log subject/from = %q/%q", lg.Subject, lg.FromAddr)
	}
	var tos []string
	if err := json.Unmarshal([]byte(lg.ToAddrs), &tos); err != nil {
		t.Fatalf("unmarshal to_addrs %q: %v", lg.ToAddrs, err)
	}
	if len(tos) != 3 {
		t.Errorf("log to_addrs = %v", tos)
	}
	if lg.TextBody != "你好，世界" || lg.HTMLBody != "<p>你好，世界</p>" {
		t.Errorf("log bodies = %q / %q", lg.TextBody, lg.HTMLBody)
	}
	if lg.ProviderMessageID == "" || lg.MessageID == "" {
		t.Errorf("log ids empty: %q / %q", lg.ProviderMessageID, lg.MessageID)
	}
}

// ---------- 防开放中继策略（forceAuth 注入模拟非 loopback 对端） ----------

func TestRequireAuthPolicy(t *testing.T) {
	db := newTestDB(t)
	if err := db.Create(&model.Channel{Name: "cap", Provider: "capture", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	s := New(db, "127.0.0.1:0", 0)
	s.forceAuth = true // 恒要求 AUTH：等价非 loopback 对端
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	// 无认证：MAIL 可达（反向路径无中继风险），RCPT 必须被 530 拦截。
	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c, err := smtp.NewClient(conn, "127.0.0.1")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Hello("client.local"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if err := c.Mail("attacker@evil.com"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	err = c.Rcpt("victim@example.com")
	var tpErr *textproto.Error
	if !errors.As(err, &tpErr) || tpErr.Code != 530 {
		t.Fatalf("RCPT without auth: err=%v, want 530", err)
	}

	// 错误 key → 535；正确 key → 235，之后正常发送并归属 ApiKey。
	goodKey := createKey(t, db, nil)
	if err := tryAuth(t, s.Addr(), "anyuser", "cm-badkey"); err == nil || !strings.Contains(err.Error(), "535") {
		t.Fatalf("auth with wrong key: err=%v, want 535", err)
	}
	if err := tryAuth(t, s.Addr(), "anyuser", goodKey); err != nil {
		t.Fatalf("auth with good key: %v", err)
	}
	resetCapture()
	sendOne(t, s.Addr(), goodKey, "relay@example.com", []string{"ok@example.com"}, "hello", "auth flow")
	msg := lastCapture(t)
	if len(msg.To) != 1 || msg.To[0] != "ok@example.com" {
		t.Errorf("authed send to = %v", msg.To)
	}
	var ak model.ApiKey
	if err := db.Where("key = ?", goodKey).First(&ak).Error; err != nil {
		t.Fatalf("reload key: %v", err)
	}
	var logs []model.SendLog
	if err := db.Where("api_key_id = ?", ak.ID).Find(&logs).Error; err != nil {
		t.Fatalf("query logs: %v", err)
	}
	if len(logs) != 1 || logs[0].Status != model.SendStatusSent {
		t.Fatalf("authed send logs = %+v", logs)
	}
}

// tryAuth 用 net/smtp.PlainAuth 完成一次 AUTH（服务端 235/535 转成 err）。
func tryAuth(t *testing.T, addr, user, pass string) error {
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
	return c.Auth(smtp.PlainAuth("", user, pass, "127.0.0.1"))
}

// sendOne 走完整 AUTH（可选）+ MAIL/RCPT/DATA 流程发一封最小报文（PLAIN 文本）；
// authPass 为空则跳过 AUTH（loopback 免认证会话）。
func sendOne(t *testing.T, addr, authPass, from string, to []string, subject, text string) {
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
	if authPass != "" {
		if err := c.Auth(smtp.PlainAuth("", "anyuser", authPass, "127.0.0.1")); err != nil {
			t.Fatalf("AUTH: %v", err)
		}
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
	fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain\r\n\r\n%s\r\n", from, strings.Join(to, ", "), subject, text)
	if err := w.Close(); err != nil {
		t.Fatalf("end data: %v", err)
	}
}

// ---------- AUTH LOGIN（原始 textproto 客户端：net/smtp 未导出该机制） ----------

func TestAuthLoginMechanism(t *testing.T) {
	db := newTestDB(t)
	goodKey := createKey(t, db, nil)
	s := New(db, "127.0.0.1:0", 0)
	s.forceAuth = true
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	tp := textproto.NewConn(conn)
	if _, _, err := tp.ReadResponse(220); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if err := tp.PrintfLine("EHLO test"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if _, _, err := tp.ReadResponse(250); err != nil { // 多行 250 一次读完
		t.Fatalf("EHLO reply: %v", err)
	}
	if err := tp.PrintfLine("AUTH LOGIN"); err != nil {
		t.Fatalf("AUTH LOGIN: %v", err)
	}
	_, msg, err := tp.ReadResponse(334)
	if err != nil {
		t.Fatalf("username challenge: %v", err)
	}
	if got, _ := base64.StdEncoding.DecodeString(msg); string(got) != "Username:" {
		t.Fatalf("username challenge = %q", msg)
	}
	if err := tp.PrintfLine("%s", base64.StdEncoding.EncodeToString([]byte("anyuser"))); err != nil {
		t.Fatalf("send username: %v", err)
	}
	_, msg, err = tp.ReadResponse(334)
	if err != nil {
		t.Fatalf("password challenge: %v", err)
	}
	if got, _ := base64.StdEncoding.DecodeString(msg); string(got) != "Password:" {
		t.Fatalf("password challenge = %q", msg)
	}
	if err := tp.PrintfLine("%s", base64.StdEncoding.EncodeToString([]byte(goodKey))); err != nil {
		t.Fatalf("send password: %v", err)
	}
	code, _, err := tp.ReadResponse(235)
	if err != nil {
		t.Fatalf("AUTH LOGIN with good key: %v", err)
	}
	if code != 235 {
		t.Fatalf("auth code = %d", code)
	}
	if err := tp.PrintfLine("QUIT"); err != nil {
		t.Fatalf("QUIT: %v", err)
	}
	if _, _, err := tp.ReadResponse(221); err != nil {
		t.Fatalf("bye: %v", err)
	}
}

// ---------- SIZE / RCPT 上限 ----------

func TestSizeLimit552(t *testing.T) {
	db := newTestDB(t)
	if err := db.Create(&model.Channel{Name: "cap", Provider: "capture", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	const maxBytes = 1024
	s := startServer(t, db, maxBytes)
	var tpErr *textproto.Error

	// MAIL SIZE= 参数超限：DATA 之前直接 552。
	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	tp := textproto.NewConn(conn)
	if _, _, err := tp.ReadResponse(220); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if err := tp.PrintfLine("EHLO test"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if _, _, err := tp.ReadResponse(250); err != nil {
		t.Fatalf("EHLO reply: %v", err)
	}
	if err := tp.PrintfLine("MAIL FROM:<a@example.com> SIZE=99999999"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	code, _, err := tp.ReadResponse(552)
	if err != nil || code != 552 {
		t.Fatalf("MAIL SIZE param: code=%d err=%v, want 552", code, err)
	}

	// DATA 载荷实际超限：读完统一 552（net/smtp.Data 返回 textproto.Error）。
	resetCapture()
	c, err := smtp.NewClient(mustDial(t, s.Addr()), "127.0.0.1")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Hello("client.local"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if err := c.Mail("a@example.com"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt("b@example.com"); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	if _, err := w.Write([]byte(strings.Repeat("a\r\n", maxBytes))); err != nil { // 3×maxBytes
		t.Fatalf("write data: %v", err)
	}
	err = w.Close()
	if !errors.As(err, &tpErr) || tpErr.Code != 552 {
		t.Fatalf("oversized data: err=%v, want 552", err)
	}
	capture.mu.Lock()
	n := len(capture.msgs)
	capture.mu.Unlock()
	if n != 0 {
		t.Fatalf("oversized mail must not reach rotator, got %d", n)
	}
}

func TestRcptOver100(t *testing.T) {
	db := newTestDB(t)
	if err := db.Create(&model.Channel{Name: "cap", Provider: "capture", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	s := startServer(t, db, 0)
	conn, err := net.Dial("tcp", s.Addr())
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
	if err := c.Mail("a@example.com"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	for i := 0; i < maxRcpt; i++ {
		if err := c.Rcpt(fmt.Sprintf("r%d@example.com", i)); err != nil {
			t.Fatalf("RCPT #%d: %v", i+1, err)
		}
	}
	err = c.Rcpt("overflow@example.com")
	var tpErr *textproto.Error
	if !errors.As(err, &tpErr) || tpErr.Code != 452 {
		t.Fatalf("RCPT #%d: err=%v, want 452", maxRcpt+1, err)
	}
}

// ---------- rotator 错误映射：429→452 / 502→451 / 400→501 ----------

func TestRotatorQuotaExhausted452(t *testing.T) {
	db := newTestDB(t) // 无任何渠道 → rotator 429 quota_exhausted
	s := startServer(t, db, 0)
	conn, err := net.Dial("tcp", s.Addr())
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
	if err := c.Mail("a@example.com"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt("b@example.com"); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	fmt.Fprint(w, "Subject: no channel\r\n\r\nbody\r\n")
	err = w.Close()
	var tpErr *textproto.Error
	if !errors.As(err, &tpErr) || tpErr.Code != 452 {
		t.Fatalf("no-channel send: err=%v, want 452", err)
	}
	if !strings.Contains(tpErr.Msg, "4.2.1") {
		t.Errorf("452 text = %q, want enhanced code 4.2.1", tpErr.Msg)
	}
	// 事务已复位：同一连接可继续（RSET 语义），RCPT 重新可用。
	if err := c.Rcpt("b@example.com"); err == nil {
		t.Fatal("after 452 the session should require a fresh MAIL before RCPT")
	}
}

func TestProviderError451(t *testing.T) {
	db := newTestDB(t)
	// mock 魔法值 fail → 平台 5xx 可重试 → 候选穷尽 502 → SMTP 451。
	if err := db.Create(&model.Channel{Name: "mock-fail", Provider: "mock", APIKey: "fail", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	s := startServer(t, db, 0)
	conn, err := net.Dial("tcp", s.Addr())
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
	if err := c.Mail("a@example.com"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt("b@example.com"); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	fmt.Fprint(w, "Subject: provider fail\r\n\r\nbody\r\n")
	err = w.Close()
	var tpErr *textproto.Error
	if !errors.As(err, &tpErr) || tpErr.Code != 451 {
		t.Fatalf("all-providers-failed: err=%v, want 451", err)
	}
	if !strings.Contains(tpErr.Msg, "4.3.0") {
		t.Errorf("451 text = %q, want enhanced code 4.3.0", tpErr.Msg)
	}
}

func TestKeyDailyQuota452(t *testing.T) {
	db := newTestDB(t)
	if err := db.Create(&model.Channel{Name: "m", Provider: "mock", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	goodKey := createKey(t, db, func(ak *model.ApiKey) { ak.DailyQuota = 1 })
	s := New(db, "127.0.0.1:0", 0)
	s.forceAuth = true // 走认证路径才能带上 Key
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	if err := tryAuth(t, s.Addr(), "anyuser", goodKey); err != nil {
		t.Fatalf("auth: %v", err)
	}
	// 第一封：250 成功并按 api_key_id 落库（计入日额度）。
	sendOne(t, s.Addr(), goodKey, "a@example.com", []string{"b@example.com"}, "first", "1st")
	var logs []model.SendLog
	if err := db.Where("status = ?", model.SendStatusSent).Find(&logs).Error; err != nil {
		t.Fatalf("query logs: %v", err)
	}
	if len(logs) != 1 || logs[0].ApiKeyID == 0 {
		t.Fatalf("first send log = %+v", logs)
	}
	// 第二封：Key 日额度已用尽 → 452（且不落新日志）。
	conn, err := net.Dial("tcp", s.Addr())
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
	if err := c.Auth(smtp.PlainAuth("", "anyuser", goodKey, "127.0.0.1")); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if err := c.Mail("a@example.com"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt("b@example.com"); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	fmt.Fprint(w, "Subject: second\r\n\r\n2nd\r\n")
	err = w.Close()
	var tpErr *textproto.Error
	if !errors.As(err, &tpErr) || tpErr.Code != 452 {
		t.Fatalf("second send: err=%v, want 452", err)
	}
	var total int64
	if err := db.Model(&model.SendLog{}).Count(&total).Error; err != nil || total != 1 {
		t.Fatalf("logs after quota rejection = %d (err=%v), want 1", total, err)
	}
}

// ---------- 会话基础行为 ----------

func TestSessionBasics(t *testing.T) {
	db := newTestDB(t)
	if err := db.Create(&model.Channel{Name: "cap", Provider: "capture", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	s := startServer(t, db, 0)
	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	tp := textproto.NewConn(conn)
	if _, _, err := tp.ReadResponse(220); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if err := tp.PrintfLine("EHLO test"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if _, _, err := tp.ReadResponse(250); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	steps := []struct {
		cmd  string
		code int
	}{
		{"NOOP", 250},
		{"FOOBAR baz", 502},                 // 未知命令
		{"RCPT TO:<x@example.com>", 503},    // 未 MAIL 先 RCPT
		{"MAIL FROM:<not-an-address>", 501}, // 路径语法
		{"MAIL FROM:<a@example.com>", 250},
		{"MAIL FROM:<a@example.com>", 503}, // 嵌套 MAIL
		{"DATA", 503},                      // 未 RCPT 先 DATA
		{"RSET", 250},
		{"RCPT TO:<x@example.com>", 503}, // RSET 后事务已复位
		{"NOOP", 250},
		{"QUIT", 221},
	}
	for i, st := range steps {
		if err := tp.PrintfLine("%s", st.cmd); err != nil {
			t.Fatalf("step %d send %q: %v", i, st.cmd, err)
		}
		code, _, err := tp.ReadResponse(st.code)
		if err != nil || code != st.code {
			t.Fatalf("step %d %q: code=%d err=%v, want %d", i, st.cmd, code, err, st.code)
		}
	}
	// QUIT 后服务端关连接：继续读应到 EOF。
	if err := tp.PrintfLine("NOOP"); err == nil {
		if _, err := tp.ReadLine(); err == nil {
			t.Fatal("connection should be closed after QUIT")
		}
	}
}

// ---------- 优雅关停 ----------

func TestStopGraceful(t *testing.T) {
	db := newTestDB(t)
	s := New(db, "127.0.0.1:0", 0)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 在途连接：读完 greeting 后挂住（不发命令），Stop 应强制关闭它。
	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, _, err := bufio.NewReader(conn).ReadLine(); err != nil { // greeting "220 ..."
		t.Fatalf("greeting: %v", err)
	}

	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(stopGrace + stopForce + 3*time.Second):
		t.Fatal("Stop() did not return in time")
	}
	if !s.isClosed() {
		t.Fatal("server should be marked closed")
	}
	// 在途连接被强制关闭：对端读到 EOF/错误。
	if _, err := conn.Read(make([]byte, 8)); err == nil {
		t.Fatal("in-flight connection should be force-closed")
	}
	// listener 已关：新连接失败。
	if _, err := net.DialTimeout("tcp", s.Addr(), time.Second); err == nil {
		t.Fatal("dial after Stop should fail")
	}
	// 重复 Stop 安全。
	s.Stop()
}
