package rotator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/provider"
	"catmail/internal/store"
)

// ---------- 测试基建：临时 sqlite + 假 Transport（零网络） ----------

// newDB 打开临时 SQLite（仅迁移不 seed，渠道与设置由用例自建）。
func newDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "rotator.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// putSetting 写入 ID=1 设置行（用例显式控制轮换参数）。
func putSetting(t *testing.T, db *gorm.DB, rotation string, maxAttempts, cooldownSeconds, threshold int) {
	t.Helper()
	if err := db.Create(&model.Setting{
		ID:                          1,
		Rotation:                    rotation,
		MaxAttempts:                 maxAttempts,
		CooldownSeconds:             cooldownSeconds,
		ConsecutiveFailureThreshold: threshold,
	}).Error; err != nil {
		t.Fatalf("create setting: %v", err)
	}
}

// fakeTransport 可编程假适配器：零网络，按渠道名分发行为并记录调用顺序与
// 实际收到的消息（并发用例下加锁；msgs 供默认发件人回退断言）。
type fakeTransport struct {
	mu    sync.Mutex
	name  string
	calls []string
	msgs  []*provider.Message
	fn    func(ch *model.Channel) (*provider.Result, *provider.ProviderError)
}

func (f *fakeTransport) Name() string { return f.name }

func (f *fakeTransport) Send(_ context.Context, ch *model.Channel, m *provider.Message) (*provider.Result, *provider.ProviderError) {
	f.mu.Lock()
	f.calls = append(f.calls, ch.Name)
	f.msgs = append(f.msgs, m)
	f.mu.Unlock()
	if f.fn == nil {
		return &provider.Result{ProviderMessageID: "fake-" + ch.Name}, nil
	}
	return f.fn(ch)
}

// newResolver 将假适配器按平台名注册成 Resolver。
func newResolver(ts ...*fakeTransport) func(string) (provider.Transport, bool) {
	m := make(map[string]provider.Transport, len(ts))
	for _, tr := range ts {
		m[tr.name] = tr
	}
	return func(name string) (provider.Transport, bool) {
		tr, ok := m[name]
		return tr, ok
	}
}

// addChannel 创建渠道（provider 默认对接 fake 假适配器，Status 零值即启用）。
func addChannel(t *testing.T, db *gorm.DB, ch *model.Channel) {
	t.Helper()
	if ch.Provider == "" {
		ch.Provider = "fake"
	}
	if err := db.Create(ch).Error; err != nil {
		t.Fatalf("create channel %q: %v", ch.Name, err)
	}
}

// reloadChannel 按名重读渠道，断言落库字段用。
func reloadChannel(t *testing.T, db *gorm.DB, name string) model.Channel {
	t.Helper()
	var ch model.Channel
	if err := db.Where("name = ?", name).First(&ch).Error; err != nil {
		t.Fatalf("reload channel %q: %v", name, err)
	}
	return ch
}

// sendInput 构造标准输入（注入假适配器 Resolver，from 可定制）。
func sendInput(from string, tr *fakeTransport) *Input {
	return &Input{
		Message: &provider.Message{
			FromAddr: from,
			To:       []string{"to@example.com"},
			Subject:  "测试邮件",
			Text:     "hello",
		},
		APIKeyID: 7,
		Resolver: newResolver(tr),
	}
}

// 常见平台错误构造（与适配器契约一致的字段组合）。
func netErr(msg string) *provider.ProviderError {
	return &provider.ProviderError{Code: "network_error", Message: msg, Retryable: true}
}

func authErr(status int) *provider.ProviderError {
	return &provider.ProviderError{HTTPStatus: status, Code: "unauthorized", Message: "invalid api key", DisableChannel: true}
}

func rateErr() *provider.ProviderError {
	return &provider.ProviderError{HTTPStatus: 429, Code: "rate_limited", Message: "too many requests", Retryable: true, Cooldown: true}
}

func paramErr() *provider.ProviderError {
	return &provider.ProviderError{HTTPStatus: 400, Code: "bad_request", Message: "invalid recipient", Retryable: false, DisableChannel: false, Cooldown: false}
}

func ptrTime(t time.Time) *time.Time { return &t }

func wantNoErr(t *testing.T, rerr *Err) {
	t.Helper()
	if rerr != nil {
		t.Fatalf("unexpected err: status=%d type=%s msg=%s", rerr.HTTPStatus, rerr.Type, rerr.Message)
	}
}

// ---------- 用例 ----------

// 惰性额度重置：过期 day/month anchor 清零后重新累计，并写回当天/当月 anchor。
func TestLazyQuotaReset(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{
		Name:                 "c1",
		DailyLimitOverride:   10,
		MonthlyLimitOverride: 100,
		UsedDay:              7,
		DayAnchor:            "2000-01-01",
		UsedMonth:            55,
		MonthAnchor:          "2000-01",
	})

	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	wantNoErr(t, rerr)
	if out.ChannelName != "c1" {
		t.Errorf("channel = %q, want c1", out.ChannelName)
	}
	ch := reloadChannel(t, db, "c1")
	if ch.UsedDay != 1 || ch.UsedMonth != 1 {
		t.Errorf("used_day=%d used_month=%d, want 1/1（过期 anchor 清零后重新累计）", ch.UsedDay, ch.UsedMonth)
	}
	today := time.Now().Format(dayLayout)
	month := time.Now().Format(monthLayout)
	if ch.DayAnchor != today || ch.MonthAnchor != month {
		t.Errorf("anchors day=%q month=%q, want %q/%q", ch.DayAnchor, ch.MonthAnchor, today, month)
	}
}

