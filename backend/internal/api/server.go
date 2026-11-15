// server.go API 服务装配：Server 定义、统一错误体与全部路由注册（SPEC §2/§5）。
package api

import (
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/docs"
	"catmail/internal/rotator"
	"catmail/internal/update"
)

// dayLayout / monthLayout 渠道额度 anchor 的日期格式。
const (
	dayLayout   = "2006-01-02"
	monthLayout = "2006-01"
)

// 统一错误体 error.type 常量（SPEC §2）。
const (
	TypeInvalidRequest = "invalid_request"
	TypeUnauthorized   = "unauthorized"
	TypeForbidden      = "forbidden"    // 403 来源不在管理 IP 白名单（E5/B4 /metrics）
	TypeRateLimited    = "rate_limited" // 429 调用方限速（B5，B5 遗留收纳：原寄 middleware.go）
	TypeQuotaExhausted = "quota_exhausted"
	TypeProviderError  = "provider_error"
	TypeServerError    = "server_error"
	// 邮件模板扩展错误码（邮件模板功能包，400）：错误体风格与 SPEC §2 一致。
	TypeUnknownTemplate = "unknown_template" // template_id 不存在
	TypeBodyConflict    = "body_conflict"    // template_id 与请求自带 html/text 冲突
)

// Server API 服务：持有 DB、缓存的会话签名密钥、登录限流器与 Cookie 配置。
type Server struct {
	db           *gorm.DB
	secretMu     sync.RWMutex
	secret       string        // Setting.SessionSecret 缓存
	cookieSecure bool          // 会话 Cookie 是否附加 Secure（CATMAIL_COOKIE_SECURE，P2-9）
	limiter      *loginLimiter // 登录失败限流（内存态，P2-1）
	// adminCIDRs 管理面 IP 白名单（E5，CATMAIL_ADMIN_CIDR 解析结果，cmd 层经
	// WithAdminAllowlist 注入）；空 = 未配置不限制（默认现状），生效范围见 Register。
	adminCIDRs []*net.IPNet
	// weakPwCache 弱口令比对结果缓存（bcrypt 哈希 → bool，见 handler_settings.go）：
	// bcrypt 比对较贵，按哈希缓存进程内结果，改密后哈希变化自然失效。
	weakPwCache sync.Map
	// updater 自动更新器（E8，cmd 层经 WithUpdateChecker 注入；CATMAIL_UPDATE_REPO
	// 未配置时为 nil，两个 /api/update/* 端点保持注册但返回 400=功能关闭）。
	updater *update.Checker
}

// Option 服务装配可选项（cmd 层按配置注入）。
type Option func(*Server)

// WithCookieSecure 会话 Cookie 附加 Secure 属性（HTTPS/反代 TLS 部署时开启，P2-9）。
func WithCookieSecure(v bool) Option {
	return func(s *Server) { s.cookieSecure = v }
}

// WithAdminAllowlist 注入管理面 IP 白名单（E5）：cidrs 为 config.ParseAdminCIDRs
// 的解析结果（CATMAIL_ADMIN_CIDR），空/未调用 = 不限制（默认现状）。
func WithAdminAllowlist(cidrs []*net.IPNet) Option {
	return func(s *Server) { s.adminCIDRs = cidrs }
}

// WithUpdateChecker 注入自动更新器（E8）：CATMAIL_UPDATE_REPO 已配置时由 cmd
// 层构造传入；传 nil / 未调用 = 功能关闭（端点仍注册，返回 400，路由契约不变）。
func WithUpdateChecker(u *update.Checker) Option {
	return func(s *Server) { s.updater = u }
}

