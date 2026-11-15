// normalize_test.go 各平台事件归一化测试：Resend/Mailgun/Brevo/Mailjet 按官方
// 文档样例报文核对（事件名映射、收件人/消息 ID/时间提取、hard/soft 退信区分、
// raw 兜底）；generic 兜底与 form 编码（Mailgun legacy）形态覆盖。
package inbound

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"catmail/internal/model"
)

// assertEvent 归一化断言（type/bounceType/recipient/msgID/occurredAt/detail）。
func assertEvent(t *testing.T, name string, ev Event,
	wantType, wantBounce, wantRecipient, wantMsgID string, wantAt time.Time) {
	t.Helper()
	if ev.Type != wantType {
		t.Errorf("%s: type = %q, want %q (detail=%q)", name, ev.Type, wantType, ev.Detail)
	}
	if ev.BounceType != wantBounce {
		t.Errorf("%s: bounce_type = %q, want %q", name, ev.BounceType, wantBounce)
	}
	if ev.Recipient != wantRecipient {
		t.Errorf("%s: recipient = %q, want %q", name, ev.Recipient, wantRecipient)
	}
	if ev.ProviderMessageID != wantMsgID {
		t.Errorf("%s: provider_message_id = %q, want %q", name, ev.ProviderMessageID, wantMsgID)
	}
	if !wantAt.IsZero() && !ev.OccurredAt.Equal(wantAt) {
		t.Errorf("%s: occurred_at = %v, want %v", name, ev.OccurredAt, wantAt)
	}
}

func TestNormalizeResend(t *testing.T) {
	// 官方 email.bounced 样例（截取关键字段）
	body := []byte(`{
		"type": "email.bounced",
		"created_at": "2026-11-14T09:00:00.500Z",
		"data": {
			"email_id": "56761188-7520-42d8-8898-ff6fc54ce618",
			"message_id": "<20261114@mail.example.com>",
			"from": "noreply@mail.example.com",
			"to": ["Recipient@Example.com"],
			"subject": "Hello",
			"bounce": {
				"message": "smtp; 550 5.5.0 mailbox unavailable",
				"diagnosticCode": ["smtp; 550 5.5.0 Requested action not taken"]
			}
		}
	}`)
	at, _ := time.Parse(time.RFC3339Nano, "2026-11-14T09:00:00.5Z")
	ev := Normalize("resend", body, nil)
	assertEvent(t, "resend bounced", ev, model.MailEventBounced, model.BounceTypeHard,
		"recipient@example.com", "56761188-7520-42d8-8898-ff6fc54ce618", at)
	if ev.Detail == "" || !strings.Contains(ev.Detail, "原始事件: email.bounced") || !strings.Contains(ev.Detail, "550 5.5.0") {
		t.Errorf("bounced detail 应含原始事件名与诊断码: %q", ev.Detail)
	}

	for _, c := range []struct {
		raw  string
		want string
	}{
		{`{"type":"email.sent","data":{"email_id":"e1","to":["a@b.com"]}}`, model.MailEventSent},
		{`{"type":"email.delivered","data":{"email_id":"e1","to":["a@b.com"]}}`, model.MailEventDelivered},
		{`{"type":"email.complained","data":{"email_id":"e1","to":["a@b.com"]}}`, model.MailEventComplained},
		{`{"type":"email.delivery_delayed","data":{"email_id":"e1","to":["a@b.com"]}}`, model.MailEventRaw},
		{`{"type":"email.opened","data":{"email_id":"e1","to":["a@b.com"]}}`, model.MailEventRaw},
		{`{"type":"contact.created"}`, model.MailEventRaw},
	} {
		ev := Normalize("resend", []byte(c.raw), nil)
		if ev.Type != c.want {
			t.Errorf("resend %s: type = %q, want %q", c.raw, ev.Type, c.want)
		}
	}
}

