// smtp_test.go SMTP 适配器零网络单测：MIME 构建、头编码、错误分类、代理配置。
package provider

import (
	"encoding/base64"
	"errors"
	"net/textproto"
	"os"
	"reflect"
	"strings"
	"testing"
)

// assertContains 大小写不敏感断言（RFC2047 编码 token 大小写不敏感）。
func assertContains(t *testing.T, s string, wants ...string) {
	low := strings.ToLower(s)
	for _, w := range wants {
		if !strings.Contains(low, strings.ToLower(w)) {
			t.Errorf("缺少 %q", w)
		}
	}
}

func TestSMTPBuildMIME(t *testing.T) {
	full, err := buildMIME(&Message{FromName: "张三", FromAddr: "sender@example.com", To: []string{"t@example.com"},
		Subject: "你好 世界", Text: "纯文本正文", HTML: "<p>hi</p>"})
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	enc := base64.StdEncoding.EncodeToString([]byte("你好 世界"))
	assertContains(t, string(full), `multipart/mixed; boundary="`, `multipart/alternative; boundary="`,
		"subject: =?utf-8?b?"+strings.ToLower(enc)+"?=", "from: =?utf-8?q?", "<sender@example.com>",
		"text/plain; charset=utf-8", "text/html; charset=utf-8",
		base64.StdEncoding.EncodeToString([]byte("纯文本正文")), base64.StdEncoding.EncodeToString([]byte("<p>hi</p>")),
		"message-id: <smtp-", "date: ", "mime-version: 1.0")

	only, err := buildMIME(&Message{FromAddr: "s@example.com", To: []string{"t@example.com"}, Subject: "t",
		Text: "only text", ReplyTo: []string{"reply@example.com"}, Headers: map[string]string{"X-Mailer": "catmail"},
		// 附件 base64 夹带换行：解码需先剔除空白（容错中间换行）
		Attachments: []Attachment{{Filename: "报告.pdf", ContentBase64: "aGVs\nbG8="}}})
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	assertContains(t, string(only), "x-mailer: catmail", "reply-to: <reply@example.com>",
		"content-transfer-encoding: base64", "filename*=utf-8''", "application/pdf", "aGVsbG8=",
		"text/plain; charset=utf-8")
	if strings.Contains(string(only), "text/html") || strings.Contains(string(only), "multipart/alternative") {
		t.Errorf("空 HTML 不应出现 text/html 或 alternative") // 空 HTML 只发 text
	}

	// 修复锁定：To/Cc 显示名经 net/mail 重排，非 ASCII 走 RFC 2047 编码（不落原始 UTF-8 到头里）
	cjk, err := buildMIME(&Message{FromAddr: "s@example.com",
		To: []string{"张三 <to@example.com>"},
		Cc: []string{"Bob <cc@example.com>"}, Subject: "t", Text: "x"})
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	assertContains(t, string(cjk),
		"to: =?utf-8?q?=E5=BC=A0=E4=B8=89?= <to@example.com>",
		`"Bob" <cc@example.com>`)
}

func TestSMTPClassifyError(t *testing.T) {
	cases := []struct {
		name                         string
		err                          error
		code                         string
		retryable, disable, cooldown bool
	}{
		// 修复锁定：530 需要认证 / 534 认证机制过弱 / 535 认证失败 → 均为凭据问题，禁用渠道
		{"auth 530", &textproto.Error{Code: 530, Msg: "authentication required"}, "auth_failed", false, true, false},
		{"auth 534", &textproto.Error{Code: 534, Msg: "auth mechanism too weak"}, "auth_failed", false, true, false},
		{"auth 535", &textproto.Error{Code: 535, Msg: "auth failed"}, "auth_failed", false, true, false},
		{"reject 550", &textproto.Error{Code: 550, Msg: "user unknown"}, "smtp_rejected", false, false, false},
		{"reject 553", &textproto.Error{Code: 553, Msg: "bad mailbox"}, "smtp_rejected", false, false, false},
		{"temporary 451", &textproto.Error{Code: 451, Msg: "try later"}, "smtp_temporary", true, false, false},
		{"rate 429", &textproto.Error{Code: 429, Msg: "too many"}, "rate_limited", true, false, true},
		{"deadline", os.ErrDeadlineExceeded, "timeout", true, false, false},
		{"network", errors.New("connection refused"), "network_error", true, false, false},
	}
	for _, tc := range cases {
		pe := smtpClassifyError(tc.err)
		if pe.Code != tc.code || pe.Retryable != tc.retryable || pe.DisableChannel != tc.disable || pe.Cooldown != tc.cooldown {
			t.Errorf("%s: got code=%s retry=%v disable=%v cooldown=%v, want %s/%v/%v/%v (%v)",
				tc.name, pe.Code, pe.Retryable, pe.DisableChannel, pe.Cooldown,
				tc.code, tc.retryable, tc.disable, tc.cooldown, pe.Message)
		}
	}
}

