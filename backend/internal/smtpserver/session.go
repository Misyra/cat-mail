// session.go SMTP 会话状态机（RFC 5321 子集，基于 textproto 思路手写——标准库
// 只有 SMTP 客户端，没有服务端）。状态流：greeting → HELO/EHLO → [AUTH] →
// MAIL → RCPT* → DATA →（转交 rotator）→ 复位可继续下一封；RSET 回到 MAIL 前，
// QUIT 结束。逐行读逐行回，天然兼容 PIPELINING（命令连发时响应按序写回）。
package smtpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/rotator"
)

// errLineTooLong 行超长：连接已不可靠（无法与客户端重新同步行边界），读循环
// 收到后直接断开。
var errLineTooLong = errors.New("smtp: line too long")

// session 单个 SMTP 会话（一连接一实例，串行处理命令，无并发状态）。
type session struct {
	srv      *Server
	conn     net.Conn
	r        *bufio.Reader
	w        *bufio.Writer
	ctx      context.Context    // rotator.Send 用：连接被关停强断时取消在途平台请求
	cancel   context.CancelFunc // ctx 的取消（STARTTLS 换连接后 cancelConn 仍可达）
	remote   string             // 对端地址（日志用）
	remoteIP string             // 对端 IP（requireAuth 判定）
	endAt    time.Time          // 会话总时长上限

	// 会话状态
	greeted  bool   // 已 HELO/EHLO
	authed   bool   // 已通过 AUTH（PLAIN/LOGIN）
	isTLS    bool   // 连接已 TLS 加密（隐式 TLS 监听天然为真，明文监听 STARTTLS 升级后置位）
	apiKeyID uint   // 认证命中的 ApiKey（0 = 未认证 / loopback 免认证）
	mailFrom string // envelope 反向路径
	haveMail bool   // 已收到 MAIL（区分「空路径 <>」与「尚未 MAIL」）
	rcpts    []string
}

func newSession(s *Server, conn net.Conn) *session {
	ctx, cancel := context.WithCancel(context.Background())
	// cancel 挂在连接上：Stop 强制关闭连接（或客户端离开）时，在途的
	// rotator.Send（平台 HTTP 请求等）随之取消，不让已死的连接拖住关停。
	// cancel 同时记在会话字段上：STARTTLS 升级会整体替换 conn（不再是最初的
	// cancellingConn），退出路径经 cancelConn 仍能触达取消。
	conn = &cancellingConn{Conn: conn, cancel: cancel}
	se := &session{
		srv:      s,
		conn:     conn,
		r:        bufio.NewReaderSize(conn, maxLineLen),
		w:        bufio.NewWriter(conn),
		ctx:      ctx,
		cancel:   cancel,
		remote:   conn.RemoteAddr().String(),
		remoteIP: remoteIPOf(conn.RemoteAddr()),
	}
	return se
}

// serve 会话主循环：回 greeting → 逐行读命令分发，直到 QUIT/错误/超时退出。
func (se *session) serve() {
	defer se.cancelConn()
	se.endAt = time.Now().Add(sessionTimeout)
	se.reply(220, "catmail ESMTP ready")
	for {
		line, err := se.readLine()
		if err != nil {
			return // 读超时/EOF/行超长：超长已在 readLine 内回过响应
		}
		if !se.dispatch(line) {
			return
		}
	}
}

func (se *session) cancelConn() {
	if se.cancel != nil {
		se.cancel()
	}
}

// dispatch 分发一行命令；返回 false 表示会话结束（QUIT 或致命错误）。
func (se *session) dispatch(line string) bool {
	verb, arg := splitCommand(line)
	switch strings.ToUpper(verb) {
	case "HELO":
		return se.cmdHelo(arg, false)
	case "EHLO":
		return se.cmdHelo(arg, true)
	case "STARTTLS":
		return se.cmdStartTLS(arg)
	case "AUTH":
		return se.cmdAuth(arg)
	case "MAIL":
		return se.cmdMail(arg)
	case "RCPT":
		return se.cmdRcpt(arg)
	case "DATA":
		return se.cmdData()
	case "RSET":
		se.resetTx()
		se.reply(250, "2.0.0 OK")
	case "NOOP":
		se.reply(250, "2.0.0 OK")
	case "QUIT":
		se.reply(221, "2.0.0 Bye")
		return false
	case "*":
		// RFC 4954 客户端取消 AUTH 的哨兵行（多在 AUTH 挑战中途回错后发出）。
		se.reply(501, "5.7.0 Authentication aborted")
	default:
		se.reply(502, "5.5.2 Command not recognized")
	}
	return true
}

