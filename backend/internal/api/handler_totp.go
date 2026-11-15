// handler_totp.go 两步验证（TOTP，C4）：绑定（setup/enable）/解绑（disable）与
// 登录第二步（login/totp）。安全基线：secret 与恢复码明文绝不进 GET 响应与日志；
// 恢复码仅 enable 响应一次性返回明文，库存 sha256 hex、命中即删除（单次有效）；
// 登录两步用 HMAC 短票据（t|exp|username|sig，复用 session secret）衔接，
// 与会话 cookie 双向隔离（verifySession 拒绝 t| 票据、login/totp 拒绝 v2 会话）。
package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// TOTP 参数与票据/恢复码常量。
const (
	totpIssuer = "CatMail" // otpauth URL 的 issuer（认证器显示名）

	// 登录第二步票据（ticket）：格式 "t|exp|username|sig"，HMAC-SHA1 签名复用
	// Setting.SessionSecret；5 分钟短时效——只证明「第一步密码已验通过」，本身
	// 不是会话，verifySession 一律拒绝（见 middleware.go 双向隔离注释）。
	totpTicketPrefix = "t"
	totpTicketTTL    = 5 * time.Minute

	recoveryCodeCount = 8  // enable 时生成的恢复码数量
	recoveryCodeLen   = 10 // 每个恢复码的字符数
)

// recoveryCodeAlphabet 恢复码字符集：A-Z2-7 免混淆字符集（base32 同源，无 0/1/8/9）。
const recoveryCodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// ---------- 票据签发与校验 ----------

// signTotpTicket 签发登录第二步票据 "t|exp|username|sig"。
func signTotpTicket(secret, username string, now time.Time) string {
	payload := fmt.Sprintf("%s|%d|%s", totpTicketPrefix, now.Add(totpTicketTTL).Unix(), username)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "|" + hex.EncodeToString(mac.Sum(nil))
}

// verifyTotpTicket 校验票据签名与有效期，返回其中的用户名。
// 与 verifySession 双向隔离：仅接受 t| 前缀，会话 cookie（v2|…）当票据用一律拒绝。
func (s *Server) verifyTotpTicket(token string) (string, bool) {
	sigIdx := strings.LastIndex(token, "|")
	if !strings.HasPrefix(token, totpTicketPrefix+"|") || sigIdx < 0 {
		return "", false // 非 t| 前缀（含会话 cookie / 旧格式 token）：直接拒绝
	}
	payload, sig := token[:sigIdx], token[sigIdx+1:]
	secret, ok := s.sessionSecret()
	if !ok {
		return "", false
	}
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(payload))
	expect := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expect)) != 1 {
		return "", false
	}
	// payload = "t|exp|username"（用户名可含 "|"，SplitN 限 3 段按段还原）
	parts := strings.SplitN(payload, "|", 3)
	if len(parts) < 3 || parts[2] == "" {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return parts[2], true
}

// ---------- 恢复码 ----------

// generateRecoveryCodes 生成 n 个随机恢复码（recoveryCodeLen 字符 A-Z2-7）：
// 256 % 32 == 0，取模无偏。
func generateRecoveryCodes(n, length int) ([]string, error) {
	codes := make([]string, n)
	buf := make([]byte, length)
	for i := range codes {
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		b := make([]byte, length)
		for j, v := range buf {
			b[j] = recoveryCodeAlphabet[int(v)%len(recoveryCodeAlphabet)]
		}
		codes[i] = string(b)
	}
	return codes, nil
}

// hashRecoveryCode 恢复码 sha256 hex（库存形态，明文不落库）。
func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// decodeRecoveryCodes 解析库存 JSON 数组；空值 = 无恢复码。
func decodeRecoveryCodes(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var codes []string
	if err := json.Unmarshal([]byte(raw), &codes); err != nil {
		return nil, err
	}
	return codes, nil
}

// ---------- TOTP 码 / 恢复码判定 ----------

// isTotpCode 形如 6 位数字 → TOTP 码；否则视为恢复码（恢复码 10 字符，长度即可区分）。
func isTotpCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// normalizeCode 统一码输入：去首尾空白、转大写（恢复码字符集为大写）。
func normalizeCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// ---------- 绑定 / 启用 / 解绑（均已登录） ----------

