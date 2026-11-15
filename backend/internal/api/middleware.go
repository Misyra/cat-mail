// middleware.go 会话鉴权中间件与 panic 恢复（SPEC §5/§8）。
package api

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

const (
	sessionCookieName = "catmail_session"  // Cookie 名（SPEC §5）
	sessionTTL        = 7 * 24 * time.Hour // 会话有效期 7 天
	ctxUsername       = "username"         // gin.Context 用户名键
	ctxUserID         = "userID"           // gin.Context 用户 ID 键（/api/auth/me 返回 id 用）
	ctxTotpEnabled    = "totpEnabled"      // gin.Context 两步验证启用状态键（C4，/api/auth/me 返回用）
	ctxSessionID      = "sessionID"        // gin.Context 会话注册表行 ID 键（安全加固三件套，C）

	// sessionTokenV3 会话 token 版本前缀（安全加固三件套，C）：v3 格式
	// "v3|exp|username|user_epoch|session_id|sig" 在 v2 结构（O10）上追加
	// session_id 段，与服务端会话注册表（sessions 表）联动——踢出/登出/密码重置
	// 联动吊销经注册表即时生效。版本前缀使全部存量 v2/更早格式 token 天然失效
	// （对齐 O10 先例：升级后重新登录一次即可，零数据迁移）。
	sessionTokenV3 = model.SessionTokenV3

	// lastSeenJitter 会话惰性刷新节流：last_seen_at 距上次刷新超过该间隔才落库
	// 一次（管理页「最后活跃」分钟级精度足够，避免每请求一写放大 SQLite 写压）。
	lastSeenJitter = time.Minute
)

// 请求体大小上限（SPEC §8 防超长输入）：发送入口放宽以容纳 base64 附件，
// 管理 API 仅小型 JSON。
const (
	maxSendBodyBytes  int64 = 30 << 20 // 30MB
	maxAdminBodyBytes int64 = 1 << 20  // 1MB
)

// bodyLimit 限制请求体大小：超限时读取方返回 *http.MaxBytesError，
// 由 bindJSON 统一转为 400「请求体过大」。
func bodyLimit(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
		}
		c.Next()
	}
}

// requireSession 会话鉴权：校验 Cookie 签名与有效期、按用户纪元比对、确认用户
// 仍存在（删号即 401，防已删账号会话复活），并查服务端会话注册表（安全加固
// 三件套，C）——v3 token 携带 session_id，注册表行已吊销（踢出/登出/密码重置
// 联动）或已过期即 401，使「踢会话」成为服务端可执行动作；
// 失败一律 401 统一错误体（除 POST /api/auth/login 外的全部 /api/*）。
// 失败路径必须 c.Abort()：否则 gin 会继续执行后续 handler，造成 401 之后
// 仍输出真实数据（双重响应泄漏）。
func (s *Server) requireSession(c *gin.Context) {
	token, err := c.Cookie(sessionCookieName)
	if err != nil || token == "" {
		unauthorized(c, "未登录或会话缺失")
		c.Abort()
		return
	}
	username, epoch, sessionID, ok := s.verifySession(token)
	if !ok {
		unauthorized(c, "会话无效或已过期")
		c.Abort()
		return
	}
	var u model.User
	// totp_enabled 一并载入（C4）：仅布尔状态供 /api/auth/me 返回，
	// secret/恢复码不进任何 GET 响应
	if err := s.db.Select("id", "session_epoch", "totp_enabled").Where("username = ?", username).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			unauthorized(c, "账号不存在或已被删除")
			c.Abort()
			return
		}
		serverError(c, err)
		c.Abort()
		return
	}
	// 按用户纪元比对（O10）：本人改密/被重置密码成功后 User.SessionEpoch +1，
	// 该用户携带旧纪元的全部会话（其他浏览器/标签）在此拒绝；当前会话由
	// changeOwnPassword 重签 cookie 保持登录
	if u.SessionEpoch != epoch {
		unauthorized(c, "会话无效或已过期")
		c.Abort()
		return
	}
	// 会话注册表校验（安全加固三件套，C）：v2/更早格式 token 无 session_id 段，
	// 在 verifySession 已拒绝；此处对 v3 token 查行——不存在/属主不符/已吊销/
	// 已过期一律 401。密码重置联动吊销、本人踢会话在此即时生效。
	var sess model.Session
	if err := s.db.First(&sess, "id = ?", sessionID).Error; err != nil {
		unauthorized(c, "会话无效或已过期")
		c.Abort()
		return
	}
	if sess.Username != username || sess.RevokedAt != nil || time.Now().After(sess.ExpiresAt) {
		unauthorized(c, "会话无效或已过期")
		c.Abort()
		return
	}
	// 惰性刷新 last_seen（节流 lastSeenJitter，条件 UPDATE 天然幂等去重）
	if time.Since(sess.LastSeenAt) >= lastSeenJitter {
		if err := s.db.Model(&model.Session{}).
			Where("id = ? AND last_seen_at < ?", sessionID, time.Now().Add(-lastSeenJitter)).
			Update("last_seen_at", time.Now()).Error; err != nil {
			log.Printf("api: refresh session last_seen failed (session=%s): %v", sessionID, err)
		}
	}
	c.Set(ctxUsername, username)
	c.Set(ctxUserID, u.ID)
	c.Set(ctxTotpEnabled, u.TOTPEnabled)
	c.Set(ctxSessionID, sessionID)
	c.Next()
}

