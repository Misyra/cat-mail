// doctor_test.go 自检包测试：临时目录 + 独立 sqlite 库跑 Run，断言输出关键行
// 与退出码；覆盖配置类 WARN（含手工构造的坏 CIDR 条目）、渠道缺字段、非法
// webhook、端口占用与 db 为 nil 的降级路径。
package doctor

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/config"
	"catmail/internal/model"
	"catmail/internal/secretcrypt"
	"catmail/internal/store"
)

// testKeyBase64 测试用 CATMAIL_SECRET_KEY：固定 32 字节的 base64（openssl rand
// -base64 的一次性产物，仅测试使用）。
const testKeyBase64 = "jP8bBi5J3wXyFOUYqXJBupzqQw3N5eWAWhI3Qfi1DDw="

// testKeyBytes testKeyBase64 解码结果（doctor 全绿场景默认已配置加密密钥：配置类
// WARN 的「干净环境」口径 = 一个正确配置的生产部署，含加密密钥）。
func testKeyBytes(t *testing.T) []byte {
	t.Helper()
	key, err := secretcrypt.ParseKey(testKeyBase64)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	return key
}

// withGlobalKey 进程级设置全局加密密钥并在测试结束还原为未设置（SetGlobalKey 为
// 全局态，测试间隔离必需）。
func withGlobalKey(t *testing.T, key []byte) {
	t.Helper()
	if key != nil {
		secretcrypt.SetGlobalKey(key)
	}
	t.Cleanup(func() { secretcrypt.SetGlobalKey(nil) })
}

