// Package doctor 实现 `catmail doctor` 自检子命令（E7）：对本机部署做一次
// 只读体检（不出网、不改业务数据），逐项输出 [OK]/[WARN]/[FAIL]/[SKIP] 与中文
// 说明，末尾汇总计数；存在任一 FAIL 项时进程退出码为 1，否则 0。
//
// 包保持函数式可测：Run(cfg, db, stdout) int 不依赖全局状态；db 允许为 nil
// （数据库打开失败时仍输出环境/配置/数据目录等其余检查项，数据库相关项按
// FAIL/SKIP 呈现，由调用方先把打开错误写到 stderr）。
package doctor

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gorm.io/gorm"

	"catmail/internal/config"
	"catmail/internal/model"
	"catmail/internal/provider"
	"catmail/internal/secretcrypt"
	"catmail/internal/store"
	"catmail/internal/version"
)

// 检查结果状态（输出前缀，同时用于计数）。
const (
	statusOK   = "OK"
	statusWarn = "WARN"
	statusFail = "FAIL"
	statusSkip = "SKIP"
)

// walWarnBytes SQLite WAL 文件大小告警阈值（512MB）：WAL 无限增长通常意味着
// 长期没有 checkpoint（长事务/只读连接残留），需关注。
const walWarnBytes = 512 << 20

// errDiskUnsupported 当前平台不支持磁盘剩余空间检查（Windows 等，见
// disk_free_other.go），doctor 以 SKIP 呈现并在文案注明。
var errDiskUnsupported = errors.New("当前平台不支持磁盘空间检查")

// Run 执行全部自检并返回退出码（任一 FAIL → 1，否则 0）。版本号统一取
// internal/version（构建时 -ldflags 注入，未注入显示 dev）。
func Run(cfg config.Config, db *gorm.DB, out io.Writer) int {
	return RunVersion(cfg, db, out, version.Version)
}

// RunVersion 同 Run，允许调用方注入程序版本号（cmd 层传入 main.version）。
func RunVersion(cfg config.Config, db *gorm.DB, out io.Writer, version string) int {
	r := &report{out: out}
	fmt.Fprintln(r.out, "CatMail doctor 自检报告")
	fmt.Fprintln(r.out, "========================")

	checkEnv(r, version)
	checkConfig(r, cfg)
	checkDataDir(r, cfg)
	checkDatabase(r, cfg, db)
	checkChannels(r, db)
	checkNotifyReceipt(r, db)
	checkPort(r, cfg)

	r.section("[8] 汇总")
	fmt.Fprintf(r.out, "  %d OK / %d WARN / %d FAIL / %d SKIP\n", r.ok, r.warn, r.fail, r.skip)
	if r.fail > 0 {
		fmt.Fprintln(r.out, "  结论: 存在 FAIL 项，请先修复后重试。")
		return 1
	}
	fmt.Fprintln(r.out, "  结论: 未发现致命问题（WARN 项建议关注）。")
	return 0
}

// ---------- [1] 环境 ----------

// checkEnv 环境自检：Go 版本、操作系统/架构、程序版本（均为展示项，恒 OK）。
func checkEnv(r *report, version string) {
	r.section("[1] 环境")
	r.item(statusOK, "Go 版本: "+runtime.Version())
	r.item(statusOK, fmt.Sprintf("操作系统/架构: %s/%s", runtime.GOOS, runtime.GOARCH))
	r.item(statusOK, "程序版本: "+version)
}

// ---------- [2] 配置 ----------

