// Package tools 设计体系工具：design_system（移植自 open-design-main 的
// design-systems 目录组合语义——选中一个体系后把其设计上下文组合进提示词）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kaelvoris/internal/design"
)

// DesignSystemTool 设计体系浏览/加载工具。
//   - 无 id 参数：列出目录中全部设计体系（名称/类别/一句话简介）。
//   - 带 id 参数：加载该体系完整设计规范（DESIGN.md + tokens.css [+ tailwind-v4.css]）。
type DesignSystemTool struct {
	// Catalog 设计体系目录（nil = 使用 design.Default()）。
	Catalog *design.Catalog
}

func (t *DesignSystemTool) Name() string { return "design_system" }

func (t *DesignSystemTool) Description() string {
	return "Browse or load frontend design systems for website / UI development. " +
		"Without parameters: list all bundled design systems (name, category, one-line summary). " +
		"With `id`: load that system's full design specification — design philosophy (DESIGN.md), " +
		"semantic design tokens (tokens.css) and optional Tailwind v4 mapping. " +
		"Load the system BEFORE writing any frontend page/component code and follow its tokens and component specs strictly."
}

func (t *DesignSystemTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"id": strProp("Design system id to load (see the list returned without parameters). Omit to list all systems."),
	}, nil)
}

func (t *DesignSystemTool) Execute(ctx context.Context, args string) (string, error) {
	cat := t.Catalog
	if cat == nil {
		cat = design.Default()
	}

	// 无参（或空参）= 目录浏览。
	id := ""
	var p struct {
		ID string `json:"id"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		id = strings.TrimSpace(p.ID)
	}
	if id == "" {
		return t.list(cat)
	}
	return t.load(cat, id)
}

// list 目录浏览：按类别分组输出一行一条。
func (t *DesignSystemTool) list(cat *design.Catalog) (string, error) {
	pkgs, err := cat.All()
	if err != nil {
		return "", err
	}
	if len(pkgs) == 0 {
		return "no design systems bundled", nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Design systems catalog: %d packages. Re-call design_system with `id` to load the full spec of one system.\n\n", len(pkgs)))
	curCat := ""
	for _, p := range pkgs {
		if p.Category != curCat {
			curCat = p.Category
			b.WriteString("\n## " + curCat + "\n")
		}
		tw := ""
		if p.HasTailwind {
			tw = " +tailwind"
		}
		b.WriteString(fmt.Sprintf("- %s (id: `%s`)%s — %s\n", p.Name, p.ID, tw, p.Description))
	}
	return b.String(), nil
}

// load 加载单体系完整规范。
func (t *DesignSystemTool) load(cat *design.Catalog, id string) (string, error) {
	designMD, tokens, tailwind, err := cat.Load(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Design System: " + id + "\n\n")
	b.WriteString("## Design Specification (DESIGN.md)\n\n")
	b.WriteString(strings.TrimSpace(designMD) + "\n\n")
	b.WriteString("## Semantic Design Tokens (tokens.css)\n\n")
	b.WriteString("Apply these CSS custom properties as the single source of truth for colors, typography, spacing, radius, shadows and motion:\n\n")
	b.WriteString("```css\n" + strings.TrimSpace(tokens) + "\n```\n")
	if strings.TrimSpace(tailwind) != "" {
		b.WriteString("\n## Tailwind v4 Mapping (tailwind-v4.css)\n\n")
		b.WriteString("Use when the target project uses Tailwind CSS v4 (`@theme` tokens):\n\n")
		b.WriteString("```css\n" + strings.TrimSpace(tailwind) + "\n```\n")
	}
	return b.String(), nil
}