// 额度耗尽：当日额度用尽的候选全部被剔除 → 429 quota_exhausted，不调用适配器、不写日志。
func TestQuotaExhaustedReturns429(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	today := time.Now().Format(dayLayout)
	addChannel(t, db, &model.Channel{Name: "c1", DailyLimitOverride: 10, UsedDay: 10, DayAnchor: today})
	addChannel(t, db, &model.Channel{Name: "c2", DailyLimitOverride: 5, UsedDay: 5, DayAnchor: today})

	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	if out != nil {
		t.Errorf("output = %+v, want nil", out)
	}
	if rerr == nil || rerr.HTTPStatus != 429 || rerr.Type != ErrQuotaExhausted {
		t.Fatalf("err = %+v, want 429 quota_exhausted", rerr)
	}
	if len(tr.calls) != 0 {
		t.Errorf("calls = %v, want none", tr.calls)
	}
	var logs int64
	db.Model(&model.SendLog{}).Count(&logs)
	if logs != 0 {
		t.Errorf("send logs = %d, want 0（未尝试发送不记日志）", logs)
	}
}

// AllowedDomains 域名过滤：不匹配的渠道被剔除；全部不匹配 → 429；匹配忽略大小写。
func TestAllowedDomainsFilter(t *testing.T) {
	t.Run("mismatched_channel_skipped", func(t *testing.T) {
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 5)
		tr := &fakeTransport{name: "fake", fn: func(ch *model.Channel) (*provider.Result, *provider.ProviderError) {
			if ch.Name == "a" {
				return nil, netErr("skip me")
			}
			return &provider.Result{ProviderMessageID: "ok-" + ch.Name}, nil
		}}
		addChannel(t, db, &model.Channel{Name: "a", AllowedDomains: " other.com , extra.org "})
		addChannel(t, db, &model.Channel{Name: "b"})

		out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
		wantNoErr(t, rerr)
		if out.ChannelName != "b" {
			t.Errorf("channel = %q, want b（a 因域名不匹配被剔除）", out.ChannelName)
		}
		if strings.Join(tr.calls, ",") != "b" {
			t.Errorf("calls = %v, want [b]", tr.calls)
		}
	})

	t.Run("all_filtered_returns_429", func(t *testing.T) {
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 5)
		tr := &fakeTransport{name: "fake"}
		addChannel(t, db, &model.Channel{Name: "a", AllowedDomains: "other.com"})

		_, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
		if rerr == nil || rerr.HTTPStatus != 429 || rerr.Type != ErrQuotaExhausted {
			t.Fatalf("err = %+v, want 429 quota_exhausted", rerr)
		}
		if len(tr.calls) != 0 {
			t.Errorf("calls = %v, want none", tr.calls)
		}
	})

	t.Run("case_insensitive_match", func(t *testing.T) {
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 5)
		tr := &fakeTransport{name: "fake"}
		addChannel(t, db, &model.Channel{Name: "a", AllowedDomains: "EXAMPLE.com"})

		out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
		wantNoErr(t, rerr)
		if out.ChannelName != "a" {
			t.Errorf("channel = %q, want a（域名匹配忽略大小写）", out.ChannelName)
		}
	})
}

// 指定 channel 语义：不存在/域名不匹配 → 400；停用/冷却未到/超额 → 429；正常可用 → 成功。
func TestSpecifiedChannelSemantics(t *testing.T) {
	newEnv := func(t *testing.T) (*gorm.DB, *fakeTransport) {
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 5)
		return db, &fakeTransport{name: "fake"}
	}
	input := func(channel string, tr *fakeTransport) *Input {
		in := sendInput("user@example.com", tr)
		in.ChannelName = channel
		return in
	}

	t.Run("not_found_returns_400", func(t *testing.T) {
		db, tr := newEnv(t)
		out, rerr := Send(context.Background(), db, input("ghost", tr))
		if out != nil || rerr == nil || rerr.HTTPStatus != 400 || rerr.Type != ErrInvalidRequest {
			t.Fatalf("out=%+v err=%+v, want 400 invalid_request", out, rerr)
		}
	})

	t.Run("domain_mismatch_returns_400", func(t *testing.T) {
		db, tr := newEnv(t)
		addChannel(t, db, &model.Channel{Name: "c1", AllowedDomains: "other.com"})
		out, rerr := Send(context.Background(), db, input("c1", tr))
		if out != nil || rerr == nil || rerr.HTTPStatus != 400 || rerr.Type != ErrInvalidRequest {
			t.Fatalf("out=%+v err=%+v, want 400 invalid_request", out, rerr)
		}
		if len(tr.calls) != 0 {
			t.Errorf("calls = %v, want none", tr.calls)
		}
	})

	t.Run("manual_disabled_returns_429", func(t *testing.T) {
		db, tr := newEnv(t)
		addChannel(t, db, &model.Channel{Name: "c1", Status: model.ChannelManualDisabled})
		out, rerr := Send(context.Background(), db, input("c1", tr))
		if out != nil || rerr == nil || rerr.HTTPStatus != 429 || rerr.Type != ErrQuotaExhausted {
			t.Fatalf("out=%+v err=%+v, want 429 quota_exhausted", out, rerr)
		}
	})

	t.Run("auto_disabled_returns_429", func(t *testing.T) {
		db, tr := newEnv(t)
		addChannel(t, db, &model.Channel{Name: "c1", Status: model.ChannelAutoDisabled})
		out, rerr := Send(context.Background(), db, input("c1", tr))
		if out != nil || rerr == nil || rerr.HTTPStatus != 429 || rerr.Type != ErrQuotaExhausted {
			t.Fatalf("out=%+v err=%+v, want 429 quota_exhausted", out, rerr)
		}
	})

	t.Run("cooldown_active_returns_429", func(t *testing.T) {
		db, tr := newEnv(t)
		addChannel(t, db, &model.Channel{Name: "c1", Status: model.ChannelCooldown,
			CooldownUntil: ptrTime(time.Now().Add(10 * time.Minute))})
		out, rerr := Send(context.Background(), db, input("c1", tr))
		if out != nil || rerr == nil || rerr.HTTPStatus != 429 || rerr.Type != ErrQuotaExhausted {
			t.Fatalf("out=%+v err=%+v, want 429 quota_exhausted", out, rerr)
		}
	})

	t.Run("quota_exhausted_returns_429", func(t *testing.T) {
		db, tr := newEnv(t)
		addChannel(t, db, &model.Channel{Name: "c1", DailyLimitOverride: 5, UsedDay: 5,
			DayAnchor: time.Now().Format(dayLayout)})
		out, rerr := Send(context.Background(), db, input("c1", tr))
		if out != nil || rerr == nil || rerr.HTTPStatus != 429 || rerr.Type != ErrQuotaExhausted {
			t.Fatalf("out=%+v err=%+v, want 429 quota_exhausted", out, rerr)
		}
	})

	t.Run("by_name_success", func(t *testing.T) {
		db, tr := newEnv(t)
		addChannel(t, db, &model.Channel{Name: "c1"})
		out, rerr := Send(context.Background(), db, input("c1", tr))
		wantNoErr(t, rerr)
		if out == nil || out.ChannelName != "c1" || out.ChannelID == 0 {
			t.Errorf("output = %+v, want 指定渠道 c1", out)
		}
	})
}

