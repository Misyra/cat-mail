// suppress_test.go 收件人抑制列表拦截（rotator.Send 单点 hook）测试：
// 命中拒发（to/cc/bcc 任一维度）、错误体形态（400/invalid_request/code=
// recipient_suppressed/命中地址可见）、未命中放行、读库故障 fail-open、
// 送达事件摄取产生的自动抑制同样被拦（收件人小写归一口径）。
package rotator

import (
	"context"
	"strings"
	"testing"

	"catmail/internal/model"
	"catmail/internal/provider"
)

// TestSendSuppressedRecipientBlocked 命中抑制列表：Send 返回 400 invalid_request
// 且 code=recipient_suppressed，message 列出命中地址与原因；mock 渠道未被调用
// （拒绝在门外）、未落 SendLog。
func TestSendSuppressedRecipientBlocked(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	addChannel(t, db, &model.Channel{Name: "m", Provider: "mock"})
	if err := db.Create(&model.Suppression{
		Email: "blocked@example.com", Reason: model.SuppressReasonBounce,
		ChannelName: "其他渠道", Detail: "hard bounce",
	}).Error; err != nil {
		t.Fatalf("seed suppression: %v", err)
	}
	if err := db.Create(&model.Suppression{
		Email: "also-blocked@example.com", Reason: model.SuppressReasonManual,
	}).Error; err != nil {
		t.Fatalf("seed suppression 2: %v", err)
	}

	tr := &fakeTransport{name: "mock", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		return &provider.Result{ProviderMessageID: "mock-1"}, nil
	}}
	in := sendInput("sender@example.com", tr)
	in.Message.To = []string{"ok@example.com", "Blocked@Example.com"} // 大小写不敏感命中
	in.Message.Bcc = []string{"also-blocked@example.com"}

	out, rerr := Send(context.Background(), db, in)
	if out != nil || rerr == nil {
		t.Fatalf("Send 应被抑制拦截，got out=%v err=%v", out, rerr)
	}
	if rerr.HTTPStatus != 400 || rerr.Type != ErrInvalidRequest {
		t.Errorf("err = %d/%s, want 400/invalid_request", rerr.HTTPStatus, rerr.Type)
	}
	if rerr.Code != CodeRecipientSuppressed {
		t.Errorf("err.Code = %q, want %q", rerr.Code, CodeRecipientSuppressed)
	}
	for _, want := range []string{"blocked@example.com", "bounce", "also-blocked@example.com"} {
		if !strings.Contains(rerr.Message, want) {
			t.Errorf("message 缺少命中信息 %q: %s", want, rerr.Message)
		}
	}
	if len(tr.calls) != 0 {
		t.Errorf("命中抑制后不应有任何渠道调用，got %v", tr.calls)
	}
	var n int64
	db.Model(&model.SendLog{}).Count(&n)
	if n != 0 {
		t.Errorf("命中抑制不应落发送日志，got %d 条", n)
	}
}

// TestSendSuppressionMissAllowsSend 未命中抑制列表：正常发送成功（拦截零误伤）。
func TestSendSuppressionMissAllowsSend(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	addChannel(t, db, &model.Channel{Name: "m", Provider: "mock"})
	if err := db.Create(&model.Suppression{
		Email: "other@example.com", Reason: model.SuppressReasonManual,
	}).Error; err != nil {
		t.Fatalf("seed suppression: %v", err)
	}
	tr := &fakeTransport{name: "mock", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		return &provider.Result{ProviderMessageID: "mock-ok"}, nil
	}}
	in := sendInput("sender@example.com", tr)
	in.Message.To = []string{"target@example.com"}

	out, rerr := Send(context.Background(), db, in)
	if rerr != nil || out == nil || out.MessageID == "" {
		t.Fatalf("未命中抑制应放行，got out=%v err=%v", out, rerr)
	}
}

// TestSendSuppressionFailOpen 抑制查询失败 fail-open 放行（读库故障不阻断发送，
// 与 rotator 其余「记账失败不阻断」取舍一致）：drop 掉 suppressions 表制造查询
// 错误（settings/channels/send_logs 不受影响，拦截失败放行后主链路照常走完）。
func TestSendSuppressionFailOpen(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	addChannel(t, db, &model.Channel{Name: "m", Provider: "mock"})
	if err := db.Migrator().DropTable(&model.Suppression{}); err != nil {
		t.Fatalf("drop suppressions: %v", err)
	}

	tr := &fakeTransport{name: "mock", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		return &provider.Result{ProviderMessageID: "mock-ok"}, nil
	}}
	in := sendInput("sender@example.com", tr)
	in.Message.To = []string{"anyone@example.com"}

	out, rerr := Send(context.Background(), db, in)
	if rerr != nil || out == nil {
		t.Fatalf("fail-open 应放行发送，got out=%v err=%v", out, rerr)
	}
}

// TestSendSuppressionAutoIngestedEntryBlocked 自动抑制链路（事件摄取 → suppressions
// → 发送拦截）的 rotator 侧闭环：入库行与摄取归一化口径一致（小写 email）即被拦。
func TestSendSuppressionAutoIngestedEntryBlocked(t *testing.T) {
	db := newDB(t)
	putSetting(t, db, "priority", 3, 60, 5)
	addChannel(t, db, &model.Channel{Name: "m", Provider: "mock"})
	if err := db.Create(&model.Suppression{
		Email: "bounced@example.com", Reason: model.SuppressReasonComplaint,
		ChannelID: 99, ChannelName: "resend主号", Detail: "原始事件: email.complained",
	}).Error; err != nil {
		t.Fatalf("seed suppression: %v", err)
	}
	tr := &fakeTransport{name: "mock", fn: func(*model.Channel) (*provider.Result, *provider.ProviderError) {
		return &provider.Result{ProviderMessageID: "mock-1"}, nil
	}}
	in := sendInput("sender@example.com", tr)
	in.Message.To = []string{"bounced@example.com"}

	_, rerr := Send(context.Background(), db, in)
	if rerr == nil || rerr.Code != CodeRecipientSuppressed {
		t.Fatalf("complaint 抑制应拦截，got err=%v", rerr)
	}
	if !strings.Contains(rerr.Message, "complaint") {
		t.Errorf("message 应含原因 complaint: %s", rerr.Message)
	}
}

// TestCheckSuppressionsDedupesAddresses 收件人去重：同一地址重复出现（to+cc）
// 只算一次命中，message 不重复列出。
func TestCheckSuppressionsDedupesAddresses(t *testing.T) {
	db := newDB(t)
	if err := db.Create(&model.Suppression{Email: "dup@example.com", Reason: model.SuppressReasonBounce}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	msg := &provider.Message{
		To: []string{"dup@example.com"}, Cc: []string{"Dup@example.com"},
	}
	rerr := checkSuppressions(db, msg)
	if rerr == nil {
		t.Fatal("应命中拦截")
	}
	if got := strings.Count(rerr.Message, "dup@example.com"); got != 1 {
		t.Errorf("命中地址应去重列出一次，got %d 次: %s", got, rerr.Message)
	}
}
