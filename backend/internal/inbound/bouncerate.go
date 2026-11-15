// bouncerate.go 渠道退信率告警（送达链路三件套）：bounced 事件摄取后计算该渠道
// 近 7 天退信率，超过 Setting.BounceRateThreshold（默认 5%，0=关闭）时经 notify
// 广播 bounce_rate_exceeded warn 事件（双通道：站内 + Webhook；开关走 notify_events
// 事件白名单）。
//
// 口径与渠道健康度（api 层 channelHealthMap）一致：窗口=本地日历日近 7 天（含
// 今天），退信率 = bounced / (delivered+bounced+complained)——终态事件内的占比，
// 避开「sent 事件可选上报」导致分母缺失的平台差异。
//
// 防风暴：进程内按渠道 1 小时去重（bounceRateGate）——退信风暴时每个渠道每小时
// 至多广播一次告警，重启后窗口重新计时（告警是提示性事件，少发无害、多发刷屏）。
package inbound

import (
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/notify"
)

// bounceRateWarnGap 同渠道两次告警的最小间隔。
const bounceRateWarnGap = time.Hour

// bounceRateGate 渠道 → 上次告警时刻（map+互斥锁，插入时顺手清理过期项）。
var bounceRateGate = struct {
	sync.Mutex
	last map[uint]time.Time
}{last: make(map[uint]time.Time)}

// EmitBounceRateWarn 摄取到 bounced 事件后调用：窗口内有终态事件且退信率超阈值
// 时广播 warn 通知。任何失败（读设置/聚合失败）只记日志，绝不影响摄取响应。
func EmitBounceRateWarn(db *gorm.DB, ch *model.Channel) {
	var setting model.Setting
	if err := db.Limit(1).Find(&setting, 1).Error; err != nil {
		log.Printf("[inbound] 退信率告警读取设置失败: %v", err)
		return
	}
	threshold := setting.BounceRateThreshold
	if threshold <= 0 || threshold > 100 {
		return // 0=关闭告警；非法配置同关闭（fail-open，告警不阻断摄取）
	}

	since := midnight(time.Now()).AddDate(0, 0, -6) // 本地日历日近 7 天（含今天）
	var row struct {
		Delivered  int64
		Bounced    int64
		Complained int64
	}
	err := db.Model(&model.MailEvent{}).
		Select("COALESCE(SUM(CASE WHEN event = ? THEN 1 ELSE 0 END), 0) AS delivered, "+
			"COALESCE(SUM(CASE WHEN event = ? THEN 1 ELSE 0 END), 0) AS bounced, "+
			"COALESCE(SUM(CASE WHEN event = ? THEN 1 ELSE 0 END), 0) AS complained",
			model.MailEventDelivered, model.MailEventBounced, model.MailEventComplained).
		Where("channel_id = ? AND created_at >= ?", ch.ID, since).
		Scan(&row).Error
	if err != nil {
		log.Printf("[inbound] 退信率告警聚合失败（渠道 %s）: %v", ch.Name, err)
		return
	}
	outcomes := row.Delivered + row.Bounced + row.Complained
	if outcomes <= 0 {
		return // 无终态事件数据：不判定（与健康度 null 口径一致）
	}
	rate := float64(row.Bounced) / float64(outcomes) * 100
	if rate < threshold {
		return
	}

	now := time.Now()
	bounceRateGate.Lock()
	for id, t := range bounceRateGate.last {
		if now.Sub(t) >= bounceRateWarnGap {
			delete(bounceRateGate.last, id)
		}
	}
	if t, ok := bounceRateGate.last[ch.ID]; ok && now.Sub(t) < bounceRateWarnGap {
		bounceRateGate.Unlock()
		return // 1 小时内已告警过：去重
	}
	bounceRateGate.last[ch.ID] = now
	bounceRateGate.Unlock()

	cid := ch.ID
	notify.Emit(notify.EventBounceRate, notify.LevelWarn,
		fmt.Sprintf("渠道「%s」退信率超阈值", ch.Name),
		fmt.Sprintf("近 7 天退信率 %.1f%%（阈值 %.1f%%）：退信 %d / 送达 %d / 投诉 %d（终态事件合计 %d）",
			rate, threshold, row.Bounced, row.Delivered, row.Complained, outcomes),
		&cid)
}

// midnight 本地时区当日零点（与 api 层统计口径一致）。
func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// ResetBounceRateGateForTest 清空退信率告警去重状态（仅测试使用）：进程级 gate
// 以渠道 ID 为键，跨测试用例复用同一 ID 的渠道时避免 1 小时去重窗口串扰。
func ResetBounceRateGateForTest() {
	bounceRateGate.Lock()
	bounceRateGate.last = make(map[uint]time.Time)
	bounceRateGate.Unlock()
}
