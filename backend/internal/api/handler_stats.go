// handler_stats.go 统计：总览与近 N 日逐日曲线（SPEC §5，聚合走 GROUP BY / 单条范围查询）。
package api

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"catmail/internal/model"
	"catmail/internal/provider"
	"catmail/internal/store"
)

// statsOverview GET /api/stats/overview：今日/本月计数（GROUP BY 单条聚合）
// + 渠道用量（含生效额度；last_error 供前端展示）。
func (s *Server) statsOverview(c *gin.Context) {
	now := time.Now()
	todayStart := midnight(now)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	// 今日 + 本月计数合并为单条聚合（2 条往返省为 1 条）：覆盖本月范围，
	// 按 status 与「是否今天」两维 GROUP BY；排除测试发送（P2-4：is_test=false）。
	// 今日数取 is_today=1 桶，本月成功数取 status=sent 全桶。
	// 方言（D2）：「是否今天」用 CASE WHEN…THEN 1 ELSE 0 显式产出整数——
	// SQLite 的 `(created_at >= ?)` 布尔表达式求值为 0/1 整数，而 PostgreSQL
	// 求值为 true/false 布尔类型（驱动无法把 bool 扫进 int 桶）；CASE WHEN
	// 写法两种方言均返回整数，无需按 driver 分支。
	var aggRows []struct {
		Status  int
		IsToday int
		N       int64
	}
	if err := s.db.Model(&model.SendLog{}).
		Select("status, CASE WHEN created_at >= ? THEN 1 ELSE 0 END AS is_today, COUNT(*) AS n", todayStart).
		Where("created_at >= ? AND is_test = ?", monthStart, false).
		Group("status, is_today").Scan(&aggRows).Error; err != nil {
		serverError(c, err)
		return
	}
	var todaySent, todayFailed, monthSent int64
	for _, r := range aggRows {
		if r.IsToday == 1 {
			switch r.Status {
			case model.SendStatusSent:
				todaySent = r.N
			case model.SendStatusFailed:
				todayFailed = r.N
			}
		}
		if r.Status == model.SendStatusSent {
			monthSent += r.N
		}
	}

	// 成功率（0-100，按今日；无数据为 0）
	var rate int
	if total := todaySent + todayFailed; total > 0 {
		rate = int(math.Round(float64(todaySent) / float64(total) * 100))
	}

	// 渠道用量表：单条查询、按需选列（与 logs 列表 logListColumns 同思路，
	// 不取 api_key/api_secret/smtp_pass 等凭据列），用量按 anchor 归一化展示
	var chs []model.Channel
	if err := s.db.Select("id", "name", "provider", "status",
		"used_day", "day_anchor", "used_month", "month_anchor",
		"daily_limit_override", "monthly_limit_override", "last_error").
		Order("id ASC").Find(&chs).Error; err != nil {
		serverError(c, err)
		return
	}
	today, month := now.Format(dayLayout), now.Format(monthLayout)
	channels := make([]gin.H, 0, len(chs))
	for i := range chs {
		ch := &chs[i]
		quota := provider.DefaultQuotaFor(ch.Provider)
		usedDay, usedMonth := ch.UsedDay, ch.UsedMonth
		if ch.DayAnchor != today {
			usedDay = 0 // anchor 未滚动到今天：展示 0，不动库
		}
		if ch.MonthAnchor != month {
			usedMonth = 0
		}
		channels = append(channels, gin.H{
			"id":                      ch.ID,
			"name":                    ch.Name,
			"provider":                ch.Provider,
			"status":                  ch.Status,
			"used_day":                usedDay,
			"effective_daily_limit":   effectiveLimitOf(ch.DailyLimitOverride, quota.Daily),
			"used_month":              usedMonth,
			"effective_monthly_limit": effectiveLimitOf(ch.MonthlyLimitOverride, quota.Monthly),
			"last_error":              ch.LastError,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"today_sent":   todaySent,
		"today_failed": todayFailed,
		"month_sent":   monthSent,
		"success_rate": rate,
		"channels":     channels,
	})
}

// statsDaily GET /api/stats/daily?days(1-90 默认 30)&channel_id(可选)：
// 本地时区按天分桶（含今天，缺数据日补 0），单条范围查询 + Go 侧聚合，
// 不依赖 SQLite 时区语义。
func (s *Server) statsDaily(c *gin.Context) {
	days := 30
	if raw := c.Query("days"); raw != "" {
		d, err := strconv.Atoi(raw)
		if err != nil || d < 1 || d > 90 {
			badRequest(c, "days 必须为 1-90 的整数")
			return
		}
		days = d
	}
	channelID := int64(0)
	if raw := c.Query("channel_id"); raw != "" {
		cid, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || cid == 0 {
			badRequest(c, "channel_id 无效")
			return
		}
		channelID = int64(cid)
	}

	now := time.Now()
	todayStart := midnight(now)
	start := todayStart.AddDate(0, 0, -(days - 1)) // 含今天共 days 天
	end := todayStart.AddDate(0, 0, 1)

	q := s.db.Model(&model.SendLog{}).
		Select("created_at", "status").
		Where("created_at >= ? AND created_at < ?", start, end).
		Where("is_test = ?", false) // 排除测试发送（P2-4，与 overview 口径一致）
	if channelID > 0 {
		q = q.Where("channel_id = ?", channelID)
	}
	var rows []struct {
		CreatedAt time.Time
		Status    int
	}
	if err := q.Scan(&rows).Error; err != nil {
		serverError(c, err)
		return
	}

	// 本地时区日期 → 桶下标（AddDate 跨夏令时安全）
	idx := make(map[string]int, days)
	dates := make([]string, days)
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i)
		idx[d.Format(dayLayout)] = i
		dates[i] = d.Format("01-02") // MM-DD（SPEC §5）
	}
	sent := make([]int64, days)
	failed := make([]int64, days)
	for _, r := range rows {
		i, ok := idx[r.CreatedAt.In(now.Location()).Format(dayLayout)]
		if !ok {
			continue
		}
		switch r.Status {
		case model.SendStatusSent:
			sent[i]++
		case model.SendStatusFailed:
			failed[i]++
		}
	}
	items := make([]dailyStatDTO, days)
	for i := 0; i < days; i++ {
		items[i] = dailyStatDTO{Date: dates[i], Sent: sent[i], Failed: failed[i]}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// ---------- 渠道健康度（A3：近 7 天按渠道聚合，列表响应附带） ----------

// channelHealthWindowDays 渠道健康统计窗口（天）：与仪表盘逐日曲线同为本地
// 日历日口径（含今天共 7 天，自今天零点回溯 6 天），而非滚动 168 小时。
const channelHealthWindowDays = 7

// channelHealthMap 渠道近 7 日健康聚合（channel_id → health）：
// 三条聚合 SQL，与渠道数无关（无 N+1）——
//
//  1. 计数/最近成功/最近失败：单条 GROUP BY channel_id 条件聚合（CASE WHEN 归并
//     sent/failed 两维 + MAX 取两状态各自最近时间）。在任务书 SQL 基础上补一列
//     failed 条件聚合（同一聚合趟内多算一列，不多一次往返）。
//  2. 失败原因 Top3：GROUP BY channel_id, substr(error,1,120) 后用窗口函数
//     ROW_NUMBER() 截取每渠道前 3（DB 侧排序截断，仅返回 ≤3×渠道数 行）。
//     取舍：Go 内存截取需把「渠道×归并原因」全量行拉回应用层——error 常含
//     收件地址/时间戳等动态片段，故障风暴时归并原因可达数百行/渠道，DB 侧
//     截断更稳；代价是依赖 SQLite ≥3.25 窗口函数（glebarez 内嵌 modernc
//     SQLite 远新于该版本，store 层已锁定驱动）。方言（D2）：PG 8.4+ 原生
//     支持窗口函数（亦可用 DISTINCT ON 等价改写，性能相当，无需分叉）；
//     substr 两种方言均支持，语义一致。
//  3. 送达侧事件计数（送达链路三件套）：mail_events 条件聚合 delivered/bounced/
//     complained，同窗口同口径，比率以终态事件合计为分母（见实现处注释）。
//
// 索引命中：send_logs.created_at 单列索引命中窗口范围过滤（idx_send_logs_
// created_at，见模型注释），is_test/status 在过滤后的行上逐行判定；GROUP BY
// channel_id 在时间窗内的行集上做临时排序——窗口行数受 7 天 retention 约束，
// 不为低区分度的 is_test 建组合索引（与 stats/daily 同一取舍）。
// 归并口径：is_test=false（排除测试发送，与 statsOverview/statsDaily 一致）。
func (s *Server) channelHealthMap(now time.Time) (map[uint]*channelHealthDTO, error) {
	// 窗口起点：本地日历日口径（含今天共 7 天），与 stats/daily 分桶一致
	since := midnight(now).AddDate(0, 0, -(channelHealthWindowDays - 1))

	// 1) 计数 + 最近成功/最近失败（单条条件聚合，SQL 两种方言完全同一份）。
	// 「最近时间」列的接收类型按 driver 分支（D2）：MAX(CASE…) 表达式列无 DATETIME
	// decltype——
	//   - SQLite（glebarez）：不自动转 time.Time → 读回字符串，先按 *string 接收
	//     再 parseDriverTimeString 解析（落库格式固定，见其注释）；
	//   - PostgreSQL：created_at 为 timestamptz → MAX 返回 time.Time，按 *string
	//     接收会扫错（「cannot scan time.Time into *string」）→ 直接 *time.Time。
	// gorm Scan 不支持 any 列，故用两个仅接收类型不同的匿名结构体，SQL 不分叉。
	aggSelect := "channel_id, COUNT(*) AS total, " +
		"SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS sent, " +
		"SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS failed, " +
		"MAX(CASE WHEN status = ? THEN created_at END) AS last_ok, " +
		"MAX(CASE WHEN status = ? THEN created_at END) AS last_fail"
	aggArgs := []any{model.SendStatusSent, model.SendStatusFailed, model.SendStatusSent, model.SendStatusFailed}
	type aggRow struct {
		ChannelID   uint
		Total       int64
		Sent        int64
		Failed      int64
		LastSuccess *time.Time
		LastFailed  *time.Time
	}
	var aggRows []aggRow
	if store.IsPostgres(s.db) {
		var rows []struct {
			ChannelID uint
			Total     int64
			Sent      int64
			Failed    int64
			LastOK    *time.Time
			LastFail  *time.Time
		}
		if err := s.db.Model(&model.SendLog{}).
			Select(aggSelect, aggArgs...).
			Where("created_at >= ? AND is_test = ?", since, false).
			Group("channel_id").
			Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			aggRows = append(aggRows, aggRow{r.ChannelID, r.Total, r.Sent, r.Failed, r.LastOK, r.LastFail})
		}
	} else {
		var rows []struct {
			ChannelID uint
			Total     int64
			Sent      int64
			Failed    int64
			LastOK    *string
			LastFail  *string
		}
		if err := s.db.Model(&model.SendLog{}).
			Select(aggSelect, aggArgs...).
			Where("created_at >= ? AND is_test = ?", since, false).
			Group("channel_id").
			Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			aggRows = append(aggRows, aggRow{r.ChannelID, r.Total, r.Sent, r.Failed,
				parseDriverTimeString(r.LastOK), parseDriverTimeString(r.LastFail)})
		}
	}
	health := make(map[uint]*channelHealthDTO, len(aggRows))
	for _, r := range aggRows {
		var rate int
		if r.Total > 0 {
			rate = int(math.Round(float64(r.Sent) / float64(r.Total) * 100))
		}
		health[r.ChannelID] = &channelHealthDTO{
			WindowDays:    channelHealthWindowDays,
			Total:         r.Total,
			Sent:          r.Sent,
			Failed:        r.Failed,
			SuccessRate:   rate,
			LastSuccessAt: r.LastSuccess,
			LastFailedAt:  r.LastFailed,
			TopErrors:     []channelTopErrorDTO{}, // 无失败时回空数组（前端 v-for 安全）
		}
	}

	// 2) 失败原因 Top3（窗口函数截断；排除空 error，与前端「无原因可展示」一致）。
	// 注意窗口 ORDER BY 不能引用 SELECT 别名（SQLite 先解析窗口定义），reason
	// 须写回原始表达式 substr(error,1,120)。
	var errRows []struct {
		ChannelID uint
		Reason    string
		N         int64
	}
	topErrSQL := `SELECT channel_id, reason, n FROM (
		SELECT channel_id, substr(error, 1, 120) AS reason, COUNT(*) AS n,
			ROW_NUMBER() OVER (
				PARTITION BY channel_id
				ORDER BY COUNT(*) DESC, substr(error, 1, 120) ASC
			) AS rn
		FROM send_logs
		WHERE created_at >= ? AND is_test = ? AND status = ? AND error <> ''
		GROUP BY channel_id, substr(error, 1, 120)
	) WHERE rn <= 3
	ORDER BY channel_id ASC, rn ASC`
	// Table("send_logs") 仅用于给 Raw 语句挂表名（perf 按表计数的回归测试、
	// 诊断日志可归因），SQL 原样执行不受影响。
	if err := s.db.Table("send_logs").Raw(topErrSQL, since, false, model.SendStatusFailed).
		Scan(&errRows).Error; err != nil {
		return nil, err
	}
	for _, r := range errRows {
		if h, ok := health[r.ChannelID]; ok {
			h.TopErrors = append(h.TopErrors, channelTopErrorDTO{Reason: r.Reason, Count: r.N})
		}
	}

	// 3) 送达侧事件聚合（送达链路三件套）：mail_events 按 channel_id 条件聚合
	//    delivered/bounced/complained 计数（同一 7 天窗口，created_at 过滤口径与
	//    受理侧一致）。单条 GROUP BY 条件聚合，与渠道数无关（无 N+1）；mail_events
	//    的 channel_id/created_at 均有索引（见模型注释）。CASE WHEN 整数产出写法
	//    与上方受理侧聚合同款（D2：SQLite/PG 均返回整数，无需分叉）。
	//    三个比率 = 计数 / 终态事件合计（delivered+bounced+complained）：sent 事件
	//    各平台可选上报，以终态事件为分母避开平台差异；窗口内无终态事件的渠道
	//    比率为 nil（JSON null，前端展示「—」，与零日志渠道 health=null 同语义）。
	var evRows []struct {
		ChannelID  uint
		Delivered  int64
		Bounced    int64
		Complained int64
	}
	evSelect := "channel_id, " +
		"COALESCE(SUM(CASE WHEN event = ? THEN 1 ELSE 0 END), 0) AS delivered, " +
		"COALESCE(SUM(CASE WHEN event = ? THEN 1 ELSE 0 END), 0) AS bounced, " +
		"COALESCE(SUM(CASE WHEN event = ? THEN 1 ELSE 0 END), 0) AS complained"
	if err := s.db.Model(&model.MailEvent{}).
		Select(evSelect, model.MailEventDelivered, model.MailEventBounced, model.MailEventComplained).
		Where("created_at >= ?", since).
		Group("channel_id").
		Scan(&evRows).Error; err != nil {
		return nil, err
	}
	for _, r := range evRows {
		h, ok := health[r.ChannelID]
		if !ok {
			// 有事件但窗口内零发送日志的渠道也产出健康块（受理侧为零值 + 送达侧计数）
			h = &channelHealthDTO{
				WindowDays: channelHealthWindowDays,
				TopErrors:  []channelTopErrorDTO{},
			}
			health[r.ChannelID] = h
		}
		h.Delivered, h.Bounced, h.Complained = r.Delivered, r.Bounced, r.Complained
		if outcomes := r.Delivered + r.Bounced + r.Complained; outcomes > 0 {
			round1 := func(v float64) float64 { return math.Round(v*10) / 10 } // 一位小数（0-100）
			delivery := round1(float64(r.Delivered) / float64(outcomes) * 100)
			bounce := round1(float64(r.Bounced) / float64(outcomes) * 100)
			complaint := round1(float64(r.Complained) / float64(outcomes) * 100)
			h.DeliveryRate, h.BounceRate, h.ComplaintRate = &delivery, &bounce, &complaint
		} // outcomes==0：比率保持 nil（无事件数据口径）
	}
	return health, nil
}

// driverTimeFormats glebarez/go-sqlite 的时间落库格式（sqlite.go writeTimeFormats，
// 默认取首项）与常见兼容格式：created_at 由 GORM 统一经驱动写入，MAX(CASE…) 聚合
// 后失去 DATETIME 列声明，读回为字符串，按落库格式解析。
var driverTimeFormats = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
}

// parseDriverTimeString 驱动返回的时间字符串 → time.Time（nil/解析失败 → nil，
// 仅影响展示层的最近时间，不影响计数聚合）。SQLite 方言（D2）专用：PG 分支由
// channelHealthMap 直接按 *time.Time 接收，无需解析。
func parseDriverTimeString(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	for _, layout := range driverTimeFormats {
		if t, err := time.Parse(layout, *s); err == nil {
			return &t
		}
	}
	return nil
}
