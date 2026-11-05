// Package design 前端设计体系目录：移植自 open-design（github.com/nexu-io/open-design）。
//
// 每个子目录是一个可移植设计体系包（portable design-system package），三件套：
//   - manifest.json   发现元数据（id/name/category/description + 声明文件路径）
//   - DESIGN.md       设计散文（视觉主题/颜色/字体/组件/间距/响应式规范）
//   - tokens.css      语义 token 样式表（--bg/--fg/--accent/--radius/...）
//
// 可选扩展件（open-design 0.23.1 起随包分发，按需经 ReadPackageFile 读取）：
//   - components.html        组件库样例（单文件 HTML）
//   - design-tokens.json     结构化 token 数据
//   - USAGE.md               使用指引
//   - components.manifest.json / preview/ / source/
//
// 另嵌 templates/（open-design design-templates 全量）：每个模板包 SKILL.md
// + assets/（种子 HTML）+ references/（布局库/检查单），由 design_template 工具消费。
//
// 目录经 go:embed 编译进二进制，运行时由 design_system / design_template 工具消费：
//   - design_system 无参调用 = 浏览目录；带 id = 加载 DESIGN.md + tokens.css [+ tailwind]
//   - design_template 无参 = 浏览模板；带 id = 加载 SKILL.md 工作流；带 id+path = 读包内文件
package design

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

//go:embed all:systems
var catalogFS embed.FS

//go:embed all:templates
var templatesFS embed.FS

// constTemplateDir embed 模板根目录（子目录 = 模板包，各含 SKILL.md）。
const constTemplateDir = "templates"

// Package 设计体系包元数据（manifest.json 子集 + 文件清单）。
type Package struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Description string `json:"description"`
	// HasTailwind 是否附带 tailwind-v4.css 映射。
	HasTailwind bool `json:"hasTailwind"`
	// HasComponents / HasUsage 是否附带 components.html / USAGE.md 扩展件
	//（open-design 0.23.1 归一化包；列表展示 +，按需 ReadPackageFile 读取）。
	HasComponents bool `json:"hasComponents"`
	HasUsage      bool `json:"hasUsage"`

	designFile   string
	tokensFile   string
	tailwindFile string
}

// manifest manifest.json schema（仅取所需字段，files 缺省走规范文件名）。
type manifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Files       struct {
		Design   string `json:"design"`
		Tokens   string `json:"tokens"`
		Tailwind string `json:"tailwind"`
	} `json:"files"`
}

// idPattern 目录 slug / manifest.id 规范（README：normalized ASCII 小写连字符）。
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// constSystemDir embed 根目录。
const constSystemDir = "systems"

// Catalog 设计体系目录（懒加载 + 进程内缓存）。
type Catalog struct {
	once sync.Once
	pkgs []Package
	idx  map[string]int
	err  error

	tplOnce sync.Once
	tpls    []Template
	tplErr  error
}

// Default 全局目录单例（进程共享；embed 读取廉价，但缓存避免每任务重扫）。
func Default() *Catalog { return &defaultCatalog }

var defaultCatalog Catalog

// NewCatalog 创建独立目录实例（测试用；生产用 Default）。
func New() *Catalog { return &Catalog{} }

func (c *Catalog) init() {
	c.once.Do(func() {
		entries, err := catalogFS.ReadDir(constSystemDir)
		if err != nil {
			c.err = err
			return
		}
		var pkgs []Package
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			id := e.Name()
			// slug 规范 + 防 embed 目录名意外（.. 等 embed 不会出现，双保险）。
			if !idPattern.MatchString(id) {
				continue
			}
			pkg, err := readPackage(id)
			if err != nil {
				continue // 坏包跳过，不影响目录可用性
			}
			pkgs = append(pkgs, pkg)
		}
		// 排序：类别 → 名称（目录列表输出稳定可读）。
		sort.Slice(pkgs, func(i, j int) bool {
			if pkgs[i].Category != pkgs[j].Category {
				return pkgs[i].Category < pkgs[j].Category
			}
			if pkgs[i].Name != pkgs[j].Name {
				return strings.ToLower(pkgs[i].Name) < strings.ToLower(pkgs[j].Name)
			}
			return pkgs[i].ID < pkgs[j].ID
		})
		idx := make(map[string]int, len(pkgs))
		for i, p := range pkgs {
			idx[pkgSlug(p, i)] = i
		}
		c.pkgs = pkgs
		c.idx = idx
	})
}

// pkgSlug 目录主键 = 包 id（Package 未导出 slug 字段，用 name+idx 索引）。
// 简化：idx 直接按 pkg.ID 建——readPackage 已保证 m.ID == 目录名。
func pkgSlug(p Package, _ int) string { return p.ID }

// All 返回全部设计体系包（按类别、名称排序）。
func (c *Catalog) All() ([]Package, error) {
	c.init()
	return c.pkgs, c.err
}

