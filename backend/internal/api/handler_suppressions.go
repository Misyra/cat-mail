// handler_suppressions.go 收件人抑制列表管理（送达链路三件套）：分页筛选列表、
// 手动添加与解除（删除）。自动入库（hard bounce/complaint）在入站摄取路径
// （handler_webhook.go autoSuppress）；拦截在 rotator.Send 单点（suppress.go）。
package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// suppressionDTO 抑制列表项。
type suppressionDTO struct {
	ID          uint      `json:"id"`
	Email       string    `json:"email"`
	Reason      string    `json:"reason"`
	ChannelID   uint      `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	Detail      string    `json:"detail"`
	CreatedAt   time.Time `json:"created_at"`
}

func newSuppressionDTO(sp *model.Suppression) suppressionDTO {
	return suppressionDTO{
		ID:          sp.ID,
		Email:       sp.Email,
		Reason:      sp.Reason,
		ChannelID:   sp.ChannelID,
		ChannelName: sp.ChannelName,
		Detail:      sp.Detail,
		CreatedAt:   sp.CreatedAt,
	}
}

// listSuppressions GET /api/suppressions?page(≥1)&page_size(1-100,默认20)&
// reason(bounce/complaint/manual)&channel_id&q(email 模糊) → {items,total}
// （created_at 倒序，id 兜底排序稳定）。
func (s *Server) listSuppressions(c *gin.Context) {
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
	q := s.db.Model(&model.Suppression{})
	switch c.Query("reason") {
	case "":
	case model.SuppressReasonBounce, model.SuppressReasonComplaint, model.SuppressReasonManual:
		q = q.Where("reason = ?", c.Query("reason"))
	default:
		badRequest(c, "reason 仅支持 bounce/complaint/manual")
		return
	}
	if raw := c.Query("channel_id"); raw != "" {
		cid, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || cid == 0 {
			badRequest(c, "channel_id 无效")
			return
		}
		q = q.Where("channel_id = ?", cid)
	}
	if kw := strings.TrimSpace(c.Query("q")); kw != "" {
		if utf8.RuneCountInString(kw) > 128 {
			badRequest(c, "q 不能超过 128 字符")
			return
		}
		pat := "%" + escapeLike(kw) + "%"
		q = q.Where("email LIKE ? ESCAPE '\\'", pat)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		serverError(c, err)
		return
	}
	var rows []model.Suppression
	if err := q.Order("created_at DESC, id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	items := make([]suppressionDTO, 0, len(rows))
	for i := range rows {
		items = append(items, newSuppressionDTO(&rows[i]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// suppressionCreateReq 手动添加请求体：email 必填（合法邮箱，≤320 字符，入库
// 小写归一），detail 可选备注；reason 恒为 manual（bounce/complaint 仅由事件
// 自动入库产生，不接受手动伪造）。
type suppressionCreateReq struct {
	Email  string `json:"email"`
	Detail string `json:"detail"`
}

// createSuppression POST /api/suppressions：手动添加（重复 → 400，唯一索引兜底
// 并发首投）；ChannelID 0=未指定来源渠道。
func (s *Server) createSuppression(c *gin.Context) {
	var req suppressionCreateReq
	if !bindJSON(c, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || utf8.RuneCountInString(email) > 320 || !validEmail(email) {
		badRequest(c, "缺少有效收件邮箱 email")
		return
	}
	if utf8.RuneCountInString(req.Detail) > 500 {
		badRequest(c, "detail 过长（≤500 字符）")
		return
	}
	row := model.Suppression{
		Email:  email,
		Reason: model.SuppressReasonManual,
		Detail: req.Detail,
	}
	if err := s.db.Create(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			badRequest(c, "该邮箱已在抑制列表中")
			return
		}
		serverError(c, err)
		return
	}
	s.audit(c, model.AuditSuppressionCreate,
		fmt.Sprintf("suppression#%d email=%s", row.ID, row.Email), "")
	c.JSON(http.StatusOK, newSuppressionDTO(&row))
}

// deleteSuppression DELETE /api/suppressions/:id：解除（物理删除该行；同邮箱
// 之后的 hard bounce 事件会重新自动入库——解除的是当前抑制状态，不是拉黑豁免）。
func (s *Server) deleteSuppression(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var sp model.Suppression
	if err := s.db.First(&sp, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "抑制记录不存在")
			return
		}
		serverError(c, err)
		return
	}
	if err := s.db.Delete(&model.Suppression{}, id).Error; err != nil {
		serverError(c, err)
		return
	}
	s.audit(c, model.AuditSuppressionDelete,
		fmt.Sprintf("suppression#%d email=%s", sp.ID, sp.Email), "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
