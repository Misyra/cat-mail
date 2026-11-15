// message.go DATA 载荷（已点去 stuffed）→ provider.Message：net/mail +
// mime/multipart（Go 标准库，零新依赖）。字段口径：
//   - Subject：RFC 2047 encoded-word 解码（=?utf-8?B?...?= 等）；
//   - 发件人：From 头优先，缺失回退 envelope MAIL FROM（本地应用常不带展示头，
//     轮换的域名过滤与渠道默认发件人回退都吃这个地址）；
//   - 收件人：To = envelope RCPT ∪ To 头并集去重（envelope 为准——信封才是真实
//     投递对象，头部只补充可见收件人）；Cc 取 Cc 头；Bcc = 仅出现在 envelope、
//     不在任何可见头部的收件人（BCC 语义：MUA 发信时本就把 Bcc 从头部剥离、
//     只留信封，此处按同一约定还原，隐私收件人不会混进 To/Cc）；
//   - 正文：递归遍历 multipart，非附件的 text/plain 与 text/html 各取第一份
//     （multipart/alternative 下即纯文本/HTML 视图）；
//   - 附件：Content-Disposition: attachment 或带 filename（CD filename /
//     Content-Type name 参数，RFC 2231/2047 解码）的部分，按 CTE 解码后转
//     base64 → provider.Attachment；
//   - 字符集仅透传不做转码（保持零依赖）：非 UTF-8 文本/文件名按原始字节入库，
//     由平台端决定编码处理——定位级取舍。
package smtpserver

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"

	"catmail/internal/provider"
)

// maxMimeDepth multipart 递归层数上限（防恶意深嵌套报文拖垮解析）。
const maxMimeDepth = 10

// buildMessage 把原始报文解析为规范 provider.Message。envelopeFrom 为 MAIL FROM
// 的反向路径，rcpts 为 envelope RCPT 列表。解析失败返回错误（SMTP 层映射 501）。
func buildMessage(raw []byte, envelopeFrom string, rcpts []string) (*provider.Message, error) {
	m, err := readMime(raw)
	if err != nil {
		return nil, err
	}
	return parseMessage(m, int64(len(raw)), envelopeFrom, rcpts)
}

// readMime 解析原始报文的头部与内容流（mail.ReadMessage）。单独导出给
// deliver：模板指令头（X-CatMail-Template*，见 template.go）与消息体共用
// 一次头部解析，避免双重读报文。
func readMime(raw []byte) (*mail.Message, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("解析邮件头失败: %w", err)
	}
	return m, nil
}

// parseMessage 由已解析的报文构造规范 provider.Message（字段口径见文件头注释）。
// maxBytes 为原始 DATA 载荷大小（解码放大上限锚点，见 parser.maxBytes）。
func parseMessage(m *mail.Message, maxBytes int64, envelopeFrom string, rcpts []string) (*provider.Message, error) {
	msg := &provider.Message{Subject: decodeRFC2047(m.Header.Get("Subject"))}

	// 发件人：From 头优先，缺失/非法回退 envelope；envelope 也无效则报错
	// （轮换需要可判定的发件域名，硬造地址会污染渠道过滤）。
	if froms, e := m.Header.AddressList("From"); e == nil && len(froms) > 0 {
		msg.FromName, msg.FromAddr = froms[0].Name, froms[0].Address
	} else if envelopeFrom != "" {
		a, e := mail.ParseAddress(envelopeFrom)
		if e != nil {
			return nil, fmt.Errorf("发件人无效（From 头与 envelope 均不可用）: %q", envelopeFrom)
		}
		msg.FromName, msg.FromAddr = a.Name, a.Address
	}

	if rts, e := m.Header.AddressList("Reply-To"); e == nil {
		for _, a := range rts {
			msg.ReplyTo = append(msg.ReplyTo, a.Address)
		}
	}

	// 收件人（口径见文件头注释）：先收集可见头部收件人，再按 envelope 并集拆分。
	var toHeader, ccHeader []string
	if as, e := m.Header.AddressList("To"); e == nil {
		for _, a := range as {
			toHeader = append(toHeader, a.Address)
		}
	}
	if as, e := m.Header.AddressList("Cc"); e == nil {
		for _, a := range as {
			ccHeader = append(ccHeader, a.Address)
		}
	}
	visible := addrSet(append(append([]string(nil), toHeader...), ccHeader...))
	toFromEnvelope := make([]string, 0, len(rcpts))
	for _, r := range rcpts {
		if visible[strings.ToLower(r)] {
			toFromEnvelope = append(toFromEnvelope, r)
		} else {
			msg.Bcc = append(msg.Bcc, r) // 仅信封可见 → BCC（不进 To/Cc 展示名单）
		}
	}
	msg.To = dedupAddrs(append(toFromEnvelope, toHeader...))
	msg.Cc = dedupAddrs(ccHeader)

	// 正文与附件：顶层按单部分/多部分统一走 walk（顶层 CTE 在 walk 的 decode 处理）。
	p := &parser{maxBytes: maxBytes}
	if err := p.walk(textproto.MIMEHeader(m.Header), m.Body,
		m.Header.Get("Content-Type"), m.Header.Get("Content-Transfer-Encoding"), 0); err != nil {
		return nil, err
	}
	msg.Text, msg.HTML = p.text, p.html
	msg.Attachments = p.atts
	return msg, nil
}

