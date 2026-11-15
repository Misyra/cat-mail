// Package httpsig 实现 CatMail 出站 Webhook 的统一签名契约（安全加固三件套，B）：
//
//	请求头 X-CatMail-Signature: t=<unix秒>,v1=<hex>
//	v1 = hex(HMAC-SHA256(secret, "<t>." + <raw body>))
//	时间戳容差 ±5 分钟。
//
// 契约要点：
//   - 签名基于「发送的原始字节」——outbox 投递时 payload 已是落库 JSON 串，
//     逐字节原样发送并参与签名，接收方按请求体原始字节重算即可校验；
//   - secret 未配置（空串）时不签名、不发该请求头（调用方校验示例须注明
//     「配置 secret 后才有签名头」），本包 Sign 返回空串；
//   - 校验端（接收方参考实现）：解析 t 与 v1 → 以请求体原始字节 + secret 重算
//     HMAC-SHA256 → 与 v1 常量时间比对 → 校验 |now - t| ≤ 5 分钟。
package httpsig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HeaderName 签名请求头名。
const HeaderName = "X-CatMail-Signature"

// MaxSkew 时间戳容差：|now - t| 超过 5 分钟的签名视为重放拒绝。
const MaxSkew = 5 * time.Minute

// ErrInvalidHeader 头格式非法（缺失/无法解析）。
var ErrInvalidHeader = errors.New("httpsig: 签名头缺失或格式非法")

// ErrBadSignature 签名不匹配（secret 不同或报文被篡改）。
var ErrBadSignature = errors.New("httpsig: 签名不匹配")

// ErrExpiredTimestamp 时间戳超出容差（疑似重放）。
var ErrExpiredTimestamp = errors.New("httpsig: 时间戳超出 ±5 分钟容差")

// Sign 生成签名头值：secret 为空返回空串（不签名口径——调用方判空后不发头）；
// 否则返回 "t=<unix秒>,v1=<hex(HMAC-SHA256(secret, "<t>." + body)>"。
func Sign(secret string, body []byte, now time.Time) string {
	if secret == "" {
		return ""
	}
	t := now.Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", t)
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", t, hex.EncodeToString(mac.Sum(nil)))
}

// Verify 校验签名头：返回 nil = 通过。顺序：格式解析 → 时间戳容差 → 签名比对
// （先拒过期再比签名，泄露面最小；比对恒定时间防时序侧信道）。
func Verify(secret string, body []byte, header string, now time.Time) error {
	if secret == "" {
		return errors.New("httpsig: 校验方未配置 secret")
	}
	t, sig, err := parseHeader(header)
	if err != nil {
		return err
	}
	if d := now.Sub(time.Unix(t, 0)); d > MaxSkew || d < -MaxSkew {
		return ErrExpiredTimestamp
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", t)
	mac.Write(body)
	if !hmac.Equal([]byte(sig), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return ErrBadSignature
	}
	return nil
}

// parseHeader 解析 "t=<unix秒>,v1=<hex>"：项序无关、项内空白容忍、大小写敏感
// （v1 是算法版本标记，t/v1 键名小写）；含未知键忽略（向前兼容多算法并挂），
// t 或 v1 缺失即格式非法。恶意超长头在解析处即拒绝（不进入 HMAC）。
func parseHeader(header string) (int64, string, error) {
	if header == "" || len(header) > 512 {
		return 0, "", ErrInvalidHeader
	}
	var (
		t          int64
		sig        string
		okT, okSig bool
	)
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return 0, "", ErrInvalidHeader
		}
		switch key {
		case "t":
			v, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, "", ErrInvalidHeader
			}
			t, okT = v, true
		case "v1":
			if !isLowerHex(value) || len(value) != sha256.Size*2 {
				return 0, "", ErrInvalidHeader
			}
			sig, okSig = value, true
		}
	}
	if !okT || !okSig {
		return 0, "", ErrInvalidHeader
	}
	return t, sig, nil
}

// isLowerHex 仅小写 hex 字符判定（Sign 输出即小写，校验从紧）。
func isLowerHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
