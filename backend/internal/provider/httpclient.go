// httpclient.go 五个 HTTP 平台适配器的共享辅助：代理客户端构建、请求收发与错误分类（SPEC §3 通用约束）。
package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

const (
	httpTimeout  = 30 * time.Second // 统一 30s 超时
	maxRespBody  = 1 << 20          // 响应体读取上限 1MB（防超大响应撑爆内存；发送接口的成功/错误响应均为小 JSON）
	maxErrorBody = 2 * 1024         // 错误信息携带的响应体上限 2KB
	maxErrMsg    = 300              // 平台 message 字段的截断上限
)

// BuildClient 构建出站 HTTP 客户端：proxyURL 支持 http(s):// 与 socks5://（含 socks5h://），空=直连；统一 30s 超时。
func BuildClient(proxyURL string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // 不读环境变量代理，仅按渠道配置出站
	if proxyURL == "" {
		return &http.Client{Transport: transport, Timeout: httpTimeout}, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		// 安全加固三件套（A）：代理地址可内嵌 user:pass@ 凭据，解析失败也不回显
		// 原文（凭据红线——错误信息会进 SendLog.Error / 测试发送响应 / 服务端日志）
		return nil, fmt.Errorf("invalid proxy url（含凭据，不回显原文）: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		transport.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h": // socks5h 与 socks5 同为经代理端解析主机名语义，处理一致
		var auth *proxy.Auth
		if u.User != nil {
			pass, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pass}
		}
		host := u.Host
		if _, _, perr := net.SplitHostPort(host); perr != nil {
			host = net.JoinHostPort(host, "1080") // 未写端口时用 socks5 默认端口
		}
		dialer, derr := proxy.SOCKS5("tcp", host, auth, proxy.Direct)
		if derr != nil {
			return nil, fmt.Errorf("create socks5 dialer: %w", derr)
		}
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			transport.DialContext = cd.DialContext
		} else {
			// 兜底：拨号器无上下文能力时包一层 DialContext（拨号期间不响应 ctx 取消，
			// 语义与已废弃的 Dial 字段一致），整体取消仍由 client.Timeout 兜底。
			transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	return &http.Client{Transport: transport, Timeout: httpTimeout}, nil
}

// PostJSON 发送 JSON POST 请求；非 2xx 响应按状态码分类为 ProviderError。
func PostJSON(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, payload any) ([]byte, *ProviderError) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, &ProviderError{Code: "encode_failed", Message: "encode request json: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, &ProviderError{Code: "build_request_failed", Message: "build request: " + err.Error(), Retryable: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	setHeaders(req.Header, headers)
	return doRequest(client, req)
}

// PostForm 发送 application/x-www-form-urlencoded POST 请求；非 2xx 响应按状态码分类为 ProviderError。
func PostForm(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, form url.Values) ([]byte, *ProviderError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &ProviderError{Code: "build_request_failed", Message: "build request: " + err.Error(), Retryable: true}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setHeaders(req.Header, headers)
	return doRequest(client, req)
}

// PostMultipart 发送 multipart/form-data POST 请求；各 part 由调用方经 buildParts 写入。
func PostMultipart(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, buildParts func(*multipart.Writer) error) ([]byte, *ProviderError) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := buildParts(w); err != nil {
		return nil, &ProviderError{Code: "encode_failed", Message: "build multipart: " + err.Error()}
	}
	if err := w.Close(); err != nil {
		return nil, &ProviderError{Code: "encode_failed", Message: "close multipart writer: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, &ProviderError{Code: "build_request_failed", Message: "build request: " + err.Error(), Retryable: true}
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	setHeaders(req.Header, headers)
	return doRequest(client, req)
}

// setHeaders 设置请求头，空值跳过。
func setHeaders(h http.Header, headers map[string]string) {
	for k, v := range headers {
		if v != "" {
			h.Set(k, v)
		}
	}
}

// doRequest 执行请求并读取响应体；网络错误归类为 Retryable。
func doRequest(client *http.Client, req *http.Request) ([]byte, *ProviderError) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, &ProviderError{Code: "network_error", Message: "network: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if err != nil {
		return nil, &ProviderError{Code: "network_error", HTTPStatus: resp.StatusCode, Message: "read response: " + err.Error(), Retryable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyStatus(resp.StatusCode, string(body))
	}
	return body, nil
}

// classifyStatus 按平台返回的 HTTP 状态码分类（SPEC §2/§3）：
// 401/403→禁用渠道；429→可重试+冷却；402/5xx→可重试；其余 4xx→参数类（不重试/不禁用/不冷却）。
// 例外：Brevo 额度用尽错误码（not_enough_credits，HTTP 400）按限流同款分类，见下方判定。
func classifyStatus(status int, body string) *ProviderError {
	pe := &ProviderError{HTTPStatus: status, Code: "provider_error"}
	if msg := extractPlatformMessage(body); msg != "" {
		pe.Message = truncate(msg, maxErrMsg) + " | body: " + truncate(body, maxErrorBody)
	} else {
		pe.Message = "body: " + truncate(body, maxErrorBody)
	}
	switch {
	case status == 401 || status == 403:
		pe.DisableChannel = true
	case status == 429:
		pe.Retryable, pe.Cooldown = true, true
	case status == 402 || status >= 500:
		pe.Retryable = true
	}
	// Brevo 额度用尽：官方错误码表定义 code=not_enough_credits（HTTP 400 + {"code","message"}），
	// 属"限速/额度"类而非请求参数错——可重试（换渠道）+ 冷却，避免无冷却地持续打到该渠道。
	if strings.EqualFold(extractPlatformCode(body), "not_enough_credits") {
		pe.Retryable, pe.Cooldown = true, true
	}
	return pe
}

// extractPlatformMessage 尽力从平台 JSON 错误响应中提取 message 类字段（兼容顶层与嵌套结构）。
func extractPlatformMessage(body string) string {
	var root any
	if json.Unmarshal([]byte(body), &root) != nil {
		return ""
	}
	return findMessage(root, 0)
}

// extractPlatformCode 提取平台错误响应顶层的 code 字段（如 Brevo 的 {"code":"not_enough_credits"}）。
func extractPlatformCode(body string) string {
	var top struct {
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(body), &top) != nil {
		return ""
	}
	return top.Code
}

// messageKeys 各平台错误消息常见字段名（"Message" 大写：Aliyun/Elastic Email；"errors" 复数：Mailtrap）。
var messageKeys = []string{"message", "Message", "ErrorMessage", "error", "errors"}

// findMessage 深度优先查找 message 类字段，最多下探 5 层
// （Mailjet v3.1 错误消息位于 Messages[].Errors[].ErrorMessage，深 4 层）。
func findMessage(v any, depth int) string {
	if depth > 5 {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		for _, key := range messageKeys {
			if s, ok := t[key].(string); ok && s != "" {
				return s
			}
		}
		for _, key := range messageKeys {
			if child, ok := t[key]; ok {
				if s := findMessage(child, depth+1); s != "" {
					return s
				}
			}
		}
		for _, child := range t {
			if s := findMessage(child, depth+1); s != "" {
				return s
			}
		}
	case []any:
		// 纯字符串数组（如 Mailtrap {"success":false,"errors":["a","b"]}）直接拼接；否则逐元素下探。
		parts := make([]string, 0, len(t))
		allStrings := true
		for _, el := range t {
			if s, ok := el.(string); ok {
				if s != "" {
					parts = append(parts, s)
				}
			} else {
				allStrings = false
				break
			}
		}
		if allStrings && len(parts) > 0 {
			return strings.Join(parts, "; ")
		}
		for _, child := range t {
			if s := findMessage(child, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}

// truncate 截断字符串至 max 字节，超长追加省略标记。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// basicAuth 组装 Basic 认证头值。
func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// FormatFrom 组装 "Name <addr>" 形式的发件人串；Name 为空时仅返回地址。
// 经 net/mail.Address.String() 处理引号转义与 RFC 2047 编码，避免含逗号/引号/尖括号/非 ASCII 的显示名拼出非法串。
func FormatFrom(m *Message) string {
	return (&mail.Address{Name: m.FromName, Address: m.FromAddr}).String()
}

// extractBareAddress 从 RFC 5322 地址串提取裸地址（"Bob <b@x.com>" → "b@x.com"，纯地址原样返回）。
// dto 层保留 "Name <addr>" 原始串，部分平台联系人字段仅收裸地址（Brevo/Mailjet 的 {email} 对象、SMTP 信封
// RCPT TO 会再包一层尖括号），须先提取；解析失败回退原文，交由平台侧报参数类错误。
func extractBareAddress(s string) string {
	s = strings.TrimSpace(s)
	if a, err := mail.ParseAddress(s); err == nil {
		return a.Address
	}
	return s
}

// guessContentType 按扩展名猜测附件 Content-Type，兜底 application/octet-stream。
func guessContentType(name string) string {
	if ext := filepath.Ext(name); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	return "application/octet-stream"
}
