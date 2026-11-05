// Package skills 技能系统：移植自 Zoo-Code src/services/skills/SkillsManager.ts。
//
// 核心语义对齐：
//   - 扫描全局(~/.kaelvoris/skills, ~/.agents/skills)、项目(.kaelvoris/skills, .agents/skills, .roo/skills)
//     与插件（~/.kaelvoris/plugins/<id>/skills/）
//   - 内置技能库（builtin/，go:embed，来源 参考/open-design-main/skills 精选包，
//     对齐 codex-rs skills install_system_skills 的"随发行版分发内置技能"语义）
//   - SKILL.md frontmatter（name/description/modeSlugs）按 agentskills.io 规范解析
//   - 覆盖规则：project > global > builtin（同名技能高层级胜出）
//   - getSkillsForMode 注入系统提示词 available_skills 段
package skills

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const skillNameMaxLen = 64

//go:embed all:builtin
var builtinFS embed.FS

// constBuiltinDir embed 根目录（子目录 = 技能包，各含 SKILL.md）。
const constBuiltinDir = "builtin"

// Metadata 技能元数据（对齐 SkillMetadata）。
type Metadata struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Path        string   `json:"path"` // SKILL.md 绝对路径
	Source      string   `json:"source"`
	ModeSlugs   []string `json:"modeSlugs,omitempty"`
	// AllowedTools 技能当轮免审工具清单（D3 对标 Claude Code allowed-tools；
	// 空 = 无授权，技能加载不影响审批流程）。
	AllowedTools []string `json:"allowedTools,omitempty"`
}

// Content 技能加载后的完整内容（对齐 SkillContent）。
type Content struct {
	Metadata
	Body string `json:"body"` // frontmatter 之后的 markdown 正文
}

type dirSpec struct {
	dir    string
	source string
}

// skillDirs 返回扫描目录（顺序即优先级：先扫描的被后扫描的同名覆盖）。
// builtin 最先扫描（优先级最低），同名时被用户目录同名技能覆盖——内置技能
// 即"随发行版附赠的默认技能"，用户可通过 ~/.agents/skills/<name> 定制替代。
// pluginSkillDirs 已安装插件自带的技能目录（<home>/.kaelvoris/plugins/<id>/skills/，
// 安装时经 CopyFrom 全目录复制落地）。读取失败（无 plugins 目录）返回 nil。
func pluginSkillDirs(home string) []string {
	entries, err := os.ReadDir(filepath.Join(home, ".kaelvoris", "plugins"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(home, ".kaelvoris", "plugins", e.Name(), "skills"))
		}
	}
	return out
}

func skillDirs(home, workspace string) []dirSpec {
	var specs []dirSpec
	specs = append(specs, dirSpec{"<builtin>", "builtin"})
	specs = append(specs, dirSpec{filepath.Join(home, ".agents", "skills"), "global"})
	specs = append(specs, dirSpec{filepath.Join(home, ".kaelvoris", "skills"), "global"})
	// 插件技能（"plugin" 层级，同名时被用户 global/project 覆盖）。
	for _, d := range pluginSkillDirs(home) {
		specs = append(specs, dirSpec{d, "plugin"})
	}
	if workspace != "" {
		specs = append(specs, dirSpec{filepath.Join(workspace, ".agents", "skills"), "project"})
		specs = append(specs, dirSpec{filepath.Join(workspace, ".roo", "skills"), "project"})
		specs = append(specs, dirSpec{filepath.Join(workspace, ".kaelvoris", "skills"), "project"})
	}
	return specs
}

// Manager 技能管理器（简版 SkillsManager：无文件监听）。
type Manager struct {
	mu        sync.RWMutex // 保护 skills 索引（Discover 与读并发；发现/加载双入口）
	skills    map[string]Metadata
	home      string
	workspace string
}

// NewManager 创建并立即发现技能。
func NewManager(home, workspace string) *Manager {
	m := &Manager{skills: map[string]Metadata{}, home: home, workspace: workspace}
	m.Discover()
	return m
}

// Discover 重新扫描全部技能目录（builtin 走 embed FS，其余走磁盘）。
func (m *Manager) Discover() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.discoverLocked()
}

// discoverLocked 无锁发现（调用方需持 m.mu）。
func (m *Manager) discoverLocked() {
	m.skills = map[string]Metadata{}
	for _, s := range skillDirs(m.home, m.workspace) {
		if s.source == "builtin" {
			m.scanBuiltin()
			continue
		}
		m.scanDir(s.dir, s.source)
	}
}

