// quota_burst_test.go API 级并发额度压测（真实 HTTP 层）：httptest.Server 承载
// 完整 gin 栈，50 goroutine 携 barrier 同时打 /v1/send，验证渠道额度在 API 并发
// 下既不超发也不少发——P1-1 条件 UPDATE 原子预留的端到端回归防线（rotator 包内
// 已有单测覆盖函数级语义，本文件补上"经过鉴权/解析/轮换/记账全链路"后的保证）。
//
// 断言可以精确到"恰好 N 个 200"的确定性来源（SQLite 单写者架构下为何不 flaky）：
//   - 不超发：额度判定与自增合并为单条条件 UPDATE（rotator.reserveQuota），
//     SQLite 单写者（WAL + busy_timeout 5000ms）串行化全部写事务——两个并发
//     请求不可能同时把 used 从 limit-1 抢到 limit。超额的唯一来源是"读快照
//     判定再回写"的旧实现，本组测试守护该回归不再出现。
//   - 不少发：受理数 = 抢到预留的请求数；预留失败的请求 fail-closed 得 429，
//     不会拖累其它请求已完成的预留。若成功数 < limit，唯一可能是 reserveQuota
//     写库报错被 fail-closed 吞掉（如 SQLITE_BUSY 超出 busy_timeout）——那是
//     必须查明修复的缺陷，不得放宽断言；本套写负载（每请求约 4 条微秒级短写，
//     预留竞争在毫秒级内结束）对 5000ms busy_timeout 余量极大，正常不应触发。
//   - mock 适配器 50ms 延迟发生在预留之后，只放大请求重叠窗口，不影响计数。
//   - 429 拒发（tried==0）不落 SendLog：日志条数恒等于成功数，守护日志与
//     额度记账同源、不丢不多。
package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// burstConcurrency 并发请求数（50 并发对单渠道 SQLite 写者已构成真实竞争）。
const burstConcurrency = 50

// burstResult 一次并发请求的结果快照。
type burstResult struct {
	status int
	body   string
	err    error
}

// removeSeedChannels 清掉 EnsureSeed 自带的「示例渠道」。额度压测要求候选渠道
// 集合精确可控：种子渠道是 mock 且不限额度，若保留，输掉预留竞争的请求会
// failover 到它照样 200（而非 429），"恰好 N 个成功"的断言必然失真。
// 必须在创建压测渠道之前调用（此时表内只有种子行，清空即种子）。
func removeSeedChannels(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec(`DELETE FROM channels`).Error; err != nil {
		t.Fatalf("remove seed channels: %v", err)
	}
}

// startBurstServer 把 gin 引擎挂到真实 httptest.Server：与 doReq 的
// ResponseRecorder 单请求直调不同，TCP 连接/读写/服务端调度全部真实并发。
func startBurstServer(t *testing.T, r *gin.Engine) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// burstSend 向 serverURL 并发投递 n 个真实 HTTP POST /v1/send。
// barrier：全部 goroutine 就位（ready.Done）后统一放行（close(start)），放行后
// runtime.Gosched 让出调度增加重叠窗口；mock 发送 50ms，服务端天然大量重叠。
// 传输层错误记录在 err 字段，返回前统一判死（压测要求 n 个请求全部送达）。
func burstSend(t *testing.T, serverURL, key string, n int) []burstResult {
	t.Helper()
	client := &http.Client{Timeout: 60 * time.Second}
	payload := sendBody("noreply@example.com", "rcpt@example.com", "并发额度压测")
	start := make(chan struct{})
	var ready, wg sync.WaitGroup
	ready.Add(n)
	results := make([]burstResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			runtime.Gosched()
			req, err := http.NewRequest(http.MethodPost, serverURL+"/v1/send", strings.NewReader(payload))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+key)
				var resp *http.Response
				resp, err = client.Do(req)
				if err == nil {
					b, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					results[i] = burstResult{status: resp.StatusCode, body: string(b), err: readErr}
					return
				}
			}
			results[i] = burstResult{err: err}
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("请求 %d 传输失败：%v（压测要求全部请求送达，否则计数断言无效）", i, r.err)
		}
	}
	return results
}

// classifyBurst 校验并发结果分布：恰好 wantOK 个 200（accepted=true），其余全部
// 429 quota_exhausted；出现任何其它状态码立即判死（见文件头"确定性来源"：
// 500/502 意味着走出了压测设计的路径，必须查明而非放宽）。
func classifyBurst(t *testing.T, results []burstResult, wantOK int) {
	t.Helper()
	ok, limited := 0, 0
	for i, r := range results {
		switch {
		case r.status == http.StatusOK:
			if !strings.Contains(r.body, `"accepted":true`) {
				t.Errorf("请求 %d 200 但 accepted!=true：%s", i, r.body)
			}
			ok++
		case r.status == http.StatusTooManyRequests:
			if !strings.Contains(r.body, `"type":"quota_exhausted"`) {
				t.Errorf("请求 %d 429 但 type!=quota_exhausted：%s", i, r.body)
			}
			limited++
		default:
			t.Fatalf("请求 %d 状态码 %d 不在 {200,429} 内：%s", i, r.status, r.body)
		}
	}
	if ok != wantOK {
		t.Errorf("200 数 = %d，want %d（少于额度：预留写库异常 fail-closed 造成少发，须排查；多于额度：原子预留被破坏，超发）", ok, wantOK)
	}
	if limited != len(results)-wantOK {
		t.Errorf("429 数 = %d，want %d", limited, len(results)-wantOK)
	}
}