// verifySession 校验 v3 会话 token "v3|exp|username|user_epoch|session_id|sig" 的
// HMAC-SHA1 签名与有效期，返回其中的用户名、签发时的按用户纪元（与
// User.SessionEpoch 的比对由 requireSession 载入用户后进行）与会话注册表行 ID
// （注册表校验由 requireSession 进行）。签名覆盖含版本前缀的完整载荷；旧格式
// token（无 v3 前缀，含 v2 的 "v2|exp|username|user_epoch|sig" 与更早的全局纪元
// 格式）一律无效（安全加固三件套，C 对齐 O10 版本前缀先例）。
func (s *Server) verifySession(token string) (string, int, string, bool) {
	sigIdx := strings.LastIndex(token, "|")
	// 双向隔离（票据侧，C4）：TOTP 登录第二步的 t| 短票据不是会话，即使签名
	// 合法也不得当会话用——票据只证明「第一步密码已验通过」，有效期与能力均
	// 远小于会话（显式拒绝使口径明确、可测试锁定）。
	if strings.HasPrefix(token, totpTicketPrefix+"|") {
		return "", 0, "", false
	}
	if !strings.HasPrefix(token, sessionTokenV3+"|") || sigIdx < 0 {
		return "", 0, "", false // 非 v3 格式：v2 及更早版本 token 或畸形 token，直接拒绝
	}
	payload, sig := token[:sigIdx], token[sigIdx+1:]
	secret, ok := s.sessionSecret()
	if !ok {
		return "", 0, "", false
	}
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(payload))
	expect := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expect)) != 1 {
		return "", 0, "", false
	}
	// payload = "v3|exp|username|user_epoch|session_id"（用户名可含 "|"，按段还原；
	// session_id 由 randomHex 生成不含 "|"，取末段安全）
	parts := strings.Split(payload, "|")
	if len(parts) < 5 {
		return "", 0, "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", 0, "", false
	}
	sessionID := parts[len(parts)-1]
	userEpoch, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil || sessionID == "" {
		return "", 0, "", false
	}
	username := strings.Join(parts[2:len(parts)-2], "|")
	if username == "" {
		return "", 0, "", false
	}
	return username, userEpoch, sessionID, true
}

// signSession 生成 v3 会话 token "v3|exp|username|user_epoch|session_id|sig"
// （exp 为过期时间戳秒，userEpoch 为签发时该用户的 User.SessionEpoch，sessionID
// 为服务端会话注册表行 ID——签发方须已写入注册表，见 handler_sessions.createSession）。
func signSession(secret, username string, userEpoch int, sessionID string, now time.Time) string {
	payload := fmt.Sprintf("%s|%d|%s|%d|%s", sessionTokenV3, now.Add(sessionTTL).Unix(), username, userEpoch, sessionID)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "|" + hex.EncodeToString(mac.Sum(nil))
}

