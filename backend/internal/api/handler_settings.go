// handler_settings.go 系统设置读取与更新（SPEC §5，越界 400）。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// commonWeakPasswords 常见弱口令清单（含首启默认口令 admin123，SPEC §1），
// 用于「初始账号弱口令」提示；命中即停，逐一 bcrypt 比对。
var commonWeakPasswords = []string{
	"admin123", "admin", "123456", "12345678", "123456789",
	"password", "password123", "qwerty", "111111", "888888", "catmail",
}

// firstAccountUsesWeakPassword 检查最早创建的账号（首启种子 admin）的口令是否命中
// 常见弱口令清单，供设置页提示修改。bcrypt 比对较贵（约几十 ms/次），结果按口令
// 哈希缓存在进程内：改密后哈希变化自然失效重算，删除账号后不再参与查询。
func (s *Server) firstAccountUsesWeakPassword() bool {
	var u model.User
	if err := s.db.Order("id ASC").First(&u).Error; err != nil {
		return false // 无账号（删除保护下不可达）或查询失败：静默不作提示
	}
	if v, ok := s.weakPwCache.Load(u.PasswordHash); ok {
		return v.(bool)
	}
	weak := false
	for _, p := range commonWeakPasswords {
		if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(p)) == nil {
			weak = true
			break
		}
	}
	s.weakPwCache.Store(u.PasswordHash, weak)
	return weak
}

// settingsResp GET /api/settings 响应体：在 settingsDTO 基础上附加弱口令提示位
// （settingsDTO 同时用作 PUT 请求体，故 GET 响应结构在此局部扩展）。
type settingsResp struct {
	settingsDTO
	WeakAdminPassword bool `json:"weak_admin_password"`
}

// getSettings GET /api/settings（SessionSecret 不对外暴露）。
func (s *Server) getSettings(c *gin.Context) {
	var st model.Setting
	err := s.db.First(&st, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 行缺失时返回默认值（与 SPEC §4 种子数据一致；通知双通道默认关闭，
		// 日志正文开关默认开启=保持现状，每日自动备份默认关闭；抑制自动入库
		// 默认开启、退信率阈值默认 5%——送达链路三件套口径）
		c.JSON(http.StatusOK, settingsResp{
			settingsDTO: settingsDTO{
				Rotation: "priority", MaxAttempts: 3, CooldownSeconds: 60,
				ConsecutiveFailureThreshold: 5, LogRetentionDays: 90,
				NotifyEvents: "", LogBodyEnabled: true,
				SuppressionAutoEnabled: true, BounceRateThreshold: 5,
			},
			WeakAdminPassword: s.firstAccountUsesWeakPassword(),
		})
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, settingsResp{
		settingsDTO: settingsDTO{
			Rotation:                    st.Rotation,
			MaxAttempts:                 st.MaxAttempts,
			CooldownSeconds:             st.CooldownSeconds,
			ConsecutiveFailureThreshold: st.ConsecutiveFailureThreshold,
			LogRetentionDays:            st.LogRetentionDays,
			NotifyInappEnabled:          st.NotifyInappEnabled,
			NotifyWebhookEnabled:        st.NotifyWebhookEnabled,
			NotifyWebhookURL:            st.NotifyWebhookURL,
			NotifyWebhookSecret:         st.NotifyWebhookSecret,
			NotifyEvents:                st.NotifyEvents,
			LogBodyEnabled:              st.LogBodyEnabled,
			BackupDailyEnabled:          st.BackupDailyEnabled,
			SuppressionAutoEnabled:      st.SuppressionAutoEnabled,
			BounceRateThreshold:         st.BounceRateThreshold,
		},
		WeakAdminPassword: s.firstAccountUsesWeakPassword(),
	})
}

