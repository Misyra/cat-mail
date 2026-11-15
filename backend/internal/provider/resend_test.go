// resend_test.go Resend/Brevo 适配器测试：httptest 假服务注入覆盖 baseURL（SPEC §3/§7）。
package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"catmail/internal/model"
)

// startFakeServer 启动假平台服务：固定返回 status 与 respBody，capture 记录请求供断言。
func startFakeServer(t *testing.T, status int, respBody string, capture func(r *http.Request, body []byte)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if capture != nil {
			capture(r, b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// setBaseURL 临时覆盖适配器的包级 baseURL，测试结束还原。
func setBaseURL(t *testing.T, p *string, v string) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// testMessage 构造字段齐全的测试消息。
func testMessage() *Message {
	return &Message{
		FromName:    "Snow",
		FromAddr:    "noreply@example.com",
		To:          []string{"a@example.com"},
		Cc:          []string{"b@example.com"},
		Bcc:         []string{"c@example.com"},
		ReplyTo:     []string{"support@example.com"},
		Subject:     "Hello",
		Text:        "Hi",
		HTML:        "<p>Hi</p>",
		Headers:     map[string]string{"X-Custom": "v"},
		Attachments: []Attachment{{Filename: "a.txt", ContentBase64: "aGVsbG8="}},
		Tags:        []Tag{{Name: "campaign", Value: "nov"}},
	}
}

// wantField 断言请求体 JSON 中存在 key 且值相等。
func wantField(t *testing.T, m map[string]any, key string, want any) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Fatalf("request body missing field %q", key)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("field %q = %#v, want %#v", key, got, want)
	}
}

// TestResendSendSuccess 成功分支：断言请求体字段齐全、认证头与成功 ID。
func TestResendSendSuccess(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	var gotBody map[string]any
	srv := startFakeServer(t, http.StatusOK, `{"id":"re-msg-123"}`, func(r *http.Request, b []byte) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not json: %v", err)
		}
	})
	setBaseURL(t, &resendBaseURL, srv.URL)

	res, perr := Resend{}.Send(context.Background(), &model.Channel{APIKey: "re_key_1234"}, testMessage())
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "re-msg-123" {
		t.Fatalf("provider id = %q, want %q", res.ProviderMessageID, "re-msg-123")
	}
	if gotPath != "/emails" {
		t.Fatalf("path = %q, want /emails", gotPath)
	}
	if got := gotHeader.Get("Authorization"); got != "Bearer re_key_1234" {
		t.Fatalf("Authorization = %q, want Bearer re_key_1234", got)
	}
	// net/mail.Address.String() 对可打印 ASCII 显示名统一加引号（go1.27 行为），仍为合法 RFC 5322 串
	wantField(t, gotBody, "from", `"Snow" <noreply@example.com>`)
	wantField(t, gotBody, "to", []any{"a@example.com"})
	wantField(t, gotBody, "cc", []any{"b@example.com"})
	wantField(t, gotBody, "bcc", []any{"c@example.com"})
	wantField(t, gotBody, "reply_to", []any{"support@example.com"})
	wantField(t, gotBody, "subject", "Hello")
	wantField(t, gotBody, "html", "<p>Hi</p>")
	wantField(t, gotBody, "text", "Hi")
	wantField(t, gotBody, "headers", map[string]any{"X-Custom": "v"})
	wantField(t, gotBody, "attachments", []any{map[string]any{"filename": "a.txt", "content": "aGVsbG8="}})
	wantField(t, gotBody, "tags", []any{map[string]any{"name": "campaign", "value": "nov"}})
}

// TestResendSend401 平台 401 → DisableChannel，不可重试/不冷却。
func TestResendSend401(t *testing.T) {
	srv := startFakeServer(t, http.StatusUnauthorized, `{"name":"validation_error","message":"Invalid API key"}`, nil)
	setBaseURL(t, &resendBaseURL, srv.URL)

	_, perr := Resend{}.Send(context.Background(), &model.Channel{APIKey: "bad_key"}, testMessage())
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

// TestResendSend429 平台 429 → Retryable + Cooldown，不禁用。
func TestResendSend429(t *testing.T) {
	srv := startFakeServer(t, http.StatusTooManyRequests, `{"message":"Rate limit exceeded"}`, nil)
	setBaseURL(t, &resendBaseURL, srv.URL)

	_, perr := Resend{}.Send(context.Background(), &model.Channel{APIKey: "re_key_1234"}, testMessage())
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	if !perr.Retryable {
		t.Fatalf("expected Retryable=true, got %+v", perr)
	}
	if !perr.Cooldown {
		t.Fatalf("expected Cooldown=true, got %+v", perr)
	}
	if perr.DisableChannel {
		t.Fatalf("expected DisableChannel=false, got %+v", perr)
	}
	if perr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d, want 429", perr.HTTPStatus)
	}
}

