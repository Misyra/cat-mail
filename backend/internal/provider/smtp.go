// smtp.go 通用 SMTP 适配器（SPEC §3）：MIME multipart/mixed、PLAIN/LOGIN 认证（按服务器 advertised 机制选择）、none|starttls|tls、仅 socks5 代理。
package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/proxy"

	"catmail/internal/model"
)

type SMTP struct{}

func (SMTP) Name() string { return "smtp" }

// smtpPlainAuth 不校验 TLS 的 PLAIN 认证（自建服务器常见明文 25 端口认证）。
type smtpPlainAuth struct{ username, password string }

func (a smtpPlainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (smtpPlainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil // net/smtp 收到 235 成功后也会调一次 Next(more=false) 收尾，此时不是挑战
	}
	return nil, errors.New("smtp: unexpected challenge")
}

// smtpLoginAuth AUTH LOGIN 认证（部分服务器如 Exchange 仅支持 LOGIN）：两轮挑战应答，先用户名后密码。
type smtpLoginAuth struct {
	username, password string
	step               int
}

func (a *smtpLoginAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	a.step = 0
	return "LOGIN", nil, nil // LOGIN 无 initial response，走挑战应答
}

func (a *smtpLoginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.username), nil
	case 2:
		return []byte(a.password), nil
	default:
		return nil, errors.New("smtp: unexpected LOGIN challenge")
	}
}

// smtpAuthFor 按服务器 EHLO advertised 的 AUTH 机制选择认证方式：PLAIN 优先，仅支持 LOGIN 时用 LOGIN；
// 服务器未 advertise 时回退 PLAIN（保持旧行为，失败经 smtpClassifyError 归类）；username 为空返回 nil（匿名）。
func smtpAuthFor(c *smtp.Client, username, password string) smtp.Auth {
	if username == "" {
		return nil
	}
	if ok, mechs := c.Extension("AUTH"); ok && !authMechSupported(mechs, "PLAIN") && authMechSupported(mechs, "LOGIN") {
		return &smtpLoginAuth{username: username, password: password}
	}
	return smtpPlainAuth{username: username, password: password}
}

