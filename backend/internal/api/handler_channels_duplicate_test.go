// handler_channels_duplicate_test.go E6 渠道复制集成测试：
//   - 副本字段逐项复制：provider/priority/发件人/SMTP 配置等非秘钥字段原样，
//     secrets（smtp_pass/proxy_url）服务端明文复制（DB 侧验证），响应掩码与原渠道一致；
//   - 副本名 = 原名-副本，冲突依次追加 -2/-3；
//   - 副本默认停用（status=1 manual_disabled），运行态归零
//     （used_day/used_month=0、last_error 清空、consecutive_failures=0、冷却/禁用原因清空）；
//   - 原渠道不受影响；渠道不存在 404。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"catmail/internal/model"
)

// duplicateChannelByAPI 调用复制接口并断言 200，返回响应 DTO。
func duplicateChannelByAPI(t *testing.T, r *gin.Engine, cookie string, id uint) map[string]any {
	t.Helper()
	w := doReq(r, "POST", fmt.Sprintf("/api/channels/%d/duplicate", id), cookie, "", "")
	return decode(t, mustStatus(t, w, http.StatusOK))
}

// TestChannelDuplicateCopiesAllFields 全字段复制 + 运行态归零 + 原渠道不受影响。
func TestChannelDuplicateCopiesAllFields(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"smtp主渠道","provider":"smtp",`+
		`"priority":3,"from_email":"noreply@example.com","from_name":"CatMail",`+
		`"smtp_host":"smtp.example.com","smtp_port":465,"smtp_user":"mailer",`+
		`"smtp_pass":"password123","smtp_tls":"tls",`+
		`"proxy_url":"socks5://user:secret@proxy.example.com:1080",`+
		`"allowed_domains":"example.com,mail.example.org","daily_limit_override":50}`)

	// 模拟运行态：用量/锚点/错误/连续失败/冷却（副本必须全部归零或清空）
	cooldown := time.Now().Add(time.Hour)
	today, month := time.Now().Format("2006-01-02"), time.Now().Format("2006-01")
	if err := db.Model(&model.Channel{}).Where("id = ?", chID).Updates(map[string]any{
		"used_day": 5, "day_anchor": today, "used_month": 7, "month_anchor": month,
		"last_error": "boom", "consecutive_failures": 4,
		"auto_disabled_reason": "连续失败 4 次", "cooldown_until": &cooldown,
		"last_used_at": time.Now(),
	}).Error; err != nil {
		t.Fatalf("seed runtime state: %v", err)
	}

	m := duplicateChannelByAPI(t, r, cookie, uint(chID))

	// 副本名与默认停用
	if m["name"] != "smtp主渠道-副本" {
		t.Fatalf("dup name: %v", m["name"])
	}
	if int(m["status"].(float64)) != model.ChannelManualDisabled {
		t.Fatalf("dup status: %v（副本应默认 manual_disabled）", m["status"])
	}
	// 非秘钥字段逐项复制
	for k, want := range map[string]any{
		"provider": "smtp", "priority": float64(3),
		"from_email": "noreply@example.com", "from_name": "CatMail",
		"smtp_host": "smtp.example.com", "smtp_port": float64(465),
		"smtp_user": "mailer", "smtp_tls": "tls",
		"allowed_domains":      "example.com,mail.example.org",
		"daily_limit_override": float64(50),
	} {
		if got := m[k]; got != want {
			t.Fatalf("dup %s = %v want %v", k, got, want)
		}
	}
	// secrets 掩码回显与原渠道一致（不泄露明文）
	if m["smtp_pass"] != "pass…d123" {
		t.Fatalf("dup smtp_pass mask: %v", m["smtp_pass"])
	}
	if m["proxy_url"] != "socks5://***@proxy.example.com:1080" {
		t.Fatalf("dup proxy_url mask: %v", m["proxy_url"])
	}
	// 运行态归零/清空
	for k, want := range map[string]any{
		"used_day": float64(0), "used_month": float64(0), "consecutive_failures": float64(0),
		"last_error": "", "auto_disabled_reason": "", "cooldown_until": nil,
	} {
		if got := m[k]; got != want {
			t.Fatalf("dup %s = %v want %v", k, got, want)
		}
	}

	// DB 侧：secrets 明文原样复制，ID 全新；原渠道不受影响
	var dup model.Channel
	if err := db.Where("name = ?", "smtp主渠道-副本").First(&dup).Error; err != nil {
		t.Fatalf("dup row missing: %v", err)
	}
	var src model.Channel
	if err := db.First(&src, chID).Error; err != nil {
		t.Fatalf("src row missing: %v", err)
	}
	if dup.ID == src.ID || dup.ID == 0 {
		t.Fatalf("dup id: %d", dup.ID)
	}
	if dup.SMTPPass != src.SMTPPass || src.SMTPPass != "password123" {
		t.Fatalf("dup smtp_pass plaintext: %q want %q", dup.SMTPPass, src.SMTPPass)
	}
	if dup.ProxyURL != src.ProxyURL || src.ProxyURL != "socks5://user:secret@proxy.example.com:1080" {
		t.Fatalf("dup proxy_url plaintext: %q want %q", dup.ProxyURL, src.ProxyURL)
	}
	if dup.SMTPUser != src.SMTPUser || dup.SMTPHost != src.SMTPHost || dup.SMTPPort != src.SMTPPort ||
		dup.FromEmail != src.FromEmail || dup.Priority != src.Priority {
		t.Fatalf("dup config fields differ: %+v", dup)
	}
	if dup.UsedDay != 0 || dup.UsedMonth != 0 || dup.DayAnchor != "" || dup.MonthAnchor != "" {
		t.Fatalf("dup usage not zeroed: %d/%d anchors %q/%q", dup.UsedDay, dup.UsedMonth, dup.DayAnchor, dup.MonthAnchor)
	}
	// 原渠道运行态原样保留
	if src.UsedDay != 5 || src.UsedMonth != 7 || src.LastError != "boom" ||
		src.ConsecutiveFailures != 4 || src.CooldownUntil == nil {
		t.Fatalf("src runtime state altered: %+v", src)
	}
}

// TestChannelDuplicateNameSuffix 重名后缀递增：-副本 → -副本-2 → -副本-3；
// 亦覆盖「-副本 名已被其他渠道占用」时直接跳到 -副本-2 的场景。
func TestChannelDuplicateNameSuffix(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	chID := createChannel(t, r, cookie, `{"name":"mc","provider":"mock"}`)

	if got := duplicateChannelByAPI(t, r, cookie, uint(chID))["name"]; got != "mc-副本" {
		t.Fatalf("1st dup name: %v", got)
	}
	if got := duplicateChannelByAPI(t, r, cookie, uint(chID))["name"]; got != "mc-副本-2" {
		t.Fatalf("2nd dup name: %v", got)
	}
	if got := duplicateChannelByAPI(t, r, cookie, uint(chID))["name"]; got != "mc-副本-3" {
		t.Fatalf("3rd dup name: %v", got)
	}

	// 显式占用 -副本 名后复制 other：直接跳到 other-副本-2
	otherID := uint(createChannel(t, r, cookie, `{"name":"other","provider":"mock"}`))
	createChannel(t, r, cookie, `{"name":"other-副本","provider":"mock"}`)
	if got := duplicateChannelByAPI(t, r, cookie, otherID)["name"]; got != "other-副本-2" {
		t.Fatalf("occupied -副本 name: %v", got)
	}
}

// TestChannelDuplicateMissing 渠道不存在 → 404；未鉴权 → 401。
func TestChannelDuplicateMissing(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	if code := doReq(r, "POST", "/api/channels/9999/duplicate", cookie, "", "").Code; code != http.StatusNotFound {
		t.Fatalf("missing channel: %d", code)
	}
	body := mustStatus(t, doReq(r, "POST", "/api/channels/1/duplicate", "", "", ""), http.StatusUnauthorized)
	if !strings.Contains(body, `"type":"unauthorized"`) {
		t.Fatalf("unauth duplicate: %s", body)
	}
	// 响应为统一错误体（无多余数据拼接）
	if !json.Valid([]byte(body)) {
		t.Fatalf("401 body not a single JSON object: %s", body)
	}
}
