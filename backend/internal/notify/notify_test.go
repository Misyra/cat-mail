// notify_test.go 事件白名单、站内落库、outbox 入队投递与 PruneOld 单测；建库方式同 store_test.go。
package notify

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/outbox"
	"catmail/internal/store"
)

// openDB 在临时目录打开 DB 并完成首启初始化（含 Setting 默认行）；测试结束自动清理。
func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "catmail.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := store.EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}
	return db
}

// setNotify 更新 Setting 行（ID=1）的通知字段。
func setNotify(t *testing.T, db *gorm.DB, cols map[string]any) {
	t.Helper()
	if err := db.Model(&model.Setting{}).Where("id = ?", 1).Updates(cols).Error; err != nil {
		t.Fatalf("update setting: %v", err)
	}
}

// notifCount 统计站内通知条数。
func notifCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var c int64
	if err := db.Model(&model.Notification{}).Count(&c).Error; err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return c
}

// outboxPendingCount 统计 pending 状态的 outbox 任务条数。
func outboxPendingCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var c int64
	if err := db.Model(&model.WebhookDelivery{}).Where("status = ?", model.OutboxStatusPending).Count(&c).Error; err != nil {
		t.Fatalf("count outbox pending: %v", err)
	}
	return c
}

// flushOutbox 驱动一轮 outbox 投递（对齐生产 scheduler 路径）。
func flushOutbox(t *testing.T, db *gorm.DB) outbox.Stats {
	t.Helper()
	return outbox.ProcessDue(db)
}

// TestEventEnabled 白名单判定：空串=全部开启；csv 子集按字面匹配；列表外事件关闭。
func TestEventEnabled(t *testing.T) {
	cases := []struct {
		csv, event string
		want       bool
	}{
		{"", EventSendFailed, true},                                                 // 空串=全部
		{"   ", EventSendFailed, true},                                              // 仅空白同空串
		{EventChannelCooldown + "," + EventSendFailed, EventSendFailed, true},       // 子集命中
		{EventChannelCooldown + "," + EventSendFailed, EventChannelDisabled, false}, // 未列入
		{EventSendFailed, EventSendFailed, true},                                    // 单事件
		{" " + EventSendFailed + " ", EventSendFailed, true},                        // 项内空白容忍
		{"unknown_event", EventSendFailed, false},                                   // 未知事件名不匹配
		{",", EventSendFailed, false},                                               // 空项不误匹配
	}
	for _, tc := range cases {
		if got := EventEnabled(tc.csv, tc.event); got != tc.want {
			t.Errorf("EventEnabled(%q, %q) = %v, want %v", tc.csv, tc.event, got, tc.want)
		}
	}
}

// TestEmitInapp 站内通道：默认关闭不落库；开启后落库且字段齐全；
// 白名单外事件被过滤，空白名单=全部放行；webhook 关闭时无 outbox 任务。
func TestEmitInapp(t *testing.T) {
	db := openDB(t)
	n := NewNotifier(db)
	cid := uint(3)

	// 默认设置（双通道全关）：不落库
	n.Emit(EventChannelDisabled, LevelError, "渠道禁用", "连续失败 5 次", &cid)
	if got := notifCount(t, db); got != 0 {
		t.Fatalf("默认关闭时落库 %d 条，want 0", got)
	}

	setNotify(t, db, map[string]any{"notify_inapp_enabled": true})
	n.Emit(EventChannelDisabled, LevelError, "渠道禁用", "连续失败 5 次", &cid)
	if got := notifCount(t, db); got != 1 {
		t.Fatalf("开启后落库 %d 条，want 1", got)
	}
	var row model.Notification
	if err := db.First(&row, 1).Error; err != nil {
		t.Fatalf("read notification: %v", err)
	}
	if row.Event != EventChannelDisabled || row.Level != LevelError ||
		row.Title != "渠道禁用" || row.Body != "连续失败 5 次" {
		t.Errorf("字段不匹配: %+v", row)
	}
	if row.ChannelID == nil || *row.ChannelID != 3 {
		t.Errorf("ChannelID = %v, want 3", row.ChannelID)
	}
	if row.Read {
		t.Error("新通知 Read 应为 false")
	}

	// 白名单外事件被过滤
	setNotify(t, db, map[string]any{"notify_events": EventChannelCooldown + "," + EventSendFailed})
	n.Emit(EventChannelDisabled, LevelError, "x", "y", nil)
	if got := notifCount(t, db); got != 1 {
		t.Fatalf("白名单外事件不应落库，当前 %d 条", got)
	}
	n.Emit(EventSendFailed, LevelError, "x", "y", nil)
	if got := notifCount(t, db); got != 2 {
		t.Fatalf("白名单内事件应落库，当前 %d 条", got)
	}

	// 空白名单=全部事件
	setNotify(t, db, map[string]any{"notify_events": ""})
	n.Emit(EventChannelRecovered, LevelInfo, "x", "y", nil)
	if got := notifCount(t, db); got != 3 {
		t.Fatalf("空白名单应放行，当前 %d 条", got)
	}

	// 全程 webhook 关闭：无 outbox 任务
	if got := outboxPendingCount(t, db); got != 0 {
		t.Errorf("webhook 关闭时 outbox 任务数 = %d, want 0", got)
	}
}