// channelByName 按名重读渠道（预留/记账均为 SQL 直改，必须重读才能断言）。
func channelByName(t *testing.T, db *gorm.DB, name string) model.Channel {
	t.Helper()
	var ch model.Channel
	if err := db.Where("name = ?", name).First(&ch).Error; err != nil {
		t.Fatalf("reload channel %s: %v", name, err)
	}
	return ch
}

// countSendLogs 统计非测试发送日志总条数。
func countSendLogs(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	return countLogsWhere(t, db, "is_test = ?", false)
}

// countLogsOfChannel 统计指定渠道的发送日志条数。
func countLogsOfChannel(t *testing.T, db *gorm.DB, channelID uint) int64 {
	t.Helper()
	return countLogsWhere(t, db, "channel_id = ?", channelID)
}

func countLogsWhere(t *testing.T, db *gorm.DB, query string, arg any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.SendLog{}).Where(query, arg).Count(&n).Error; err != nil {
		t.Fatalf("count send logs: %v", err)
	}
	return n
}

// TestQuotaBurstDailyLimit 日额度并发：override=20、月不限，50 并发 →
// 恰好 20 个 200、30 个 429 quota_exhausted；used_day 终值精确 =20
// （不多不少：>20 超发、<20 少发，都是回归）；used_month 同步精确 =20；
// 发送日志 = 成功数。
func TestQuotaBurstDailyLimit(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	removeSeedChannels(t, db)
	createChannel(t, r, cookie,
		`{"name":"压测日额","provider":"mock","priority":-10,"daily_limit_override":20,"monthly_limit_override":0}`)
	key := createKey(t, r, cookie, "压测Key")

	srv := startBurstServer(t, r)
	results := burstSend(t, srv.URL, key, burstConcurrency)
	classifyBurst(t, results, 20)

	ch := channelByName(t, db, "压测日额")
	if ch.UsedDay != 20 {
		t.Fatalf("used_day = %d，want 20（终值不多不少：超发/少发均为回归）", ch.UsedDay)
	}
	// 预留对日/月同步 +1：月维度不限额，也应精确累计到 20（守护两条计数管线一致）
	if ch.UsedMonth != 20 {
		t.Fatalf("used_month = %d，want 20（预留日/月同步记账）", ch.UsedMonth)
	}
	if n := countSendLogs(t, db); n != 20 {
		t.Fatalf("send logs = %d，want 20（429 拒发不落日志：日志与成功数严格一致）", n)
	}
}

// TestQuotaBurstMonthlyLimit 月额度并发独立生效：日 override=0（不限）、
// 月 override=5，50 并发 → 恰好 5 个 200、45 个 429；used_month 精确 =5；
// used_day 同步 =5（日维度不限额但随预留等量累计）；日志 = 成功数。
func TestQuotaBurstMonthlyLimit(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	removeSeedChannels(t, db)
	createChannel(t, r, cookie,
		`{"name":"压测月额","provider":"mock","priority":-10,"daily_limit_override":0,"monthly_limit_override":5}`)
	key := createKey(t, r, cookie, "压测Key")

	srv := startBurstServer(t, r)
	results := burstSend(t, srv.URL, key, burstConcurrency)
	classifyBurst(t, results, 5)

	ch := channelByName(t, db, "压测月额")
	if ch.UsedMonth != 5 {
		t.Fatalf("used_month = %d，want 5（月维度原子预留终值不多不少）", ch.UsedMonth)
	}
	if ch.UsedDay != 5 {
		t.Fatalf("used_day = %d，want 5（日不限额同样随预留同步累计）", ch.UsedDay)
	}
	if n := countSendLogs(t, db); n != 5 {
		t.Fatalf("send logs = %d，want 5（日志与成功数严格一致）", n)
	}
}

// TestQuotaBurstRotation 轮换下的并发额度分摊：A 日额 3、B 日额 7、priority 相同，
// 50 并发不带 channel → 成功数恰为 3+7=10、其余 429；A/B 各自精确记满 3/7
// （候选间 failover 不重复预留、不漏预留）；每渠道日志数与其记账一一对应。
func TestQuotaBurstRotation(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	removeSeedChannels(t, db)
	createChannel(t, r, cookie, `{"name":"压测A","provider":"mock","daily_limit_override":3}`)
	createChannel(t, r, cookie, `{"name":"压测B","provider":"mock","daily_limit_override":7}`)
	key := createKey(t, r, cookie, "压测Key")

	srv := startBurstServer(t, r)
	results := burstSend(t, srv.URL, key, burstConcurrency)
	classifyBurst(t, results, 10)

	// 成功请求只允许落在压测A/B 上（无种子渠道、无指定渠道）
	for i, res := range results {
		if res.status == http.StatusOK &&
			!strings.Contains(res.body, `"channel":"压测A"`) &&
			!strings.Contains(res.body, `"channel":"压测B"`) {
			t.Fatalf("请求 %d 成功但渠道不属于压测A/B：%s", i, res.body)
		}
	}

	a := channelByName(t, db, "压测A")
	b := channelByName(t, db, "压测B")
	if a.UsedDay != 3 {
		t.Fatalf("A used_day = %d，want 3", a.UsedDay)
	}
	if b.UsedDay != 7 {
		t.Fatalf("B used_day = %d，want 7", b.UsedDay)
	}
	if n := countSendLogs(t, db); n != 10 {
		t.Fatalf("send logs = %d，want 10（日志与成功数严格一致）", n)
	}
	// 分摊口径：每渠道日志数 = 其预留记账（3/7），不得互相挪用或丢失
	if la, lb := countLogsOfChannel(t, db, a.ID), countLogsOfChannel(t, db, b.ID); la != 3 || lb != 7 {
		t.Fatalf("A/B logs = %d/%d，want 3/7（每渠道日志与记账一一对应）", la, lb)
	}
}
