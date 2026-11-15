// httpsig_test.go 签名契约单测（安全加固三件套，B）：正确性/篡改拒绝/时间戳过期/
// 头格式解析边界。
package httpsig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// sign 便捷签名（固定时间）。
func signAt(secret string, body []byte, at time.Time) string {
	return Sign(secret, body, at)
}

// hmacHex 独立重算 HMAC-SHA256（测试参考实现，与被测代码路径解耦）。
func hmacHex(secret, s string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(s))
	return hex.EncodeToString(mac.Sum(nil))
}

// TestSignFormat 签名头格式：t=<unix秒>,v1=<hex(HMAC-SHA256(secret,"<t>."+body))>。
func TestSignFormat(t *testing.T) {
	now := time.Unix(1762944000, 0)
	body := []byte(`{"source":"catmail"}`)
	header := signAt("secret", body, now)
	if !strings.HasPrefix(header, "t=1762944000,v1=") {
		t.Fatalf("头格式不符: %q", header)
	}
	// 独立重算比对（参考实现口径）
	want := hmacHex("secret", "1762944000."+string(body))
	if !strings.HasSuffix(header, want) {
		t.Fatalf("v1 = %q, want %q", header, "t=1762944000,v1="+want)
	}
}

// TestSignEmptySecretNoHeader secret 未设置（拍板口径）：Sign 返回空串，调用方
// 不应发送签名头。
func TestSignEmptySecretNoHeader(t *testing.T) {
	if got := Sign("", []byte("body"), time.Now()); got != "" {
		t.Fatalf("secret 为空时 Sign = %q, want 空串（不发头）", got)
	}
}

// TestVerifyRoundTrip 正确签名通过校验；secret/body/时间戳任一被篡改拒绝。
func TestVerifyRoundTrip(t *testing.T) {
	now := time.Unix(1762944000, 0)
	body := []byte("payload-bytes")
	header := signAt("secret", body, now)

	if err := Verify("secret", body, header, now); err != nil {
		t.Fatalf("正确签名应通过: %v", err)
	}
	// 时间容差边界：±5 分钟内通过（含边界），超出拒绝
	if err := Verify("secret", body, header, now.Add(MaxSkew)); err != nil {
		t.Errorf("边界 +5min 应通过: %v", err)
	}
	if err := Verify("secret", body, header, now.Add(-MaxSkew)); err != nil {
		t.Errorf("边界 -5min 应通过: %v", err)
	}
	if err := Verify("secret", body, header, now.Add(MaxSkew+time.Second)); err != ErrExpiredTimestamp {
		t.Errorf("超容差应 ErrExpiredTimestamp，got %v", err)
	}
	if err := Verify("secret", body, header, now.Add(-MaxSkew-time.Second)); err != ErrExpiredTimestamp {
		t.Errorf("负向超容差应 ErrExpiredTimestamp，got %v", err)
	}
	// body 篡改
	if err := Verify("secret", []byte("payload-tampered"), header, now); err != ErrBadSignature {
		t.Errorf("body 篡改应拒绝，got %v", err)
	}
	// secret 篡改
	if err := Verify("wrong-secret", body, header, now); err != ErrBadSignature {
		t.Errorf("secret 错误应拒绝，got %v", err)
	}
	// 旧时间戳 + 新签名（对旧 body 重算）拒绝：签名基于 t，改 t 必须重签才可能通过
	oldHeader := signAt("secret", body, now.Add(-time.Hour))
	if err := Verify("secret", body, oldHeader, now); err != ErrExpiredTimestamp {
		t.Errorf("过期时间戳应拒绝，got %v", err)
	}
}

// TestVerifyHeaderParsing 头解析边界：缺 t / 缺 v1 / 大写 hex / 非数字 t / 未知键
// 容忍 / 空白容忍 / 超长头拒绝。
func TestVerifyHeaderParsing(t *testing.T) {
	now := time.Unix(1762944000, 0)
	body := []byte("x")
	good := signAt("secret", body, now)
	sig := strings.SplitN(good, "v1=", 2)[1]

	cases := []struct {
		name   string
		header string
		wantOK bool
	}{
		{"标准", good, true},
		{"项序无关", "v1=" + sig + ",t=1762944000", true},
		{"空白容忍", " t=1762944000, v1=" + sig + " ", true},
		{"未知键忽略", "t=1762944000,v1=" + sig + ",v2=future", true},
		{"缺 t", "v1=" + sig, false},
		{"缺 v1", "t=1762944000", false},
		{"大写 hex 拒绝", "t=1762944000,v1=" + strings.ToUpper(sig), false},
		{"hex 长度不符", "t=1762944000,v1=abcd", false},
		{"t 非数字", "t=abc,v1=" + sig, false},
		{"无等号项", "t=1762944000," + sig, false},
		{"空头", "", false},
		{"超长头", "t=1762944000,v1=" + sig + strings.Repeat(",x=1", 200), false},
	}
	for _, tc := range cases {
		err := Verify("secret", body, tc.header, now)
		if tc.wantOK && err != nil {
			t.Errorf("%s: 应通过，got %v", tc.name, err)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("%s: 应拒绝", tc.name)
		}
	}
	// 校验方未配置 secret：显式拒绝（不产生空 HMAC 误通过）
	if err := Verify("", body, good, now); err == nil {
		t.Error("校验方无 secret 应拒绝")
	}
}