// checkConfig 配置自检：报告生效值并对三类常见部署隐患给 WARN（默认口令、
// 明文 Cookie、可信反代未设）。非法 CIDR 在 config.Load 已快速失败，这里仅
// 防御手工构造的 cfg（含无法解析条目时 WARN，不 panic）。
func checkConfig(r *report, cfg config.Config) {
	r.section("[2] 配置")
	r.item(statusOK, "监听地址: "+cfg.Addr)
	if store.IsPostgresDSN(cfg.DBPath) {
		r.item(statusOK, fmt.Sprintf("数据源类型: postgres（%s）", maskPostgresDSN(cfg.DBPath)))
	} else {
		r.item(statusOK, fmt.Sprintf("数据源类型: sqlite（%s）", cfg.DBPath))
	}

	// 管理后台 IP 白名单（E5）：条数即生效值；nil 条目属异常 cfg（正常路径
	// 不可能出现——config.Load 解析失败的进程根本起不来）。
	if n := len(cfg.AdminCIDRs); n > 0 {
		if bad := countNilCIDRs(cfg.AdminCIDRs); bad > 0 {
			r.item(statusWarn, fmt.Sprintf("管理后台 IP 白名单: 共 %d 条，其中 %d 条无法解析（请检查 CATMAIL_ADMIN_CIDR）", n, bad))
		} else {
			r.item(statusOK, fmt.Sprintf("管理后台 IP 白名单: %d 条（%s）", n, strings.Join(cidrStrings(cfg.AdminCIDRs), ", ")))
		}
	} else {
		r.item(statusOK, "管理后台 IP 白名单: 未配置（不限制管理端来源 IP）")
	}

	if cfg.AdminPass == "admin123" {
		r.item(statusWarn, "管理员口令仍为默认 admin123（CATMAIL_ADMIN_PASS），存在被爆破风险，请立即修改")
	} else {
		r.item(statusOK, "管理员口令: 已自定义（非默认 admin123）")
	}

	switch {
	case cfg.CookieSecure:
		r.item(statusOK, "COOKIE_SECURE: 已开启（会话 Cookie 附加 Secure 属性）")
	case isLoopbackAddr(cfg.Addr):
		r.item(statusOK, "COOKIE_SECURE: 未开启（监听 loopback，本地部署可接受）")
	default:
		r.item(statusWarn, "COOKIE_SECURE 未开启且监听地址非 loopback：会话 Cookie 可能经明文链路泄露，HTTPS 部署应开启 CATMAIL_COOKIE_SECURE=1")
	}

	if len(cfg.TrustedProxies) == 0 {
		r.item(statusWarn, "TRUSTED_PROXIES 未设置：反代部署必须设置（CATMAIL_TRUSTED_PROXIES），否则 X-Forwarded-For 可伪造，管理 IP 白名单与登录限流的 IP 维度失效；直连部署可忽略")
	} else {
		r.item(statusOK, fmt.Sprintf("TRUSTED_PROXIES: %d 条（%s）", len(cfg.TrustedProxies), strings.Join(cfg.TrustedProxies, ", ")))
	}

	// 渠道凭据加密密钥（安全加固三件套，A）：未配置给 WARN（明文兼容现状，但
	// 备份文件/整库即可读出全部渠道凭据）；已配置即 OK（合法性由 config.Load
	// 快速失败保证，此处不再重复校验）。
	if cfg.SecretKey == nil {
		r.item(statusWarn, "CATMAIL_SECRET_KEY 未配置：渠道凭据明文存储（现状兼容），建议配置 base64 编码 32 字节密钥（openssl rand -base64 32）后重启完成存量加密迁移")
	} else {
		r.item(statusOK, "CATMAIL_SECRET_KEY: 已配置（渠道凭据 AES-256-GCM 静态加密）")
	}

	// SMTP TLS 入口（证书可读性 + 策略/监听生效值）：config.Load 已在启动期把
	// 非法组合挡在门外，这里防御手工构造的 cfg（证书配错按 FAIL 呈现，不 panic）。
	if cfg.SMTPTLSCertFile == "" && cfg.SMTPTLSKeyFile == "" {
		r.item(statusOK, "SMTP TLS 证书: 未配置（STARTTLS/隐式 TLS 关闭，明文入口维持现状）")
	} else if err := config.ValidateSMTPTLSCertPair(cfg.SMTPTLSCertFile, cfg.SMTPTLSKeyFile); err != nil {
		r.item(statusFail, fmt.Sprintf("SMTP TLS 证书不可用（STARTTLS/隐式 TLS 无法启用）: %v", err))
	} else {
		r.item(statusOK, fmt.Sprintf("SMTP TLS 证书: 可读且密钥对匹配（cert=%s key=%s）", cfg.SMTPTLSCertFile, cfg.SMTPTLSKeyFile))
	}
	if cfg.SMTPTLSAddr == "" {
		r.item(statusOK, "SMTP 隐式 TLS 监听: 未配置（CATMAIL_SMTP_TLS_ADDR，465 风格关闭）")
	} else {
		r.item(statusOK, fmt.Sprintf("SMTP 隐式 TLS 监听: %s（accept 后立即 TLS 握手）", cfg.SMTPTLSAddr))
	}
	if p, err := config.ParseSMTPTLSPolicy(cfg.SMTPTLSPolicy); err != nil {
		r.item(statusWarn, fmt.Sprintf("SMTP TLS 策略取值无法解析（请检查 CATMAIL_SMTP_TLS_POLICY）: %v", err))
	} else if p == config.SMTPTLSPolicyRequire {
		r.item(statusOK, "SMTP TLS 策略: require（明文监听必须 STARTTLS 升级后才允许 AUTH）")
	} else {
		r.item(statusOK, "SMTP TLS 策略: optional（明文监听广播 STARTTLS 但不强制）")
	}
}