// 指定渠道冷却已到期：视为可用并立即复位为 ENABLED。
func TestSpecifiedExpiredCooldownRejoins(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "c1", Status: model.ChannelCooldown,
		CooldownUntil: ptrTime(time.Now().Add(-time.Minute))})

	in := sendInput("user@example.com", tr)
	in.ChannelName = "c1"
	out, rerr := Send(context.Background(), db, in)
	wantNoErr(t, rerr)
	if out.ChannelName != "c1" {
		t.Errorf("channel = %q, want c1", out.ChannelName)
	}
	ch := reloadChannel(t, db, "c1")
	if ch.Status != model.ChannelEnabled || ch.CooldownUntil != nil {
		t.Errorf("status=%d cooldown_until=%v, want enabled/nil（冷却到期立即复位）", ch.Status, ch.CooldownUntil)
	}
}

// 冷却到期（cooldown_until <= now）的渠道视为可用并立即复位为 ENABLED。
func TestCooldownExpiredChannelRejoins(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "c1", Status: model.ChannelCooldown,
		CooldownUntil: ptrTime(time.Now().Add(-time.Minute))})

	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	wantNoErr(t, rerr)
	if out.ChannelName != "c1" {
		t.Errorf("channel = %q, want c1", out.ChannelName)
	}
	ch := reloadChannel(t, db, "c1")
	if ch.Status != model.ChannelEnabled || ch.CooldownUntil != nil || ch.UsedDay != 1 {
		t.Errorf("status=%d cooldown_until=%v used_day=%d, want enabled/nil/1", ch.Status, ch.CooldownUntil, ch.UsedDay)
	}
}

// failover：第一家网络错误 → 换下一家成功；失败方额度不计、累计失败 1 次。
func TestFailoverOnRetryableError(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake", fn: func(ch *model.Channel) (*provider.Result, *provider.ProviderError) {
		if ch.Name == "a" {
			return nil, netErr("dial timeout")
		}
		return &provider.Result{ProviderMessageID: "ok-b"}, nil
	}}
	addChannel(t, db, &model.Channel{Name: "a"})
	addChannel(t, db, &model.Channel{Name: "b"})

	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	wantNoErr(t, rerr)
	if out.ChannelName != "b" || out.ProviderMessageID != "ok-b" {
		t.Errorf("output = %+v, want channel b / ok-b", out)
	}
	if out.MessageID == "" {
		t.Error("message id empty")
	}
	if strings.Join(tr.calls, ",") != "a,b" {
		t.Errorf("calls = %v, want [a,b]", tr.calls)
	}
	a := reloadChannel(t, db, "a")
	if a.ConsecutiveFailures != 1 || a.UsedDay != 0 || a.LastError == "" {
		t.Errorf("a: cf=%d used_day=%d last_error=%q, want 1/0/非空", a.ConsecutiveFailures, a.UsedDay, a.LastError)
	}
	if a.Status != model.ChannelEnabled {
		t.Errorf("a status = %d, want enabled（未达阈值不禁用）", a.Status)
	}
	b := reloadChannel(t, db, "b")
	if b.UsedDay != 1 || b.LastUsedAt == nil || b.ConsecutiveFailures != 0 {
		t.Errorf("b: used_day=%d last_used_at=%v cf=%d, want 1/非nil/0", b.UsedDay, b.LastUsedAt, b.ConsecutiveFailures)
	}
}

// 连续失败达阈值自动禁用：第 threshold 次后 AUTO_DISABLED，之后不再被选中。
func TestConsecutiveFailuresAutoDisable(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 2) // 阈值=2
	tr := &fakeTransport{name: "fake", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		return nil, netErr("dial timeout")
	}}
	addChannel(t, db, &model.Channel{Name: "c1"})

	for i := 1; i <= 2; i++ {
		out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
		if out != nil || rerr == nil || rerr.HTTPStatus != 502 || rerr.Type != ErrProviderError {
			t.Fatalf("send #%d: out=%+v err=%+v, want 502 provider_error", i, out, rerr)
		}
	}
	ch := reloadChannel(t, db, "c1")
	if ch.ConsecutiveFailures != 2 || ch.Status != model.ChannelAutoDisabled {
		t.Fatalf("cf=%d status=%d, want 2/auto_disabled", ch.ConsecutiveFailures, ch.Status)
	}
	if !strings.Contains(ch.AutoDisabledReason, "连续失败") {
		t.Errorf("reason = %q, want 含「连续失败」", ch.AutoDisabledReason)
	}
	// 已自动禁用：不再参与轮换 → 429。
	_, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	if rerr == nil || rerr.HTTPStatus != 429 || rerr.Type != ErrQuotaExhausted {
		t.Fatalf("err = %+v, want 429 quota_exhausted", rerr)
	}
	if len(tr.calls) != 2 {
		t.Errorf("calls = %d, want 2（禁用后不再调用）", len(tr.calls))
	}
}