// authMechSupported 判断服务器 advertised 的 AUTH 机制列表（空格分隔）是否含某机制（大小写不敏感）。
func authMechSupported(mechs, want string) bool {
	for _, f := range strings.Fields(mechs) {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

// smtpDialerFor 按渠道代理配置构造拨号函数；空=直连，仅支持 socks5://。
func smtpDialerFor(proxyURL string) (func(context.Context, string, string) (net.Conn, error), *ProviderError) {
	if proxyURL == "" {
		d := &net.Dialer{Timeout: 30 * time.Second}
		return d.DialContext, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil || (u.Scheme != "socks5" && u.Scheme != "socks5h") {
		return nil, &ProviderError{Code: "bad_proxy", Message: "smtp 渠道仅支持 socks5 代理"}
	}
	d, err := proxy.FromURL(u, proxy.Direct)
	if err != nil {
		return nil, &ProviderError{Code: "bad_proxy", Message: "smtp 代理配置无效: " + err.Error()}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if cd, ok := d.(proxy.ContextDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		return d.Dial(network, addr)
	}, nil
}

func smtpTransaction(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), host, addr, mode string, user, pass string, envelopeFrom string, rcpts []string, body []byte) error {
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	// 整体超时；mode=tls 为 465 隐式 TLS，starttls 明文建连后升级，InsecureSkipVerify 均保持 false
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if mode == "tls" {
		conn = tls.Client(conn, &tls.Config{ServerName: host})
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()
	if mode == "starttls" {
		if err = c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if auth := smtpAuthFor(c, user, pass); auth != nil {
		if err = c.Auth(auth); err != nil {
			return err
		}
	}
	if err = c.Mail(envelopeFrom); err != nil { // envelope from=FromAddr
		return err
	}
	for _, r := range rcpts {
		if err = c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write(body); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	_ = c.Quit() // 消息已在 DATA 阶段被服务器接受
	return nil
}

// Send 通过 SMTP 发送一封邮件；SPEC：SMTP 无平台消息 ID，成功生成 smtp-<uuid>。
func (SMTP) Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError) {
	msg := *m
	if msg.FromAddr == "" {
		msg.FromAddr = ch.FromEmail
	}
	if msg.FromAddr == "" {
		return nil, &ProviderError{Code: "bad_param", Message: "缺少发件人地址（Message.FromAddr 或渠道 FromEmail）"}
	}
	rcpts := smtpEnvelopeRcpts(msg.To, msg.Cc, msg.Bcc)
	mode := strings.ToLower(strings.TrimSpace(ch.SMTPTLS))
	ports := map[string]int{"": 25, "none": 25, "starttls": 587, "tls": 465} // 空 SMTPTLS 按 none
	if _, ok := ports[mode]; !ok {
		return nil, &ProviderError{Code: "bad_param", Message: "SMTPTLS 取值无效: " + ch.SMTPTLS + "（应为 none|starttls|tls）"}
	}
	port := ch.SMTPPort
	if port == 0 {
		port = ports[mode]
	}
	body, err := buildMIME(&msg)
	if err != nil {
		return nil, &ProviderError{Code: "bad_param", Message: err.Error()}
	}
	dial, perr := smtpDialerFor(ch.ProxyURL)
	if perr != nil {
		return nil, perr
	}
	// SMTPUser 非空走认证（PLAIN/LOGIN 按服务器 advertised 机制协商）；空则匿名
	if err := smtpTransaction(ctx, dial, ch.SMTPHost, fmt.Sprintf("%s:%d", ch.SMTPHost, port), mode, ch.SMTPUser, ch.SMTPPass, msg.FromAddr, rcpts, body); err != nil {
		return nil, smtpClassifyError(err)
	}
	return &Result{ProviderMessageID: "smtp-" + uuid.NewString()}, nil
}

// smtpClassifyError 错误分类：网络/超时→Retryable；530/534/535 认证相关（需认证/机制过弱/认证失败）→DisableChannel；
// 其余 5xx 拒收→参数类（全 false）；4xx→Retryable（429 另加 Cooldown）。
func smtpClassifyError(err error) *ProviderError {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &ProviderError{Code: "timeout", Message: "smtp 连接超时: " + err.Error(), Retryable: true}
	}
	var te *textproto.Error
	if !errors.As(err, &te) {
		return &ProviderError{Code: "network_error", Message: err.Error(), Retryable: true}
	}
	pe := &ProviderError{Message: fmt.Sprintf("smtp %d %s", te.Code, te.Msg)}
	switch {
	case te.Code == 530 || te.Code == 534 || te.Code == 535: // 530 需要认证 / 534 认证机制过弱 / 535 认证失败：均为凭据问题，禁用渠道
		pe.Code, pe.DisableChannel = "auth_failed", true
	case te.Code == 429:
		pe.Code, pe.Retryable, pe.Cooldown = "rate_limited", true, true
	case te.Code >= 500: // 550/553 等拒收：参数类全 false
		pe.Code = "smtp_rejected"
	default:
		pe.Code, pe.Retryable = "smtp_temporary", true
	}
	return pe
}

// smtpEnvelopeRcpts 合并 to/cc/bcc 为信封收件人列表。dto 层保留 "Name <addr>" 原始串，
// 而 net/smtp 的 RCPT TO 会再包一层尖括号（RCPT TO:<addr>），故信封仅取裸地址；
// 显示名只进 MIME 的 To/Cc 头（buildMIME），不进信封。
func smtpEnvelopeRcpts(lists ...[]string) []string {
	out := make([]string, 0)
	for _, l := range lists {
		for _, a := range l {
			out = append(out, extractBareAddress(a))
		}
	}
	return out
}

// buildMIME 构建完整 MIME 报文（CRLF 行结尾，供 DATA 阶段发送）。
func buildMIME(m *Message) ([]byte, error) {
	var b bytes.Buffer
	writeHeader := func(k, v string) { // 头值 CR/LF 换空格防注入
		b.WriteString(k + ": " + strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(v, "\r", " "), "\n", " ")) + "\r\n")
	}
	// 经 net/mail.Address.String() 处理显示名的引号转义与 RFC 2047 编码，避免特殊字符拼出非法头
	from := (&mail.Address{Name: m.FromName, Address: m.FromAddr}).String()
	writeHeader("From", from)
	if len(m.To) > 0 {
		writeHeader("To", smtpFormatAddresses(m.To))
	}
	if len(m.Cc) > 0 {
		writeHeader("Cc", smtpFormatAddresses(m.Cc))
	}
	if len(m.ReplyTo) > 0 {
		writeHeader("Reply-To", smtpFormatAddresses(m.ReplyTo))
	}
	writeHeader("Subject", smtpEncodeHeader(m.Subject))
	writeHeader("Date", time.Now().Format(time.RFC1123Z))
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	writeHeader("Message-ID", "<smtp-"+uuid.NewString()+"@"+host+">")
	writeHeader("MIME-Version", "1.0")
	reserved := " content-type content-transfer-encoding mime-version date message-id from to cc bcc subject reply-to "
	for k, v := range m.Headers { // 自定义头（跳过结构性头）
		if !strings.Contains(reserved, " "+strings.ToLower(k)+" ") {
			writeHeader(k, v)
		}
	}
	mixed := "=_sm_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	writeHeader("Content-Type", `multipart/mixed; boundary="`+mixed+`"`)
	b.WriteString("\r\n")
	if m.Text != "" && m.HTML != "" {
		alt := "=_sm_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		fmt.Fprintf(&b, "--%s\r\nContent-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", alt, alt)
		smtpWriteBase64Part(&b, alt, "text/plain; charset=utf-8", "", []byte(m.Text))
		smtpWriteBase64Part(&b, alt, "text/html; charset=utf-8", "", []byte(m.HTML))
		fmt.Fprintf(&b, "--%s--\r\n", alt)
	} else if m.HTML != "" {
		smtpWriteBase64Part(&b, mixed, "text/html; charset=utf-8", "", []byte(m.HTML))
	} else { // 空 HTML 只发 text
		smtpWriteBase64Part(&b, mixed, "text/plain; charset=utf-8", "", []byte(m.Text))
	}
	for _, a := range m.Attachments {
		content, derr := decodeBase64(a.ContentBase64) // 容错中间空白字符
		if derr != nil {
			return nil, fmt.Errorf("附件 %q base64 解码失败: %w", a.Filename, derr)
		}
		ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(a.Filename)))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		nameCT := mime.FormatMediaType(ctype, map[string]string{"name": a.Filename}) // 中文文件名走 RFC2231
		if nameCT == "" {
			nameCT = ctype
		}
		disp := mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})
		smtpWriteBase64Part(&b, mixed, nameCT, disp, content)
	}
	fmt.Fprintf(&b, "--%s--\r\n", mixed)
	return b.Bytes(), nil
}

func smtpWriteBase64Part(b *bytes.Buffer, boundary, contentType, disposition string, data []byte) {
	fmt.Fprintf(b, "--%s\r\nContent-Type: %s\r\n", boundary, contentType)
	if disposition != "" {
		fmt.Fprintf(b, "Content-Disposition: %s\r\n", disposition)
	}
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b64 := base64.StdEncoding.EncodeToString(data)
	for len(b64) > 76 {
		b.WriteString(b64[:76] + "\r\n")
		b64 = b64[76:]
	}
	b.WriteString(b64 + "\r\n")
}

func smtpEncodeHeader(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return r > 127 }) >= 0 {
		return mime.BEncoding.Encode("utf-8", s)
	}
	return s
}

// smtpFormatAddresses 逐个地址经 net/mail 解析后按 RFC 5322/2047 重排（引号转义与非 ASCII 显示名编码），
// 与 From 头同一处理；解析失败回退原文，交由服务器校验。
func smtpFormatAddresses(list []string) string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		if addr, err := mail.ParseAddress(strings.TrimSpace(a)); err == nil {
			out = append(out, addr.String())
			continue
		}
		out = append(out, a)
	}
	return strings.Join(out, ", ")
}

func init() {
	Register(SMTP{})
}