// scanBuiltin 扫描 embed 内置技能库（结构与磁盘 skillDir 一致：一级子目录 +
// SKILL.md；子目录内 references/ 等辅助文件随包嵌入）。
func (m *Manager) scanBuiltin() {
	entries, err := builtinFS.ReadDir(constBuiltinDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := builtinFS.ReadFile(constBuiltinDir + "/" + e.Name() + "/SKILL.md")
		if err != nil {
			continue
		}
		fm, _, err := parseFrontmatter(b)
		if err != nil {
			continue
		}
		name := fm.Name
		if name == "" {
			name = e.Name()
		}
		// 校验规则与磁盘技能一致（name/目录名一致 + 合法字符）。
		if fm.Name != "" && fm.Name != e.Name() {
			continue
		}
		if !validName(name) {
			continue
		}
		// 覆盖规则：后扫描的高层级覆盖 builtin（Discover 先扫描 builtin）。
		m.skills[name] = Metadata{
			Name:        name,
			Description: fm.Description,
			// Path 为 embed 路径（builtin/<dir>/SKILL.md），Load 按来源区分读取。
			Path:         constBuiltinDir + "/" + e.Name() + "/SKILL.md",
			Source:       "builtin",
			ModeSlugs:    fm.ModeSlugs,
			AllowedTools: fm.AllowedTools,
		}
	}
}

// scanDir 扫描技能目录下的一级子目录，寻找 SKILL.md。
func (m *Manager) scanDir(dir, source string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // 目录不存在跳过
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m.loadMetadata(filepath.Join(dir, e.Name()), source, e.Name())
	}
}

// loadMetadata 加载单个 SKILL.md（对齐 loadSkillMetadata：校验 name 匹配目录名规范）。
// agentskills.io 规范：name 必须与技能目录名一致；缺省时取目录名。
func (m *Manager) loadMetadata(skillDir, source, dirName string) {
	b, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return
	}
	fm, _, err := parseFrontmatter(b)
	if err != nil {
		return
	}
	// name 缺省取目录名（对齐 effectiveSkillName = skillName || basename）。
	name := fm.Name
	if name == "" {
		name = dirName
	}
	// 规范要求 name 与目录名一致（agentskills.io）；显式 name 不匹配则跳过。
	if fm.Name != "" && fm.Name != dirName {
		return
	}
	if !validName(name) {
		return
	}
	md := Metadata{
		Name:         name,
		Description:  fm.Description,
		Path:         filepath.Join(skillDir, "SKILL.md"),
		Source:       source,
		ModeSlugs:    fm.ModeSlugs,
		AllowedTools: fm.AllowedTools,
	}
	// 覆盖规则：project > global（后写入胜出，skillDirs 顺序已保证）。
	m.skills[md.Name] = md
}

// contains 判断字符串列表是否包含指定元素。
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// All 返回全部已发现技能（设置页展示用）。
func (m *Manager) All() []Metadata {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Metadata, 0, len(m.skills))
	for _, s := range m.skills {
		out = append(out, s)
	}
	return out
}

// ForMode 返回当前模式可用技能（对齐 getSkillsForMode；无 modeSlugs = 全模式可用）。
func (m *Manager) ForMode(mode string) []Metadata {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Metadata{}
	for _, s := range m.skills {
		if len(s.ModeSlugs) == 0 || contains(s.ModeSlugs, mode) {
			out = append(out, s)
		}
	}
	return out
}

// Get 加载技能内容（对齐 getSkillContent；builtin 技能从 embed FS 读取）。
// 索引 miss 时自动 Discover 刷新重试一次——列表（ListSkills 每次新建 Manager）
// 与读取（SkillRaw/引擎 SkillTool 走缓存 Manager）必须口径一致：外部在任务
// 运行期间新增/删除技能文件后，缓存索引仍能读到最新结果（否则「列表显示、
// 点击 not found」）。
func (m *Manager) Load(name string) (*Content, error) {
	m.mu.RLock()
	_, ok := m.skills[name]
	m.mu.RUnlock()
	if !ok {
		m.Discover() // 磁盘变动后刷新索引（读操作最坏一次扫描，可忽略）
	}
	m.mu.RLock()
	md, ok := m.skills[name]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("skill %q not found", name)
	}
	var b []byte
	var err error
	if md.Source == "builtin" {
		b, err = builtinFS.ReadFile(md.Path)
	} else {
		b, err = os.ReadFile(md.Path)
	}
	if err != nil {
		return nil, fmt.Errorf("read skill: %w", err)
	}
	_, body, err := parseFrontmatter(b)
	if err != nil {
		return nil, err
	}
	return &Content{Metadata: md, Body: body}, nil
}

