// elasticemail_test.go Elastic Email v4 适配器测试：httptest 假服务注入覆盖 baseURL（SPEC §3/§7）。
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"catmail/internal/model"
)

// TestElasticEmailSendSuccess 成功分支：认证头、路径、Recipients/Content 逐字段、MessageID 解析。
func TestElasticEmailSendSuccess(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	var gotBody map[string]any
	srv := startFakeServer(t, http.StatusOK, `{"MessageID":"ee-msg-1","TransactionID":"ee-tx-1"}`, func(r *http.Request, b []byte) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not json: %v", err)
		}
	})
	setBaseURL(t, &elasticEmailBaseURL, srv.URL)

	res, perr := ElasticEmail{}.Send(context.Background(), &model.Channel{APIKey: "ee_key_1234"}, testMessage())
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "ee-msg-1" {
		t.Fatalf("provider id = %q, want ee-msg-1", res.ProviderMessageID)
	}
	if gotPath != "/emails/transactional" {
		t.Fatalf("path = %q, want /emails/transactional", gotPath)
	}
	if got := gotHeader.Get("X-ElasticEmail-ApiKey"); got != "ee_key_1234" {
		t.Fatalf("X-ElasticEmail-ApiKey = %q, want ee_key_1234", got)
	}
	recipients, ok := gotBody["Recipients"].(map[string]any)
	if !ok {
		t.Fatalf("Recipients 应为对象, got %#v", gotBody["Recipients"])
	}
	wantField(t, recipients, "To", []any{"a@example.com"})
	wantField(t, recipients, "CC", []any{"b@example.com"})
	wantField(t, recipients, "BCC", []any{"c@example.com"})
	content, ok := gotBody["Content"].(map[string]any)
	if !ok {
		t.Fatalf("Content 应为对象, got %#v", gotBody["Content"])
	}
	// net/mail.Address.String() 对可打印 ASCII 显示名统一加引号（go1.27 行为），仍为合法 RFC 5322 串
	wantField(t, content, "From", `"Snow" <noreply@example.com>`)
	wantField(t, content, "Subject", "Hello")
	wantField(t, content, "ReplyTo", "support@example.com")
	wantField(t, content, "Headers", map[string]any{"X-Custom": "v"})
	wantField(t, content, "Body", []any{
		map[string]any{"ContentType": "HTML", "Content": "<p>Hi</p>"},
		map[string]any{"ContentType": "PlainText", "Content": "Hi"},
	})
	wantField(t, content, "Attachments", []any{
		map[string]any{"BinaryContent": "aGVsbG8=", "Name": "a.txt", "ContentType": guessContentType("a.txt")},
	})
}

// TestElasticEmailSendTransactionIDFallback 响应缺 MessageID 时回退 TransactionID（官方两字段均可返回）。
func TestElasticEmailSendTransactionIDFallback(t *testing.T) {
	srv := startFakeServer(t, http.StatusOK, `{"TransactionID":"ee-tx-only"}`, nil)
	setBaseURL(t, &elasticEmailBaseURL, srv.URL)

	res, perr := ElasticEmail{}.Send(context.Background(), &model.Channel{APIKey: "ee_key_1234"}, testMessage())
	if perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	if res.ProviderMessageID != "ee-tx-only" {
		t.Fatalf("provider id = %q, want ee-tx-only", res.ProviderMessageID)
	}
}

// TestElasticEmailReplyToJoined 官方 ReplyTo 为单个字符串：多个回信地址逗号连接。
func TestElasticEmailReplyToJoined(t *testing.T) {
	var gotBody map[string]any
	srv := startFakeServer(t, http.StatusOK, `{"MessageID":"ee-msg-2"}`, func(_ *http.Request, b []byte) {
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not json: %v", err)
		}
	})
	setBaseURL(t, &elasticEmailBaseURL, srv.URL)

	m := testMessage()
	m.ReplyTo = []string{"r1@example.com", "r2@example.com"}
	tr := ElasticEmail{}
	if _, perr := tr.Send(context.Background(), &model.Channel{APIKey: "ee_key_1234"}, m); perr != nil {
		t.Fatalf("expected success, got error: %+v", perr)
	}
	content := gotBody["Content"].(map[string]any)
	if got := content["ReplyTo"]; got != "r1@example.com,r2@example.com" {
		t.Fatalf("ReplyTo = %v, want 逗号连接", got)
	}
}

// TestElasticEmailErrorClasses 错误分类：401/403 禁用、429 冷却、402/5xx 重试、其余 4xx 参数类；
// 错误体 "Message"（大写 M，官方 v4 风格）应被提取。
func TestElasticEmailErrorClasses(t *testing.T) {
	cases := []struct {
		name                         string
		status                       int
		body                         string
		retryable, disable, cooldown bool
	}{
		{"401 disable", http.StatusUnauthorized, `{"Error":2,"Message":"Access denied."}`, false, true, false},
		{"429 cooldown", http.StatusTooManyRequests, `{"Message":"Too many requests."}`, true, false, true},
		{"402 retryable", http.StatusPaymentRequired, `{"Message":"Payment required."}`, true, false, false},
		{"500 retryable", http.StatusInternalServerError, `{"Message":"Internal error."}`, true, false, false},
		{"422 param", http.StatusUnprocessableEntity, `{"Error":1,"Message":"Invalid recipient."}`, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFakeServer(t, tc.status, tc.body, nil)
			setBaseURL(t, &elasticEmailBaseURL, srv.URL)
			_, perr := ElasticEmail{}.Send(context.Background(), &model.Channel{APIKey: "ee_key_1234"}, testMessage())
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
			if !strings.Contains(perr.Message, "Message") {
				t.Errorf("错误应经 classifyStatus 携带响应体, got %q", perr.Message)
			}
		})
	}
}