// Register 挂载全部路由：/v1/send 与 /emails（Bearer ApiKey）、
// /api/*（Cookie 会话，除登录外全部拦截）、/metrics（Prometheus 抓取）。
// 静态 SPA 由 cmd 层另行挂载。
func Register(r gin.IRouter, db *gorm.DB, opts ...Option) {
	s := &Server{db: db, limiter: newLoginLimiter()}
	for _, o := range opts {
		o(s)
	}
	// 管理面 IP 白名单（E5）：空列表 = 未配置不限制（中间件自身直通），恒挂载即可。
	// 生效范围（宁窄勿宽）仅限管理面：/api/auth/login（未登录也拦，防外网爆破）、
	// 全部 requireSession 的 /api/* 与 /metrics（抓取器按网段放行）；
	// 明确不适用于 /api/health、/v1/send、/emails（探针/调用方可来自任意来源）。
	mw := AdminIPAllowlist(s.adminCIDRs)

	// 健康检查：公开无鉴权（集成冒烟契约 {ok:true}）
	r.GET("/api/health", s.health)

	// API 文档（B2）：单文件 HTML 文档页与 OpenAPI 规范原文。公开无鉴权——
	// 文档内容不含任何真实凭据/敏感配置（示例均为脱敏占位值），故不进
	// requireSession、也不受管理 IP 白名单约束；显式挂载先于 cmd 层 SPA
	// NoRoute 兜底生效，不会被 index.html fallback 吞掉。
	r.GET("/docs", gin.WrapF(docs.Page))
	r.GET("/docs/openapi.yaml", gin.WrapF(docs.SpecYAML))

	// Prometheus 指标（B4）：不挂 requireSession（抓取器不会登录），仅白名单——
	// 未配置 CATMAIL_ADMIN_CIDR 时全网可达，生产建议与白名单一同配置收紧。
	r.GET("/metrics", mw, s.metrics)

	// 对外发送入口：/emails 与 /v1/send 完全同构（SPEC §2 Resend SDK 兼容）；
	// 请求体放宽至 30MB 以容纳 base64 附件，超限统一 400
	r.POST("/v1/send", bodyLimit(maxSendBodyBytes), s.send)
	r.POST("/emails", bodyLimit(maxSendBodyBytes), s.send)

	// 送达事件入站 webhook（送达链路三件套）：公开无会话鉴权——渠道定位与校验
	// 走 URL 中每渠道独立 token（与可选平台签名双重校验，见 handler_webhook.go），
	// 不受管理 IP 白名单约束（事件平台可来自任意来源），与 /v1/send 豁免口径一致。
	// 请求体上限 1MB（事件报文均为小体量 JSON/form）。
	r.POST("/webhooks/mail-events/:token", bodyLimit(maxAdminBodyBytes), s.ingestMailEvent)

	// 登录放行；其余 /api/* 全部要求会话（SPEC §5），请求体上限 1MB；
	// login 与管理组均先过白名单（E5），白名单外来源未登录即 403。
	// login/totp 为登录第二步（凭 t| 短票据而非会话），与 login 同层级放行。
	r.POST("/api/auth/login", mw, bodyLimit(maxAdminBodyBytes), s.login)
	r.POST("/api/auth/login/totp", mw, bodyLimit(maxAdminBodyBytes), s.loginTotp)
	api := r.Group("/api", mw, s.requireSession, bodyLimit(maxAdminBodyBytes))

	api.POST("/auth/logout", s.logout)
	api.GET("/auth/me", s.me)
	api.PUT("/auth/password", s.changeOwnPassword)

	// 会话管理（安全加固三件套，C）：本人活跃会话列表、踢指定会话（不能踢当前）、
	// 下线其余全部。吊销只写注册表（revoked_at），即时生效、不影响 API Key。
	api.GET("/sessions", s.listSessions)
	api.DELETE("/sessions/:id", s.deleteSession)
	api.POST("/sessions/revoke-others", s.revokeOtherSessions)

	// 两步验证绑定/解绑（C4，均已登录）：setup/enable/disable
	api.POST("/auth/totp/setup", s.totpSetup)
	api.POST("/auth/totp/enable", s.totpEnable)
	api.POST("/auth/totp/disable", s.totpDisable)

	api.GET("/users", s.listUsers)
	api.POST("/users", s.createUser)
	api.PUT("/users/:id/password", s.resetUserPassword)
	api.DELETE("/users/:id", s.deleteUser)

	api.GET("/channels", s.listChannels)
	api.POST("/channels", s.createChannel)
	api.PUT("/channels/:id", s.updateChannel)
	api.DELETE("/channels/:id", s.deleteChannel)
	api.POST("/channels/:id/status", s.channelStatus)
	api.POST("/channels/:id/test", s.testChannel)
	api.POST("/channels/:id/duplicate", s.duplicateChannel)

	// 邮件模板管理（邮件模板功能包）：列表/详情/新建/编辑/删除。发送入口经
	// template_id + variables 引用模板（渲染 hook 见 send_template.go 与 SMTP
	// 入口 template.go）；审计动作 template_create/update/delete。
	api.GET("/templates", s.listTemplates)
	api.POST("/templates", s.createTemplate)
	api.GET("/templates/:id", s.getTemplate)
	api.PUT("/templates/:id", s.updateTemplate)
	api.DELETE("/templates/:id", s.deleteTemplate)
	// 送达事件入站 webhook 的渠道 token 轮换（送达链路三件套）：挂 /api 会话组内，
	// 与组内其余渠道管理动作同口径（白名单 + 会话）。
	api.POST("/channels/:id/webhook-token", s.rotateChannelWebhookToken)

	api.GET("/logs", s.listLogs)
	api.GET("/logs/export", s.exportLogs) // 静态段 export，与 :id 参数路由并存（gin 静态优先）
	api.GET("/logs/:id", s.getLog)
	api.POST("/logs/:id/resend", s.resendLog)

	// 送达事件（送达链路三件套）：管理端查询（入站摄取在 /webhooks 公开端点）
	api.GET("/mail-events", s.listMailEvents)
	api.GET("/mail-events/:id", s.getMailEvent)

	// 抑制列表（送达链路三件套）：管理端查询/手动添加/解除
	api.GET("/suppressions", s.listSuppressions)
	api.POST("/suppressions", s.createSuppression)
	api.DELETE("/suppressions/:id", s.deleteSuppression)

	api.GET("/keys", s.listKeys)
	api.POST("/keys", s.createKey)
	api.PUT("/keys/:id", s.updateKey)
	api.DELETE("/keys/:id", s.deleteKey)
	// D1 回执连通性测试：属管理面编辑弹窗的实测动作，挂在 /api 组内即同时受
	// 管理 IP 白名单（mw）与会话鉴权保护——与组内其余 /api/* 口径一致，符合预期。
	api.POST("/keys/:id/webhook/test", s.testKeyWebhook)
	// 回执签名密钥轮换（安全加固三件套，B）：创建 Key 已自动生成，本端点重置。
	api.POST("/keys/:id/webhook-secret", s.resetKeyWebhookSecret)

	api.GET("/stats/overview", s.statsOverview)
	api.GET("/stats/daily", s.statsDaily)

	api.GET("/notifications", s.listNotifications)
	api.GET("/notifications/unread_count", s.unreadCount)
	api.POST("/notifications/read", s.markNotificationsRead)

	api.GET("/settings", s.getSettings)
	api.PUT("/settings", s.putSettings)
	api.POST("/settings/notify/test", s.testNotify)
	// 通知 Webhook 签名密钥生成/轮换（安全加固三件套，B）：PUT /api/settings 忽略
	// notify_webhook_secret 字段，变更仅经本端点（防整表单保存误清）。
	api.POST("/settings/notify/webhook-secret", s.rotateNotifyWebhookSecret)

	// Webhook 投递记录查询（安全加固三件套，B）：outbox 投递的排查端点
	api.GET("/webhook-deliveries", s.listWebhookDeliveries)

	// E4 管理操作审计：查询端点（写入在各管理 handler 成功路径插桩，见 audit.go）
	api.GET("/audit", s.listAuditLogs)

	// E8 自动更新：检查（结果进程内缓存 1h）与落盘更新。挂在 /api 会话组内即同时
	// 受管理 IP 白名单（mw）与会话鉴权保护——apply 等于「替换自身二进制」，是
	// 管理面最高危操作之一，与组内其余 /api/* 口径一致。
	api.GET("/update/check", s.updateCheck)
	api.POST("/update/apply", s.updateApply)

	// A2 数据管理：整库备份与发送日志手动清理（会话组内，审计见 handler_backup.go）
	api.GET("/backups", s.listBackups)
	api.POST("/backups", s.createBackup)
	api.GET("/backups/:name/download", s.downloadBackup)
	api.DELETE("/backups/:name", s.deleteBackup)
	api.POST("/logs/purge", s.purgeLogs) // 静态段 purge，与 :id 参数路由并存（gin 静态优先）
}