// countNilCIDRs 统计白名单中无法解析的条目（nil 指针）。
func countNilCIDRs(cidrs []*net.IPNet) int {
	n := 0
	for _, c := range cidrs {
		if c == nil {
			n++
		}
	}
	return n
}

// cidrStrings 白名单条目转字符串（nil 条目占位标注，绝不 panic）。
func cidrStrings(cidrs []*net.IPNet) []string {
	out := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		if c == nil {
			out = append(out, "（无法解析）")
			continue
		}
		out = append(out, c.String())
	}
	return out
}

// isLoopbackAddr 判断监听地址是否仅暴露 loopback（127.x/::1/localhost）。
// 主机部分为空（如 ":8080"）意味着绑定全部网卡，按非 loopback 处理（宁严勿宽）。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // 无端口形态，整体按主机处理
	}
	switch {
	case host == "":
		return false
	case strings.EqualFold(host, "localhost"):
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// maskPostgresDSN 打码连接串中的口令后回显；解析失败时不回显原文（含凭据）。
// 手工拼接口令段（url.UserPassword 会把 * 百分号编码为 %2A，可读性差）。
func maskPostgresDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return "postgres://…（含凭据，不回显）"
	}
	masked := u.Scheme + "://" + u.User.Username()
	if _, ok := u.User.Password(); ok {
		masked += ":****"
	}
	masked += "@" + u.Host + u.Path
	if u.RawQuery != "" {
		masked += "?" + u.RawQuery
	}
	return masked
}

// ---------- [3] 数据目录 ----------

