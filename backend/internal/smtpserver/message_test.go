// message_test.go buildMessage 的表驱动单元测试：multipart/alternative/mixed、
// CTE 解码（quoted-printable/base64）、From 头与 envelope 回退、BCC 还原、
// RFC 2047 解码与坏数据拒收。
package smtpserver

import (
	"encoding/base64"
	"strconv"
	"testing"
)

func TestBuildMessagePlainText(t *testing.T) {
	raw := "From: Alice <alice@example.com>\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: plain\r\n" +
		"\r\n" +
		"hello world\r\n"
	msg, err := buildMessage([]byte(raw), "env@example.com", []string{"bob@example.com"})
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	if msg.Subject != "plain" || msg.FromName != "Alice" || msg.FromAddr != "alice@example.com" {
		t.Errorf("subject/from = %q %q <%q>", msg.Subject, msg.FromName, msg.FromAddr)
	}
	if msg.Text != "hello world\r\n" || msg.HTML != "" { // 单部分正文保留行尾 CRLF
		t.Errorf("text/html = %q / %q", msg.Text, msg.HTML)
	}
	if len(msg.To) != 1 || msg.To[0] != "bob@example.com" {
		t.Errorf("to = %v", msg.To)
	}
}

func TestBuildMessageQuotedPrintable(t *testing.T) {
	// 顶层 text/plain + QP 编码正文（中文 UTF-8）：“你好” = E4 BD A0 E5 A5 BD。
	raw := "Subject: qp\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"=E4=BD=A0=E5=A5=BD\r\n"
	msg, err := buildMessage([]byte(raw), "", nil)
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	if msg.Text != "你好\r\n" {
		t.Errorf("text = %q, want 你好\\r\\n", msg.Text)
	}
}

func TestBuildMessageFromEnvelopeFallback(t *testing.T) {
	// 无 From 头：回退 envelope MAIL FROM。
	raw := "Subject: no from header\r\n\r\nbody\r\n"
	msg, err := buildMessage([]byte(raw), "env@example.com", nil)
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	if msg.FromAddr != "env@example.com" {
		t.Errorf("from = %q, want envelope fallback", msg.FromAddr)
	}

	// envelope 也无效：报错（501 拒收，不硬造地址）。
	if _, err := buildMessage([]byte(raw), "not an address", nil); err == nil {
		t.Fatal("invalid envelope from should error")
	}
}

func TestBuildMessageBCCToCc(t *testing.T) {
	// envelope: b1（仅信封）+ v1（在 To 头）；Cc 头 c1。
	raw := "To: v1@example.com\r\nCc: c1@example.com\r\nSubject: bcc\r\n\r\nbody\r\n"
	rcpts := []string{"v1@example.com", "b1@example.com"}
	msg, err := buildMessage([]byte(raw), "env@example.com", rcpts)
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	if len(msg.To) != 1 || msg.To[0] != "v1@example.com" {
		t.Errorf("to = %v", msg.To)
	}
	if len(msg.Cc) != 1 || msg.Cc[0] != "c1@example.com" {
		t.Errorf("cc = %v", msg.Cc)
	}
	if len(msg.Bcc) != 1 || msg.Bcc[0] != "b1@example.com" {
		t.Errorf("bcc = %v", msg.Bcc)
	}
}

func TestBuildMessageAttachments(t *testing.T) {
	payload := "bin\x00\x01\xff"
	b64 := base64.StdEncoding.EncodeToString([]byte(payload))
	raw := "Subject: att\r\n" +
		"Content-Type: multipart/mixed; boundary=\"b\"\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"body\r\n" +
		"--b\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"a.bin\"\r\n" +
		"\r\n" +
		b64 + "\r\n" +
		"--b\r\n" +
		"Content-Type: application/zip\r\n" +
		"Content-Transfer-Encoding: base64\r\n" + // attachment 无 filename → 兜底命名
		"Content-Disposition: attachment\r\n" +
		"\r\n" +
		"UEsDBAo=\r\n" +
		"--b--\r\n"
	msg, err := buildMessage([]byte(raw), "", nil)
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	if len(msg.Attachments) != 2 {
		t.Fatalf("attachments = %d, want 2", len(msg.Attachments))
	}
	if msg.Attachments[0].Filename != "a.bin" || msg.Attachments[0].ContentBase64 != b64 {
		t.Errorf("att[0] = %+v", msg.Attachments[0])
	}
	if msg.Attachments[1].Filename != "attachment-1" {
		t.Errorf("att[1].filename = %q, want 兜底 attachment-1", msg.Attachments[1].Filename)
	}
	decoded, err := base64.StdEncoding.DecodeString(msg.Attachments[1].ContentBase64)
	if err != nil || string(decoded) != "PK\x03\x04\n" {
		t.Errorf("att[1] decode = %q err=%v", decoded, err)
	}
}