// TestSMTPEnvelopeRcpts 信封收件人仅取裸地址：dto 保留 "Name <addr>" 原始串，net/smtp 的 RCPT TO 会再包一层尖括号。
func TestSMTPEnvelopeRcpts(t *testing.T) {
	got := smtpEnvelopeRcpts(
		[]string{"Bob <b@example.com>", "bare@example.com"},
		[]string{"<angle@example.com>"},
		nil,
	)
	want := []string{"b@example.com", "bare@example.com", "angle@example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("smtpEnvelopeRcpts = %v, want %v", got, want)
	}
	// 解析失败回退原文（交由服务器/上游校验报错），且列表可多段合并
	if got := smtpEnvelopeRcpts([]string{"not-an-address"}, []string{"x@example.com"}); !reflect.DeepEqual(got, []string{"not-an-address", "x@example.com"}) {
		t.Fatalf("fallback = %v, want [not-an-address x@example.com]", got)
	}
}

func TestSMTPDialerForProxy(t *testing.T) {
	if _, pe := smtpDialerFor(""); pe != nil {
		t.Errorf("空代理应直连, got %v", pe)
	}
	if _, pe := smtpDialerFor("socks5://127.0.0.1:1080"); pe != nil { // 仅构造 dialer，不拨号
		t.Errorf("socks5 代理应构造成功, got %v", pe)
	}
	_, pe := smtpDialerFor("http://127.0.0.1:8080")
	if pe == nil || !strings.Contains(pe.Message, "socks5") || pe.Retryable || pe.DisableChannel || pe.Cooldown {
		t.Errorf("http 代理应返回「仅支持 socks5」参数类错误, got %+v", pe)
	}
}

// TestSMTPLoginAuth 修复锁定：AUTH LOGIN 两轮挑战应答（Start 无 initial response，先用户名后密码）。
func TestSMTPLoginAuth(t *testing.T) {
	a := &smtpLoginAuth{username: "user", password: "pass"}
	mech, initial, err := a.Start(nil)
	if err != nil || mech != "LOGIN" || initial != nil {
		t.Fatalf("Start = (%q, %v, %v), want (LOGIN, nil, nil)", mech, initial, err)
	}
	// 首轮挑战（net/smtp 已 base64 解码，内容多为 "Username:"）→ 用户名；次轮 → 密码
	r1, err := a.Next([]byte("Username:"), true)
	if err != nil || string(r1) != "user" {
		t.Fatalf("Next#1 = (%q, %v), want user", r1, err)
	}
	r2, err := a.Next([]byte("Password:"), true)
	if err != nil || string(r2) != "pass" {
		t.Fatalf("Next#2 = (%q, %v), want pass", r2, err)
	}
	if r3, err := a.Next(nil, true); err == nil || r3 != nil {
		t.Fatalf("Next#3 应报意外挑战, got (%q, %v)", r3, err)
	}
	if r, err := a.Next(nil, false); err != nil || r != nil {
		t.Fatalf("more=false 应返回 (nil, nil), got (%q, %v)", r, err)
	}
}

// TestSMTPAuthMechSupported 机制列表匹配大小写不敏感（服务器可能返回小写 plain）。
func TestSMTPAuthMechSupported(t *testing.T) {
	if !authMechSupported("PLAIN LOGIN XCLIENT", "PLAIN") || !authMechSupported("plain", "PLAIN") {
		t.Errorf("PLAIN 应被识别")
	}
	if authMechSupported("XOAUTH2 OAUTHBEARER", "LOGIN") {
		t.Errorf("不含 LOGIN 不应误判")
	}
}
