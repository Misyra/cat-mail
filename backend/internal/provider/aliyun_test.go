// aliyun_test.go 阿里云 DirectMail 适配器测试：httptest 假服务注入覆盖 baseURL（SPEC §3/§7）。
// 签名正确性由 aliyun_sign_test.go 黄金值锁定，此处断言请求结构（公共参数/业务参数/端点）与错误分类。
package provider

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"catmail/internal/model"
)

// testAliyunMessage 无附件测试消息（testMessage 自带附件，而 SingleSendMail 官方不支持附件）。
func testAliyunMessage() *Message {
	m := testMessage()
	m.Attachments = nil
	return m
}

// testAliyunChannel 构造字段齐全的阿里云渠道。
func testAliyunChannel() *model.Channel {
	return &model.Channel{
		Provider:  "aliyun",
		APIKey:    "testid",
		APISecret: "testsecret",
		FromEmail: "noreply@example.net",
		FromName:  "Snow",
	}
}

// parseAliyunRequest 从请求中取出公共 query 参数与业务 form 参数。
func parseAliyunRequest(t *testing.T, r *http.Request, rawBody []byte) (url.Values, url.Values) {
	t.Helper()
	q := r.URL.Query()
	f, err := url.ParseQuery(string(rawBody))
	if err != nil {
		t.Errorf("form body not urlencoded: %v", err)
	}
	return q, f
}

