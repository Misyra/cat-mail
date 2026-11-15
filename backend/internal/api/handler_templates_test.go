// handler_templates_test.go 邮件模板功能包测试：CRUD（含变量自动提取/显式清单/
// 部分更新语义/重名兜底）、审计落库（template_create/update/delete）、发送入口
// 集成（渲染替换、unknown_template、body_conflict、缺失变量可见、/emails 同构、
// 幂等重放、渠道测试发送带模板、SendLog.template_id 与日志详情回显）。
package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gorm.io/gorm"

	"catmail/internal/model"
)

// createTemplate 建一个模板，返回响应 JSON（含 id/name/variables）。
func createTemplate(t *testing.T, r *gin.Engine, cookie, body string) map[string]any {
	t.Helper()
	m := decode(t, mustStatus(t, doReq(r, "POST", "/api/templates", cookie, "", body), http.StatusOK))
	return m
}

// templateSeed 最小可用模板请求体。
func templateSeed(name string) string {
	return fmt.Sprintf(`{"name":%q,"subject":"订单 {{order_no}} 已发货","html":"<p>亲爱的 {{user.name}}，您的订单已发货</p>","text":"亲爱的 {{user.name}}，您的订单已发货"}`, name)
}

// ---------- CRUD 与审计 ----------

// TestTemplateCRUDLifecycle 新建（变量自动提取）→ 列表 → 详情 → 部分更新 → 删除。
func TestTemplateCRUDLifecycle(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 新建：variables 未提供 → 自动提取（subject+html+text 合并去重保序）
	m := createTemplate(t, r, cookie, templateSeed("发货通知"))
	id := int(m["id"].(float64))
	if m["name"] != "发货通知" {
		t.Fatalf("name: %v", m["name"])
	}
	vars, _ := m["variables"].([]any)
	if len(vars) != 2 || vars[0] != "order_no" || vars[1] != "user.name" {
		t.Fatalf("auto-extracted variables: %v", vars)
	}
	// 响应含正文（创建返回详情形态）
	if m["html_body"] == "" || m["text_body"] == "" {
		t.Fatalf("create response missing bodies: %v", m)
	}

	// 列表：不含正文大字段
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/templates", cookie, "", ""), http.StatusOK))
	items := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list items: %v", items)
	}
	item := items[0].(map[string]any)
	if _, has := item["html_body"]; has {
		t.Fatal("list item must not carry html_body")
	}

	// 详情：含正文
	m = decode(t, mustStatus(t, doReq(r, "GET", fmt.Sprintf("/api/templates/%d", id), cookie, "", ""), http.StatusOK))
	if m["html_body"] == "" {
		t.Fatalf("detail html_body: %v", m["html_body"])
	}

	// 部分更新：只改主题与正文（name 未提供保留原值），variables 未提供 →
	// 按合并后内容重新提取；显式空串字段按提交值清空
	m = decode(t, mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/templates/%d", id), cookie, "",
		`{"subject":"{{order_no}} 已签收","html":"<p>{{user.name}} 签收</p>","text":""}`), http.StatusOK))
	if m["subject"] != "{{order_no}} 已签收" {
		t.Fatalf("updated subject: %v", m["subject"])
	}
	if m["name"] != "发货通知" {
		t.Fatalf("omitted name must keep: %v", m["name"])
	}
	if m["text_body"] != "" {
		t.Fatalf("updated text must be emptied: %v", m["text_body"])
	}
	vars, _ = m["variables"].([]any)
	if len(vars) != 2 || vars[0] != "order_no" || vars[1] != "user.name" {
		t.Fatalf("re-extracted variables: %v", vars)
	}

	// 部分更新：显式 variables（可添加未出现在内容的变量；空数组 = 清空）
	m = decode(t, mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/templates/%d", id), cookie, "",
		`{"variables":["order_no","user.name","extra.plan"]}`), http.StatusOK))
	vars, _ = m["variables"].([]any)
	if len(vars) != 3 || vars[2] != "extra.plan" {
		t.Fatalf("explicit variables: %v", vars)
	}
	m = decode(t, mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/templates/%d", id), cookie, "",
		`{"variables":[]}`), http.StatusOK))
	if vars, _ = m["variables"].([]any); len(vars) != 0 {
		t.Fatalf("empty explicit variables must clear: %v", vars)
	}

	// 删除
	mustStatus(t, doReq(r, "DELETE", fmt.Sprintf("/api/templates/%d", id), cookie, "", ""), http.StatusOK)
	if code := doReq(r, "GET", fmt.Sprintf("/api/templates/%d", id), cookie, "", "").Code; code != http.StatusNotFound {
		t.Fatalf("after delete: %d", code)
	}
}

