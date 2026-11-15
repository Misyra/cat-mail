// tls.go SMTP 入口的传输安全能力：STARTTLS 协商升级（RFC 3207）与 TLS 策略。
// 隐式 TLS 监听（accept 后立即握手）在 smtpserver.go 的 serveConn 内；本文件
// 承载会话内的 STARTTLS 命令处理、策略常量与证书装载工具。
package smtpserver

import (
	"bufio"
	"crypto/tls"
	"log"
)

// SMTP TLS 策略（CATMAIL_SMTP_TLS_POLICY，与 config 层取值同口径）。
const (
	TLSPolicyOptional = "optional" // 明文监听广播 STARTTLS 但不强制（默认）
	TLSPolicyRequire  = "require"  // 明文监听必须 STARTTLS 升级后才允许 AUTH
)

// normalizeTLSPolicy 策略归一：空值 = optional；未识别取值兜底 optional 并记日志
// （config.Load 已快速失败，这里是对手工构造 Server 的防御）。
func normalizeTLSPolicy(p string) string {
	switch p {
	case TLSPolicyRequire:
		return TLSPolicyRequire
	case "", TLSPolicyOptional:
		return TLSPolicyOptional
	default:
		log.Printf("[smtp] unknown TLS policy %q, fallback to optional", p)
		return TLSPolicyOptional
	}
}

// NewTLSConfig 从证书/私钥路径构建服务端 TLS 配置：TLS 1.2 为最低版本（老旧
// 客户端协商出来的弱 TLS 对 AUTH 凭据是净风险）。证书装载失败返回错误——config
// 层已在启动时校验过一遍，这里再失败属引擎态异常，调用方直接致命退出。
func NewTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
	}, nil
}

// ---------- STARTTLS（RFC 3207） ----------

// cmdStartTLS 处理 STARTTLS：明文连接在证书已配置时协商升级。时机限 EHLO 之后、
// 邮件事务之前（顺序错误 503）；未配证书 502（未广播的命令按未知命令处理）；
// 已加密连接（隐式 TLS 监听）再来一次 503。回 220 后立即握手：成功则整套连接/
// 读写器替换为 TLS 版本并复位会话状态（RFC 3207 §4.2：TLS 前获取的认证与事务
// 状态全部丢弃，之后必须重新 EHLO），失败则断开——连接完整性已不可信，无继续
// 会话的意义。
func (se *session) cmdStartTLS(arg string) bool {
	if se.srv.tlsConfig == nil {
		se.reply(502, "5.5.1 STARTTLS not supported")
		return true
	}
	if se.isTLS {
		se.reply(503, "5.5.1 TLS already active")
		return true
	}
	if !se.greeted {
		se.reply(503, "5.5.1 Send HELO/EHLO first")
		return true
	}
	if se.haveMail {
		se.reply(503, "5.5.1 STARTTLS not permitted during mail transaction")
		return true
	}
	se.reply(220, "2.0.0 Ready to start TLS")
	tlsConn := tls.Server(se.conn, se.srv.tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		// 握手超时/失败：deadline 由 reply 设置（cmdTimeout 内）。
		log.Printf("[smtp] STARTTLS handshake from %s failed: %v", se.remote, err)
		return false
	}
	// 连接与读写器整体替换。旧 bufio 里若有 TLS 前残留的管线化数据即被丢弃
	// （RFC 3207 要求客户端收到 220 后先握手再发命令，管线化穿越 STARTTLS 属
	// 客户端违规，不做兼容）；丢弃旧缓冲也避免明文片段混入加密流。
	se.conn = tlsConn
	se.r = bufio.NewReaderSize(tlsConn, maxLineLen)
	se.w = bufio.NewWriter(tlsConn)
	se.isTLS = true
	// RFC 3207 §4.2 状态复位：问候/认证/邮件事务全部丢弃。
	se.greeted = false
	se.authed = false
	se.apiKeyID = 0
	se.resetTx()
	log.Printf("[smtp] STARTTLS negotiated (remote=%s)", se.remote)
	return true
}
