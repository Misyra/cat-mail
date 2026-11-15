// mailjet_test.go Mailjet 适配器测试：httptest 假服务注入覆盖 baseURL（SPEC §3/§7）。
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"catmail/internal/model"
)

// TestMailjetSendSuccess 成功分支：断言认证头与请求体字段（ReplyTo 单对象、Headers、Attachments）。
func TestMailjetSendSuccess(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody struct {
		Messages []map[string]any `json:"Messages"`
	}
	srv := startFakeServer(t, http.StatusOK,
		`{"Messages":[{"Status":"success","To":[{"Email":"a@example.com","MessageID":123456789}]}]}`,
		func(r *http.Request, b []byte) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			if err := json.Unmarshal(b, &gotBody); err != nil {
				t.Errorf("request body not json: %v", err)
			}
		})
	setBaseURL(t, &mailjetBaseURL, srv.URL)

	res, perr := Mailjet{}.Send(context.Background(), &model.Channel{APIKey: "mj_pub", APISecret: "mj_priv"}, testMessage())
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "123456789" {
		t.Fatalf("provider id = %q, want %q（数字 MessageID 转字符串）", res.ProviderMessageID, "123456789")
	}
	if gotPath != "/v3.1/send" {
		t.Fatalf("path = %q, want /v3.1/send", gotPath)
	}
	if gotAuth != basicAuth("mj_pub", "mj_priv") { // basicAuth 已含 "Basic " 前缀
		t.Fatalf("Authorization = %q, want %q", gotAuth, basicAuth("mj_pub", "mj_priv"))
	}
	if len(gotBody.Messages) != 1 {
		t.Fatalf("Messages len = %d, want 1", len(gotBody.Messages))
	}
	msg := gotBody.Messages[0]
	wantField(t, msg, "From", map[string]any{"Email": "noreply@example.com", "Name": "Snow"})
	wantField(t, msg, "To", []any{map[string]any{"Email": "a@example.com"}})
	wantField(t, msg, "Cc", []any{map[string]any{"Email": "b@example.com"}})
	wantField(t, msg, "Bcc", []any{map[string]any{"Email": "c@example.com"}})
	// 契约锁定：ReplyTo 为单对象 {Email}（参考官方 v3.1 schema）
	wantField(t, msg, "ReplyTo", map[string]any{"Email": "support@example.com"})
	wantField(t, msg, "Subject", "Hello")
	wantField(t, msg, "TextPart", "Hi")
	wantField(t, msg, "HTMLPart", "<p>Hi</p>")
	// 契约锁定：自定义头原样透传
	wantField(t, msg, "Headers", map[string]any{"X-Custom": "v"})
	// 契约锁定：附件带 Filename/ContentType/Base64Content 三字段
	atts, ok := msg["Attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("Attachments = %#v, want 1 个元素", msg["Attachments"])
	}
	att, _ := atts[0].(map[string]any)
	if att["Filename"] != "a.txt" || att["Base64Content"] != "aGVsbG8=" {
		t.Fatalf("attachment = %#v, want Filename/Base64Content 匹配", att)
	}
	if ct, _ := att["ContentType"].(string); !strings.Contains(ct, "text/plain") { // 系统类型表可能带 charset 后缀
		t.Fatalf("ContentType = %q, want text/plain（按扩展名猜测）", ct)
	}
}

// TestMailjetFullFormatRecipients 修复锁定：dto 层保留 "Name <addr>" 原始串，而 Mailjet 的 To/Cc/Bcc/ReplyTo
// 对象 Email 字段仅收裸地址（须提取）；From 经 Email+Name 两字段保留显示名。
func TestMailjetFullFormatRecipients(t *testing.T) {
	var gotBody struct {
		Messages []map[string]any `json:"Messages"`
	}
	srv := startFakeServer(t, http.StatusOK,
		`{"Messages":[{"Status":"success","To":[{"Email":"b@example.com","MessageID":42}]}]}`,
		func(_ *http.Request, b []byte) {
			if err := json.Unmarshal(b, &gotBody); err != nil {
				t.Errorf("request body not json: %v", err)
			}
		})
	setBaseURL(t, &mailjetBaseURL, srv.URL)

	m := testMessage()
	m.To = []string{"Bob <b@example.com>"}
	m.Cc = []string{"Carol <c@example.com>"}
	m.Bcc = []string{"Dave <d@example.com>"}
	m.ReplyTo = []string{"Eve <e@example.com>"}
	res, perr := Mailjet{}.Send(context.Background(), &model.Channel{APIKey: "mj_pub", APISecret: "mj_priv"}, m)
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "42" {
		t.Fatalf("provider id = %q, want 42", res.ProviderMessageID)
	}
	if len(gotBody.Messages) != 1 {
		t.Fatalf("Messages len = %d, want 1", len(gotBody.Messages))
	}
	msg := gotBody.Messages[0]
	wantField(t, msg, "From", map[string]any{"Email": "noreply@example.com", "Name": "Snow"})
	wantField(t, msg, "To", []any{map[string]any{"Email": "b@example.com"}})
	wantField(t, msg, "Cc", []any{map[string]any{"Email": "c@example.com"}})
	wantField(t, msg, "Bcc", []any{map[string]any{"Email": "d@example.com"}})
	wantField(t, msg, "ReplyTo", map[string]any{"Email": "e@example.com"})
}

// TestMailjetSend401 平台 401 → DisableChannel，不可重试/不冷却。
func TestMailjetSend401(t *testing.T) {
	srv := startFakeServer(t, http.StatusUnauthorized, `{"ErrorMessage":"Unauthorized"}`, nil)
	setBaseURL(t, &mailjetBaseURL, srv.URL)

	_, perr := Mailjet{}.Send(context.Background(), &model.Channel{APIKey: "bad_key"}, testMessage())
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
	if !strings.Contains(perr.Message, "Unauthorized") {
		t.Fatalf("Message should carry platform message, got %q", perr.Message)
	}
}

// TestMailjetSend400NestedError 修复锁定：v3.1 校验错误为 HTTP 400 + Messages[].Errors[].ErrorMessage
// （JSON 深度 4），错误信息应携带平台原始 message。
func TestMailjetSend400NestedError(t *testing.T) {
	srv := startFakeServer(t, http.StatusBadRequest,
		`{"Messages":[{"Status":"error","Errors":[{"ErrorCode":"mj-0006","ErrorMessage":"Illegal recycled email address","StatusCode":400}]}]}`, nil)
	setBaseURL(t, &mailjetBaseURL, srv.URL)

	_, perr := Mailjet{}.Send(context.Background(), &model.Channel{APIKey: "mj_pub", APISecret: "mj_priv"}, testMessage())
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	if perr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("HTTPStatus = %d, want 400", perr.HTTPStatus)
	}
	if perr.Retryable || perr.DisableChannel || perr.Cooldown {
		t.Fatalf("expected 参数类 400（全 false）, got %+v", perr)
	}
	if !strings.Contains(perr.Message, "Illegal recycled email address") {
		t.Fatalf("Message should carry nested platform ErrorMessage, got %q", perr.Message)
	}
}

// TestMailjetSend429 平台 429 → Retryable + Cooldown，不禁用。
func TestMailjetSend429(t *testing.T) {
	srv := startFakeServer(t, http.StatusTooManyRequests, `{"ErrorMessage":"Too many requests"}`, nil)
	setBaseURL(t, &mailjetBaseURL, srv.URL)

	_, perr := Mailjet{}.Send(context.Background(), &model.Channel{APIKey: "mj_pub"}, testMessage())
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