// ---------- 基础命令 ----------

// cmdHelo HELO/EHLO：记录问候并复位事务语境（RFC 5321 §4.1.4）。EHLO 回多行
// 扩展广播（末行不带连字符）；HELO 只回单行 250。
func (se *session) cmdHelo(arg string, extended bool) bool {
	domain := strings.TrimSpace(arg)
	if extended && domain == "" {
		se.reply(501, "5.5.4 EHLO requires domain address")
		return true
	}
	se.greeted = true
	se.resetTx()
	if !extended {
		se.reply(250, "catmail")
		return true
	}
	// EHLO 必须是单个多行应答（首行问候语 + 各扩展行，末行不带连字符）——拆成
	// 两条应答会让按「一条应答」读取的客户端整体错位一行。
	lines := []string{
		"catmail greets " + domain,
		"PIPELINING",
		"SIZE " + strconv.FormatInt(se.srv.maxBytes, 10),
		"8BITMIME",
		"AUTH PLAIN LOGIN",
	}
	// STARTTLS 广播（RFC 3207）：证书已配置即广播（明文监听供客户端协商升级）；
	// 隐式 TLS 监听按任务契约同样广播该扩展（会话已处 TLS 之内，客户端真发起
	// STARTTLS 会被 cmdStartTLS 以 503 拒绝——RFC 8314 §2 建议隐式端口不广播，
	// 这里以「广播但不重复协商」折中任务口径与协议安全）；未配证书维持纯明文
	// 现状不广播。
	if se.srv.tlsConfig != nil {
		lines = append(lines, "STARTTLS")
	}
	se.replyMulti(250, lines...)
	return true
}

// cmdMail 解析 MAIL FROM:<path> [KEY=VAL ...]：路径校验 + SIZE 参数超限即 552；
// BODY=8BITMIME、RET、ENVID 等其余参数接受但忽略（不影响轮换发送）。未 HELO /
// 嵌套 MAIL 均 503。
func (se *session) cmdMail(arg string) bool {
	if !se.greeted {
		se.reply(503, "5.5.1 Send HELO/EHLO first")
		return true
	}
	if se.haveMail {
		se.reply(503, "5.5.1 Nested MAIL command")
		return true
	}
	rest, ok := cutPrefixCI(arg, "FROM:")
	if !ok {
		se.reply(501, "5.5.4 Syntax error in MAIL command")
		return true
	}
	path, params := splitPathParams(rest)
	addr, err := parsePath(path)
	if err != nil {
		se.reply(501, "5.1.7 Bad sender address syntax")
		return true
	}
	for _, p := range params {
		key, val, _ := strings.Cut(p, "=")
		if !strings.EqualFold(key, "SIZE") {
			continue
		}
		n, e := strconv.ParseInt(val, 10, 64)
		if e != nil || n < 0 {
			se.reply(501, "5.5.4 Bad SIZE parameter")
			return true
		}
		if n > se.srv.maxBytes {
			se.reply(552, "5.3.4 Message size exceeds fixed maximum message size")
			return true
		}
	}
	se.mailFrom, se.haveMail = addr, true
	se.rcpts = nil
	se.reply(250, "2.1.0 OK")
	return true
}

// cmdRcpt 解析 RCPT TO:<addr>：防开放中继的核心闸门——非 loopback 对端（或测试
// 注入）必须先 AUTH；收件人上限 100；ORCPT 等参数忽略。
func (se *session) cmdRcpt(arg string) bool {
	if !se.haveMail {
		se.reply(503, "5.5.1 Need MAIL command")
		return true
	}
	if se.srv.requireAuth(se.remoteIP) && !se.authed {
		se.reply(530, "5.7.0 Authentication required")
		return true
	}
	rest, ok := cutPrefixCI(arg, "TO:")
	if !ok {
		se.reply(501, "5.5.4 Syntax error in RCPT command")
		return true
	}
	path, _ := splitPathParams(rest)
	addr, err := parsePath(path)
	if err != nil || addr == "" { // RCPT 不接受空路径（<>）
		se.reply(501, "5.1.3 Bad recipient address syntax")
		return true
	}
	if len(se.rcpts) >= maxRcpt {
		se.reply(452, "4.5.3 Too many recipients")
		return true
	}
	se.rcpts = append(se.rcpts, addr)
	se.reply(250, "2.1.5 OK")
	return true
}

