// handler_notifications.go 站内通知中心（O9 失败通知）：分页列表、未读数、
// 标记已读与通知通道连通性测试（SPEC §5）。
package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/notify"
)

// notifyEventTokens 通知事件 token（与 notify 包常量一致，顺序固定）：
// 五类基础事件 + bounce_rate_exceeded（送达链路三件套，渠道退信率超阈值 warn）。
var notifyEventTokens = []string{
	notify.EventChannelDisabled,
	notify.EventChannelCooldown,
	notify.EventChannelRecovered,
	notify.EventSendFailed,
	notify.EventUpdateAvailable,
	notify.EventBounceRate,
}

// normalizeNotifyEvents 校验并规范化 notify_events（PUT /api/settings 用）：
// 仅允许 notifyEventTokens 的逗号子集（项内空白容忍，其余空白原样剔除），
// 含未知/空 token → 错误；空串（或仅空白，即缺省语义）= 全部事件 →
// 规范化为固定顺序的全 token 串（前端契约：空=全部）。
func normalizeNotifyEvents(csv string) (string, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return strings.Join(notifyEventTokens, ","), nil
	}
	tokens := strings.Split(csv, ",")
	trimmed := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		t := strings.TrimSpace(tok)
		if !notifyEventValid(t) {
			return "", fmt.Errorf("notify_events 含未知事件 token（仅支持 %s）",
				strings.Join(notifyEventTokens, "/"))
		}
		trimmed = append(trimmed, t)
	}
	return strings.Join(trimmed, ","), nil
}

// notifyEventValid 判断 token 是否为合法事件名。
func notifyEventValid(tok string) bool {
	for _, want := range notifyEventTokens {
		if tok == want {
			return true
		}
	}
	return false
}

// notificationDTO 通知列表项（SPEC §5 契约字段）。
type notificationDTO struct {
	ID        uint      `json:"id"`
	Event     string    `json:"event"`
	Level     string    `json:"level"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	ChannelID *uint     `json:"channel_id"` // 关联渠道；与具体渠道无关的事件为 null
	Read      bool      `json:"read"`
	CreatedAt time.Time `json:"created_at"`
}

func newNotificationDTO(n *model.Notification) notificationDTO {
	return notificationDTO{
		ID:        n.ID,
		Event:     n.Event,
		Level:     n.Level,
		Title:     n.Title,
		Body:      n.Body,
		ChannelID: n.ChannelID,
		Read:      n.Read,
		CreatedAt: n.CreatedAt,
	}
}

// countUnread 统计未读通知数。
func (s *Server) countUnread() (int64, error) {
	var n int64
	err := s.db.Model(&model.Notification{}).Where("read = ?", false).Count(&n).Error
	return n, err
}

// listNotifications GET /api/notifications?page(≥1)&page_size(1-100,默认20)
// → {items,total,unread}，按 created_at/id 倒序（与 /api/logs 同口径）。
func (s *Server) listNotifications(c *gin.Context) {
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
	var total int64
	if err := s.db.Model(&model.Notification{}).Count(&total).Error; err != nil {
		serverError(c, err)
		return
	}
	var rows []model.Notification
	if err := s.db.Order("created_at DESC, id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	unread, err := s.countUnread()
	if err != nil {
		serverError(c, err)
		return
	}
	items := make([]notificationDTO, 0, len(rows))
	for i := range rows {
		items = append(items, newNotificationDTO(&rows[i]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total, "unread": unread})
}

// unreadCount GET /api/notifications/unread_count → {count}（顶栏轮询）。
func (s *Server) unreadCount(c *gin.Context) {
	unread, err := s.countUnread()
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"count": unread})
}

// markReadReq 标记已读请求体：ids=null（或缺省）= 全部已读；数组 = 指定 id 集合。
// 指针形态区分 null（全部）与 []（空集 no-op）。
type markReadReq struct {
	IDs *[]uint64 `json:"ids"`
}

// markNotificationsRead POST /api/notifications/read：标记已读，幂等（已读行
// 不再重复计数）→ {ok:true,updated:本次标记条数}。
func (s *Server) markNotificationsRead(c *gin.Context) {
	var req markReadReq
	if !bindJSON(c, &req) {
		return
	}
	if req.IDs != nil && len(*req.IDs) == 0 { // 空数组：无目标，直接成功返回
		c.JSON(http.StatusOK, gin.H{"ok": true, "updated": 0})
		return
	}
	q := s.db.Model(&model.Notification{}).Where("read = ?", false)
	if req.IDs != nil {
		q = q.Where("id IN ?", *req.IDs)
	}
	res := q.Update("read", true)
	if res.Error != nil {
		serverError(c, res.Error)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "updated": res.RowsAffected})
}

// testNotify POST /api/settings/notify/test：按「已保存」的通知设置实测双通道。
// 站内通道直接落一条测试 Notification（不经事件白名单与开关——测试必须可达）；
// webhook 通道仅在已启用时实测 notify.TestWebhook（按已保存的 notify_webhook_secret
// 签名，与 outbox 真实投递同一契约；未启用视为通过，与前端契约一致：
// webhook_ok=true 且 webhook_error=""）。
func (s *Server) testNotify(c *gin.Context) {
	var st model.Setting
	err := s.db.First(&st, 1).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		serverError(c, err)
		return
	}
	row := model.Notification{
		Event: "test", Level: notify.LevelInfo,
		Title: "CatMail 测试通知",
		Body:  "收到即表示站内通知通道工作正常。",
	}
	inappOK := true
	if err := s.db.Create(&row).Error; err != nil {
		inappOK = false // 落库失败照常返回，由 inapp_ok 如实上报
		log.Printf("api: testNotify 站内测试通知落库失败: %v", err)
	}
	webhookOK, webhookErr := true, ""
	if st.NotifyWebhookEnabled {
		if u := strings.TrimSpace(st.NotifyWebhookURL); u == "" {
			webhookOK, webhookErr = false, "webhook 已启用但未配置地址"
		} else if err := notify.TestWebhook(u, st.NotifyWebhookSecret); err != nil {
			webhookOK, webhookErr = false, err.Error()
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"inapp_ok":      inappOK,
		"webhook_ok":    webhookOK,
		"webhook_error": webhookErr,
	})
}