// Load 加载指定设计体系。返回 DESIGN.md 全文、tokens.css 全文与
// tailwind-v4.css 全文（无 tailwind 文件时为空串）。
func (c *Catalog) Load(id string) (design, tokens, tailwind string, err error) {
	c.init()
	if c.err != nil {
		return "", "", "", c.err
	}
	i, ok := c.idx[id]
	if !ok {
		return "", "", "", fmt.Errorf("design system %q not found; call design_system without id to list available systems", id)
	}
	pkg := c.pkgs[i]
	base := constSystemDir + "/" + pkg.ID
	if design, err = readEmbedFile(base, pkg.designFile); err != nil {
		return "", "", "", fmt.Errorf("load design spec: %w", err)
	}
	if tokens, err = readEmbedFile(base, pkg.tokensFile); err != nil {
		return "", "", "", fmt.Errorf("load tokens: %w", err)
	}
	if pkg.tailwindFile != "" {
		tailwind, err = readEmbedFile(base, pkg.tailwindFile)
		if err != nil {
			return "", "", "", fmt.Errorf("load tailwind mapping: %w", err)
		}
	}
	return design, tokens, tailwind, nil
}

// readEmbedFile 读包内声明文件（相对包目录；文件名来自 manifest，非用户输入）。
func readEmbedFile(base, name string) (string, error) {
	if name == "" {
		name = ""
	}
	p := base + "/" + strings.TrimPrefix(name, "/")
	b, err := catalogFS.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readPackage 解析单个包：manifest.json 元数据 + 规范文件名缺省回退。
func readPackage(id string) (Package, error) {
	base := constSystemDir + "/" + id
	b, err := catalogFS.ReadFile(base + "/manifest.json")
	if err != nil {
		return Package{}, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Package{}, err
	}
	// README 规则：目录 slug 与 manifest.id 必须一致。
	if m.ID == "" {
		m.ID = id
	}
	if m.ID != id {
		return Package{}, fmt.Errorf("manifest id %q != directory %q", m.ID, id)
	}
	pkg := Package{
		ID:          m.ID,
		Name:        m.Name,
		Category:    m.Category,
		Description: m.Description,
	}
	// files.* 缺省走规范文件名（README：v1 固定 DESIGN.md / tokens.css / tailwind-v4.css）。
	pkg.designFile = defaultStr(m.Files.Design, "DESIGN.md")
	pkg.tokensFile = defaultStr(m.Files.Tokens, "tokens.css")
	tw := m.Files.Tailwind
	if tw == "" {
		tw = "tailwind-v4.css"
	}
	if _, err := fs.Stat(catalogFS, base+"/"+tw); err == nil {
		pkg.tailwindFile = tw
		pkg.HasTailwind = true
	}
	// 扩展件探测（仅标记存在；内容按需读，避免每次 Load 全量输出撑爆上下文）。
	if _, err := fs.Stat(catalogFS, base+"/components.html"); err == nil {
		pkg.HasComponents = true
	}
	if _, err := fs.Stat(catalogFS, base+"/USAGE.md"); err == nil {
		pkg.HasUsage = true
	}
	return pkg, nil
}

func defaultStr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// ReadPackageFile 读取指定设计体系包内任意文件（相对包根，如
// "components.html" / "design-tokens.json" / "USAGE.md" / "preview/colors.html"）。
// 安全：拒绝绝对路径与 .. 穿越；仅在包目录内解析。
func (c *Catalog) ReadPackageFile(id, rel string) (string, error) {
	c.init()
	if c.err != nil {
		return "", c.err
	}
	if _, ok := c.idx[id]; !ok {
		return "", fmt.Errorf("design system %q not found", id)
	}
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
		return "", fmt.Errorf("invalid package path %q", rel)
	}
	b, err := catalogFS.ReadFile(constSystemDir + "/" + id + "/" + rel)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ─── 设计模板（design-templates 全量，open-design 0.23.1）──────────────────

// Template 设计模板元数据（SKILL.md frontmatter 解析结果 + 文件清单标记）。
type Template struct {
	ID          string   `json:"id"`                 // 目录名
	Name        string   `json:"name"`               // frontmatter name（缺省 = id）
	Description string   `json:"description"`        // 一句话用途
	Triggers    []string `json:"triggers,omitempty"` // 触发词
	Mode        string   `json:"mode,omitempty"`     // od.mode（prototype/deck/live-artifact/...）
	// HasSeed / HasRefs 是否附带 assets/ 种子与 references/ 参考库。
	HasSeed bool `json:"hasSeed"`
	HasRefs bool `json:"hasRefs"`
}

// parseTplFrontmatter 解析模板 SKILL.md 的 `---` YAML frontmatter（最小解析，
// 不引第三方依赖；与 internal/skills.parseFrontmatter 同族但字段集不同：
// 模板有 triggers 列表与 od.mode 嵌套键）。
func parseTplFrontmatter(b []byte) (*tplFrontmatter, error) {
	s := string(b)
	if !strings.HasPrefix(s, "---") {
		return &tplFrontmatter{}, nil
	}
	rest := s[3:]
	rest = strings.TrimLeft(rest, "-")
	nl := strings.Index(rest, "\n")
	if nl < 0 {
		return nil, fmt.Errorf("malformed frontmatter")
	}
	end := strings.Index(rest[nl:], "\n---")
	if end < 0 {
		return nil, fmt.Errorf("frontmatter not closed")
	}
	raw := rest[nl:end]
	fm := &tplFrontmatter{}
	var currentKey string
	var listBuf []string
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
	flushList := func() {
		if currentKey == "triggers" && len(listBuf) > 0 {
			fm.Triggers = listBuf
		}
		listBuf = nil
		if currentKey != "" {
			currentKey = ""
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if blockKey != "" {
			// 块标量收集：缩进非空行继续；非缩进行终止。
			if line != "" && (line[0] == ' ' || line[0] == '\t') {
				blockLines = append(blockLines, trimmed)
				continue
			}
			flushBlock()
		}
		if strings.HasPrefix(trimmed, "- ") && currentKey != "" {
			listBuf = append(listBuf, strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")), `"'`))
			continue
		}
		if currentKey != "" || len(listBuf) > 0 {
			flushList()
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if strings.Contains(key, " ") || strings.Contains(key, "\t") {
			continue // 嵌套键（如 od.mode 内的 preview.type）跳过——非列表值也非顶值
		}
		val := strings.TrimSpace(line[idx+1:])
		if val == "|" || val == "|-" || val == "|+" || val == ">" || val == ">-" || val == ">+" {
			blockKey = key
			blockLines = nil
			currentKey = ""
			continue
		}
		currentKey = key
		if val == "" {
			continue // 列表字段，等后续 - item 行
		}
		val = strings.Trim(val, `"'`)
		switch key {
		case "name":
			fm.Name = val
			currentKey = ""
		case "description":
			fm.Description = val
			currentKey = ""
		}
	}
	flushList()
	flushBlock()
	return fm, nil
}

// tplFrontmatter 模板 SKILL.md frontmatter 所需字段子集。
type tplFrontmatter struct {
	Name        string
	Description string
	Triggers    []string
	Mode        string
}

// Templates 扫描全部设计模板（一级子目录含 SKILL.md 才收录；按 ID 排序）。
func (c *Catalog) Templates() ([]Template, error) {
	c.tplOnce.Do(func() {
		entries, err := templatesFS.ReadDir(constTemplateDir)
		if err != nil {
			c.tplErr = err
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			id := e.Name()
			if !idPattern.MatchString(id) {
				continue
			}
			b, err := templatesFS.ReadFile(constTemplateDir + "/" + id + "/SKILL.md")
			if err != nil {
				continue // 无 SKILL.md 的数据目录跳过
			}
			fm, err := parseTplFrontmatter(b)
			if err != nil {
				continue
			}
			name := fm.Name
			if name == "" {
				name = id
			}
			// od.mode 从 frontmatter 嵌套块提取（"  mode: prototype" 缩进行）。
			mode := tplODMode(b)
			tpl := Template{
				ID:          id,
				Name:        name,
				Description: fm.Description,
				Triggers:    fm.Triggers,
				Mode:        mode,
			}
			base := constTemplateDir + "/" + id
			if ents, err := templatesFS.ReadDir(base + "/assets"); err == nil && len(ents) > 0 {
				tpl.HasSeed = true
			}
			if ents, err := templatesFS.ReadDir(base + "/references"); err == nil && len(ents) > 0 {
				tpl.HasRefs = true
			}
			c.tpls = append(c.tpls, tpl)
		}
		sort.Slice(c.tpls, func(i, j int) bool { return c.tpls[i].ID < c.tpls[j].ID })
	})
	return c.tpls, c.tplErr
}

// tplODModePattern od.mode 提取：frontmatter 内 "od:" 块下的 "  mode: xxx"。
var tplODModePattern = regexp.MustCompile(`(?m)^od:\s*$[\s\S]*?^\s{2}mode:\s*(\S+)`)

// tplODMode 从 SKILL.md 原文提取 od.mode（无则空串）。
func tplODMode(b []byte) string {
	if m := tplODModePattern.FindSubmatch(b); m != nil {
		return strings.Trim(string(m[1]), `"'`)
	}
	return ""
}

// ReadTemplateFile 读取指定模板包内任意文件（相对模板根，如
// "SKILL.md" / "assets/template.html" / "references/layouts.md"）。
// 安全：拒绝绝对路径与 .. 穿越。
func (c *Catalog) ReadTemplateFile(id, rel string) (string, error) {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
		return "", fmt.Errorf("invalid template path %q", rel)
	}
	b, err := templatesFS.ReadFile(constTemplateDir + "/" + id + "/" + rel)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