// TestAliyunSendSuccess 成功分支：公共参数齐全、业务参数逐项、EnvId 作为消息 ID。
func TestAliyunSendSuccess(t *testing.T) {
	var gotPath, gotContentType string
	var gotQuery, gotForm url.Values
	srv := startFakeServer(t, http.StatusOK, `{"EnvId":"env-1","RequestId":"req-1"}`, func(r *http.Request, b []byte) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotQuery, gotForm = parseAliyunRequest(t, r, b)
	})
	setBaseURL(t, &aliyunBaseURL, srv.URL)

	res, perr := Aliyun{}.Send(context.Background(), testAliyunChannel(), testAliyunMessage())
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "env-1" {
		t.Fatalf("provider id = %q, want env-1", res.ProviderMessageID)
	}
	if gotPath != "/" {
		t.Fatalf("path = %q, want /", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "application/x-www-form-urlencoded") {
		t.Fatalf("Content-Type = %q, want form-urlencoded", gotContentType)
	}
	// 公共参数（官方 SingleSendMail 请求示例 + RPC 公共请求参数）
	if got := gotQuery.Get("Action"); got != "SingleSendMail" {
		t.Errorf("Action = %q, want SingleSendMail", got)
	}
	if got := gotQuery.Get("Version"); got != "2017-06-06" {
		t.Errorf("Version = %q, want 2017-06-06", got)
	}
	if got := gotQuery.Get("Format"); got != "JSON" {
		t.Errorf("Format = %q, want JSON", got)
	}
	if got := gotQuery.Get("AccessKeyId"); got != "testid" {
		t.Errorf("AccessKeyId = %q, want testid", got)
	}
	if got := gotQuery.Get("SignatureMethod"); got != "HMAC-SHA1" {
		t.Errorf("SignatureMethod = %q, want HMAC-SHA1", got)
	}
	if got := gotQuery.Get("SignatureVersion"); got != "1.0" {
		t.Errorf("SignatureVersion = %q, want 1.0", got)
	}
	if got := gotQuery.Get("SignatureNonce"); got == "" {
		t.Error("SignatureNonce 不能为空")
	}
	if got := gotQuery.Get("Signature"); got == "" {
		t.Error("Signature 不能为空")
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`).MatchString(gotQuery.Get("Timestamp")) {
		t.Errorf("Timestamp = %q, want UTC ISO8601", gotQuery.Get("Timestamp"))
	}
	// 业务参数（官方 SingleSendMail 参数表）
	if got := gotForm.Get("AccountName"); got != "noreply@example.net" {
		t.Errorf("AccountName = %q, want noreply@example.net", got)
	}
	if got := gotForm.Get("AddressType"); got != "1" {
		t.Errorf("AddressType = %q, want 1", got)
	}
	if got := gotForm.Get("ReplyToAddress"); got != "false" {
		t.Errorf("ReplyToAddress = %q, want false（字符串，非 bool）", got)
	}
	if got := gotForm.Get("Subject"); got != "Hello" {
		t.Errorf("Subject = %q, want Hello", got)
	}
	if got := gotForm.Get("HtmlBody"); got != "<p>Hi</p>" {
		t.Errorf("HtmlBody = %q, want <p>Hi</p>", got)
	}
	if got := gotForm.Get("TextBody"); got != "Hi" {
		t.Errorf("TextBody = %q, want Hi", got)
	}
	if got := gotForm.Get("FromAlias"); got != "Snow" {
		t.Errorf("FromAlias = %q, want Snow", got)
	}
}

// TestAliyunSendRequestIdFallback 响应缺 EnvId 时回退 RequestId（官方两字段均有返回，容错）。
func TestAliyunSendRequestIdFallback(t *testing.T) {
	srv := startFakeServer(t, http.StatusOK, `{"RequestId":"req-only"}`, nil)
	setBaseURL(t, &aliyunBaseURL, srv.URL)
	res, perr := Aliyun{}.Send(context.Background(), testAliyunChannel(), testAliyunMessage())
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "req-only" {
		t.Fatalf("provider id = %q, want req-only", res.ProviderMessageID)
	}
}

// TestAliyunSendMergesRecipients 官方 ToAddress 单参数无独立 cc/bcc：合并顺序 To,Cc,Bcc，逗号分隔。
func TestAliyunSendMergesRecipients(t *testing.T) {
	var gotForm url.Values
	srv := startFakeServer(t, http.StatusOK, `{"EnvId":"env-2"}`, func(r *http.Request, b []byte) {
		_, gotForm = parseAliyunRequest(t, r, b)
	})
	setBaseURL(t, &aliyunBaseURL, srv.URL)

	_, perr := Aliyun{}.Send(context.Background(), testAliyunChannel(), testAliyunMessage())
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	want := "a@example.com,b@example.com,c@example.com"
	if got := gotForm.Get("ToAddress"); got != want {
		t.Fatalf("ToAddress = %q, want %q（To+Cc+Bcc 合并）", got, want)
	}
}

// TestAliyunSend403 签名/密钥类错误（官方 SignatureDoesNotMatch/InvalidAccessKeyId 为 403）→ 禁用渠道。
func TestAliyunSend403(t *testing.T) {
	srv := startFakeServer(t, http.StatusForbidden,
		`{"Message":"SignatureDoesNotMatch The signature specified is not valid.","RequestId":"r","Recommend":"x","HostId":"dm.aliyuncs.com","Code":"SignatureDoesNotMatch"}`, nil)
	setBaseURL(t, &aliyunBaseURL, srv.URL)

	_, perr := Aliyun{}.Send(context.Background(), testAliyunChannel(), testAliyunMessage())
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	if !perr.DisableChannel || perr.Retryable || perr.Cooldown {
		t.Fatalf("expected DisableChannel only, got %+v", perr)
	}
	if perr.HTTPStatus != http.StatusForbidden {
		t.Fatalf("HTTPStatus = %d, want 403", perr.HTTPStatus)
	}
	if !strings.Contains(perr.Message, "SignatureDoesNotMatch") {
		t.Fatalf("Message 应提取平台 Message 字段（大写 M），got %q", perr.Message)
	}
}

// TestAliyunSend400Param 参数类 400 → 不重试/不禁用/不冷却，携带平台原始 Message。
func TestAliyunSend400Param(t *testing.T) {
	srv := startFakeServer(t, http.StatusBadRequest,
		`{"Message":"The specified subject is invalid.","Code":"InvalidSubject.Malformed","RequestId":"r"}`, nil)
	setBaseURL(t, &aliyunBaseURL, srv.URL)

	_, perr := Aliyun{}.Send(context.Background(), testAliyunChannel(), testAliyunMessage())
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	if perr.Retryable || perr.DisableChannel || perr.Cooldown {
		t.Fatalf("expected param-class error, got %+v", perr)
	}
	if !strings.Contains(perr.Message, "The specified subject is invalid.") {
		t.Fatalf("Message 应携带平台原文, got %q", perr.Message)
	}
}

// TestAliyunSend500Retryable 服务端 5xx → 可重试。
func TestAliyunSend500Retryable(t *testing.T) {
	srv := startFakeServer(t, http.StatusInternalServerError, `{"Message":"Internal error"}`, nil)
	setBaseURL(t, &aliyunBaseURL, srv.URL)

	_, perr := Aliyun{}.Send(context.Background(), testAliyunChannel(), testAliyunMessage())
	if perr == nil || !perr.Retryable || perr.DisableChannel {
		t.Fatalf("expected Retryable 5xx, got %+v", perr)
	}
}

// TestAliyunSendChannelConfig 发信地址/密钥缺失 → channel_config（本地配置错，不 disable 渠道）。
func TestAliyunSendChannelConfig(t *testing.T) {
	ch := &model.Channel{APIKey: "testid", APISecret: "testsecret"}
	_, perr := Aliyun{}.Send(context.Background(), ch, testMessage())
	if perr == nil || perr.Code != "channel_config" || !strings.Contains(perr.Message, "FromEmail") {
		t.Fatalf("expected channel_config about FromEmail, got %+v", perr)
	}
	ch = &model.Channel{FromEmail: "noreply@example.net"}
	_, perr = Aliyun{}.Send(context.Background(), ch, testMessage())
	if perr == nil || perr.Code != "channel_config" || !strings.Contains(perr.Message, "AccessKeyId") {
		t.Fatalf("expected channel_config about AccessKeyId, got %+v", perr)
	}
}

// TestAliyunSendAttachmentUnsupported SingleSendMail 官方不支持附件：带附件直接报错而非静默丢内容。
func TestAliyunSendAttachmentUnsupported(t *testing.T) {
	srv := startFakeServer(t, http.StatusOK, `{"EnvId":"env-3"}`, nil)
	setBaseURL(t, &aliyunBaseURL, srv.URL)

	m := testAliyunMessage()
	tr := Aliyun{}
	if _, perr := tr.Send(context.Background(), testAliyunChannel(), m); perr != nil {
		t.Fatalf("无附件应正常发送, got %+v", perr)
	}
	m.Attachments = []Attachment{{Filename: "a.txt", ContentBase64: "aGVsbG8="}}
	_, perr := Aliyun{}.Send(context.Background(), testAliyunChannel(), m)
	if perr == nil || perr.Code != "unsupported_attachment" {
		t.Fatalf("expected unsupported_attachment, got %+v", perr)
	}
	if perr.Retryable || perr.DisableChannel || perr.Cooldown {
		t.Fatalf("附件不支持属请求语义错误，不应重试/禁用/冷却, got %+v", perr)
	}
}

// TestAliyunEndpointBase 端点解析：官方默认 dm.aliyuncs.com；Region 填完整主机名时覆盖。
func TestAliyunEndpointBase(t *testing.T) {
	cases := []struct{ region, want string }{
		{"", "https://dm.aliyuncs.com"},
		{"cn-hangzhou", "https://dm.aliyuncs.com"}, // 非主机名不覆盖（官方未公开区域端点）
		{"dm.cn-hangzhou.aliyuncs.com", "https://dm.cn-hangzhou.aliyuncs.com"},
		{"  dm.eu-central-1.aliyuncs.com ", "https://dm.eu-central-1.aliyuncs.com"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"}, // 带 scheme 的完整端点原样采用
	}
	for _, tc := range cases {
		if got := aliyunEndpointBase(tc.region); got != tc.want {
			t.Errorf("aliyunEndpointBase(%q) = %q, want %q", tc.region, got, tc.want)
		}
	}
}

// TestAliyunSendRegionOverride Region 主机覆盖端到端生效（请求打到覆盖主机路径的假服务）。
func TestAliyunSendRegionOverride(t *testing.T) {
	var gotHost string
	srv := startFakeServer(t, http.StatusOK, `{"EnvId":"env-4"}`, func(r *http.Request, _ []byte) {
		gotHost = r.Host
	})
	setBaseURL(t, &aliyunBaseURL, srv.URL)

	ch := testAliyunChannel()
	ch.Region = srv.URL // 带 scheme 的完整端点原样采用
	tr := Aliyun{}
	if _, perr := tr.Send(context.Background(), ch, testAliyunMessage()); perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if gotHost != strings.TrimPrefix(srv.URL, "http://") {
		t.Fatalf("request host = %q, want %q", gotHost, strings.TrimPrefix(srv.URL, "http://"))
	}
}
