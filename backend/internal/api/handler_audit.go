// handler_audit.go 管理操作审计查询（E4）：分页筛选列表（GET /api/audit，
// created_at 倒序）。审计行为写入见 audit.go 与各 handler 成功路径插桩。
package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// auditDTO 审计列表项响应体（model.AuditLog 的 snake_case 投影，时间口径与
// 其余 DTO 一致：time.Time 序列化为 RFC3339）。
type auditDTO struct {
	ID        uint      `json:"id"`
	Username  string    `json:"username"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Detail    string    `json:"detail"`
	IP        string    `json:"ip"`
	CreatedAt time.Time `json:"created_at"`
}

// auditActionMax 动作筛选输入上限（动作枚举最长 19 字符，留足余量防超长 LIKE）。
const auditActionMax = 64

// auditUsernameMax 操作者筛选输入上限（与 User.Username 的 64 字符存储上限一致）。
const auditUsernameMax = 64

// listAuditLogs GET /api/audit?page&page_size(1-100)&action=&username=：
// created_at/id 倒序分页（id 作 tiebreak 保证同秒记录顺序稳定）。
//   - action：前缀匹配（如 action=channel 命中全部 channel_* 动作），便于自由
//     输入按资源类别过滤；escapeLike 转义通配符防注入。
//   - username：精确匹配（操作者名）。
func (s *Server) listAuditLogs(c *gin.Context) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		badRequest(c, "page 必须为 ≥1 的整数")
		return
	}
	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if err != nil || pageSize < 1 || pageSize > 100 {
		badRequest(c, "page_size 必须为 1-100 的整数")
		return
	}
	action := strings.TrimSpace(c.Query("action"))
	if utf8.RuneCountInString(action) > auditActionMax {
		badRequest(c, "action 不能超过 64 字符")
		return
	}
	username := strings.TrimSpace(c.Query("username"))
	if utf8.RuneCountInString(username) > auditUsernameMax {
		badRequest(c, "username 不能超过 64 字符")
		return
	}

	// 条件构造器按需重建（与 logQueryCond 同风格，避免 GORM 语句复用）；
	// 全部参数校验已前置完成，此处不再产生错误。
	cond := func(q *gorm.DB) *gorm.DB {
		if action != "" {
			// 前缀匹配：action=channel → channel_create/channel_update/…
			q = q.Where("action LIKE ? ESCAPE '\\'", escapeLike(action)+"%")
		}
		if username != "" {
			q = q.Where("username = ?", username)
		}
		return q
	}

	var total int64
	if err := cond(s.db.Model(&model.AuditLog{})).Count(&total).Error; err != nil {
		serverError(c, err)
		return
	}
	var logs []model.AuditLog
	if err := cond(s.db.Model(&model.AuditLog{})).
		Order("created_at DESC, id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&logs).Error; err != nil {
		serverError(c, err)
		return
	}
	items := make([]auditDTO, 0, len(logs))
	for _, l := range logs {
		items = append(items, auditDTO{
			ID:        l.ID,
			Username:  l.Username,
			Action:    l.Action,
			Target:    l.Target,
			Detail:    l.Detail,
			IP:        l.IP,
			CreatedAt: l.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}
