// CatMail(猫邮) 主程序：config → store 初始化 → api 路由 + SPA 静态资源 → 优雅退出。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"catmail/internal/api"
	"catmail/internal/config"
	"catmail/internal/doctor"
	"catmail/internal/notify"
	"catmail/internal/receipt"
	"catmail/internal/scheduler"
	"catmail/internal/secretcrypt"
	"catmail/internal/smtpserver"
	"catmail/internal/store"
	"catmail/internal/update"
	"catmail/internal/version"
	"catmail/internal/web"
)

func main() {
	// doctor 子命令（E7）：自检后立即退出，不启动 HTTP 服务。
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		runDoctor()
		return
	}
	cfg := config.Load()

	// 渠道凭据加密密钥（安全加固三件套，A）：CATMAIL_SECRET_KEY 已配置时注入全局
	// 密钥（模型读写路径透明加解密），并对存量明文凭据做一次性迁移（事务、幂等）；
	// 未配置 = 明文兼容模式（现状），但库里已有密文时拒绝启动（ErrCiphertextNeedsKey）
	// ——明文模式解不开密文，继续运行只会产出更多不可解数据。非法密钥（base64 解码
	// 失败 / 长度≠32）已在 config.Load 快速失败。
	if cfg.SecretKey != nil {
		secretcrypt.SetGlobalKey(cfg.SecretKey)
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}
	if _, migrated, err := store.EnsureChannelEncryption(db, cfg.SecretKey); err != nil {
		log.Fatalf("init credential encryption: %v", err)
	} else if migrated > 0 {
		log.Printf("credential encryption: 已将 %d 个渠道的明文凭据迁移为密文（AES-256-GCM）", migrated)
	}
	if err := store.EnsureSeed(db, cfg.AdminUser, cfg.AdminPass); err != nil {
		log.Fatalf("seed store: %v", err)
	}

	// 通知发射器（O9）：站内通知中心 + Webhook 双通道。rotator/scheduler 经包级
	// notify.Emit 发射事件（未接线时自动 no-op，接线后按 Setting 开关分流）。
	// Webhook 通道（安全加固三件套，B）经 outbox 落库投递：入队即持久化，投递由
	// scheduler 周期驱动（退避重试/签名见 internal/outbox），无进程内 worker，
	// 退出无需排空（重启后按 outbox 续投）。
	notify.SetDefault(notify.NewNotifier(db))

	// 调用方回执发射器（D1）：send_accepted / send_failed 按 Key 上的
	// webhook_url/webhook_events 推送（显式 opt-in），rotator 经包级
	// receipt.EmitAccepted / EmitFailed 触发（未接线时自动 no-op）。
	// 投递同样走 outbox（重启不丢、指数退避），无进程内 worker。
	receipt.SetDefault(receipt.NewDispatcher(db))

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// 全局中间件（W2.5/O45）：安全响应头（CSP/XFO/nosniff 等，覆盖 NoRoute 静态、
	// API、docs、metrics 的全部响应）与 gzip 响应压缩。必须在路由注册之前 Use——
	// 已注册路由的中间件链在注册时固化，后补不影响存量路由。
	r.Use(gin.Logger(), api.Recovery(), api.SecurityHeaders(), web.Gzip())

	// 可信反代（CATMAIL_TRUSTED_PROXIES，逗号分隔 CIDR）：设置后 gin 才信任这些
	// 链路的 X-Forwarded-For 等头并解析真实客户端 IP。反代部署必须设置——未设置时
	// 保持 gin 默认行为（信任所有直连对端），外网请求可伪造 XFF，使管理 IP 白名单
	// （E5）与登录限流的 IP 维度失效；直连部署无需设置。非法条目已在 config.Load
	// 快速失败，此处错误仅剩引擎态异常，同样直接终止。
	if len(cfg.TrustedProxies) > 0 {
		if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
			log.Fatalf("set trusted proxies: %v", err)
		}
	}

	// /api/*（Cookie 会话）与 /v1/send、/emails（Bearer ApiKey）；
	// CATMAIL_COOKIE_SECURE 开启时会话 Cookie 附加 Secure（P2-9）；
	// CATMAIL_ADMIN_CIDR 配置管理面 IP 白名单（E5）：login、/api/* 管理组与
	// /metrics 生效，/api/health、/v1/send、/emails 豁免。
	// E8 自动更新：CATMAIL_UPDATE_REPO 未配置时 updater 为 nil，两个 /api/update/*
	// 端点保持注册但返回 400（功能关闭，路由契约不变）。
	var updater *update.Checker
	if cfg.UpdateRepo != "" {
		updater = update.New(cfg.UpdateRepo, cfg.UpdatePrerelease, version.Version)
	}
	api.Register(r, db,
		api.WithCookieSecure(cfg.CookieSecure),
		api.WithAdminAllowlist(cfg.AdminCIDRs),
		api.WithUpdateChecker(updater))
	// 其余路径：嵌入 SPA（assets 直出，index.html fallback）
	registerStatic(r)
	// 定时任务：每小时冷却释放、每日日志清理（SPEC §7）
	cronScheduler := scheduler.Start(db)

	srv := &http.Server{Addr: cfg.Addr, Handler: r}
	go func() {
		log.Printf("catmail listening on %s (db=%s)", cfg.Addr, cfg.DBPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	// E8 启动检查（CATMAIL_UPDATE_CHECK_ON_START=true 且已配置 REPO 时）：服务
	// 已开始对外服务后异步检查一次（结果进程内缓存 1h，/api/update/check 直接复用）；
	// 发现新版本经 notify.Emit 双通道（站内 + Webhook）广播 update_available 事件。
	// 自带超时与错误兜底，失败只记日志，绝不影响常驻服务。
	if updater != nil && cfg.UpdateCheckOnStart {
		go updater.CheckAndNotify()
	}

	// E1 SMTP 入口：CATMAIL_SMTP_ADDR / CATMAIL_SMTP_TLS_ADDR 未设置（默认空）=
	// 功能关闭，不监听；设置（如 ":2525"/":465"）即启用——只支持 SMTP 的本地应用
	// 把 CatMail 当普通 SMTP 服务器用，DATA 收完后经 rotator 走渠道轮换（防开放
	// 中继策略见包注释）。证书（CATMAIL_SMTP_CERT_FILE/KEY_FILE）配置后明文监听
	// 广播 STARTTLS、TLS 监听做隐式 TLS（require 策略见 config 注释）；监听/证书
	// 失败视为致命（端口被占等），对齐 HTTP listen 的处理姿态（config.Load 已把
	// 证书不可用挡在启动前，此处失败属引擎态异常）。
	var smtpSrv *smtpserver.Server
	if cfg.SMTPAddr != "" || cfg.SMTPTLSAddr != "" {
		smtpSrv = smtpserver.New(db, cfg.SMTPAddr, int64(cfg.SMTPMaxSizeMB)<<20)
		if cfg.SMTPTLSCertFile != "" {
			tlsCfg, err := smtpserver.NewTLSConfig(cfg.SMTPTLSCertFile, cfg.SMTPTLSKeyFile)
			if err != nil {
				log.Fatalf("smtp tls: %v", err)
			}
			smtpSrv = smtpSrv.WithTLS(cfg.SMTPTLSAddr, tlsCfg, cfg.SMTPTLSPolicy)
		}
		if err := smtpSrv.Start(); err != nil {
			log.Fatalf("smtp listen: %v", err)
		}
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	// SMTP 入口关停（E1）：放在 HTTP 之后、scheduler 之前——HTTP 先停止新请求，
	// SMTP 随后关闭 listener 并给在途会话（含 rotator 平台请求与 SendLog 落库）
	// 有限等待，之后才停定时任务与排空通知/回执队列，保证退出前发送日志尽量
	// 完整落库。未启用（CATMAIL_SMTP_ADDR 为空）时为 nil，跳过。
	if smtpSrv != nil {
		smtpSrv.Stop()
	}
	scheduler.Stop(cronScheduler)
	// 关闭数据库：SQLite 最后一个连接关闭时自动 checkpoint WAL，
	// 避免 -wal/-shm 残留未合并（正常退出路径统一收口）。
	if sqlDB, err := db.DB(); err != nil {
		log.Printf("close db: %v", err)
	} else if err := sqlDB.Close(); err != nil {
		log.Printf("close db: %v", err)
	}
}

// runDoctor `catmail doctor` 自检子命令（E7）：重新加载配置并打开数据库（与常驻
// 服务同一入口，AutoMigrate 幂等补列），执行只读体检后以检查结果为退出码
// （任一 FAIL → 1）。数据库打开失败不终止：doctor 仍输出环境/配置/数据目录等
// 检查项，数据库相关项按 FAIL 呈现，打开原因写到 stderr。
func runDoctor() {
	cfg := config.Load()
	// 加密密钥注入（只读体检不迁移）：已配置时渠道凭据检查读到的是解密后的明文，
	// 加密状态项由 doctor 自行按原始列值盘点呈现（见 doctor.checkCredentialEncryption）。
	if cfg.SecretKey != nil {
		secretcrypt.SetGlobalKey(cfg.SecretKey)
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "catmail doctor: 打开数据库失败: %v\n", err)
	}
	code := doctor.RunVersion(cfg, db, os.Stdout, version.Version)
	if db != nil {
		if sqlDB, cerr := db.DB(); cerr == nil {
			_ = sqlDB.Close()
		}
	}
	os.Exit(code)
}

// registerStatic 挂载 go:embed 的 SPA 静态资源：命中文件直出，其余回退 index.html；
// /api、/v1、/emails（含各自子路径）下的未知路径返回统一 404 错误体（SPEC §7，
// P2-12：/emails/* 子路径不落 SPA fallback，保持与 /v1 同构的 JSON 404）。
// 缓存策略（W2.5/O45，须在 ServeFileFS 之前设置）：assets/ 下带内容哈希的产物
// 一年 immutable 长缓存（发版换哈希，绝不脏读）；index.html（含直接请求）与 SPA
// fallback 恒 no-cache（每次协商校验，保证发版后立取新入口）；API 响应不在此处，
// 缓存头不动（会话接口本就动态）。
func registerStatic(r *gin.Engine) {
	staticFS := web.FS()
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if p == "/api" || strings.HasPrefix(p, "/api/") ||
			p == "/v1" || strings.HasPrefix(p, "/v1/") ||
			p == "/emails" || strings.HasPrefix(p, "/emails/") ||
			// /webhooks/*（送达事件入站端点前缀）：未知路径同样统一 JSON 404，
			// 不落 SPA fallback（入站端点是机器调用面，与 /v1、/emails 同口径）
			p == "/webhooks" || strings.HasPrefix(p, "/webhooks/") {
			api.NotFound(c)
			return
		}
		name := strings.TrimPrefix(p, "/")
		if name != "" {
			if f, err := staticFS.Open(name); err == nil {
				st, serr := f.Stat()
				_ = f.Close()
				if serr == nil && !st.IsDir() {
					switch {
					case strings.HasPrefix(name, "assets/"):
						c.Header("Cache-Control", "public, max-age=31536000, immutable")
					case name == "index.html":
						c.Header("Cache-Control", "no-cache")
					}
					http.ServeFileFS(c.Writer, c.Request, staticFS, name)
					return
				}
			}
		}
		c.Header("Cache-Control", "no-cache")
		http.ServeFileFS(c.Writer, c.Request, staticFS, "index.html")
	})
}
