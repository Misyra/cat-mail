// handler_logs.go 发送日志：分页筛选列表（排序可调）、全字段详情、CSV 导出与
// 按快照重发（SPEC §5）。
package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/provider"
	"catmail/internal/rotator"
)

// logListColumns 列表查询列（排除 html/text 正文大字段）。
const logListColumns = "id, message_id, api_key_id, channel_id, channel_name, provider, template_id, is_test, status, " +
	"from_addr, to_addrs, subject, provider_message_id, error, attempts, latency_ms, created_at"

// 日志排序枚举（sort 参数）。
const (
	logSortNewest      = "newest"       // created_at 倒序（默认）
	logSortOldest      = "oldest"       // created_at 正序
	logSortLatencyDesc = "latency_desc" // 耗时降序，同耗时按 created_at 倒序
)

// logOrderClause 由 sort 参数返回 ORDER BY 子句；id 作 tiebreak 保证
// 分页与导出批次间顺序确定。sort 须经 logQueryCond 校验合法。
func logOrderClause(sort string) string {
	switch sort {
	case logSortOldest:
		return "created_at ASC, id ASC"
	case logSortLatencyDesc:
		return "latency_ms DESC, created_at DESC, id DESC"
	default:
		return "created_at DESC, id DESC"
	}
}

// logQueryCond 解析 /api/logs 与 /api/logs/export 共用的查询参数：
// sort（newest 默认 / oldest / latency_desc，非法值 → 错误）、
// status（"sent"/"failed"/"pending"，空=全部）、channel_id、q 模糊（subject+to_addrs）。
// 条件构造器按需重建，避免 GORM 语句复用。
func logQueryCond(c *gin.Context) (cond func(*gorm.DB) (*gorm.DB, error), sort string, err error) {
	switch c.Query("sort") {
	case "":
		sort = logSortNewest
	case logSortNewest, logSortOldest, logSortLatencyDesc:
		sort = c.Query("sort")
	default:
		return nil, "", errors.New("sort 仅支持 newest/oldest/latency_desc")
	}
	cond = func(q *gorm.DB) (*gorm.DB, error) {
		switch c.Query("status") {
		case "":
		case "sent":
			q = q.Where("status = ?", model.SendStatusSent)
		case "failed":
			q = q.Where("status = ?", model.SendStatusFailed)
		case "pending":
			q = q.Where("status = ?", model.SendStatusPending)
		default:
			return nil, errors.New("status 仅支持 sent/failed/pending")
		}
		if raw := c.Query("channel_id"); raw != "" {
			cid, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || cid == 0 {
				return nil, errors.New("channel_id 无效")
			}
			q = q.Where("channel_id = ?", cid)
		}
		if kw := strings.TrimSpace(c.Query("q")); kw != "" {
			if utf8.RuneCountInString(kw) > 128 {
				return nil, errors.New("q 不能超过 128 字符")
			}
			pat := "%" + escapeLike(kw) + "%"
			q = q.Where("subject LIKE ? ESCAPE '\\' OR to_addrs LIKE ? ESCAPE '\\'", pat, pat)
		}
		return q, nil
	}
	return cond, sort, nil
}

// listLogs GET /api/logs：page≥1、page_size 1-100（默认 20）、
// status/channel_id/q 筛选与 sort 排序（SPEC §5；status 为字符串枚举，F3 契约；
// sort 为 newest/oldest/latency_desc 枚举）。
func (s *Server) listLogs(c *gin.Context) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		badRequest(c, "page 必须为 ≥1 的整数")
		return
	}
	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if err != nil || pageSize < 1 || pageSize > 100 {
		badRequest(c, "page_size 必须为 1-100 的整数")
		return
	}
	cond, sort, qerr := logQueryCond(c)
	if qerr != nil {
		badRequest(c, qerr.Error())
		return
	}

	base, err := cond(s.db.Model(&model.SendLog{}))
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		serverError(c, err)
		return
	}

	base, err = cond(s.db.Model(&model.SendLog{}))
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	var logs []model.SendLog
	if err := base.Select(logListColumns).
		Order(logOrderClause(sort)).
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&logs).Error; err != nil {
		serverError(c, err)
		return
	}
	items := make([]logItemDTO, 0, len(logs))
	for _, l := range logs {
		items = append(items, newLogItem(&l))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// getLog GET /api/logs/:id：完整详情，含 html_body/text_body（SPEC §5）。
func (s *Server) getLog(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var l model.SendLog
	if err := s.db.First(&l, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "日志不存在")
			return
		}
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, logDetailDTO{
		logItemDTO: newLogItem(&l),
		HTMLBody:   l.HTMLBody,
		TextBody:   l.TextBody,
	})
}

