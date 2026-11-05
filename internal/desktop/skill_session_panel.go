package desktop

// 会话技能面板（用户需求 2026-12：聊天页右侧「技能」TAB）：
//   - 用户把技能库中的技能显式「添加」进会话，右侧面板只显示已添加技能；
//   - 每个已添加技能可配置参数（技能 SKILL.md frontmatter 中的 params 声明
//     驱动前端动态表单；未声明参数的技能仅显示描述）；
//   - 「运行」按钮把技能正文 + 参数以任务形式发进当前会话。
//
// 持久化：随会话（session.SessionSkills map[技能名]参数JSON）落盘
// ~/.kaelvoris/sessions/<id>.json，重启/续聊不丢。
//
// 技能参数声明约定（对 kaelvoris 技能 frontmatter 的轻量扩展，向后兼容）：
//   ---
//   name: my-skill
//   description: ...
//   params:
//     - name: target
//       label: 目标路径
//       type: string        # string | text | number | boolean | select
//       required: true
//       default: ./doc
//       options: [a, b, c]  # 仅 select 用
//       description: 参数说明
//   ---

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kaelvoris/internal/session"
	"kaelvoris/internal/skills"
)

// SkillParamField 技能可配置参数声明（前端动态表单依据）。
type SkillParamField struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // string | text | number | boolean | select
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
	Description string   `json:"description,omitempty"`
}

// SessionSkillView 右栏已挂载技能条目（元数据 + 当前参数 + 表单声明）。
type SessionSkillView struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Source      string            `json:"source"`
	ModeSlugs   []string          `json:"modeSlugs,omitempty"`
	Params      []SkillParamField `json:"params,omitempty"`    // frontmatter params 声明（空 = 无可配置参数）
	Values      map[string]string `json:"values,omitempty"`    // 当前配置值（JSON 解析后）
	RawValues   string            `json:"rawValues,omitempty"` // 参数 JSON 原文（编辑回显）
}

// sessionSkillMounts 取会话技能挂载表（nil 安全；返回内部 map 副本）。
func (a *App) sessionSkillMounts(sessionID string) map[string]string {
	if sessionID == "" {
		return nil
	}
	s, err := a.store.Get(sessionID)
	if err != nil {
		return nil
	}
	s.Mu().RLock()
	defer s.Mu().RUnlock()
	out := make(map[string]string, len(s.SessionSkills))
	for k, v := range s.SessionSkills {
		out[k] = v
	}
	return out
}

// GetSessionSkills 已挂载技能列表（元数据 + 参数声明 + 当前值）。
// 挂载记录指向已删除技能时仍返回条目（Source=missing），前端可提示清理。
func (a *App) GetSessionSkills(sessionID string) []SessionSkillView {
	mounts := a.sessionSkillMounts(sessionID)
	if len(mounts) == 0 {
		return []SessionSkillView{}
	}
	// 一次发现全量技能，再按挂载名查元数据。
	mgr := a.currentSkillMgr()
	byName := map[string]skills.Metadata{}
	for _, m := range mgr.All() {
		byName[m.Name] = m
	}
	out := make([]SessionSkillView, 0, len(mounts))
	for name, raw := range mounts {
		view := SessionSkillView{Name: name, RawValues: raw, Values: map[string]string{}}
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &view.Values) // 损坏 JSON：按空参数处理不致命
		}
		if m, ok := byName[name]; ok {
			view.Description = m.Description
			view.Source = m.Source
			view.ModeSlugs = m.ModeSlugs
			view.Params = skillParamFields(m.Name, mgr)
		} else {
			view.Source = "missing" // 技能目录已被删除，前端提示清理
		}
		out = append(out, view)
	}
	// 名称稳定排序（map 遍历无序，避免面板顺序抖动）。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// skillParamFields 读取技能 SKILL.md frontmatter 的 params 声明（解析失败 = 无参数）。
func skillParamFields(name string, mgr *skills.Manager) []SkillParamField {
	raw, err := mgr.LoadRaw(name)
	if err != nil {
		return nil
	}
	return parseSkillParams(raw)
}