// cmdData 接收 DATA 载荷并转交 rotator：354 → 逐行点去 stuffed 读到独占一行的
// "." → 超限 552 / 解析失败 501 / rotator 错误映射（250、501、452、451）。
// 成功或失败后事务复位，同一连接可继续发下一封（本地 MUA/应用常见用法）。
func (se *session) cmdData() bool {
	if !se.haveMail {
		se.reply(503, "5.5.1 Need MAIL command")
		return true
	}
	if len(se.rcpts) == 0 {
		se.reply(503, "5.5.1 Need RCPT command")
		return true
	}
	se.reply(354, "End data with <CR><LF>.<CR><LF>")
	raw, oversized, err := se.readData()
	if err != nil {
		return false // 读中断（超时/EOF/行超长）：无法与客户端重新同步，断开
	}
	if oversized {
		se.reply(552, "5.3.4 Message size exceeds fixed maximum message size")
		se.resetTx()
		return true
	}
	code, text := se.deliver(raw)
	se.reply(code, text)
	se.resetTx()
	return true
}

// readData 读 DATA 载荷直至独占一行的 "."：逐行点去 stuffed（RFC 5321 §4.5.2，
// 行首 ".." 还原为 "."），累计超上限后不再缓冲内容但继续读完剩余行（保持协议
// 同步，收尾统一回 552）。
func (se *session) readData() (raw []byte, oversized bool, err error) {
	var n int64
	for {
		line, e := se.readLine()
		if e != nil {
			return nil, false, e
		}
		if line == "." {
			return raw, oversized, nil
		}
		line = strings.TrimPrefix(line, ".")
		n += int64(len(line)) + 2
		if n > se.srv.maxBytes {
			oversized = true
			continue
		}
		raw = append(raw, line...)
		raw = append(raw, '\r', '\n')
	}
}

func (se *session) resetTx() {
	se.haveMail = false
	se.mailFrom = ""
	se.rcpts = nil
}

// ---------- AUTH（RFC 4954：PLAIN / LOGIN 服务端实现） ----------

// cmdAuth 认证入口：仅支持 PLAIN/LOGIN（EHLO 广播口径一致）。已认证或邮件事务
// 进行中 503；未知机制 504。TLS_POLICY=require 时明文连接必须先 STARTTLS 升级
// 成功才允许 AUTH（防明文密码泄露，RFC 4954 §4 专用响应码 530），隐式 TLS 监听
// 与已升级会话天然豁免。
func (se *session) cmdAuth(arg string) bool {
	if se.authed {
		se.reply(503, "5.5.1 Already authenticated")
		return true
	}
	if se.haveMail {
		se.reply(503, "5.5.1 AUTH not permitted during mail transaction")
		return true
	}
	if se.srv.tlsPolicy == TLSPolicyRequire && !se.isTLS {
		se.reply(530, "5.7.0 Must issue a STARTTLS command first")
		return true
	}
	mech, rest, _ := strings.Cut(arg, " ")
	mech = strings.ToUpper(strings.TrimSpace(mech))
	rest = strings.TrimSpace(rest)
	switch mech {
	case "PLAIN":
		return se.authPlain(rest)
	case "LOGIN":
		return se.authLogin(rest)
	default:
		se.reply(504, "5.5.4 Unrecognized authentication type")
		return true
	}
}

// authPlain AUTH PLAIN（RFC 4616）：初始响应可随命令同行携带，也可挑战后补发；
// 载荷为 base64(authzid\0authcid\0passwd)。
func (se *session) authPlain(initial string) bool {
	line := initial
	if line == "" {
		se.reply(334, "")
		l, err := se.readLine()
		if err != nil {
			return false
		}
		line = strings.TrimSpace(l)
	}
	if line == "*" {
		se.reply(501, "5.7.0 Authentication aborted")
		return true
	}
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		se.reply(501, "5.5.2 Invalid base64 data")
		return true
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 {
		se.reply(501, "5.5.2 Malformed PLAIN response")
		return true
	}
	return se.finishAuth(parts[1], parts[2]) // [0]=authzid 恒忽略
}

