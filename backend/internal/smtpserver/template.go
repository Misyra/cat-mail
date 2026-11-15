// template.go SMTP 入口的邮件模板支持（邮件模板功能包）：本地应用经报文头
// 携带模板指令——
//
//	X-CatMail-Template: <模板 ID，十进制正整数>
//	X-CatMail-Template-Vars: <渲染变量 JSON 对象>（或 "base64:<base64(JSON)>"，
//	    供 UTF-8 变量值在纯 ASCII SMTP 链路上的稳妥携带；两种形态均可）
//
// 语义与 /v1/send 的 template_id+variables 完全同口径（见 api 层 send_template.go）：
// 渲染发生在转交轮换器之前，渲染结果整体替换 subject/html/text；
// unknown_template（模板不存在）与 body_conflict（报文自带非空正文时指定模板）
// 均为内容类永久失败 → 501（与 invalid_request 的状态码映射一致）。冲突采取
// 显式失败而非模板优先：防止调用方把占位符草稿当正文静默发出去。
package smtpserver

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/provider"
	"catmail/internal/render"
)

// 模板指令头名（textproto 头名大小写不敏感，Get 按规范名匹配）。
const (
	headerTemplateName = "X-CatMail-Template"      // 模板 ID
	headerTemplateVars = "X-CatMail-Template-Vars" // 渲染变量 JSON
)

// maxTemplateVars SMTP 路径的渲染变量数量上限（与 API 层 validateVariables 同口径）。
const maxTemplateVars = 100

// tmplDirective 报文头携带的模板指令（id=0 表示未指定模板）。
type tmplDirective struct {
	id   uint
	vars map[string]any
}

// extractTemplateDirective 从已解析报文头提取模板指令；未携带指令头返回零值
// （不启用模板，行为与启用前一致）。指令头存在但取值非法（ID 非正整数、变量
// 不是 JSON 对象、base64 解码失败）→ 错误（deliver 映射 501 永久失败）。
func extractTemplateDirective(m *mail.Message) (tmplDirective, error) {
	rawID := strings.TrimSpace(m.Header.Get(headerTemplateName))
	if rawID == "" {
		return tmplDirective{}, nil
	}
	id64, err := strconv.ParseUint(rawID, 10, 64)
	if err != nil || id64 == 0 {
		return tmplDirective{}, fmt.Errorf("%s 无效（须为正整数）", headerTemplateName)
	}
	d := tmplDirective{id: uint(id64)}
	rawVars := strings.TrimSpace(m.Header.Get(headerTemplateVars))
	if rawVars == "" {
		return d, nil
	}
	if b64, ok := strings.CutPrefix(rawVars, "base64:"); ok {
		decoded, e := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(b64), ""))
		if e != nil {
			return tmplDirective{}, fmt.Errorf("%s 的 base64 解码失败", headerTemplateVars)
		}
		rawVars = string(decoded)
	}
	var vars map[string]any
	if err := json.Unmarshal([]byte(rawVars), &vars); err != nil {
		return tmplDirective{}, fmt.Errorf("%s 不是合法的 JSON 对象", headerTemplateVars)
	}
	if len(vars) > maxTemplateVars {
		return tmplDirective{}, fmt.Errorf("模板变量数量超限（≤%d）", maxTemplateVars)
	}
	d.vars = vars
	return d, nil
}

// applyTemplate 模板渲染 hook（deliver 在 parseMessage 之后、enforceKeyPolicy/
// rotator.Send 之前调用）：携带指令头时加载模板并渲染，就地替换 msg 的
// subject/html/text。返回模板 ID（未使用模板返回 0）随 SendLog 落库追溯。
func (se *session) applyTemplate(m *mail.Message, msg *provider.Message) (uint, error) {
	d, err := extractTemplateDirective(m)
	if err != nil {
		return 0, err
	}
	if d.id == 0 {
		return 0, nil
	}
	// body_conflict：报文自带非空正文时指定模板，显式失败（取舍见文件头注释）
	if strings.TrimSpace(msg.HTML) != "" || strings.TrimSpace(msg.Text) != "" {
		return 0, fmt.Errorf("使用模板时正文由模板渲染：请移除报文正文或去掉 %s 头（body_conflict）", headerTemplateName)
	}
	var tpl model.Template
	err = se.srv.db.First(&tpl, d.id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, fmt.Errorf("模板不存在：%d（unknown_template）", d.id)
	}
	if err != nil {
		log.Printf("[smtp] template load failed (id=%d, remote=%s): %v", d.id, se.remote, err)
		return 0, errors.New("查询模板失败")
	}
	rendered, missing := render.ApplyContent(render.Content{
		Subject: tpl.Subject,
		HTML:    tpl.HTML,
		Text:    tpl.Text,
	}, d.vars)
	msg.Subject, msg.HTML, msg.Text = rendered.Subject, rendered.HTML, rendered.Text
	// 缺失变量留空（与 API 路径同策略），变量名入服务端日志可见
	if len(missing) > 0 {
		log.Printf("[smtp] template missing variables (template_id=%d): %v", tpl.ID, missing)
	}
	return tpl.ID, nil
}
