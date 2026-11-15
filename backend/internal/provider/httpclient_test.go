// httpclient_test.go 共享辅助单测：裸地址提取、状态码分类、响应体上限、代理客户端构建。
package provider

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestExtractBareAddress 全格式提取裸地址；纯地址原样；解析失败回退原文。
func TestExtractBareAddress(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Bob <b@example.com>", "b@example.com"},           // "Name <addr>" → 裸地址
		{"bare@example.com", "bare@example.com"},           // 纯地址原样返回
		{"<angle@example.com>", "angle@example.com"},       // 仅尖括号 → 裸地址
		{`"Smith, John" <s@example.com>`, "s@example.com"}, // 含逗号显示名（带引号）→ 裸地址
		{"  padded@example.com  ", "padded@example.com"},   // 首尾空白先剔除
		{"not-an-address", "not-an-address"},               // 解析失败回退原文
		{"", ""},                                           // 空串回退
	}
	for _, tc := range cases {
		if got := extractBareAddress(tc.in); got != tc.want {
			t.Errorf("extractBareAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestClassifyStatus 状态码分类（SPEC §2/§3）：401/403→禁用；429→重试+冷却；402/5xx→重试；
// 其余 4xx→参数类；Brevo 额度码 not_enough_credits（HTTP 400）→重试+冷却；错误信息携带平台原始 message。
func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		name                         string
		status                       int
		body                         string
		message                      string // 期望包含的平台原始 message（空=不断言）
		retryable, disable, cooldown bool
	}{
		{"401 disable", 401, `{"message":"Invalid API key"}`, "Invalid API key", false, true, false},
		{"403 disable", 403, `{"message":"Forbidden"}`, "Forbidden", false, true, false},
		{"429 cooldown", 429, `{"message":"Rate limit exceeded"}`, "Rate limit exceeded", true, false, true},
		{"402 retryable", 402, `{"message":"Payment Required"}`, "Payment Required", true, false, false},
		{"500 retryable", 500, `{"message":"Internal error"}`, "Internal error", true, false, false},
		{"400 param", 400, `{"message":"bad recipient"}`, "bad recipient", false, false, false},
		{"404 param", 404, `Domain not found`, "Domain not found", false, false, false},
		// 修复锁定：Brevo 额度用尽走 HTTP 400 + code=not_enough_credits（官方错误码表），
		// 不能按参数类处理（否则无冷却，额度用尽后每条消息仍会持续打到该渠道）
		{"brevo not_enough_credits", 400, `{"code":"not_enough_credits","message":"Not enough credits"}`, "Not enough credits", true, false, true},
		{"brevo quota uppercase", 400, `{"code":"NOT_ENOUGH_CREDITS","message":"x"}`, "x", true, false, true},
		// Brevo 其它错误码不误伤
		{"brevo invalid_parameter", 400, `{"code":"invalid_parameter","message":"bad to"}`, "bad to", false, false, false},
		// 修复锁定：Mailjet v3.1 错误消息位于 Messages[].Errors[].ErrorMessage（JSON 深度 4），应能提取
		{"mailjet nested", 400,
			`{"Messages":[{"Status":"error","Errors":[{"ErrorCode":"mj-0006","ErrorMessage":"Illegal recycled email address","StatusCode":400}]}]}`,
			"Illegal recycled email address", false, false, false},
		// Aliyun 错误体 Message 字段大写 M，应能提取
		{"aliyun Message", 400,
			`{"RequestId":"r","Message":"The specified subject is invalid.","Recommend":"x","HostId":"dm.aliyuncs.com","Code":"InvalidSubject.Malformed"}`,
			"The specified subject is invalid.", false, false, false},
		// Mailtrap 错误体 {"success":false,"errors":[...]} 字符串数组，应拼接提取
		{"mailtrap errors array", 400,
			`{"success":false,"errors":["Invalid recipient","Sending domain not verified"]}`,
			"Invalid recipient; Sending domain not verified", false, false, false},
		// 非 JSON 错误体：不崩、仍带原始 body
		{"non-json", 500, `<html>oops</html>`, "", true, false, false},
	}
	for _, tc := range cases {
		pe := classifyStatus(tc.status, tc.body)
		if pe.Retryable != tc.retryable || pe.DisableChannel != tc.disable || pe.Cooldown != tc.cooldown {
			t.Errorf("%s: got retry=%v disable=%v cooldown=%v, want %v/%v/%v (%s)",
				tc.name, pe.Retryable, pe.DisableChannel, pe.Cooldown,
				tc.retryable, tc.disable, tc.cooldown, pe.Message)
		}
		if pe.HTTPStatus != tc.status {
			t.Errorf("%s: HTTPStatus = %d, want %d", tc.name, pe.HTTPStatus, tc.status)
		}
		if tc.message != "" && !strings.Contains(pe.Message, tc.message) {
			t.Errorf("%s: Message = %q, want 含平台原始 message %q", tc.name, pe.Message, tc.message)
		}
	}
}

// TestClassifyStatusTruncation 超长平台 message 与错误体应被截断（上限 maxErrMsg/maxErrorBody）。
func TestClassifyStatusTruncation(t *testing.T) {
	long := strings.Repeat("x", 10*1024)
	pe := classifyStatus(400, long)
	if len(pe.Message) > maxErrMsg+len("...(truncated)")+len(" | body: ")+maxErrorBody+len("...(truncated)") {
		t.Errorf("Message 长度 %d 超出截断上限", len(pe.Message))
	}
}

// TestPostJSONBodyLimit 修复锁定：响应体读取设 1MB 上限（防超大响应撑爆内存），超限截断后交由调用方解析报错。
func TestPostJSONBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxRespBody+64*1024))
	}))
	defer srv.Close()

	body, perr := PostJSON(context.Background(), srv.Client(), srv.URL, nil, map[string]string{"k": "v"})
	if perr != nil {
		t.Fatalf("PostJSON 不应报错（截断属预期），got %v", perr)
	}
	if len(body) != maxRespBody {
		t.Fatalf("body len = %d, want 上限 %d", len(body), maxRespBody)
	}
}

