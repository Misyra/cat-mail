// rotator_notify_test.go 失败通知埋点（O9）：三类事件（渠道禁用/冷却/终态发送失败）
// 经 mock 魔法值与额度抢占竞态触发，断言 Notification 落库字段、事件白名单过滤与
// 站内开关生效。包级 notify.Emit 走默认发射器：用例内 SetDefault 接线测试库，结束解除。
package rotator

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/notify"
	"catmail/internal/provider"
)

// wireNotifier 接线默认发射器指向测试库，测试结束解除（包级状态，串行测试安全）。
func wireNotifier(t *testing.T, db *gorm.DB) {
	t.Helper()
	notify.SetDefault(notify.NewNotifier(db))
	t.Cleanup(func() { notify.SetDefault(nil) })
}

// enableInapp 开启站内通知通道（Setting 行 ID=1）。
func enableInapp(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Model(&model.Setting{}).Where("id = ?", 1).
		Update("notify_inapp_enabled", true).Error; err != nil {
		t.Fatalf("enable inapp: %v", err)
	}
}

// notifyRows 按 event 取通知行（按 id 升序）。
func notifyRows(t *testing.T, db *gorm.DB, event string) []model.Notification {
	t.Helper()
	var rows []model.Notification
	if err := db.Where("event = ?", event).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	return rows
}

// notifyTotal 统计通知总条数。
func notifyTotal(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.Notification{}).Count(&n).Error; err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// mockInput 构造走真实 mock 适配器（provider.Get，无 Resolver）的发送输入：
// 4 个收件人供「收件人前 3 个」预览断言。
func mockInput() *Input {
	return &Input{
		Message: &provider.Message{
			FromAddr: "noreply@example.com",
			To:       []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"},
			Subject:  "通知埋点断言主题",
			Text:     "hello",
		},
		APIKeyID: 7,
	}
}

