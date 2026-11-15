// config_smtptls_test.go SMTP TLS 配置（policy 解析 / 证书对校验 / 组合校验）：
// 证书用例以临时自签证书（t.TempDir）覆盖可读与加载失败路径。
package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// genCertPair 生成一对自签证书/私钥 PEM 文件写入 dir，返回路径（TLS 用例共用
// 的最小 ECDSA P-256 证书，IP SAN 覆盖 127.0.0.1）。
func genCertPair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "catmail-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

func TestParseSMTPTLSPolicy(t *testing.T) {
	cases := []struct {
		raw    string
		want   string
		wantOK bool
	}{
		{"", SMTPTLSPolicyOptional, true}, // 空 = 默认 optional
		{"optional", SMTPTLSPolicyOptional, true},
		{"OPTIONAL", SMTPTLSPolicyOptional, true}, // 大小写不敏感
		{" require ", SMTPTLSPolicyRequire, true}, // 两侧空白容忍
		{"require", SMTPTLSPolicyRequire, true},
		{"mandatory", "", false},
		{"require,optional", "", false},
	}
	for _, c := range cases {
		got, err := ParseSMTPTLSPolicy(c.raw)
		if c.wantOK && (err != nil || got != c.want) {
			t.Errorf("ParseSMTPTLSPolicy(%q) = %q, %v; want %q", c.raw, got, err, c.want)
		}
		if !c.wantOK && err == nil {
			t.Errorf("ParseSMTPTLSPolicy(%q) 应返回错误，实际 %q", c.raw, got)
		}
	}
}

func TestValidateSMTPTLSCertPair(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := genCertPair(t, dir)

	// 两者均空 = 未配置 TLS，合法（维持纯明文现状）。
	if err := ValidateSMTPTLSCertPair("", ""); err != nil {
		t.Errorf("empty pair should be allowed: %v", err)
	}
	// 成对且可用。
	if err := ValidateSMTPTLSCertPair(certPath, keyPath); err != nil {
		t.Errorf("valid pair: %v", err)
	}
	// 只配其一。
	for _, p := range [][2]string{{certPath, ""}, {"", keyPath}} {
		if err := ValidateSMTPTLSCertPair(p[0], p[1]); err == nil {
			t.Errorf("half pair (%q, %q) should fail", p[0], p[1])
		}
	}
	// 文件不存在。
	if err := ValidateSMTPTLSCertPair(filepath.Join(dir, "nope.pem"), keyPath); err == nil {
		t.Error("missing cert should fail")
	}
	if err := ValidateSMTPTLSCertPair(certPath, filepath.Join(dir, "nope.pem")); err == nil {
		t.Error("missing key should fail")
	}
	// 内容非 PEM/密钥对。
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write bad pem: %v", err)
	}
	if err := ValidateSMTPTLSCertPair(bad, keyPath); err == nil {
		t.Error("garbage cert should fail")
	}
}

func TestParseSMTPTLSCombo(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := genCertPair(t, dir)

	// 默认全空：optional。
	p, err := parseSMTPTLSCombo("", "", "", "")
	if err != nil || p != SMTPTLSPolicyOptional {
		t.Errorf("default combo = %q, %v; want optional", p, err)
	}
	// 全量合法（require + 证书 + TLS 监听）。
	p, err = parseSMTPTLSCombo(certPath, keyPath, ":465", "REQUIRE")
	if err != nil || p != SMTPTLSPolicyRequire {
		t.Errorf("full combo = %q, %v; want require", p, err)
	}
	// 隐式 TLS 监听必须有证书。
	_, err = parseSMTPTLSCombo("", "", ":465", "")
	if err == nil || !strings.Contains(err.Error(), "CATMAIL_SMTP_TLS_ADDR") {
		t.Errorf("tls addr without cert: err=%v, want TLS_ADDR hint", err)
	}
	// require 必须有证书（否则明文入口 AUTH 全灭，属配置事故）。
	_, err = parseSMTPTLSCombo("", "", "", "require")
	if err == nil || !strings.Contains(err.Error(), "require") {
		t.Errorf("require without cert: err=%v, want require hint", err)
	}
	// policy 非法。
	_, err = parseSMTPTLSCombo(certPath, keyPath, "", "bogus")
	if err == nil {
		t.Error("bad policy should fail")
	}
}