// checkDataDir 数据目录自检（仅 SQLite）：目录存在/可创建、可写性（建临时文件
// 即删）、磁盘剩余空间（unix 用 Statfs，其余平台 SKIP 并注明）。PostgreSQL 数据
// 目录由数据库服务自身管理，整段 SKIP。
func checkDataDir(r *report, cfg config.Config) {
	r.section("[3] 数据目录")
	if store.IsPostgresDSN(cfg.DBPath) {
		r.item(statusSkip, "数据目录检查仅适用 SQLite 数据源，PostgreSQL 由数据库服务自身管理，跳过")
		return
	}
	dir := filepath.Dir(cfg.DBPath)
	if _, err := os.Stat(dir); err != nil {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			r.item(statusFail, fmt.Sprintf("数据目录 %s 不存在且无法创建: %v", dir, mkErr))
		} else {
			r.item(statusOK, fmt.Sprintf("数据目录 %s 原不存在，已确认可创建", dir))
		}
	} else {
		r.item(statusOK, "数据目录存在: "+dir)
	}

	// 可写性：真实落一个临时文件再删除，比 unix 权限位判断更可靠（含 ACL/只读挂载）。
	f, err := os.CreateTemp(dir, ".doctor-write-*")
	if err != nil {
		r.item(statusFail, fmt.Sprintf("数据目录不可写（无法创建临时文件）: %v", err))
	} else {
		_ = f.Close()
		_ = os.Remove(f.Name())
		r.item(statusOK, "数据目录可写: "+dir)
	}

	// 磁盘剩余空间：仅报告不设阈值（阈值随部署规模变化大，交给运维判断）。
	if free, err := diskFreeBytes(dir); err != nil {
		if errors.Is(err, errDiskUnsupported) {
			r.item(statusSkip, fmt.Sprintf("磁盘剩余空间: 当前平台（%s/%s）不支持该检查，跳过", runtime.GOOS, runtime.GOARCH))
		} else {
			r.item(statusFail, fmt.Sprintf("磁盘剩余空间获取失败: %v", err))
		}
	} else {
		r.item(statusOK, fmt.Sprintf("磁盘剩余空间: %s（%s 所在文件系统）", formatBytes(free), dir))
	}
}

// ---------- [4] 数据库 ----------

// checkDatabase 数据库自检：连接 Ping；SQLite 专属的 PRAGMA quick_check 与 WAL
// 文件大小（>512MB WARN）；逐表行数概览（两种方言通用）。PostgreSQL 跳过
// SQLite 专属项并注明。
func checkDatabase(r *report, cfg config.Config, db *gorm.DB) {
	r.section("[4] 数据库")
	if db == nil {
		r.item(statusFail, "数据库未打开（原因见启动错误输出），本节仅此一项")
		return
	}
	pg := store.IsPostgres(db)

	if sqlDB, err := db.DB(); err != nil {
		r.item(statusFail, fmt.Sprintf("获取底层连接失败: %v", err))
	} else if err := sqlDB.Ping(); err != nil {
		r.item(statusFail, fmt.Sprintf("数据库连接 Ping 失败: %v", err))
	} else {
		r.item(statusOK, fmt.Sprintf("数据库连接正常（%s）", dialectLabel(cfg, pg)))
	}

	if pg {
		r.item(statusSkip, "PRAGMA quick_check 与 WAL 文件大小为 SQLite 专属检查，PostgreSQL 跳过（完整性可用 pg_amcheck/pg_dump 校验）")
	} else {
		var res string
		if err := db.Raw("PRAGMA quick_check").Scan(&res).Error; err != nil {
			r.item(statusFail, fmt.Sprintf("PRAGMA quick_check 执行失败: %v", err))
		} else if res != "ok" {
			r.item(statusFail, fmt.Sprintf("PRAGMA quick_check 异常（库文件可能损坏）: %s", res))
		} else {
			r.item(statusOK, "PRAGMA quick_check: ok（库结构完整）")
		}
		checkWALSize(r, cfg, db)
	}

	if counts, err := tableCounts(db); err != nil {
		r.item(statusFail, fmt.Sprintf("逐表行数统计失败: %v", err))
	} else {
		r.item(statusOK, "表行数概览: "+counts)
	}
}