// totpSetup POST /api/auth/totp/setup：生成 160bit secret（base32），存
// totp_secret 但 enabled=false（待 enable 校验通过才生效），返回 {secret,
// otpauth_url}（issuer=CatMail，account=username）。重复调用覆盖未生效的旧
// secret（重新扫码即可）；已启用 → 400。secret 只在本次响应与 otpauth URL 中
// 出现，不进任何 GET 响应与日志。
func (s *Server) totpSetup(c *gin.Context) {
	var u model.User
	if err := s.db.Select("id", "username", "totp_enabled").
		Where("username = ?", c.GetString(ctxUsername)).First(&u).Error; err != nil {
		serverError(c, err)
		return
	}
	if u.TOTPEnabled {
		badRequest(c, "两步验证已启用")
		return
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: u.Username,
		// 默认 SecretSize=20（160bit）、SHA1、6 位、30s 周期：与主流认证器兼容
	})
	if err != nil {
		serverError(c, err)
		return
	}
	if err := s.db.Model(&model.User{}).Where("id = ?", u.ID).Updates(map[string]any{
		"totp_secret":  key.Secret(),
		"totp_enabled": false,
	}).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"secret": key.Secret(), "otpauth_url": key.URL()})
}

// totpEnableReq 启用请求体。
type totpEnableReq struct {
	Code string `json:"code"`
}

// totpEnable POST /api/auth/totp/enable {code}：验 TOTP（±1 窗口）→ enabled=true
// + 生成 8 个恢复码（明文仅本次响应返回，库存 sha256）+ bump 本人 epoch 并重签
// 当前 cookie（对齐改密模式：当前会话保持、其他全失效）。
func (s *Server) totpEnable(c *gin.Context) {
	var req totpEnableReq
	if !bindJSON(c, &req) {
		return
	}
	code := normalizeCode(req.Code)
	if code == "" {
		badRequest(c, "验证码不能为空")
		return
	}
	username := c.GetString(ctxUsername)
	var u model.User
	if err := s.db.Select("id", "username", "totp_secret", "totp_enabled").
		Where("username = ?", username).First(&u).Error; err != nil {
		serverError(c, err)
		return
	}
	if u.TOTPEnabled {
		badRequest(c, "两步验证已启用")
		return
	}
	if u.TOTPSecret == "" {
		badRequest(c, "请先开始绑定")
		return
	}
	// ±1 窗口（totp.Validate 默认 Skew=1）：容忍认证器时钟小幅漂移
	if !totp.Validate(code, u.TOTPSecret) {
		unauthorized(c, "验证码错误")
		return
	}
	codes, err := generateRecoveryCodes(recoveryCodeCount, recoveryCodeLen)
	if err != nil {
		serverError(c, err)
		return
	}
	hashes := make([]string, len(codes))
	for i, rc := range codes {
		hashes[i] = hashRecoveryCode(rc)
	}
	store, err := json.Marshal(hashes)
	if err != nil {
		serverError(c, err)
		return
	}
	// 签名密钥先取（不可用时直接失败，不做任何写入）：口径同 changeOwnPassword
	secret, ok := s.sessionSecret()
	if !ok {
		serverError(c, errors.New("session secret unavailable"))
		return
	}
	// 启用、恢复码入库与纪元递增同一事务；新纪元事务内回读后重签当前 cookie
	var newEpoch int
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.User{}).Where("id = ?", u.ID).Updates(map[string]any{
			"totp_enabled":   true,
			"recovery_codes": string(store),
		}).Error; err != nil {
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
	// expires_at，对齐改密模式）：当前会话保持，其他全失效
	s.reissueCurrentSession(c, secret, username, newEpoch)
	// 恢复码明文一次性返回（此后任何接口不再返回）
	c.JSON(http.StatusOK, gin.H{"recovery_codes": codes})
}

// totpDisableReq 解绑请求体（凭密码，不用 TOTP 码——手机丢了也能解绑）。
type totpDisableReq struct {
	Password string `json:"password"`
}

