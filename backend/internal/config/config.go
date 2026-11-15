// Package config 负责从环境变量解析 CatMail 运行配置。
package config

import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config 为进程级运行配置（全部来自环境变量，缺省值见 SPEC §1）。
type Config struct {
	Addr string // 监听地址，CATMAIL_ADDR，默认 :8080
	// DBPath 数据源（CATMAIL_DB，D2 支持两种取值形态，按前缀自动选驱动）：
	//   - SQLite 文件路径（默认 ./data/catmail.db）：相对/绝对路径均可，驱动自动
	//     追加 WAL/busy_timeout pragma（SPEC §8）；
	//   - postgres:// 或 postgresql:// 开头的 PostgreSQL 连接串（如
	//     postgres://user:pass@host:5432/catmail?sslmode=disable）：走 gorm
	//     postgres 驱动（pgx stdlib）。备份等 SQLite 专属功能在 PG 连接上自动
	//     停用（store.ErrSQLiteOnly，接口 400 透传），请用 pg_dump/pg_restore。
	DBPath       string
	AdminUser    string // 首启管理员用户名，CATMAIL_ADMIN_USER，默认 admin
	AdminPass    string // 首启管理员密码，CATMAIL_ADMIN_PASS，默认 admin123
	CookieSecure bool   // 会话 Cookie 附加 Secure 属性（CATMAIL_COOKIE_SECURE，HTTPS 部署开启）
	// AdminCIDRs 管理后台 IP 白名单（CATMAIL_ADMIN_CIDR，逗号分隔 CIDR，E5）。
	// 已解析为类型安全的 []*net.IPNet；空切片 = 未配置，不限制（默认现状）。
	// 非法条目在 Load 时直接 log.Fatalf 快速失败，不会带病启动。
	AdminCIDRs []*net.IPNet
	// TrustedProxies 可信反代 CIDR 列表（CATMAIL_TRUSTED_PROXIES，逗号分隔 CIDR，
	// 已解析校验并归一化，cmd 层传给 gin SetTrustedProxies）。设置后 gin 才信任这些
	// 链路的 X-Forwarded-For 等头并从中解析真实客户端 IP——反代部署必须设置，否则
	// c.ClientIP() 取到的是反代地址（或被伪造的 XFF），管理 IP 白名单（E5）与登录
	// 限流的 IP 维度均会失效。空 = 未设置，保持 gin 默认信任行为（直连部署）。
	TrustedProxies []string
	// SMTPAddr SMTP 入口监听地址（CATMAIL_SMTP_ADDR，E1）：默认空 = 功能关闭，
	// 不监听任何端口；设为 ":2525" 等即启用 SMTP 服务，让只支持 SMTP 的本地应用
	// 把 CatMail 当普通 SMTP 服务器用（内部经 rotator 走渠道轮换发送）。
	SMTPAddr string
	// SMTPMaxSizeMB 单封邮件（DATA 载荷）最大体积上限，单位 MB
	// （CATMAIL_SMTP_MAX_SIZE_MB）：默认 25，EHLO 的 SIZE 广播与超限 552 拒收
	// 共用该值；必须为正整数（0/负数在 Load 时快速失败，见 mustSMTPMaxSizeMB）。
	SMTPMaxSizeMB int
	// SMTPTLSAddr SMTP 隐式 TLS 监听地址（CATMAIL_SMTP_TLS_ADDR，465 风格）：
	// 默认空 = 关闭；设为 ":465" 等即启用——accept 后立即 tls.Server 包装再进
	// 现有会话逻辑（防开放中继/AUTH/额度语义与明文入口完全一致）。仅在
	// SMTPTLSCertFile/SMTPTLSKeyFile 已配置时可用（隐式 TLS 无证书无从握手），
	// 否则 Load 快速失败（见 mustSMTPTLS）。
	SMTPTLSAddr string
	// SMTPTLSCertFile / SMTPTLSKeyFile TLS 证书与私钥路径（CATMAIL_SMTP_CERT_FILE /
	// CATMAIL_SMTP_KEY_FILE）：两者必须成对配置，且文件存在、可读、能作为
	// TLS 密钥对加载（Load 时校验，任何一项不满足快速失败）。配置后明文监听与
	// 隐式 TLS 监听分别广播 STARTTLS / 直接 TLS 握手；未配置则维持纯明文现状
	// （STARTTLS 不广播，CATMAIL_SMTP_TLS_ADDR 不允许启用）。
	SMTPTLSCertFile string
	SMTPTLSKeyFile  string
	// SMTPTLSPolicy STARTTLS 策略（CATMAIL_SMTP_TLS_POLICY）：optional（默认）=
	// 明文监听广播 STARTTLS 但不强制；require = 明文监听必须 STARTTLS 升级成功后
	// 才允许 AUTH（未升级时 AUTH 回 530，防明文密码泄露），隐式 TLS 监听天然豁免。
	// require 需要证书已配置（否则明文入口永远无法升级，AUTH 无从谈起），未配置
	// 证书即快速失败。取值非法在 Load 快速失败（见 ParseSMTPTLSPolicy）。
	SMTPTLSPolicy string
	// UpdateRepo 自动更新源仓库（CATMAIL_UPDATE_REPO，E8，"owner/repo"）：
	// 默认空 = 功能关闭（/api/update/check、/api/update/apply 返回 400，不做启动检查）。
	// 格式非法（非 owner/repo 两段或含非法字符）在 Load 时快速失败。
	UpdateRepo string
	// UpdatePrerelease 更新检查是否纳入预发布版本（CATMAIL_UPDATE_PRERELEASE，
	// 默认 false：仅查 GitHub latest 正式版；true 时取最近一次发布含 prerelease）。
	UpdatePrerelease bool
	// UpdateCheckOnStart 启动后异步检查一次更新（CATMAIL_UPDATE_CHECK_ON_START，
	// 默认 false）；仅当 UpdateRepo 非空时生效，发现新版本经 notify 双通道广播。
	UpdateCheckOnStart bool
	// SecretKey 渠道凭据加密密钥（CATMAIL_SECRET_KEY，安全加固三件套）：
	// base64 编码 32 字节解码结果；nil = 未配置（Channel 敏感字段明文读写，
	// 现状兼容，doctor 给 WARN）。设置但解码非法（非 base64 / 长度≠32）在 Load
	// 快速失败（见 config_secretkey.go mustSecretKey）。cmd 层据此调用
	// secretcrypt.SetGlobalKey 并驱动存量迁移。
	SecretKey []byte
}

