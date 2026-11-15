// smtp2go_test.go SMTP2GO 适配器测试：httptest 假服务注入覆盖 baseURL（SPEC §3/§7）。
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"catmail/internal/model"
)

// smtp2goCustomHeaders 取请求体 custom_headers 为 [{header,value}] 便于断言。
func smtp2goCustomHeaders(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["custom_headers"].([]any)
	if !ok {
		t.Fatalf("custom_headers missing or not array: %#v", body["custom_headers"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("custom_headers element not object: %#v", e)
		}
		out = append(out, m)
	}
	return out
}

// TestSMTP2GoSendSuccess 成功分支：断言认证头与请求体字段（fileblob/mimetype/custom_headers 含 Reply-To）。
func TestSMTP2GoSendSuccess(t *testing.T) {
	var gotPath, gotKey string
	var gotBody map[string]any
	srv := startFakeServer(t, http.StatusOK, `{"data":{"email_id":"s2g-msg-1"},"error_code":0}`, func(r *http.Request, b []byte) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Smtp2go-API-Key")
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not json: %v", err)
		}
	})
	setBaseURL(t, &smtp2goBaseURL, srv.URL)

	m := testMessage()
	// 官方 to/cc/bcc 接受 "Name <addr>" 全格式：原样透传，不提取裸地址
	m.To = []string{"Bob <b@example.com>"}
	m.Cc = []string{"Carol <c@example.com>"}
	m.Bcc = []string{"Dave <d@example.com>"}
	res, perr := SMTP2Go{}.Send(context.Background(), &model.Channel{APIKey: "s2g_key_1234"}, m)
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "s2g-msg-1" {
		t.Fatalf("provider id = %q, want %q", res.ProviderMessageID, "s2g-msg-1")
	}
	if gotPath != "/v3/email/send" {
		t.Fatalf("path = %q, want /v3/email/send", gotPath)
	}
	if gotKey != "s2g_key_1234" {
		t.Fatalf("X-Smtp2go-API-Key = %q, want s2g_key_1234", gotKey)
	}
	wantField(t, gotBody, "sender", `"Snow" <noreply@example.com>`)
	// "Name <addr>" 全格式原样透传（官方文档 to/cc/bcc 格式即 Name <name@example.com>）
	wantField(t, gotBody, "to", []any{"Bob <b@example.com>"})
	wantField(t, gotBody, "cc", []any{"Carol <c@example.com>"})
	wantField(t, gotBody, "bcc", []any{"Dave <d@example.com>"})
	wantField(t, gotBody, "subject", "Hello")
	wantField(t, gotBody, "text_body", "Hi")
	wantField(t, gotBody, "html_body", "<p>Hi</p>")
	// 修复锁定：官方请求不含 reply_to 字段（会被静默忽略），Reply-To 走 custom_headers
	if _, ok := gotBody["reply_to"]; ok {
		t.Fatalf("request body should not contain non-official field reply_to")
	}
	// custom_headers 顺序确定：先 Reply-To（取 m.ReplyTo[0]），后用户自定义头
	wantField(t, gotBody, "custom_headers", []any{
		map[string]any{"header": "Reply-To", "value": "support@example.com"},
		map[string]any{"header": "X-Custom", "value": "v"},
	})
	// 修复锁定：附件字段为官方的 fileblob，并携带按扩展名猜测的 mimetype
	atts, ok := gotBody["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("attachments = %#v, want 1 个元素", gotBody["attachments"])
	}
	att, _ := atts[0].(map[string]any)
	if att["filename"] != "a.txt" || att["fileblob"] != "aGVsbG8=" {
		t.Fatalf("attachment = %#v, want filename/fileblob 匹配", att)
	}
	if mt, _ := att["mimetype"].(string); !strings.Contains(mt, "text/plain") { // 系统类型表可能带 charset 后缀
		t.Fatalf("mimetype = %q, want text/plain（按扩展名猜测）", mt)
	}
	if _, ok := att["filebase64"]; ok {
		t.Fatalf("attachment should not contain legacy field filebase64")
	}
}

// TestSMTP2GoForbiddenHeaders 官方禁止经 custom_headers 设置 Content-Type/Content-Transfer-Encoding/MIME-Version（大小写不敏感过滤）。
func TestSMTP2GoForbiddenHeaders(t *testing.T) {
	var gotBody map[string]any
	srv := startFakeServer(t, http.StatusOK, `{"data":{"email_id":"s2g-msg-2"},"error_code":0}`, func(_ *http.Request, b []byte) {
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not json: %v", err)
		}
	})
	setBaseURL(t, &smtp2goBaseURL, srv.URL)

	m := testMessage()
	m.Headers = map[string]string{
		"Content-Type":              "text/html",
		"content-transfer-encoding": "base64", // 小写也要被过滤
		"Mime-Version":              "1.0",
		"X-Allowed":                 "ok",
	}
	_, perr := SMTP2Go{}.Send(context.Background(), &model.Channel{APIKey: "s2g_key_1234"}, m)
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	headers := smtp2goCustomHeaders(t, gotBody)
	want := map[string]string{"Reply-To": "support@example.com", "X-Allowed": "ok"}
	if len(headers) != len(want) {
		t.Fatalf("custom_headers = %#v, want %v", headers, want)
	}
	for _, h := range headers {
		name, _ := h["header"].(string)
		val, _ := h["value"].(string)
		if want[name] != val {
			t.Fatalf("custom_headers[%q] = %q, want %v", name, val, want)
		}
	}
}

// TestSMTP2GoSend401 平台 401 → DisableChannel，不可重试/不冷却。
func TestSMTP2GoSend401(t *testing.T) {
	srv := startFakeServer(t, http.StatusUnauthorized, `{"error_code":401,"message":"Invalid API key"}`, nil)
	setBaseURL(t, &smtp2goBaseURL, srv.URL)

	_, perr := SMTP2Go{}.Send(context.Background(), &model.Channel{APIKey: "bad_key"}, testMessage())
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

// TestSMTP2GoSend429 平台 429 → Retryable + Cooldown，不禁用。
func TestSMTP2GoSend429(t *testing.T) {
	srv := startFakeServer(t, http.StatusTooManyRequests, `{"error_code":429,"message":"Rate limit exceeded"}`, nil)
	setBaseURL(t, &smtp2goBaseURL, srv.URL)

	_, perr := SMTP2Go{}.Send(context.Background(), &model.Channel{APIKey: "s2g_key_1234"}, testMessage())
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
