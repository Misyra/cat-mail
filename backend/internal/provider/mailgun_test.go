// mailgun_test.go Mailgun 适配器测试：httptest 假服务 + r.ParseMultipartForm 断言表单字段（SPEC §3/§7）。
package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"catmail/internal/model"
)

// TestMailgunSendSuccess 成功分支：断言认证头与 multipart 表单字段（from/to/subject/h:头/o:tag 多个/附件内容）。
func TestMailgunSendSuccess(t *testing.T) {
	var gotPath, gotAuth string
	var form url.Values
	var attFilename, attCT, attContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("parse multipart form: %v", err)
		}
		form = r.MultipartForm.Value
		if fhs := r.MultipartForm.File["attachment"]; len(fhs) == 1 {
			attFilename = fhs[0].Filename
			attCT = fhs[0].Header.Get("Content-Type")
			f, err := fhs[0].Open()
			if err != nil {
				t.Errorf("open attachment: %v", err)
			}
			b, err := io.ReadAll(f)
			if err != nil {
				t.Errorf("read attachment: %v", err)
			}
			_ = f.Close()
			attContent = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"<20261111.123@mg.example.com>"}`))
	}))
	t.Cleanup(srv.Close)
	setBaseURL(t, &mailgunBaseUS, srv.URL)

	m := testMessage()
	// 官方 to/cc/bcc 接受 "Name <addr>" 全格式：原样透传，不提取裸地址
	m.To = []string{"Bob <b@example.com>"}
	m.Cc = []string{"Carol <c@example.com>"}
	m.Bcc = []string{"Dave <d@example.com>"}
	m.Tags = []Tag{{Name: "campaign", Value: "nov"}, {Name: "season", Value: "summer"}}
	m.Headers = map[string]string{"X-Custom": "v", "reply-to": "dup@example.com"}  // 小写也要跳过，避免与 ReplyTo 循环重复发头
	m.Attachments = []Attachment{{Filename: "a.txt", ContentBase64: "aGVs\nbG8="}} // base64 夹带换行：解码需容错

	res, perr := Mailgun{}.Send(context.Background(), &model.Channel{APIKey: "mg_key_1234", Domain: "mg.example.com"}, m)
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "<20261111.123@mg.example.com>" {
		t.Fatalf("provider id = %q, want <20261111.123@mg.example.com>", res.ProviderMessageID)
	}
	if gotPath != "/v3/mg.example.com/messages" {
		t.Fatalf("path = %q, want /v3/mg.example.com/messages", gotPath)
	}
	if gotAuth != basicAuth("api", "mg_key_1234") { // basicAuth 已含 "Basic " 前缀
		t.Fatalf("Authorization = %q, want %q", gotAuth, basicAuth("api", "mg_key_1234"))
	}
	// 可重复字段每地址一个 part；"Name <addr>" 全格式原样透传（不提取裸地址）
	if got := form["to"]; len(got) != 1 || got[0] != "Bob <b@example.com>" {
		t.Fatalf("to = %v, want [Bob <b@example.com>]", got)
	}
	if got := form["cc"]; len(got) != 1 || got[0] != "Carol <c@example.com>" {
		t.Fatalf("cc = %v, want [Carol <c@example.com>]", got)
	}
	if got := form["bcc"]; len(got) != 1 || got[0] != "Dave <d@example.com>" {
		t.Fatalf("bcc = %v, want [Dave <d@example.com>]", got)
	}
	if got := form.Get("from"); got != `"Snow" <noreply@example.com>` {
		t.Fatalf("from = %q, want %q", got, `"Snow" <noreply@example.com>`)
	}
	if got := form.Get("subject"); got != "Hello" {
		t.Fatalf("subject = %q, want Hello", got)
	}
	if got := form.Get("text"); got != "Hi" {
		t.Fatalf("text = %q, want Hi", got)
	}
	if got := form.Get("html"); got != "<p>Hi</p>" {
		t.Fatalf("html = %q, want <p>Hi</p>", got)
	}
	// 自定义头 h:<Name>
	if got := form["h:X-Custom"]; len(got) != 1 || got[0] != "v" {
		t.Fatalf("h:X-Custom = %v, want [v]", got)
	}
	// 修复锁定：Headers 里的 reply-to 被跳过，h:Reply-To 仅来自 ReplyTo 循环（不重复发头）
	if got := form["h:Reply-To"]; len(got) != 1 || got[0] != "support@example.com" {
		t.Fatalf("h:Reply-To = %v, want 仅 [support@example.com]（不重复）", got)
	}
	// 修复锁定：官方支持重复 o:tag part，每个 tag 写一个
	if got := form["o:tag"]; len(got) != 2 || got[0] != "nov" || got[1] != "summer" {
		t.Fatalf("o:tag = %v, want [nov summer]", got)
	}
	// 附件 file part：filename 原样、Content-Type 按扩展名猜测、内容为容错解码后的字节
	if attFilename != "a.txt" {
		t.Fatalf("attachment filename = %q, want a.txt", attFilename)
	}
	if !strings.Contains(attCT, "text/plain") { // 系统类型表可能带 charset 后缀
		t.Fatalf("attachment Content-Type = %q, want text/plain（按扩展名猜测）", attCT)
	}
	if attContent != "hello" {
		t.Fatalf("attachment content = %q, want hello（base64 容错解码）", attContent)
	}
}

// TestMailgunSend401 平台 401 → DisableChannel，不可重试/不冷却。
func TestMailgunSend401(t *testing.T) {
	srv := startFakeServer(t, http.StatusUnauthorized, `{"message":"Invalid API key"}`, nil)
	setBaseURL(t, &mailgunBaseUS, srv.URL)

	_, perr := Mailgun{}.Send(context.Background(),
		&model.Channel{APIKey: "bad_key", Domain: "mg.example.com"}, testMessage())
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	if !perr.DisableChannel {
		t.Fatalf("expected DisableChannel=true, got %+v", perr)
	}
	if perr.Retryable || perr.Cooldown {
		t.Fatalf("expected Retryable/Cooldown=false, got %+v", perr)
	}
	if perr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("HTTPStatus = %d, want 401", perr.HTTPStatus)
	}
	if !strings.Contains(perr.Message, "Invalid API key") {
		t.Fatalf("Message should carry platform message, got %q", perr.Message)
	}
}

// TestMailgunSend429 平台 429 → Retryable + Cooldown，不禁用。
func TestMailgunSend429(t *testing.T) {
	srv := startFakeServer(t, http.StatusTooManyRequests, `{"message":"Rate limit exceeded"}`, nil)
	setBaseURL(t, &mailgunBaseUS, srv.URL)

	_, perr := Mailgun{}.Send(context.Background(),
		&model.Channel{APIKey: "mg_key_1234", Domain: "mg.example.com"}, testMessage())
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	if !perr.Retryable || !perr.Cooldown {
		t.Fatalf("expected Retryable=true, Cooldown=true, got %+v", perr)
	}
	if perr.DisableChannel {
		t.Fatalf("expected DisableChannel=false, got %+v", perr)
	}
	if perr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d, want 429", perr.HTTPStatus)
	}
}

// TestMailgunEndpointRegion region 路由：eu（大小写/空白容错）走 EU 节点，其余 US。
func TestMailgunEndpointRegion(t *testing.T) {
	cases := []struct{ region, domain, want string }{
		{"", "mg.example.com", "https://api.mailgun.net/v3/mg.example.com/messages"},
		{"us", "mg.example.com", "https://api.mailgun.net/v3/mg.example.com/messages"},
		{"eu", "mg.example.com", "https://api.eu.mailgun.net/v3/mg.example.com/messages"},
		{" EU ", "mg.example.com", "https://api.eu.mailgun.net/v3/mg.example.com/messages"},
		{"eu", " mg.example.com ", "https://api.eu.mailgun.net/v3/mg.example.com/messages"},
	}
	for _, tc := range cases {
		if got := mailgunEndpoint(tc.region, tc.domain); got != tc.want {
			t.Errorf("mailgunEndpoint(%q, %q) = %q, want %q", tc.region, tc.domain, got, tc.want)
		}
	}
}

// TestMailgunSendConfigErrors 渠道配置与请求装配错误分支：缺域名/坏代理/附件解码失败/响应缺 id。
func TestMailgunSendConfigErrors(t *testing.T) {
	_, perr := Mailgun{}.Send(context.Background(), &model.Channel{APIKey: "k"}, testMessage())
	if perr == nil || perr.Code != "channel_config" || !perr.Retryable {
		t.Fatalf("缺发送域名应报 channel_config+Retryable, got %+v", perr)
	}

	srv := startFakeServer(t, http.StatusOK, `{"id":"ok"}`, nil)
	setBaseURL(t, &mailgunBaseUS, srv.URL)

	_, perr = Mailgun{}.Send(context.Background(), &model.Channel{APIKey: "k", Domain: "d", ProxyURL: "::::"}, testMessage())
	if perr == nil || perr.Code != "proxy_error" || !perr.Retryable {
		t.Fatalf("非法代理 URL 应报 proxy_error+Retryable, got %+v", perr)
	}

	m := testMessage()
	m.Attachments = []Attachment{{Filename: "a.txt", ContentBase64: "!!!not-base64!!!"}}
	_, perr = Mailgun{}.Send(context.Background(), &model.Channel{APIKey: "k", Domain: "d"}, m)
	if perr == nil || perr.Code != "encode_failed" || !strings.Contains(perr.Message, "attachment") {
		t.Fatalf("附件 base64 非法应报 encode_failed（含文件名定位）, got %+v", perr)
	}

	srv2 := startFakeServer(t, http.StatusOK, `{}`, nil)
	setBaseURL(t, &mailgunBaseUS, srv2.URL)
	_, perr = Mailgun{}.Send(context.Background(), &model.Channel{APIKey: "k", Domain: "d"}, testMessage())
	if perr == nil || perr.Code != "unexpected_response" {
		t.Fatalf("响应缺 id 应报 unexpected_response, got %+v", perr)
	}
}