// TestTemplateValidation 创建/更新的输入校验与重名兜底（预检 + 唯一索引并发兜底）。
func TestTemplateValidation(t *testing.T) {
	r, _ := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	cases := []struct {
		name string
		body string
		msg  string
	}{
		{"缺名称", `{"subject":"s","text":"t"}`, "模板名称不能为空"},
		{"缺主题", `{"name":"a","text":"t"}`, "subject"},
		{"无正文", `{"name":"a","subject":"s"}`, "html 与 text 至少提供一个"},
		{"主题超长", `{"name":"a","subject":"` + strings.Repeat("长", 999) + `","text":"t"}`, "≤998"},
		{"正文超限", `{"name":"a","subject":"s","text":"` + strings.Repeat("t", 1048577) + `"}`, "请求体过大"},
		{"变量清单超限", `{"name":"a","subject":"s","text":"t","variables":[` + strings.Repeat(`"v",`, 101) + `"v"]}`, "数量超限"},
		{"变量名超长", `{"name":"a","subject":"s","text":"t","variables":["` + strings.Repeat("v", 129) + `"]}`, "变量名过长"},
	}
	for _, tc := range cases {
		w := doReq(r, "POST", "/api/templates", cookie, "", tc.body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400 got %d body=%s", tc.name, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), tc.msg) {
			t.Fatalf("%s: message %q must contain %q", tc.name, w.Body.String(), tc.msg)
		}
	}

	// 重名：预检 400 + 文案
	createTemplate(t, r, cookie, templateSeed("唯一名"))
	w := doReq(r, "POST", "/api/templates", cookie, "", templateSeed("唯一名"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "模板名称已存在") {
		t.Fatalf("dup name: %d %s", w.Code, w.Body.String())
	}
}

// TestTemplateAudit CRUD 成功路径落审计（template_create/update/delete），失败请求不落。
func TestTemplateAudit(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	m := createTemplate(t, r, cookie, templateSeed("审计模板"))
	id := int(m["id"].(float64))
	mustStatus(t, doReq(r, "PUT", fmt.Sprintf("/api/templates/%d", id), cookie, "", `{"subject":"新主题 {{x}}"}`), http.StatusOK)
	// 失败请求（重名）不产生审计行
	doReq(r, "POST", "/api/templates", cookie, "", templateSeed("审计模板"))
	mustStatus(t, doReq(r, "DELETE", fmt.Sprintf("/api/templates/%d", id), cookie, "", ""), http.StatusOK)

	type row struct {
		Action string
		Target string
		Detail string
	}
	var rows []row
	if err := db.Model(&model.AuditLog{}).Where("action LIKE ?", "template_%").
		Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("audit rows: %+v", rows)
	}
	if rows[0].Action != model.AuditTemplateCreate || rows[0].Target != "template#1 name=审计模板" {
		t.Fatalf("create audit: %+v", rows[0])
	}
	if !strings.Contains(rows[0].Detail, "variables=2") {
		t.Fatalf("create audit detail: %+v", rows[0])
	}
	if rows[1].Action != model.AuditTemplateUpdate || !strings.Contains(rows[1].Detail, "subject") {
		t.Fatalf("update audit: %+v", rows[1])
	}
	if rows[2].Action != model.AuditTemplateDelete || rows[2].Target != "template#1 name=审计模板" {
		t.Fatalf("delete audit: %+v", rows[2])
	}
}

// ---------- 发送入口集成 ----------

// sendTemplateEnv 搭建「mock 渠道（priority 最优先）+ Key + 模板」环境。
func sendTemplateEnv(t *testing.T) (*gin.Engine, *gorm.DB, string, string) {
	t.Helper()
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	createChannel(t, r, cookie, `{"name":"mock主渠道","provider":"mock","priority":-10}`)
	key := createKey(t, r, cookie, "模板Key")
	return r, db, key, cookie
}

// assertSendLog 断言最近一条 SendLog 的主题/正文/模板 ID。
func assertSendLog(t *testing.T, db *gorm.DB, subject, html, text string, templateID uint) {
	t.Helper()
	var lg model.SendLog
	if err := db.Model(&model.SendLog{}).Order("id DESC").First(&lg).Error; err != nil {
		t.Fatalf("send log: %v", err)
	}
	if lg.Subject != subject {
		t.Fatalf("log subject: %q, want %q", lg.Subject, subject)
	}
	if lg.HTMLBody != html || lg.TextBody != text {
		t.Fatalf("log body: html=%q text=%q", lg.HTMLBody, lg.TextBody)
	}
	if lg.TemplateID != templateID {
		t.Fatalf("log template_id: %d, want %d", lg.TemplateID, templateID)
	}
}