// putSettings PUT /api/settings：逐项范围校验（SPEC §5）。
func (s *Server) putSettings(c *gin.Context) {
	var req settingsDTO
	if !bindJSON(c, &req) {
		return
	}
	switch {
	case req.Rotation != "priority" && req.Rotation != "round_robin":
		badRequest(c, "rotation 仅支持 priority/round_robin")
		return
	case req.MaxAttempts < 1 || req.MaxAttempts > 10:
		badRequest(c, "max_attempts 取值范围 1-10")
		return
	case req.CooldownSeconds < 1:
		badRequest(c, "cooldown_seconds 必须 ≥1")
		return
	case req.ConsecutiveFailureThreshold < 3 || req.ConsecutiveFailureThreshold > 20:
		badRequest(c, "consecutive_failure_threshold 取值范围 3-20")
		return
	case req.LogRetentionDays < 0 || req.LogRetentionDays > 365:
		badRequest(c, "log_retention_days 取值范围 0-365（0=永久保留）")
		return
	case req.BounceRateThreshold < 0 || req.BounceRateThreshold > 100:
		badRequest(c, "bounce_rate_threshold 取值范围 0-100（百分比，0=关闭告警）")
		return
	}
	// 失败通知设置（O9）：webhook 启用时地址必须为 http(s):// 形式（与前端
	// 校验同口径：启用必填）；notify_events 仅允许五事件 token 的逗号子集
	// （E8 新增 update_available，枚举见 handler_notifications.go notifyEventTokens），
	// 空串/缺省规范化为全事件串（前端契约：空=全部）
	if req.NotifyWebhookEnabled {
		u, err := url.Parse(strings.TrimSpace(req.NotifyWebhookURL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			badRequest(c, "notify_webhook_url 必须为 http(s):// 形式")
			return
		}
	}
	events, eerr := normalizeNotifyEvents(req.NotifyEvents)
	if eerr != nil {
		badRequest(c, eerr.Error())
		return
	}
	req.NotifyEvents = events
	req.NotifyWebhookURL = strings.TrimSpace(req.NotifyWebhookURL)

	var st model.Setting
	err := s.db.First(&st, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 种子缺失时补建（P2-10）：生成随机会话签名密钥——空密钥的 HMAC 任何人
		// 可伪造，等于无签名防护
		secret, e := randomHex(32)
		if e != nil {
			serverError(c, e)
			return
		}
		st = model.Setting{ID: 1, SessionSecret: secret}
	} else if err != nil {
		serverError(c, err)
		return
	}
	// 审计（E4）：保存前快照旧值，落库成功后对比得出变更字段名清单（不含值）
	oldSettings := settingsDTO{
		Rotation:                    st.Rotation,
		MaxAttempts:                 st.MaxAttempts,
		CooldownSeconds:             st.CooldownSeconds,
		ConsecutiveFailureThreshold: st.ConsecutiveFailureThreshold,
		LogRetentionDays:            st.LogRetentionDays,
		NotifyInappEnabled:          st.NotifyInappEnabled,
		NotifyWebhookEnabled:        st.NotifyWebhookEnabled,
		NotifyWebhookURL:            st.NotifyWebhookURL,
		NotifyEvents:                st.NotifyEvents,
		LogBodyEnabled:              st.LogBodyEnabled,
		BackupDailyEnabled:          st.BackupDailyEnabled,
		SuppressionAutoEnabled:      st.SuppressionAutoEnabled,
		BounceRateThreshold:         st.BounceRateThreshold,
	}
	st.Rotation = req.Rotation
	st.MaxAttempts = req.MaxAttempts
	st.CooldownSeconds = req.CooldownSeconds
	st.ConsecutiveFailureThreshold = req.ConsecutiveFailureThreshold
	st.LogRetentionDays = req.LogRetentionDays
	st.NotifyInappEnabled = req.NotifyInappEnabled
	st.NotifyWebhookEnabled = req.NotifyWebhookEnabled
	st.NotifyWebhookURL = req.NotifyWebhookURL
	st.NotifyEvents = req.NotifyEvents
	st.LogBodyEnabled = req.LogBodyEnabled
	st.BackupDailyEnabled = req.BackupDailyEnabled
	st.SuppressionAutoEnabled = req.SuppressionAutoEnabled
	st.BounceRateThreshold = req.BounceRateThreshold
	// NotifyWebhookSecret 刻意不取 req（PUT 忽略该字段）：签名密钥仅经
	// POST /api/settings/notify/webhook-secret 生成/轮换，防整表单保存误清；
	// st 从库加载未触碰该列，Save 原样保留。
	st.UpdatedAt = time.Now()
	if err := s.db.Save(&st).Error; err != nil {
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录，detail 只含变更字段名清单，任何字段值（含
	// notify_webhook_url 等可能内嵌 token 的地址）不入审计
	s.audit(c, model.AuditSettingsUpdate, "settings#1",
		joinAuditFields(changedSettingFields(oldSettings, req)))
	// 响应回服务端保存后的完整设置（含 notify_webhook_secret 现值）：前端以响应
	// 重置表单/缓存，PUT 忽略 secret 的语义对调用方透明。
	c.JSON(http.StatusOK, settingsResp{
		settingsDTO: settingsDTO{
			Rotation:                    st.Rotation,
			MaxAttempts:                 st.MaxAttempts,
			CooldownSeconds:             st.CooldownSeconds,
			ConsecutiveFailureThreshold: st.ConsecutiveFailureThreshold,
			LogRetentionDays:            st.LogRetentionDays,
			NotifyInappEnabled:          st.NotifyInappEnabled,
			NotifyWebhookEnabled:        st.NotifyWebhookEnabled,
			NotifyWebhookURL:            st.NotifyWebhookURL,
			NotifyWebhookSecret:         st.NotifyWebhookSecret,
			NotifyEvents:                st.NotifyEvents,
			LogBodyEnabled:              st.LogBodyEnabled,
			BackupDailyEnabled:          st.BackupDailyEnabled,
			SuppressionAutoEnabled:      st.SuppressionAutoEnabled,
			BounceRateThreshold:         st.BounceRateThreshold,
		},
		WeakAdminPassword: s.firstAccountUsesWeakPassword(),
	})
}

// rotateWebhookSecretReq 请求体：rotate=true 强制轮换（否则仅在 secret 为空时
// 生成——存量升级后首次调用即补齐）。请求体整体可选（空 body = 生成语义）。
type rotateWebhookSecretReq struct {
	Rotate bool `json:"rotate"`
}

// rotateNotifyWebhookSecret POST /api/settings/notify/webhook-secret（安全加固
// 三件套，B）：生成或轮换通知 Webhook 签名密钥（32 字节随机 hex），返回新值供
// 设置卡片展示/复制。轮换后存量未投递的 outbox 任务按新值签名（接收方需同步
// 更新校验密钥）。审计 settings_webhook_secret（action=ensure|rotate，secret 值
// 不入审计）。
func (s *Server) rotateNotifyWebhookSecret(c *gin.Context) {
	var req rotateWebhookSecretReq
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
	var st model.Setting
	err := s.db.First(&st, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 设置行缺失时补建（与 putSettings 同口径）：先生成随机会话密钥占位
		secret, e := randomHex(32)
		if e != nil {
			serverError(c, e)
			return
		}
		st = model.Setting{ID: 1, SessionSecret: secret}
	} else if err != nil {
		serverError(c, err)
		return
	}
	if st.NotifyWebhookSecret == "" || req.Rotate {
		secret, gerr := randomHex(32)
		if gerr != nil {
			serverError(c, gerr)
			return
		}
		if err := s.db.Model(&model.Setting{}).Where("id = ?", st.ID).
			Update("notify_webhook_secret", secret).Error; err != nil {
			serverError(c, err)
			return
		}
		st.NotifyWebhookSecret = secret
		// 审计（E4）：仅成功路径记录；secret 值不入审计
		detail := "action=ensure"
		if req.Rotate {
			detail = "action=rotate"
		}
		s.audit(c, model.AuditSettingsWebhookSecret, "settings#1", detail)
	}
	c.JSON(http.StatusOK, gin.H{"webhook_secret": st.NotifyWebhookSecret})
}