// ---------- CSV 导出（GET /api/logs/export） ----------

// 导出行数上限与流式写出批次大小（不建全量切片，csv.Writer 直连 ResponseWriter）。
const (
	logExportCap   = 10000 // 达到上限即停止并追加备注提示行
	logExportBatch = 500   // 每批查询条数
)

// logCSVColumns 导出列（契约）：时间(本地 RFC3339)/状态/渠道/平台/发件人/
// 收件人/主题/耗时ms/测试/错误信息/消息ID；末列「备注」仅截断提示行填写。
var logCSVColumns = []string{
	"时间", "状态", "渠道", "平台", "发件人", "收件人", "主题", "耗时ms", "测试", "错误信息", "消息ID", "备注",
}

// exportLogs GET /api/logs/export：查询参数与 /api/logs 完全一致
// （status/channel_id/q/sort；page/page_size 忽略），流式输出 CSV 文件下载。
// 响应头 Content-Type: text/csv; charset=utf-8、Content-Disposition: attachment
// （文件名 catmail-logs-<UTC YYYYMMDD-HHMMSS>.csv）；开头写 UTF-8 BOM（Excel
// 中文兼容）。行数上限 10000，达到上限时在末尾追加一行，其「备注」列为
// 「已达导出上限，仅含前 10000 条」。CSV 注入防护：以 = + - @ 开头的单元格前置单引号。
func (s *Server) exportLogs(c *gin.Context) {
	cond, sort, err := logQueryCond(c)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	order := logOrderClause(sort)

	// 首批先查后写：查询失败仍可返回 JSON 500（一旦写出 BOM 就无法再改状态码）。
	// cond 的错误是请求参数校验错误（status/channel_id/q），映射 400；
	// 仅真正的查询失败走 serverError。
	first := make([]model.SendLog, 0, logExportBatch)
	qdb, cerr := cond(s.db.Model(&model.SendLog{}).Select(logListColumns))
	if cerr != nil {
		badRequest(c, cerr.Error())
		return
	}
	if err := qdb.Order(order).Limit(logExportBatch).Find(&first).Error; err != nil {
		serverError(c, err)
		return
	}

	h := c.Writer.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "catmail-logs-"+time.Now().UTC().Format("20060102-150405")+".csv"))
	c.Writer.WriteHeader(http.StatusOK)

	// UTF-8 BOM：Excel 依据 BOM 识别 UTF-8 中文内容
	if _, err := c.Writer.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		log.Printf("api: export write BOM: %v", err)
		return
	}
	w := csv.NewWriter(c.Writer)
	_ = w.Write(logCSVColumns)

	written := writeLogCSVRows(w, first)
	more := len(first) == logExportBatch // 首批不满说明已到数据末尾
	for more && written < logExportCap {
		limit := min(logExportBatch, logExportCap-written)
		var rows []model.SendLog
		var q *gorm.DB
		var qerr error
		if q, qerr = cond(s.db.Model(&model.SendLog{}).Select(logListColumns)); qerr == nil {
			qerr = q.Order(order).Offset(written).Limit(limit).Find(&rows).Error
		}
		if qerr != nil {
			// 响应头已提交，无法改为 500：中止写出并记录服务端日志
			log.Printf("api: export batch at offset %d: %v", written, qerr)
			w.Flush()
			return
		}
		written += writeLogCSVRows(w, rows)
		more = len(rows) == limit
	}

	// 达到上限：探测是否仍有更多行，有则追加备注提示行
	if written >= logExportCap {
		var probe []model.SendLog
		q, qerr := cond(s.db.Model(&model.SendLog{}).Select("id"))
		if qerr == nil {
			qerr = q.Order(order).Offset(logExportCap).Limit(1).Find(&probe).Error
		}
		if qerr != nil {
			log.Printf("api: export truncation probe: %v", qerr)
		}
		if len(probe) > 0 {
			hint := make([]string, len(logCSVColumns))
			hint[len(logCSVColumns)-1] = fmt.Sprintf("已达导出上限，仅含前 %d 条", logExportCap)
			_ = w.Write(hint)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		log.Printf("api: export csv write: %v", err)
	}
}