// parser 聚合遍历结果：文本/HTML 正文各取第一份，附件逐个累积。
type parser struct {
	text     string
	html     string
	atts     []provider.Attachment
	maxBytes int64 // 单部分解码输出上限（> 报文原始大小即异常，防解码放大）
	attachN  int   // 无文件名附件的兜底序号
}

// walk 递归处理一个 MIME 部分（h 为该部分头，contentType/cte 为已取出的对应值；
// 顶层与 multipart 子部分共用）。
func (p *parser) walk(h textproto.MIMEHeader, body io.Reader, contentType, cte string, depth int) error {
	if depth > maxMimeDepth {
		return fmt.Errorf("MIME 嵌套过深（>%d 层）", maxMimeDepth)
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" {
		// 缺省/非法 Content-Type：按 text/plain 兜底（RFC 5322 默认值）。
		mediaType, params = "text/plain", nil
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("multipart 缺少 boundary 参数")
		}
		mr := multipart.NewReader(body, boundary)
		for {
			// NextRawPart：拿原始部分，CTE 统一在叶子节点自行解码（quoted-printable
			// 由 NextPart 自动解码的规则不适用于顶层与 base64，统一口径更不易错）。
			part, e := mr.NextRawPart()
			if e == io.EOF {
				return nil
			}
			if e != nil {
				return fmt.Errorf("解析 multipart 失败: %w", e)
			}
			if err := p.walk(part.Header, part,
				part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), depth+1); err != nil {
				return err
			}
		}
	}

	// 叶子部分：先判定附件属性，再解码内容。
	disposition, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"] // 老式写法 Content-Type: xxx; name="f.ext"
	}
	isAttachment := disposition == "attachment" || filename != ""
	data, err := p.decode(body, cte)
	if err != nil {
		return err
	}
	switch {
	case isAttachment:
		if filename == "" { // disposition=attachment 但没写文件名：兜底命名
			p.attachN++
			filename = fmt.Sprintf("attachment-%d", p.attachN)
		}
		p.atts = append(p.atts, provider.Attachment{
			Filename:      decodeRFC2047(filename),
			ContentBase64: base64.StdEncoding.EncodeToString(data),
		})
	case mediaType == "text/plain" && p.text == "":
		p.text = string(data)
	case mediaType == "text/html" && p.html == "":
		p.html = string(data)
	default:
		// 其余叶子类型（image/* 内联、application/octet-stream 无文件名等）忽略：
		// provider.Message 没有内联资源位（content-id 引用），定位级不实现。
	}
	return nil
}

// decode 按 Content-Transfer-Encoding 解码部分内容：base64（容忍换行/空白）与
// quoted-printable 实际解码，7bit/8bit/binary/空/未知一律原样透传。输出以
// maxBytes 为上限，防解码放大吃内存。
func (p *parser) decode(r io.Reader, cte string) ([]byte, error) {
	cte = strings.ToLower(strings.TrimSpace(cte))
	switch cte {
	case "base64":
		raw, err := p.readAll(r)
		if err != nil {
			return nil, err
		}
		// 与 provider 附件容错口径一致：剔除全部空白（编码中间的换行/空格合法）
		// 后严格解码；坏数据报错给客户端（501），不静默截断。
		clean := strings.Join(strings.Fields(string(raw)), "")
		out, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return nil, fmt.Errorf("base64 解码失败: %w", err)
		}
		return out, nil
	case "quoted-printable":
		out, err := p.readAll(quotedprintable.NewReader(r))
		if err != nil {
			return nil, fmt.Errorf("quoted-printable 解码失败: %w", err)
		}
		return out, nil
	default: // "" | 7bit | 8bit | binary | 未知
		return p.readAll(r)
	}
}

// readAll 限额读取：超过 maxBytes 报错（正常报文任何部分解码后都不应大于原始
// DATA 载荷；超限只可能是构造出来的放大攻击报文）。
func (p *parser) readAll(r io.Reader) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, p.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取内容失败: %w", err)
	}
	if int64(len(out)) > p.maxBytes {
		return nil, fmt.Errorf("MIME 部分解码后超过报文上限")
	}
	return out, nil
}

// decodeRFC2047 解码 encoded-word（主题/文件名常用）；非 encoded-word 文本原样
// 返回，解码失败（不支持的 charset 等）回退原文，绝不因装饰性头失败而拒信。
func decodeRFC2047(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	out, err := (&mime.WordDecoder{CharsetReader: passthroughCharset}).DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

// passthroughCharset 非 UTF-8/ASCII charset 的兜底：不做编码转换（零依赖取舍），
// 字节原样透传——GBK/ISO-8859-1 等主题与文件名将按原始字节存储与展示。
func passthroughCharset(charset string, input io.Reader) (io.Reader, error) {
	return input, nil
}

// addrSet 小写地址集合（收件人去重判定）。
func addrSet(addrs []string) map[string]bool {
	set := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		set[strings.ToLower(a)] = true
	}
	return set
}

// dedupAddrs 去重保序（同址大小写不同视为同一地址，保留首现写法）。
func dedupAddrs(addrs []string) []string {
	seen := make(map[string]bool, len(addrs))
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		k := strings.ToLower(a)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, a)
	}
	return out
}
