// Package inbound 实现送达事件的入站摄取基础设施（送达链路三件套）：
// 平台 webhook 签名校验（verify.go，Svix 规范与 Mailgun 规范的真实实现）、
// 事件归一化（normalize.go）与渠道退信率告警（bouncerate.go）。
// HTTP handler（公开入站端点）与管理端点在 internal/api 层接线。
//
// 鉴权取舍（任务书「URL 带 token 或专用请求头二选一」）：选 **URL 带 token**。
// 原因：主流邮件平台（Resend/Mailgun/Brevo/SMTP2GO…）的 webhook 均不支持配置
// 自定义请求头，URL 是唯一可由运营者携带凭据的位置；token 为每渠道独立的随机
// 串（Channel.WebhookToken，建渠道自动生成、可轮换），对无签名机制的平台（Brevo
// 等）即共享 secret，对 Resend/Mailgun 则叠加真实签名校验（见 verify.go），
// 两者独立生效、取更严格者。
package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Svix（Resend webhook 规范）常量：签名头版本前缀、whsec_ 密钥前缀、时间戳容差。
const (
	svixSignatureVersion = "v1"
	svixSecretPrefix     = "whsec_"
	// SvixTolerance 签名时间戳容差（官方库默认 5 分钟）：超窗拒绝即防重放——
	// 被截获的请求只有在窗口内重放才有效；窗口内的重放由调用方的 svix-id 去重
	// 缓存兜底（见 replay.go），两层合起来构成完整防重放。
	SvixTolerance = 5 * time.Minute
)

// Svix 校验错误（errors.Is 可判定，调用方据此统一 401 拒绝与日志归因）。
var (
	ErrSvixHeaders      = errors.New("svix: 缺少签名头")
	ErrSvixHeadersBad   = errors.New("svix: 签名头格式无效")
	ErrSvixSecret       = errors.New("svix: 签名密钥无效")
	ErrSvixNoMatch      = errors.New("svix: 签名不匹配")
	ErrSvixTooOld       = errors.New("svix: 事件时间戳过旧")
	ErrSvixTooNew       = errors.New("svix: 事件时间戳过新")
	ErrMailgunSignature = errors.New("mailgun: 签名不匹配")
)

// VerifySvix 按 Svix 规范校验 webhook 签名（Resend 送达事件走此路径）。
// 与 svix 官方 Go 库实现逐条对齐（svix-webhooks/go/webhook.go）：
//   - 密钥去掉 whsec_ 前缀后 base64(std) 解码；
//   - 签名内容 "{id}.{timestamp}.{payload}"（timestamp 取头中的 unix 秒），
//     HMAC-SHA256 后 base64 编码，形如 "v1,<base64>"；
//   - svix-signature 头允许空格分隔多个签名（轮换密钥窗口期），逐个比对，
//     仅认 v1 版本；比对用 hmac.Equal 常量时间比较；
//   - 头缺一不可（svix-id/svix-timestamp/svix-signature，兼容 Svix 的
//     webhook-* 备用头名），时间戳超 ±tolerance 拒绝（防重放主闸门）。
//
// 参数均为原始报文值：payload 必须是未经任何 re-marshal 的原始请求体字节。
func VerifySvix(secret string, payload []byte, id, timestamp, signatures string, now time.Time) error {
	if id == "" || timestamp == "" || signatures == "" {
		return ErrSvixHeaders
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, svixSecretPrefix))
	if err != nil || len(key) == 0 {
		return ErrSvixSecret
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return ErrSvixHeadersBad
	}
	// 容差双边界：过旧（重放历史报文）与过新（时钟漂移）都拒绝。
	tsTime := time.Unix(ts, 0)
	if now.Sub(tsTime) > SvixTolerance {
		return ErrSvixTooOld
	}
	if tsTime.Unix() > now.Add(SvixTolerance).Unix() {
		return ErrSvixTooNew
	}
	// 签名内容按官方口径：id + "." + timestamp 秒 + "." + 原始 payload。
	toSign := fmt.Sprintf("%s.%d.%s", id, ts, payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(toSign))
	expected := []byte(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	for _, versioned := range strings.Split(signatures, " ") {
		sigParts := strings.Split(strings.TrimSpace(versioned), ",")
		if len(sigParts) != 2 || sigParts[0] != svixSignatureVersion {
			continue
		}
		if hmac.Equal([]byte(sigParts[1]), expected) {
			return nil
		}
	}
	return ErrSvixNoMatch
}

// SvixHeaders 从请求头提取 Svix 签名三元组（svix-* 为主、webhook-* 为 Svix 的
// 兼容备用头名），返回 (id, timestamp, signatures, 是否存在)。
// exists = 三头同时存在：存在即视为 Svix 签名请求（校验失败一律拒绝，不回退
// token 校验——claimed 签名必须验真，防「自带假头绕过签名」）。
func SvixHeaders(get func(string) string) (id, timestamp, signatures string, exists bool) {
	id, timestamp, signatures = get("svix-id"), get("svix-timestamp"), get("svix-signature")
	if id == "" || timestamp == "" || signatures == "" {
		id, timestamp, signatures = get("webhook-id"), get("webhook-timestamp"), get("webhook-signature")
	}
	return id, timestamp, signatures, id != "" && timestamp != "" && signatures != ""
}

// MailgunTolerance Mailgun 签名时间戳容差：官方文档明示「webhook 处理存在平台
// 侧延迟，不要设得过紧」，取 15 分钟（Svix 官方 5 分钟口径的一半从严—放宽折中）。
const MailgunTolerance = 15 * time.Minute

// VerifyMailgun 按 Mailgun 规范校验 webhook 签名：HMAC-SHA256(timestamp+token,
// key=Webhook Signing Key) 的 hex 与 signature 恒等比对（官方 Node 示例同口径，
// subaccount 的 parent-signature 与主签名同算法，主签名校验即可）。
// 时间戳超 ±tolerance 拒绝；token 值一次性的重放去重由调用方缓存兜底（replay.go）。
func VerifyMailgun(signingKey string, timestamp, token, signature string, now time.Time) error {
	if signingKey == "" || timestamp == "" || token == "" || signature == "" {
		return ErrMailgunSignature
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return ErrMailgunSignature
	}
	tsTime := time.Unix(ts, 0)
	if now.Sub(tsTime) > MailgunTolerance || tsTime.Unix() > now.Add(MailgunTolerance).Unix() {
		return ErrMailgunSignature // 容差越界：拒绝（详情不外泄，统一按签名失败处理）
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(timestamp + token))
	expected := hex.EncodeToString(mac.Sum(nil))
	// hex 摘要恒定长度，subtle 恒定时间比较防时序侧信道。
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return ErrMailgunSignature
	}
	return nil
}
