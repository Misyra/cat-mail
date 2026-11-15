// handler_metrics_test.go B4 Prometheus /metrics 测试：文本格式规范行
// （HELP/TYPE/样本逐条断言）、标签值转义（反斜杠/引号/换行 + 中文 UTF-8 原样）、
// is_test 不混入 sends_total、渠道三态 gauge（含自动禁用归 disabled）、
// 白名单接线（未配置全网可达 / 配置后白名单外 403，不要求登录）。
package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// seedMetricsData 造指标数据：渠道三态 + 含特殊字符渠道名、全量/测试/在途
// 发送日志、未读/已读通知。种子示例渠道「示例渠道」删除以做闭环断言。
func seedMetricsData(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Where("name = ?", "示例渠道").Delete(&model.Channel{}).Error; err != nil {
		t.Fatalf("delete seed channel: %v", err)
	}
	chs := []model.Channel{
		{Name: "主渠道", Provider: "resend", Status: model.ChannelEnabled, FromEmail: "a@b.com"},
		{Name: `奇"怪\渠道`, Provider: "mailjet", Status: model.ChannelManualDisabled, FromEmail: "a@b.com"},
		{Name: "冷却渠道", Provider: "smtp", Status: model.ChannelCooldown, FromEmail: "a@b.com"},
		{Name: "自动禁用渠道", Provider: "brevo", Status: model.ChannelAutoDisabled, FromEmail: "a@b.com"},
	}
	for i := range chs {
		if err := db.Create(&chs[i]).Error; err != nil {
			t.Fatalf("create channel: %v", err)
		}
	}
	logs := []model.SendLog{
		{ChannelName: "主渠道", Provider: "resend", Status: model.SendStatusSent, IsTest: false},
		{ChannelName: "主渠道", Provider: "resend", Status: model.SendStatusSent, IsTest: false},
		{ChannelName: "主渠道", Provider: "resend", Status: model.SendStatusFailed, IsTest: false},
		// 特殊字符快照：引号/反斜杠（来自已删渠道也照常输出）
		{ChannelName: `奇"怪\渠道`, Provider: "mailjet", Status: model.SendStatusSent, IsTest: false},
		// 换行转义（落库快照可含任意字符）
		{ChannelName: "多\n行", Provider: "resend", Status: model.SendStatusSent, IsTest: false},
		// 在途 pending：不入指标（status 仅 sent/failed）
		{ChannelName: "主渠道", Provider: "resend", Status: model.SendStatusPending, IsTest: false},
		// 测试发送：只进 catmail_test_sends_total
		{ChannelName: "测试专用", Provider: "mock", Status: model.SendStatusSent, IsTest: true},
	}
	for i := range logs {
		if err := db.Create(&logs[i]).Error; err != nil {
			t.Fatalf("create send log: %v", err)
		}
	}
	nots := []model.Notification{
		{Event: "send_failed", Level: "error", Title: "t", Read: false},
		{Event: "channel_cooldown", Level: "warn", Title: "t", Read: true},
	}
	for i := range nots {
		if err := db.Create(&nots[i]).Error; err != nil {
			t.Fatalf("create notification: %v", err)
		}
	}
}

// fetchMetrics 以指定来源 IP 抓取 /metrics，校验 200 与 Content-Type，返回响应体。
func fetchMetrics(t *testing.T, r *gin.Engine, ip string) string {
	t.Helper()
	w := reqFromIP(r, "GET", "/metrics", ip, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics from %s: status = %d, want 200 (body=%s)", ip, w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain（Prometheus 文本格式）", ct)
	}
	return w.Body.String()
}

// TestMetricsFormat 逐条断言 HELP/TYPE 行、样本行（计数/转义/中文）、
// is_test 与 pending 不混入 sends_total、渠道三态 gauge、未读 gauge。
func TestMetricsFormat(t *testing.T) {
	r, db := newWiredEnv(t, "")
	seedMetricsData(t, db)
	body := fetchMetrics(t, r, "203.0.113.9:1000")

	// HELP 行：四族齐备（HELP 后跟说明文本，按前缀断言）
	for _, want := range []string{
		`# HELP catmail_sends_total `,
		`# HELP catmail_test_sends_total `,
		`# HELP catmail_channels `,
		`# HELP catmail_notifications_unread `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少 HELP 行 %q", want)
		}
	}
	// TYPE 行：类型正确（计数器 _total 结尾、gauge）
	for _, want := range []string{
		`# TYPE catmail_sends_total counter`,
		`# TYPE catmail_test_sends_total counter`,
		`# TYPE catmail_channels gauge`,
		`# TYPE catmail_notifications_unread gauge`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("缺少 TYPE 行 %q", want)
		}
	}

	// 样本行逐条：计数、标签顺序、转义（引号/反斜杠/换行）、中文原样
	for _, want := range []string{
		`catmail_sends_total{channel="主渠道",provider="resend",status="sent"} 2`,
		`catmail_sends_total{channel="主渠道",provider="resend",status="failed"} 1`,
		`catmail_sends_total{channel="奇\"怪\\渠道",provider="mailjet",status="sent"} 1`,
		`catmail_sends_total{channel="多\n行",provider="resend",status="sent"} 1`,
		`catmail_test_sends_total{channel="测试专用",provider="mock",status="sent"} 1`,
		`catmail_channels{channel="主渠道",provider="resend",state="enabled"} 1`,
		`catmail_channels{channel="奇\"怪\\渠道",provider="mailjet",state="disabled"} 1`,
		`catmail_channels{channel="冷却渠道",provider="smtp",state="cooldown"} 1`,
		`catmail_channels{channel="自动禁用渠道",provider="brevo",state="disabled"} 1`,
		`catmail_notifications_unread 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("缺少样本行 %q\nbody:\n%s", want, body)
		}
	}

	// 排除面：is_test 不混入 sends_total；pending 不入指标；种子渠道已删；
	// 引号不得裸输出（转义后应为 奇\"怪）
	for _, forbidden := range []string{
		`catmail_sends_total{channel="测试专用"`,
		`catmail_channels{channel="示例渠道"`,
		`status="pending"`,
		`channel="奇"怪`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("不应出现 %q\nbody:\n%s", forbidden, body)
		}
	}

	// 渠道全集：4 个渠道各一条 gauge 样本
	if n := strings.Count(body, "catmail_channels{"); n != 4 {
		t.Errorf("catmail_channels 样本数 = %d, want 4\nbody:\n%s", n, body)
	}
}

// TestMetricsAllowlist 白名单接线：未配置 CIDR 全网可达（生产需配置
// CATMAIL_ADMIN_CIDR 收紧）；配置后白名单外 403、白名单内免登录可抓取。
func TestMetricsAllowlist(t *testing.T) {
	// 未配置：任意来源 200
	r, db := newWiredEnv(t, "")
	seedMetricsData(t, db)
	fetchMetrics(t, r, "203.0.113.9:1000")

	// 配置后：白名单外 403 统一错误体；白名单内无需会话 200
	r2, db2 := newWiredEnv(t, "10.0.0.0/8")
	seedMetricsData(t, db2)
	w := reqFromIP(r2, "GET", "/metrics", "203.0.113.9:1000", "")
	if w.Code != http.StatusForbidden || errType(t, w.Body.String()) != TypeForbidden {
		t.Fatalf("白名单外 /metrics: status = %d, want 403 forbidden (body=%s)", w.Code, w.Body.String())
	}
	fetchMetrics(t, r2, "10.2.3.4:1000")
}
