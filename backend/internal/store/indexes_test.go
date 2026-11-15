package store

import (
	"reflect"
	"testing"

	"catmail/internal/model"
)

// TestSendLogIndexes 校验 SendLog 常用查询列的索引齐全（SPEC §8）：
// 日志列表筛选（status/channel_id）、时间排序与清理（created_at）、
// Key 累计发送聚合（api_key_id）、message_id 贯穿查询均走索引，
// 耗时排序走 (latency_ms, created_at, id) 组合索引（L5）。
func TestSendLogIndexes(t *testing.T) {
	db := openSeeded(t)
	m := db.Migrator()
	for _, idx := range []string{
		"idx_send_logs_created_at",
		"idx_send_logs_status",
		"idx_send_logs_channel_id",
		"idx_send_logs_api_key_id",
		"idx_send_logs_message_id",
		"idx_send_logs_latency",
	} {
		if !m.HasIndex(&model.SendLog{}, idx) {
			t.Errorf("send_logs 缺少索引 %s", idx)
		}
	}
}

// TestSendLogLatencyIndexOrder (latency_ms, created_at, id) 组合索引的列序
// （L5）：耗时排序（ORDER BY latency_ms, created_at, id）只有按该列序建索引
// 才能直接走索引顺序，列序错误则索引退化为无效。
func TestSendLogLatencyIndexOrder(t *testing.T) {
	db := openSeeded(t)
	idxs, err := db.Migrator().GetIndexes(&model.SendLog{})
	if err != nil {
		t.Fatalf("get indexes: %v", err)
	}
	for _, idx := range idxs {
		if idx.Name() != "idx_send_logs_latency" {
			continue
		}
		want := []string{"latency_ms", "created_at", "id"}
		if got := idx.Columns(); !reflect.DeepEqual(got, want) {
			t.Fatalf("idx_send_logs_latency 列序 = %v, want %v", got, want)
		}
		return
	}
	t.Fatal("send_logs 缺少索引 idx_send_logs_latency")
}

// TestNotificationIndexes 校验 Notification 的 (read, created_at) 组合索引与
// channel_id 索引：通知中心「已读过滤 + 时间倒序」与按渠道筛选走索引（取舍见模型注释）。
func TestNotificationIndexes(t *testing.T) {
	db := openSeeded(t)
	m := db.Migrator()
	for _, idx := range []string{
		"idx_notifications_read_created_at",
		"idx_notifications_channel_id",
	} {
		if !m.HasIndex(&model.Notification{}, idx) {
			t.Errorf("notifications 缺少索引 %s", idx)
		}
	}
}
