// handler_auth.go 登录/登出/会话信息/本人改密（SPEC §5）。
package api

import (
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// loginReq 登录请求体。
type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// ---------- 登录失败限流（P2-1） ----------

// 登录限流参数：同 用户名+IP 组合在窗口内失败达阈值 → 锁定 lockout 时长。
const (
	loginFailWindow  = 10 * time.Minute // 失败计数窗口
	loginFailLimit   = 5                // 窗口内允许的最大失败次数
	loginLockoutTime = 15 * time.Minute // 触发阈值后的锁定时长
)

// loginFail 单个 用户名+IP 组合的失败计数状态。
type loginFail struct {
	count       int       // 窗口内失败次数
	windowStart time.Time // 窗口起点（窗口内首次失败时间）
	lockedUntil time.Time // 零值 = 未锁定
}

// loginLimiter 登录失败限流器（内存态，单实例足够）：防在线爆破。
// 惰性清理过期条目（读写时顺带清扫），不建后台 goroutine；
// Server 每次装配新建实例，测试环境天然隔离。
type loginLimiter struct {
	mu      sync.Mutex
	m       map[string]*loginFail
	window  time.Duration
	lockout time.Duration
	limit   int
}

// newLoginLimiter 按默认参数构造限流器。
func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		m:       map[string]*loginFail{},
		window:  loginFailWindow,
		lockout: loginLockoutTime,
		limit:   loginFailLimit,
	}
}

// loginLimitKey 限流键：用户名 + 客户端 IP（NUL 分隔避免键歧义）。
func loginLimitKey(username, ip string) string {
	return username + "\x00" + ip
}

// loginUsernameKey 仅按用户名的限流键（\x01 前缀与组合键隔离）：ClientIP 取自
// X-Forwarded-For 等可伪造头，攻击者轮换伪造 IP 可绕过「用户名+IP」组合键，
// 用户名维度的独立计数保证单账号爆破始终受限。
func loginUsernameKey(username string) string {
	return "\x01" + username
}

// dummyBcryptHash 「用户不存在」路径的哨兵哈希（对随机口令生成，无法反推）：
// 该路径同样执行一次 bcrypt 比对，与「密码错误」路径耗时对齐，防止按响应时间枚举用户名。
var dummyBcryptHash = func() []byte {
	pw, err := randomHex(16)
	if err != nil {
		return nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return nil
	}
	return h
}()

// locked 判断该组合是否处于锁定期；锁定期满时清掉旧状态重新计数。
func (l *loginLimiter) locked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)
	f, ok := l.m[key]
	if !ok {
		return false
	}
	if !f.lockedUntil.IsZero() {
		if now.Before(f.lockedUntil) {
			return true
		}
		delete(l.m, key) // 锁定已过期：清零重新计数
	}
	return false
}

// hit 记录一次失败；窗口内累计达阈值即进入锁定（本次请求仍按原 401 返回，
// 后续请求开始 429）。
func (l *loginLimiter) hit(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)
	f, ok := l.m[key]
	if !ok || now.Sub(f.windowStart) > l.window {
		// 无记录或窗口已滚过：重新起窗（锁定过期的旧记录同被覆盖）
		f = &loginFail{windowStart: now}
		l.m[key] = f
	}
	f.count++
	if f.count >= l.limit {
		f.lockedUntil = now.Add(l.lockout)
	}
}

// reset 成功登录后清除该组合的失败计数。
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

// gcLocked 惰性清理：删除窗口已滚过且未锁定（或锁定已过期）的条目；调用方须持锁。
func (l *loginLimiter) gcLocked(now time.Time) {
	for k, f := range l.m {
		if !f.lockedUntil.IsZero() {
			if now.Before(f.lockedUntil) {
				continue // 锁定中：保留
			}
		} else if now.Sub(f.windowStart) <= l.window {
			continue // 窗口内：保留
		}
		delete(l.m, k)
	}
}

// ---------- 登录 / 登出 / 会话 ----------