// 平台 429：渠道进入 COOLDOWN（cooldown_until = now + CooldownSeconds）并 failover 到下一家。
func TestCooldownOnRateLimit(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake", fn: func(ch *model.Channel) (*provider.Result, *provider.ProviderError) {
		if ch.Name == "a" {
			return nil, rateErr()
		}
		return &provider.Result{ProviderMessageID: "ok-b"}, nil
	}}
	addChannel(t, db, &model.Channel{Name: "a"})
	addChannel(t, db, &model.Channel{Name: "b"})

	before := time.Now()
	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	after := time.Now()
	wantNoErr(t, rerr)
	if out.ChannelName != "b" {
		t.Errorf("channel = %q, want b（429 后换下一家）", out.ChannelName)
	}
	ch := reloadChannel(t, db, "a")
	if ch.Status != model.ChannelCooldown {
		t.Fatalf("status = %d, want cooldown", ch.Status)
	}
	if ch.CooldownUntil == nil || ch.CooldownUntil.Before(before.Add(60*time.Second)) || ch.CooldownUntil.After(after.Add(60*time.Second)) {
		t.Errorf("cooldown_until = %v, want ≈ now+60s", ch.CooldownUntil)
	}
	if !strings.Contains(ch.LastError, "too many requests") {
		t.Errorf("last_error = %q, want 含平台错误", ch.LastError)
	}
}

// 平台 401：立即 AUTO_DISABLED（原因+last_error），不消耗额度，failover 继续。
func TestAuthErrorDisablesImmediately(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake", fn: func(ch *model.Channel) (*provider.Result, *provider.ProviderError) {
		if ch.Name == "a" {
			return nil, authErr(401)
		}
		return &provider.Result{ProviderMessageID: "ok-b"}, nil
	}}
	addChannel(t, db, &model.Channel{Name: "a"})
	addChannel(t, db, &model.Channel{Name: "b"})

	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	wantNoErr(t, rerr)
	if out.ChannelName != "b" {
		t.Errorf("channel = %q, want b", out.ChannelName)
	}
	ch := reloadChannel(t, db, "a")
	if ch.Status != model.ChannelAutoDisabled || ch.UsedDay != 0 {
		t.Errorf("status=%d used_day=%d, want auto_disabled/0", ch.Status, ch.UsedDay)
	}
	if !strings.Contains(ch.AutoDisabledReason, "401") {
		t.Errorf("reason = %q, want 含 401", ch.AutoDisabledReason)
	}
	if ch.LastError == "" {
		t.Error("last_error empty")
	}
}

// 参数类 4xx：不换渠道、不改渠道状态，直接 400 invalid_request。
func TestParamErrorNoFailover(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		return nil, paramErr()
	}}
	addChannel(t, db, &model.Channel{Name: "a"})
	addChannel(t, db, &model.Channel{Name: "b"})

	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	if out != nil || rerr == nil || rerr.HTTPStatus != 400 || rerr.Type != ErrInvalidRequest {
		t.Fatalf("out=%+v err=%+v, want 400 invalid_request", out, rerr)
	}
	if !strings.Contains(rerr.Message, "invalid recipient") {
		t.Errorf("message = %q, want 含平台错误", rerr.Message)
	}
	if strings.Join(tr.calls, ",") != "a" {
		t.Errorf("calls = %v, want [a]（不 failover）", tr.calls)
	}
	ch := reloadChannel(t, db, "a")
	if ch.Status != model.ChannelEnabled || ch.ConsecutiveFailures != 0 || ch.LastError != "" {
		t.Errorf("a: status=%d cf=%d last_error=%q, want 渠道状态不变", ch.Status, ch.ConsecutiveFailures, ch.LastError)
	}
	var log model.SendLog
	if err := db.First(&log).Error; err != nil {
		t.Fatalf("send log missing: %v", err)
	}
	if log.Status != model.SendStatusFailed || log.Attempts != 1 || !strings.Contains(log.Error, "invalid recipient") {
		t.Errorf("log: status=%d attempts=%d error=%q, want failed/1/含平台错误", log.Status, log.Attempts, log.Error)
	}
}

// 额度只在受理（accepted）时 +1：失败尝试不计，受理一次计一次。
func TestQuotaOnlyIncrementsOnAccept(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake", fn: func(ch *model.Channel) (*provider.Result, *provider.ProviderError) {
		if ch.Name == "bad" {
			return nil, netErr("dial timeout")
		}
		return &provider.Result{ProviderMessageID: "ok-" + ch.Name}, nil
	}}
	addChannel(t, db, &model.Channel{Name: "bad"})
	addChannel(t, db, &model.Channel{Name: "good"})

	// 第一次：bad 失败（不计额度）→ good 成功（+1）。
	if _, rerr := Send(context.Background(), db, sendInput("user@example.com", tr)); rerr != nil {
		t.Fatalf("send #1: %v", rerr)
	}
	if ch := reloadChannel(t, db, "bad"); ch.UsedDay != 0 {
		t.Errorf("bad used_day = %d, want 0（失败不消耗额度）", ch.UsedDay)
	}
	if ch := reloadChannel(t, db, "good"); ch.UsedDay != 1 {
		t.Errorf("good used_day = %d, want 1", ch.UsedDay)
	}

	// 第二次成功：再 +1。
	if _, rerr := Send(context.Background(), db, sendInput("user@example.com", tr)); rerr != nil {
		t.Fatalf("send #2: %v", rerr)
	}
	if ch := reloadChannel(t, db, "good"); ch.UsedDay != 2 {
		t.Errorf("good used_day = %d, want 2（每次受理 +1）", ch.UsedDay)
	}
}

// TestSendPanicReleasesQuota panic 安全（O8）：transport.Send panic 时（api 层
// Recovery 兜底进程不崩）已预留的额度被 defer 配对释放，不静默多计；释放后额度
// 可再次预留。panic 照常上抛给调用方（不吞）。
func TestSendPanicReleasesQuota(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		panic("boom: 模拟适配器 panic")
	}}
	addChannel(t, db, &model.Channel{Name: "pk", DailyLimitOverride: 3, MonthlyLimitOverride: 3})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic should propagate to caller (由 api 层 Recovery 兜底)")
			}
		}()
		_, _ = Send(context.Background(), db, sendInput("user@example.com", tr))
	}()

	if ch := reloadChannel(t, db, "pk"); ch.UsedDay != 0 || ch.UsedMonth != 0 {
		t.Errorf("quota not released after panic: used_day=%d used_month=%d, want 0/0",
			ch.UsedDay, ch.UsedMonth)
	}
	// 释放后额度可再次预留（未被 panic 多计吃掉）
	if ch := reloadChannel(t, db, "pk"); !reserveQuota(db, &ch, time.Now()) {
		t.Error("reserve after panic release should succeed")
	}
}

