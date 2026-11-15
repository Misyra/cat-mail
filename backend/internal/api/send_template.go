// send_template.go 发送入口的邮件模板渲染 hook（邮件模板功能包）：
// /v1/send、/emails（共用 sendExec）与渠道测试发送共用本文件的加载与渲染逻辑。
//
// 取舍说明（实现契约）：
//   - 渲染时机：幂等检查之后（idemClaim 在 send 外层）、rotator.Send 之前——
//     渲染结果即发送快照，幂等键重放语义不变（重放直接回放存档响应，不重渲染）；
//   - 冲突策略：请求同时携带 template_id 与非空 html/text → 400 body_conflict。
//     显式失败优于静默覆盖：模板优先会掩盖调用方的参数 bug（误留旧正文），
//     而事务性邮件发错内容比发不出去代价更高。subject 不参与冲突判定（Resend
//     SDK 的 subject 为必填字段，SDK 调用方迁移到模板时必然仍带着 subject，
//     报错会破坏 SDK 同构迁移路径）——模板渲染结果整体替换 subject/html/text，
//     请求 subject 被模板渲染结果覆盖；
//   - 未知模板：400 unknown_template（错误体风格对齐 SPEC §2）；
//   - 未提供的变量：占位符留空，变量名清单在成功响应 missing_variables 字段与
//     服务端日志可见（v1 不做 unknown_variable 报错，理由见 render 包注释）。
package api

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/render"
)

// 模板渲染变量上限（SPEC §8 输入校验完备）。
const (
	maxVariablesCount    = 100 // variables 对象键数量上限
	maxVariableNameChars = 128 // 单个变量名（含点路径）长度上限
)

// loadTemplate 按 ID 加载模板；不存在 → 400 unknown_template，查询失败 → 500。
func (s *Server) loadTemplate(id uint) (*model.Template, *reqErr) {
	var tpl model.Template
	err := s.db.First(&tpl, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, &reqErr{status: 400, typ: TypeUnknownTemplate,
			message: fmt.Sprintf("模板不存在：%d", id)}
	}
	if err != nil {
		return nil, serverReqErr(err)
	}
	return &tpl, nil
}

// validateVariables 渲染变量对象校验：数量 ≤100、变量名非空 ≤128 字符
// （按 Unicode 码点计，与全仓字符串校验口径一致）。
// 值类型/大小不设限（JSON 请求体已有 30MB 总闸，渲染结果另有正文 1MB 校验兜底）。
func validateVariables(vars map[string]any) *reqErr {
	if len(vars) > maxVariablesCount {
		return badReqErr("variables 数量超限（≤%d）", maxVariablesCount)
	}
	for k := range vars {
		if n := utf8.RuneCountInString(strings.TrimSpace(k)); n == 0 || n > maxVariableNameChars {
			return badReqErr("variables 键无效（非空且 ≤%d 字符）", maxVariableNameChars)
		}
	}
	return nil
}

// applySendTemplate 请求携带 template_id 时渲染模板并就地替换 req 的
// subject/html/text（渲染先于 buildMessage 的常规校验：模板发送下 subject 与
// 正文由模板提供，请求可两者皆缺）。返回模板 ID（未使用模板返回 0）、缺失变量
// 名清单与请求层错误。
//
// 请求未携带 template_id 时 variables 被忽略（无模板可渲染，保持零开销直通）。
func (s *Server) applySendTemplate(req *sendRequest) (uint, []string, *reqErr) {
	if req.TemplateID == 0 {
		return 0, nil, nil
	}
	// 冲突判定：模板语义下正文只能来自模板——请求自带 html/text 视为调用方
	// 参数 bug，显式 400 body_conflict（取舍见文件头注释）。
	if strings.TrimSpace(req.HTML) != "" || strings.TrimSpace(req.Text) != "" {
		return 0, nil, &reqErr{status: 400, typ: TypeBodyConflict,
			message: "template_id 与 html/text 互斥：使用模板时正文由模板渲染，请移除请求中的 html/text"}
	}
	if rerr := validateVariables(req.Variables); rerr != nil {
		return 0, nil, rerr
	}
	tpl, rerr := s.loadTemplate(req.TemplateID)
	if rerr != nil {
		return 0, nil, rerr
	}
	rendered, missing := render.ApplyContent(render.Content{
		Subject: tpl.Subject,
		HTML:    tpl.HTML,
		Text:    tpl.Text,
	}, req.Variables)
	req.Subject, req.HTML, req.Text = rendered.Subject, rendered.HTML, rendered.Text
	if len(missing) > 0 {
		// 缺失变量留空并在服务端日志可见（响应侧由 sendExec 附 missing_variables）
		log.Printf("api: send template missing variables (template_id=%d): %v", tpl.ID, missing)
	}
	return tpl.ID, missing, nil
}