// ReadFile 读取技能包内任意文件（相对技能目录，如 references/guidelines.md）。
func (m *Manager) ReadFile(name, rel string) (string, error) {
	m.mu.RLock()
	_, ok := m.skills[name]
	m.mu.RUnlock()
	if !ok {
		m.Discover()
	}
	m.mu.RLock()
	md, ok := m.skills[name]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	if strings.Contains(rel, "..") {
		return "", fmt.Errorf("invalid path %q", rel)
	}
	skillRoot := strings.TrimSuffix(filepath.ToSlash(md.Path), "/SKILL.md")
	full := skillRoot + "/" + rel
	if md.Source == "builtin" {
		b, err := builtinFS.ReadFile(full)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	b, err := os.ReadFile(filepath.FromSlash(full))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// LoadRaw 返回技能 SKILL.md 原始全文（frontmatter+body，编辑器回显）。
// builtin 技能从 embed 读取（只读语义由 UI 层按 Source 禁编辑）。
// 索引 miss 时自动 Discover 刷新重试（与 Load 同口径，见 Load 注释）。
func (m *Manager) LoadRaw(name string) (string, error) {
	m.mu.RLock()
	_, ok := m.skills[name]
	m.mu.RUnlock()
	if !ok {
		m.Discover()
	}
	m.mu.RLock()
	md, ok := m.skills[name]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}
	var b []byte
	var err error
	if md.Source == "builtin" {
		b, err = builtinFS.ReadFile(md.Path)
	} else {
		b, err = os.ReadFile(md.Path)
	}
	if err != nil {
		return "", fmt.Errorf("read skill: %w", err)
	}
	return string(b), nil
}

// Save 写入/新建技能，统一落 project 层 <workspace>/.kaelvoris/skills/<name>/SKILL.md。
// 校验：validName(name)；content 若含 frontmatter 显式 name 必须与 name 一致。
// 成功后 Discover() 刷新索引。workspace 为空时报错。
func (m *Manager) Save(name, content string) error {
	if !validName(name) {
		return fmt.Errorf("invalid skill name %q (want lowercase letters, digits, hyphen; max %d chars)", name, skillNameMaxLen)
	}
	if m.workspace == "" {
		return fmt.Errorf("workspace not set: cannot save skill %q", name)
	}
	// frontmatter 显式 name 与目标名不一致则拒绝（agentskills.io 规范：name 与目录名一致）。
	if fm, _, err := parseFrontmatter([]byte(content)); err == nil && fm.Name != "" && fm.Name != name {
		return fmt.Errorf("frontmatter name %q conflicts with skill name %q", fm.Name, name)
	}
	dir := filepath.Join(m.workspace, ".kaelvoris", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	p := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}
	m.Discover()
	return nil
}

// Delete 删除技能目录（RemoveAll 技能根目录）。builtin 禁删（返回错误）。
// 同名多层时按 Metadata.Path 定位实际层级删除；成功后 Discover()。
func (m *Manager) Delete(name string) error {
	m.mu.RLock()
	md, ok := m.skills[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("skill %q not found", name)
	}
	if md.Source == "builtin" {
		return fmt.Errorf("skill %q is builtin: cannot delete", name)
	}
	skillDir := filepath.Dir(md.Path)
	// 防御：只允许删除 SKILL.md 所在的技能根目录，根目录名必须等于技能名。
	if filepath.Base(skillDir) != name {
		return fmt.Errorf("refusing to delete %q: dir name mismatch", skillDir)
	}
	if err := os.RemoveAll(skillDir); err != nil {
		return fmt.Errorf("remove skill dir: %w", err)
	}
	m.Discover()
	return nil
}

// BuiltinNames 返回内置技能名清单（设置页分组展示用）。
func (m *Manager) BuiltinNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []string{}
	for name, s := range m.skills {
		if s.Source == "builtin" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Names 返回全部技能名（错误提示用，对齐 available skills 列表）。
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.skills))
	for name := range m.skills {
		out = append(out, name)
	}
	return out
}

// frontmatter SKILL.md YAML frontmatter 字段（轻量解析，不引第三方依赖）。
type frontmatter struct {
	Name         string
	Description  string
	ModeSlugs    []string
	AllowedTools []string // allowed-tools 字段（空格/逗号分隔或列表项；D3）
}