// TestNotifyAuthDisabled 401 禁用路径：渠道 AUTO_DISABLED 且落 channel_disabled
// 通知（error 级，title/body/channel_id 齐全）；同请求终态另落一条 send_failed。
func TestNotifyAuthDisabled(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	enableInapp(t, db)
	wireNotifier(t, db)
	addChannel(t, db, &model.Channel{Name: "认证挂了", Provider: "mock", APIKey: "fail_auth"})

	out, rerr := Send(context.Background(), db, mockInput())
	if out != nil || rerr == nil || rerr.HTTPStatus != 502 {
		t.Fatalf("send: out=%v rerr=%+v", out, rerr)
	}
	ch := reloadChannel(t, db, "认证挂了")
	if ch.Status != model.ChannelAutoDisabled {
		t.Fatalf("status = %d, want auto_disabled", ch.Status)
	}

	rows := notifyRows(t, db, notify.EventChannelDisabled)
	if len(rows) != 1 {
		t.Fatalf("channel_disabled rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Level != notify.LevelError {
		t.Errorf("level = %q, want error", row.Level)
	}
	if !strings.Contains(row.Title, "认证挂了") || !strings.Contains(row.Title, "已自动禁用") {
		t.Errorf("title = %q", row.Title)
	}
	if !strings.Contains(row.Body, "平台认证/授权失败") ||
		!strings.Contains(row.Body, "mock: simulated auth failure") {
		t.Errorf("body = %q", row.Body)
	}
	if row.ChannelID == nil || *row.ChannelID != ch.ID {
		t.Errorf("channel_id = %v, want %d", row.ChannelID, ch.ID)
	}
	if row.Read {
		t.Error("新通知 Read 应为 false")
	}

	// 同一请求候选穷尽 → 终态 send_failed 恰好一条
	if sf := notifyRows(t, db, notify.EventSendFailed); len(sf) != 1 {
		t.Fatalf("send_failed rows = %d, want 1", len(sf))
	}
}

// TestNotifyCooldown 429 冷却路径：渠道进 COOLDOWN 且落 channel_cooldown 通知
// （warn 级，body 含冷却秒数，channel_id 指向该渠道）。
func TestNotifyCooldown(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 90, 5)
	enableInapp(t, db)
	wireNotifier(t, db)
	addChannel(t, db, &model.Channel{Name: "限流渠道", Provider: "mock", APIKey: "fail_rate_limit"})

	out, rerr := Send(context.Background(), db, mockInput())
	if out != nil || rerr == nil || rerr.HTTPStatus != 502 {
		t.Fatalf("send: out=%v rerr=%+v", out, rerr)
	}
	ch := reloadChannel(t, db, "限流渠道")
	if ch.Status != model.ChannelCooldown || ch.CooldownUntil == nil {
		t.Fatalf("status/until = %d / %v, want cooldown", ch.Status, ch.CooldownUntil)
	}

	rows := notifyRows(t, db, notify.EventChannelCooldown)
	if len(rows) != 1 {
		t.Fatalf("channel_cooldown rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Level != notify.LevelWarn {
		t.Errorf("level = %q, want warn", row.Level)
	}
	if !strings.Contains(row.Title, "限流渠道") || !strings.Contains(row.Title, "进入冷却") {
		t.Errorf("title = %q", row.Title)
	}
	if !strings.Contains(row.Body, "平台限流") || !strings.Contains(row.Body, "90") {
		t.Errorf("body = %q（应含平台限流与冷却 90 秒）", row.Body)
	}
	if row.ChannelID == nil || *row.ChannelID != ch.ID {
		t.Errorf("channel_id = %v, want %d", row.ChannelID, ch.ID)
	}
}

// TestNotifySendFailed502 候选穷尽路径：单次可重试失败（未达禁用阈值）→ 502，
// 落 send_failed 通知：message_id 与失败 SendLog 贯穿、主题截 100 字、收件人前 3 个、
// 已试渠道数、最后错误；channel_id 为空；不产生 channel_disabled。
func TestNotifySendFailed502(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 1, 60, 5) // threshold=5：单次失败不禁用
	enableInapp(t, db)
	wireNotifier(t, db)
	addChannel(t, db, &model.Channel{Name: "抖动渠道", Provider: "mock", APIKey: "fail"})

	longSubject := strings.Repeat("主", 150)
	in := mockInput()
	in.Message.Subject = longSubject
	out, rerr := Send(context.Background(), db, in)
	if out != nil || rerr == nil || rerr.HTTPStatus != 502 {
		t.Fatalf("send: out=%v rerr=%+v", out, rerr)
	}
	if ch := reloadChannel(t, db, "抖动渠道"); ch.Status != model.ChannelEnabled || ch.ConsecutiveFailures != 1 {
		t.Fatalf("channel after retryable failure: status=%d cf=%d", ch.Status, ch.ConsecutiveFailures)
	}

	rows := notifyRows(t, db, notify.EventSendFailed)
	if len(rows) != 1 {
		t.Fatalf("send_failed rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Level != notify.LevelError {
		t.Errorf("level = %q, want error", row.Level)
	}
	if row.ChannelID != nil {
		t.Errorf("send_failed channel_id = %v, want nil（请求级事件不指向单渠道）", row.ChannelID)
	}
	// message_id 贯穿：body 中的 uuid 必须命中失败 SendLog
	m := regexp.MustCompile(`message_id: ([0-9a-f-]+)`).FindStringSubmatch(row.Body)
	if m == nil {
		t.Fatalf("body missing message_id: %q", row.Body)
	}
	var lg model.SendLog
	if err := db.Where("message_id = ? AND status = ?", m[1], model.SendStatusFailed).First(&lg).Error; err != nil {
		t.Fatalf("body message_id %q 没有对应失败 SendLog: %v", m[1], err)
	}
	// 主题截 100 字 + 省略号
	if !strings.Contains(row.Body, "主题: "+strings.Repeat("主", 100)+"…") {
		t.Errorf("subject 未按 100 字截断: %q", row.Body)
	}
	if strings.Contains(row.Body, strings.Repeat("主", 150)) {
		t.Errorf("subject 未截断（150 字全量出现）: %q", row.Body)
	}
	// 收件人前 3 个 + 总数注记
	if !strings.Contains(row.Body, "收件人: a@example.com, b@example.com, c@example.com 等 4 个") {
		t.Errorf("recipients preview: %q", row.Body)
	}
	if !strings.Contains(row.Body, "已试渠道: 1 个") {
		t.Errorf("tried count: %q", row.Body)
	}
	if !strings.Contains(row.Body, "mock: simulated provider failure") {
		t.Errorf("last error: %q", row.Body)
	}
	// 可重试失败未达阈值：不落 channel_disabled
	if rows := notifyRows(t, db, notify.EventChannelDisabled); len(rows) != 0 {
		t.Fatalf("channel_disabled rows = %d, want 0", len(rows))
	}
}

// TestNotifySendFailedQuotaRaced tried==0 路径：全部候选输在额度预留环节（并发
// 竞态语义）→ 429，同样落 send_failed（已试渠道 0 个）。用 BEFORE UPDATE 触发器
// 让任何递增 used_day 的 UPDATE 失败，确定性模拟预留抢占必败。
func TestNotifySendFailedQuotaRaced(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	enableInapp(t, db)
	wireNotifier(t, db)
	addChannel(t, db, &model.Channel{Name: "满额渠道", Provider: "mock"})
	if err := db.Exec(`CREATE TRIGGER block_quota_before_update BEFORE UPDATE ON channels
		WHEN NEW.used_day > OLD.used_day BEGIN SELECT RAISE(ABORT, 'blocked'); END`).Error; err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	out, rerr := Send(context.Background(), db, mockInput())
	if out != nil || rerr == nil || rerr.HTTPStatus != 429 {
		t.Fatalf("send: out=%v rerr=%+v（tried==0 应映射 429）", out, rerr)
	}

	rows := notifyRows(t, db, notify.EventSendFailed)
	if len(rows) != 1 {
		t.Fatalf("send_failed rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if !strings.Contains(row.Body, "已试渠道: 0 个") {
		t.Errorf("tried count: %q", row.Body)
	}
	if !strings.Contains(row.Body, "所有候选渠道的额度已耗尽") {
		t.Errorf("last error: %q", row.Body)
	}
}

// TestNotifyEventFilter 事件白名单：notify_events 只放行 send_failed 时，
// channel_disabled 被过滤、send_failed 照常落库。
func TestNotifyEventFilter(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	enableInapp(t, db)
	if err := db.Model(&model.Setting{}).Where("id = ?", 1).
		Update("notify_events", notify.EventSendFailed).Error; err != nil {
		t.Fatalf("set notify_events: %v", err)
	}
	wireNotifier(t, db)
	addChannel(t, db, &model.Channel{Name: "过滤渠道", Provider: "mock", APIKey: "fail_auth"})

	out, rerr := Send(context.Background(), db, mockInput())
	if out != nil || rerr == nil || rerr.HTTPStatus != 502 {
		t.Fatalf("send: out=%v rerr=%+v", out, rerr)
	}

	if rows := notifyRows(t, db, notify.EventChannelDisabled); len(rows) != 0 {
		t.Fatalf("白名单外事件不应落库，channel_disabled rows = %d", len(rows))
	}
	if sf := notifyRows(t, db, notify.EventSendFailed); len(sf) != 1 {
		t.Fatalf("白名单内事件应落库，send_failed rows = %d", len(sf))
	}
}

// TestNotifyInappOffNoRows 站内通道关闭：事件照常发射但不落任何 Notification。
func TestNotifyInappOffNoRows(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5) // notify_inapp_enabled 零值=false
	wireNotifier(t, db)
	addChannel(t, db, &model.Channel{Name: "静默渠道", Provider: "mock", APIKey: "fail_auth"})

	out, rerr := Send(context.Background(), db, mockInput())
	if out != nil || rerr == nil || rerr.HTTPStatus != 502 {
		t.Fatalf("send: out=%v rerr=%+v", out, rerr)
	}
	if got := notifyTotal(t, db); got != 0 {
		t.Fatalf("站内关闭时落库 %d 条, want 0", got)
	}
}
