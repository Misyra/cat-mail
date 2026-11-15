// render_test.go 渲染引擎单测：占位符替换、HTML 转义（无逃生门）、缺失变量
// 收集（留空 + 去重保序）、嵌套对象取值、非标量序列化与变量提取。
package render

import (
	"reflect"
	"strings"
	"testing"
)

// TestRenderBasic 占位符替换：单串/多变量/重复出现/空白容忍/无占位符快路径。
func TestRenderBasic(t *testing.T) {
	vars := map[string]any{"name": "张三", "code": float64(123456)}
	cases := []struct {
		tpl  string
		want string
	}{
		{"您好 {{name}}", "您好 张三"},
		{"{{ name }}和{{name}}", "张三和张三"}, // 空白容忍 + 重复出现
		{"验证码：{{code}}，30 分钟内有效", "验证码：123456，30 分钟内有效"},
		{"无占位符原文", "无占位符原文"},
		{"{{}} 保留原文", "{{}} 保留原文"}, // 空占位符非合法变量，保留原文
		{"", ""},
	}
	for _, c := range cases {
		got, missing := Render(c.tpl, vars)
		if got != c.want {
			t.Errorf("Render(%q) = %q, want %q (missing=%v)", c.tpl, got, c.want, missing)
		}
		if len(missing) != 0 {
			t.Errorf("Render(%q) missing = %v, want 空", c.tpl, missing)
		}
	}
}

// TestRenderHTMLEscape HTML 语境变量转义：<>&'" 全部转义，模板自身标记不受影响；
// 纯文本语境不转义（subject/text 语境转义会破坏 & 等字符）。
func TestRenderHTMLEscape(t *testing.T) {
	vars := map[string]any{"v": `<b>&"'` + "\t</b>"}
	got, missing := RenderHTML("<p>{{v}}</p>", vars)
	want := "<p>&lt;b&gt;&amp;&#34;&#39;\t&lt;/b&gt;</p>"
	if got != want {
		t.Errorf("RenderHTML = %q, want %q", got, want)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want 空", missing)
	}
	// 无逃生门：同一函数链无任何开关可关闭转义（行为锁定，防后续误加 raw 选项漏测）
	got2, _ := RenderHTML("{{v}}", vars)
	if strings.Contains(got2, "<b>") {
		t.Error("HTML 语境变量必须转义，不得出现原始标记")
	}
	// 纯文本语境：& 等字符原样保留
	raw, _ := Render("AT{{v}}", map[string]any{"v": "&T"})
	if raw != "AT&T" {
		t.Errorf("Render text = %q, want %q", raw, "AT&T")
	}
}

// TestRenderMissingVariables 缺失变量：占位符留空并收集变量名（去重保序，
// 跨 subject/html/text 三个字段归并）；JSON null 视为未提供。
func TestRenderMissingVariables(t *testing.T) {
	vars := map[string]any{"present": "ok"}
	out, missing := ApplyContent(Content{
		Subject: "订单 {{order_no}}",
		HTML:    "<p>{{present}}</p><p>{{b}} {{a}} {{b}}</p>",
		Text:    "{{c}}",
	}, vars)
	if out.Subject != "订单 " || out.HTML != "<p>ok</p><p>  </p>" || out.Text != "" {
		t.Errorf("渲染结果不符: %+v", out)
	}
	want := []string{"order_no", "b", "a", "c"} // 去重保序：b 重复出现仅记一次
	if !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
	// null 值：视为未提供（无有效内容），留空并上报
	_, missing = Render("{{n}}", map[string]any{"n": nil})
	if !reflect.DeepEqual(missing, []string{"n"}) {
		t.Errorf("null missing = %v, want [n]", missing)
	}
}

// TestRenderNestedPath 嵌套对象取值（点路径）：多级下探、中间层缺失/非对象、
// 中文键。
func TestRenderNestedPath(t *testing.T) {
	vars := map[string]any{
		"user": map[string]any{"name": "李四", "addr": map[string]any{"city": "杭州"}},
		"订单号":  "SO-1",
	}
	cases := []struct {
		tpl     string
		want    string
		missing []string
	}{
		{"{{user.name}}", "李四", nil},
		{"{{ user.addr.city }}", "杭州", nil},
		{"{{订单号}}", "SO-1", nil},
		{"{{user.zip}}", "", []string{"user.zip"}},                 // 叶子缺失
		{"{{user.addr.city.x}}", "", []string{"user.addr.city.x"}}, // 叶子是标量再下探 → 缺失
		{"{{x.y}}", "", []string{"x.y"}},                           // 顶层缺失
	}
	for _, c := range cases {
		got, missing := Render(c.tpl, vars)
		if got != c.want || !reflect.DeepEqual(missing, c.missing) {
			t.Errorf("Render(%q) = (%q, %v), want (%q, %v)", c.tpl, got, missing, c.want, c.missing)
		}
	}
}

// TestRenderNonScalar 非标量值：对象/数组序列化为紧凑 JSON（HTML 语境下仍转义）；
// 数字按最小表示（1 不渲染成 1e+00）。
func TestRenderNonScalar(t *testing.T) {
	got, _ := Render("{{obj}}", map[string]any{"obj": map[string]any{"a": float64(1)}})
	if got != `{"a":1}` {
		t.Errorf("object value = %q, want %q", got, `{"a":1}`)
	}
	got, _ = Render("{{n}}", map[string]any{"n": float64(1)})
	if got != "1" {
		t.Errorf("number value = %q, want 1", got)
	}
	got, _ = Render("{{f}}", map[string]any{"f": 1.5})
	if got != "1.5" {
		t.Errorf("float value = %q, want 1.5", got)
	}
	got, _ = RenderHTML("{{obj}}", map[string]any{"obj": "<x>"})
	if got != "&lt;x&gt;" {
		t.Errorf("html object value = %q, want &lt;x&gt;", got)
	}
}

// TestApplyContentEscapeBoundary 语境边界：同一变量在 html 转义、subject/text 原样。
func TestApplyContentEscapeBoundary(t *testing.T) {
	vars := map[string]any{"v": "<i>&"}
	out, _ := ApplyContent(Content{Subject: "s{{v}}", HTML: "h{{v}}", Text: "t{{v}}"}, vars)
	if out.Subject != "s<i>&" || out.HTML != "h&lt;i&gt;&amp;" || out.Text != "t<i>&" {
		t.Errorf("语境边界不符: %+v", out)
	}
}

// TestApplyContentNoVars 变量对象为空/nil：全部占位符留空并上报，不 panic。
func TestApplyContentNoVars(t *testing.T) {
	out, missing := ApplyContent(Content{Subject: "{{a}}", HTML: "<p>{{b}}</p>"}, nil)
	if out.Subject != "" || out.HTML != "<p></p>" {
		t.Errorf("渲染结果不符: %+v", out)
	}
	if !reflect.DeepEqual(missing, []string{"a", "b"}) {
		t.Errorf("missing = %v, want [a b]", missing)
	}
}

// TestExtractVars 变量提取：去重保序、点路径原样、空白容忍、空占位符忽略、
// 多字段合并提取。
func TestExtractVars(t *testing.T) {
	got := ExtractVars("{{b}} {{ a.b }}{{b}}{{}}末尾 {{订单号}}")
	want := []string{"b", "a.b", "订单号"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractVars = %v, want %v", got, want)
	}
	if ExtractVars("无占位符") != nil {
		t.Error("无占位符应返回 nil")
	}
}
