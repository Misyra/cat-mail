// Package smtpserver 实现定位级 SMTP 入口（E1）：让只支持 SMTP 的本地应用把
// CatMail 当普通 SMTP 服务器用，DATA 收完后解析 MIME 消息并转交 rotator 走
// 渠道轮换发送（不重造发送语义：额度/限速/failover/日志全部复用轮换器现状）。
//
// 范围与取舍（刻意保持简洁）：
//   - 传输安全（TLS）：证书（CATMAIL_SMTP_CERT_FILE/KEY_FILE）配置后提供两种
//     加密入口——隐式 TLS 监听（465 风格，CATMAIL_SMTP_TLS_ADDR，accept 后立即
//     tls.Server 包装再进会话）与明文监听上的 STARTTLS 协商升级（RFC 3207）；
//     两种监听的 EHLO 均广播 STARTTLS 扩展（隐式 TLS 会话内真发起该命令以 503
//     拒绝，不做二次协商）。CATMAIL_SMTP_TLS_POLICY=require 时明文监听必须先
//     STARTTLS 才允许 AUTH（防明文密码泄露，隐式 TLS 监听天然豁免）。未配证书
//     维持纯明文现状，不广播 STARTTLS——公网加密端口也可继续由反向代理/隧道
//     （stunnel、frp、cloudflared 等）承担 TLS 终结后转发到本入口。
//   - 防开放中继（重点安全设计）：按「对端地址是否 loopback」逐连接判定——
//     loopback（127.0.0.0/8、::1、localhost）允许无认证（本地应用零配置接入）；
//     非 loopback 必须 AUTH（RCPT 前 530 拦截），凭据 = password 是某个 enabled
//     的 ApiKey（username 任意），命中记 ApiKeyID 用于日志归属与 Key 级限速/日
//     额度。逐连接判定而非只看监听地址：监听 0.0.0.0 时环回客户端可免认证、
//     内网/远程客户端必须 AUTH，同一实例两种策略并存，不会意外变成开放中继。
//   - EHLO 扩展：PIPELINING、SIZE（上限=配置值）、8BITMIME、AUTH PLAIN LOGIN、
//     STARTTLS（证书已配置且连接尚未加密时）。
//   - 会话健壮性：每连接独立 goroutine；单命令读写 60s、单连接总时长 5 分钟
//     deadline；handler panic recover 不带崩进程；优雅关停（关 listener + 有限
//     等待 + 强制关闭在途连接，风格对齐 scheduler.Stop）。
package smtpserver

import (
	"crypto/tls"
	"errors"
	"log"
	"net"
	"sync"
	"time"

	"gorm.io/gorm"
)

// 会话与关停参数（包级常量，测试依赖超时行为的用例可内联使用）。
const (
	// sessionTimeout 单连接总时长上限：到期后下一次读写即超时断开（防连接悬挂
	// 占住 goroutine 与 fd）。
	sessionTimeout = 5 * time.Minute
	// cmdTimeout 单命令（一次行读 / 一次应答写）的超时，同时受 sessionTimeout 截断。
	cmdTimeout = 60 * time.Second
	// maxLineLen 单行长度上限（bufio 缓冲即上限）：远大于 RFC 5321 的 998 字符，
	// 容纳 8BITMIME 长 binhex 行；超过即 500 并断开，防单行无限膨胀吃内存。
	maxLineLen = 64 * 1024
	// maxRcpt 单邮件事务收件人上限（与 API 层 to+cc+bcc ≤100 对齐）。
	maxRcpt = 100
	// stopGrace 优雅关停先给在途会话的自然收尾窗口（在途 rotator.Send 含平台
	// 请求与落库，无法中断只能等待）；
	// stopForce 强制关闭剩余连接后的兜底等待。总预算 5s，对齐 main 的 HTTP
	// Shutdown 超时与 scheduler.Stop 的「有限等待 + 超时记日志」风格。
	stopGrace = 3 * time.Second
	stopForce = 2 * time.Second
)

