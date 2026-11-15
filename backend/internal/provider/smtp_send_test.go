// smtp_send_test.go SMTP 适配器 Send 端到端零网络测试：本地最小 SMTP 假服务端，
// 覆盖信封/认证协商/错误分类贯通 Send 的完整事务路径（此前 Send/smtpTransaction 0% 覆盖）。
package provider

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"catmail/internal/model"
)

// fakeSMTPServer 最小 SMTP 假服务端（RFC 5321 子集）：单连接顺序处理，
// 按脚本决定 EHLO 广告的 AUTH 机制与各阶段失败码，记录收到的信封与正文。
type fakeSMTPServer struct {
	t *testing.T

	authMechs    string // EHLO 广告的 AUTH 机制串（空=不广告，客户端匿名）
	authFailCode int    // AUTH 阶段失败码（0=成功）
	mailFailCode int    // MAIL FROM 阶段失败码（0=成功）
	rcptFailCode int    // RCPT TO 阶段失败码（0=成功）

	mu       sync.Mutex
	authLine string   // 认证行原文（AUTH PLAIN <b64> / AUTH LOGIN）
	authNext []string // AUTH LOGIN 挑战应答收到的解码片段
	mailFrom string
	rcpts    []string
	bodies   []string // DATA 结束时收到的完整正文
}

func startFakeSMTP(t *testing.T, srv *fakeSMTPServer) (addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serve(conn)
		}
	}()
	return ln.Addr().String()
}

// reply 写一行 SMTP 应答。
func (s *fakeSMTPServer) reply(w *bufio.Writer, code int, msg string) {
	fmt.Fprintf(w, "%d %s\r\n", code, msg)
	_ = w.Flush()
}

func (s *fakeSMTPServer) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	s.reply(w, 220, "fake ESMTP")

	var rcptFailNext bool // 脚本失败码只吃一次：首个 RCPT 失败、后续放行
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, rest, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "EHLO":
			// 多行应答：续行（250-）在前，终止行（250␣）最后，net/smtp 才能解析到 AUTH 扩展
			fmt.Fprintf(w, "250-fake\r\n")
			if s.authMechs != "" {
				fmt.Fprintf(w, "250-AUTH %s\r\n", s.authMechs)
			}
			fmt.Fprintf(w, "250 OK\r\n")
			_ = w.Flush()
		case "AUTH":
			s.mu.Lock()
			s.authLine = line
			s.mu.Unlock()
			if s.authFailCode != 0 {
				s.reply(w, s.authFailCode, "bad credentials")
				continue
			}
			mech, _, _ := strings.Cut(rest, " ")
			if strings.EqualFold(mech, "LOGIN") {
				// 两轮挑战应答：解码客户端应答便于断言
				for _, challenge := range []string{"Username:", "Password:"} {
					s.reply(w, 334, base64.StdEncoding.EncodeToString([]byte(challenge)))
					ans, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if dec, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(ans)); derr == nil {
						s.mu.Lock()
						s.authNext = append(s.authNext, string(dec))
						s.mu.Unlock()
					}
				}
			}
			s.reply(w, 235, "accepted")
		case "MAIL":
			s.mu.Lock()
			s.mailFrom = strings.TrimPrefix(rest, "FROM:")
			fail := s.mailFailCode != 0
			s.mu.Unlock()
			if fail {
				s.reply(w, s.mailFailCode, "mailbox busy")
				continue
			}
			s.reply(w, 250, "ok")
		case "RCPT":
			s.mu.Lock()
			s.rcpts = append(s.rcpts, strings.TrimPrefix(rest, "TO:"))
			fail := s.rcptFailCode != 0 && !rcptFailNext
			rcptFailNext = true
			s.mu.Unlock()
			if fail {
				s.reply(w, s.rcptFailCode, "user unknown")
				continue
			}
			s.reply(w, 250, "ok")
		case "DATA":
			s.reply(w, 354, "go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" || l == ".\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.bodies = append(s.bodies, b.String())
			s.mu.Unlock()
			s.reply(w, 250, "accepted")
		case "QUIT":
			s.reply(w, 221, "bye")
			return
		default:
			s.reply(w, 502, "not implemented")
		}
	}
}

// snapshot 加锁读取记录。
func (s *fakeSMTPServer) snapshot() (authLine string, authNext []string, mailFrom string, rcpts, bodies []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authLine, append([]string(nil), s.authNext...), s.mailFrom, append([]string(nil), s.rcpts...), append([]string(nil), s.bodies...)
}

// smtpChannel 构造指向假服务端的 SMTP 渠道（host=127.0.0.1，端口取自监听地址）。
func smtpChannel(addr string) *model.Channel {
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	return &model.Channel{Provider: "smtp", SMTPHost: host, SMTPPort: port, SMTPTLS: "none"}
}

