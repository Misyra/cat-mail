// handler_metrics.go Prometheus 抓取端点（B4）：GET /metrics 手写暴露
// Prometheus 文本暴露格式（version 0.0.4），不引入 client_golang 依赖——
// 指标全集仅四个来源均为 DB 聚合的计数/状态值，文本拼装足够且零新增依赖。
//
// 鉴权：挂管理 IP 白名单（E5），不挂 requireSession——Prometheus 抓取器不会
// 登录；未配置 CATMAIL_ADMIN_CIDR 时全网可达，生产建议与白名单一同配置收紧。
//
// 实现取舍：抓取时实时聚合，不做缓存——全部查询走 SendLog/Channel/Notification
// 的既有索引列（status/created_at、全表小），SQLite 下毫秒级；缓存反而引入陈旧
// 窗口（告警滞后）与失效逻辑，得不偿失。
package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"catmail/internal/model"
)

// metricsContentType Prometheus 文本暴露格式 0.0.4（UTF-8：channel 名等标签值可含中文）。
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// escapeLabelValue 按 Prometheus 文本格式转义 label 值中的反斜杠、双引号与换行
// （其余字符如中文按 UTF-8 原样输出）。HELP 文本为固定常量，不含需转义字符。
func escapeLabelValue(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// metricsLabel 渲染一对已转义的 label="value"。
func metricsLabel(name, value string) string {
	return name + `="` + escapeLabelValue(value) + `"`
}

// channelStateLabel Channel.Status → gauge 标签 state（enabled/disabled/cooldown）：
// 手动停用与自动禁用同归 disabled，未知取值宁窄勿宽归 disabled。
func channelStateLabel(status int) string {
	switch status {
	case model.ChannelEnabled:
		return "enabled"
	case model.ChannelCooldown:
		return "cooldown"
	default:
		return "disabled"
	}
}

// sendStatusLabel SendLog.Status → 标签 status（仅 sent/failed；pending 为在途
// 瞬态不入指标，语义与 SPEC §5 的统计口径一致）。
func sendStatusLabel(status int) (string, bool) {
	switch status {
	case model.SendStatusSent:
		return "sent", true
	case model.SendStatusFailed:
		return "failed", true
	default:
		return "", false
	}
}

// metrics GET /metrics：四族指标，样本行族内按字典序输出（输出确定性，便于
// diff 与测试断言；Prometheus 文本格式不要求顺序）。
func (s *Server) metrics(c *gin.Context) {
	// 全量发送计数（is_test=false）：channel_name/provider 为 SendLog 落库冗余
	// 快照，已删渠道的历史发送仍按快照输出；GROUP BY 一条聚合查询
	sendLines, err := s.sendCounterLines("catmail_sends_total", false)
	if err != nil {
		serverError(c, err)
		return
	}
	// 测试发送计数（is_test=true，渠道「测试发送」单独计数，不混入 sends_total）
	testLines, err := s.sendCounterLines("catmail_test_sends_total", true)
	if err != nil {
		serverError(c, err)
		return
	}

	// 渠道状态 gauge：渠道全集输出（停用/冷却渠道不掉样本），每渠道恒为 1
	var chs []model.Channel
	if err := s.db.Select("name", "provider", "status").
		Order("id ASC").Find(&chs).Error; err != nil {
		serverError(c, err)
		return
	}
	chLines := make([]string, 0, len(chs))
	for i := range chs {
		ch := &chs[i]
		chLines = append(chLines, "catmail_channels{"+
			strings.Join([]string{
				metricsLabel("channel", ch.Name),
				metricsLabel("provider", ch.Provider),
				metricsLabel("state", channelStateLabel(ch.Status)),
			}, ",")+"} 1")
	}

	// 未读通知 gauge（与 /api/notifications/unread_count 同口径）
	unread, err := s.countUnread()
	if err != nil {
		serverError(c, err)
		return
	}

	sort.Strings(sendLines)
	sort.Strings(testLines)
	sort.Strings(chLines)

	b := &strings.Builder{}
	writeMetricFamily(b, "catmail_sends_total", "counter",
		"全量发送计数（按渠道/平台/结果状态，排除测试发送）", sendLines)
	writeMetricFamily(b, "catmail_test_sends_total", "counter",
		"测试发送计数（渠道测试发送 is_test=true，不混入 catmail_sends_total）", testLines)
	writeMetricFamily(b, "catmail_channels", "gauge",
		"渠道状态（state: enabled/disabled/cooldown，每渠道恒为 1）", chLines)
	writeMetricFamily(b, "catmail_notifications_unread", "gauge",
		"站内通知未读数", []string{"catmail_notifications_unread " + strconv.FormatInt(unread, 10)})

	c.Data(http.StatusOK, metricsContentType, []byte(b.String()))
}

// sendCounterLines 聚合 SendLog（按 channel_name/provider/status GROUP BY，仅
// sent/failed 两态）并渲染为 name 族样本行；isTest 区分全量与测试发送计数。
func (s *Server) sendCounterLines(name string, isTest bool) ([]string, error) {
	var rows []struct {
		ChannelName string
		Provider    string
		Status      int
		N           int64
	}
	if err := s.db.Model(&model.SendLog{}).
		Select("channel_name, provider, status, COUNT(*) AS n").
		Where("is_test = ? AND status IN ?", isTest,
			[]int{model.SendStatusSent, model.SendStatusFailed}).
		Group("channel_name, provider, status").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		status, ok := sendStatusLabel(r.Status)
		if !ok {
			continue
		}
		lines = append(lines, name+"{"+
			strings.Join([]string{
				metricsLabel("channel", r.ChannelName),
				metricsLabel("provider", r.Provider),
				metricsLabel("status", status),
			}, ",")+"} "+strconv.FormatInt(r.N, 10))
	}
	return lines, nil
}

// writeMetricFamily 输出一个指标族的 HELP/TYPE 行与样本行（空族仍输出 HELP/TYPE，
// 抓取器与告警规则可依赖指标恒存在）。
func writeMetricFamily(b *strings.Builder, name, typ, help string, lines []string) {
	b.WriteString("# HELP " + name + " " + help + "\n")
	b.WriteString("# TYPE " + name + " " + typ + "\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
}
