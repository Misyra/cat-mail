// handler_outbox.go Webhook 投递记录查询（安全加固三件套，B）：GET /api/webhook-deliveries
// 分页列表供排查——outbox 投递由 scheduler 周期驱动（退避/转 dead 见 internal/outbox），
// 管理员可在此查看每条通知/回执投递的类型、状态、尝试次数与最后错误。
package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"catmail/internal/model"
)

// webhookDeliveryDTO 投递记录响应项。URL 原样返回（通知 webhook 地址可能内嵌
// token，但本端点在 /api 会话组内、与渠道 webhook_url 明文回显同口径，仅管理员可见）；
// secret_ref 为签名密钥引用（非密钥值本身），payload 不回显（大字段无排查价值，
// 且事件正文可能含收件人等敏感信息，避免整页放大）。
type webhookDeliveryDTO struct {
	ID          uint      `json:"id"`
	EventType   string    `json:"event_type"`
	URL         string    `json:"url"`
	SecretRef   string    `json:"secret_ref"`
	Attempts    int       `json:"attempts"`
	Status      string    `json:"status"`
	LastError   string    `json:"last_error"`
	NextRetryAt time.Time `json:"next_retry_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// listWebhookDeliveries GET /api/webhook-deliveries?page&page_size&status&event_type
// → {items,total}，按 id 倒序（新任务在前，排查口径）。
func (s *Server) listWebhookDeliveries(c *gin.Context) {
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
	q := s.db.Model(&model.WebhookDelivery{})
	if st := c.Query("status"); st != "" {
		switch st {
		case model.OutboxStatusPending, model.OutboxStatusSent, model.OutboxStatusDead:
			q = q.Where("status = ?", st)
		default:
			badRequest(c, "status 仅支持 pending/sent/dead")
			return
		}
	}
	if et := c.Query("event_type"); et != "" {
		q = q.Where("event_type = ?", et)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		serverError(c, err)
		return
	}
	var rows []model.WebhookDelivery
	if err := q.Order("id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	items := make([]webhookDeliveryDTO, 0, len(rows))
	for i := range rows {
		items = append(items, webhookDeliveryDTO{
			ID:          rows[i].ID,
			EventType:   rows[i].EventType,
			URL:         rows[i].URL,
			SecretRef:   rows[i].SecretRef,
			Attempts:    rows[i].Attempts,
			Status:      rows[i].Status,
			LastError:   rows[i].LastError,
			NextRetryAt: rows[i].NextRetryAt,
			CreatedAt:   rows[i].CreatedAt,
			UpdatedAt:   rows[i].UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}
