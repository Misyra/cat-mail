// handler_channel_webhook.go 渠道入站 webhook token 管理（送达链路三件套）：
// POST /api/channels/:id/webhook-token——存量渠道补生成（token 为空的旧行）或
// 显式轮换（rotate:true）。轮换后旧 URL 立即失效（token 唯一索引精确匹配），
// 平台侧需更新 webhook 地址。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// webhookTokenPath 入站事件端点的路径前缀（前端拼接完整 URL 用）。
const webhookTokenPath = "/webhooks/mail-events/"

// rotateChannelWebhookTokenReq 请求体：rotate=true 强制轮换（否则仅在 token 为空
// 时补生成——存量渠道升级后首次调用即补齐）。请求体整体可选（空 body = 补生成）。
type rotateChannelWebhookTokenReq struct {
	Rotate bool `json:"rotate"`
}

// rotateChannelWebhookToken POST /api/channels/:id/webhook-token →
// {webhook_token, webhook_url}（webhook_url 为带 token 的相对路径，前端拼 origin）。
func (s *Server) rotateChannelWebhookToken(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req rotateChannelWebhookTokenReq
	if c.Request.Body != nil {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			badRequest(c, "请求体读取失败")
			return
		}
		// 空 body 容忍为缺省（rotate=false，补生成语义）；非空 body 必须是合法 JSON
		if len(body) > 0 && json.Unmarshal(body, &req) != nil {
			badRequest(c, "请求体格式错误")
			return
		}
	}
	var ch model.Channel
	if err := s.db.First(&ch, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "渠道不存在")
			return
		}
		serverError(c, err)
		return
	}
	rotate := ch.WebhookToken == "" || req.Rotate
	if rotate {
		token, err := randomHex(16) // 32 位 hex：256bit 熵，穷举不可行
		if err != nil {
			serverError(c, err)
			return
		}
		// 唯一索引兜底并发轮换（极小概率撞 token）：冲突如实报错请调用方重试
		res := s.db.Model(&model.Channel{}).Where("id = ?", ch.ID).Update("webhook_token", token)
		if res.Error != nil {
			if errors.Is(res.Error, gorm.ErrDuplicatedKey) {
				serverError(c, errors.New("webhook token 冲突，请重试"))
				return
			}
			serverError(c, res.Error)
			return
		}
		ch.WebhookToken = token
		// 审计（E4）：仅成功路径记录；token 值不入审计（凭据类红线同款口径）
		detail := "ensure"
		if req.Rotate {
			detail = "rotate"
		}
		s.audit(c, model.AuditChannelWebhookToken,
			fmt.Sprintf("channel#%d name=%s", ch.ID, ch.Name), detail)
	}
	c.JSON(http.StatusOK, gin.H{
		"webhook_token": ch.WebhookToken,
		"webhook_url":   webhookTokenPath + ch.WebhookToken,
	})
}
