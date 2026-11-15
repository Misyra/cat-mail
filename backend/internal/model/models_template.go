// models_template.go 邮件模板模型（Template，邮件模板功能包）。
// 独立成文件而非追加进 models.go：新 gorm 模型放新文件，避免与并行改动共享文件。
// 双方言同构（SQLite | PostgreSQL）：仅用 GORM 通用类型与唯一索引，无方言专属列型。
package model

import "time"

// Template 邮件模板：可复用的 subject/html/text 内容骨架，占位符风格 {{var}}，
// 发送入口经 template_id + variables 引用（渲染发生在幂等检查之后、发送之前，
// 见 api 层 send_template.go 与 render 包）。
type Template struct {
	ID uint `gorm:"primaryKey"`
	// Name 模板名，唯一；渲染与 CRUD 均按 ID 引用，名称仅供人读与管理列表展示。
	Name string `gorm:"uniqueIndex;size:128"`
	// Subject 模板主题（占位符按纯文本语境渲染，不做 HTML 转义——主题是邮件头
	// 而非 HTML 语境，转义会破坏 & 等字符）。CRUD 校验非空且 ≤998 字符（RFC 单头行）。
	Subject string
	// HTML / Text 模板正文：占位符渲染时 html 变量做 HTML 转义、text 原样替换；
	// 两者至少一个非空（与发送请求的正文校验同口径），各 ≤1MB 字符。
	HTML string `gorm:"default:''"`
	Text string `gorm:"default:''"`
	// Variables 模板变量清单：JSON 数组字符串（元素为变量名）。可由管理端显式
	// 提供，缺省时从 subject/html/text 内容自动提取（render.ExtractVars）。
	// 存 JSON 而非 csv：变量名来自调用方 JSON 键，可含逗号等任意字符。
	Variables string `gorm:"default:''"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// 模板管理审计动作（AuditLog.Action，对齐既有 15 类动作的命名风格：
// 前缀标识资源类别 template，后缀标识动作）。
const (
	AuditTemplateCreate = "template_create"
	AuditTemplateUpdate = "template_update"
	AuditTemplateDelete = "template_delete"
)
