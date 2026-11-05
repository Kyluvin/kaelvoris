// design_template 工具：open-design design-templates 全量模板浏览/加载
// （2026-10 全量融合自 github.com/nexu-io/open-design v0.23.1）。
// 三态：无参 = 浏览模板目录；带 id = 加载 SKILL.md 工作流；带 id+path = 读包内文件。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kaelvoris/internal/design"
)

// DesignTemplateTool 设计模板浏览/加载工具。
type DesignTemplateTool struct {
	// Catalog 设计目录（nil = 使用 design.Default()）。
	Catalog *design.Catalog
}

func (t *DesignTemplateTool) Name() string { return "design_template" }

func (t *DesignTemplateTool) Description() string {
	return "Browse or load frontend design TEMPLATES (seed + workflow recipes, e.g. web prototypes, decks/presentations, live dashboards, wireframes). " +
		"Without parameters: list all bundled templates (name, mode, one-line description). " +
		"With `id`: load that template's SKILL.md workflow — read its seed (assets/template.html) and references/ BEFORE writing code. " +
		"With `id` + `path`: read one file inside the template package (e.g. `assets/template.html`, `references/layouts.md`). " +
		"Combine with design_system: load the active design system tokens, then reuse this template's seed instead of writing CSS from scratch."
}

func (t *DesignTemplateTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"id":   strProp("Template id to load (see the list returned without parameters). Omit to list all templates."),
		"path": strProp("File inside the template package to read (relative to template root, e.g. `assets/template.html`, `references/layouts.md`). Requires `id`. Omit to load the SKILL.md workflow."),
	}, nil)
}

func (t *DesignTemplateTool) Execute(ctx context.Context, args string) (string, error) {
	cat := t.Catalog
	if cat == nil {
		cat = design.Default()
	}
	var p struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	id := strings.TrimSpace(p.ID)
	path := strings.TrimSpace(p.Path)
	if id == "" {
		return t.list(cat)
	}
	if path != "" {
		return t.readFile(cat, id, path)
	}
	return t.load(cat, id)
}

// list 模板目录浏览：按 mode 分组输出一行一条。
func (t *DesignTemplateTool) list(cat *design.Catalog) (string, error) {
	tpls, err := cat.Templates()
	if err != nil {
		return "", err
	}
	if len(tpls) == 0 {
		return "no design templates bundled", nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Design templates catalog: %d packages. Re-call design_template with `id` to load the workflow (SKILL.md) of one template.\n\n", len(tpls)))
	cur := ""
	for _, p := range tpls {
		if p.Mode != cur {
			cur = p.Mode
			if cur == "" {
				cur = "general"
			}
			b.WriteString("\n## " + cur + "\n")
		}
		marks := ""
		if p.HasSeed {
			marks += " +seed"
		}
		if p.HasRefs {
			marks += " +refs"
		}
		if len(p.Triggers) > 0 {
			marks += " triggers:" + strings.Join(p.Triggers, "/")
		}
		b.WriteString(fmt.Sprintf("- %s (id: `%s`)%s — %s\n", p.Name, p.ID, marks, p.Description))
	}
	return b.String(), nil
}

// load 加载单模板工作流（SKILL.md 正文，frontmatter 已剥离）。
func (t *DesignTemplateTool) load(cat *design.Catalog, id string) (string, error) {
	tpls, err := cat.Templates()
	if err != nil {
		return "", err
	}
	found := false
	for _, p := range tpls {
		if p.ID == id {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("design template %q not found; call design_template without id to list available templates", id)
	}
	raw, err := cat.ReadTemplateFile(id, "SKILL.md")
	if err != nil {
		return "", fmt.Errorf("load template workflow: %w", err)
	}
	body := stripTplFrontmatter(raw)
	var b strings.Builder
	b.WriteString("# Design Template: " + id + "\n\n")
	b.WriteString("Workflow (SKILL.md). Follow it strictly; read the seed and references it lists BEFORE writing any code:\n\n")
	b.WriteString(strings.TrimSpace(body) + "\n")
	// 文件清单（帮助模型下一步按需 ReadPackageFile，不必猜路径）。
	if seed, err := cat.ReadTemplateFile(id, "assets/template.html"); err == nil && seed != "" {
		b.WriteString("\n(Bundled files: `assets/template.html` seed, references/ — read via design_template `id` + `path`.)\n")
	}
	return b.String(), nil
}

// readFile 读模板包内文件（SKILL.md 之外的种子/参考库）。
func (t *DesignTemplateTool) readFile(cat *design.Catalog, id, rel string) (string, error) {
	if _, err := cat.ReadTemplateFile(id, "SKILL.md"); err != nil {
		return "", fmt.Errorf("design template %q not found", id)
	}
	return cat.ReadTemplateFile(id, pathClean(rel))
}

// pathClean 规整相对路径（去 ./ 前缀；穿越校验由 ReadTemplateFile 兜底）。
func pathClean(p string) string {
	return strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
}

// stripTplFrontmatter 剥离 `---` frontmatter（与 skills.Load 同语义，工具输出不带 frontmatter）。
func stripTplFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---") {
		return s
	}
	rest := s[3:]
	nl := strings.Index(rest, "\n")
	if nl < 0 {
		return s
	}
	end := strings.Index(rest[nl:], "\n---")
	if end < 0 {
		return s
	}
	body := rest[nl+end+4:]
	if idx := strings.Index(body, "\n"); idx >= 0 {
		return body[idx+1:]
	}
	return ""
}