// checkWALSize WAL 文件大小检查：>512MB WARN（通常意味着长期未 checkpoint）。
// 文件不存在视为 0（未进 WAL 或已 checkpoint 合并，均属正常）。
func checkWALSize(r *report, cfg config.Config, db *gorm.DB) {
	path, err := store.DBFilePath(db)
	if err != nil || path == "" {
		path = cfg.DBPath // 内存库等取不到真实路径时退回配置值
	}
	var size int64
	if st, err := os.Stat(path + "-wal"); err == nil {
		size = st.Size()
	}
	if size > walWarnBytes {
		r.item(statusWarn, fmt.Sprintf("WAL 文件 %s-wal 为 %s（超过 512MB），建议安排停机 checkpoint 或排查长事务", path, formatBytes(uint64(size))))
	} else {
		r.item(statusOK, fmt.Sprintf("WAL 文件大小: %s（%s-wal）", formatBytes(uint64(size)), path))
	}
}

// tableCounts 逐表统计行数概览（channels/users/api_keys/send_logs/notifications），
// 返回 "channels=1 users=1 …" 形式的单行摘要。
func tableCounts(db *gorm.DB) (string, error) {
	tables := []struct {
		label string
		dest  any
	}{
		{"channels", &model.Channel{}},
		{"users", &model.User{}},
		{"api_keys", &model.ApiKey{}},
		{"send_logs", &model.SendLog{}},
		{"notifications", &model.Notification{}},
	}
	parts := make([]string, 0, len(tables))
	for _, t := range tables {
		var n int64
		if err := db.Model(t.dest).Count(&n).Error; err != nil {
			return "", fmt.Errorf("统计 %s 行数: %w", t.label, err)
		}
		parts = append(parts, fmt.Sprintf("%s=%d", t.label, n))
	}
	return strings.Join(parts, " "), nil
}

// dialectLabel 数据源类型的展示名（与 config 层 IsPostgresDSN 同口径）。
func dialectLabel(cfg config.Config, pg bool) string {
	if pg || store.IsPostgresDSN(cfg.DBPath) {
		return "postgres"
	}
	return "sqlite"
}

// ---------- [5] 渠道配置完整性 ----------

// checkChannels 渠道配置完整性（只读，不出网）：凭据加密状态盘点（安全加固三件套，
// A）+ 逐渠道检查 provider 注册状态与按类型必填字段；from_email 缺失给 WARN（仅影响
// 页面测试发送，不影响 API 发送时的请求级 from）。凭据有效性无法离线验证，末尾提示
// 用页面测试发送。
func checkChannels(r *report, db *gorm.DB) {
	r.section("[5] 渠道配置完整性（只读，不出网）")
	if db == nil {
		r.item(statusSkip, "数据库未打开，渠道检查跳过")
		return
	}
	// 存量迁移完成状态（A）：按原始列值盘点（不经 serializer 解密）。未配置密钥
	// 且库里有密文 → FAIL（服务将拒绝启动，明文兼容模式解不开）；已配置且仍有
	// 明文行 → FAIL（启动迁移未完成即被绕过，正常路径不可达，提示重启执行迁移）。
	st, err := store.CheckChannelEncryption(db)
	if err != nil {
		r.item(statusFail, fmt.Sprintf("渠道凭据加密状态盘点失败: %v", err))
	} else {
		switch {
		case st.CiphertextRows > 0 && !secretcrypt.GlobalKeySet():
			r.item(statusFail, fmt.Sprintf("存在 %d 个含加密凭据的渠道但未配置 CATMAIL_SECRET_KEY（服务将拒绝启动）", st.CiphertextRows))
		case st.PlaintextRows > 0 && secretcrypt.GlobalKeySet():
			r.item(statusFail, fmt.Sprintf("CATMAIL_SECRET_KEY 已配置但仍有 %d 个渠道凭据为明文（存量迁移未完成，请重启服务执行迁移）", st.PlaintextRows))
		case secretcrypt.GlobalKeySet():
			r.item(statusOK, fmt.Sprintf("凭据加密存量迁移: 已完成（%d 个渠道均为密文或无凭据）", st.TotalRows))
		default:
			r.item(statusOK, fmt.Sprintf("凭据加密存量迁移: 未启用（%d 个渠道均为明文/无凭据，CATMAIL_SECRET_KEY 未配置）", st.TotalRows))
		}
	}

	var chans []model.Channel
	if err := db.Order("id").Find(&chans).Error; err != nil {
		r.item(statusFail, fmt.Sprintf("读取渠道列表失败: %v", err))
	} else if len(chans) == 0 {
		r.item(statusOK, "无渠道配置（发送前请先在管理后台添加渠道）")
	} else {
		for i := range chans {
			checkChannel(r, &chans[i])
		}
	}
	fmt.Fprintln(r.out, "  提示: 凭据有效性无法离线验证，请在管理页面对各渠道执行「测试发送」确认。")
}

