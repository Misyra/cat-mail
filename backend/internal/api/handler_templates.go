// handler_templates.go 邮件模板管理：列表/详情/新建/编辑/删除（对齐 SPEC §5
// 管理端点风格：统一错误体、指针字段部分更新语义、审计仅记成功路径）。
// 变量清单为派生数据：请求未显式提供时按 subject/html/text 内容自动提取
// （render.ExtractVars），落库为 JSON 数组字符串供前端变量提示与展示。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/render"
)

// 模板字段校验上限（SPEC §8 输入校验完备；subject 与发送请求口径一致）。
const (
	maxTemplateNameChars    = 128 // 与渠道名同口径（列 size:128）
	maxTemplateSubjectChars = 998 // RFC 单头行上限，与 /v1/send subject 校验一致
	maxTemplateVarsCount    = 100 // 显式变量清单条数上限
	maxTemplateVarNameChars = 128 // 单个变量名长度上限（与 variables 键校验一致）
)

// templateItemDTO 模板列表项（不含正文大字段，与日志列表同口径）。
type templateItemDTO struct {
	ID        uint      `json:"id"`
	Name      string    `json:"name"`
	Subject   string    `json:"subject"`
	Variables []string  `json:"variables"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// templateDetailDTO 模板详情：全字段，含 html_body/text_body。
type templateDetailDTO struct {
	templateItemDTO
	HTMLBody string `json:"html_body"`
	TextBody string `json:"text_body"`
}

// templateInput 模板创建/更新请求体。字段指针语义对齐渠道编辑（nil = 保留原值）：
// variables 未提供（nil）时按合并后的内容自动提取（变量清单是内容派生数据，
// 随内容更新保持新鲜）；显式提供空数组 = 清空变量清单。
type templateInput struct {
	Name      *string   `json:"name"`
	Subject   *string   `json:"subject"`
	HTML      *string   `json:"html"`
	Text      *string   `json:"text"`
	Variables *[]string `json:"variables"`
}

// newTemplateItem / newTemplateDetail 由落库记录构造响应体。
func newTemplateItem(t *model.Template) templateItemDTO {
	return templateItemDTO{
		ID:        t.ID,
		Name:      t.Name,
		Subject:   t.Subject,
		Variables: decodeTemplateVariables(t.Variables),
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

func newTemplateDetail(t *model.Template) templateDetailDTO {
	return templateDetailDTO{
		templateItemDTO: newTemplateItem(t),
		HTMLBody:        t.HTML,
		TextBody:        t.Text,
	}
}

// decodeTemplateVariables 落库 JSON 串 → 变量名数组；空串/解析失败返回 nil
// （防御历史数据与手工改库，详情面板不因变量清单损坏而 500）。
func decodeTemplateVariables(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// encodeTemplateVariables 变量名数组 → 落库 JSON 串；空清单存空串。
func encodeTemplateVariables(names []string) string {
	if len(names) == 0 {
		return ""
	}
	b, err := json.Marshal(names)
	if err != nil {
		return "" // 元素恒为字符串，序列化不可能失败（防御兜底）
	}
	return string(b)
}

// normalizeTemplateVars 显式提供的变量清单规范化：去空白项、去重保序；
// 超限 → 错误（调用方输出 400）。
func normalizeTemplateVars(list []string) ([]string, error) {
	if len(list) > maxTemplateVarsCount {
		return nil, fmt.Errorf("变量清单数量超限（≤%d）", maxTemplateVarsCount)
	}
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, raw := range list {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if n := utf8.RuneCountInString(name); n > maxTemplateVarNameChars {
			return nil, fmt.Errorf("变量名过长（≤%d 字符）", maxTemplateVarNameChars)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// validateTemplate 校验模板合并后的完整内容（创建与更新统一入口，SPEC §8）：
// name/subject 非空限长，html/text 各限 1MB 且至少一个非空（与发送请求的正文
// 校验同口径——避免存出发送时必然失败的模板）。
func validateTemplate(name, subject, html, text string) error {
	if n := utf8.RuneCountInString(name); n < 1 || n > maxTemplateNameChars {
		return errors.New("模板名称长度须为 1-128 个字符")
	}
	if strings.TrimSpace(subject) == "" {
		return errors.New("模板主题（subject）不能为空")
	}
	if utf8.RuneCountInString(subject) > maxTemplateSubjectChars {
		return fmt.Errorf("模板主题过长（≤%d 字符）", maxTemplateSubjectChars)
	}
	if utf8.RuneCountInString(html) > maxBodyChars || utf8.RuneCountInString(text) > maxBodyChars {
		return fmt.Errorf("正文过大：html/text 各不超过 %d 字符", maxBodyChars)
	}
	if strings.TrimSpace(html) == "" && strings.TrimSpace(text) == "" {
		return errors.New("html 与 text 至少提供一个")
	}
	return nil
}

// listTemplates GET /api/templates：全量列表（id 升序，模板为低频管理资源、
// 无分页需求，与渠道列表口径一致）。
func (s *Server) listTemplates(c *gin.Context) {
	var tpls []model.Template
	if err := s.db.Order("id ASC").Find(&tpls).Error; err != nil {
		serverError(c, err)
		return
	}
	items := make([]templateItemDTO, 0, len(tpls))
	for i := range tpls {
		items = append(items, newTemplateItem(&tpls[i]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// getTemplate GET /api/templates/:id：完整详情（含正文）。
func (s *Server) getTemplate(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var tpl model.Template
	if err := s.db.First(&tpl, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "模板不存在")
			return
		}
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, newTemplateDetail(&tpl))
}

// createTemplate POST /api/templates：name/subject 必填，html/text 至少一个，
// 变量清单缺省时自动提取；重名 400（预检 + 唯一索引并发兜底，与渠道同口径）。
func (s *Server) createTemplate(c *gin.Context) {
	var in templateInput
	if !bindJSON(c, &in) {
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		badRequest(c, "模板名称不能为空")
		return
	}
	if in.Subject == nil || strings.TrimSpace(*in.Subject) == "" {
		badRequest(c, "模板主题（subject）不能为空")
		return
	}
	name := strings.TrimSpace(*in.Name)
	subject := strings.TrimSpace(*in.Subject)
	htmlBody, textBody := "", ""
	if in.HTML != nil {
		htmlBody = *in.HTML
	}
	if in.Text != nil {
		textBody = *in.Text
	}
	if err := validateTemplate(name, subject, htmlBody, textBody); err != nil {
		badRequest(c, err.Error())
		return
	}
	variables, err := resolveTemplateVariables(in.Variables, subject, htmlBody, textBody)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	tpl := &model.Template{
		Name:      name,
		Subject:   subject,
		HTML:      htmlBody,
		Text:      textBody,
		Variables: encodeTemplateVariables(variables),
	}
	if err := s.db.Create(tpl).Error; err != nil {
		// 并发重名兜底：唯一约束冲突（TranslateError）映射 400
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			badRequest(c, errTemplateNameTaken.Error())
			return
		}
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录；正文/变量值不入审计，仅记变量条数
	s.audit(c, model.AuditTemplateCreate,
		fmt.Sprintf("template#%d name=%s", tpl.ID, tpl.Name),
		fmt.Sprintf("variables=%d", len(variables)))
	c.JSON(http.StatusOK, newTemplateDetail(tpl))
}

// updateTemplate PUT /api/templates/:id：指针字段 nil = 保留原值；variables
// 未提供时按合并后内容重新提取（派生数据随内容保持新鲜）；重名 400。
func (s *Server) updateTemplate(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var tpl model.Template
	if err := s.db.First(&tpl, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "模板不存在")
			return
		}
		serverError(c, err)
		return
	}
	var in templateInput
	if !bindJSON(c, &in) {
		return
	}
	old := tpl
	if in.Name != nil {
		if name := strings.TrimSpace(*in.Name); name != "" {
			tpl.Name = name // 名称不允许为空：显式空串视作未修改
		}
	}
	if in.Subject != nil && strings.TrimSpace(*in.Subject) != "" {
		tpl.Subject = strings.TrimSpace(*in.Subject) // 主题不允许为空：同上
	}
	if in.HTML != nil {
		tpl.HTML = *in.HTML
	}
	if in.Text != nil {
		tpl.Text = *in.Text
	}
	if err := validateTemplate(tpl.Name, tpl.Subject, tpl.HTML, tpl.Text); err != nil {
		badRequest(c, err.Error())
		return
	}
	variables, err := resolveTemplateVariables(in.Variables, tpl.Subject, tpl.HTML, tpl.Text)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	tpl.Variables = encodeTemplateVariables(variables)
	if err := s.ensureTemplateNameFree(tpl.Name, tpl.ID); err != nil {
		badRequest(c, err.Error())
		return
	}
	// 只写模板可写集（无运行时计数列，全量 Select 覆盖清空的字段）
	if err := s.db.Model(&model.Template{}).Where("id = ?", tpl.ID).
		Select("Name", "Subject", "HTML", "Text", "Variables").
		Updates(tpl).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			badRequest(c, errTemplateNameTaken.Error())
			return
		}
		serverError(c, err)
		return
	}
	// 审计（E4）：仅记变更字段名，值不入审计（正文可能含业务敏感内容）
	s.audit(c, model.AuditTemplateUpdate,
		fmt.Sprintf("template#%d name=%s", tpl.ID, tpl.Name),
		joinAuditFields(changedTemplateFields(&old, &tpl)))
	c.JSON(http.StatusOK, newTemplateDetail(&tpl))
}

// resolveTemplateVariables 变量清单解析：显式提供 → 规范化（可清空）；
// 未提供 → 按内容自动提取（subject/html/text 三处合并）。
func resolveTemplateVariables(provided *[]string, subject, html, text string) ([]string, error) {
	if provided != nil {
		return normalizeTemplateVars(*provided)
	}
	vars := render.ExtractVars(subject, html, text)
	if len(vars) > maxTemplateVarsCount {
		// 自动提取同样受数量上限约束（模板正文写满占位符的极端情况）
		return nil, fmt.Errorf("模板变量数量超限（≤%d）", maxTemplateVarsCount)
	}
	return vars, nil
}

// changedTemplateFields 比较模板更新前后的差异，返回变更字段名列表（json 名）。
func changedTemplateFields(old, new *model.Template) []string {
	type field struct {
		name    string
		changed bool
	}
	fields := []field{
		{"name", old.Name != new.Name},
		{"subject", old.Subject != new.Subject},
		{"html", old.HTML != new.HTML},
		{"text", old.Text != new.Text},
		{"variables", old.Variables != new.Variables},
	}
	var changed []string
	for _, f := range fields {
		if f.changed {
			changed = append(changed, f.name)
		}
	}
	return changed
}

// deleteTemplate DELETE /api/templates/:id。历史 SendLog 保留 template_id 供
// 追溯（名称不冗余存储，前端经模板列表解析，已删除的显示为 ID）。
func (s *Server) deleteTemplate(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var tpl model.Template
	if err := s.db.First(&tpl, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notFound(c, "模板不存在")
			return
		}
		serverError(c, err)
		return
	}
	if err := s.db.Delete(&model.Template{}, id).Error; err != nil {
		serverError(c, err)
		return
	}
	// 审计（E4）：仅成功路径记录
	s.audit(c, model.AuditTemplateDelete,
		fmt.Sprintf("template#%d name=%s", tpl.ID, tpl.Name), "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// errTemplateNameTaken 模板重名。
var errTemplateNameTaken = errors.New("模板名称已存在")

// ensureTemplateNameFree 校验模板名唯一（excludeID 用于更新时排除自身）。
func (s *Server) ensureTemplateNameFree(name string, excludeID uint) error {
	var n int64
	q := s.db.Model(&model.Template{}).Where("name = ?", name)
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	if err := q.Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return errTemplateNameTaken
	}
	return nil
}