// SendLog 落库字段：uuid 贯穿、渠道冗余快照、Attempts/LatencyMs、IsTest 透传、正文快照。
func TestSendLogFields(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake", fn: func(ch *model.Channel) (*provider.Result, *provider.ProviderError) {
		if ch.Name == "a" {
			return nil, netErr("dial timeout")
		}
		return &provider.Result{ProviderMessageID: "ok-b"}, nil
	}}
	addChannel(t, db, &model.Channel{Name: "a"})
	addChannel(t, db, &model.Channel{Name: "b"})

	in := sendInput("user@example.com", tr)
	in.IsTest = true
	in.APIKeyID = 42
	in.Message.HTML = "<p>hi</p>"
	out, rerr := Send(context.Background(), db, in)
	wantNoErr(t, rerr)

	var logs []model.SendLog
	if err := db.Find(&logs).Error; err != nil {
		t.Fatalf("query logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1（一次请求一条日志）", len(logs))
	}
	log := logs[0]
	if log.MessageID != out.MessageID {
		t.Errorf("message_id = %q, want %q", log.MessageID, out.MessageID)
	}
	if log.ApiKeyID != 42 || log.ChannelID != out.ChannelID || log.ChannelName != "b" || log.Provider != "fake" {
		t.Errorf("log api_key_id=%d channel_id=%d name=%q provider=%q", log.ApiKeyID, log.ChannelID, log.ChannelName, log.Provider)
	}
	if !log.IsTest {
		t.Error("is_test = false, want true（透传）")
	}
	if log.Status != model.SendStatusSent {
		t.Errorf("status = %d, want sent", log.Status)
	}
	if log.FromAddr != "user@example.com" || log.Subject != "测试邮件" || log.TextBody != "hello" || log.HTMLBody != "<p>hi</p>" {
		t.Errorf("snapshot fields mismatch: from=%q subject=%q text=%q html=%q", log.FromAddr, log.Subject, log.TextBody, log.HTMLBody)
	}
	var to []string
	if err := json.Unmarshal([]byte(log.ToAddrs), &to); err != nil || len(to) != 1 || to[0] != "to@example.com" {
		t.Errorf("to_addrs = %q (err=%v), want [to@example.com]", log.ToAddrs, err)
	}
	if log.ProviderMessageID != "ok-b" || log.Error != "" {
		t.Errorf("provider_message_id=%q error=%q, want ok-b/空", log.ProviderMessageID, log.Error)
	}
	if log.Attempts != 2 {
		t.Errorf("attempts = %d, want 2（failover 一次后成功）", log.Attempts)
	}
	if log.LatencyMs < 0 {
		t.Errorf("latency_ms = %d, want >= 0", log.LatencyMs)
	}
}

// 重试耗尽：attempts = min(max_attempts, 候选数)，超出部分不尝试 → 502 附最后错误。
func TestRetryExhaustedReturns502AndCapsAttempts(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 2, 60, 5) // 最多尝试 2 家
	tr := &fakeTransport{name: "fake", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		return nil, netErr("dial timeout")
	}}
	addChannel(t, db, &model.Channel{Name: "a"})
	addChannel(t, db, &model.Channel{Name: "b"})
	addChannel(t, db, &model.Channel{Name: "c"})

	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	if out != nil || rerr == nil || rerr.HTTPStatus != 502 || rerr.Type != ErrProviderError {
		t.Fatalf("out=%+v err=%+v, want 502 provider_error", out, rerr)
	}
	if !strings.Contains(rerr.Message, "dial timeout") {
		t.Errorf("message = %q, want 附最后一次平台错误", rerr.Message)
	}
	if strings.Join(tr.calls, ",") != "a,b" {
		t.Errorf("calls = %v, want [a,b]（受 max_attempts 限制）", tr.calls)
	}
	c := reloadChannel(t, db, "c")
	if c.Status != model.ChannelEnabled || c.ConsecutiveFailures != 0 {
		t.Errorf("c: status=%d cf=%d, want 未被尝试", c.Status, c.ConsecutiveFailures)
	}
	var log model.SendLog
	if err := db.First(&log).Error; err != nil {
		t.Fatalf("send log missing: %v", err)
	}
	if log.Status != model.SendStatusFailed || log.Attempts != 2 || log.ChannelName != "b" || log.Error == "" {
		t.Errorf("log: status=%d attempts=%d channel=%q error=%q, want failed/2/b/非空",
			log.Status, log.Attempts, log.ChannelName, log.Error)
	}
}

// priority 策略：priority 升序优先于 last_used_at 与创建顺序。
func TestPriorityOrdering(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 5, 60, 10)
	tr := &fakeTransport{name: "fake", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		return nil, netErr("dial timeout")
	}}
	addChannel(t, db, &model.Channel{Name: "p2", Priority: 2, LastUsedAt: ptrTime(time.Now().Add(-3 * time.Hour))}) // 最久未用但优先级最低
	addChannel(t, db, &model.Channel{Name: "p0", Priority: 0, LastUsedAt: ptrTime(time.Now().Add(-time.Hour))})
	addChannel(t, db, &model.Channel{Name: "p1", Priority: 1})

	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	if out != nil || rerr == nil || rerr.HTTPStatus != 502 {
		t.Fatalf("out=%+v err=%+v, want 502", out, rerr)
	}
	if strings.Join(tr.calls, ",") != "p0,p1,p2" {
		t.Errorf("calls = %v, want [p0,p1,p2]（priority 升序）", tr.calls)
	}
}

