// webhook_test.go webhook 通道单测：四家机器人 payload 逐字段、通用 payload、
// TestWebhook 直发与签名头契约，全部经 httptest 真实收发断言。
package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"catmail/internal/httpsig"
	"catmail/internal/provider"
)

// newCaptureServer 起一个把请求体解析为 JSON 并记录的 httptest 服务，返回
// 服务与取快照函数（handler 与断言并发访问，加锁保护）。
func newCaptureServer(t *testing.T, status int) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return bodies
	}
}

// TestBotPayloads 四家机器人 payload 逐字段正确（host 嗅探 → 各自格式，统一文本前缀）。
func TestBotPayloads(t *testing.T) {
	const wantText = "[CatMail] 标题\n正文一行"
	cases := []struct {
		name, rawURL string
		want         map[string]any
	}{
		{"feishu", "https://open.feishu.cn/open-apis/bot/v2/hook/xxx",
			map[string]any{"msg_type": "text", "content": map[string]any{"text": wantText}}},
		{"dingtalk", "https://oapi.dingtalk.com/robot/send?access_token=x",
			map[string]any{"msgtype": "text", "text": map[string]any{"content": wantText}}},
		{"wecom", "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x",
			map[string]any{"msgtype": "text", "text": map[string]any{"content": wantText}}},
		{"slack", "https://hooks.slack.com/services/T000/B000/XXX",
			map[string]any{"text": wantText}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, bodies := newCaptureServer(t, http.StatusOK)
			payload, err := buildPayload(tc.rawURL, EventChannelDisabled, LevelError, "标题", "正文一行", nil)
			if err != nil {
				t.Fatalf("buildPayload: %v", err)
			}
			if err := sendTestDirect(srv.URL, payload, ""); err != nil {
				t.Fatalf("send: %v", err)
			}
			got := bodies()
			if len(got) != 1 {
				t.Fatalf("request count = %d, want 1", len(got))
			}
			if !reflect.DeepEqual(got[0], tc.want) {
				t.Errorf("payload = %v, want %v", got[0], tc.want)
			}
		})
	}
}

// sendTestDirect 直发已构造 payload（与 TestWebhook 同一传输栈与签名逻辑）：
// secret 空串不签名。
func sendTestDirect(endpoint string, payload any, secret string) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	headers := map[string]string{}
	if h := httpsig.Sign(secret, raw, time.Now()); h != "" {
		headers[httpsig.HeaderName] = h
	}
	ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
	defer cancel()
	_, perr := provider.PostJSON(ctx, webhookClient(), endpoint, headers, json.RawMessage(raw))
	if perr == nil {
		return nil
	}
	return perr
}

// TestTestWebhook 导出的 TestWebhook：2xx 返回 nil；非 2xx 返回错误；非法 URL 报错。
func TestTestWebhook(t *testing.T) {
	srv, bodies := newCaptureServer(t, http.StatusOK)
	if err := TestWebhook(srv.URL, ""); err != nil {
		t.Fatalf("TestWebhook(200): %v", err)
	}
	got := bodies()
	if len(got) != 1 {
		t.Fatalf("request count = %d, want 1", len(got))
	}
	// 走与真实事件相同的 host 适配：httptest 地址落到通用 payload
	if got[0]["source"] != "catmail" || got[0]["event"] != "test" {
		t.Errorf("payload = %v, want source=catmail event=test", got[0])
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(bad.Close)
	if err := TestWebhook(bad.URL, ""); err == nil {
		t.Error("TestWebhook(403) 应返回错误")
	}
	if err := TestWebhook("://bad url", ""); err == nil {
		t.Error("非法 URL 应返回错误")
	}
}

// TestTestWebhookSignatureHeader 签名头契约（安全加固三件套，B）：secret 空 →
// 不发头；secret 非空 → X-CatMail-Signature 可按契约重算校验（HMAC-SHA256 覆盖
// "<t>." + 原始请求体，±5min 容差），接收端参考实现即 httpsig.Verify。
func TestTestWebhookSignatureHeader(t *testing.T) {
	const secret = "whsec-test-abcdef"
	var mu sync.Mutex
	var gotBodies [][]byte
	var gotHeaders []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBodies = append(gotBodies, raw)
		gotHeaders = append(gotHeaders, r.Header)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// secret 未配置：不发签名头（拍板口径）
	if err := TestWebhook(srv.URL, ""); err != nil {
		t.Fatalf("TestWebhook(no secret): %v", err)
	}
	mu.Lock()
	if n := len(gotBodies); n != 1 {
		t.Fatalf("request count = %d, want 1", n)
	}
	if gotHeaders[0].Get(httpsig.HeaderName) != "" {
		t.Error("secret 为空时不应携带签名头")
	}
	mu.Unlock()

	// secret 已配置：头存在且可校验
	if err := TestWebhook(srv.URL, secret); err != nil {
		t.Fatalf("TestWebhook(with secret): %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := len(gotBodies); n != 2 {
		t.Fatalf("request count = %d, want 2", n)
	}
	header := gotHeaders[1].Get(httpsig.HeaderName)
	if header == "" {
		t.Fatal("secret 已配置时应携带签名头")
	}
	if err := httpsig.Verify(secret, gotBodies[1], header, time.Now()); err != nil {
		t.Errorf("签名校验失败: %v（header=%q）", err, header)
	}
}