// smtpMsg 最小发送消息（from 缺省为空，供渠道默认发件人回退用例控制）。
func smtpMsg() *Message {
	return &Message{FromAddr: "s@example.com", To: []string{"to@example.com"}, Subject: "hello", Text: "hi"}
}

// TestSMTPSendSuccess 匿名发送成功：断言信封 from/rcpt（裸地址）与 DATA 正文（MIME 报文）。
func TestSMTPSendSuccess(t *testing.T) {
	srv := &fakeSMTPServer{t: t}
	addr := startFakeSMTP(t, srv)

	m := smtpMsg()
	m.To = []string{"Bob <to@example.com>"}
	m.Cc = []string{"cc@example.com"}
	res, perr := SMTP{}.Send(context.Background(), smtpChannel(addr), m)
	if perr != nil {
		t.Fatalf("expected success, got %+v", perr)
	}
	if !strings.HasPrefix(res.ProviderMessageID, "smtp-") {
		t.Fatalf("provider id = %q, want smtp-<uuid>", res.ProviderMessageID)
	}
	_, _, mailFrom, rcpts, bodies := srv.snapshot()
	if mailFrom != "<s@example.com>" {
		t.Errorf("MAIL FROM = %q, want <s@example.com>", mailFrom)
	}
	if len(rcpts) != 2 || rcpts[0] != "<to@example.com>" || rcpts[1] != "<cc@example.com>" {
		t.Errorf("RCPT TO = %v, want 信封裸地址 [to@example.com cc@example.com]（显示名只进 MIME 头）", rcpts)
	}
	if len(bodies) != 1 {
		t.Fatalf("DATA 次数 = %d, want 1", len(bodies))
	}
	assertContains(t, bodies[0], "subject: hello", "from: <s@example.com>", `to: "Bob" <to@example.com>`, "cc: <cc@example.com>")
}

// TestSMTPSendAuth 协商认证：服务器广告 PLAIN（默认选 PLAIN）与仅 LOGIN（选 LOGIN 两轮应答）。
func TestSMTPSendAuth(t *testing.T) {
	t.Run("plain_preferred", func(t *testing.T) {
		srv := &fakeSMTPServer{t: t, authMechs: "PLAIN LOGIN XCLIENT"}
		addr := startFakeSMTP(t, srv)
		ch := smtpChannel(addr)
		ch.SMTPUser, ch.SMTPPass = "user", "pass"
		if _, perr := (SMTP{}).Send(context.Background(), ch, smtpMsg()); perr != nil {
			t.Fatalf("expected success, got %+v", perr)
		}
		authLine, _, _, _, _ := srv.snapshot()
		if !strings.HasPrefix(authLine, "AUTH PLAIN ") {
			t.Fatalf("AUTH 行 = %q, want AUTH PLAIN <initial>", authLine)
		}
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(authLine, "AUTH PLAIN ")))
		if err != nil || string(dec) != "\x00user\x00pass" {
			t.Errorf("PLAIN 凭据 = %q (%v), want \\x00user\\x00pass", dec, err)
		}
	})

	t.Run("login_when_only_login_advertised", func(t *testing.T) {
		srv := &fakeSMTPServer{t: t, authMechs: "LOGIN"}
		addr := startFakeSMTP(t, srv)
		ch := smtpChannel(addr)
		ch.SMTPUser, ch.SMTPPass = "user", "pass"
		if _, perr := (SMTP{}).Send(context.Background(), ch, smtpMsg()); perr != nil {
			t.Fatalf("expected success, got %+v", perr)
		}
		authLine, authNext, _, _, _ := srv.snapshot()
		if !strings.HasPrefix(authLine, "AUTH LOGIN") {
			t.Fatalf("AUTH 行 = %q, want AUTH LOGIN", authLine)
		}
		if len(authNext) != 2 || authNext[0] != "user" || authNext[1] != "pass" {
			t.Errorf("LOGIN 应答 = %v, want [user pass]", authNext)
		}
	})
}

// TestSMTPSendChannelFromFallback 请求未带 from 时回退渠道默认发件人（与轮换路径 P1-4 同口径）。
func TestSMTPSendChannelFromFallback(t *testing.T) {
	srv := &fakeSMTPServer{t: t}
	addr := startFakeSMTP(t, srv)
	ch := smtpChannel(addr)
	ch.FromEmail, ch.FromName = "default@example.com", "默认发件"
	m := smtpMsg()
	m.FromAddr = "" // 请求未提供 from → 回退渠道默认
	if _, perr := (SMTP{}).Send(context.Background(), ch, m); perr != nil {
		t.Fatalf("expected success, got %+v", perr)
	}
	_, _, mailFrom, _, bodies := srv.snapshot()
	if mailFrom != "<default@example.com>" {
		t.Errorf("MAIL FROM = %q, want 回退渠道默认 <default@example.com>", mailFrom)
	}
	if len(bodies) == 1 {
		// 适配器内回退仅填 FromEmail（FromName 由轮换路径 P1-4 负责），头为裸地址
		assertContains(t, bodies[0], "from: <default@example.com>")
	}
}

