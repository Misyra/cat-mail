// aliyun_sign_test.go 阿里云 POP 签名黄金值测试：期望值由独立实现（python3 hmac/hashlib +
// urllib，按官方 SDK rpc_signature_composer.go 同一算法步骤）离线计算后逐字硬编码，
// 防止 PercentEncode 特殊规则（空格 +→%20 等）或排序回归。
package provider

import (
	"net/url"
	"regexp"
	"testing"
	"time"
)

// popTestCommon 固定的公共参数（黄金值输入，勿改：改了黄金值即失效）。
func popTestCommon() url.Values {
	v := url.Values{}
	v.Set("AccessKeyId", "testid")
	v.Set("Action", "SingleSendMail")
	v.Set("Format", "JSON")
	v.Set("SignatureMethod", "HMAC-SHA1")
	v.Set("SignatureVersion", "1.0")
	v.Set("SignatureNonce", "9e3a1b2c-0000-4000-8000-000000000001")
	v.Set("Timestamp", "2026-11-12T00:00:00Z")
	v.Set("Version", "2017-06-06")
	return v
}

// TestPopStringToSignGolden 签名原文逐字锁定。
// 期望串由 python3 独立计算（urlencode 排序 → 官方三替换 → 整串 quote → "POST&%2F&" 前缀）。
func TestPopStringToSignGolden(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{
			name: "plain",
			form: func() url.Values {
				v := url.Values{}
				v.Set("AccountName", "noreply@example.net")
				v.Set("AddressType", "1")
				v.Set("ReplyToAddress", "false")
				v.Set("ToAddress", "a@example.com,b@example.com")
				v.Set("Subject", "Hello")
				v.Set("HtmlBody", "<p>Hi</p>")
				v.Set("FromAlias", "Snow")
				return v
			}(),
			want: "POST&%2F&" +
				"AccessKeyId%3Dtestid%26AccountName%3Dnoreply%2540example.net%26Action%3DSingleSendMail%26" +
				"AddressType%3D1%26Format%3DJSON%26FromAlias%3DSnow%26HtmlBody%3D%253Cp%253EHi%253C%252Fp%253E%26" +
				"ReplyToAddress%3Dfalse%26SignatureMethod%3DHMAC-SHA1%26SignatureNonce%3D9e3a1b2c-0000-4000-8000-000000000001%26" +
				"SignatureVersion%3D1.0%26Subject%3DHello%26Timestamp%3D2026-11-12T00%253A00%253A00Z%26" +
				"ToAddress%3Da%2540example.com%252Cb%2540example.com%26Version%3D2017-06-06",
		},
		{
			name: "percent encode tricky chars",
			form: func() url.Values {
				v := url.Values{}
				v.Set("AccountName", "noreply@example.net")
				v.Set("AddressType", "1")
				v.Set("ReplyToAddress", "false")
				v.Set("ToAddress", "User Name <a@example.com>")
				v.Set("Subject", "He llo+ti*tle~中文") // 空格/+/~/中文命中官方特殊编码规则
				v.Set("HtmlBody", "<p>Hi</p>")
				v.Set("FromAlias", "Snow")
				return v
			}(),
			want: "POST&%2F&" +
				"AccessKeyId%3Dtestid%26AccountName%3Dnoreply%2540example.net%26Action%3DSingleSendMail%26" +
				"AddressType%3D1%26Format%3DJSON%26FromAlias%3DSnow%26HtmlBody%3D%253Cp%253EHi%253C%252Fp%253E%26" +
				"ReplyToAddress%3Dfalse%26SignatureMethod%3DHMAC-SHA1%26SignatureNonce%3D9e3a1b2c-0000-4000-8000-000000000001%26" +
				"SignatureVersion%3D1.0%26Subject%3DHe%2520llo%252Bti%252Atle~%25E4%25B8%25AD%25E6%2596%2587%26" +
				"Timestamp%3D2026-11-12T00%253A00%253A00Z%26ToAddress%3DUser%2520Name%2520%253Ca%2540example.com%253E%26" +
				"Version%3D2017-06-06",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := url.Values{}
			for k, vs := range popTestCommon() {
				merged[k] = vs
			}
			for k, vs := range tc.form {
				merged[k] = vs
			}
			got := popStringToSign("POST", merged)
			if got != tc.want {
				t.Fatalf("popStringToSign mismatch:\n got  %s\n want %s", got, tc.want)
			}
		})
	}
}

// TestPopSignGolden HMAC-SHA1(密钥=AccessKeySecret+"&") 签名黄金值（python3 hmac/sha1/base64 独立计算）。
func TestPopSignGolden(t *testing.T) {
	cases := []struct {
		stringToSign string
		want         string
	}{
		{
			"POST&%2F&AccessKeyId%3Dtestid%26AccountName%3Dnoreply%2540example.net%26Action%3DSingleSendMail%26" +
				"AddressType%3D1%26Format%3DJSON%26FromAlias%3DSnow%26HtmlBody%3D%253Cp%253EHi%253C%252Fp%253E%26" +
				"ReplyToAddress%3Dfalse%26SignatureMethod%3DHMAC-SHA1%26SignatureNonce%3D9e3a1b2c-0000-4000-8000-000000000001%26" +
				"SignatureVersion%3D1.0%26Subject%3DHello%26Timestamp%3D2026-11-12T00%253A00%253A00Z%26" +
				"ToAddress%3Da%2540example.com%252Cb%2540example.com%26Version%3D2017-06-06",
			"JSbRxvpzK1qPeQPQU9Aoz3JlVB8=",
		},
		{
			"POST&%2F&AccessKeyId%3Dtestid%26AccountName%3Dnoreply%2540example.net%26Action%3DSingleSendMail%26" +
				"AddressType%3D1%26Format%3DJSON%26FromAlias%3DSnow%26HtmlBody%3D%253Cp%253EHi%253C%252Fp%253E%26" +
				"ReplyToAddress%3Dfalse%26SignatureMethod%3DHMAC-SHA1%26SignatureNonce%3D9e3a1b2c-0000-4000-8000-000000000001%26" +
				"SignatureVersion%3D1.0%26Subject%3DHe%2520llo%252Bti%252Atle~%25E4%25B8%25AD%25E6%2596%2587%26" +
				"Timestamp%3D2026-11-12T00%253A00%253A00Z%26ToAddress%3DUser%2520Name%2520%253Ca%2540example.com%253E%26" +
				"Version%3D2017-06-06",
			"cPjD9KXDbNDjkLuI6INKT+uFKQc=",
		},
	}
	for i, tc := range cases {
		if got := popSign(tc.stringToSign, "testsecret"); got != tc.want {
			t.Errorf("case %d: popSign = %q, want %q", i, got, tc.want)
		}
	}
}

// TestPopTimestampAndNonce Timestamp 为 UTC ISO8601 格式（官方 SDK GetTimeInFormatISO8601 同款），
// SignatureNonce 每次调用唯一。
func TestPopTimestampAndNonce(t *testing.T) {
	ts := popTimestamp()
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`).MatchString(ts) {
		t.Fatalf("Timestamp = %q, want UTC ISO8601 (2006-01-02T15:04:05Z)", ts)
	}
	parsed, err := time.Parse(popTimestampFormat, ts)
	if err != nil || time.Since(parsed) > time.Minute {
		t.Fatalf("Timestamp 应为最近 UTC 时刻, got %q (err=%v)", ts, err)
	}
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		n := popNonce()
		if n == "" || seen[n] {
			t.Fatalf("popNonce 应非空且唯一, got %q (round %d)", n, i)
		}
		seen[n] = true
	}
}