// newTestEnv 临时目录 + 独立 sqlite 库构造被测环境；mutate 在开库前修改默认
// 配置（默认配置已规避全部配置类 WARN：自定义口令、loopback 监听、已设可信代理、
// 已配置加密密钥）。
func newTestEnv(t *testing.T, mutate func(*config.Config)) (config.Config, *gorm.DB) {
	t.Helper()
	cfg := config.Config{
		Addr:           "127.0.0.1:0", // 端口 0 = 试探绑定必成功
		DBPath:         filepath.Join(t.TempDir(), "test.db"),
		AdminUser:      "admin",
		AdminPass:      "s3cure-pass",
		TrustedProxies: []string{"10.0.0.0/8"},
		SecretKey:      testKeyBytes(t),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return cfg, db
}

// mustCIDR 解析测试用 CIDR，失败即 Fatal。
func mustCIDR(t *testing.T, raw string) *net.IPNet {
	t.Helper()
	_, ipnet, err := net.ParseCIDR(raw)
	if err != nil {
		t.Fatalf("parse cidr %q: %v", raw, err)
	}
	return ipnet
}

// TestRunAllOK 干净环境全绿：退出码 0，输出含各节关键行，无 WARN/FAIL/SKIP。
func TestRunAllOK(t *testing.T) {
	cfg, db := newTestEnv(t, nil)
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 0 {
		t.Fatalf("退出码 = %d, want 0\n输出:\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"[1] 环境", "程序版本: dev",
		"[2] 配置", "数据源类型: sqlite", "管理后台 IP 白名单: 未配置",
		"SMTP TLS 证书: 未配置", "SMTP 隐式 TLS 监听: 未配置", "SMTP TLS 策略: optional",
		"[3] 数据目录", "数据目录存在", "数据目录可写", "磁盘剩余空间",
		"[4] 数据库", "数据库连接正常", "PRAGMA quick_check: ok", "表行数概览: channels=0 users=0 api_keys=0 send_logs=0 notifications=0",
		"[5] 渠道配置完整性", "无渠道配置", "测试发送",
		"[6] 通知与回执", "失败通知 Webhook 未启用", "无 API Key 配置回执 Webhook",
		"[7] 端口监听", "端口可绑定: 127.0.0.1:0",
		"[8] 汇总", "0 WARN / 0 FAIL / 0 SKIP",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n完整输出:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[WARN]") || strings.Contains(got, "[FAIL]") {
		t.Errorf("干净环境不应出现 WARN/FAIL\n完整输出:\n%s", got)
	}
}

// TestRunConfigWarningsAndBadCIDR 配置类 WARN：默认口令、COOKIE_SECURE 未开且
// 非 loopback、TRUSTED_PROXIES 未设；坏 CIDR 场景——config.Load 已拒绝非法字符串，
// 这里手工构造含无法解析条目的 cfg，验证 doctor 防御性处理（不 panic、按 WARN）。
func TestRunConfigWarningsAndBadCIDR(t *testing.T) {
	cfg, db := newTestEnv(t, func(c *config.Config) {
		// ":0" 的空主机名按非 loopback 判定（doctor.go 宁严勿宽），可触发
		// COOKIE_SECURE/TRUSTED_PROXIES 两条 WARN；端口 0 总能绑定，
		// 不与本机真实运行的 8080 实例相撞（测试保持封闭可复跑）。
		c.Addr = ":0"
		c.AdminPass = "admin123"
		c.TrustedProxies = nil
		c.AdminCIDRs = []*net.IPNet{mustCIDR(t, "192.168.1.0/24"), nil}
	})
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 0 {
		t.Fatalf("仅 WARN 场景退出码 = %d, want 0\n输出:\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"[WARN] 管理员口令仍为默认 admin123",
		"[WARN] COOKIE_SECURE 未开启且监听地址非 loopback",
		"[WARN] TRUSTED_PROXIES 未设置",
		"[WARN] 管理后台 IP 白名单: 共 2 条，其中 1 条无法解析",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n完整输出:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[FAIL]") {
		t.Errorf("坏 CIDR 场景应为 WARN 而非 FAIL\n完整输出:\n%s", got)
	}
}

// TestRunChannelMissingFields 渠道缺必填字段 → FAIL（退出码 1）；缺 from_email
// → 仅 WARN。逐渠道列出缺什么。
func TestRunChannelMissingFields(t *testing.T) {
	cfg, db := newTestEnv(t, nil)
	chans := []model.Channel{
		{Name: "mg缺域名", Provider: "mailgun", Status: model.ChannelEnabled},
		{Name: "rs缺key", Provider: "resend", Status: model.ChannelEnabled},
		{Name: "mock缺from", Provider: "mock", Status: model.ChannelEnabled},
	}
	for i := range chans {
		if err := db.Create(&chans[i]).Error; err != nil {
			t.Fatalf("seed channel: %v", err)
		}
	}
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 1 {
		t.Fatalf("退出码 = %d, want 1\n输出:\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{
		`渠道 "mg缺域名"`, "缺少必填字段", "domain",
		`渠道 "rs缺key"`, "APIKey",
		`渠道 "mock缺from"`, "[WARN]", "from_email",
		"1 WARN / 2 FAIL",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n完整输出:\n%s", want, got)
		}
	}
}

// TestRunInvalidWebhooks 通知/回执 webhook 非法：启用但地址非法 → FAIL（退出码 1），
// 回执非法 Key 点名。
func TestRunInvalidWebhooks(t *testing.T) {
	cfg, db := newTestEnv(t, nil)
	if err := db.Create(&model.Setting{ID: 1, NotifyWebhookEnabled: true, NotifyWebhookURL: "nota-url"}).Error; err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	if err := db.Create(&model.ApiKey{Name: "回执Key", Key: "cm-test", Enabled: true, WebhookURL: "ftp://example.com/hook"}).Error; err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 1 {
		t.Fatalf("退出码 = %d, want 1\n输出:\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"[FAIL] 失败通知 Webhook 已启用但地址非法",
		`webhook_url 非法: 回执Key`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n完整输出:\n%s", want, got)
		}
	}
}

// TestRunValidGenericWebhookNoWarn 通用 webhook（host 不在已知机器人域名）合法，
// 不产生 WARN/FAIL（退出码 0）。
func TestRunValidGenericWebhookNoWarn(t *testing.T) {
	cfg, db := newTestEnv(t, nil)
	if err := db.Create(&model.Setting{ID: 1, NotifyWebhookEnabled: true, NotifyWebhookURL: "https://hooks.example.com/abc"}).Error; err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 0 {
		t.Fatalf("退出码 = %d, want 0\n输出:\n%s", code, out.String())
	}
	if got := out.String(); strings.Contains(got, "[WARN]") || strings.Contains(got, "[FAIL]") {
		t.Errorf("合法通用 webhook 不应告警\n完整输出:\n%s", got)
	}
}

// TestRunPortOccupied 端口被占用 → FAIL（退出码 1）。
func TestRunPortOccupied(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	cfg, db := newTestEnv(t, func(c *config.Config) { c.Addr = ln.Addr().String() })
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 1 {
		t.Fatalf("退出码 = %d, want 1\n输出:\n%s", code, out.String())
	}
	if got := out.String(); !strings.Contains(got, "[FAIL] 端口") {
		t.Errorf("端口占用应输出 FAIL\n完整输出:\n%s", got)
	}
}

// TestRunNilDB 数据库打开失败的降级路径（db 为 nil）：数据库项 FAIL（退出码 1），
// 环境与配置节照常输出，不 panic。
func TestRunNilDB(t *testing.T) {
	cfg := config.Config{Addr: "127.0.0.1:0", DBPath: filepath.Join(t.TempDir(), "x.db")}
	var out bytes.Buffer
	if code := Run(cfg, nil, &out); code != 1 {
		t.Fatalf("退出码 = %d, want 1\n输出:\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{"[1] 环境", "[2] 配置", "[FAIL] 数据库未打开"} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n完整输出:\n%s", want, got)
		}
	}
}

// TestRunPostgresSkip DSN 为 postgres 时：数据目录与 SQLite 专属项 SKIP，报告
// 不含 sqlite 专属输出（不真正连库——db 为 nil 走 FAIL 降级，SKIP 判定只看 DSN）。
func TestRunPostgresSkip(t *testing.T) {
	cfg := config.Config{
		Addr:      "127.0.0.1:0",
		DBPath:    "postgres://user:pass@localhost:5432/catmail?sslmode=disable",
		AdminPass: "s3cure-pass",
	}
	var out bytes.Buffer
	if code := Run(cfg, nil, &out); code != 1 { // db=nil：数据库项 FAIL
		t.Fatalf("退出码 = %d, want 1\n输出:\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"数据源类型: postgres（postgres://user:****@localhost:5432/catmail?sslmode=disable）",
		"[SKIP] 数据目录检查仅适用 SQLite 数据源",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n完整输出:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "user:****") || strings.Contains(got, "user:pass") {
		t.Errorf("postgres 连接串口令应被打码\n完整输出:\n%s", got)
	}
}

// TestIsLoopbackAddr loopback 判定边界：空主机（绑定全部网卡）非 loopback。
func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"127.0.0.1:8080", true},
		{"[::1]:8080", true},
		{"localhost:8080", true},
		{"192.168.1.5:8080", false},
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// ---------- SMTP TLS 入口检查项 ----------

// genDoctorCertPair 生成自签证书/私钥写入临时目录（与 config/smtpserver 用例同
// 思路的最小 ECDSA P-256 证书），返回路径。
func genDoctorCertPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "doctor-test"},
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
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// TestRunSMTPTLSConfigured TLS 配置齐备：证书可读 OK、隐式 TLS 监听生效值 OK、
// 策略生效值 OK、TLS 端口可绑定 OK；require 策略文案同步生效值。退出码 0。
func TestRunSMTPTLSConfigured(t *testing.T) {
	certPath, keyPath := genDoctorCertPair(t)
	cfg, db := newTestEnv(t, func(c *config.Config) {
		c.SMTPTLSCertFile = certPath
		c.SMTPTLSKeyFile = keyPath
		c.SMTPTLSAddr = "127.0.0.1:0" // 端口 0 = 试探绑定必成功
		c.SMTPTLSPolicy = config.SMTPTLSPolicyRequire
	})
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 0 {
		t.Fatalf("退出码 = %d, want 0\n输出:\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"[OK] SMTP TLS 证书: 可读且密钥对匹配（cert=" + certPath,
		"[OK] SMTP 隐式 TLS 监听: 127.0.0.1:0",
		"[OK] SMTP TLS 策略: require",
		"[OK] SMTP TLS 端口可绑定: 127.0.0.1:0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n完整输出:\n%s", want, got)
		}
	}
}