// TestPostJSONNetworkError 网络错误 → Retryable（连接拒绝场景）。
func TestPostJSONNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // 立即关闭，端口大概率不可达
	client := &http.Client{Timeout: 2 * time.Second}
	_, perr := PostJSON(context.Background(), client, srv.URL, nil, map[string]string{})
	if perr == nil || perr.Code != "network_error" || !perr.Retryable {
		t.Fatalf("expected network_error Retryable, got %+v", perr)
	}
}

// TestBuildClientProxy 修复锁定：非法代理 scheme 报错（不降级直连）；http/socks5 构建成功；空=直连。
func TestBuildClientProxy(t *testing.T) {
	if c, err := BuildClient(""); err != nil || c == nil {
		t.Fatalf("空代理应直连, got (%v, %v)", c, err)
	}
	for _, ok := range []string{"http://127.0.0.1:8080", "https://127.0.0.1:8443", "socks5://127.0.0.1:1080", "socks5://u:p@127.0.0.1", "socks5h://127.0.0.1:1080"} {
		if _, err := BuildClient(ok); err != nil {
			t.Errorf("BuildClient(%q) 应成功, got %v", ok, err)
		}
	}
	for _, bad := range []string{"ftp://127.0.0.1:21", "::::not-a-url"} {
		if _, err := BuildClient(bad); err == nil {
			t.Errorf("BuildClient(%q) 应报错（不降级直连）", bad)
		}
	}
}

// TestPostJSONEncodeFailed payload 不可序列化 → encode_failed（不发出任何请求）。
func TestPostJSONEncodeFailed(t *testing.T) {
	_, perr := PostJSON(context.Background(), http.DefaultClient, "http://127.0.0.1:1/x", nil, map[string]any{"f": func() {}})
	if perr == nil || perr.Code != "encode_failed" {
		t.Fatalf("expected encode_failed, got %+v", perr)
	}
}

// TestPostMultipartBuildPartsError 表单装配失败 → encode_failed（multipart 构建/关闭错误归入装配）。
func TestPostMultipartBuildPartsError(t *testing.T) {
	sentinel := errors.New("boom")
	_, perr := PostMultipart(context.Background(), http.DefaultClient, "http://127.0.0.1:1/x", nil,
		func(*multipart.Writer) error { return sentinel })
	if perr == nil || perr.Code != "encode_failed" || !strings.Contains(perr.Message, "boom") {
		t.Fatalf("expected encode_failed carrying build error, got %+v", perr)
	}
}

// TestFindMessageGuards findMessage 深度上限（防恶意深层嵌套撑爆遍历）与 error 字段为对象时的递归下探。
func TestFindMessageGuards(t *testing.T) {
	// {"error": {...}}：error 字段为对象 → 继续下探取到内层 message
	if got := findMessage(map[string]any{"error": map[string]any{"message": "inner"}}, 0); got != "inner" {
		t.Errorf("error 对象嵌套 message 应可提取, got %q", got)
	}
	// 超过 5 层不再下探
	var deep any = "bottom"
	for i := 0; i < 8; i++ {
		deep = map[string]any{"error": deep}
	}
	if got := findMessage(deep, 0); got != "" {
		t.Errorf("超过深度上限应返回空, got %q", got)
	}
}

// TestGuessContentTypeFallback 无扩展名附件兜底 application/octet-stream（有扩展名走系统类型表）。
func TestGuessContentTypeFallback(t *testing.T) {
	if got := guessContentType("noext"); got != "application/octet-stream" {
		t.Errorf("guessContentType(noext) = %q, want octet-stream 兜底", got)
	}
	if got := guessContentType("a.txt"); got == "application/octet-stream" {
		t.Errorf("guessContentType(a.txt) 不应落入兜底, got %q", got)
	}
}
