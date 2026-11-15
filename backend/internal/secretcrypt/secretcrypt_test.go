// secretcrypt_test.go 加密层单测：格式/往返/防篡改/幂等/明文兼容/密钥校验。
package secretcrypt

import (
	"encoding/base64"
	"strings"
	"testing"
)

// testKey 固定测试密钥（32 字节）。
var testKey = []byte("0123456789abcdef0123456789abcdef")

// TestParseKey base64 + 长度校验：合法 32B 通过；非 base64 / 长度不符 / 空值拒绝。
func TestParseKey(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(testKey)
	key, err := ParseKey(good)
	if err != nil || string(key) != string(testKey) {
		t.Fatalf("ParseKey(good) = %v, %v", key, err)
	}
	if _, err := ParseKey(" " + good + " "); err != nil {
		t.Error("两侧空白应容忍")
	}
	for name, bad := range map[string]string{
		"not base64":      "!!!not-base64!!!",
		"too short":       base64.StdEncoding.EncodeToString([]byte("short")),
		"too long":        base64.StdEncoding.EncodeToString(make([]byte, 33)),
		"empty":           "",
		"whitespace only": "   ",
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%s) 应拒绝", name)
		}
	}
}

// TestEncryptDecryptRoundTrip 往返：密文带前缀、含 nonce 与 ct 两段、明文还原一致；
// 同明文两次加密产生不同 nonce（语义安全）。
func TestEncryptDecryptRoundTrip(t *testing.T) {
	for _, pt := range []string{"sk-resend-live-abc123", "user:pass@host:5432", "中文密码", strings.Repeat("x", 2048)} {
		ct, err := EncryptWith(testKey, pt)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		if !strings.HasPrefix(ct, Prefix) {
			t.Fatalf("密文应带 %q 前缀: %q", Prefix, ct)
		}
		rest := strings.TrimPrefix(ct, Prefix)
		if n := strings.Count(rest, ":"); n != 1 {
			t.Fatalf("密文体应为 nonce:ct 两段，got %d 个冒号: %q", n, ct)
		}
		got, err := DecryptWith(testKey, ct)
		if err != nil || got != pt {
			t.Fatalf("roundtrip: got=%q err=%v", got, err)
		}
	}
	ct1, _ := EncryptWith(testKey, "same-plaintext")
	ct2, _ := EncryptWith(testKey, "same-plaintext")
	if ct1 == ct2 {
		t.Error("相同明文的两次加密应产生不同密文（随机 nonce）")
	}
}

// TestDecryptTamperRejected 密文任一字节被篡改（含 nonce/ct/格式）→ 解密失败。
func TestDecryptTamperRejected(t *testing.T) {
	ct, err := EncryptWith(testKey, "secret-value")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	body := strings.TrimPrefix(ct, Prefix)
	nonceB64, ctB64, _ := strings.Cut(body, ":")
	// ct 尾字节翻转
	raw, _ := base64.StdEncoding.DecodeString(ctB64)
	raw[len(raw)-1] ^= 0x01
	tampered := Prefix + nonceB64 + ":" + base64.StdEncoding.EncodeToString(raw)
	if _, err := DecryptWith(testKey, tampered); err == nil {
		t.Error("篡改密文应解密失败（GCM 认证）")
	}
	// 错误密钥
	wrongKey := []byte("ffffffffffffffffffffffffffffffff")
	if _, err := DecryptWith(wrongKey, ct); err == nil {
		t.Error("错误密钥应解密失败")
	}
	// 格式损坏
	for _, bad := range []string{Prefix, Prefix + "only", Prefix + "!!!:???"} {
		if _, err := DecryptWith(testKey, bad); err == nil {
			t.Errorf("损坏密文 %q 应解密失败", bad)
		}
	}
	// 非密文值：DecryptWith 明确拒绝（调用方 Decrypt 才有明文兼容语义）
	if _, err := DecryptWith(testKey, "plain-text"); err == nil {
		t.Error("DecryptWith 对非密文应拒绝")
	}
}

// TestGlobalKeyPlaintextCompat 全局密钥口径（模型读写路径的实际行为）：
//   - 未设密钥：Encrypt/Decrypt 均原样透传（明文兼容现状）；
//   - 已设密钥：Encrypt 加密（空串与已加密值幂等）、Decrypt 解密
//     （未加密值透传，容忍迁移前的旧行）；密文 + 无密钥 → ErrNoKey。
func TestGlobalKeyPlaintextCompat(t *testing.T) {
	SetGlobalKey(nil)
	t.Cleanup(func() { SetGlobalKey(nil) })

	if Encrypt("plain") != "plain" {
		t.Error("无密钥时 Encrypt 应原样返回")
	}
	if v, err := Decrypt("plain"); err != nil || v != "plain" {
		t.Errorf("无密钥时 Decrypt 应透传: %q %v", v, err)
	}
	if _, err := Decrypt(Prefix + "AAAA:BBBB"); err != ErrNoKey {
		t.Errorf("无密钥遇密文应返回 ErrNoKey，got %v", err)
	}

	SetGlobalKey(testKey)
	ct := Encrypt("sk-live-test")
	if !strings.HasPrefix(ct, Prefix) {
		t.Fatalf("有密钥时 Encrypt 应加密: %q", ct)
	}
	if v, err := Decrypt(ct); err != nil || v != "sk-live-test" {
		t.Fatalf("有密钥时 Decrypt 往返失败: %q %v", v, err)
	}
	// 幂等：空串不加密；已加密值不再二次加密
	if Encrypt("") != "" {
		t.Error("空串应原样返回")
	}
	if again := Encrypt(ct); again != ct {
		t.Errorf("已加密值应原样返回（幂等）: %q", again)
	}
	// 明文兼容：迁移前的旧行（无前缀）读取透传
	if v, err := Decrypt("legacy-plain"); err != nil || v != "legacy-plain" {
		t.Errorf("旧行明文应透传: %q %v", v, err)
	}
	if !GlobalKeySet() {
		t.Error("GlobalKeySet 应为 true")
	}
}
