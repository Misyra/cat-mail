// Package render 实现邮件模板占位符渲染（{{var}}）：变量替换、HTML 转义、
// 缺失变量收集与模板变量提取。纯函数、零外部依赖，被发送入口（/v1/send、
// /emails、SMTP）与模板 CRUD（变量自动提取）共用。
//
// 渲染语义（v1 从简，刻意取舍）：
//   - 占位符风格 {{var}}，容忍两侧空白（{{ var }} 等价）；变量名支持点路径
//     取嵌套对象值（{{user.name}}），来自调用方 JSON 变量对象的键；
//   - 变量按语境转义：HTML 正文里的变量做 HTML 转义（<>&'"），无逃生门
//     （不提供关闭转义的选项）；subject 与纯文本正文是邮件头/纯文本语境，
//     转义反而破坏内容（& → &amp;），原样替换；
//   - 未提供的变量：占位符替换为空串，变量名收集进 missing（去重保序），
//     由调用方在响应/服务端日志可见——v1 不做 unknown_variable 报错：留空
//     对模板演化友好（可选变量天然支持），渲染结果整体随发送日志落库可在
//     详情面板核对，且模板渲染缺参不应阻断事务性发送（failover 网关可用性优先）。
package render

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// placeholderRe 匹配 {{...}} 占位符：变量名为花括号间任意非空内容（去两侧空白）。
// 不预设变量名字符集——变量名即调用方 JSON 对象的键，ASCII 与中文键均合法；
// 含嵌套花括号的占位符（{{a{{b}}}}）不匹配整体，按最内层处理，渲染结果可预期。
var placeholderRe = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// Content 一组待渲染的模板内容字段（subject/html/text）。
type Content struct {
	Subject string
	HTML    string
	Text    string
}

// ApplyContent 对模板内容统一渲染（发送入口的共用 hook）：
//   - subject/text 为纯文本语境：变量原样替换；
//   - html 为 HTML 语境：变量值做 HTML 转义（无逃生门）；
//   - 未提供的变量占位符替换为空串，变量名收集进返回的 missing（去重保序）。
//
// 返回渲染后的内容副本，不修改入参。
func ApplyContent(in Content, vars map[string]any) (Content, []string) {
	var missing []string
	out := Content{}
	out.Subject, missing = renderString(in.Subject, vars, false, missing)
	out.HTML, missing = renderString(in.HTML, vars, true, missing)
	out.Text, missing = renderString(in.Text, vars, false, missing)
	return out, dedupeStrings(missing)
}

// Render 按纯文本语境渲染单个模板串（subject/text 用；HTML 请用 RenderHTML）。
// 返回渲染结果与缺失变量名（去重保序）。
func Render(tpl string, vars map[string]any) (string, []string) {
	out, missing := renderString(tpl, vars, false, nil)
	return out, dedupeStrings(missing)
}

// RenderHTML 按 HTML 语境渲染单个模板串：变量值做 HTML 转义（<>&'"），
// 防调用方变量注入标记破坏正文结构（XSS/排版注入）。无逃生门（v1 取舍）。
func RenderHTML(tpl string, vars map[string]any) (string, []string) {
	out, missing := renderString(tpl, vars, true, nil)
	return out, dedupeStrings(missing)
}

// ExtractVars 提取模板串中的占位符变量名（跨多个模板串合并后去重保序，
// 含点路径原样返回）。供模板 CRUD 缺省时从 subject/html/text 自动提取变量清单。
func ExtractVars(tpls ...string) []string {
	var names []string
	for _, tpl := range tpls {
		for _, m := range placeholderRe.FindAllStringSubmatch(tpl, -1) {
			name := strings.TrimSpace(m[1])
			if name == "" {
				continue
			}
			names = append(names, name)
		}
	}
	return dedupeStrings(names)
}

// renderString 渲染单个模板串：占位符逐个替换；escape 决定变量是否 HTML 转义。
// missing 沿调用链累计（同串内重复出现的缺失变量由最终 dedupe 归并）。
func renderString(tpl string, vars map[string]any, escape bool, missing []string) (string, []string) {
	if !strings.Contains(tpl, "{{") {
		return tpl, missing // 快路径：无占位符原样返回，零分配
	}
	var b strings.Builder
	b.Grow(len(tpl))
	last := 0
	for _, idx := range placeholderRe.FindAllStringSubmatchIndex(tpl, -1) {
		name := strings.TrimSpace(tpl[idx[2]:idx[3]])
		if name == "" {
			continue // 空占位符 {{}}：非合法变量，保留原文
		}
		b.WriteString(tpl[last:idx[0]])
		val, ok := lookup(vars, name)
		if !ok {
			missing = append(missing, name) // 未提供：占位符替换为空串
		} else {
			s := stringify(val)
			if escape {
				s = html.EscapeString(s)
			}
			b.WriteString(s)
		}
		last = idx[1]
	}
	b.WriteString(tpl[last:])
	return b.String(), missing
}

// lookup 按（可含点路径的）变量名取值：逐段下探嵌套 map（JSON 对象解码形态）。
// 任一段缺失或中间层不是对象 → 未提供；JSON null 视为未提供（无有效内容）。
func lookup(vars map[string]any, name string) (any, bool) {
	if len(vars) == 0 {
		return nil, false
	}
	cur := any(vars)
	for _, seg := range strings.Split(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	if cur == nil {
		return nil, false
	}
	return cur, true
}

// stringify 变量值 → 字符串：字符串原样；数字/布尔按最小表示（JSON 解码为
// float64，FormatFloat(-1) 保证 1 → "1"、1.5 → "1.5"）；对象/数组序列化为
// 紧凑 JSON（HTML 语境下仍会经转义，不构成注入面）。
func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// dedupeStrings 去重保序（缺失变量清单与提取结果共用）。
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