// round_robin 策略：仅按 last_used_at 升序（nil 最久未用排最前），忽略 priority。
func TestRoundRobinOrdering(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "round_robin", 5, 60, 10)
	tr := &fakeTransport{name: "fake", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		return nil, netErr("dial timeout")
	}}
	addChannel(t, db, &model.Channel{Name: "r1", Priority: 0, LastUsedAt: ptrTime(time.Now().Add(-1 * time.Hour))})
	addChannel(t, db, &model.Channel{Name: "r2", Priority: -5, LastUsedAt: ptrTime(time.Now().Add(-2 * time.Hour))})
	addChannel(t, db, &model.Channel{Name: "r3", Priority: 9}) // 从未使用

	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	if out != nil || rerr == nil || rerr.HTTPStatus != 502 {
		t.Fatalf("out=%+v err=%+v, want 502", out, rerr)
	}
	if strings.Join(tr.calls, ",") != "r3,r2,r1" {
		t.Errorf("calls = %v, want [r3,r2,r1]（last_used_at 升序，nil 最先）", tr.calls)
	}
}

// ---------- 并发语义（P1-1/P1-3）与渠道默认发件人回退（P1-4） ----------

// P1-1 并发额度：单渠道 limit=10，20 个并发请求 → 恰好受理 10 个、其余 429，
// used_day == 受理数。条件 UPDATE 预留保证判定原子：读快照判定的旧实现会让
// 两个并发请求都从 used=limit-1 通过而超额。
func TestConcurrentQuotaReservation(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "c1", DailyLimitOverride: 10})

	const total = 20
	var accepted, quotaErrs int64
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
			switch {
			case rerr == nil:
				if out == nil || out.ChannelName != "c1" {
					t.Errorf("output = %+v, want c1", out)
				}
				atomic.AddInt64(&accepted, 1)
			case rerr.HTTPStatus == 429 && rerr.Type == ErrQuotaExhausted:
				atomic.AddInt64(&quotaErrs, 1)
			default:
				t.Errorf("unexpected err: status=%d type=%s msg=%s", rerr.HTTPStatus, rerr.Type, rerr.Message)
			}
		}()
	}
	wg.Wait()

	if accepted != 10 {
		t.Errorf("accepted = %d, want 10（limit=10，不超额）", accepted)
	}
	if quotaErrs != total-10 {
		t.Errorf("429 count = %d, want %d（输掉预留竞争的请求视为超额）", quotaErrs, total-10)
	}
	if len(tr.calls) != int(accepted) {
		t.Errorf("calls = %d, want %d（被拒请求不调用适配器）", len(tr.calls), accepted)
	}
	ch := reloadChannel(t, db, "c1")
	if ch.UsedDay != int(accepted) {
		t.Errorf("used_day = %d, want %d（额度与受理数严格一致）", ch.UsedDay, accepted)
	}
}

// P1-3 并发连续失败：原子递增不丢计数。
func TestConcurrentFailuresCountAtomically(t *testing.T) {
	t.Run("count_never_lost", func(t *testing.T) {
		// 阈值放大到不触发禁用：10 个并发失败请求，每次尝试恰好 +1 → 计数 == 尝试数。
		// 读-改-写旧实现下并发自增互相覆盖，计数必然小于尝试数。
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 100)
		tr := &fakeTransport{name: "fake", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
			return nil, netErr("dial timeout")
		}}
		addChannel(t, db, &model.Channel{Name: "c1"})

		const total = 10
		var wg sync.WaitGroup
		for i := 0; i < total; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = Send(context.Background(), db, sendInput("user@example.com", tr))
			}()
		}
		wg.Wait()

		ch := reloadChannel(t, db, "c1")
		if ch.ConsecutiveFailures != len(tr.calls) || ch.ConsecutiveFailures != total {
			t.Errorf("cf = %d calls = %d, want 均为 %d（读-改-写并发会丢计数，原子递增不丢）",
				ch.ConsecutiveFailures, len(tr.calls), total)
		}
		if ch.Status != model.ChannelEnabled {
			t.Errorf("status = %d, want enabled（未达阈值不禁用）", ch.Status)
		}
	})

	t.Run("auto_disabled_at_threshold", func(t *testing.T) {
		// 阈值 5：先顺序失败 4 次抬高基线（仍启用），再并发 10 个失败请求。
		// 触发第 5 次失败的请求必然在启用期间收集到候选 → 最终必为 AUTO_DISABLED；
		// 且计数与实际发生的失败尝试次数严格一致（不被并发覆盖丢失）。
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 5)
		tr := &fakeTransport{name: "fake", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
			return nil, netErr("dial timeout")
		}}
		addChannel(t, db, &model.Channel{Name: "c1"})
		for i := 0; i < 4; i++ {
			_, _ = Send(context.Background(), db, sendInput("user@example.com", tr))
		}

		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = Send(context.Background(), db, sendInput("user@example.com", tr))
			}()
		}
		wg.Wait()

		ch := reloadChannel(t, db, "c1")
		if ch.Status != model.ChannelAutoDisabled || !strings.Contains(ch.AutoDisabledReason, "连续失败") {
			t.Fatalf("status=%d reason=%q, want auto_disabled", ch.Status, ch.AutoDisabledReason)
		}
		if ch.ConsecutiveFailures != len(tr.calls) || ch.ConsecutiveFailures < 5 {
			t.Errorf("cf = %d calls = %d, want 相等且 ≥5（计数不被覆盖丢失）",
				ch.ConsecutiveFailures, len(tr.calls))
		}
	})
}