// health GET /api/health：公开健康探针，不校验会话。
func (s *Server) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// NotFound 为 /api、/v1、/emails 下的未知路径返回统一 404 错误体，
// 供 cmd 层 NoRoute 使用（静态资源不走此分支）。
func NotFound(c *gin.Context) {
	writeErr(c, http.StatusNotFound, TypeInvalidRequest, "接口不存在")
}

// ---------- 统一错误体与通用助手 ----------

// writeErr 输出统一错误体 {error:{type,message}}（SPEC §2）。
func writeErr(c *gin.Context, status int, typ, msg string) {
	c.JSON(status, gin.H{"error": gin.H{"type": typ, "message": msg}})
}

// writeErrCode 输出带机器可读错误码的统一错误体 {error:{type,code,message}}：
// code 为加法扩展（送达链路三件套的 recipient_suppressed），不破坏既有调用方
// 对 type/message 字段的解析。
func writeErrCode(c *gin.Context, status int, typ, code, msg string) {
	c.JSON(status, gin.H{"error": gin.H{"type": typ, "code": code, "message": msg}})
}

// writeRotatorErr 透传 rotator 统一错误（/v1/send、/emails、日志重发共用）：
// Err.Code 非空时附加 code 字段（见 writeErrCode），否则保持原错误体形态。
func writeRotatorErr(c *gin.Context, err *rotator.Err) {
	if err == nil {
		return
	}
	if err.Code != "" {
		writeErrCode(c, err.HTTPStatus, err.Type, err.Code, err.Message)
		return
	}
	writeErr(c, err.HTTPStatus, err.Type, err.Message)
}

