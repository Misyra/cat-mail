// smtpserver_tls_test.go SMTP 入口 TLS 能力测试：自签临时证书起服务，覆盖隐式
// TLS 监听握手与全流程、STARTTLS 协商（含升级后状态复位）、require 策略下明文
// AUTH 被拒与升级后放行、未配证书不广播 STARTTLS。
package smtpserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
)

// genSelfSignedCert 生成自签证书/私钥写入临时目录（ECDSA P-256，IP SAN 含
// 127.0.0.1），返回文件路径。握手用 InsecureSkipVerify 或配 IP SAN 均可通过。
func genSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "catmail-smtp-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// startTLSServer 起一个带证书的 SMTP 服务：withPlain 启用明文监听（广播
// STARTTLS），withImplicit 启用隐式 TLS 监听；policy 为 STARTTLS 下的 AUTH 策略。
// 地址均用 127.0.0.1:0 随机端口。
func startTLSServer(t *testing.T, db *gorm.DB, policy string, withPlain, withImplicit bool) *Server {
	t.Helper()
	certFile, keyFile := genSelfSignedCert(t)
	tlsCfg, err := NewTLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatalf("new tls config: %v", err)
	}
	addr, tlsAddr := "", ""
	if withPlain {
		addr = "127.0.0.1:0"
	}
	if withImplicit {
		tlsAddr = "127.0.0.1:0"
	}
	s := New(db, addr, 0)
	s.WithTLS(tlsAddr, tlsCfg, policy)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)
	return s
}

// dialImplicitTLS 拨通隐式 TLS 端口并完成客户端侧握手。
func dialImplicitTLS(t *testing.T, addr string) *tls.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls dial %s: %v", addr, err)
	}
	return conn
}

// ---------- 隐式 TLS 监听（465 风格） ----------