// parseSkillParams 从 SKILL.md 全文中解析 frontmatter params 块。
// 轻量逐行解析（不引 YAML 库）：识别 `params:` 列表与 `- name/label/type/
// required/default/options/description` 字段行；正文中的同名文本不参与
// （frontmatter 结束符 `---` 后停止）。
func parseSkillParams(raw string) []SkillParamField {
	lines := strings.Split(raw, "\n")
	inFM := false
	inParams := false
	var out []SkillParamField
	var cur *SkillParamField
	for _, ln := range lines {
		t := strings.TrimRight(ln, "\r")
		trimmed := strings.TrimSpace(t)
		if trimmed == "---" {
			if !inFM {
				inFM = true // frontmatter 开始
				continue
			}
			break // frontmatter 结束：停止解析（正文不再参与）
		}
		if !inFM {
			continue
		}
		if trimmed == "params:" {
			inParams = true
			continue
		}
		if !inParams {
			continue
		}
		// params 块结束（回到无缩进的其它键）。
		if !strings.HasPrefix(t, " ") && !strings.HasPrefix(t, "\t") {
			if cur != nil {
				out = append(out, *cur)
				cur = nil
			}
			inParams = false
			continue
		}
		if strings.HasPrefix(trimmed, "-") {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &SkillParamField{}
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			if k, v, ok := cutKV(rest); ok && k == "name" {
				cur.Name = v // 行内写法：- name: target
			} else if rest != "" {
				cur.Name = rest // 罕见：- target（直接值）
			}
			continue
		}
		if cur == nil {
			continue
		}
		k, v, ok := cutKV(trimmed)
		if !ok {
			continue
		}
		switch k {
		case "name":
			cur.Name = v
		case "label":
			cur.Label = v
		case "type":
			cur.Type = v
		case "required":
			cur.Required = v == "true" || v == "yes"
		case "default":
			cur.Default = v
		case "description":
			cur.Description = v
		case "options":
			cur.Options = parseInlineList(v)
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	// 过滤无效项（无 name 的声明无法配置）。
	res := make([]SkillParamField, 0, len(out))
	for _, p := range out {
		if p.Name != "" {
			if p.Label == "" {
				p.Label = p.Name
			}
			if p.Type == "" {
				p.Type = "string"
			}
			res = append(res, p)
		}
	}
	return res
}

// cutKV "key: value" 切分（去引号）。
func cutKV(s string) (string, string, bool) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", false
	}
	k := strings.TrimSpace(s[:i])
	v := strings.TrimSpace(s[i+1:])
	v = strings.Trim(v, `"'`)
	return k, v, k != ""
}