// Load 读取环境变量并返回配置，未设置时取默认值。
func Load() Config {
	return Config{
		Addr:               envOr("CATMAIL_ADDR", ":8080"),
		DBPath:             envOr("CATMAIL_DB", "./data/catmail.db"),
		AdminUser:          envOr("CATMAIL_ADMIN_USER", "admin"),
		AdminPass:          envOr("CATMAIL_ADMIN_PASS", "admin123"),
		CookieSecure:       envBool("CATMAIL_COOKIE_SECURE"),
		AdminCIDRs:         mustAdminCIDRs(os.Getenv("CATMAIL_ADMIN_CIDR")),
		TrustedProxies:     mustTrustedProxies(os.Getenv("CATMAIL_TRUSTED_PROXIES")),
		SMTPAddr:           os.Getenv("CATMAIL_SMTP_ADDR"),
		SMTPMaxSizeMB:      mustSMTPMaxSizeMB(os.Getenv("CATMAIL_SMTP_MAX_SIZE_MB")),
		SMTPTLSAddr:        os.Getenv("CATMAIL_SMTP_TLS_ADDR"),
		SMTPTLSCertFile:    os.Getenv("CATMAIL_SMTP_CERT_FILE"),
		SMTPTLSKeyFile:     os.Getenv("CATMAIL_SMTP_KEY_FILE"),
		SMTPTLSPolicy:      mustSMTPTLS(os.Getenv("CATMAIL_SMTP_CERT_FILE"), os.Getenv("CATMAIL_SMTP_KEY_FILE"), os.Getenv("CATMAIL_SMTP_TLS_ADDR"), os.Getenv("CATMAIL_SMTP_TLS_POLICY")),
		UpdateRepo:         mustUpdateRepo(os.Getenv("CATMAIL_UPDATE_REPO")),
		UpdatePrerelease:   envBool("CATMAIL_UPDATE_PRERELEASE"),
		UpdateCheckOnStart: envBool("CATMAIL_UPDATE_CHECK_ON_START"),
		SecretKey:          mustSecretKey(os.Getenv("CATMAIL_SECRET_KEY")),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool 布尔型环境变量："1" / "true"（不区分大小写）为真，未设置或其余取值为假。
func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// ParseAdminCIDRs 解析 CATMAIL_ADMIN_CIDR：逗号分隔 CIDR 列表（如
// "192.168.1.0/24,10.0.0.5/32"，IPv4/IPv6 均可，条目两侧空白与结尾逗号容忍）。
// 空/未设置返回 nil，语义为不限制（默认现状）。仅接受 CIDR 记法，裸 IP 视为
// 非法（宁窄勿宽：单主机请显式写 /32 或 /128），任一条目非法即返回错误。
func ParseAdminCIDRs(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("条目 %q 不是合法 CIDR（单主机请用 /32 或 /128）: %w", part, err)
		}
		out = append(out, ipnet)
	}
	return out, nil
}