// writeLogCSVRows 批量写出日志行，返回已写出的条数（写出失败由 Flush().Error() 暴露）。
func writeLogCSVRows(w *csv.Writer, logs []model.SendLog) int {
	n := 0
	for i := range logs {
		if w.Write(logCSVRow(&logs[i])) != nil {
			break
		}
		n++
	}
	return n
}

// logCSVRow 单条日志 → CSV 行：时间本地 RFC3339、状态/测试为枚举文案、
// 收件人 "; " 连接；除备注列外所有单元格经 sanitizeCSVCell 注入防护。
func logCSVRow(l *model.SendLog) []string {
	status := "pending"
	switch l.Status {
	case model.SendStatusSent:
		status = "sent"
	case model.SendStatusFailed:
		status = "failed"
	}
	test := "no"
	if l.IsTest {
		test = "yes"
	}
	return []string{
		sanitizeCSVCell(l.CreatedAt.Local().Format(time.RFC3339)),
		sanitizeCSVCell(status),
		sanitizeCSVCell(l.ChannelName),
		sanitizeCSVCell(l.Provider),
		sanitizeCSVCell(l.FromAddr),
		sanitizeCSVCell(strings.Join(parseToAddrs(l.ToAddrs), "; ")),
		sanitizeCSVCell(l.Subject),
		sanitizeCSVCell(strconv.FormatInt(l.LatencyMs, 10)),
		sanitizeCSVCell(test),
		sanitizeCSVCell(l.Error),
		sanitizeCSVCell(l.ProviderMessageID),
		"", // 备注：仅截断提示行使用
	}
}

// sanitizeCSVCell CSV 注入防护：以 = + - @ 开头的单元格前置单引号，
// 防止 Excel/表格软件将单元格内容解析为公式执行。
func sanitizeCSVCell(s string) string {
	if s != "" {
		switch s[0] {
		case '=', '+', '-', '@':
			return "'" + s
		}
	}
	return s
}

// parseToAddrs 落库 JSON 串 → 收件人数组；解析失败时原样返回单项。
func parseToAddrs(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(raw), &out) == nil {
		return out
	}
	return []string{raw}
}

// newLogItem 由落库记录构造列表项 DTO（to_addrs 反序列化为数组）。
func newLogItem(l *model.SendLog) logItemDTO {
	return logItemDTO{
		ID:                l.ID,
		MessageID:         l.MessageID,
		ApiKeyID:          l.ApiKeyID,
		ChannelID:         l.ChannelID,
		ChannelName:       l.ChannelName,
		Provider:          l.Provider,
		TemplateID:        l.TemplateID,
		IsTest:            l.IsTest,
		Status:            l.Status,
		FromAddr:          l.FromAddr,
		ToAddrs:           newToAddrs(l.ToAddrs),
		Subject:           l.Subject,
		ProviderMessageID: l.ProviderMessageID,
		Error:             l.Error,
		Attempts:          l.Attempts,
		LatencyMs:         l.LatencyMs,
		CreatedAt:         l.CreatedAt,
	}
}

// ---------- 日志重发（POST /api/logs/:id/resend，E3） ----------

