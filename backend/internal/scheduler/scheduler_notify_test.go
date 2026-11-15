// scheduler_notify_test.go 冷却释放的恢复通知（O9）与站内通知保留上限清理：
// 断言逐渠道复位时恰好各发一条 channel_recovered（幂等不重发、白名单过滤生效），
// pruneNotifications 仅保留最近 notificationKeep 条。
package scheduler

import (
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/notify"
)

// enableNotify 开启站内通知通道（可选附带 notify_events 白名单）。
func enableNotify(t *testing.T, db *gorm.DB, events string) {
	t.Helper()
	cols := map[string]any{"notify_inapp_enabled": true, "notify_events": events}
	if err := db.Model(&model.Setting{}).Where("id = ?", 1).Updates(cols).Error; err != nil {
		t.Fatalf("enable notify: %v", err)
	}
}

// allNotifications 取全部通知行（id 升序）。
func allNotifications(t *testing.T, db *gorm.DB) []model.Notification {
	t.Helper()
	var rows []model.Notification
	if err := db.Model(&model.Notification{}).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	return rows
}

// wireNotifier 接线默认发射器指向测试库，测试结束解除（包级状态，串行测试安全）。
func wireNotifier(t *testing.T, db *gorm.DB) {
	t.Helper()
	notify.SetDefault(notify.NewNotifier(db))
	t.Cleanup(func() { notify.SetDefault(nil) })
}

// TestReleaseEmitsRecovered 冷却释放逐渠道发恢复通知：仅被本任务复位的渠道
// 各一条（info 级、title 带渠道名、channel_id 关联），未到期/非冷却渠道不发；
// 重复执行幂等不重发。
func TestReleaseEmitsRecovered(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, 90)
	enableNotify(t, db, "")
	wireNotifier(t, db)

	putChannel(t, db, "到期恢复", model.ChannelCooldown, ptrTime(time.Now().Add(-time.Hour))) // 应复位+发通知
	putChannel(t, db, "冷却未到期", model.ChannelCooldown, ptrTime(time.Now().Add(time.Hour))) // 不动
	putChannel(t, db, "正常渠道", model.ChannelEnabled, ptrTime(time.Now().Add(-time.Hour)))  // 不动

	releaseExpiredCooldowns(db)

	rows := allNotifications(t, db)
	if len(rows) != 1 {
		t.Fatalf("notifications = %d 条, want 1（仅被复位渠道发一条）", len(rows))
	}
	row := rows[0]
	if row.Event != notify.EventChannelRecovered || row.Level != notify.LevelInfo {
		t.Errorf("event/level = %s/%s, want %s/%s", row.Event, row.Level,
			notify.EventChannelRecovered, notify.LevelInfo)
	}
	if row.Title != "渠道「到期恢复」已恢复可用" {
		t.Errorf("title = %q", row.Title)
	}
	if row.ChannelID == nil || *row.ChannelID != 1 {
		t.Errorf("channel_id = %v, want 1", row.ChannelID)
	}
	// 渠道确实被复位
	if ch := getChannel(t, db, 1); ch.Status != model.ChannelEnabled || ch.CooldownUntil != nil {
		t.Errorf("渠道未复位: status=%d until=%v", ch.Status, ch.CooldownUntil)
	}

	// 幂等：再执行一次，不重复发通知
	releaseExpiredCooldowns(db)
	if rows := allNotifications(t, db); len(rows) != 1 {
		t.Fatalf("幂等执行后 notifications = %d 条, want 1", len(rows))
	}
}

// TestReleaseRecoveredEventFilter 事件白名单过滤：notify_events 不含
// channel_recovered 时渠道照常复位，但不落通知。
func TestReleaseRecoveredEventFilter(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, 90)
	enableNotify(t, db, notify.EventSendFailed) // 白名单只放行 send_failed
	wireNotifier(t, db)
	putChannel(t, db, "过滤恢复", model.ChannelCooldown, ptrTime(time.Now().Add(-time.Hour)))

	releaseExpiredCooldowns(db)

	if ch := getChannel(t, db, 1); ch.Status != model.ChannelEnabled {
		t.Errorf("渠道应照常复位: status=%d", ch.Status)
	}
	if rows := allNotifications(t, db); len(rows) != 0 {
		t.Fatalf("白名单外事件不应落库，notifications = %d 条", len(rows))
	}
}

// TestPruneNotifications 站内通知保留上限：超过 notificationKeep 条时仅保留最近
// notificationKeep 条，不足时不动。
func TestPruneNotifications(t *testing.T) {
	db := newDB(t)

	rows := make([]model.Notification, notificationKeep+2)
	for i := range rows {
		rows[i] = model.Notification{
			Event: notify.EventSendFailed, Level: notify.LevelError,
			Title: fmt.Sprintf("n-%d", i),
		}
	}
	if err := db.CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatalf("seed notifications: %v", err)
	}

	pruneNotifications(db)

	var n int64
	if err := db.Model(&model.Notification{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != notificationKeep {
		t.Fatalf("保留 %d 条, want %d", n, notificationKeep)
	}
	// 保留的应是最近的（id 最大）一批
	var firstID uint
	if err := db.Model(&model.Notification{}).Order("id ASC").Limit(1).Pluck("id", &firstID).Error; err != nil {
		t.Fatal(err)
	}
	if firstID != 3 {
		t.Errorf("最旧保留 id = %d, want 3（前 2 条应被清理）", firstID)
	}

	// 不足上限：不再删除
	pruneNotifications(db)
	if err := db.Model(&model.Notification{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != notificationKeep {
		t.Errorf("二次清理后 %d 条, want %d", n, notificationKeep)
	}
}

// TestPruneNotificationsFailSoft 库不可用时软失败（记日志返回，绝不 panic）。
func TestPruneNotificationsFailSoft(t *testing.T) {
	db := newDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	pruneNotifications(db) // 只要不 panic 即视为通过
}