func badRequest(c *gin.Context, msg string) {
	writeErr(c, http.StatusBadRequest, TypeInvalidRequest, msg)
}

func unauthorized(c *gin.Context, msg string) {
	writeErr(c, http.StatusUnauthorized, TypeUnauthorized, msg)
}

func notFound(c *gin.Context, msg string) {
	writeErr(c, http.StatusNotFound, TypeInvalidRequest, msg)
}

// serverError 内部错误：详情入服务端日志，客户端仅收到通用文案（不泄露堆栈/路径）。
func serverError(c *gin.Context, err error) {
	log.Printf("api: %s %s: %v", c.Request.Method, c.Request.URL.Path, err)
	writeErr(c, http.StatusInternalServerError, TypeServerError, "服务器内部错误")
}

// bindJSON 解析 JSON 请求体，失败时输出 400 并返回 false；
// 请求体超过 bodyLimit 时返回专用文案；其余解析错误统一文案（P2-11：不回显
// 底层错误细节，避免泄露内部字段名/类型信息）。
func bindJSON(c *gin.Context, obj any) bool {
	if err := c.ShouldBindJSON(obj); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) || strings.Contains(err.Error(), "request body too large") {
			badRequest(c, "请求体过大")
			return false
		}
		badRequest(c, "请求体格式错误")
		return false
	}
	return true
}

// parseID 解析路径参数 :id，非法时输出 400 并返回 false。
func parseID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		badRequest(c, "无效的资源 ID")
		return 0, false
	}
	return uint(id), true
}

// midnight 本地时区当日零点（统计口径）。
func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// escapeLike 转义 LIKE 通配符，配合 ESCAPE '\' 使用，防止关键词注入通配符。
func escapeLike(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}