// TestBrevoFullFormatRecipients 修复锁定：dto 层保留 "Name <addr>" 原始串，而 Brevo 的 to/cc/bcc/replyTo
// 对象 email 字段仅收裸地址（须提取）；sender 经 name+email 两字段保留显示名。
func TestBrevoFullFormatRecipients(t *testing.T) {
	var gotBody map[string]any
	srv := startFakeServer(t, http.StatusOK, `{"messageId":"brevo-msg-2"}`, func(_ *http.Request, b []byte) {
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not json: %v", err)
		}
	})
	setBaseURL(t, &brevoBaseURL, srv.URL)

	m := testMessage()
	m.To = []string{"Bob <b@example.com>"}
	m.Cc = []string{"Carol <c@example.com>"}
	m.Bcc = []string{"Dave <d@example.com>"}
	m.ReplyTo = []string{"Eve <e@example.com>"}
	_, perr := Brevo{}.Send(context.Background(), &model.Channel{APIKey: "brevo_key_1234"}, m)
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	wantField(t, gotBody, "sender", map[string]any{"name": "Snow", "email": "noreply@example.com"})
	wantField(t, gotBody, "to", []any{map[string]any{"email": "b@example.com"}})
	wantField(t, gotBody, "cc", []any{map[string]any{"email": "c@example.com"}})
	wantField(t, gotBody, "bcc", []any{map[string]any{"email": "d@example.com"}})
	wantField(t, gotBody, "replyTo", map[string]any{"email": "e@example.com"})
}

// TestBrevoSend402QuotaExhausted 修复锁定：Brevo 日额度/credits 用尽返回 HTTP 400 + code=not_enough_credits
// （官方错误码表），应按限流同款分类（Retryable+Cooldown），而非参数类错误。
func TestBrevoSend402QuotaExhausted(t *testing.T) {
	srv := startFakeServer(t, http.StatusBadRequest, `{"code":"not_enough_credits","message":"Not enough credits"}`, nil)
	setBaseURL(t, &brevoBaseURL, srv.URL)

	_, perr := Brevo{}.Send(context.Background(), &model.Channel{APIKey: "brevo_key_1234"}, testMessage())
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	if !perr.Retryable || !perr.Cooldown {
		t.Fatalf("expected Retryable=true Cooldown=true (quota exhausted), got %+v", perr)
	}
	if perr.DisableChannel {
		t.Fatalf("expected DisableChannel=false, got %+v", perr)
	}
	if perr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("HTTPStatus = %d, want 400", perr.HTTPStatus)
	}
	if !strings.Contains(perr.Message, "Not enough credits") {
		t.Fatalf("Message should carry platform message, got %q", perr.Message)
	}
}

// TestBrevoSendSuccess Brevo 成功分支（加分项）：断言 api-key 头与请求体字段。
func TestBrevoSendSuccess(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	var gotBody map[string]any
	srv := startFakeServer(t, http.StatusOK, `{"messageId":"brevo-msg-1"}`, func(r *http.Request, b []byte) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not json: %v", err)
		}
	})
	setBaseURL(t, &brevoBaseURL, srv.URL)

	res, perr := Brevo{}.Send(context.Background(), &model.Channel{APIKey: "brevo_key_1234"}, testMessage())
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "brevo-msg-1" {
		t.Fatalf("provider id = %q, want %q", res.ProviderMessageID, "brevo-msg-1")
	}
	if gotPath != "/v3/smtp/email" {
		t.Fatalf("path = %q, want /v3/smtp/email", gotPath)
	}
	if got := gotHeader.Get("api-key"); got != "brevo_key_1234" {
		t.Fatalf("api-key = %q, want brevo_key_1234", got)
	}
	wantField(t, gotBody, "sender", map[string]any{"name": "Snow", "email": "noreply@example.com"})
	wantField(t, gotBody, "to", []any{map[string]any{"email": "a@example.com"}})
	wantField(t, gotBody, "cc", []any{map[string]any{"email": "b@example.com"}})
	wantField(t, gotBody, "bcc", []any{map[string]any{"email": "c@example.com"}})
	// 官方 schema：replyTo 为单对象 {email,name}，非数组
	wantField(t, gotBody, "replyTo", map[string]any{"email": "support@example.com"})
	wantField(t, gotBody, "subject", "Hello")
	wantField(t, gotBody, "htmlContent", "<p>Hi</p>")
	wantField(t, gotBody, "textContent", "Hi")
	wantField(t, gotBody, "headers", map[string]any{"X-Custom": "v"})
	wantField(t, gotBody, "attachment", []any{map[string]any{"name": "a.txt", "content": "aGVsbG8="}})
	wantField(t, gotBody, "tags", []any{"nov"})
}