func TestBuildMessageNestedMultipartAndSubject(t *testing.T) {
	// 深层嵌套 + 非 encoded-word 主题原样保留 + 无 Content-Type 的部分按 text/plain。
	raw := "Subject: =?utf-8?B?5rWL6K+V?= tail\r\n" +
		"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
		"\r\n" +
		"--outer\r\n" +
		"Content-Type: multipart/alternative; boundary=\"inner\"\r\n" +
		"\r\n" +
		"--inner\r\n" +
		"\r\n" +
		"plain text\r\n" +
		"--inner\r\n" +
		"Content-Type: text/html\r\n" +
		"\r\n" +
		"<b>html</b>\r\n" +
		"--inner--\r\n" +
		"--outer--\r\n"
	msg, err := buildMessage([]byte(raw), "", nil)
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	if msg.Subject != "测试 tail" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if msg.Text != "plain text" || msg.HTML != "<b>html</b>" {
		t.Errorf("text/html = %q / %q", msg.Text, msg.HTML)
	}
}

func TestBuildMessageRejectsBadContent(t *testing.T) {
	// 坏 base64 附件 → 报错（501），不静默截断。
	raw := "Subject: bad\r\n" +
		"Content-Type: multipart/mixed; boundary=\"b\"\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"x\"\r\n" +
		"\r\n" +
		"!!!!not-base64!!!!\r\n" +
		"--b--\r\n"
	if _, err := buildMessage([]byte(raw), "", nil); err == nil {
		t.Fatal("bad base64 attachment should error")
	}

	// multipart 缺 boundary → 报错。
	raw2 := "Subject: nb\r\nContent-Type: multipart/mixed\r\n\r\nbody\r\n"
	if _, err := buildMessage([]byte(raw2), "", nil); err == nil {
		t.Fatal("multipart without boundary should error")
	}

	// MIME 深嵌套超限 → 报错。
	if _, err := buildMessage([]byte(nestedRaw(maxMimeDepth+2)), "", nil); err == nil {
		t.Fatal("deep nested multipart should error")
	}
	// 合法深度内不报错。
	if _, err := buildMessage([]byte(nestedRaw(maxMimeDepth-1)), "", nil); err != nil {
		t.Fatalf("nested within limit should parse: %v", err)
	}
}

// nestedRaw 构造 depth 层 multipart/mixed 嵌套（每层一个子部分），最内层为
// 空头 text/plain。
func nestedRaw(depth int) string {
	if depth == 0 {
		return "\r\ninnermost\r\n" // 空头部（紧随边界的空行）+ 正文
	}
	b := "b" + strconv.Itoa(depth)
	return "Content-Type: multipart/mixed; boundary=\"" + b + "\"\r\n" +
		"\r\n" +
		"--" + b + "\r\n" +
		nestedRaw(depth-1) +
		"--" + b + "--\r\n"
}

func TestParsePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"<a@example.com>", "a@example.com", true},
		{"a@example.com", "a@example.com", true},          // 裸地址宽容
		{"<>", "", true},                                  // 空路径（仅 MAIL 合法）
		{"<Name <a@example.com>>", "a@example.com", true}, // 剥外层 <> 后按 mail 解析，取地址
		{"<a@example.com", "", false},
		{"a b@example.com", "", false},
		{"", "", false}, // RCPT 空路径非法
	}
	for _, c := range cases {
		got, err := parsePath(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parsePath(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parsePath(%q) should fail", c.in)
		}
	}
}