// TestRunSMTPTLSBadCert 手工构造的 cfg 证书文件缺失：防御性 FAIL（退出码 1），
// 不 panic（config.Load 正常路径下启动即被拒绝，doctor 仍需能出报告）。
func TestRunSMTPTLSBadCert(t *testing.T) {
	cfg, db := newTestEnv(t, func(c *config.Config) {
		c.SMTPTLSCertFile = filepath.Join(t.TempDir(), "missing.pem")
		c.SMTPTLSKeyFile = "key.pem"
	})
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 1 {
		t.Fatalf("退出码 = %d, want 1\n输出:\n%s", code, out.String())
	}
	if got := out.String(); !strings.Contains(got, "[FAIL] SMTP TLS 证书不可用") {
		t.Errorf("证书缺失应输出 FAIL\n完整输出:\n%s", got)
	}
}

// ---------- 凭据加密检查项（安全加固三件套，A） ----------

// seedEncryptedChannel 直写一条含密文凭据的渠道（绕过 serializer 的原始列写法，
// 模拟「已迁移库」的存量状态）。
func seedEncryptedChannel(t *testing.T, db *gorm.DB, name, cipherText string) {
	t.Helper()
	if err := db.Model(&model.Channel{}).Create(map[string]any{
		"name": name, "provider": "mock", "status": model.ChannelEnabled,
		"api_key": cipherText,
	}).Error; err != nil {
		t.Fatalf("seed encrypted channel: %v", err)
	}
}

