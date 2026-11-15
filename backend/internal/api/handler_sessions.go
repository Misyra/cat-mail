// handler_sessions.go 服务端会话注册表管理端点（安全加固三件套，C）：
//   - GET  /api/sessions              本人活跃会话列表（当前会话标记）
//   - DELETE /api/sessions/:id        踢指定会话（不能踢当前）
//   - POST /api/sessions/revoke-others 踢其余全部
//
// 吊销只改注册表（revoked_at），不动 API Key（与密码重置联动解耦：本人主动下线
// 其他会话属自防动作，不影响集成）。被吊销会话的旧 token 签名仍合法，但
// requireSession 查注册表即 401——即时生效，无需等 token 过期。
package api

import (
	"fmt"
	"log"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// sessionUAMax User-Agent 截断上限（字符）：与会话表 size:512 列宽对齐。
const sessionUAMax = 512

// createSession 登录成功后的注册表写入：生成 32 字节随机 hex 会话 ID，快照来源
// IP 与 User-Agent（展示「哪台设备」用）。签名密钥与 token 由调用方继续签发
// （signSession 携带本 ID）。失败返回错误——登录流程须整体失败（无注册表行就
// 签发 token 等于绕过注册表校验）。
func (s *Server) createSession(c *gin.Context, username string, now time.Time) (string, error) {
	id, err := randomHex(16) // 32 位 hex：256bit 熵，碰撞不可行
	if err != nil {
		return "", err
	}
	row := model.Session{
		ID:         id,
		Username:   username,
		IP:         c.ClientIP(),
		UserAgent:  truncateSessionUA(c.Request.UserAgent()),
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(sessionTTL),
	}
	if err := s.db.Create(&row).Error; err != nil {
		return "", err
	}
	return id, nil
}

// truncateSessionUA UA 截断（按 Unicode 码点，防超长写库）。
func truncateSessionUA(ua string) string {
	if utf8.RuneCountInString(ua) <= sessionUAMax {
		return ua
	}
	runes := []rune(ua)
	return string(runes[:sessionUAMax])
}

// sessionItemDTO 会话列表项（本人视角）。
type sessionItemDTO struct {
	ID         string    `json:"id"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Current    bool      `json:"current"`
}

// listSessions GET /api/sessions：本人未吊销且未过期的会话，按最后活跃倒序；
// 当前会话带 current 标记（前端展示「本机」徽标）。
func (s *Server) listSessions(c *gin.Context) {
	username := c.GetString(ctxUsername)
	now := time.Now()
	var rows []model.Session
	if err := s.db.Where("username = ? AND revoked_at IS NULL AND expires_at > ?", username, now).
		Order("last_seen_at DESC, id DESC").Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	current := c.GetString(ctxSessionID)
	items := make([]sessionItemDTO, 0, len(rows))
	for _, r := range rows {
		items = append(items, sessionItemDTO{
			ID:         r.ID,
			IP:         r.IP,
			UserAgent:  r.UserAgent,
			CreatedAt:  r.CreatedAt,
			LastSeenAt: r.LastSeenAt,
			ExpiresAt:  r.ExpiresAt,
			Current:    r.ID == current,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// revokeSession 吊销指定会话行（属主与未吊销前置校验由调用方完成或在 WHERE 中），
// 返回是否实际吊销（并发重复吊销 RowsAffected=0）。
func (s *Server) revokeSession(id, username string) (bool, error) {
	res := s.db.Model(&model.Session{}).
		Where("id = ? AND username = ? AND revoked_at IS NULL", id, username).
		Update("revoked_at", time.Now())
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// deleteSession DELETE /api/sessions/:id：踢本人指定会话。不能踢当前会话
// （下线自己请走登出，口径明确）；非本人会话一律 404（不探测他人会话存在性）。
// 审计 sessions_revoke（action=kick）。
func (s *Server) deleteSession(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		badRequest(c, "无效的会话 ID")
		return
	}
	username := c.GetString(ctxUsername)
	if id == c.GetString(ctxSessionID) {
		badRequest(c, "不能下线当前会话（请使用退出登录）")
		return
	}
	revoked, err := s.revokeSession(id, username)
	if err != nil {
		serverError(c, err)
		return
	}
	if !revoked {
		notFound(c, "会话不存在或已下线")
		return
	}
	// 审计（E4）：仅成功路径记录
	s.audit(c, model.AuditSessionRevoke,
		fmt.Sprintf("user#%d name=%s", c.GetUint(ctxUserID), username), "action=kick session="+id)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// revokeOthersResp revoke-others 响应体。
type revokeOthersResp struct {
	Revoked int64 `json:"revoked"`
}

// revokeOtherSessions POST /api/sessions/revoke-others：下线本人除当前外的全部
// 活跃会话（单条 UPDATE 圈定，幂等）。审计 sessions_revoke（action=revoke_others）。
func (s *Server) revokeOtherSessions(c *gin.Context) {
	username := c.GetString(ctxUsername)
	res := s.db.Model(&model.Session{}).
		Where("username = ? AND revoked_at IS NULL AND id <> ?", username, c.GetString(ctxSessionID)).
		Update("revoked_at", time.Now())
	if res.Error != nil {
		serverError(c, res.Error)
		return
	}
	// 审计（E4）：仅成功路径记录；计数入 detail（非敏感摘要口径）
	s.audit(c, model.AuditSessionRevoke,
		fmt.Sprintf("user#%d name=%s", c.GetUint(ctxUserID), username),
		fmt.Sprintf("action=revoke_others count=%d", res.RowsAffected))
	c.JSON(http.StatusOK, revokeOthersResp{Revoked: res.RowsAffected})
}

// reissueCurrentSession 当前会话重签（改密/两步验证启用解绑共用，安全加固三件套
// C + O10 语义）：token 携带新纪元但保留原 session_id——注册表行仍有效（当前
// 会话保持登录），expires_at 同步顺延为新的 token 过期点。注册表行由 requireSession
// 保证存在（中间件已校验），此处顺延失败仅记日志不阻断（token 自身仍有效至
// 原 exp，届时需重新登录，属可接受的降级）。
func (s *Server) reissueCurrentSession(c *gin.Context, secret, username string, newEpoch int) {
	sessionID := c.GetString(ctxSessionID)
	now := time.Now()
	s.setSessionCookie(c, signSession(secret, username, newEpoch, sessionID, now))
	if err := s.db.Model(&model.Session{}).
		Where("id = ? AND username = ? AND revoked_at IS NULL", sessionID, username).
		Update("expires_at", now.Add(sessionTTL)).Error; err != nil {
		log.Printf("api: extend session expiry failed (session=%s): %v", sessionID, err)
	}
}

// revokeAllSessionsOfUser 批量吊销某用户全部活跃会话（密码重置联动三件套之一，
// 安全加固三件套 C）：返回吊销条数。事务内调用（tx 传入保证与纪元递增原子）。
func revokeAllSessionsOfUser(tx *gorm.DB, username string) (int64, error) {
	res := tx.Model(&model.Session{}).
		Where("username = ? AND revoked_at IS NULL", username).
		Update("revoked_at", time.Now())
	return res.RowsAffected, res.Error
}
