// mailtrap_test.go Mailtrap 适配器测试：httptest 假服务注入覆盖 baseURL（SPEC §3/§7）。
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"catmail/internal/model"
)

// TestMailtrapSendSuccess 成功分支：Api-Token 头、路径、from/to/reply_to/正文/附件逐字段、message_ids 解析。
func TestMailtrapSendSuccess(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	var gotBody map[string]any
	srv := startFakeServer(t, http.StatusOK, `{"success":true,"message_ids":["mt-msg-1","mt-msg-2"]}`, func(r *http.Request, b []byte) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not json: %v", err)
		}
	})
	setBaseURL(t, &mailtrapBaseURL, srv.URL)

	res, perr := Mailtrap{}.Send(context.Background(), &model.Channel{APIKey: "mt_key_1234"}, testMessage())
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "mt-msg-1" {
		t.Fatalf("provider id = %q, want mt-msg-1（message_ids 首个）", res.ProviderMessageID)
	}
	if gotPath != "/api/send" {
		t.Fatalf("path = %q, want /api/send", gotPath)
	}
	if got := gotHeader.Get("Api-Token"); got != "mt_key_1234" {
		t.Fatalf("Api-Token = %q, want mt_key_1234", got)
	}
	wantField(t, gotBody, "from", map[string]any{"name": "Snow", "email": "noreply@example.com"})
	wantField(t, gotBody, "to", []any{map[string]any{"email": "a@example.com"}})
	wantField(t, gotBody, "cc", []any{map[string]any{"email": "b@example.com"}})
	wantField(t, gotBody, "bcc", []any{map[string]any{"email": "c@example.com"}})
	// 官方 schema：reply_to 为单对象，多个 Reply-To 时取第一个
	wantField(t, gotBody, "reply_to", map[string]any{"email": "support@example.com"})
	wantField(t, gotBody, "subject", "Hello")
	wantField(t, gotBody, "text", "Hi")
	wantField(t, gotBody, "html", "<p>Hi</p>")
	wantField(t, gotBody, "headers", map[string]any{"X-Custom": "v"})
	wantField(t, gotBody, "attachments", []any{
		map[string]any{"filename": "a.txt", "content": "aGVsbG8=", "type": guessContentType("a.txt"), "disposition": "attachment"},
	})
}

// TestMailtrapFullFormatRecipients dto 层保留 "Name <addr>" 原始串，官方 email 字段仅收邮箱格式，应提取裸地址。
func TestMailtrapFullFormatRecipients(t *testing.T) {
	var gotBody map[string]any
	srv := startFakeServer(t, http.StatusOK, `{"success":true,"message_ids":["mt-msg-3"]}`, func(_ *http.Request, b []byte) {
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not json: %v", err)
		}
	})
	setBaseURL(t, &mailtrapBaseURL, srv.URL)

	m := testMessage()
	m.To = []string{"Bob <b@example.com>"}
	m.ReplyTo = []string{"Eve <e@example.com>"}
	tr := Mailtrap{}
	if _, perr := tr.Send(context.Background(), &model.Channel{APIKey: "mt_key_1234"}, m); perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	wantField(t, gotBody, "to", []any{map[string]any{"email": "b@example.com"}})
	wantField(t, gotBody, "reply_to", map[string]any{"email": "e@example.com"})
}

// TestMailtrapErrorClasses 错误分类（官方 400=参数错、401=凭证、403=域名/权限、500=可重试）；
// 错误体 {"success":false,"errors":[...]} 字符串数组应被提取拼接。
func TestMailtrapErrorClasses(t *testing.T) {
	cases := []struct {
		name                         string
		status                       int
		body                         string
		retryable, disable, cooldown bool
	}{
		{"401 disable", http.StatusUnauthorized, `{"success":false,"errors":["Invalid API token"]}`, false, true, false},
		{"403 disable", http.StatusForbidden, `{"success":false,"errors":["Sending domain not verified"]}`, false, true, false},
		{"429 cooldown", http.StatusTooManyRequests, `{"success":false,"errors":["Rate limit exceeded"]}`, true, false, true},
		{"402 retryable", http.StatusPaymentRequired, `{"success":false,"errors":["Monthly quota exhausted"]}`, true, false, false},
		{"500 retryable", http.StatusInternalServerError, `{"success":false,"errors":["Internal error"]}`, true, false, false},
		{"400 param", http.StatusBadRequest, `{"success":false,"errors":["Invalid recipient"]}`, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFakeServer(t, tc.status, tc.body, nil)
			setBaseURL(t, &mailtrapBaseURL, srv.URL)
			_, perr := Mailtrap{}.Send(context.Background(), &model.Channel{APIKey: "mt_key_1234"}, testMessage())
			if perr == nil {
				t.Fatal("expected error, got nil")
			}
			if perr.Retryable != tc.retryable || perr.DisableChannel != tc.disable || perr.Cooldown != tc.cooldown {
				t.Fatalf("got retry=%v disable=%v cooldown=%v, want %v/%v/%v (%s)",
					perr.Retryable, perr.DisableChannel, perr.Cooldown, tc.retryable, tc.disable, tc.cooldown, perr.Message)
			}
			if perr.HTTPStatus != tc.status {
				t.Fatalf("HTTPStatus = %d, want %d", perr.HTTPStatus, tc.status)
			}
		})
	}
}

// TestMailtrapErrorsArrayJoined 官方错误体为字符串数组：findMessage 应拼接全部元素而非丢弃。
func TestMailtrapErrorsArrayJoined(t *testing.T) {
	srv := startFakeServer(t, http.StatusBadRequest,
		`{"success":false,"errors":["Invalid recipient","Sending domain not verified"]}`, nil)
	setBaseURL(t, &mailtrapBaseURL, srv.URL)

	_, perr := Mailtrap{}.Send(context.Background(), &model.Channel{APIKey: "mt_key_1234"}, testMessage())
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(perr.Message, "Invalid recipient") || !strings.Contains(perr.Message, "Sending domain not verified") {
		t.Fatalf("errors 数组应拼接进 Message, got %q", perr.Message)
	}
}
