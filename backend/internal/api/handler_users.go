// handler_users.go 账号管理（SPEC §5：登录用户均为管理员角色）。
package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// maxUsers 账号数量上限（SPEC 任务 B5）。
const maxUsers = 10

// errMustKeepOneUser 至少保留一个账号（事务内复核失败时返回）。
var errMustKeepOneUser = errors.New("必须至少保留一个账号")

// createUserReq 新建账号请求体。
type createUserReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// listUsers 用户列表（不含密码字段）。
func (s *Server) listUsers(c *gin.Context) {
	var users []model.User
	if err := s.db.Select("id", "username", "created_at").Order("id ASC").Find(&users).Error; err != nil {
		serverError(c, err)
		return
	}
	items := make([]userDTO, 0, len(users))
	for _, u := range users {
		items = append(items, userDTO{ID: u.ID, Username: u.Username, CreatedAt: u.CreatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// createUser 新建账号：用户名 2-32 字符、密码 ≥6、重名 400、上限 10 个。
// 重名采用「预检 + 唯一索引兜底」双层防护（P2-13）：并发创建击穿预检时，
// Create 的唯一约束冲突被翻译为 gorm.ErrDuplicatedKey，同样映射 400。
func (s *Server) createUser(c *gin.Context) {
	var req createUserReq
	if !bindJSON(c, &req) {
		return
	}
	username := strings.TrimSpace(req.Username)
	if err := validateUsername(username); err != nil {
		badRequest(c, err.Error())
		return
	}
	if err := validatePassword(req.Password); err != nil {
		badRequest(c, err.Error())
		return
	}
	var total int64
	if err := s.db.Model(&model.User{}).Count(&total).Error; err != nil {
		serverError(c, err)
		return
	}
	if total >= maxUsers {
		badRequest(c, "账号数量已达上限（10 个）")
		return
	}
	var dup int64
	if err := s.db.Model(&model.User{}).Where("username = ?", username).Count(&dup).Error; err != nil {
		serverError(c, err)
		return
	}
	if dup > 0 {
		badRequest(c, "用户名已存在")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		serverError(c, err)
		return
	}
	u := model.User{Username: username, PasswordHash: string(hash)}
	if err := s.db.Create(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			badRequest(c, "用户名已存在") // 并发重名：唯一索引兜底（P2-13）
			return
		}
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录；密码（含哈希）绝不入审计
	s.audit(c, model.AuditUserCreate,
		fmt.Sprintf("user#%d name=%s", u.ID, u.Username), "")
	c.JSON(http.StatusOK, userDTO{ID: u.ID, Username: u.Username, CreatedAt: u.CreatedAt})
}

// resetPasswordReq 管理员重置密码请求体。
type resetPasswordReq struct {
	NewPassword string `json:"new_password"`
}

// resetUserPassword 重置任意账号密码（新密码同规则）。
// 会话语义（按用户纪元，O10）：仅递增「目标用户」的 session_epoch——目标用户的
// 全部已签发会话立即失效；操作者及全体其他用户的会话不受影响。
// 若操作者重置自己的密码（API 允许、前端不提供该入口），操作者当前会话同样失效，
// 用新密码重新登录即可（此处不做当前会话重签）。
// 吊销联动（安全加固三件套，C，对齐 LanQin 口径）：同一事务内再执行联动三件套——
// 目标用户全部活跃会话吊销（sessions 表 revoked_at）+ 该用户创建的全部 API Key
// 停用（enabled=false + disabled_reason=password_reset，管理页可见原因提示、可
// 手动启用）；本人改密不受此联动影响（见 changeOwnPassword）。
func (s *Server) resetUserPassword(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req resetPasswordReq
	if !bindJSON(c, &req) {
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		badRequest(c, err.Error())
		return
	}
	var u model.User
	if err := s.db.First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "账号不存在")
			return
		}
		serverError(c, err)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		serverError(c, err)
		return
	}
	// 改密、目标用户会话纪元递增（按用户纪元，O10）与吊销联动（安全加固三件套，
	// C）同一事务：避免「密码已改而纪元未递增/联动未生效」的部分失败中间态
	//（目标用户旧会话仍然有效、其创建的 Key 仍可发送）。
	var (
		sessionsRevoked int64
		keysDisabled    int64
	)
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.User{}).Where("id = ?", u.ID).
			Update("password_hash", string(hash)).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.User{}).Where("id = ?", u.ID).
			Update("session_epoch", gorm.Expr("session_epoch + 1")).Error; err != nil {
			return err
		}
		// 联动一：全部活跃会话吊销（v3 token 的注册表校验即时拒绝）
		var rerr error
		sessionsRevoked, rerr = revokeAllSessionsOfUser(tx, u.Username)
		if rerr != nil {
			return rerr
		}
		// 联动二：该用户创建的全部 Key 停用（created_by 归属；存量无主 Key 空串
		// 不受影响，宁窄勿宽不误伤）。disabled_reason 供管理页展示「密码重置后
		// 停用，可手动启用」徽标；手动启用即清空标记。
		res := tx.Model(&model.ApiKey{}).
			Where("created_by = ? AND enabled = ?", u.Username, true).
			Updates(map[string]any{
				"enabled":         false,
				"disabled_reason": model.ApiKeyDisabledByPasswordReset,
			})
		keysDisabled = res.RowsAffected
		return res.Error
	}); err != nil {
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录；新密码明文/哈希绝不入审计。联动结果单独落
	// password_reset_linkage 一条（detail 仅计数，非敏感摘要口径）。
	s.audit(c, model.AuditUserResetPassword,
		fmt.Sprintf("user#%d name=%s", u.ID, u.Username), "")
	s.audit(c, model.AuditPasswordResetLinkage,
		fmt.Sprintf("user#%d name=%s", u.ID, u.Username),
		fmt.Sprintf("sessions_revoked=%d api_keys_disabled=%d", sessionsRevoked, keysDisabled))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// deleteUser 删除账号：不能删自己；必须至少保留一个账号。
// 保留约束在事务内「删除前计数 + 删除后复核」双层保证（P2-13）：并发删除竞态下
// 若删空全部账号则整体回滚，check-then-act 不再可击穿。
// 会话失效（按用户纪元，O10）：行删除后 requireSession 的存在性校验对该用户全部
// 旧会话直接 401（「账号不存在或已被删除」），无需额外纪元操作——硬删除后再对
// 目标 epoch+1 是无观测效果的空操作，故不做。
func (s *Server) deleteUser(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var u model.User
	if err := s.db.First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "账号不存在")
			return
		}
		serverError(c, err)
		return
	}
	if u.Username == c.GetString(ctxUsername) {
		badRequest(c, "不能删除当前登录账号")
		return
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 删除前计数：仅剩一个时拒绝（快路径）
		var total int64
		if err := tx.Model(&model.User{}).Count(&total).Error; err != nil {
			return err
		}
		if total <= 1 {
			return errMustKeepOneUser
		}
		if err := tx.Delete(&model.User{}, u.ID).Error; err != nil {
			return err
		}
		// 删除后复核：并发删除竞态下若清空全部账号，回滚本次删除
		var remain int64
		if err := tx.Model(&model.User{}).Count(&remain).Error; err != nil {
			return err
		}
		if remain < 1 {
			return errMustKeepOneUser
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errMustKeepOneUser) {
			badRequest(c, errMustKeepOneUser.Error())
			return
		}
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录
	s.audit(c, model.AuditUserDelete,
		fmt.Sprintf("user#%d name=%s", u.ID, u.Username), "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