func TestNormalizeMailgunJSON(t *testing.T) {
	// 官方 permanent failed 样例（截取关键字段；signature 信封由鉴权层消费）
	body := []byte(`{
		"event": "failed",
		"timestamp": 1770146798.592,
		"recipient": "bad@example.com",
		"severity": "permanent",
		"reason": "bounce",
		"message": {"headers": {"message-id": "20260212174255.58bbb7ce85a423e5@mailgun.com"}},
		"delivery-status": {"code": 550, "message": "5.5.0 mailbox unavailable", "bounce-type": "hard"}
	}`)
	ev := Normalize("mailgun", body, nil)
	assertEvent(t, "mailgun failed(permanent)", ev, model.MailEventBounced, model.BounceTypeHard,
		"bad@example.com", "20260212174255.58bbb7ce85a423e5@mailgun.com",
		time.Unix(1770146798, 0))
	if !strings.Contains(ev.Detail, "code=550") || !strings.Contains(ev.Detail, "mailbox unavailable") {
		t.Errorf("detail 应含投递状态: %q", ev.Detail)
	}

	// temporary → soft
	ev = Normalize("mailgun", []byte(`{"event":"failed","severity":"temporary","recipient":"a@b.com"}`), nil)
	assertEvent(t, "mailgun failed(temporary)", ev, model.MailEventBounced, model.BounceTypeSoft, "a@b.com", "", time.Time{})

	// delivered / accepted / complained / opened
	for _, c := range []struct {
		raw  string
		want string
	}{
		{`{"event":"delivered","recipient":"a@b.com","message":{"headers":{"message-id":"mid@x"}}}`, model.MailEventDelivered},
		{`{"event":"accepted","recipient":"a@b.com"}`, model.MailEventSent},
		{`{"event":"complained","recipient":"a@b.com"}`, model.MailEventComplained},
		{`{"event":"opened","recipient":"a@b.com"}`, model.MailEventRaw},
		{`{"event":"unknown-new-event","recipient":"a@b.com"}`, model.MailEventRaw},
	} {
		ev := Normalize("mailgun", []byte(c.raw), nil)
		if ev.Type != c.want {
			t.Errorf("mailgun %s: type = %q, want %q", c.raw, ev.Type, c.want)
		}
	}
}

func TestNormalizeMailgunLegacyForm(t *testing.T) {
	// legacy form 编码：event=bounced（官方定义为永久退信）+ message-headers 数组
	form := url.Values{
		"event":           {"bounced"},
		"recipient":       {"legacy@example.com"},
		"timestamp":       {"1770146798"},
		"token":           {"tok"},
		"signature":       {"sig"},
		"message-headers": {`[["From","x@y"],["Message-Id","<legacy-mid@domain>"]]`},
	}
	ev := Normalize("mailgun", nil, form)
	assertEvent(t, "mailgun legacy bounced", ev, model.MailEventBounced, model.BounceTypeHard,
		"legacy@example.com", "<legacy-mid@domain>", time.Unix(1770146798, 0))

	form.Set("event", "soft-bounced")
	ev = Normalize("mailgun", nil, form)
	if ev.Type != model.MailEventBounced || ev.BounceType != model.BounceTypeSoft {
		t.Errorf("legacy soft-bounced = %s/%s", ev.Type, ev.BounceType)
	}
}

func TestNormalizeBrevo(t *testing.T) {
	body := []byte(`{"event":"hard_bounce","email":"a@b.com","message-id":"<brevo-mid@relay>","ts_event":1770146798,"reason":"550 unknown user"}`)
	ev := Normalize("brevo", body, nil)
	assertEvent(t, "brevo hard_bounce", ev, model.MailEventBounced, model.BounceTypeHard,
		"a@b.com", "<brevo-mid@relay>", time.Unix(1770146798, 0))
	if !strings.Contains(ev.Detail, "550 unknown user") {
		t.Errorf("detail 应含退信原因: %q", ev.Detail)
	}
	for _, c := range []struct {
		raw  string
		want string
	}{
		{`{"event":"request","email":"a@b.com"}`, model.MailEventSent},
		{`{"event":"delivered","email":"a@b.com"}`, model.MailEventDelivered},
		{`{"event":"soft_bounce","email":"a@b.com"}`, model.MailEventBounced},
		{`{"event":"spam","email":"a@b.com"}`, model.MailEventComplained},
		{`{"event":"invalid_email","email":"a@b.com"}`, model.MailEventBounced},
		{`{"event":"blocked","email":"a@b.com"}`, model.MailEventRaw},
		{`{"event":"opened","email":"a@b.com"}`, model.MailEventRaw},
		{`{"event":"click","email":"a@b.com"}`, model.MailEventRaw},
	} {
		ev := Normalize("brevo", []byte(c.raw), nil)
		if ev.Type != c.want {
			t.Errorf("brevo %s: type = %q, want %q", c.raw, ev.Type, c.want)
		}
		if bt := ev.BounceType; ev.Type == model.MailEventBounced && bt == "" {
			t.Errorf("brevo %s: bounced 应带 bounce_type", c.raw)
		}
	}
}

