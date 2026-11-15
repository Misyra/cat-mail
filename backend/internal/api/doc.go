// Package api 提供 CatMail 的 HTTP 层：管理后台 Cookie 会话 API（/api/*）、
// 对外发送入口（/v1/send、/emails，Bearer ApiKey）与统一错误体
// {error:{type,message}}（SPEC §2/§5/§8）。
package api