// sessionSecret 读取（并缓存）Setting.SessionSecret；Secret 仅首启生成、运行期不变。
// 空密钥一律拒绝（P2-10）：空密钥的 HMAC 任何人可伪造，等于无签名防护。
func (s *Server) sessionSecret() (string, bool) {
	s.secretMu.RLock()
	if s.secret != "" {
		v := s.secret
		s.secretMu.RUnlock()
		return v, true
	}
	s.secretMu.RUnlock()

	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	if s.secret != "" {
		return s.secret, true
	}
	var st model.Setting
	if err := s.db.First(&st, 1).Error; err != nil {
		log.Printf("api: load session secret: %v", err)
		return "", false
	}
	if st.SessionSecret == "" {
		// 不缓存空值：等待 handler_settings 补建密钥后自动恢复
		log.Printf("api: session secret is empty, refusing to sign/verify sessions")
		return "", false
	}
	s.secret = st.SessionSecret
	return s.secret, true
}

// setSessionCookie 写入会话 Cookie：HttpOnly + SameSite=Lax + Path=/（SPEC §8）；
// CATMAIL_COOKIE_SECURE 开启时附加 Secure（HTTPS 部署，P2-9）。
func (s *Server) setSessionCookie(c *gin.Context, token string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieName, token, int(sessionTTL.Seconds()), "/", "", s.cookieSecure, true)
}

// clearSessionCookie 清除会话 Cookie（属性与写入时保持一致）。
func (s *Server) clearSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieName, "", -1, "/", "", s.cookieSecure, true)
}

// Recovery panic 恢复：对客户端统一返回 500 错误体；堆栈只进服务端日志（不泄露）。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("api: panic on %s %s: %v\n%s",
					c.Request.Method, c.Request.URL.Path, rec, debug.Stack())
				if c.Writer.Written() {
					c.AbortWithStatus(http.StatusInternalServerError)
					return
				}
				writeErr(c, http.StatusInternalServerError, TypeServerError, "服务器内部错误")
			}
		}()
		c.Next()
	}
}

// contentSecurityPolicy 内容安全策略（W2.5/O45）。逐条说明：
//   - `script-src 'unsafe-inline'`：index.html 首屏防闪烁的内联主题脚本所需；
//     去 inline 的 hash/外置方案单独一轮再做；
//   - `img-src https:` 与 `style-src https:`：日志页邮件 HTML 预览（srcdoc iframe
//     继承父页 CSP）不破的前提——邮件自带远程图片与内联/远程样式必须放行；
//   - `frame-ancestors 'none'` 与 X-Frame-Options: DENY 双保险防点击劫持；
//   - `object-src 'none'` / `base-uri 'self'` / `form-action 'self'`：封死插件、
//     <base> 劫持与表单外提交等旁路。
const contentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline' https:; img-src 'self' data: https:; " +
	"font-src 'self' data:; connect-src 'self'; frame-src 'self' data: blob:; " +
	"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// SecurityHeaders 全局安全响应头中间件（W2.5/O45）：作用于全部响应（含 NoRoute
// 静态/SPA fallback、API、docs、metrics）。经 r.Use 在路由注册之前挂载。
//   - nosniff：禁止浏览器 MIME 嗅探，防误判执行；
//   - X-Frame-Options: DENY + CSP frame-ancestors 'none'：双保险禁 iframe 嵌套
//     （点击劫持）；
//   - Referrer-Policy: same-origin：站内跳转才带 Referer，外链与跨源请求不带，
//     避免后台 URL（含查询参数）泄露给第三方；
//   - Permissions-Policy：显式关闭摄像头/麦克风/地理位置等强能力；
//   - X-Robots-Tag：管理后台不进搜索引擎索引（与 index.html 的 meta robots 呼应）；
//   - CSP 见 contentSecurityPolicy。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		c.Next()
	}
}

// AdminIPAllowlist 构造管理后台 IP 白名单中间件（导出构造函数，E5）：
// allowed 为已解析 CIDR 列表（config.Load().AdminCIDRs），空列表 = 不限制。
func AdminIPAllowlist(allowed []*net.IPNet) gin.HandlerFunc {
	return adminIPAllowlist(allowed)
}