// TestEmitEnqueuesOutbox webhook 通道改 outbox 落库投递（安全加固三件套，B）：
// Emit 落 pending 任务（secret 引用指向 Setting），驱动一轮投递全部送达；
// 任务即时持久化（无进程内队列，重启由 scheduler 续投）。
func TestEmitEnqueuesOutbox(t *testing.T) {
	db := openDB(t)
	srv, bodies := newCaptureServer(t, http.StatusOK)
	setNotify(t, db, map[string]any{"notify_webhook_enabled": true, "notify_webhook_url": srv.URL})

	n := NewNotifier(db)
	for i := 0; i < 10; i++ {
		n.Emit(EventSendFailed, LevelWarn, "t", "b", nil)
	}
	if got := outboxPendingCount(t, db); got != 10 {
		t.Fatalf("Emit 后 outbox pending 任务 = %d, want 10（入队即持久化）", got)
	}
	st := flushOutbox(t, db)
	if st.Sent != 10 {
		t.Fatalf("投递 sent = %d, want 10（stats=%+v）", st.Sent, st)
	}
	if got := len(bodies()); got != 10 {
		t.Fatalf("收到 %d 个 webhook，want 10", got)
	}
}

// TestGenericWebhookViaEmit 包级 Emit 全链路：webhook 通用 payload 逐字段 + 站内同步落库。
func TestGenericWebhookViaEmit(t *testing.T) {
	db := openDB(t)
	srv, bodies := newCaptureServer(t, http.StatusOK)
	setNotify(t, db, map[string]any{
		"notify_inapp_enabled":   true,
		"notify_webhook_enabled": true,
		"notify_webhook_url":     srv.URL,
	})

	n := NewNotifier(db)
	SetDefault(n)
	t.Cleanup(func() { SetDefault(nil) })
	cid := uint(7)
	Emit(EventSendFailed, LevelError, "发送失败", "所有渠道尝试失败", &cid)
	Emit(EventChannelCooldown, LevelWarn, "渠道冷却", "示例渠道进入冷却", nil)
	flushOutbox(t, db)

	got := bodies()
	if len(got) != 2 {
		t.Fatalf("收到 %d 个 webhook，want 2", len(got))
	}
	b := got[0]
	if b["source"] != "catmail" {
		t.Errorf("source = %v, want catmail", b["source"])
	}
	if b["event"] != EventSendFailed || b["level"] != LevelError {
		t.Errorf("event/level = %v/%v, want %s/%s", b["event"], b["level"], EventSendFailed, LevelError)
	}
	if b["title"] != "发送失败" || b["body"] != "所有渠道尝试失败" {
		t.Errorf("title/body = %v/%v", b["title"], b["body"])
	}
	if b["channel_id"] != float64(7) {
		t.Errorf("channel_id = %v, want 7", b["channel_id"])
	}
	at, err := time.Parse(time.RFC3339, b["at"].(string))
	if err != nil {
		t.Errorf("at 非法 RFC3339: %v", err)
	} else if d := time.Since(at); d < -time.Minute || d > time.Minute {
		t.Errorf("at 偏离当前时间: %v", d)
	}
	if got[1]["channel_id"] != nil {
		t.Errorf("无渠道事件 channel_id = %v, want null", got[1]["channel_id"])
	}

	// 站内通道同步落库（2 条）
	if c := notifCount(t, db); c != 2 {
		t.Errorf("站内通知 %d 条, want 2", c)
	}
}

// TestNilDefaultEmitNoop 未 SetDefault 时包级 Emit 与 nil 接收者均 no-op，不 panic。
func TestNilDefaultEmitNoop(t *testing.T) {
	SetDefault(nil)
	Emit(EventSendFailed, LevelError, "t", "b", nil) // 不应 panic
	var n *Notifier
	n.Emit(EventSendFailed, LevelError, "t", "b", nil)
}

// TestPruneOld 保留最近 keep 条；keep<=0 或不足 keep 条时不动数据。
func TestPruneOld(t *testing.T) {
	db := openDB(t)
	for i := 0; i < 5; i++ {
		if err := db.Create(&model.Notification{Event: EventSendFailed, Level: LevelWarn, Title: "t"}).Error; err != nil {
			t.Fatalf("seed notification: %v", err)
		}
	}
	deleted, err := PruneOld(db, 3)
	if err != nil {
		t.Fatalf("PruneOld: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	var ids []uint
	if err := db.Model(&model.Notification{}).Order("id").Pluck("id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != 3 || ids[2] != 5 {
		t.Errorf("剩余 id = %v, want [3 4 5]（最近 3 条）", ids)
	}
	if deleted, _ := PruneOld(db, 10); deleted != 0 {
		t.Errorf("不足 keep 条时 deleted = %d, want 0", deleted)
	}
	if deleted, _ := PruneOld(db, 0); deleted != 0 {
		t.Errorf("keep=0 时 deleted = %d, want 0", deleted)
	}
}