// P1-4 渠道默认发件人回退（轮换路径）：每个候选独立回退互不污染；
// 请求已提供 from 时不回退；无 from 的请求不因 AllowedDomains 被误剔除。
func TestChannelDefaultFromFallback(t *testing.T) {
	t.Run("per_candidate_isolation", func(t *testing.T) {
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 5)
		tr := &fakeTransport{name: "fake", fn: func(ch *model.Channel) (*provider.Result, *provider.ProviderError) {
			if ch.Name == "a" {
				return nil, netErr("dial timeout")
			}
			return &provider.Result{ProviderMessageID: "ok-b"}, nil
		}}
		addChannel(t, db, &model.Channel{Name: "a", FromEmail: "a@alpha.com", FromName: "Alpha"})
		addChannel(t, db, &model.Channel{Name: "b", FromEmail: "b@beta.com"})

		in := sendInput("", tr) // 请求不带 from
		out, rerr := Send(context.Background(), db, in)
		wantNoErr(t, rerr)
		if out.ChannelName != "b" {
			t.Errorf("channel = %q, want b", out.ChannelName)
		}
		// 共享 msg 未被污染（下一候选不继承上一候选的 from）。
		if in.Message.FromAddr != "" || in.Message.FromName != "" {
			t.Errorf("shared msg polluted: from=%q/%q", in.Message.FromName, in.Message.FromAddr)
		}
		if len(tr.msgs) != 2 {
			t.Fatalf("msgs = %d, want 2", len(tr.msgs))
		}
		if tr.msgs[0].FromAddr != "a@alpha.com" || tr.msgs[0].FromName != "Alpha" {
			t.Errorf("candidate a from = %q/%q, want a@alpha.com/Alpha", tr.msgs[0].FromName, tr.msgs[0].FromAddr)
		}
		if tr.msgs[1].FromAddr != "b@beta.com" || tr.msgs[1].FromName != "" {
			t.Errorf("candidate b from = %q/%q, want b@beta.com/空（独立回退）", tr.msgs[1].FromName, tr.msgs[1].FromAddr)
		}
		// 日志记录实际发送用的 from。
		var log model.SendLog
		if err := db.First(&log).Error; err != nil {
			t.Fatalf("send log missing: %v", err)
		}
		if log.FromAddr != "b@beta.com" {
			t.Errorf("log from = %q, want b@beta.com", log.FromAddr)
		}
	})

	t.Run("no_from_not_filtered_by_allowed_domains", func(t *testing.T) {
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 5)
		tr := &fakeTransport{name: "fake"}
		addChannel(t, db, &model.Channel{Name: "c1",
			AllowedDomains: "mail.example.com", FromEmail: "s@mail.example.com"})

		out, rerr := Send(context.Background(), db, sendInput("", tr))
		wantNoErr(t, rerr)
		if out.ChannelName != "c1" {
			t.Errorf("channel = %q, want c1（无 from 请求不因 AllowedDomains 被误剔除）", out.ChannelName)
		}
		if tr.msgs[0].FromAddr != "s@mail.example.com" {
			t.Errorf("from = %q, want 渠道默认 s@mail.example.com", tr.msgs[0].FromAddr)
		}
	})

	t.Run("explicit_from_wins", func(t *testing.T) {
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 5)
		tr := &fakeTransport{name: "fake"}
		addChannel(t, db, &model.Channel{Name: "c1", FromEmail: "default@example.com"})

		if _, rerr := Send(context.Background(), db, sendInput("user@example.com", tr)); rerr != nil {
			t.Fatalf("send: %v", rerr)
		}
		if tr.msgs[0].FromAddr != "user@example.com" {
			t.Errorf("from = %q, want user@example.com（请求提供的 from 优先于渠道默认）", tr.msgs[0].FromAddr)
		}
	})
}

// ---------- 补充关键路径：异常输入/适配器缺失/月额度/排序并列/设置归一化 ----------

// 缺少消息内容（in 或 in.Message 为 nil）→ 400，不触库不发送。
func TestSendNilInputReturns400(t *testing.T) {
	db := newDB(t)
	out, rerr := Send(context.Background(), db, nil)
	if out != nil || rerr == nil || rerr.HTTPStatus != 400 || rerr.Type != ErrInvalidRequest {
		t.Fatalf("out=%+v err=%+v, want 400 invalid_request", out, rerr)
	}
	out, rerr = Send(context.Background(), db, &Input{})
	if out != nil || rerr == nil || rerr.HTTPStatus != 400 {
		t.Fatalf("Message=nil: out=%+v err=%+v, want 400", out, rerr)
	}
}

// 适配器缺失（Resolver 查不到平台）视为可重试故障：计入尝试次数、按阈值累计失败
// （阈值=1 时立即自动禁用），并 failover 到下一候选成功——回归锁定：适配器缺失
// 不能静默吞掉尝试次数，也不能中断整个轮换。
func TestMissingTransportFailsOver(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 1) // 阈值 1：一次失败即禁用
	tr := &fakeTransport{name: "fake"}
	addChannel(t, db, &model.Channel{Name: "ghost", Provider: "ghost"}) // 无适配器
	addChannel(t, db, &model.Channel{Name: "real", Provider: "fake"})

	out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
	wantNoErr(t, rerr)
	if out.ChannelName != "real" {
		t.Fatalf("channel = %q, want real（ghost 适配器缺失后 failover）", out.ChannelName)
	}
	if strings.Join(tr.calls, ",") != "real" {
		t.Errorf("calls = %v, want [real]（ghost 不应触发 transport.Send）", tr.calls)
	}
	ghost := reloadChannel(t, db, "ghost")
	if ghost.Status != model.ChannelAutoDisabled || ghost.ConsecutiveFailures != 1 {
		t.Errorf("ghost 状态=%d 连续失败=%d, want AUTO_DISABLED/1", ghost.Status, ghost.ConsecutiveFailures)
	}
	if ghost.UsedDay != 0 {
		t.Errorf("ghost used_day = %d, want 0（未预留额度）", ghost.UsedDay)
	}
}

// 月维度额度过滤：当月额度耗尽（日维度未用）的候选被剔除——此前仅覆盖日维度耗尽，
// 月度分支（quotaBlocked 第二判）漏测。
func TestMonthlyQuotaBlocksCandidate(t *testing.T) {
	t.Run("exhausted_channel_filtered", func(t *testing.T) {
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 5)
		tr := &fakeTransport{name: "fake"}
		now := time.Now()
		addChannel(t, db, &model.Channel{
			Name: "c1", MonthlyLimitOverride: 5, UsedMonth: 5,
			MonthAnchor: now.Format(monthLayout), DayAnchor: now.Format(dayLayout),
		})
		addChannel(t, db, &model.Channel{Name: "c2"})

		out, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
		wantNoErr(t, rerr)
		if out.ChannelName != "c2" {
			t.Fatalf("channel = %q, want c2（月额度耗尽的 c1 被过滤）", out.ChannelName)
		}
	})

	t.Run("all_exhausted_returns_429", func(t *testing.T) {
		db := newDB(t)
		putSetting(t, db, "priority", 3, 60, 5)
		tr := &fakeTransport{name: "fake"}
		now := time.Now()
		addChannel(t, db, &model.Channel{
			Name: "c1", MonthlyLimitOverride: 5, UsedMonth: 5,
			MonthAnchor: now.Format(monthLayout), DayAnchor: now.Format(dayLayout),
		})

		_, rerr := Send(context.Background(), db, sendInput("user@example.com", tr))
		if rerr == nil || rerr.HTTPStatus != 429 || rerr.Type != ErrQuotaExhausted {
			t.Fatalf("err = %+v, want 429 quota_exhausted", rerr)
		}
		if len(tr.calls) != 0 {
			t.Errorf("calls = %v, want none", tr.calls)
		}
	})
}

