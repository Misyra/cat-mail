// aliyun.go 阿里云邮件推送 DirectMail 适配器：Action=SingleSendMail，RPC 风格（POST + form 业务参数 +
// query 公共参数），HMAC-SHA1 POP 签名见 aliyun_sign.go。
// 依据：官方文档 help.aliyun.com/zh/direct-mail/singlesendmail（SingleSendMail 返回 EnvId+RequestId，
// 不支持附件；Subject ≤100 字符；ToAddress 逗号分隔最多 100 个）与 /direct-mail/batchsendmail
// （BatchSendMail 仅收预创建的模板+收件人列表，不适用即时报文，故本适配器只走 SingleSendMail）。
package provider

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"catmail/internal/model"
)

// aliyunBaseURL 官方接入点 dm.aliyuncs.com（SingleSendMail 请求示例原文），包级变量供测试注入覆盖。
var aliyunBaseURL = "https://dm.aliyuncs.com"

// aliyunAction/aliyunVersion SingleSendMail 系统规定参数（官方文档）。
const (
	aliyunAction  = "SingleSendMail"
	aliyunVersion = "2017-06-06"
)

// Aliyun 阿里云 DirectMail 平台适配器。
type Aliyun struct{}

func (Aliyun) Name() string { return "aliyun" }

// aliyunEndpointBase 渠道端点：官方当前只公开 dm.aliyuncs.com；Region 字段可覆盖端点
// （填完整主机名如 dm.cn-hangzhou.aliyuncs.com，或带 scheme 的完整 URL；留空/其他值用官方默认）。
func aliyunEndpointBase(region string) string {
	region = strings.TrimSpace(region)
	switch {
	case region == "":
		return aliyunBaseURL
	case strings.Contains(region, "://"): // 已带 scheme 的完整端点
		return region
	case strings.Contains(region, "."): // 主机名覆盖
		return "https://" + region
	}
	return aliyunBaseURL
}

// Send 经 DirectMail SingleSendMail 发送，成功取响应 EnvId（缺失回退 RequestId）。
//
// 渠道字段映射：APIKey=AccessKeyId、APISecret=AccessKeySecret、FromEmail=AccountName（发信地址）、
// FromName=FromAlias（发信昵称，可选）、Region=端点主机覆盖（可选）。
// 官方单参数无独立抄送/密送：ToAddress 合并 To+Cc+Bcc 送出（保证送达，非密送语义，见字段映射表）。
// SingleSendMail 不支持附件与自定义头：带附件直接报 unsupported_attachment（不静默丢内容），
// Headers 丢弃；Tags 对应需控制台预创建的 TagName，透传任意值有 400 风险，故不映射。
func (Aliyun) Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError) {
	if strings.TrimSpace(ch.FromEmail) == "" {
		return nil, &ProviderError{Code: "channel_config", Message: "aliyun: 发信地址未配置（FromEmail=AccountName）", Retryable: true}
	}
	if strings.TrimSpace(ch.APIKey) == "" || strings.TrimSpace(ch.APISecret) == "" {
		return nil, &ProviderError{Code: "channel_config", Message: "aliyun: AccessKeyId/APISecret 未配置", Retryable: true}
	}
	if len(m.Attachments) > 0 {
		return nil, &ProviderError{Code: "unsupported_attachment",
			Message: "aliyun: SingleSendMail 不支持附件（官方文档），请改用其他渠道发送该消息"}
	}
	client, err := BuildClient(ch.ProxyURL)
	if err != nil {
		return nil, &ProviderError{Code: "proxy_error", Message: err.Error(), Retryable: true}
	}

	form := url.Values{}
	form.Set("AccountName", ch.FromEmail) // 必填：控制台配置的发信地址
	form.Set("AddressType", "1")          // 必填：1=发信地址（0=随机账号）
	form.Set("ReplyToAddress", "false")   // 必填：字符串 true/false，是否启用控制台配置的回信地址
	form.Set("ToAddress", joinNonEmpty(append(append([]string{}, m.To...), append(m.Cc, m.Bcc...)...), ","))
	form.Set("Subject", m.Subject)
	if m.HTML != "" {
		form.Set("HtmlBody", m.HTML) // 官方：HtmlBody/TextBody 两者必须传其一（SDK 限制 80K）
	}
	if m.Text != "" {
		form.Set("TextBody", m.Text)
	}
	if name := strings.TrimSpace(m.FromName); name != "" {
		form.Set("FromAlias", name) // 可选：发信人昵称，长度 <15 字符
	}

	common := url.Values{}
	common.Set("Action", aliyunAction)
	common.Set("Version", aliyunVersion)
	common.Set("Format", "JSON")
	common.Set("AccessKeyId", ch.APIKey)
	common.Set("SignatureMethod", "HMAC-SHA1")
	common.Set("SignatureVersion", "1.0")
	common.Set("SignatureNonce", popNonce())
	common.Set("Timestamp", popTimestamp())

	merged := url.Values{}
	for k, vs := range form {
		merged[k] = vs
	}
	for k, vs := range common {
		merged[k] = vs
	}
	signature := popSign(popStringToSign("POST", merged), ch.APISecret)
	common.Set("Signature", signature)

	endpoint := aliyunEndpointBase(ch.Region) + "/?" + common.Encode()
	body, perr := PostForm(ctx, client, endpoint, nil, form)
	if perr != nil {
		return nil, perr
	}
	var out struct {
		EnvId     string `json:"EnvId"`
		RequestId string `json:"RequestId"`
	}
	if json.Unmarshal(body, &out) != nil || (out.EnvId == "" && out.RequestId == "") {
		return nil, &ProviderError{Code: "unexpected_response",
			Message: "aliyun: missing EnvId in response | body: " + truncate(string(body), maxErrorBody)}
	}
	id := out.EnvId
	if id == "" {
		id = out.RequestId
	}
	return &Result{ProviderMessageID: id}, nil
}

func init() {
	Register(Aliyun{})
}
