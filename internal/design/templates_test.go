// open-design 0.23.1 全量融合测试：模板目录扫描、模板 frontmatter 解析、
// 包内文件读取（ReadPackageFile / ReadTemplateFile）与路径穿越防护。
package design

import (
	"strings"
	"testing"
)

func TestTemplatesDiscovery(t *testing.T) {
	c := New()
	tpls, err := c.Templates()
	if err != nil {
		t.Fatalf("Templates() error: %v", err)
	}
	// open-design 0.23.1 design-templates 全量（>100 模板包）。
	if len(tpls) < 100 {
		t.Fatalf("templates = %d, want >= 100 (bundled open-design design-templates)", len(tpls))
	}
	// 排序稳定性（ID 升序）。
	for i := 1; i < len(tpls); i++ {
		if tpls[i-1].ID >= tpls[i].ID {
			t.Fatalf("templates not sorted at %d", i)
		}
	}
	// web-prototype 是通用桌面原型模板（含种子与参考库）。
	var webProto *Template
	for i := range tpls {
		if tpls[i].ID == "web-prototype" {
			webProto = &tpls[i]
			break
		}
	}
	if webProto == nil {
		t.Fatal("template web-prototype missing")
	}
	if !webProto.HasSeed || !webProto.HasRefs {
		t.Errorf("web-prototype should ship assets/ seed and references/ (seed=%v refs=%v)", webProto.HasSeed, webProto.HasRefs)
	}
}

func TestTemplateFrontmatterParsing(t *testing.T) {
	// web-prototype：多行块标量 description + triggers 列表。
	c := New()
	tpls, err := c.Templates()
	if err != nil {
		t.Fatalf("Templates(): %v", err)
	}
	var wp *Template
	for i := range tpls {
		if tpls[i].ID == "web-prototype" {
			wp = &tpls[i]
			break
		}
	}
	if wp == nil {
		t.Fatal("web-prototype missing")
	}
	if wp.Name != "web-prototype" {
		t.Errorf("name = %q, want web-prototype", wp.Name)
	}
	if wp.Description == "" || len(wp.Description) < 40 {
		t.Errorf("block-scalar description not parsed: %q", wp.Description)
	}
	if len(wp.Triggers) == 0 {
		t.Error("triggers list not parsed")
	}
	// od.mode 提取（web-prototype 是 prototype 模式）。
	if wp.Mode != "prototype" {
		t.Errorf("od.mode = %q, want prototype", wp.Mode)
	}
}

func TestReadTemplateFile(t *testing.T) {
	c := New()
	// SKILL.md 与种子文件可读。
	skill, err := c.ReadTemplateFile("web-prototype", "SKILL.md")
	if err != nil {
		t.Fatalf("ReadTemplateFile(SKILL.md): %v", err)
	}
	if !strings.Contains(skill, "Web Prototype Skill") {
		t.Errorf("SKILL.md body unexpected: %.80s", skill)
	}
	seed, err := c.ReadTemplateFile("web-prototype", "assets/template.html")
	if err != nil {
		t.Fatalf("ReadTemplateFile(assets/template.html): %v", err)
	}
	if !strings.Contains(seed, "<style") && !strings.Contains(seed, ":root") {
		t.Error("seed html should contain style/root tokens")
	}
	// 路径穿越拒绝。
	if _, err := c.ReadTemplateFile("web-prototype", "../systems/linear-app/tokens.css"); err == nil {
		t.Error("expected error for .. traversal")
	}
	if _, err := c.ReadTemplateFile("web-prototype", "/abs/path"); err == nil {
		t.Error("expected error for absolute path")
	}
}

func TestReadPackageFileExtendedAssets(t *testing.T) {
	c := New()
	// linear-app 0.23.1 归一化包：components.html / USAGE.md 扩展件。
	pkgs, err := c.All()
	if err != nil {
		t.Fatalf("All(): %v", err)
	}
	var linear *Package
	for i := range pkgs {
		if pkgs[i].ID == "linear-app" {
			linear = &pkgs[i]
			break
		}
	}
	if linear == nil {
		t.Fatal("linear-app missing from catalog")
	}
	if !linear.HasComponents || !linear.HasUsage {
		t.Errorf("linear-app extended assets flags: components=%v usage=%v", linear.HasComponents, linear.HasUsage)
	}
	comp, err := c.ReadPackageFile("linear-app", "components.html")
	if err != nil {
		t.Fatalf("ReadPackageFile(components.html): %v", err)
	}
	if !strings.Contains(comp, "<") {
		t.Error("components.html should be html")
	}
	// 目录遍历：确认含扩展件的包占多数（全量融合证据）。
	withComp := 0
	for _, p := range pkgs {
		if p.HasComponents {
			withComp++
		}
	}
	if withComp < 50 {
		t.Errorf("packages with components.html = %d, want >= 50 (open-design normalized bundles)", withComp)
	}
	// 未知包与穿越拒绝。
	if _, err := c.ReadPackageFile("no-such", "x"); err == nil {
		t.Error("expected error for unknown package")
	}
	if _, err := c.ReadPackageFile("linear-app", "../../app.go"); err == nil {
		t.Error("expected error for traversal")
	}
}