// checkChannel 单渠道完整性检查：provider 是否注册 → 按类型必填字段（缺失 FAIL）
// → from_email（缺失 WARN：测试发送不可用）。
func checkChannel(r *report, ch *model.Channel) {
	label := fmt.Sprintf("渠道 %q（%s，%s）", ch.Name, ch.Provider, channelStatus(ch.Status))
	if _, ok := provider.Get(ch.Provider); !ok {
		r.item(statusFail, label+" provider 未注册（不支持的渠道类型，请检查渠道数据）")
		return
	}
	var missing []string
	switch ch.Provider {
	case "smtp":
		if ch.SMTPHost == "" {
			missing = append(missing, "smtp_host")
		}
		if ch.SMTPPort <= 0 {
			missing = append(missing, "smtp_port")
		}
	case "mailgun":
		if ch.Domain == "" {
			missing = append(missing, "domain（发送域名）")
		}
	case "aliyun":
		if ch.APISecret == "" {
			missing = append(missing, "APISecret（AccessKeySecret）")
		}
		if ch.FromEmail == "" {
			missing = append(missing, "from_email（发信地址 AccountName）")
		}
	case "mailjet":
		if ch.APIKey == "" {
			missing = append(missing, "APIKey（public key）")
		}
		if ch.APISecret == "" {
			missing = append(missing, "APISecret（private key）")
		}
	case "resend", "brevo", "smtp2go", "elasticemail", "mailtrap":
		if ch.APIKey == "" {
			missing = append(missing, "APIKey")
		}
		// mock 为本地模拟渠道，无需任何必填字段。
	}
	if len(missing) > 0 {
		r.item(statusFail, label+" 缺少必填字段: "+strings.Join(missing, "、"))
		return
	}
	if ch.FromEmail == "" {
		r.item(statusWarn, label+" 未配置 from_email：页面「测试发送」不可用（API 发送可用请求级 from）")
		return
	}
	r.item(statusOK, label+" 配置完整")
}

// channelStatus 渠道状态中文名（model.Channel* 常量映射）。
func channelStatus(s int) string {
	switch s {
	case model.ChannelEnabled:
		return "启用"
	case model.ChannelManualDisabled:
		return "手动停用"
	case model.ChannelAutoDisabled:
		return "自动禁用"
	case model.ChannelCooldown:
		return "冷却中"
	default:
		return fmt.Sprintf("未知(%d)", s)
	}
}

// ---------- [6] 通知与回执 ----------

