// Package docs 内嵌 API 文档（B2）：OpenAPI 3.1 规范原文与单文件 HTML 文档页。
//
// 公开无鉴权（server.go 挂载于会话与管理 IP 白名单之外）：文档内容不含任何
// 真实凭据或敏感配置——示例一律为脱敏占位值（cm-abcd…wxyz、****** 等），
// 故可对任意来源开放；规范与 SPEC.md 同步维护，页面零外部依赖。
package docs

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"text/template"

	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var specYAML []byte

//go:embed page.gohtml
var pageTmplSrc string

var (
	renderOnce sync.Once
	pageHTML   []byte
	renderErr  error
)

// render 页面构建（进程内一次）：YAML → 通用树 → JSON → 模板注入为单文件
// HTML（Go 侧完成解析，页面无需携带 YAML 解析器）。json.Marshal 默认 HTML
// 转义（<>& → \uXXXX），注入 <script> 标签安全，且顺带校验 YAML 树可无损
// 转为 JSON（yaml.v3 字符串键映射 → JSON 对象）。
func render() ([]byte, error) {
	var root any
	if err := yaml.Unmarshal(specYAML, &root); err != nil {
		return nil, fmt.Errorf("docs: parse openapi.yaml: %w", err)
	}
	specJSON, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("docs: encode spec json: %w", err)
	}
	tmpl, err := template.New("page").Parse(pageTmplSrc)
	if err != nil {
		return nil, fmt.Errorf("docs: parse page template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, string(specJSON)); err != nil {
		return nil, fmt.Errorf("docs: render page: %w", err)
	}
	return buf.Bytes(), nil
}

// rendered 惰性渲染：首次请求构建并缓存，失败时返回同一错误。
func rendered() ([]byte, error) {
	renderOnce.Do(func() { pageHTML, renderErr = render() })
	return pageHTML, renderErr
}

// Page GET /docs：单文件 HTML 文档页（http.HandlerFunc，经 gin.WrapF 挂载）。
func Page(w http.ResponseWriter, _ *http.Request) {
	b, err := rendered()
	if err != nil {
		http.Error(w, "docs page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// SpecYAML GET /docs/openapi.yaml：规范原文直出（application/yaml）。
func SpecYAML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(specYAML)
}