// mustAdminCIDRs Load 用的快速失败包装：CATMAIL_ADMIN_CIDR 非法时进程启动即
// log.Fatalf 终止并写明原因，绝不带病启动（白名单配错比不生效更危险）。
func mustAdminCIDRs(raw string) []*net.IPNet {
	cidrs, err := ParseAdminCIDRs(raw)
	if err != nil {
		log.Fatalf("config: CATMAIL_ADMIN_CIDR 解析失败: %v", err)
	}
	return cidrs
}

// mustTrustedProxies 解析 CATMAIL_TRUSTED_PROXIES：复用 ParseAdminCIDRs 的 CIDR
// 校验（同一记法与容错规则，仅接受 CIDR，裸 IP 请显式写 /32 或 /128），返回归一化
// 后的 CIDR 字符串列表（gin SetTrustedProxies 接受 CIDR 记法）。空/未设置返回 nil =
// 保持 gin 默认信任行为；任一条目非法即 log.Fatalf 快速失败——信任列表配错会直接
// 决定 ClientIP 的真伪，绝不带病启动。
func mustTrustedProxies(raw string) []string {
	cidrs, err := ParseAdminCIDRs(raw)
	if err != nil {
		log.Fatalf("config: CATMAIL_TRUSTED_PROXIES 解析失败: %v", err)
	}
	if len(cidrs) == 0 {
		return nil
	}
	out := make([]string, 0, len(cidrs))
	for _, n := range cidrs {
		out = append(out, n.String())
	}
	return out
}

// mustSMTPMaxSizeMB 解析 CATMAIL_SMTP_MAX_SIZE_MB：正整数 MB，未设置默认 25。
// 非法（非数字）或非正值 log.Fatalf 快速失败——SIZE 上限配错比不生效更危险；
// 0（「不限」）刻意不接受：EHLO SIZE 需要确定数值，无上限会放大滥用面，需要
// 更大额度请直接配置更大的 MB 值。
func mustSMTPMaxSizeMB(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 25
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		log.Fatalf("config: CATMAIL_SMTP_MAX_SIZE_MB 必须为正整数（单位 MB），当前值 %q", raw)
	}
	return n
}

// ParseUpdateRepo 校验 CATMAIL_UPDATE_REPO：空/未设置 = 功能关闭（返回空串）；
// 非空必须为 "owner/repo" 两段——GitHub 仓库名与用户/组织名允许字母数字与
// . _ -，且不以 . 开头（防 "."/".." 与路径穿越拼接），两段两侧空白容忍。
func ParseUpdateRepo(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("必须是 owner/repo 两段形式，当前值 %q", raw)
	}
	for _, p := range parts {
		if !validRepoSegment(p) {
			return "", fmt.Errorf("段 %q 含非法字符（仅允许字母数字与 . _ -，且不以 . 开头）", p)
		}
	}
	return raw, nil
}

// validRepoSegment 校验 owner/repo 单段：非空、不以 . 开头、仅字母数字与 . _ -。
func validRepoSegment(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// mustUpdateRepo Load 用的快速失败包装：CATMAIL_UPDATE_REPO 格式非法时启动即
// log.Fatalf——更新源配错会让 apply 把二进制下载到错误地址，比不生效更危险。
func mustUpdateRepo(raw string) string {
	repo, err := ParseUpdateRepo(raw)
	if err != nil {
		log.Fatalf("config: CATMAIL_UPDATE_REPO 解析失败: %v", err)
	}
	return repo
}
