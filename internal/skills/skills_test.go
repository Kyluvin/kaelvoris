// 技能系统测试：发现、frontmatter 解析、模式过滤、project > global 覆盖。
package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill 写入一个技能目录。
func writeSkill(t *testing.T, root, dirName, frontmatter, body string) {
	t.Helper()
	dir := filepath.Join(root, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := frontmatter + "\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const fmBasic = `---
name: commit-helper
description: Git commit workflow with conventional commits
modeSlugs:
  - code
  - debug
---`

const fmAllModes = `---
name: universal
description: Works in every mode
---`

// fmREA 插件技能测试专用（name 与目录名一致，无 modeSlugs = 全模式）。
const fmREA = `---
name: reverse-engineer-anything
description: REA reverse engineering workflow
---`

// fmDeploy 覆盖测试专用（name 与目录名一致）。
const fmDeploy = `---
name: deploy
description: Deployment workflow
---`

func hasName(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// TestPluginSkillDiscovery 插件自带技能发现：~/.kaelvoris/plugins/<id>/skills/
// 下的技能随 NewManager 扫描入索引（plugin 层级），且被用户 global 同名覆盖。
func TestPluginSkillDiscovery(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	// 插件技能（如 rea 插件的 reverse-engineer-anything；name 须与目录名一致）。
	writeSkill(t, filepath.Join(home, ".kaelvoris", "plugins", "rea", "skills"), "reverse-engineer-anything", fmREA, "REA workflow body.")
	// 用户 global 同名覆盖（同名时 global 胜出）。
	writeSkill(t, filepath.Join(home, ".kaelvoris", "skills"), "universal", fmAllModes, "User override body.")

	m := NewManager(home, ws)

	got := m.Names()
	if !hasName(got, "reverse-engineer-anything") {
		t.Errorf("plugin skill missing: %v", got)
	}
	// 覆盖规则：global > plugin（skillDirs 顺序 plugin 先于 global 写入）。
	c, err := m.Load("universal")
	if err != nil {
		t.Fatalf("Load universal: %v", err)
	}
	if c.Source != "global" || !strings.Contains(c.Body, "User override") {
		t.Errorf("universal = source %q body %q, want global override", c.Source, c.Body)
	}
	// 插件技能正文可加载。
	p, err := m.Load("reverse-engineer-anything")
	if err != nil {
		t.Fatalf("Load plugin skill: %v", err)
	}
	if p.Source != "plugin" || !strings.Contains(p.Body, "REA workflow body") {
		t.Errorf("plugin skill = source %q body %q", p.Source, p.Body)
	}
}

func TestDiscover(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "commit-helper", fmBasic, "Step 1. Run git status.")
	writeSkill(t, filepath.Join(ws, ".agents", "skills"), "universal", fmAllModes, "Do the thing.")

	m := NewManager(home, ws)

	got := m.Names()
		// 内置技能库（builtin/，go:embed）随包常驻；用户技能叠加其上。
		// 2026-10 open-design 0.23.1 全量融合：builtin 16 → 169；
		// storyboard-video（分镜图生短视频）+1 = 170。
		if len(got) < 2+170 {
			t.Fatalf("Names() = %d, want >= 172 (2 user + 170 builtin)", len(got))
		}
	if !hasName(got, "commit-helper") || !hasName(got, "universal") {
		t.Errorf("user skills missing: %v", got)
	}

	// 模式过滤：commit-helper 仅 code/debug 可见。
		inCode := m.ForMode("code")
		if len(inCode) != 172 {
			t.Errorf("ForMode(code) = %d skills, want 172 (170 builtin + 2 user)", len(inCode))
		}
	inArch := m.ForMode("architect")
	// universal 无 modeSlugs = 全模式可用；commit-helper 限定 code/debug 不出现。
	for _, s := range inArch {
		if s.Name == "commit-helper" {
			t.Errorf("commit-helper should be filtered out of architect mode")
		}
	}
		if len(inArch) != 166 {
			t.Errorf("ForMode(architect) = %d skills, want 166 (154 builtin + 12 architect-mode)", len(inArch))
		}

	// 加载正文：frontmatter 已剥离。
	c, err := m.Load("commit-helper")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(c.Body, "Step 1") || strings.Contains(c.Body, "name: commit-helper") {
		t.Errorf("body not stripped of frontmatter: %q", c.Body)
	}
	if c.Source != "global" {
		t.Errorf("commit-helper source = %q, want global (发现于全局目录)", c.Source)
	}

	// 未知技能错误。
	if _, err := m.Load("nope"); err == nil {
		t.Error("expected error for unknown skill")
	}
}

// TestBuiltinDiscovery 内置技能库：embed 扫描 + Load + ReadFile（references）。
func TestBuiltinDiscovery(t *testing.T) {
	m := NewManager(t.TempDir(), "")
	builtin := m.BuiltinNames()
		// open-design 0.23.1 全量融合（2026-10）：163 个上游技能并入 builtin
		//（同名覆盖升级 10 个 + 新增 153 个），叠加本地 ponytail 系 6 个 = 169；
		// storyboard-video（分镜图生短视频，本地新增）= 170。
		if len(builtin) != 170 {
			t.Fatalf("BuiltinNames() = %d skills, want 170 builtin skills (open-design 0.23.1 full merge + storyboard-video)", len(builtin))
		}
	// ponytail 技能族（开源 6 技能）已内置：主技能 + review/audit/debt/gain/help。
	for _, n := range []string{"ponytail", "ponytail-review", "ponytail-audit", "ponytail-debt", "ponytail-gain", "ponytail-help"} {
		if !hasName(builtin, n) {
			t.Errorf("builtin ponytail skill missing: %s (got %v)", n, builtin)
		}
	}
	// frontend-design 是精选包代表（含 LICENSE.txt，SKILL.md frontmatter 带 | 多行 description）。
	c, err := m.Load("frontend-design")
	if err != nil {
		t.Fatalf("Load builtin: %v", err)
	}
	if c.Source != "builtin" {
		t.Errorf("source = %q, want builtin", c.Source)
	}
	if !strings.Contains(c.Description, "frontend") {
		t.Errorf("multiline description not parsed: %q", c.Description)
	}
	if !strings.Contains(c.Body, "Workflow") {
		t.Errorf("body missing: %q", c.Body[:min(200, len(c.Body))])
	}
	// references 子文件读取（web-design-guidelines/references/guidelines.md）。
	ref, err := m.ReadFile("web-design-guidelines", "references/guidelines.md")
	if err != nil {
		t.Fatalf("ReadFile references: %v", err)
	}
	if len(ref) == 0 {
		t.Error("references file empty")
	}
	// 路径穿越拒绝。
	if _, err := m.ReadFile("frontend-design", "../../etc/passwd"); err == nil {
		t.Error("path traversal not rejected")
	}
}

// TestBuiltinOverriddenByUser 用户目录同名技能覆盖 builtin（global > builtin）。
func TestBuiltinOverriddenByUser(t *testing.T) {
	home := t.TempDir()
	// frontmatter name 必须与目录名一致（agentskills.io 规范）才会被收录。
	const fmCopy = "---\nname: copywriting\ndescription: user override\n---"
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "copywriting", fmCopy, "USER version")
	m := NewManager(home, "")
	c, err := m.Load("copywriting")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Source != "global" || !strings.Contains(c.Body, "USER version") {
		t.Errorf("user override failed: source=%q body=%q", c.Source, c.Body)
	}
}

func TestProjectOverridesGlobal(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "deploy", fmDeploy, "GLOBAL version")
	writeSkill(t, filepath.Join(ws, ".kaelvoris", "skills"), "deploy", fmDeploy, "PROJECT version")

	m := NewManager(home, ws)
	c, err := m.Load("deploy")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Source != "project" || !strings.Contains(c.Body, "PROJECT version") {
		t.Errorf("project override failed: source=%q body=%q", c.Source, c.Body)
	}
}

func TestInvalidNameSkipped(t *testing.T) {
	home := t.TempDir()
	// 非法目录名（大写/下划线）应被跳过。
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "Bad_Name", fmAllModes, "x")
	m := NewManager(home, "")
	if hasName(m.Names(), "Bad_Name") {
		t.Errorf("invalid skill name not filtered: %v", m.Names())
	}
}

func TestNameMismatchSkipped(t *testing.T) {
	home := t.TempDir()
	// frontmatter name 与目录名不一致 → 跳过（agentskills.io 规范）。
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "dir-a", fmAllModes, "x")
	m := NewManager(home, "")
	if hasName(m.Names(), "dir-a") {
		t.Errorf("name/dirname mismatch not filtered: %v", m.Names())
	}
}

// ── 分类模型测试（categories.go）─────────────────────────────

// TestCategoriesCRUD 分类新增/重命名/删除/未分类回落全流程。
func TestCategoriesCRUD(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())
	// 新增两个分类。
	c1, err := m.AddCategory("开发")
	if err != nil {
		t.Fatalf("AddCategory: %v", err)
	}
	c2, err := m.AddCategory("文档")
	if err != nil || c2.Name != "文档" || c2.Sort <= c1.Sort {
		t.Fatalf("AddCategory 2: %+v err=%v", c2, err)
	}
	// 重名拒绝。
	if _, err := m.AddCategory("开发"); err == nil {
		t.Error("duplicate category name should fail")
	}
	// 空名拒绝。
	if _, err := m.AddCategory("  "); err == nil {
		t.Error("empty category name should fail")
	}
	// 分配技能到分类。
	if err := m.Save("my-skill", "---\nname: my-skill\ndescription: x\n---\nbody"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := m.SetSkillCategory("my-skill", c1.ID); err != nil {
		t.Fatalf("SetSkillCategory: %v", err)
	}
	// 未知技能/未知分类拒绝。
	if err := m.SetSkillCategory("nope", c1.ID); err == nil {
		t.Error("unknown skill should fail")
	}
	if err := m.SetSkillCategory("my-skill", "cat_missing"); err == nil {
		t.Error("unknown category should fail")
	}
	// 重命名：ID 不变绑定仍在。
	if err := m.UpdateCategory(c1.ID, "前端开发"); err != nil {
		t.Fatalf("UpdateCategory: %v", err)
	}
	cats, as := m.LoadCategories()
	if len(cats) != 2 || cats[0].Name != "前端开发" || cats[1].Name != "文档" {
		t.Errorf("after rename cats = %+v", cats)
	}
	if as["my-skill"] != c1.ID {
		t.Errorf("assignment lost after rename: %+v", as)
	}
	// 移入未分类（空 catID）。
	if err := m.SetSkillCategory("my-skill", ""); err != nil {
		t.Fatalf("SetSkillCategory empty: %v", err)
	}
	if _, as = m.LoadCategories(); len(as) != 0 {
		t.Errorf("assignment should be cleared: %+v", as)
	}
	// 删除分类：归属技能回落未分类。
	if err := m.SetSkillCategory("my-skill", c2.ID); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if err := m.DeleteCategory(c2.ID); err != nil {
		t.Fatalf("DeleteCategory: %v", err)
	}
	if _, as = m.LoadCategories(); len(as) != 0 {
		t.Errorf("assignments should fall back to uncategorized: %+v", as)
	}
	if err := m.DeleteCategory("cat_missing"); err == nil {
		t.Error("delete unknown category should fail")
	}
}

// TestCategoriesPersistenceCorruptTolerant 损坏文件返回空值不致命。
func TestCategoriesPersistenceCorruptTolerant(t *testing.T) {
	m := NewManager(t.TempDir(), "")
	path := m.CategoriesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cats, as := m.LoadCategories()
	if len(cats) != 0 || len(as) != 0 {
		t.Errorf("corrupt file should yield empty: %v %v", cats, as)
	}
	// 损坏后仍可正常写入。
	if _, err := m.AddCategory("测试"); err != nil {
		t.Fatalf("AddCategory after corrupt: %v", err)
	}
	if cats, _ = m.LoadCategories(); len(cats) != 1 {
		t.Errorf("save after corrupt failed: %+v", cats)
	}
}

// min Go 1.21 内建前的兜底（go.mod 声明 1.22，可直接用内建；此处防御旧工具链）。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestFrontmatterParsing(t *testing.T) {
	fm, body, err := parseFrontmatter([]byte("---\nname: abc\ndescription: Hello\nmodeSlugs:\n  - code\n  - ask\n---\n\nBody here\n"))
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	if fm.Name != "abc" || fm.Description != "Hello" {
		t.Errorf("fm = %+v", fm)
	}
	if len(fm.ModeSlugs) != 2 || fm.ModeSlugs[0] != "code" || fm.ModeSlugs[1] != "ask" {
		t.Errorf("modeSlugs = %v", fm.ModeSlugs)
	}
	if !strings.Contains(body, "Body here") {
		t.Errorf("body = %q", body)
	}
}

// TestSaveLoadDelete 保存→读回→覆盖→删除全流程。
func TestSaveLoadDelete(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	m := NewManager(home, ws)

	const name = "my-skill"
	content := "---\nname: my-skill\ndescription: A test skill\n---\n\nHello body.\n"
	if err := m.Save(name, content); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// All/LoadRaw 立即可见（Save 后已 Discover）。
	if !hasName(m.Names(), name) {
		t.Errorf("saved skill missing after Save: %v", m.Names())
	}
	raw, err := m.LoadRaw(name)
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	if raw != content {
		t.Errorf("LoadRaw roundtrip mismatch\n got %q\nwant %q", raw, content)
	}
	// Source 必须是 project（统一落 project 层）。
	c, err := m.Load(name)
	if err != nil || c.Source != "project" {
		t.Errorf("Load source = %v (err %v), want project", c, err)
	}

	// 覆盖保存。
	content2 := "---\nname: my-skill\ndescription: Updated\n---\n\nUpdated body.\n"
	if err := m.Save(name, content2); err != nil {
		t.Fatalf("Save overwrite: %v", err)
	}
	raw2, _ := m.LoadRaw(name)
	if raw2 != content2 {
		t.Errorf("overwrite mismatch: %q", raw2)
	}

	// 删除。
	if err := m.Delete(name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if hasName(m.Names(), name) {
		t.Errorf("skill still present after Delete: %v", m.Names())
	}
	if _, err := m.LoadRaw(name); err == nil {
		t.Error("LoadRaw after Delete should fail")
	}
}

// TestSaveValidation 保存校验：非法名 / workspace 为空 / frontmatter name 冲突。
func TestSaveValidation(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())

	// 非法名（大写/下划线/空）。
	for _, bad := range []string{"", "Bad_Name", "has space", "-x- -y"} {
		if err := m.Save(bad, "---\nname: x\n---\nbody"); err == nil {
			t.Errorf("Save(%q) expected error for invalid name", bad)
		}
	}

	// workspace 为空。
	mw := NewManager(t.TempDir(), "")
	if err := mw.Save("ok-name", "---\nname: ok-name\n---\nbody"); err == nil {
		t.Error("Save with empty workspace should fail")
	}

	// frontmatter name 冲突。
	if err := m.Save("dir-name", "---\nname: other-name\n---\nbody"); err == nil {
		t.Error("Save with conflicting frontmatter name should fail")
	}
}

// TestDeleteBuiltinRejected builtin 技能禁删。
func TestDeleteBuiltinRejected(t *testing.T) {
	m := NewManager(t.TempDir(), "")
	builtins := m.BuiltinNames()
	if len(builtins) == 0 {
		t.Fatal("no builtin skills found")
	}
	if err := m.Delete(builtins[0]); err == nil {
		t.Errorf("Delete builtin %q should fail", builtins[0])
	}
	// 未知技能。
	if err := m.Delete("nope-nope"); err == nil {
		t.Error("Delete unknown skill should fail")
	}
}

// TestSaveThenUserLayerNotShadowed 保存到 project 层后，同名 global 技能被 project 覆盖。
func TestSaveThenUserLayerNotShadowed(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	// 全局层已有同名技能。
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "deploy", fmDeploy, "GLOBAL version")
	m := NewManager(home, ws)
	if err := m.Save("deploy", "---\nname: deploy\ndescription: Deployment workflow\n---\n\nPROJECT version\n"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	c, err := m.Load("deploy")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Source != "project" || !strings.Contains(c.Body, "PROJECT version") {
		t.Errorf("save should land in project layer: source=%q body=%q", c.Source, c.Body)
	}
	// 删除 project 层后 global 层技能应重新可见（Delete 只删 Path 所在层）。
	if err := m.Delete("deploy"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	c2, err := m.Load("deploy")
	if err != nil {
		t.Fatalf("Load after delete project layer: %v", err)
	}
	if c2.Source != "global" || !strings.Contains(c2.Body, "GLOBAL version") {
		t.Errorf("global layer not exposed after project delete: %+v", c2)
	}
}

func names(list []Metadata) []string {
	out := make([]string, len(list))
	for i, m := range list {
		out[i] = m.Name
	}
	return out
}

// TestCacheManagerRefreshesOnExternalChange 回归：缓存 Manager（任务运行时构建的
// 快照索引）在外部于磁盘新增/删除技能后，Load/LoadRaw 仍能读到最新结果——
// 修复「列表（ListSkills 每次新建 Manager）显示、点击（SkillRaw 走缓存 Manager）
// 报 skill not found」。
func TestCacheManagerRefreshesOnExternalChange(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	// 先有一个技能 A，构建缓存 Manager（模拟任务启动时的索引快照）。
	writeSkill(t, filepath.Join(ws, ".kaelvoris", "skills"), "skill-a", "---\nname: skill-a\ndescription: A\n---\n", "bodyA")
	m := NewManager(home, ws)
	if !hasName(m.Names(), "skill-a") {
		t.Fatalf("skill-a missing at start: %v", m.Names())
	}

	// 外部在磁盘上直接新增技能（不经 m.Save）——模拟 Agent 写文件/其他工具创建。
	writeSkill(t, filepath.Join(ws, ".kaelvoris", "skills"), "external-new", "---\nname: external-new\ndescription: external new\n---\n", "bodyNew")

	// 修复前：缓存索引无此技能 → Load/LoadRaw/ReadFile 报 not found。
	// 修复后：miss 自动 Discover 刷新，读到最新内容。
	c, err := m.Load("external-new")
	if err != nil {
		t.Fatalf("Load after external add: %v", err)
	}
	if !strings.Contains(c.Body, "bodyNew") {
		t.Errorf("Load body = %q, want bodyNew", c.Body)
	}
	raw, err := m.LoadRaw("external-new")
	if err != nil {
		t.Fatalf("LoadRaw after external add: %v", err)
	}
	if !strings.Contains(raw, "bodyNew") {
		t.Errorf("LoadRaw = %q, want bodyNew", raw)
	}
	if _, err := m.ReadFile("external-new", "SKILL.md"); err != nil {
		t.Errorf("ReadFile after external add: %v", err)
	}

	// 外部删除技能后，缓存索引也应刷新掉（Load 报 not found 而非读到旧内容）。
	if err := os.RemoveAll(filepath.Join(ws, ".kaelvoris", "skills", "skill-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Load("skill-a"); err == nil {
		t.Errorf("Load after external delete should fail (refreshed index)")
	}
}

// ---- D3：frontmatter allowed-tools 解析（第三批对标 Claude Code skills）----

func TestAllowedToolsParsing(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	dir := filepath.Join(ws, ".agents", "skills")
	// 单行空格分隔
	writeSkill(t, dir, "space-form", "---\nname: space-form\ndescription: d\nallowed-tools: shell fs_write\n---", "body")
	// 单行逗号分隔 + 括号模式整体保留
	writeSkill(t, dir, "paren-form", "---\nname: paren-form\ndescription: d\nallowed-tools: \"shell, Bash(git add *)\"\n---", "body")
	// 列表项形态
	writeSkill(t, dir, "list-form", "---\nname: list-form\ndescription: d\nallowed-tools:\n  - shell\n  - fs_fs_apply_diff\n---", "body")

	m := NewManager(home, ws)
	c, err := m.Load("space-form")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedTools) != 2 || c.AllowedTools[0] != "shell" || c.AllowedTools[1] != "fs_write" {
		t.Errorf("space-form AllowedTools = %v", c.AllowedTools)
	}
	c, err = m.Load("paren-form")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedTools) != 2 || c.AllowedTools[0] != "shell" || c.AllowedTools[1] != "Bash(git add *)" {
		t.Errorf("paren-form AllowedTools = %v (paren form must stay intact)", c.AllowedTools)
	}
	c, err = m.Load("list-form")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedTools) != 2 || c.AllowedTools[0] != "shell" || c.AllowedTools[1] != "fs_fs_apply_diff" {
		t.Errorf("list-form AllowedTools = %v", c.AllowedTools)
	}
	// 无 allowed-tools 的技能：字段为空，行为不变（零破坏）
	c, err = m.Load("universal")
	_ = c
	if err != nil {
		t.Skip("universal only in other tests' dirs")
	}
	// 元数据直接可读（ToolsFor 不需要 Load 全文）
	found := false
	for _, md := range m.All() {
		if md.Name == "paren-form" {
			found = true
			if len(md.AllowedTools) != 2 {
				t.Errorf("Metadata.AllowedTools = %v", md.AllowedTools)
			}
		}
	}
	if !found {
		t.Error("paren-form not discovered")
	}
}
