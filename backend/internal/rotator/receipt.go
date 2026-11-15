// receipt.go D1 调用方 Webhook 回执埋点：受理成功（send_accepted）与终态失败
// （send_failed，429 额度/限速与 502 候选穷尽）时经包级 receipt.EmitAccepted /
// EmitFailed 向调用方 Key 配置的 webhook 推送 JSON 回执。回执目标由 Key 上的
// webhook_url / webhook_events 决定（receipt.emit 内读库判定），未配置即 no-op；
// 发射器未接线（未 SetDefault）同样 no-op——回执故障绝不影响发送主链路。
// 与 notifySendFailed（管理员侧告警）相互独立、并行发射，互不复用。
package rotator

import (
	"catmail/internal/provider"
	"catmail/internal/receipt"
)

// receiptAccepted 受理成功 → receipt.EventSendAccepted：data 携带 message_id、
// provider、channel、provider_message_id、to、subject、latency_ms、accepted_at。
// To 快照拷贝：入队后由 worker 异步序列化，避免共享底层数组被并发改写。
func receiptAccepted(in *Input, out *Output, msg *provider.Message) {
	if in.APIKeyID == 0 {
		return // 无调用方（测试直发等）：回执是 Key 级能力，无从推送
	}
	receipt.EmitAccepted(in.APIKeyID, receipt.AcceptedData{
		MessageID:         out.MessageID,
		Provider:          out.Provider,
		Channel:           out.ChannelName,
		ProviderMessageID: out.ProviderMessageID,
		To:                append([]string(nil), in.Message.To...),
		Subject:           msg.Subject,
		LatencyMs:         out.LatencyMs,
	})
}

// receiptFailed 终态失败 → receipt.EventSendFailed：data 携带 message_id、error、
// tried、failed_at。仅在 429（额度耗尽/出站限速）与 502（候选穷尽）两个终态调用；
// 参数类 400（invalid_request）不推——调用方在同步响应里当场已见错误。
func receiptFailed(in *Input, messageID string, tried int, errText string) {
	if in.APIKeyID == 0 {
		return
	}
	receipt.EmitFailed(in.APIKeyID, receipt.FailedData{
		MessageID: messageID,
		Error:     errText,
		Tried:     tried,
	})
}