// authLogin AUTH LOGIN（非 RFC 官方机制，事实标准挑战串为 base64("Username:") /
// base64("Password:")，与常见客户端互通）。
func (se *session) authLogin(initial string) bool {
	line := initial
	if line == "" {
		se.reply(334, base64.StdEncoding.EncodeToString([]byte("Username:")))
		l, err := se.readLine()
		if err != nil {
			return false
		}
		line = strings.TrimSpace(l)
	}
	if line == "*" {
		se.reply(501, "5.7.0 Authentication aborted")
		return true
	}
	user, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		se.reply(501, "5.5.2 Invalid base64 data")
		return true
	}
	se.reply(334, base64.StdEncoding.EncodeToString([]byte("Password:")))
	l, err := se.readLine()
	if err != nil {
		return false
	}
	line = strings.TrimSpace(l)
	if line == "*" {
		se.reply(501, "5.7.0 Authentication aborted")
		return true
	}
	pass, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		se.reply(501, "5.5.2 Invalid base64 data")
		return true
	}
	return se.finishAuth(string(user), string(pass))
}

// finishAuth 凭据校验：password 必须是某个 enabled 的 ApiKey（username 任意，
// 仅留日志——凭据模型是「API Key 即密码」，与 /v1/send 的 Bearer 同源）。命中
// 记 ApiKeyID，后续邮件的 SendLog 归属该 Key 并受其限速/日额度约束。
func (se *session) finishAuth(username, password string) bool {
	var ak model.ApiKey
	err := se.srv.db.Where("key = ?", password).First(&ak).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && !ak.Enabled) {
		se.reply(535, "5.7.8 Authentication credentials invalid")
		return true
	}
	if err != nil {
		log.Printf("[smtp] auth lookup failed (remote=%s): %v", se.remote, err)
		se.reply(451, "4.3.0 Temporary authentication failure")
		return true
	}
	se.authed = true
	se.apiKeyID = ak.ID
	// last_used_at 尽力刷新（与 api 层口径一致：失败仅记日志不阻断会话）。
	if e := se.srv.db.Model(&model.ApiKey{}).Where("id = ?", ak.ID).Update("last_used_at", time.Now()).Error; e != nil {
		log.Printf("[smtp] update key last_used_at failed (key_id=%d): %v", ak.ID, e)
	}
	log.Printf("[smtp] auth ok (remote=%s, user=%q, key_id=%d)", se.remote, username, ak.ID)
	se.reply(235, "2.7.0 Authentication successful")
	return true
}

// ---------- 发送（rotator 接线与错误映射） ----------

// deliver 解析消息并调 rotator.Send，把统一错误映射为 SMTP 应答（E1 设计第 3 条）：
//
//	accepted (200)     → 250 2.0.0 OK queued as <message_id>
//	invalid_request 400 → 501（永久失败：内容/参数问题，客户端需改内容重试）
//	quota/rate      429 → 452 4.2.1（临时失败：可稍后重试）
//	502/其他            → 451 4.3.0（临时失败：候选渠道穷尽/内部错误，可重试）
//
// 失败路径是否落 SendLog 与 rotator 现状一致（429 不落库但有失败通知/回执，
// 502 落最后一条失败日志），SMTP 层不额外造日志。
func (se *session) deliver(raw []byte) (code int, text string) {
	m, err := readMime(raw)
	if err != nil {
		return 501, "5.6.0 " + err.Error()
	}
	msg, err := parseMessage(m, int64(len(raw)), se.mailFrom, se.rcpts)
	if err != nil {
		return 501, "5.6.0 " + err.Error()
	}
	// 邮件模板渲染 hook（邮件模板功能包）：报文头携带 X-CatMail-Template 指令
	// 时渲染模板并替换 subject/html/text（语义与 API 路径同口径，见 template.go）；
	// 指令非法/unknown_template/body_conflict 均为内容类永久失败 → 501。
	templateID, terr := se.applyTemplate(m, msg)
	if terr != nil {
		return 501, "5.6.0 " + terr.Error()
	}
	// Key 级策略（B5）在 SMTP 路径的强制点：认证会话按命中 Key 的 rate_per_sec /
	// daily_quota 拦截（loopback 免认证会话无 Key，天然跳过）。被拒不发送、
	// 不落发送日志（与 API 层「拒绝在门外」口径一致）。
	if code, text, ok := se.enforceKeyPolicy(); !ok {
		return code, text
	}
	out, rerr := rotator.Send(se.ctx, se.srv.db, &rotator.Input{
		Message:    msg,
		APIKeyID:   se.apiKeyID,
		IsTest:     false,
		TemplateID: templateID,
	})
	if rerr == nil {
		return 250, "2.0.0 OK queued as " + out.MessageID
	}
	switch rerr.HTTPStatus {
	case 400:
		return 501, "5.6.0 " + rerr.Message
	case 429:
		return 452, "4.2.1 " + rerr.Message
	default:
		return 451, "4.3.0 " + rerr.Message
	}
}