// parseFrontmatter 解析 `---` 包裹的 YAML frontmatter（仅支持扁平 key: value 与 modeSlugs 列表）。
func parseFrontmatter(b []byte) (*frontmatter, string, error) {
	s := string(b)
	if !strings.HasPrefix(s, "---") {
		return &frontmatter{}, string(b), nil
	}
	rest := s[3:]
	// 允许 ---\n 与 ----\n 两种开头。
	rest = strings.TrimLeft(rest, "-")
	nl := strings.Index(rest, "\n")
	if nl < 0 {
		return nil, "", fmt.Errorf("malformed frontmatter")
	}
	end := strings.Index(rest[nl:], "\n---")
	if end < 0 {
		return nil, "", fmt.Errorf("frontmatter not closed")
	}
	raw := rest[nl:end]
	body := strings.TrimPrefix(rest[end:], "\n---")
	// body 去掉 --- 行剩余部分。
	if idx := strings.Index(body, "\n"); idx >= 0 {
		body = body[idx+1:]
	} else {
		body = ""
	}

	fm := &frontmatter{}
	var currentList []string
	var currentKey string
	// YAML 块标量（open-design 技能包 description: | 多行写法）：
	// 收集比 key 缩进更深的后续行，按空格折叠为单行描述。
	var blockKey string
	var blockLines []string
	flushBlock := func() {
		if blockKey != "" {
			if blockKey == "description" {
				fm.Description = strings.TrimSpace(strings.Join(blockLines, " "))
			}
			blockKey = ""
			blockLines = nil
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if blockKey != "" {
			// 块标量收集：缩进非空行继续；非缩进行（新 key/列表）终止。
			if line != "" && (line[0] == ' ' || line[0] == '\t') {
				blockLines = append(blockLines, trimmed)
				continue
			}
			flushBlock()
		}
		if strings.HasPrefix(trimmed, "- ") {
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			currentList = append(currentList, item)
			continue
		}
		// 遇到新的 key，冲刷上一个列表。
		if currentKey != "" && len(currentList) > 0 {
			applyList(fm, currentKey, currentList)
			currentList = nil
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// 块标量开始（| > 及其 chomp 指示符）：进入多行收集。
		if val == "|" || val == "|-" || val == "|+" || val == ">" || val == ">-" || val == ">+" {
			blockKey = key
			blockLines = nil
			currentKey = ""
			continue
		}
		currentKey = key
		if val == "" {
			continue // 列表字段，等后续行
		}
		val = strings.Trim(val, `"'`)
		switch key {
		case "name":
			fm.Name = val
		case "description":
			fm.Description = val
		case "allowed-tools", "allowedTools":
			// D3 技能级工具授权：单行值按空格+逗号切分（"shell, fs_write" /
			// "Bash(git add *)" 等；列表项（- Tool）走 applyList 收集）。
			fm.AllowedTools = splitTools(val)
		}
	}
	flushBlock()
	if currentKey != "" && len(currentList) > 0 {
		applyList(fm, currentKey, currentList)
	}
	return fm, body, nil
}

func applyList(fm *frontmatter, key string, list []string) {
	if key == "modeSlugs" || key == "modes" {
		fm.ModeSlugs = list
	}
	if key == "allowed-tools" || key == "allowedTools" {
		// 列表项形态（- shell \n - fs_write）：逐项收集。
		fm.AllowedTools = append(fm.AllowedTools, list...)
	}
}

// splitTools 把 allowed-tools 单行值切分为工具清单：逗号+空白分隔，
// 但括号模式串（如 Bash(git add *)）内部空格不拆——含未闭合 ( 的 token
// 持续向后聚 until 出现 ) 为止。
func splitTools(val string) []string {
	val = strings.Trim(val, `"'`)
	if strings.TrimSpace(val) == "" {
		return nil
	}
	var out []string
	var cur strings.Builder // 括号聚合中的 token（含内部空格）
	paren := false
	for _, f := range strings.Fields(val) {
		if paren {
			cur.WriteString(" ")
			cur.WriteString(f)
			if strings.Contains(f, ")") {
				paren = false
				out = append(out, strings.TrimSuffix(cur.String(), ","))
			}
			continue
		}
		if strings.Contains(f, "(") && !strings.Contains(f, ")") {
			paren = true
			cur.Reset()
			cur.WriteString(f)
			continue
		}
		if p := strings.Trim(f, ","); p != "" { // 逗号分隔符剥离
			out = append(out, p)
		}
	}
	if paren && cur.Len() > 0 {
		out = append(out, cur.String()) // 未闭合兜底：聚合段整体保留
	}
	return out
}

// validName 校验技能名（对齐 agentskills.io 规范：小写字母数字连字符）。
func validName(name string) bool {
	if name == "" || len(name) > skillNameMaxLen {
		return false
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return false
		}
	}
	return true
}
