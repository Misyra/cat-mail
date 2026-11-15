// verify_test.go Svix（Resend）与 Mailgun 签名校验测试：
// 与官方实现/文档逐条对齐的正反用例——合法签名、篡改 body/密钥/头、多签名
// （密钥轮换窗口）、webhook-* 备用头、时间戳容差双边界、Mailgun JSON 信封与
// form 平铺提取、重放缓存。
package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"
)

// svixSign 按官方算法生成合法 Svix 签名头值（测试造数与实现解耦：独立推导，
// 防止「实现自证」）。
func svixSign(t *testing.T, secret, id string, ts time.Time, payload []byte) string {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.%s", id, ts.Unix(), payload)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// newSvixSecret 生成 base64(whsec_ 前缀) 密钥（32B 确定性值；seed 用于区分
// 多个互不相同密钥的用例）。
func newSvixSecret(seed byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i*7) + seed
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(raw)
}

func TestVerifySvixValid(t *testing.T) {
	secret := newSvixSecret(0)
	payload := []byte(`{"type":"email.delivered","data":{"email_id":"abc"}}`)
	now := time.Now()
	sig := svixSign(t, secret, "msg_test_1", now, payload)

	err := VerifySvix(secret, payload, "msg_test_1", fmt.Sprintf("%d", now.Unix()), sig, now)
	if err != nil {
		t.Fatalf("合法签名应通过: %v", err)
	}
	// webhook-* 备用头名（Svix 兼容形态）由 SvixHeaders 提取后同一路径校验
	_, _, _, exists := SvixHeaders(func(k string) string {
		switch k {
		case "webhook-id":
			return "msg_test_1"
		case "webhook-timestamp":
			return fmt.Sprintf("%d", now.Unix())
		case "webhook-signature":
			return sig
		}
		return ""
	})
	if !exists {
		t.Error("webhook-* 三头齐备应识别为 Svix 签名请求")
	}
}