// checkNotifyReceipt 通知/回执自检：管理员侧失败通知 Webhook（O9，启用才校验）
// 与调用方回执 Webhook（D1，按 Key 显式 opt-in）。校验口径与 api 层一致：
// 须为可解析的 http(s):// 完整地址（含 host）。host 不在已知机器人域名（飞书/
// 钉钉/企业微信/Slack）属合法通用 webhook，不做 WARN。
func checkNotifyReceipt(r *report, db *gorm.DB) {
	r.section("[6] 通知与回执")
	if db == nil {
		r.item(statusSkip, "数据库未打开，通知/回执检查跳过")
		return
	}

	var st model.Setting
	if err := db.Limit(1).Find(&st, 1).Error; err != nil {
		r.item(statusFail, fmt.Sprintf("读取通知设置失败: %v", err))
	} else if !st.NotifyWebhookEnabled {
		r.item(statusOK, "失败通知 Webhook 未启用，跳过")
	} else if u, err := parseWebhookURL(st.NotifyWebhookURL); err != nil {
		r.item(statusFail, fmt.Sprintf("失败通知 Webhook 已启用但地址非法（须为 http(s):// 完整地址）: %q", st.NotifyWebhookURL))
	} else {
		r.item(statusOK, fmt.Sprintf("失败通知 Webhook 已启用: %s（host 非已知机器人域名时按通用 JSON 推送）", u))
	}

	var keys []model.ApiKey
	if err := db.Find(&keys).Error; err != nil {
		r.item(statusFail, fmt.Sprintf("读取 API Key 列表失败: %v", err))
		return
	}
	configured, bad := 0, []string{}
	for _, k := range keys {
		if k.WebhookURL == "" {
			continue // 未 opt-in 回执，不检查
		}
		configured++
		if _, err := parseWebhookURL(k.WebhookURL); err != nil {
			bad = append(bad, k.Name)
		}
	}
	switch {
	case configured == 0:
		r.item(statusOK, "无 API Key 配置回执 Webhook，跳过")
	case len(bad) > 0:
		r.item(statusFail, fmt.Sprintf("%d 个 Key 的回执 webhook_url 非法: %s（须为 http(s):// 完整地址）", len(bad), strings.Join(bad, "、")))
	default:
		r.item(statusOK, fmt.Sprintf("%d 个 Key 配置了回执 Webhook，地址均合法", configured))
	}
}

// parseWebhookURL webhook 地址校验（与 api 层 validateWebhookURL 同口径：允许空，
// 非空必须可解析且为 http(s):// 含 host），成功返回解析后的地址。
func parseWebhookURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("非法 webhook 地址: %q", raw)
	}
	return u.String(), nil
}

// ---------- [7] 端口监听 ----------

// checkPort 端口自检：试探绑定配置地址后立即关闭。若常驻服务已在该端口运行，
// 本项会 FAIL 并提示占用原因（doctor 适用于启动前/独立运行体检）。SMTP 隐式
// TLS 监听地址已配置时同样试探（TLS 监听状态）。
func checkPort(r *report, cfg config.Config) {
	r.section("[7] 端口监听")
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		r.item(statusFail, fmt.Sprintf("端口 %s 无法绑定（若服务已在此端口运行属正常，否则请排查占用）: %v", cfg.Addr, err))
		return
	}
	_ = ln.Close()
	r.item(statusOK, fmt.Sprintf("端口可绑定: %s", cfg.Addr))

	if cfg.SMTPTLSAddr != "" {
		tln, err := net.Listen("tcp", cfg.SMTPTLSAddr)
		if err != nil {
			r.item(statusFail, fmt.Sprintf("SMTP TLS 端口 %s 无法绑定（若服务已在此端口运行属正常，否则请排查占用）: %v", cfg.SMTPTLSAddr, err))
			return
		}
		_ = tln.Close()
		r.item(statusOK, fmt.Sprintf("SMTP TLS 端口可绑定: %s", cfg.SMTPTLSAddr))
	}
}

// ---------- 报告输出器 ----------

// report 自检报告输出器：逐项打印并计数，供末尾汇总与退出码判定。
type report struct {
	out                  io.Writer
	ok, warn, fail, skip int
}

// section 打印小节标题。
func (r *report) section(title string) {
	fmt.Fprintln(r.out, "\n"+title)
}

// item 打印一行检查结果并计数。
func (r *report) item(status, detail string) {
	switch status {
	case statusOK:
		r.ok++
	case statusWarn:
		r.warn++
	case statusFail:
		r.fail++
	case statusSkip:
		r.skip++
	}
	fmt.Fprintf(r.out, "  [%s] %s\n", status, detail)
}

// formatBytes 人类可读字节数（1 KB=1024B，两级以上保留一位小数）。
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