func TestNormalizeMailjet(t *testing.T) {
	hard := []byte(`{"event":"bounce","message_id":1234567890,"email":"a@b.com","ts_event":1770146798,"hard_bounce":true}`)
	ev := Normalize("mailjet", hard, nil)
	assertEvent(t, "mailjet bounce(hard)", ev, model.MailEventBounced, model.BounceTypeHard,
		"a@b.com", "1234567890", time.Unix(1770146798, 0))

	soft := []byte(`{"event":"bounce","message_id":1,"email":"a@b.com","hard_bounce":false}`)
	ev = Normalize("mailjet", soft, nil)
	if ev.BounceType != model.BounceTypeSoft {
		t.Errorf("mailjet hard_bounce=false 应为 soft, got %q", ev.BounceType)
	}
	missing := []byte(`{"event":"bounce","message_id":1,"email":"a@b.com"}`)
	ev = Normalize("mailjet", missing, nil)
	if ev.BounceType != model.BounceTypeSoft {
		t.Errorf("hard_bounce 缺省应保守为 soft, got %q", ev.BounceType)
	}

	for _, c := range []struct {
		raw  string
		want string
	}{
		{`{"event":"sent","email":"a@b.com"}`, model.MailEventSent},
		{`{"event":"spam","email":"a@b.com"}`, model.MailEventComplained},
		{`{"event":"open","email":"a@b.com"}`, model.MailEventRaw},
		{`{"event":"blocked","email":"a@b.com"}`, model.MailEventRaw},
	} {
		ev := Normalize("mailjet", []byte(c.raw), nil)
		if ev.Type != c.want {
			t.Errorf("mailjet %s: type = %q, want %q", c.raw, ev.Type, c.want)
		}
	}
}

func TestNormalizeGeneric(t *testing.T) {
	// smtp2go/elasticemail/mailtrap/aliyun 等走 generic 兜底（常见字段名）
	ev := Normalize("smtp2go", []byte(`{"event":"delivered","email":"a@b.com","message_id":"s2g-1"}`), nil)
	assertEvent(t, "generic delivered", ev, model.MailEventDelivered, "", "a@b.com", "s2g-1", time.Time{})

	ev = Normalize("elasticemail", []byte(`{"EventType":"hard_bounce","recipient":"c@d.com"}`), nil)
	if ev.Type != model.MailEventBounced || ev.BounceType != model.BounceTypeHard {
		t.Errorf("generic hard_bounce = %s/%s", ev.Type, ev.BounceType)
	}
	// 裸 bounce 保守为 soft（hard 误判会自动进抑制列表，宁可保守）
	ev = Normalize("someplatform", []byte(`{"event":"bounce","email":"e@f.com"}`), nil)
	if ev.Type != model.MailEventBounced || ev.BounceType != model.BounceTypeSoft {
		t.Errorf("generic 裸 bounce 应为 bounced/soft, got %s/%s", ev.Type, ev.BounceType)
	}
	// 未知事件名 → raw 存档
	ev = Normalize("aliyun", []byte(`{"type":"suspicious","email":"g@h.com"}`), nil)
	if ev.Type != model.MailEventRaw {
		t.Errorf("未知事件应 raw 存档, got %q", ev.Type)
	}
	// 完全无法识别的事件名 → 整条 raw
	ev = Normalize("mock", []byte(`{"foo":"bar"}`), nil)
	if ev.Type != model.MailEventRaw {
		t.Errorf("无事件名应 raw 存档, got %q", ev.Type)
	}
}

func TestNormalizeUnrecognizedBody(t *testing.T) {
	// 非 JSON 非 form：raw 存档不猜测
	ev := Normalize("resend", []byte("plain text"), nil)
	if ev.Type != model.MailEventRaw {
		t.Errorf("非 JSON 报文应 raw 存档, got %q", ev.Type)
	}
	// 空对象：raw（无事件名可归类）
	ev = Normalize("resend", []byte(`{}`), nil)
	if ev.Type != model.MailEventRaw {
		t.Errorf("空对象应 raw 存档, got %q", ev.Type)
	}
}
