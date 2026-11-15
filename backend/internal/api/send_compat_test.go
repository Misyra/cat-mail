// send_compat_test.go Resend SDK 兼容契约测试（SPEC §2 / POST /emails）：
// to/cc/bcc/reply_to 接受 string 与 string[] 两种形态（单收件人时官方 SDK 常发
// 字符串）；attachments 用官方字段 filename/content（base64，content_base64 为别名）；
// tags 为 [{name,value}]；headers 为对象。
package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestStringOrListUnmarshal stringOrList 两种 JSON 形态与 null 的解析。
func TestStringOrListUnmarshal(t *testing.T) {
	cases := []struct {
		name, raw string
		want      []string
	}{
		{"string", `"a@b.com"`, []string{"a@b.com"}},
		{"array", `["a@b.com","c@d.com"]`, []string{"a@b.com", "c@d.com"}},
		{"empty array", `[]`, []string{}},
		{"null", `null`, nil},
	}
	for _, tc := range cases {
		var s stringOrList
		if err := json.Unmarshal([]byte(tc.raw), &s); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		if !reflect.DeepEqual([]string(s), tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, []string(s), tc.want)
		}
	}
}

// TestEmailsResendSDKShapes /emails 端到端：官方 SDK 实际发出的 JSON 形态全部受理
// （种子示例渠道 mock 适配器本地成功，零外网）。
func TestEmailsResendSDKShapes(t *testing.T) {
	r, _ := newTestEnv(t)
	key := createKey(t, r, login(t, r, "admin", "admin123"), "sdk")

	// 单收件人字符串 + reply_to 字符串 + 官方 attachments(filename/content)/tags/headers
	w := doReq(r, "POST", "/emails", "", key, `{
		"from":"Snow <a@b.com>","to":"c@d.com","subject":"sdk-1","text":"t",
		"reply_to":"r@e.com",
		"headers":{"X-Custom":"v"},
		"attachments":[{"filename":"a.txt","content":"aGVsbG8="}],
		"tags":[{"name":"k","value":"v"}]
	}`)
	m := decode(t, mustStatus(t, w, http.StatusOK))
	if m["accepted"] != true {
		t.Fatalf("string shapes rejected: %v", m)
	}

	// 多收件人数组形态：to/cc/bcc/reply_to 均为数组
	w = doReq(r, "POST", "/emails", "", key,
		`{"from":"a@b.com","to":["c@d.com"],"cc":["e@f.com"],"bcc":["g@h.com"],"reply_to":["r@e.com"],"subject":"sdk-2","text":"t"}`)
	mustStatus(t, w, http.StatusOK)

	// attachments.content 字段被读取：非法 base64 的 content → 400
	w = doReq(r, "POST", "/emails", "", key,
		`{"from":"a@b.com","to":"c@d.com","subject":"s","text":"t","attachments":[{"filename":"a.txt","content":"!!!"}]}`)
	if body := mustStatus(t, w, http.StatusBadRequest); !strings.Contains(body, "base64") {
		t.Fatalf("bad content b64: %s", body)
	}

	// filename/content 与 content_base64 均缺失 → 400（提示语含两个字段名）
	w = doReq(r, "POST", "/emails", "", key,
		`{"from":"a@b.com","to":"c@d.com","subject":"s","text":"t","attachments":[{"filename":"a.txt"}]}`)
	if body := mustStatus(t, w, http.StatusBadRequest); !strings.Contains(body, "content/content_base64") {
		t.Fatalf("missing content: %s", body)
	}
}