// TestSMTPSendErrors 贯通 Send 的错误分类与参数校验分支。
func TestSMTPSendErrors(t *testing.T) {
	t.Run("missing_from", func(t *testing.T) {
		ch := smtpChannel("127.0.0.1:1") // 不真正拨号：参数校验在前
		m := smtpMsg()
		m.FromAddr = ""
		if _, perr := (SMTP{}).Send(context.Background(), ch, m); perr == nil || perr.Code != "bad_param" {
			t.Fatalf("缺少发件人应报 bad_param, got %+v", perr)
		}
	})

	t.Run("invalid_tls_mode", func(t *testing.T) {
		srv := &fakeSMTPServer{t: t}
		addr := startFakeSMTP(t, srv)
		ch := smtpChannel(addr)
		ch.SMTPTLS = "ssl" // 非法取值
		if _, perr := (SMTP{}).Send(context.Background(), ch, smtpMsg()); perr == nil || perr.Code != "bad_param" || !strings.Contains(perr.Message, "ssl") {
			t.Fatalf("非法 SMTPTLS 应报 bad_param（带原值）, got %+v", perr)
		}
	})

	t.Run("auth_535_disable_channel", func(t *testing.T) {
		srv := &fakeSMTPServer{t: t, authMechs: "PLAIN", authFailCode: 535}
		addr := startFakeSMTP(t, srv)
		ch := smtpChannel(addr)
		ch.SMTPUser, ch.SMTPPass = "u", "p"
		_, perr := SMTP{}.Send(context.Background(), ch, smtpMsg())
		if perr == nil || perr.Code != "auth_failed" || !perr.DisableChannel || perr.Retryable || perr.Cooldown {
			t.Fatalf("535 应分类 auth_failed+DisableChannel, got %+v", perr)
		}
	})

	t.Run("rcpt_550_param_class", func(t *testing.T) {
		srv := &fakeSMTPServer{t: t, rcptFailCode: 550}
		addr := startFakeSMTP(t, srv)
		_, perr := SMTP{}.Send(context.Background(), smtpChannel(addr), smtpMsg())
		if perr == nil || perr.Code != "smtp_rejected" || perr.Retryable || perr.DisableChannel || perr.Cooldown {
			t.Fatalf("550 应分类 smtp_rejected（参数类全 false）, got %+v", perr)
		}
	})

	t.Run("mail_429_cooldown", func(t *testing.T) {
		srv := &fakeSMTPServer{t: t, mailFailCode: 429}
		addr := startFakeSMTP(t, srv)
		_, perr := SMTP{}.Send(context.Background(), smtpChannel(addr), smtpMsg())
		if perr == nil || perr.Code != "rate_limited" || !perr.Retryable || !perr.Cooldown {
			t.Fatalf("429 应分类 rate_limited+Retryable+Cooldown, got %+v", perr)
		}
	})
}

// TestSMTPSendMimeVariants HTML-only 报文与超长正文（base64 分行）经真实 DATA 阶段投递。
func TestSMTPSendMimeVariants(t *testing.T) {
	srv := &fakeSMTPServer{t: t}
	addr := startFakeSMTP(t, srv)
	m := smtpMsg()
	m.HTML = "<p>" + strings.Repeat("长正文", 100) + "</p>" // 编码后 >76 字符，走 base64 分行
	m.Text = ""
	if _, perr := (SMTP{}).Send(context.Background(), smtpChannel(addr), m); perr != nil {
		t.Fatalf("expected success, got %+v", perr)
	}
	_, _, _, _, bodies := srv.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("DATA 次数 = %d, want 1", len(bodies))
	}
	assertContains(t, bodies[0], "content-type: text/html; charset=utf-8")
	// 正文完整可还原：base64 行独立解码应含原文（不因分行丢内容）
	for _, line := range strings.Split(bodies[0], "\r\n") {
		if dec, err := base64.StdEncoding.DecodeString(line); err == nil && strings.Contains(string(dec), "长正文") {
			return
		}
	}
	t.Error("HTML 正文未完整投递（base64 分行应可解码还原）")
}

// TestSMTPFormatAddressesFallback 解析失败回退原文（与信封裸地址口径独立）。
func TestSMTPFormatAddressesFallback(t *testing.T) {
	if got := smtpFormatAddresses([]string{"not-an-address", "a@example.com"}); got != "not-an-address, <a@example.com>" {
		t.Errorf("smtpFormatAddresses = %q, want 原文回退 + 规范地址", got)
	}
}
