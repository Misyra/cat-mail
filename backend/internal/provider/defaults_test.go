// defaults_test.go 默认额度单测：与 research/providers-mainstream.md 调研口径锁定一致。
package provider

import "testing"

// TestDefaultQuotaFor 调研口径锁定：Resend 100/3000、Brevo 300/0（日限型）、Mailjet 200/6000、
// SMTP2GO 200/1000、Mailgun 100/0、Elastic Email 100/3000、Mailtrap 150/4000（官方定价页），
// 阿里云 DirectMail 200/2000（官方免费额度：每日最多 200 封免费、总量 2000 封）；未知平台 {0,0} 不限。
func TestDefaultQuotaFor(t *testing.T) {
	cases := map[string]DefaultQuota{
		"resend":       {Daily: 100, Monthly: 3000},
		"brevo":        {Daily: 300, Monthly: 0},
		"mailjet":      {Daily: 200, Monthly: 6000},
		"smtp2go":      {Daily: 200, Monthly: 1000},
		"mailgun":      {Daily: 100, Monthly: 0},
		"elasticemail": {Daily: 100, Monthly: 3000},
		"mailtrap":     {Daily: 150, Monthly: 4000},
		"aliyun":       {Daily: 200, Monthly: 2000},
		"smtp":         {Daily: 0, Monthly: 0},
		"mock":         {Daily: 0, Monthly: 0},
		"unknown":      {Daily: 0, Monthly: 0},
	}
	for name, want := range cases {
		if got := DefaultQuotaFor(name); got != want {
			t.Errorf("DefaultQuotaFor(%q) = %+v, want %+v", name, got, want)
		}
	}
}