// resendCooldown 同一日志两次重发的最小间隔（E3 防滥用）：管理端误连点或脚本
// 重放不至于在短时间内对同一批收件人重复轰炸。
const resendCooldown = 30 * time.Second

// resendGate 进程内重发节流：日志 id → 上次触发重发的时刻（map+互斥锁）。
// 进程内实现，重启即丢失、窗口重新计时——重发是低频人工动作，无需持久化。
// 插入时顺手清理过期项，map 规模 ≈ 最近 30s 内的重发请求数，不会无限增长。
var resendGate = struct {
	sync.Mutex
	last map[uint]time.Time
}{last: make(map[uint]time.Time)}

// resendGateClaim 占坑：同一日志 id 在 resendCooldown 内已有触发（不论发送成败）
// 则返回 false。失败尝试同样占用窗口——防对注定失败的日志风暴式重试。
func resendGateClaim(id uint) bool {
	now := time.Now()
	resendGate.Lock()
	defer resendGate.Unlock()
	for k, t := range resendGate.last {
		if now.Sub(t) >= resendCooldown {
			delete(resendGate.last, k)
		}
	}
	if t, ok := resendGate.last[id]; ok && now.Sub(t) < resendCooldown {
		return false
	}
	resendGate.last[id] = now
	return true
}

// resendLog POST /api/logs/:id/resend（会话鉴权）：按日志快照原样重发一封邮件。
//   - 正文缺失（A4 隐私开关历史/为空）→ 400「该日志未记录正文，无法重发」；
//     重发必须按原内容进行，无内容不可凭空重建。
//   - 构造 provider.Message（from/to/subject/html/text 原样）。headers 未落库
//     （SendLog 无该列），附件/标签同理不在快照内——重发不含原始自定义头。
//   - 走 rotator.Send 正常轮换，不带 channel：原渠道可能已被禁用/冷却/额度耗尽，
//     重发的目的是把信送到，交给轮换器在当前可用渠道中选择；is_test=false
//     （正常发送口径），message_id 由 rotator 生成新 uuid，与原日志区分。
//   - 防滥用：同一日志 id 30 秒内只允许重发一次（resendGate，进程内态）。
//   - 成功响应复用发送成功体 {id,channel,provider,provider_message_id,accepted}；
//     失败透传 rotator 的统一错误体（429 额度/502 候选穷尽等）。
func (s *Server) resendLog(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var l model.SendLog
	if err := s.db.First(&l, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "日志不存在")
			return
		}
		serverError(c, err)
		return
	}
	if strings.TrimSpace(l.HTMLBody) == "" && strings.TrimSpace(l.TextBody) == "" {
		badRequest(c, "该日志未记录正文，无法重发")
		return
	}
	tos := parseToAddrs(l.ToAddrs)
	if len(tos) == 0 {
		badRequest(c, "该日志缺少收件人信息，无法重发")
		return
	}
	if !resendGateClaim(id) {
		writeErr(c, http.StatusTooManyRequests, TypeQuotaExhausted,
			"该日志 30 秒内已重发过，请稍后再试")
		return
	}
	// FromAddr 为空（P1-4 历史日志）时留空放行：由 rotator 按候选渠道默认发件人回退
	msg := &provider.Message{
		FromAddr: l.FromAddr,
		To:       tos,
		Subject:  l.Subject,
		HTML:     l.HTMLBody,
		Text:     l.TextBody,
	}
	// 重发按日志快照（已渲染正文）原样进行，不重新渲染模板；TemplateID 随行
	// 记入新日志保留来源追溯（模板已删除时仅余 ID，展示由前端解析）。
	out, err := rotator.Send(c.Request.Context(), s.db, &rotator.Input{Message: msg, TemplateID: l.TemplateID})
	if err != nil {
		writeRotatorErr(c, err) // 抑制拦截等带 code 的错误透传 error.code（送达链路三件套）
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":                  out.MessageID,
		"channel":             out.ChannelName,
		"provider":            out.Provider,
		"provider_message_id": out.ProviderMessageID,
		"accepted":            true,
	})
}
