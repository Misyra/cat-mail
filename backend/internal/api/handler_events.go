// handler_events.go 送达事件管理端查询（送达链路三件套）：分页筛选列表与含
// raw 报文的详情（入站摄取在 /webhooks/mail-events/:token 公开端点）。
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// mailEventListColumns 列表查询列（排除 raw 大字段）。
const mailEventListColumns = "id, channel_id, channel_name, provider, event, bounce_type, " +
	"provider_message_id, message_id, send_log_id, recipient, detail, occurred_at, created_at"

// mailEventQueryCond 解析 /api/mail-events 查询参数：
// event（枚举 sent/delivered/bounced/complained/raw，空=全部）、channel_id、
// q 收件人模糊（≤128 字符）。
func mailEventQueryCond(c *gin.Context) (func(*gorm.DB) (*gorm.DB, error), error) {
	cond := func(q *gorm.DB) (*gorm.DB, error) {
		switch c.Query("event") {
		case "":
		case model.MailEventSent, model.MailEventDelivered, model.MailEventBounced,
			model.MailEventComplained, model.MailEventRaw:
			q = q.Where("event = ?", c.Query("event"))
		default:
			return nil, errors.New("event 仅支持 sent/delivered/bounced/complained/raw")
		}
		if raw := c.Query("bounce_type"); raw != "" {
			if raw != model.BounceTypeHard && raw != model.BounceTypeSoft {
				return nil, errors.New("bounce_type 仅支持 hard/soft")
			}
			q = q.Where("bounce_type = ?", raw)
		}
		if raw := c.Query("channel_id"); raw != "" {
			cid, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || cid == 0 {
				return nil, errors.New("channel_id 无效")
			}
			q = q.Where("channel_id = ?", cid)
		}
		if kw := strings.TrimSpace(c.Query("q")); kw != "" {
			if utf8.RuneCountInString(kw) > 128 {
				return nil, errors.New("q 不能超过 128 字符")
			}
			pat := "%" + escapeLike(kw) + "%"
			q = q.Where("recipient LIKE ? ESCAPE '\\'", pat)
		}
		return q, nil
	}
	return cond, nil
}

// mailEventDTO 送达事件列表项/详情（raw 仅详情携带）。
type mailEventDTO struct {
	ID                uint      `json:"id"`
	ChannelID         uint      `json:"channel_id"`
	ChannelName       string    `json:"channel_name"`
	Provider          string    `json:"provider"`
	Event             string    `json:"event"`
	BounceType        string    `json:"bounce_type"`
	ProviderMessageID string    `json:"provider_message_id"`
	MessageID         string    `json:"message_id"`
	SendLogID         uint      `json:"send_log_id"`
	Recipient         string    `json:"recipient"`
	Detail            string    `json:"detail"`
	OccurredAt        time.Time `json:"occurred_at"`
	CreatedAt         time.Time `json:"created_at"`
	Raw               *any      `json:"raw,omitempty"` // 仅详情：JSON 解析失败回退原始字符串
}

// newMailEventDTO 由落库记录构造 DTO；withRaw 时尝试把 raw 解析为 JSON 对象
// （前端可直接格式化展示；解析失败回退字符串）。
func newMailEventDTO(e *model.MailEvent, withRaw bool) mailEventDTO {
	dto := mailEventDTO{
		ID:                e.ID,
		ChannelID:         e.ChannelID,
		ChannelName:       e.ChannelName,
		Provider:          e.Provider,
		Event:             e.Event,
		BounceType:        e.BounceType,
		ProviderMessageID: e.ProviderMessageID,
		MessageID:         e.MessageID,
		SendLogID:         e.SendLogID,
		Recipient:         e.Recipient,
		Detail:            e.Detail,
		OccurredAt:        e.OccurredAt,
		CreatedAt:         e.CreatedAt,
	}
	if withRaw && e.Raw != "" {
		var v any
		if json.Unmarshal([]byte(e.Raw), &v) == nil {
			dto.Raw = &v
		} else {
			raw := any(e.Raw) // *string 不能直接赋给 *any：经 any 装箱保持语义
			dto.Raw = &raw
		}
	}
	return dto
}

// listMailEvents GET /api/mail-events?page(≥1)&page_size(1-100,默认20)&event&
// bounce_type&channel_id&q → {items,total}（occurred_at 倒序，id 兜底排序稳定）。
func (s *Server) listMailEvents(c *gin.Context) {
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
	cond, cerr := mailEventQueryCond(c)
	if cerr != nil {
		badRequest(c, cerr.Error())
		return
	}
	base, err := cond(s.db.Model(&model.MailEvent{}))
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		serverError(c, err)
		return
	}
	base, err = cond(s.db.Model(&model.MailEvent{}))
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	var rows []model.MailEvent
	if err := base.Select(mailEventListColumns).
		Order("occurred_at DESC, id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	items := make([]mailEventDTO, 0, len(rows))
	for i := range rows {
		items = append(items, newMailEventDTO(&rows[i], false))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// getMailEvent GET /api/mail-events/:id：完整详情（含 raw 报文）。
func (s *Server) getMailEvent(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var e model.MailEvent
	if err := s.db.First(&e, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "送达事件不存在")
			return
		}
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, newMailEventDTO(&e, true))
}