// Server SMTP 入口服务：监听 addr（可选再监听 tlsAddr 隐式 TLS 端口），把会话
// 收下的邮件转交 rotator。
type Server struct {
	db       *gorm.DB
	addr     string
	maxBytes int64 // 单封邮件 DATA 载荷上限（字节），SIZE 广播与 552 共用
	// forceAuth 测试注入：恒定要求 AUTH（等价于把所有对端视为非 loopback），
	// 便于在单进程内验证「非 loopback 必须 AUTH」策略，无需真实非环回链路。
	forceAuth bool

	// TLS 能力（WithTLS 配置）：tlsConfig 非 nil = 证书就绪，明文监听广播
	// STARTTLS、tlsAddr 监听做隐式 TLS；tlsPolicy 为 STARTTLS 下的 AUTH 策略
	// （TLSPolicy* 常量，require 时明文连接 AUTH 前必须升级成功）。
	tlsAddr   string
	tlsConfig *tls.Config
	tlsPolicy string

	limiter keyLimiter // Key 级出站限速（B5 rate_per_sec 在 SMTP 路径的实现）

	ln     net.Listener // 明文监听（addr 为空时为 nil，仅 TLS 入口形态）
	tlsLn  net.Listener // 隐式 TLS 监听
	mu     sync.Mutex
	conns  map[net.Conn]struct{} // 在途连接登记（Stop 时强制关闭用）
	wg     sync.WaitGroup        // accept 循环 + 每连接 goroutine
	closed bool
}

// New 创建 SMTP 入口服务（不监听；Start 才开始）。maxBytes 为单封邮件上限
// （字节），非正值兜底默认 25MB（SIZE 广播与 552 拒收共用，0 上限无意义）。
// TLS 能力经 WithTLS 追加配置（证书缺失时默认纯明文，维持既有调用形态）。
func New(db *gorm.DB, addr string, maxBytes int64) *Server {
	if maxBytes <= 0 {
		maxBytes = 25 << 20
	}
	return &Server{
		db:       db,
		addr:     addr,
		maxBytes: maxBytes,
		conns:    make(map[net.Conn]struct{}),
	}
}

// WithTLS 配置 TLS 能力（Start 前调用，链式返回）：tlsAddr 非空启用隐式 TLS
// 监听（465 风格）；tlsConfig 非 nil 启用 STARTTLS 广播与隐式握手；policy 为
// STARTTLS 下的 AUTH 策略（TLSPolicy* 常量，未识别取值兜底 optional 并记日志）。
func (s *Server) WithTLS(tlsAddr string, tlsConfig *tls.Config, policy string) *Server {
	s.tlsAddr = tlsAddr
	s.tlsConfig = tlsConfig
	s.tlsPolicy = normalizeTLSPolicy(policy)
	return s
}

// Start 监听并开始接受连接（后台 goroutine，不阻塞）。addr 为空跳过明文监听
// （仅隐式 TLS 入口形态）；监听失败返回错误（如端口被占用），调用方应视为致命
// ——对齐 main 里 HTTP listen 的处理姿态。TLS 监听失败时回滚已建立的明文监听，
// 不留下半启动态。
func (s *Server) Start() error {
	if s.addr != "" {
		ln, err := net.Listen("tcp", s.addr)
		if err != nil {
			return err
		}
		s.ln = ln
		s.wg.Add(1)
		go s.acceptLoop(ln, false)
		log.Printf("[smtp] listening on %s (max_size=%d bytes, starttls=%v)", s.addr, s.maxBytes, s.tlsConfig != nil)
	}
	if s.tlsAddr != "" {
		if s.tlsConfig == nil {
			return errors.New("smtp: TLS 监听地址已配置但未提供 TLS 证书")
		}
		tln, err := net.Listen("tcp", s.tlsAddr)
		if err != nil {
			if s.ln != nil {
				_ = s.ln.Close()
			}
			return err
		}
		s.tlsLn = tln
		s.wg.Add(1)
		go s.acceptLoop(tln, true)
		log.Printf("[smtp] TLS listening on %s (implicit TLS, max_size=%d bytes)", s.tlsAddr, s.maxBytes)
	}
	return nil
}

