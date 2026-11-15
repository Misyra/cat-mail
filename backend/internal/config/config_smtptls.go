// config_smtptls.go SMTP 入口 TLS 相关配置（CATMAIL_SMTP_TLS_ADDR /
// CATMAIL_SMTP_CERT_FILE / CATMAIL_SMTP_KEY_FILE / CATMAIL_SMTP_TLS_POLICY）
// 的解析与组合校验。校验保持纯函数（ParseSMTPTLSPolicy / ValidateSMTPTLSCertPair /
// parseSMTPTLSCombo）供单测与 doctor 复用；Load 经 mustSMTPTLS 快速失败包装——
// 证书配错会让 STARTTLS/隐式 TLS 静默失效或带病启动，比不生效更危险。
package config

import (
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"strings"
)

// SMTP TLS 策略取值（CATMAIL_SMTP_TLS_POLICY）。
const (
	SMTPTLSPolicyOptional = "optional" // 明文监听广播 STARTTLS 但不强制（默认）
	SMTPTLSPolicyRequire  = "require"  // 明文监听必须 STARTTLS 升级后才允许 AUTH
)

// ParseSMTPTLSPolicy 校验 CATMAIL_SMTP_TLS_POLICY：空/未设置 = optional（默认）；
// 接受 optional/require（大小写不敏感、两侧空白容忍，归一化为小写）；其余取值
// 返回错误。
func ParseSMTPTLSPolicy(raw string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	switch p {
	case "":
		return SMTPTLSPolicyOptional, nil
	case SMTPTLSPolicyOptional, SMTPTLSPolicyRequire:
		return p, nil
	default:
		return "", fmt.Errorf("必须是 %s 或 %s，当前值 %q", SMTPTLSPolicyOptional, SMTPTLSPolicyRequire, raw)
	}
}

// ValidateSMTPTLSCertPair 校验证书/私钥文件组合：两者均空 = 未配置 TLS（返回 nil，
// 语义为维持纯明文现状）；只配其一返回错误（半套配置无法完成握手）；成对配置则
// 校验文件存在、可读，并实际按 TLS 密钥对加载（格式错误/不匹配同样拒绝）——
// 启动期把「证书能用」一次验完，不在客户端首次握手时才发现。
func ValidateSMTPTLSCertPair(certFile, keyFile string) error {
	if certFile == "" && keyFile == "" {
		return nil
	}
	if certFile == "" || keyFile == "" {
		return fmt.Errorf("证书与私钥必须成对配置（CATMAIL_SMTP_CERT_FILE / CATMAIL_SMTP_KEY_FILE）：cert=%q key=%q", certFile, keyFile)
	}
	for label, path := range map[string]string{"证书": certFile, "私钥": keyFile} {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("%s文件不可读 %q: %w", label, path, err)
		}
		_ = f.Close()
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return fmt.Errorf("TLS 密钥对加载失败（格式错误或 cert/key 不匹配）: %w", err)
	}
	return nil
}

// parseSMTPTLSCombo SMTP TLS 配置的组合校验（纯函数，mustSMTPTLS 与 doctor 共用）：
//   - 隐式 TLS 监听（CATMAIL_SMTP_TLS_ADDR）必须先配置证书（无证书无从握手）；
//   - policy=require 必须先配置证书（否则明文入口永远无法 STARTTLS 升级，AUTH
//     无从谈起——等价于让明文入口的认证全灭，属配置事故而非安全策略）；
//   - 证书对按 ValidateSMTPTLSCertPair 全量校验。
//
// 返回归一化后的 policy。
func parseSMTPTLSCombo(certFile, keyFile, tlsAddr, policy string) (string, error) {
	policy, err := ParseSMTPTLSPolicy(policy)
	if err != nil {
		return "", fmt.Errorf("CATMAIL_SMTP_TLS_POLICY 解析失败: %w", err)
	}
	if err := ValidateSMTPTLSCertPair(certFile, keyFile); err != nil {
		return "", err
	}
	if tlsAddr != "" && certFile == "" {
		return "", fmt.Errorf("CATMAIL_SMTP_TLS_ADDR 已配置（%q）但未配置证书（CATMAIL_SMTP_CERT_FILE / CATMAIL_SMTP_KEY_FILE）：隐式 TLS 监听必须持证书握手", tlsAddr)
	}
	if policy == SMTPTLSPolicyRequire && certFile == "" {
		return "", fmt.Errorf("CATMAIL_SMTP_TLS_POLICY=require 但未配置证书：明文入口无 STARTTLS 可升级，AUTH 将被全部拒绝；请先配置 CATMAIL_SMTP_CERT_FILE / CATMAIL_SMTP_KEY_FILE")
	}
	return policy, nil
}

// mustSMTPTLS Load 用的快速失败包装：任一 TLS 配置非法即 log.Fatalf 终止并写明
// 原因，绝不带病启动。返回归一化后的 policy。
func mustSMTPTLS(certFile, keyFile, tlsAddr, policy string) string {
	p, err := parseSMTPTLSCombo(certFile, keyFile, tlsAddr, policy)
	if err != nil {
		log.Fatalf("config: SMTP TLS 配置校验失败: %v", err)
	}
	return p
}
