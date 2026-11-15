// defaults.go 各平台默认额度与说明（SPEC §3，0=不限，由平台报错兜底）。
package provider

// DefaultQuota 平台默认发送额度。
type DefaultQuota struct {
	Daily   int // 每日默认额度，0=不限
	Monthly int // 每月默认额度，0=不限
}

// ProviderInfo 平台元信息（供管理后台展示与额度兜底）。
type ProviderInfo struct {
	Name        string
	Label       string
	Description string
	Quota       DefaultQuota
}

// Providers 全部支持的平台（顺序即前端下拉展示顺序）。
var Providers = []ProviderInfo{
	{Name: "resend", Label: "Resend", Description: "每日 100 封 / 每月 3000 封（免费档）", Quota: DefaultQuota{Daily: 100, Monthly: 3000}},
	{Name: "brevo", Label: "Brevo", Description: "每日 300 封（免费档）", Quota: DefaultQuota{Daily: 300, Monthly: 0}},
	{Name: "mailjet", Label: "Mailjet", Description: "每日 200 封 / 每月 6000 封（免费档）", Quota: DefaultQuota{Daily: 200, Monthly: 6000}},
	{Name: "smtp2go", Label: "SMTP2GO", Description: "每日 200 封 / 每月 1000 封（免费档）", Quota: DefaultQuota{Daily: 200, Monthly: 1000}},
	{Name: "mailgun", Label: "Mailgun", Description: "试用额度，由平台报错兜底", Quota: DefaultQuota{Daily: 100, Monthly: 0}},
	{Name: "elasticemail", Label: "Elastic Email", Description: "每日 100 封 / 每月 3000 封（Email API 免费档）", Quota: DefaultQuota{Daily: 100, Monthly: 3000}},
	{Name: "mailtrap", Label: "Mailtrap", Description: "每日 150 封 / 每月 4000 封（免费档）", Quota: DefaultQuota{Daily: 150, Monthly: 4000}},
	{Name: "aliyun", Label: "阿里云邮件推送", Description: "每日最多 200 封免费 / 免费总量 2000 封；超出转按量计费，日发送配额随信誉等级上调", Quota: DefaultQuota{Daily: 200, Monthly: 2000}},
	{Name: "smtp", Label: "SMTP", Description: "通用 SMTP，不限额度", Quota: DefaultQuota{Daily: 0, Monthly: 0}},
	{Name: "mock", Label: "Mock", Description: "本地模拟发送，用于测试；APIKey 设为 fail/fail_rate_limit/fail_auth 可模拟失败", Quota: DefaultQuota{Daily: 0, Monthly: 0}},
}

// DefaultQuotaFor 返回平台默认额度；未知平台返回 {0,0}（不限）。
func DefaultQuotaFor(provider string) DefaultQuota {
	for _, p := range Providers {
		if p.Name == provider {
			return p.Quota
		}
	}
	return DefaultQuota{}
}