// Addr 返回明文监听实际地址（测试用 127.0.0.1:0 启动后取真实端口；未 Start 或
// 未监听为空）。
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// TLSAddr 返回隐式 TLS 监听实际地址（未启用/未 Start 为空）。
func (s *Server) TLSAddr() string {
	if s.tlsLn == nil {
		return ""
	}
	return s.tlsLn.Addr().String()
}

// Stop 优雅关停：先关 listener（不再接受新连接），给在途会话 stopGrace 自然收尾；
// 仍存活的连接强制关闭（客户端侧表现为连接中断，按 SMTP 临时失败语义自行重试），
// 再兜底等待 stopForce。重复/未 Start 调用安全。风格对齐 scheduler.Stop：有限
// 等待 + 超时记日志，不 panic 不阻塞进程退出超过预算。
func (s *Server) Stop() {
	s.mu.Lock()
	if s.closed || (s.ln == nil && s.tlsLn == nil) {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ln, tln := s.ln, s.tlsLn
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if tln != nil {
		_ = tln.Close()
	}

	if s.waitConns(stopGrace) {
		return
	}
	log.Printf("[smtp] shutdown: %v 后仍有在途连接，强制关闭", stopGrace)
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close() // 会话读写立即出错退出；在途 rotator.Send 由 ctx 取消收尾
	}
	s.mu.Unlock()
	if !s.waitConns(stopForce) {
		log.Printf("[smtp] shutdown: 强制关闭后 %v 内仍有会话未退出（多为平台请求/DB 写收尾）", stopForce)
	}
}

// waitConns 等待全部连接 goroutine 结束，最多 d；返回 true = 全部退出。
func (s *Server) waitConns(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// acceptLoop 接受循环：每连接一个 goroutine（并发模型与 HTTP 层同构）。
// implicitTLS 标记来自隐式 TLS 监听：accept 后先做服务端 TLS 握手再进会话。
func (s *Server) acceptLoop(ln net.Listener, implicitTLS bool) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.isClosed() {
				return // 关停引发的 accept 错误是正常退出路径
			}
			log.Printf("[smtp] accept: %v", err)
			return
		}
		s.track(conn)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			s.serveConn(conn, implicitTLS)
		}()
	}
}

// serveConn 单连接入口：隐式 TLS 监听先握手（失败即断开——客户端非 TLS 接入或
// 证书校验不过，无会话可言）；panic recover 不带崩进程（O8 同款兜底），无论
// 正常/异常退出最终都关闭连接。
func (s *Server) serveConn(conn net.Conn, implicitTLS bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[smtp] panic serving %s: %v", conn.RemoteAddr(), r)
		}
		_ = conn.Close()
	}()
	if implicitTLS && s.tlsConfig != nil {
		// 握手限时 cmdTimeout（与单命令同量级；session 的 per-op deadline 在
		// 握手成功后由会话自行接管）。
		_ = conn.SetDeadline(time.Now().Add(cmdTimeout))
		tlsConn := tls.Server(conn, s.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			log.Printf("[smtp] TLS handshake from %s failed: %v", conn.RemoteAddr(), err)
			return
		}
		conn = tlsConn
	}
	se := newSession(s, conn)
	if implicitTLS {
		se.isTLS = true
	}
	se.serve()
}

func (s *Server) track(conn net.Conn) {
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// requireAuth 该对端是否必须 AUTH（防开放中继的核心判定，封装成独立方法供
// 测试注入 forceAuth）：非 loopback 对端必须；对端 IP 解析失败从严要求。
func (s *Server) requireAuth(remoteIP string) bool {
	if s.forceAuth {
		return true
	}
	return !isLoopbackHost(remoteIP)
}
