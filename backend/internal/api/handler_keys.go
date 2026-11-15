// handler_keys.go API Key 管理：明文仅创建时返回一次，列表一律掩码（SPEC §5）。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/receipt"
)

// createKeyReq 新建 Key 请求体。daily_quota/rate_per_sec 为调用方维度配额与
// 限速（B5）：0 = 不限（默认），负数整体拒绝。
type createKeyReq struct {
	Name       string `json:"name"`
	DailyQuota int    `json:"daily_quota"`
	RatePerSec int    `json:"rate_per_sec"`
}

// updateKeyReq 更新 Key 请求体（指针字段：未提供 = 保留）。webhook_* 为调用方
// 回执配置（D1）：传空串 = 清除（显式 opt-in 的反向操作），与 name 的指针语义一致。
// daily_quota/rate_per_sec 为调用方维度配额与限速（B5）：指针语义同 webhook 字段
// ——未提供 = 保留，显式 0 = 改为不限；负数整体拒绝（不做部分写入）。
type updateKeyReq struct {
	Name          *string `json:"name"`
	Enabled       *bool   `json:"enabled"`
	WebhookURL    *string `json:"webhook_url"`
	WebhookEvents *string `json:"webhook_events"`
	DailyQuota    *int    `json:"daily_quota"`
	RatePerSec    *int    `json:"rate_per_sec"`
}