// TestSendWithTemplate /v1/send 带 template_id+variables：渲染结果替换
// subject/html/text 并落 SendLog（含 template_id）；HTML 变量做转义。
func TestSendWithTemplate(t *testing.T) {
	r, db, key, cookie := sendTemplateEnv(t)
	m := createTemplate(t, r, cookie, `{
		"name":"欢迎",
		"subject":"欢迎 {{name}}",
		"html":"<p>{{name}} 的验证码是 {{code}}，<b>{{name}}</b></p>",
		"text":"{{name}} 的验证码是 {{code}}"}`)
	tplID := uint(m["id"].(float64))

	w := doReq(r, "POST", "/v1/send", "", key, fmt.Sprintf(
		`{"to":["a@example.com"],"template_id":%d,"variables":{"name":"<i>张三</i>","code":"123456"}}`, tplID))
	resp := decode(t, mustStatus(t, w, http.StatusOK))
	if resp["accepted"] != true {
		t.Fatalf("accepted: %v", resp)
	}
	if _, has := resp["missing_variables"]; has {
		t.Fatalf("no missing variables expected: %v", resp["missing_variables"])
	}
	assertSendLog(t, db, "欢迎 <i>张三</i>",
		"<p>&lt;i&gt;张三&lt;/i&gt; 的验证码是 123456，<b>&lt;i&gt;张三&lt;/i&gt;</b></p>",
		"<i>张三</i> 的验证码是 123456", tplID)

	// 日志列表 API 回显 template_id
	m = decode(t, mustStatus(t, doReq(r, "GET", "/api/logs?page_size=1", cookie, "", ""), http.StatusOK))
	item := m["items"].([]any)[0].(map[string]any)
	if got := int(item["template_id"].(float64)); got != int(tplID) {
		t.Fatalf("log api template_id: %d, want %d", got, tplID)
	}
}

// TestSendTemplateMissingVariables 未提供的变量：占位符留空，响应 missing_variables
// 可见（留空策略的响应可见性；不阻断发送）。
func TestSendTemplateMissingVariables(t *testing.T) {
	r, db, key, cookie := sendTemplateEnv(t)
	m := createTemplate(t, r, cookie, `{"name":"通知","subject":"通知 {{title}}","text":"{{a}} {{b}}"}`)
	tplID := uint(m["id"].(float64))

	w := doReq(r, "POST", "/v1/send", "", key,
		fmt.Sprintf(`{"to":["a@example.com"],"template_id":%d,"variables":{"a":"1"}}`, tplID))
	resp := decode(t, mustStatus(t, w, http.StatusOK))
	missing, _ := resp["missing_variables"].([]any)
	if len(missing) != 2 || missing[0] != "title" || missing[1] != "b" {
		t.Fatalf("missing_variables: %v, want [title b]", missing)
	}
	// 发送管道对主题做 TrimSpace（buildMessage），尾随空格不保留
	assertSendLog(t, db, "通知", "", "1 ", tplID)
}