// TestRunCredentialEncryptionKeyMissingWarn 未配置 CATMAIL_SECRET_KEY：WARN 提示
// 建议配置；存量迁移状态按「未启用」呈现（无密文行时不 FAIL）。
func TestRunCredentialEncryptionKeyMissingWarn(t *testing.T) {
	cfg, db := newTestEnv(t, func(c *config.Config) { c.SecretKey = nil })
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 0 {
		t.Fatalf("退出码 = %d, want 0（无密文行时明文兼容不 FAIL）\n输出:\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"[WARN] CATMAIL_SECRET_KEY 未配置",
		"[OK] 凭据加密存量迁移: 未启用",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n完整输出:\n%s", want, got)
		}
	}
}

// TestRunCredentialEncryptionCiphertextNoKey 未配置密钥但库里已有密文凭据 → FAIL
// （服务将拒绝启动，doctor 同步点名），退出码 1。
func TestRunCredentialEncryptionCiphertextNoKey(t *testing.T) {
	cfg, db := newTestEnv(t, func(c *config.Config) { c.SecretKey = nil })
	seedEncryptedChannel(t, db, "已加密渠道", secretcrypt.Prefix+"AAAA:BBBB")
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 1 {
		t.Fatalf("退出码 = %d, want 1\n输出:\n%s", code, out.String())
	}
	if got := out.String(); !strings.Contains(got, "存在 1 个含加密凭据的渠道但未配置 CATMAIL_SECRET_KEY") {
		t.Errorf("应输出密文无密钥 FAIL\n完整输出:\n%s", got)
	}
}

// TestRunCredentialEncryptionMigrated 密钥已配置且存量已迁移（密文行）：迁移状态 OK。
func TestRunCredentialEncryptionMigrated(t *testing.T) {
	key := testKeyBytes(t)
	withGlobalKey(t, key)
	ct, err := secretcrypt.EncryptWith(key, "sk-live-xxx")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	cfg, db := newTestEnv(t, nil) // SecretKey 已默认配置
	seedEncryptedChannel(t, db, "已加密渠道", ct)
	var out bytes.Buffer
	if code := Run(cfg, db, &out); code != 0 {
		t.Fatalf("退出码 = %d, want 0\n输出:\n%s", code, out.String())
	}
	if got := out.String(); !strings.Contains(got, "凭据加密存量迁移: 已完成") {
		t.Errorf("已迁移库应输出 OK\n完整输出:\n%s", got)
	}
}
