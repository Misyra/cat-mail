// helpers.go 供 api 层摄取 handler 使用的导出助手：Mailgun 签名三元组提取与
// 防重放缓存（replay.go 的导出别名）。
package inbound

// SigTriple 从（可能含 signature 信封的）报文 map 中提取 Mailgun 签名三元组
// (timestamp, token, signature)；任一缺失返回空串项。api 层据此判定「齐备即
// 签名请求」，验败必拒（见 handler_webhook.go authorizeInbound）。
func SigTriple(m map[string]any) (timestamp, token, signature string) {
	timestamp = asString(m["timestamp"])
	token = asString(m["token"])
	signature = asString(m["signature"])
	return
}

// NestedOrSelf 取 m[key] 的嵌套对象（如 Mailgun 的 signature 信封），不存在时
// 返回 m 自身（顶层平铺形态），调用方对两种形态统一提取。
func NestedOrSelf(m map[string]any, key string) map[string]any {
	if nested, ok := m[key].(map[string]any); ok && nested != nil {
		return nested
	}
	return m
}

// ReplayCache 防重放缓存导出别名（实现见 replay.go）。
type ReplayCache = replayCache