// adminIPAllowlist 管理后台 IP 白名单（E5）：来源 IP 命中任一 CIDR 才放行，
// 否则 403 统一错误体并 Abort（Abort 必须显式，否则 gin 继续执行后续 handler）。
// 生效范围（宁窄勿宽）仅限管理面：/api/auth/login（未登录也拦，防外网爆破）与
// 全部 requireSession 的 /api/*；明确不适用于 /api/health、/v1/send、/emails
// （调用方/健康探针可来自任意来源），接线时按路由挂载点控制。
//
// 来源取 c.ClientIP()：遵循 gin 现有 TrustedProxies 语义，本中间件不改全局信任
// 配置。注意：反代部署必须正确设置 gin 信任（CATMAIL_TRUSTED_PROXIES，cmd 层
// 调 SetTrustedProxies）才能取到真实来源 IP；若引擎信任了不可信链路，XFF 可被
// 伪造，白名单形同虚设。
// ClientIP 无法判定（空）时按不在白名单处理（宁窄勿宽），并记入服务端日志。
func adminIPAllowlist(allowed []*net.IPNet) gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(allowed) == 0 { // 未配置 = 不限制（默认现状）
			c.Next()
			return
		}
		ip := net.ParseIP(c.ClientIP())
		if ip != nil {
			for _, n := range allowed {
				if n.Contains(ip) {
					c.Next()
					return
				}
			}
		}
		log.Printf("api: admin allowlist denied %s %s (client ip %q)",
			c.Request.Method, c.Request.URL.Path, c.ClientIP())
		writeErr(c, http.StatusForbidden, TypeForbidden, "来源 IP 不在管理白名单内")
		c.Abort()
	}
}

// ---------- 调用方维度配额与限速（B5） ----------

// enforceKeyPolicy 调用方维度限速与日额度：强制点在发送入口鉴权（authAPIKey
// 命中有效 key 之后、进入发送流程之前）——请求被拒在门外，不进入发送流程、
// 不落发送日志（与渠道额度 429 语义对齐：配额/限速拒绝的请求与未鉴权请求同样
// 「未发生」，调用方侧观感一致，回执 webhook 亦不推送）。
//
// 校验顺序：先限速后额度。限速是保护（防单 key 打满出站吞吐、拖慢其他调用方），
// 额度是业务（当日发送量上限）；被限速的请求尚未取得发送资格，不应触发额度
// 判定（额度按 status=sent 落库口径统计，被拒请求本就不落日志，两者天然一致，
// 先限速同时省掉被限请求的一次 COUNT 查询）。
//
// 两个维度均可独立关闭（0=不限）：rate_per_sec=0 跳过 limiter（零延迟直通），
// daily_quota=0 跳过 COUNT 查询（零额外 DB 开销）。存量 Key AutoMigrate 补列
// 默认两值皆 0，升级后行为与 B5 之前完全一致。
func (s *Server) enforceKeyPolicy(c *gin.Context, ak *model.ApiKey) bool {
	if ak.RatePerSec > 0 && !s.keyLimiter().wait(c.Request.Context(), ak.ID, ak.RatePerSec) {
		writeErr(c, http.StatusTooManyRequests, TypeRateLimited, "请求过于频繁（rate_per_sec 限制）")
		return false
	}
	if ak.DailyQuota > 0 {
		sent, err := s.keyDailySentCount(ak.ID)
		if err != nil {
			serverError(c, err)
			return false
		}
		if sent >= int64(ak.DailyQuota) {
			writeErr(c, http.StatusTooManyRequests, TypeQuotaExhausted,
				fmt.Sprintf("ApiKey 今日额度已用尽（daily_quota=%d）", ak.DailyQuota))
			return false
		}
	}
	return true
}

// keyDailySentCount 统计该 Key 今日发送成功条数：单条索引查询（send_logs 的
// api_key_id/status/created_at 均有索引，按 key 过滤后行数有界），本地日历日
// 零点起算（midnight 与 stats 统计口径一致）。测试日志 api_key_id 恒为空
// （测试发送不携带调用方 key），天然不计入，与 total_sent 口径的差异即在此。
func (s *Server) keyDailySentCount(keyID uint) (int64, error) {
	var n int64
	err := s.db.Model(&model.SendLog{}).
		Where("api_key_id = ? AND status = ? AND created_at >= ?",
			keyID, model.SendStatusSent, midnight(time.Now())).
		Count(&n).Error
	return n, err
}