// parseInlineList "a, b, c" → [a b c]（select 选项行内写法）。
func parseInlineList(v string) []string {
	v = strings.Trim(v, "[]")
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.Trim(strings.TrimSpace(p), `"'`); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// AddSessionSkill 把技能添加进会话（右侧技能 TAB「添加」入口）。
// 重复添加幂等（保留已有参数配置）。
func (a *App) AddSessionSkill(sessionID, skillName string) error {
	sessionID = strings.TrimSpace(sessionID)
	skillName = strings.TrimSpace(skillName)
	if sessionID == "" {
		return fmt.Errorf("会话 ID 不能为空")
	}
	if skillName == "" {
		return fmt.Errorf("技能名不能为空")
	}
	mgr := a.currentSkillMgr()
	found := false
	for _, m := range mgr.All() {
		if m.Name == skillName {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("技能不存在: %s", skillName)
	}
	s, err := a.store.Get(sessionID)
	if err != nil {
		return err
	}
	s.Mu().Lock()
	if s.SessionSkills == nil {
		s.SessionSkills = map[string]string{}
	}
	if _, ok := s.SessionSkills[skillName]; !ok {
		s.SessionSkills[skillName] = "" // 新添加：无参数
	}
	s.Mu().Unlock()
	a.touchSession(s)
	return nil
}

// SetSessionSkillParams 保存已挂载技能的参数配置（values 任意可 JSON 序列化
// 对象；后端统一存为 JSON 字符串）。
func (a *App) SetSessionSkillParams(sessionID, skillName string, values map[string]interface{}) error {
	sessionID = strings.TrimSpace(sessionID)
	skillName = strings.TrimSpace(skillName)
	if sessionID == "" || skillName == "" {
		return fmt.Errorf("会话 ID 与技能名不能为空")
	}
	s, err := a.store.Get(sessionID)
	if err != nil {
		return err
	}
	raw := ""
	if len(values) > 0 {
		b, err := json.Marshal(values)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	s.Mu().Lock()
	if s.SessionSkills == nil {
		s.SessionSkills = map[string]string{}
	}
	s.SessionSkills[skillName] = raw
	s.Mu().Unlock()
	a.touchSession(s)
	return nil
}

// RemoveSessionSkill 从会话移除技能（不动技能库本体）。
func (a *App) RemoveSessionSkill(sessionID, skillName string) error {
	sessionID = strings.TrimSpace(sessionID)
	skillName = strings.TrimSpace(skillName)
	if sessionID == "" {
		return fmt.Errorf("会话 ID 不能为空")
	}
	s, err := a.store.Get(sessionID)
	if err != nil {
		return err
	}
	s.Mu().Lock()
	delete(s.SessionSkills, skillName)
	s.Mu().Unlock()
	a.touchSession(s)
	return nil
}

// touchSession 更新会话时间戳并触发列表推送（技能增删改后侧栏会话卡可感知）。
// 沿用 SetSessionWorkspace 既有落盘模式：SetXxx → Save → pushSessionsChanged。
func (a *App) touchSession(s *session.Session) {
	a.store.Save(s)
	a.pushSessionsChanged()
}

// nowMillis 当前毫秒时间戳（新会话 ID 生成，与 app.go 内联格式一致）。
func nowMillis() int64 { return time.Now().UnixMilli() }

// RunSessionSkill 一键运行：把技能正文 + 参数注入提示词，作为任务发送到当前会话。
// 返回会话 ID（前端复用 sendCurrent 的会话接管语义）。
func (a *App) RunSessionSkill(sessionID, skillName, valuesJSON string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	skillName = strings.TrimSpace(skillName)
	if skillName == "" {
		return "", fmt.Errorf("技能名不能为空")
	}
	mgr := a.currentSkillMgr()
	c, err := mgr.Load(skillName)
	if err != nil {
		return "", fmt.Errorf("技能加载失败: %w", err)
	}
	// 参数回填：运行时以 valuesJSON 为准（缺省回退已保存配置，再回退默认值）。
	vals := map[string]string{}
	if raw := strings.TrimSpace(valuesJSON); raw != "" {
		_ = json.Unmarshal([]byte(raw), &vals)
	}
	if saved := a.sessionSkillMounts(sessionID); saved != nil {
		for k, v := range parseValuesJSON(saved[skillName]) {
			if _, ok := vals[k]; !ok {
				vals[k] = v
			}
		}
	}
	prompt := buildSkillRunPrompt(c, vals, a.skillParamFieldsFor(skillName))
	// 新会话运行：先挂载技能再发任务，保证会话面板与任务语义一致。
	if sessionID == "" {
		sessionID = fmt.Sprintf("%d", nowMillis())
		if err := a.AddSessionSkill(sessionID, skillName); err != nil {
			return "", err
		}
	}
	sid, err := a.SendMessage(sessionID, prompt, nil)
	if err != nil {
		return "", err
	}
	return sid, nil
}

// skillParamFieldsFor 对外暴露参数声明查询（RunSessionSkill 组装提示词用）。
func (a *App) skillParamFieldsFor(name string) []SkillParamField {
	return skillParamFields(name, a.currentSkillMgr())
}

// sessionSkillsSnippet 会话挂载技能注入段（runTask 每轮任务拼入任务文本）：
// 无挂载返回空串（零注入零开销）。每个技能输出名称/参数/指令全文，模型
// 执行任务时遵循已挂载技能的约束与流程（与 checkpoint-restore 同级：
// 一次性 user 消息语义，不入系统提示词）。
func (a *App) sessionSkillsSnippet(sessionID string) string {
	mounts := a.sessionSkillMounts(sessionID)
	if len(mounts) == 0 {
		return ""
	}
	mgr := a.currentSkillMgr()
	var b strings.Builder
	b.WriteString("[session-skills] 本会话已挂载以下技能，执行任务时请遵循其指令与约束：")
	for name, raw := range mounts {
		c, err := mgr.Load(name)
		if err != nil {
			continue // 技能目录已删除：静默跳过（面板有 missing 提示清理）
		}
		b.WriteString("\n\n=== 技能: " + c.Name + " ===")
		if c.Description != "" {
			b.WriteString("\n（" + c.Description + "）")
		}
		if vals := parseValuesJSON(raw); len(vals) > 0 {
			b.WriteString("\n用户配置参数：")
			for k, v := range vals {
				b.WriteString("\n- " + k + " = " + v)
			}
		}
		b.WriteString("\n" + c.Body)
	}
	return b.String()
}

// parseValuesJSON 参数 JSON 字符串 → map（损坏输入静默为空 map，不致命）。
func parseValuesJSON(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}
	}
	out := map[string]string{}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// buildSkillRunPrompt 技能运行提示词组装：
//   - 技能正文全文（frontmatter 之后）作为执行指令主体；
//   - 用户配置参数以「参数名 = 值」清单列出（无参数则省略参数段）。
//   - 约束：仅执行本技能范围，不动其它挂载技能。
func buildSkillRunPrompt(c *skills.Content, vals map[string]string, fields []SkillParamField) string {
	var b strings.Builder
	b.WriteString("请严格按照以下技能的指令执行任务。\n\n[技能] ")
	b.WriteString(c.Name)
	if c.Description != "" {
		b.WriteString(" — " + c.Description)
	}
	// 参数段：声明字段顺序输出（含未配置的默认值），值取 values 优先。
	if len(fields) > 0 {
		b.WriteString("\n\n[参数]")
		for _, f := range fields {
			v, ok := vals[f.Name]
			if !ok || v == "" {
				v = f.Default
			}
			if v == "" {
				v = "（未配置）"
			}
			b.WriteString("\n- " + f.Label + " (" + f.Name + ") = " + v)
		}
	}
	// 额外运行时值（声明外的自由参数一并传递，容错用户自定义）。
	if len(vals) > 0 {
		declared := map[string]bool{}
		for _, f := range fields {
			declared[f.Name] = true
		}
		extra := make([]string, 0)
		for k, v := range vals {
			if !declared[k] && v != "" {
				extra = append(extra, k+" = "+v)
			}
		}
		if len(extra) > 0 {
			b.WriteString("\n\n[附加参数]")
			for _, e := range extra {
				b.WriteString("\n- " + e)
			}
		}
	}
	b.WriteString("\n\n[技能指令全文]\n")
	b.WriteString(c.Body)
	return b.String()
}