func TestImplicitTLSFullFlow(t *testing.T) {
	resetCapture()
	db := newTestDB(t)
	if err := db.Create(&model.Channel{Name: "cap", Provider: "capture", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	s := startTLSServer(t, db, TLSPolicyOptional, false, true)
	if s.Addr() != "" {
		t.Errorf("plain addr should be empty for implicit-only server, got %q", s.Addr())
	}

	conn := dialImplicitTLS(t, s.TLSAddr())
	c, err := smtp.NewClient(conn, "127.0.0.1")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Hello("client.local"); err != nil {
		t.Fatalf("EHLO over TLS: %v", err)
	}
	// 任务契约：证书配置后两种监听的 EHLO 均广播 STARTTLS（隐式 TLS 内真发起
	// 会被 503 拒绝，见下方原始客户端断言）。
	if ok, _ := c.Extension("STARTTLS"); !ok {
		t.Fatal("EHLO should advertise STARTTLS when certificate is configured")
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
	if _, err := w.Write([]byte("Subject: via implicit tls\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("end data: %v", err)
	}
	if err := c.Quit(); err != nil {
		t.Fatalf("QUIT: %v", err)
	}
	capture.mu.Lock()
	n := len(capture.msgs)
	capture.mu.Unlock()
	if n != 1 {
		t.Fatalf("implicit TLS send delivered %d messages, want 1", n)
	}
}

// 非 TLS 客户端直连隐式 TLS 端口：握手失败即断开（不回 220 greeting）。
func TestImplicitTLSRejectsPlainDial(t *testing.T) {
	db := newTestDB(t)
	s := startTLSServer(t, db, TLSPolicyOptional, false, true)
	conn, err := net.Dial("tcp", s.TLSAddr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// 服务端等客户端 ClientHello，客户端等服务端 greeting：双方互等直到超时/
	// 握手失败。写一行明文触发服务端握手错误后连接关闭，读到 EOF。
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Write([]byte("EHLO plain\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	if n, err := conn.Read(buf); err == nil && n > 0 {
		t.Fatalf("plain client should be rejected, got reply %q", buf[:n])
	}
}

// ---------- STARTTLS 协商（RFC 3207） ----------

func TestSTARTTLSNegotiation(t *testing.T) {
	resetCapture()
	db := newTestDB(t)
	if err := db.Create(&model.Channel{Name: "cap", Provider: "capture", Status: model.ChannelEnabled}).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	goodKey := createKey(t, db, nil)
	s := startTLSServer(t, db, TLSPolicyOptional, true, false)

	// 升级前：STARTTLS 已广播；认证后发信全流程走通。
	c, err := smtp.NewClient(mustDial(t, s.Addr()), "127.0.0.1")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Hello("client.local"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		t.Fatal("EHLO should advertise STARTTLS when certificate is configured")
	}
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	// 升级后重新 EHLO：按任务契约仍广播 STARTTLS 扩展（真发起则 503，见
	// TestSTARTTLSInsideTLSRejected）。
	if ok, _ := c.Extension("STARTTLS"); !ok {
		t.Fatal("EHLO after STARTTLS should still advertise STARTTLS (契约口径)")
	}
	// 升级后 AUTH 可用（optional 策略不强制，但加密链路照常支持）。
	if err := c.Auth(smtp.PlainAuth("", "anyuser", goodKey, "127.0.0.1")); err != nil {
		t.Fatalf("AUTH after STARTTLS: %v", err)
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
	if _, err := w.Write([]byte("Subject: via starttls\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("end data: %v", err)
	}
	capture.mu.Lock()
	n := len(capture.msgs)
	capture.mu.Unlock()
	if n != 1 {
		t.Fatalf("STARTTLS send delivered %d messages, want 1", n)
	}
}

// 隐式 TLS 会话内发起 STARTTLS：按任务契约扩展虽广播，但不允许二次协商，503。
func TestSTARTTLSInsideTLSRejected(t *testing.T) {
	db := newTestDB(t)
	s := startTLSServer(t, db, TLSPolicyOptional, false, true)
	conn := dialImplicitTLS(t, s.TLSAddr())
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
	if err := tp.PrintfLine("STARTTLS"); err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	if code, _, err := tp.ReadResponse(503); err != nil || code != 503 {
		t.Fatalf("STARTTLS inside TLS: code=%d err=%v, want 503", code, err)
	}
}

// STARTTLS 时序与状态复位：未 EHLO 503、邮件事务中 503、升级后旧状态丢弃
// （升级前 AUTH 的会话升级后需重新 EHLO + 重新 AUTH）。
func TestSTARTTLSOrderingAndStateReset(t *testing.T) {
	db := newTestDB(t)
	s := startTLSServer(t, db, TLSPolicyOptional, true, false)

	conn := mustDial(t, s.Addr())
	defer conn.Close()
	tp := textproto.NewConn(conn)
	if _, _, err := tp.ReadResponse(220); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	// 未 EHLO 先 STARTTLS：503。
	if err := tp.PrintfLine("STARTTLS"); err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	if code, _, err := tp.ReadResponse(503); err != nil || code != 503 {
		t.Fatalf("STARTTLS before EHLO: code=%d err=%v, want 503", code, err)
	}
	if err := tp.PrintfLine("EHLO test"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if _, _, err := tp.ReadResponse(250); err != nil {
		t.Fatalf("EHLO reply: %v", err)
	}
	// 邮件事务中 STARTTLS：503。
	if err := tp.PrintfLine("MAIL FROM:<a@example.com>"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if _, _, err := tp.ReadResponse(250); err != nil {
		t.Fatalf("MAIL reply: %v", err)
	}
	if err := tp.PrintfLine("STARTTLS"); err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	if code, _, err := tp.ReadResponse(503); err != nil || code != 503 {
		t.Fatalf("STARTTLS during transaction: code=%d err=%v, want 503", code, err)
	}
	if err := tp.PrintfLine("RSET"); err != nil {
		t.Fatalf("RSET: %v", err)
	}
	if _, _, err := tp.ReadResponse(250); err != nil {
		t.Fatalf("RSET reply: %v", err)
	}
	// 正常升级：220 → TLS 握手 → 旧 textproto 作废，换 TLS 版本继续。
	if err := tp.PrintfLine("STARTTLS"); err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	if _, _, err := tp.ReadResponse(220); err != nil {
		t.Fatalf("220 reply: %v", err)
	}
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	tp = textproto.NewConn(tlsConn)
	if err := tp.PrintfLine("EHLO test"); err != nil {
		t.Fatalf("EHLO after upgrade: %v", err)
	}
	if _, _, err := tp.ReadResponse(250); err != nil { // 多行应答一次读完
		t.Fatalf("EHLO reply: %v", err)
	}
	// 升级后会话状态已复位：无需 AUTH（loopback 免认证语义不变）即可 MAIL。
	if err := tp.PrintfLine("MAIL FROM:<a@example.com>"); err != nil {
		t.Fatalf("MAIL after upgrade: %v", err)
	}
	if code, _, err := tp.ReadResponse(250); err != nil || code != 250 {
		t.Fatalf("MAIL after upgrade: code=%d err=%v, want 250", code, err)
	}
}

// ---------- require 策略：明文 AUTH 被拒，升级后放行 ----------

func TestTLSPolicyRequireRejectsPlaintextAuth(t *testing.T) {
	db := newTestDB(t)
	goodKey := createKey(t, db, nil)
	s := startTLSServer(t, db, TLSPolicyRequire, true, false)
	s.forceAuth = true // 非 loopback 对端语义：RCPT 闸门与策略闸门正交并存
	plain := base64.StdEncoding.EncodeToString([]byte("\x00anyuser\x00" + goodKey))

	// 明文连接 AUTH：530 Must issue a STARTTLS command first（凭据正确也不放行
	// ——闸门在凭据校验之前）。
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
	if err := tp.PrintfLine("AUTH PLAIN %s", plain); err != nil {
		t.Fatalf("AUTH PLAIN: %v", err)
	}
	if code, _, err := tp.ReadResponse(530); err != nil || code != 530 {
		t.Fatalf("plaintext AUTH under require policy: code=%d err=%v, want 530", code, err)
	}

	// STARTTLS 升级成功后：同一策略下 AUTH 放行（隐式豁免语义的镜像验证）。
	if err := tp.PrintfLine("STARTTLS"); err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	if _, _, err := tp.ReadResponse(220); err != nil {
		t.Fatalf("220 reply: %v", err)
	}
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	tp = textproto.NewConn(tlsConn)
	if err := tp.PrintfLine("EHLO test"); err != nil {
		t.Fatalf("EHLO after upgrade: %v", err)
	}
	if _, _, err := tp.ReadResponse(250); err != nil {
		t.Fatalf("EHLO reply: %v", err)
	}
	if err := tp.PrintfLine("AUTH PLAIN %s", plain); err != nil {
		t.Fatalf("AUTH PLAIN after upgrade: %v", err)
	}
	if code, _, err := tp.ReadResponse(235); err != nil || code != 235 {
		t.Fatalf("AUTH after STARTTLS under require policy: code=%d err=%v, want 235", code, err)
	}
	// 认证 + 非 loopback 闸门并存：RCPT 放行、发信可走通。
	if err := tp.PrintfLine("MAIL FROM:<a@example.com>"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if _, _, err := tp.ReadResponse(250); err != nil {
		t.Fatalf("MAIL reply: %v", err)
	}
	if err := tp.PrintfLine("RCPT TO:<b@example.com>"); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	if code, _, err := tp.ReadResponse(250); err != nil || code != 250 {
		t.Fatalf("RCPT after auth: code=%d err=%v, want 250", code, err)
	}
}

// 隐式 TLS 监听天然豁免 require 策略：TLS 会话内 AUTH 直接放行。
func TestTLSPolicyRequireExemptsImplicitTLS(t *testing.T) {
	db := newTestDB(t)
	goodKey := createKey(t, db, nil)
	s := startTLSServer(t, db, TLSPolicyRequire, false, true)

	conn := dialImplicitTLS(t, s.TLSAddr())
	c, err := smtp.NewClient(conn, "127.0.0.1")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Hello("client.local"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if err := c.Auth(smtp.PlainAuth("", "anyuser", goodKey, "127.0.0.1")); err != nil {
		t.Fatalf("AUTH on implicit TLS under require policy: %v", err)
	}
}

// ---------- 未配证书：维持纯明文现状 ----------

func TestSTARTTLSNotAdvertisedWithoutCert(t *testing.T) {
	db := newTestDB(t)
	s := startServer(t, db, 0) // 无 TLS 配置（既有明文入口形态）
	c, err := smtp.NewClient(mustDial(t, s.Addr()), "127.0.0.1")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Hello("client.local"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Fatal("EHLO must not advertise STARTTLS without certificate")
	}
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
	if err := tp.PrintfLine("STARTTLS"); err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	if code, _, err := tp.ReadResponse(502); err != nil || code != 502 {
		t.Fatalf("STARTTLS without cert: code=%d err=%v, want 502", code, err)
	}
}

// 隐式 TLS + 明文双监听并存：同一实例两个入口各自工作（优雅关停双双关闭）。
func TestDualListenersStop(t *testing.T) {
	db := newTestDB(t)
	s := startTLSServer(t, db, TLSPolicyOptional, true, true)
	if s.Addr() == "" || s.TLSAddr() == "" {
		t.Fatalf("both listeners required: plain=%q tls=%q", s.Addr(), s.TLSAddr())
	}
	plainConn := mustDial(t, s.Addr())
	defer plainConn.Close()
	if _, _, err := textproto.NewConn(plainConn).ReadResponse(220); err != nil {
		t.Fatalf("plain greeting: %v", err)
	}
	tlsConn := dialImplicitTLS(t, s.TLSAddr())
	defer tlsConn.Close()
	if _, _, err := textproto.NewConn(tlsConn).ReadResponse(220); err != nil {
		t.Fatalf("TLS greeting: %v", err)
	}
	s.Stop()
	// 关停后两个入口均不再接受新连接。
	if _, err := net.DialTimeout("tcp", s.Addr(), time.Second); err == nil {
		t.Error("plain listener should be closed after Stop")
	}
	if _, err := net.DialTimeout("tcp", s.TLSAddr(), time.Second); err == nil {
		t.Error("TLS listener should be closed after Stop")
	}
	if !s.isClosed() {
		t.Error("server should be marked closed")
	}
	// 重复 Stop 安全。
	s.Stop()
}