// listKeys Key 列表：key 一律掩码 cm-abcd…wxyz；total_sent 单条 GROUP BY
// 聚合（该 key status=sent 的日志总数，含测试发送，避免 N+1）。
func (s *Server) listKeys(c *gin.Context) {
	var keys []model.ApiKey
	if err := s.db.Order("id ASC").Find(&keys).Error; err != nil {
		serverError(c, err)
		return
	}
	totals, err := s.keySentTotals()
	if err != nil {
		serverError(c, err)
		return
	}
	items := make([]keyDTO, 0, len(keys))
	for i := range keys {
		items = append(items, newKeyDTO(&keys[i], totals[keys[i].ID]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// keySentTotals 单条 GROUP BY 聚合各 Key 的发送成功总数（SendLog 中
// status=sent，含测试发送，不做 is_test 过滤）；无发送记录的 Key 不在
// 返回 map 中，调用方按 0 处理。
func (s *Server) keySentTotals() (map[uint]int64, error) {
	var rows []struct {
		ApiKeyID uint
		N        int64
	}
	if err := s.db.Model(&model.SendLog{}).
		Select("api_key_id, COUNT(*) AS n").
		Where("status = ?", model.SendStatusSent).
		Group("api_key_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	m := make(map[uint]int64, len(rows))
	for _, r := range rows {
		m[r.ApiKeyID] = r.N
	}
	return m, nil
}

// createKey 新建 Key：生成 cm- + 32 位随机 hex；明文仅本次响应返回。
func (s *Server) createKey(c *gin.Context) {
	var req createKeyReq
	if !bindJSON(c, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
		badRequest(c, "名称不能为空且不超过 64 字符")
		return
	}
	// B5：配额/限速非负校验（0=不限），负数直接拒绝
	if req.DailyQuota < 0 {
		badRequest(c, "daily_quota 不能为负数（0=不限）")
		return
	}
	if req.RatePerSec < 0 {
		badRequest(c, "rate_per_sec 不能为负数（0=不限）")
		return
	}
	secret, err := randomHex(16)
	if err != nil {
		serverError(c, err)
		return
	}
	// 回执签名密钥（安全加固三件套，B）：创建即自动生成（32 字节随机 hex），管理页
	// 可查看/复制、可经 POST /api/keys/:id/webhook-secret 轮换；回执投递据此签名。
	whsec, err := randomHex(32)
	if err != nil {
		serverError(c, err)
		return
	}
	k := model.ApiKey{
		Name:          name,
		Key:           "cm-" + secret,
		Enabled:       true,
		DailyQuota:    req.DailyQuota,
		RatePerSec:    req.RatePerSec,
		WebhookSecret: whsec,
		// 创建者归属（安全加固三件套，C）：密码重置联动停用按 created_by 圈定；
		// 存量升级 Key（created_by 为空）不受联动影响。
		CreatedBy: c.GetString(ctxUsername),
	}
	if err := s.db.Create(&k).Error; err != nil {
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录；key 明文绝不入审计（响应返回明文，审计只记名称）
	s.audit(c, model.AuditKeyCreate,
		fmt.Sprintf("key#%d name=%s", k.ID, k.Name),
		fmt.Sprintf("daily_quota=%d rate_per_sec=%d", k.DailyQuota, k.RatePerSec))
	c.JSON(http.StatusOK, gin.H{
		"id":   k.ID,
		"name": k.Name,
		"key":  k.Key, // 明文 key 仅此一次返回（SPEC §5）
	})
}

// updateKey 更新名称/启停。
func (s *Server) updateKey(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req updateKeyReq
	if !bindJSON(c, &req) {
		return
	}
	var k model.ApiKey
	if err := s.db.First(&k, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "API Key 不存在")
			return
		}
		serverError(c, err)
		return
	}
	updates := map[string]any{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
			badRequest(c, "名称不能为空且不超过 64 字符")
			return
		}
		updates["name"] = name
		k.Name = name
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
		k.Enabled = *req.Enabled
		// 手动启用清空联动停用标记（安全加固三件套，C）：管理页「密码重置后停用」
		// 徽标以 enabled=false + disabled_reason 联合判定，启用即恢复正常展示。
		if *req.Enabled {
			updates["disabled_reason"] = ""
			k.DisabledReason = ""
		}
	}
	// 调用方回执配置（D1）：nil = 保留；空串 = 清除（不推送）。URL 校验 http(s)://、
	// 事件白名单校验均在写库前完成，非法请求整体拒绝（不做部分写入）。
	if req.WebhookURL != nil {
		u := strings.TrimSpace(*req.WebhookURL)
		if err := validateWebhookURL(u); err != nil {
			badRequest(c, err.Error())
			return
		}
		updates["webhook_url"] = u
		k.WebhookURL = u
	}
	if req.WebhookEvents != nil {
		ev, err := validateWebhookEvents(*req.WebhookEvents)
		if err != nil {
			badRequest(c, err.Error())
			return
		}
		updates["webhook_events"] = ev
		k.WebhookEvents = ev
	}
	// 调用方维度配额与限速（B5）：nil = 保留；显式 0 = 改为不限；负数整体拒绝
	// （与 webhook 字段同风格：校验在写库前完成，不做部分写入）。
	if req.DailyQuota != nil {
		if *req.DailyQuota < 0 {
			badRequest(c, "daily_quota 不能为负数（0=不限）")
			return
		}
		updates["daily_quota"] = *req.DailyQuota
		k.DailyQuota = *req.DailyQuota
	}
	if req.RatePerSec != nil {
		if *req.RatePerSec < 0 {
			badRequest(c, "rate_per_sec 不能为负数（0=不限）")
			return
		}
		updates["rate_per_sec"] = *req.RatePerSec
		k.RatePerSec = *req.RatePerSec
	}
	// P2-14：显式 Updates 只写请求携带的字段——Save 全字段覆盖会把并发发送
	// 刚更新的 last_used_at 用读取时刻的旧快照抹掉
	if len(updates) > 0 {
		if err := s.db.Model(&model.ApiKey{}).Where("id = ?", k.ID).Updates(updates).Error; err != nil {
			serverError(c, err)
			return
		}
	}
	// 响应结构与列表一致：附该 key 的发送成功总数（单条 COUNT 聚合）
	var totalSent int64
	if err := s.db.Model(&model.SendLog{}).
		Where("api_key_id = ? AND status = ?", k.ID, model.SendStatusSent).
		Count(&totalSent).Error; err != nil {
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录，detail 只记本次请求提供的字段名（值不敏感的
	// webhook_url 等同样不记值——回执地址可能内嵌 token）
	s.audit(c, model.AuditKeyUpdate,
		fmt.Sprintf("key#%d name=%s", k.ID, k.Name),
		joinAuditFields(keyUpdateFields(req)))
	c.JSON(http.StatusOK, newKeyDTO(&k, totalSent))
}

// keyUpdateFields 列出本次请求实际提供的字段名（nil = 未提供）。
func keyUpdateFields(req updateKeyReq) []string {
	var fields []string
	if req.Name != nil {
		fields = append(fields, "name")
	}
	if req.Enabled != nil {
		fields = append(fields, "enabled")
	}
	if req.WebhookURL != nil {
		fields = append(fields, "webhook_url")
	}
	if req.WebhookEvents != nil {
		fields = append(fields, "webhook_events")
	}
	if req.DailyQuota != nil {
		fields = append(fields, "daily_quota")
	}
	if req.RatePerSec != nil {
		fields = append(fields, "rate_per_sec")
	}
	return fields
}

// deleteKey 删除 Key。删除前先读取记录：一并承担 404 判定，并为审计 target
// 提供名称（E4）。
func (s *Server) deleteKey(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var k model.ApiKey
	if err := s.db.First(&k, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "API Key 不存在")
			return
		}
		serverError(c, err)
		return
	}
	if err := s.db.Delete(&model.ApiKey{}, id).Error; err != nil {
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录
	s.audit(c, model.AuditKeyDelete,
		fmt.Sprintf("key#%d name=%s", k.ID, k.Name), "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// newKeyDTO 掩码响应体（totalSent：该 key 的 status=sent 日志总数，含测试发送；
// daily_quota/rate_per_sec 为调用方维度配额与限速，0=不限，B5）。webhook_secret
// 明文回显（管理页查看/复制用，/api 会话保护，口径同设置页 notify_webhook_secret）；
// disabled_reason 供前端展示联动停用徽标。
func newKeyDTO(k *model.ApiKey, totalSent int64) keyDTO {
	return keyDTO{
		ID:             k.ID,
		Name:           k.Name,
		MaskedKey:      maskedAPIKey(k.Key),
		Enabled:        k.Enabled,
		WebhookURL:     k.WebhookURL,
		WebhookEvents:  k.WebhookEvents,
		WebhookSecret:  k.WebhookSecret,
		DailyQuota:     k.DailyQuota,
		RatePerSec:     k.RatePerSec,
		DisabledReason: k.DisabledReason,
		TotalSent:      totalSent,
		LastUsedAt:     k.LastUsedAt,
		CreatedAt:      k.CreatedAt,
	}
}

// resetKeyWebhookSecret POST /api/keys/:id/webhook-secret（安全加固三件套，B）：
// 轮换该 Key 的回执签名密钥（32 字节随机 hex），返回新值供管理页展示/复制。
// 轮换后存量未投递的 outbox 回执任务按新值签名（调用方需同步更新校验密钥）。
// 审计 key_webhook_secret（secret 值不入审计）。
func (s *Server) resetKeyWebhookSecret(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var k model.ApiKey
	if err := s.db.First(&k, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "API Key 不存在")
			return
		}
		serverError(c, err)
		return
	}
	secret, err := randomHex(32)
	if err != nil {
		serverError(c, err)
		return
	}
	if err := s.db.Model(&model.ApiKey{}).Where("id = ?", k.ID).
		Update("webhook_secret", secret).Error; err != nil {
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录；secret 值不入审计
	s.audit(c, model.AuditKeyWebhookSecret,
		fmt.Sprintf("key#%d name=%s", k.ID, k.Name), "action=rotate")
	c.JSON(http.StatusOK, gin.H{"webhook_secret": secret})
}

// ---------- 调用方回执配置（D1） ----------

// validateWebhookURL 校验调用方回执地址：允许空（=不推送），非空必须为可解析的
// http(s):// 地址（含 host）。返回规范化后的值。
func validateWebhookURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("webhook_url 必须是合法的 http(s):// 地址")
	}
	return nil
}

// validateWebhookEvents 校验回执事件白名单：允许空（=不推送），非空必须是
// receipt.Events 的子集（逗号分隔、项内空白容忍、去重），返回规范化 csv。
func validateWebhookEvents(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	seen := make(map[string]bool, len(receipt.Events))
	var out []string
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !receipt.ValidEvent(e) {
			return "", fmt.Errorf("webhook_events 含未知事件 %q（可选：%s）",
				e, strings.Join(receipt.Events, " / "))
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return strings.Join(out, ","), nil
}

// testKeyWebhook POST /api/keys/:id/webhook/test（D1）：向该 Key 已配置的回执
// 地址发送一条测试回执（event="test"，单次尝试不重试、5s 超时），结果供 Key 编辑
// 弹窗直接展示——与 testNotify 同风格：恒 200，成败经 ok/error 字段上报。
// 路由挂在 /api 组内，同时受管理 IP 白名单与会话鉴权保护（见 server.go Register）。
func (s *Server) testKeyWebhook(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var k model.ApiKey
	if err := s.db.First(&k, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "API Key 不存在")
			return
		}
		serverError(c, err)
		return
	}
	if k.WebhookURL == "" {
		badRequest(c, "该 Key 未配置回执 Webhook URL")
		return
	}
	// 按该 Key 已保存的回执签名密钥签名（与 outbox 真实投递同一契约；密钥为空的
	// 存量 Key 不签名不发头）
	if err := receipt.TestWebhook(k.ID, k.WebhookURL, k.WebhookSecret); err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// randomHex 生成 n 字节随机数的 hex 编码。
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