// login 校验 bcrypt 凭据并签发 7 天 HMAC 会话 Cookie。
// 超长凭据与错误凭据同样返回 401（bcrypt 计算成本随输入长度线性放大，入口处截断防滥用）。
// 登录限流（P2-1）：锁定检查先于凭据校验；命中锁定一律 429「尝试次数过多」。
// 两步验证（C4）：密码正确但用户已启用 TOTP → 不建会话，200 返回
// {totp_required:true, ticket}（5 分钟 HMAC 短票据），由登录第二步
// POST /api/auth/login/totp 凭票据 + 动态码/恢复码完成登录。
func (s *Server) login(c *gin.Context) {
	var req loginReq
	if !bindJSON(c, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		badRequest(c, "用户名与密码不能为空")
		return
	}
	key := loginLimitKey(req.Username, c.ClientIP())
	ukey := loginUsernameKey(req.Username)
	if s.limiter.locked(key, time.Now()) || s.limiter.locked(ukey, time.Now()) {
		writeErr(c, http.StatusTooManyRequests, TypeQuotaExhausted, "尝试次数过多，请稍后再试")
		return
	}
	if len(req.Username) > 256 || len(req.Password) > 256 {
		s.limiter.hit(key, time.Now())
		s.limiter.hit(ukey, time.Now())
		unauthorized(c, "用户名或密码错误")
		return
	}
	var u model.User
	err := s.db.Where("username = ?", req.Username).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(req.Password)) // 耗时对齐，防用户名枚举
		s.limiter.hit(key, time.Now())                                           // 不存在的用户同样计数，防枚举爆破
		s.limiter.hit(ukey, time.Now())
		unauthorized(c, "用户名或密码错误")
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		s.limiter.hit(key, time.Now())
		s.limiter.hit(ukey, time.Now())
		unauthorized(c, "用户名或密码错误")
		return
	}
	secret, ok := s.sessionSecret()
	if !ok {
		serverError(c, errors.New("session secret unavailable"))
		return
	}
	s.limiter.reset(key) // 成功登录清零失败计数（组合键与用户名键一并清零）
	s.limiter.reset(ukey)
	// 两步验证（C4）：第一步仅证明密码正确，不建会话——签发短票据进入第二步；
	// 票据签名复用 session secret，与会话 cookie 双向隔离（middleware.go）
	if u.TOTPEnabled {
		c.JSON(http.StatusOK, gin.H{"totp_required": true, "ticket": signTotpTicket(secret, u.Username, time.Now())})
		return
	}
	// v3 token（安全加固三件套，C）：登录即写服务端会话注册表（IP/UA 快照），
	// token 携带 session_id——踢出/吊销经注册表即时生效
	sessionID, err := s.createSession(c, u.Username, time.Now())
	if err != nil {
		serverError(c, err)
		return
	}
	s.setSessionCookie(c, signSession(secret, u.Username, u.SessionEpoch, sessionID, time.Now()))
	c.JSON(http.StatusOK, gin.H{"username": u.Username})
}

// logout 清除会话 Cookie，并尽力吊销注册表当前会话行（安全加固三件套，C：
// 服务端同步失效，复用被盗 cookie 无效）；吊销失败不阻断登出（客户端 Cookie
// 已清，响应恒 200）。
func (s *Server) logout(c *gin.Context) {
	if sid := c.GetString(ctxSessionID); sid != "" {
		if _, err := s.revokeSession(sid, c.GetString(ctxUsername)); err != nil {
			log.Printf("api: revoke session on logout failed (session=%s): %v", sid, err)
		}
	}
	s.clearSessionCookie(c)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// me 当前会话用户：返回 {id, username, totp_enabled}（id 与两步验证状态由会话
// 中间件注入；secret/恢复码不在此出现）。
func (s *Server) me(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"id":           c.GetUint(ctxUserID),
		"username":     c.GetString(ctxUsername),
		"totp_enabled": c.GetBool(ctxTotpEnabled),
	})
}

// changePasswordReq 本人改密请求体。
type changePasswordReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// changeOwnPassword 修改当前登录账号密码（bcrypt 校验旧密码）。
// 会话语义（按用户纪元，O10）：改密与本人 session_epoch +1 同一事务；成功后用
// 新纪元「重签当前会话 Cookie」——当前会话保持登录（无需重新登录），本人其他
// 已签发会话（其他浏览器/标签）全部失效。页面提示文案由前端负责，后端仅保证
// 上述行为与 200 响应必带重签后的 Set-Cookie。
func (s *Server) changeOwnPassword(c *gin.Context) {
	var req changePasswordReq
	if !bindJSON(c, &req) {
		return
	}
	username := c.GetString(ctxUsername)
	var u model.User
	if err := s.db.Where("username = ?", username).First(&u).Error; err != nil {
		serverError(c, err)
		return
	}
	// 旧密码超长（>72 字节，超出 bcrypt 输入上限）不可能匹配任何已存密码哈希，
	// 与新密码同规则直接按「旧密码错误」处理，避免超长输入进入 bcrypt 比对。
	if len(req.OldPassword) > 72 || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.OldPassword)) != nil {
		badRequest(c, "旧密码错误")
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		badRequest(c, err.Error())
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		serverError(c, err)
		return
	}
	// 签名密钥先取（不可用时直接失败，不做任何写入）：密钥仅首启生成、运行期
	// 不变，事务前后取值一致。
	secret, ok := s.sessionSecret()
	if !ok {
		serverError(c, errors.New("session secret unavailable"))
		return
	}
	// 改密与会话纪元递增（按用户纪元，O10）同一事务（与 resetUserPassword 口径
	// 对齐）：避免「密码已改而纪元未递增」的部分失败中间态（本人旧会话仍然有效）。
	// 新纪元在事务内回读（同一连接可见本事务写入），保证重签 cookie 携带的纪元
	// 与提交值严格一致。
	var newEpoch int
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.User{}).Where("id = ?", u.ID).
			Update("password_hash", string(hash)).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.User{}).Where("id = ?", u.ID).
			Update("session_epoch", gorm.Expr("session_epoch + 1")).Error; err != nil {
			return err
		}
		var fresh model.User
		if err := tx.Select("session_epoch").Where("id = ?", u.ID).Take(&fresh).Error; err != nil {
			return err
		}
		newEpoch = fresh.SessionEpoch
		return nil
	}); err != nil {
		serverError(c, err)
		return
	}
	// 当前会话重签（新纪元，安全加固三件套 C：保留原 session_id 并顺延注册表
	// expires_at）：保持本会话登录；旧纪元 cookie 与本人其他会话全部失效
	s.reissueCurrentSession(c, secret, username, newEpoch)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