// ---------- 行读写 ----------

// readLine 读一行（CRLF 结尾，容忍裸 LF 的劣质客户端）：基于 bufio.ReadSlice
// 零拷贝读常规行；超过 maxLineLen 回 500 并断开。每次读前设置 deadline（单命令
// 60s，且不超过会话总时长上限）。
func (se *session) readLine() (string, error) {
	se.setDeadline()
	line, err := se.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		se.reply(500, "5.5.2 Line too long")
		return "", errLineTooLong
	}
	if err != nil {
		return "", err // EOF / 超时 / 连接关闭
	}
	line = trimEOL(line)
	return string(line), nil
}

// reply 单行应答。
func (se *session) reply(code int, text string) {
	se.setDeadline()
	fmt.Fprintf(se.w, "%d %s\r\n", code, sanitizeReply(text))
	_ = se.w.Flush()
}

// replyMulti 多行应答（EHLO 扩展广播）：前 n-1 行 "250-"，末行 "250 "。
func (se *session) replyMulti(code int, lines ...string) {
	se.setDeadline()
	for i, l := range lines {
		sep := "-"
		if i == len(lines)-1 {
			sep = " "
		}
		fmt.Fprintf(se.w, "%d%s%s\r\n", code, sep, sanitizeReply(l))
	}
	_ = se.w.Flush()
}

// setDeadline 单命令读写 deadline：now+60s，不超过会话总时长。
func (se *session) setDeadline() {
	dl := time.Now().Add(cmdTimeout)
	if dl.After(se.endAt) {
		dl = se.endAt
	}
	_ = se.conn.SetDeadline(dl)
}

// ---------- 小工具 ----------

// sanitizeReply 应答文本防注入：剥掉 CR/LF（错误文案可能携带来自报文内容的
// 片段，如地址解析错误），保证 SMTP 应答恒为单行。
func sanitizeReply(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, text)
}

func splitCommand(line string) (verb, arg string) {
	if i := strings.IndexByte(line, ' '); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

// cutPrefixCI 大小写不敏感的前缀剥离（MAIL/RCPT 动词参数按规范大写，但宽容
// 客户端小写）。
func cutPrefixCI(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// splitPathParams 拆出路径与空格分隔的 ESMTP 参数（"<a@b> SIZE=123" →
// "<a@b>", ["SIZE=123"]）。
func splitPathParams(s string) (path string, params []string) {
	path, rest, _ := strings.Cut(strings.TrimSpace(s), " ")
	params = strings.Fields(rest)
	return path, params
}

// parsePath 解析反向/转发路径：接受 <addr> 与裸 addr（常见客户端省略尖括号，
// 宽容处理）；<> 为空路径（仅 MAIL 合法，投递退信场景）。返回规范化地址。
func parsePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "<>" {
		return "", nil
	}
	if strings.HasPrefix(p, "<") && strings.HasSuffix(p, ">") {
		p = p[1 : len(p)-1]
	} else if strings.ContainsAny(p, "<>") {
		return "", errors.New("bad path syntax")
	}
	if p == "" {
		return "", errors.New("empty path")
	}
	a, err := mail.ParseAddress(p)
	if err != nil {
		return "", err
	}
	return a.Address, nil
}

// trimEOL 去掉行尾 CRLF（容忍裸 LF）。
func trimEOL(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	return line
}

// cancellingConn 连接包装：Close 时级联取消会话 ctx（rotator.Send 的在途平台
// 请求随之取消）。
type cancellingConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *cancellingConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}