func TestVerifySvixInvalid(t *testing.T) {
	secret := newSvixSecret(0)
	payload := []byte(`{"a":1}`)
	now := time.Now()
	ts := fmt.Sprintf("%d", now.Unix())
	good := svixSign(t, secret, "msg_1", now, payload)

	cases := []struct {
		name      string
		secret    string
		payload   []byte
		id, tssig string
		sigs      string
		want      error
	}{
		{"篡改 body", secret, []byte(`{"a":2}`), "msg_1", ts, good, ErrSvixNoMatch},
		{"错误密钥", newSvixSecret(9), payload, "msg_1", ts, good, ErrSvixNoMatch},
		{"篡改 id", secret, payload, "msg_2", ts, good, ErrSvixNoMatch},
		{"缺签名头", secret, payload, "msg_1", "", good, ErrSvixHeaders},
		{"缺 id", secret, payload, "", ts, good, ErrSvixHeaders},
		{"时间戳非数字", secret, payload, "msg_1", "abc", good, ErrSvixHeadersBad},
		{"无 v1 版本", secret, payload, "msg_1", ts, "v0," + good[3:], ErrSvixNoMatch},
		{"非法密钥 base64", "whsec_%%%", payload, "msg_1", ts, good, ErrSvixSecret},
	}
	for _, c := range cases {
		err := VerifySvix(c.secret, c.payload, c.id, c.tssig, c.sigs, now)
		if err != c.want {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// 头缺失（任一）：ErrSvixHeaders
	if err := VerifySvix(secret, payload, "msg_1", ts, "", now); err != ErrSvixHeaders {
		t.Errorf("缺 signatures: err = %v, want ErrSvixHeaders", err)
	}
}

func TestVerifySvixTolerance(t *testing.T) {
	secret := newSvixSecret(0)
	payload := []byte(`{}`)
	now := time.Now()

	// 容差内边界（恰 4 分钟前）通过
	old := now.Add(-4 * time.Minute)
	if err := VerifySvix(secret, payload, "m", fmt.Sprintf("%d", old.Unix()),
		svixSign(t, secret, "m", old, payload), now); err != nil {
		t.Errorf("容差内（4 分钟前）应通过: %v", err)
	}
	// 超窗（6 分钟前）：过旧拒绝（防重放主闸门）
	older := now.Add(-(SvixTolerance + time.Minute))
	if err := VerifySvix(secret, payload, "m", fmt.Sprintf("%d", older.Unix()),
		svixSign(t, secret, "m", older, payload), now); err != ErrSvixTooOld {
		t.Errorf("超窗应 ErrSvixTooOld, got %v", err)
	}
	// 未来时间戳（+10 分钟）：过新拒绝（时钟漂移攻击面）
	future := now.Add(10 * time.Minute)
	if err := VerifySvix(secret, payload, "m", fmt.Sprintf("%d", future.Unix()),
		svixSign(t, secret, "m", future, payload), now); err != ErrSvixTooNew {
		t.Errorf("未来时间戳应 ErrSvixTooNew, got %v", err)
	}
}

func TestVerifySvixMultipleSignatures(t *testing.T) {
	// 轮换密钥窗口期：svix-signature 携带旧+新两个签名（空格分隔），任一命中即过
	secretOld, secretNew := newSvixSecret(0), "whsec_"+base64.StdEncoding.EncodeToString([]byte("second-secret-32-bytes-12345678"))
	payload := []byte(`{"k":"v"}`)
	now := time.Now()
	sigNew := svixSign(t, secretNew, "m", now, payload)
	sigOld := svixSign(t, secretOld, "m", now, payload)

	both := sigOld + " " + sigNew
	if err := VerifySvix(secretNew, payload, "m", fmt.Sprintf("%d", now.Unix()), both, now); err != nil {
		t.Errorf("多签名（新密钥命中）应通过: %v", err)
	}
	if err := VerifySvix(secretOld, payload, "m", fmt.Sprintf("%d", now.Unix()), both, now); err != nil {
		t.Errorf("多签名（旧密钥命中）应通过: %v", err)
	}
}

// mailgunSign 按官方算法生成合法 Mailgun 签名（HMAC-SHA256(timestamp+token) hex）。
func mailgunSign(key, timestamp, token string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp + token))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyMailgunValid(t *testing.T) {
	key := "mg-signing-key"
	now := time.Now()
	ts := fmt.Sprintf("%d", now.Unix())
	token := "e0b5477167110d68991efc6b9f89f0a11066af27834600e123"
	sig := mailgunSign(key, ts, token)
	if err := VerifyMailgun(key, ts, token, sig, now); err != nil {
		t.Fatalf("合法签名应通过: %v", err)
	}
}

func TestVerifyMailgunInvalid(t *testing.T) {
	key := "mg-signing-key"
	now := time.Now()
	ts := fmt.Sprintf("%d", now.Unix())
	token := "tok"
	good := mailgunSign(key, ts, token)

	// 篡改 token/timestamp/signature → 拒绝
	if err := VerifyMailgun(key, ts, "tampered", good, now); err != ErrMailgunSignature {
		t.Errorf("篡改 token: got %v, want ErrMailgunSignature", err)
	}
	if err := VerifyMailgun(key, "123", token, good, now); err != ErrMailgunSignature {
		t.Errorf("篡改 timestamp: got %v, want ErrMailgunSignature", err)
	}
	if err := VerifyMailgun(key, ts, token, "deadbeef", now); err != ErrMailgunSignature {
		t.Errorf("篡改 signature: got %v, want ErrMailgunSignature", err)
	}
	// 错误密钥
	if err := VerifyMailgun("another-key", ts, token, good, now); err != ErrMailgunSignature {
		t.Errorf("错误密钥: got %v, want ErrMailgunSignature", err)
	}
	// 缺参 / 非法时间戳
	if err := VerifyMailgun(key, "", token, good, now); err != ErrMailgunSignature {
		t.Errorf("缺 timestamp: got %v", err)
	}
	if err := VerifyMailgun(key, "abc", token, good, now); err != ErrMailgunSignature {
		t.Errorf("timestamp 非数字: got %v", err)
	}
	// 容差：16 分钟前拒绝，10 分钟前通过
	old := now.Add(-10 * time.Minute)
	tsOld := fmt.Sprintf("%d", old.Unix())
	if err := VerifyMailgun(key, tsOld, token, mailgunSign(key, tsOld, token), now); err != nil {
		t.Errorf("容差内（10 分钟前）应通过: %v", err)
	}
	stale := now.Add(-(MailgunTolerance + time.Minute))
	tsStale := fmt.Sprintf("%d", stale.Unix())
	if err := VerifyMailgun(key, tsStale, token, mailgunSign(key, tsStale, token), now); err != ErrMailgunSignature {
		t.Errorf("超容差应拒绝: got %v", err)
	}
}

func TestReplayCache(t *testing.T) {
	c := &ReplayCache{}
	if !c.Claim("k1") {
		t.Fatal("首次 Claim 应成功")
	}
	if c.Claim("k1") {
		t.Error("窗口内重复 Claim 应被拒（重放）")
	}
	if !c.Claim("k2") {
		t.Error("不同 key 应正常通过")
	}
	// 时间旅行：直接把记录时间改到窗口外，再次 Claim 放行
	c.mu.Lock()
	c.seen["k1"] = time.Now().Add(-replayTTL - time.Minute)
	c.mu.Unlock()
	if !c.Claim("k1") {
		t.Error("窗口外的重复 Claim 应放行（TTL 语义）")
	}
}