// domainAllowed 单元语义：空配置不限制；配置了 AllowedDomains 时空域名不匹配
// （P1-4 依赖调用方在 from 为空时跳过过滤，本断言锁定过滤函数本身的口径）。
func TestDomainAllowedUnit(t *testing.T) {
	cases := []struct {
		allowed, domain string
		want            bool
	}{
		{"", "x.com", true},                // 未配置 = 不限制
		{"  ", "x.com", true},              // 空白配置 = 不限制
		{"a.com,b.com", "", false},         // 配置了但域名为空 = 不匹配
		{"a.com", "a.com", true},           // 精确匹配
		{" A.com , B.com ", "a.com", true}, // 大小写与空白容错
		{"a.com", "b.com", false},          // 不在列表
	}
	for _, tc := range cases {
		if got := domainAllowed(tc.allowed, tc.domain); got != tc.want {
			t.Errorf("domainAllowed(%q, %q) = %v, want %v", tc.allowed, tc.domain, got, tc.want)
		}
	}
}

// 排序并列与逆序：last_used_at 相等时按 ID 升序兜底（结果稳定，failover 顺序可预期）；
// compareLastUsed 全分支口径（nil 约定 + 相等返回 0）。
func TestSortCandidatesTiebreak(t *testing.T) {
	t1 := time.Now().Add(-time.Hour)
	t2 := t1.Add(-time.Minute)
	if got := compareLastUsed(nil, &t1); got != -1 {
		t.Errorf("compareLastUsed(nil, t) = %d, want -1（nil 视为最久未用）", got)
	}
	if got := compareLastUsed(&t1, nil); got != 1 {
		t.Errorf("compareLastUsed(t, nil) = %d, want 1", got)
	}
	if got := compareLastUsed(nil, nil); got != 0 {
		t.Errorf("compareLastUsed(nil, nil) = %d, want 0", got)
	}
	if got := compareLastUsed(&t1, &t2); got != 1 {
		t.Errorf("compareLastUsed(t1>t2) = %d, want 1", got)
	}
	if got := compareLastUsed(&t1, &t1); got != 0 {
		t.Errorf("compareLastUsed(相等) = %d, want 0", got)
	}

	cs := []model.Channel{
		{ID: 3, Priority: 1, LastUsedAt: ptrTime(t1)},
		{ID: 1, Priority: 1, LastUsedAt: ptrTime(t1)},
		{ID: 2, Priority: 1, LastUsedAt: ptrTime(t1)},
	}
	sortCandidates(cs, "round_robin") // 并列 last_used_at → 仅剩 ID 兜底
	for i, ch := range cs {
		if ch.ID != uint(i+1) {
			t.Fatalf("round_robin 并列排序 = %v, want [1 2 3]", []uint{cs[0].ID, cs[1].ID, cs[2].ID})
		}
	}
}

// 设置归一化兜底：非法/越界设置不进引擎（rotation 仅认 round_robin、max_attempts≥1、
// cooldown 非负、失败阈值≥1 否则回默认 5）。
func TestNormalizeSettingsFallbacks(t *testing.T) {
	rotation, maxAttempts, cooldown, threshold := normalizeSettings(model.Setting{
		Rotation: "weird", MaxAttempts: -3, CooldownSeconds: -5, ConsecutiveFailureThreshold: 0,
	})
	if rotation != "priority" || maxAttempts != 1 || cooldown != 0 || threshold != 5 {
		t.Fatalf("非法设置归一化 = (%q,%d,%d,%d), want (priority,1,0,5)", rotation, maxAttempts, cooldown, threshold)
	}
	rotation, _, _, _ = normalizeSettings(model.Setting{Rotation: "round_robin", MaxAttempts: 3, CooldownSeconds: 60, ConsecutiveFailureThreshold: 2})
	if rotation != "round_robin" {
		t.Fatalf("round_robin 应被保留, got %q", rotation)
	}
}

// 未注入 Resolver 时走全局注册表（生产路径，此前测试全部注入 Resolver 而漏测）：
// provider=mock 渠道经 provider.Get 解析成功发送。
func TestSendUsesGlobalRegistry(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	addChannel(t, db, &model.Channel{Name: "m", Provider: "mock"})

	in := &Input{Message: &provider.Message{FromAddr: "user@example.com", To: []string{"to@example.com"}, Subject: "t", Text: "x"}, APIKeyID: 1}
	out, rerr := Send(context.Background(), db, in)
	wantNoErr(t, rerr)
	if !strings.HasPrefix(out.ProviderMessageID, "mock-") {
		t.Fatalf("provider id = %q, want mock-<uuid>（经全局注册表解析 mock 适配器）", out.ProviderMessageID)
	}
}

// 库不可用（连接已关闭）→ 500 server_error：fail-closed 且不 panic（设置读取失败即中止）。
func TestSendDBUnavailableReturns500(t *testing.T) {
	db := newDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	out, rerr := Send(context.Background(), db, sendInput("user@example.com", &fakeTransport{name: "fake"}))
	if out != nil || rerr == nil || rerr.HTTPStatus != 500 || rerr.Type != ErrServerError {
		t.Fatalf("out=%+v err=%+v, want 500 server_error", out, rerr)
	}
}