// TestSendUnknownTemplate template_id 不存在 → 400 unknown_template（SPEC §2 错误体）。
func TestSendUnknownTemplate(t *testing.T) {
	r, _, key, _ := sendTemplateEnv(t)
	w := doReq(r, "POST", "/v1/send", "", key, `{"to":["a@example.com"],"template_id":999}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", w.Code)
	}
	errObj := decode(t, w.Body.String())["error"].(map[string]any)
	if errObj["type"] != "unknown_template" {
		t.Fatalf("type: %v", errObj["type"])
	}
}

// TestSendBodyConflict 请求同时携带 template_id 与 html/text → 400 body_conflict
// （显式失败优于静默覆盖，取舍见 send_template.go 文件头）。
func TestSendBodyConflict(t *testing.T) {
	r, _, key, cookie := sendTemplateEnv(t)
	createTemplate(t, r, cookie, templateSeed("冲突模板"))

	w := doReq(r, "POST", "/v1/send", "", key,
		`{"to":["a@example.com"],"template_id":1,"text":"旧正文"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d body=%s", w.Code, w.Body.String())
	}
	errObj := decode(t, w.Body.String())["error"].(map[string]any)
	if errObj["type"] != "body_conflict" {
		t.Fatalf("type: %v", errObj["type"])
	}
	// html 也同样冲突
	if w := doReq(r, "POST", "/v1/send", "", key,
		`{"to":["a@example.com"],"template_id":1,"html":"<p>x</p>"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("html conflict: %d", w.Code)
	}
}

// TestSendTemplateOnEmails /emails 与 /v1/send 同构：template_id+variables 同样生效。
func TestSendTemplateOnEmails(t *testing.T) {
	r, db, key, cookie := sendTemplateEnv(t)
	m := createTemplate(t, r, cookie, `{"name":"同构","subject":"S {{v}}","text":"T {{v}}"}`)
	tplID := uint(m["id"].(float64))

	w := doReq(r, "POST", "/emails", "", key,
		fmt.Sprintf(`{"to":["a@example.com"],"template_id":%d,"variables":{"v":"ok"}}`, tplID))
	mustStatus(t, w, http.StatusOK)
	assertSendLog(t, db, "S ok", "", "T ok", tplID)
}

// TestSendTemplateIdempotentReplay 幂等键重放语义不变：渲染发生在幂等检查之后，
// 同 key 重放逐字节回放存档响应（missing_variables 一并存档），不二次渲染/发送。
func TestSendTemplateIdempotentReplay(t *testing.T) {
	r, db, key, _ := sendTemplateEnv(t)
	m := createTemplate(t, r, login(t, r, "admin", "admin123"),
		`{"name":"幂等","subject":"hi {{v}}","text":"t"}`)
	tplID := uint(m["id"].(float64))

	body := fmt.Sprintf(`{"to":["a@example.com"],"template_id":%d,"variables":{"v":"first"}}`, tplID)
	first := mustStatus(t,
		idemReq(r, "POST", "/v1/send", key, body, map[string]string{"Idempotency-Key": "tpl-key-1"}),
		http.StatusOK)

	// 同 key 重放：逐字节回放存档响应，不二次渲染/发送
	w2 := idemReq(r, "POST", "/v1/send", key, body, map[string]string{"Idempotency-Key": "tpl-key-1"})
	if w2.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w2.Code, w2.Body.String())
	}
	if w2.Body.String() != first {
		t.Fatalf("replay body mismatch:\n%s\nvs\n%s", w2.Body.String(), first)
	}
	if w2.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("replay header missing")
	}
	// 只落一条日志（重放未实际发送），且渲染结果一致
	var n int64
	db.Model(&model.SendLog{}).Count(&n)
	if n != 1 {
		t.Fatalf("send logs: %d, want 1", n)
	}
	assertSendLog(t, db, "hi first", "", "t", tplID)
}

// TestChannelTestSendWithTemplate 渠道测试发送带可选模板：渲染替换默认测试内容、
// 测试日志带 template_id；unknown_template 在预留额度前拒绝（不占额度）。
func TestChannelTestSendWithTemplate(t *testing.T) {
	r, db, _, cookie := sendTemplateEnv(t)
	m := createTemplate(t, r, cookie, `{"name":"测试模板","subject":"测试 {{v}}","text":"正文 {{v}}"}`)
	tplID := uint(m["id"].(float64))

	// 成功路径：响应含渲染缺失变量提示，测试日志带模板 ID
	w := doReq(r, "POST", "/api/channels/1/test", cookie, "",
		fmt.Sprintf(`{"to":"a@example.com","template_id":%d,"variables":{}}`, tplID))
	resp := decode(t, mustStatus(t, w, http.StatusOK))
	if resp["ok"] != true {
		t.Fatalf("ok: %v / %v", resp["ok"], resp["error"])
	}
	if missing, _ := resp["missing_variables"].([]any); len(missing) != 1 || missing[0] != "v" {
		t.Fatalf("missing_variables: %v", resp["missing_variables"])
	}
	var lg model.SendLog
	if err := db.Model(&model.SendLog{}).Where("is_test = ?", true).Order("id DESC").First(&lg).Error; err != nil {
		t.Fatalf("test log: %v", err)
	}
	if lg.Subject != "测试 " || lg.TextBody != "正文 " || lg.TemplateID != tplID {
		t.Fatalf("test log: subject=%q text=%q tpl=%d", lg.Subject, lg.TextBody, lg.TemplateID)
	}

	// unknown_template：400 且不预留额度（渠道 used_day 不变）
	w = doReq(r, "POST", "/api/channels/1/test", cookie, "",
		`{"to":"a@example.com","template_id":999}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown template: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown_template") {
		t.Fatalf("error type: %s", w.Body.String())
	}
	var ch model.Channel
	db.First(&ch, 1)
	if ch.UsedDay != 1 { // 仅第一次成功测试占用
		t.Fatalf("used_day after failed test: %d, want 1", ch.UsedDay)
	}
}