// totpDisable POST /api/auth/totp/disable {password}：校验当前密码 → 清空
// totp_secret/totp_enabled/recovery_codes 三字段 + bump epoch 重签 cookie
// （当前会话保持，其他全失效）。
func (s *Server) totpDisable(c *gin.Context) {
	var req totpDisableReq
	if !bindJSON(c, &req) {
		return
	}
	username := c.GetString(ctxUsername)
	var u model.User
	if err := s.db.Select("id", "username", "password_hash", "totp_enabled").
		Where("username = ?", username).First(&u).Error; err != nil {
		serverError(c, err)
		return
	}
	if !u.TOTPEnabled {
		badRequest(c, "两步验证未启用")
		return
	}
	// 超长密码（>72 字节）不可能匹配任何已存哈希，按「密码错误」处理，
	// 避免超长输入进入 bcrypt 比对（口径同 changeOwnPassword）
	if len(req.Password) > 72 || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		badRequest(c, "密码错误")
		return
	}
	secret, ok := s.sessionSecret()
	if !ok {
		serverError(c, errors.New("session secret unavailable"))
		return
	}
	var newEpoch int
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.User{}).Where("id = ?", u.ID).Updates(map[string]any{
			"totp_secret":    "",
			"totp_enabled":   false,
			"recovery_codes": "",
		}).Error; err != nil {
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
	// 当前会话重签（新纪元，安全加固三件套 C：保留原 session_id，对齐改密模式）
	s.reissueCurrentSession(c, secret, username, newEpoch)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------- 登录两步 ----------

// loginTotpReq 登录第二步请求体。
type loginTotpReq struct {
	Ticket string `json:"ticket"`
	Code   string `json:"code"`
}

// loginTotp POST /api/auth/login/totp {ticket, code}：code 为 6 位 TOTP 或恢复码
// （命中即从库存删除，单次有效）。复用登录限流（用户名+IP 双键）与防枚举口径：
// 全部失败路径（票据缺失/伪造/过期/码错/账号消失或已解绑）统一 401「用户名或
// 密码错误」，与登录失败文案一致。
func (s *Server) loginTotp(c *gin.Context) {
	var req loginTotpReq
	if !bindJSON(c, &req) {
		return
	}
	fail := func() {
		unauthorized(c, "用户名或密码错误")
	}
	hit := func(username string) {
		s.limiter.hit(loginLimitKey(username, c.ClientIP()), time.Now())
		s.limiter.hit(loginUsernameKey(username), time.Now())
	}
	ticket := strings.TrimSpace(req.Ticket)
	code := normalizeCode(req.Code)
	if ticket == "" || code == "" {
		fail()
		return
	}
	// 双向隔离（会话侧）：会话 cookie（v3/v2）不是票据，显式拒绝（票据校验本身也
	// 只接受 t| 前缀，此处前置拦截使口径明确、可测试锁定）
	if strings.HasPrefix(ticket, sessionTokenV3+"|") || strings.HasPrefix(ticket, "v2|") {
		fail()
		return
	}
	username, ok := s.verifyTotpTicket(ticket)
	if !ok {
		fail()
		return
	}
	key := loginLimitKey(username, c.ClientIP())
	ukey := loginUsernameKey(username)
	if s.limiter.locked(key, time.Now()) || s.limiter.locked(ukey, time.Now()) {
		writeErr(c, http.StatusTooManyRequests, TypeQuotaExhausted, "尝试次数过多，请稍后再试")
		return
	}
	var u model.User
	err := s.db.Select("id", "username", "session_epoch", "totp_secret", "totp_enabled", "recovery_codes").
		Where("username = ?", username).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		hit(username) // 账号在两步之间被删：票据作废，同样计数
		fail()
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	if !u.TOTPEnabled {
		hit(username) // 两步验证已被解绑：票据作废
		fail()
		return
	}
	if !s.consumeTotpCode(c, &u, code) {
		hit(username)
		fail()
		return
	}
	secret, ok := s.sessionSecret()
	if !ok {
		serverError(c, errors.New("session secret unavailable"))
		return
	}
	s.limiter.reset(key) // 成功清零失败计数（组合键与用户名键一并清零）
	s.limiter.reset(ukey)
	// v3 token（安全加固三件套，C）：登录第二步即写服务端会话注册表
	sessionID, serr := s.createSession(c, u.Username, time.Now())
	if serr != nil {
		serverError(c, serr)
		return
	}
	s.setSessionCookie(c, signSession(secret, u.Username, u.SessionEpoch, sessionID, time.Now()))
	c.JSON(http.StatusOK, gin.H{"username": u.Username})
}

// consumeTotpCode 校验两步验证码：6 位数字走 TOTP（±1 窗口）；否则按恢复码——
// sha256 后与库存逐条常量时间比对，命中即从库存删除（Compare-And-Swap 保证
// 并发重复使用同一码只有一个成功）。
func (s *Server) consumeTotpCode(c *gin.Context, u *model.User, code string) bool {
	if isTotpCode(code) {
		return u.TOTPSecret != "" && totp.Validate(code, u.TOTPSecret)
	}
	hash := hashRecoveryCode(code)
	codes, err := decodeRecoveryCodes(u.RecoveryCodes)
	if err != nil {
		log.Printf("api: decode recovery codes for %s: %v", u.Username, err)
		return false
	}
	idx := -1
	for i, h := range codes {
		if subtle.ConstantTimeCompare([]byte(h), []byte(hash)) == 1 {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	remaining := append(append([]string{}, codes[:idx]...), codes[idx+1:]...)
	store, err := json.Marshal(remaining)
	if err != nil {
		log.Printf("api: marshal recovery codes for %s: %v", u.Username, err)
		return false
	}
	// CAS 删除：库存已被并发请求变更（同码被并发消费）→ 本请求按失败处理
	res := s.db.Model(&model.User{}).
		Where("id = ? AND recovery_codes = ?", u.ID, u.RecoveryCodes).
		Update("recovery_codes", string(store))
	if res.Error != nil {
		log.Printf("api: consume recovery code for %s: %v", u.Username, res.Error)
		return false
	}
	return res.RowsAffected == 1
}
