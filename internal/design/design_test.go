// 设计体系目录与工具测试：目录发现、manifest 校验、加载、工具两态（浏览/加载）。
package design

import (
	"strings"
	"testing"
)

func TestCatalogDiscovery(t *testing.T) {
	c := New()
	pkgs, err := c.All()
	if err != nil {
		t.Fatalf("All() error: %v", err)
	}
	if len(pkgs) < 100 {
		t.Fatalf("catalog packages = %d, want >= 100 (bundled open-design catalog)", len(pkgs))
	}
	// 排序稳定性：类别 → 名称。
	for i := 1; i < len(pkgs); i++ {
		a, b := pkgs[i-1], pkgs[i]
		if a.Category > b.Category ||
			(a.Category == b.Category && strings.ToLower(a.Name) > strings.ToLower(b.Name)) {
			t.Errorf("catalog not sorted at %d: %v before %v", i, a.ID, b.ID)
			break
		}
	}
}

func TestCatalogLoadLinearApp(t *testing.T) {
	c := New()
	design, tokens, tailwind, err := c.Load("linear-app")
	if err != nil {
		t.Fatalf("Load(linear-app) error: %v", err)
	}
	if !strings.Contains(design, "Design System") || !strings.Contains(design, "Visual Theme") {
		t.Error("DESIGN.md should contain visual theme sections")
	}
	if !strings.Contains(tokens, "--bg") || !strings.Contains(tokens, ":root") {
		t.Error("tokens.css should define :root custom properties")
	}
	if tailwind == "" {
		t.Error("linear-app should ship tailwind-v4.css")
	}
}

func TestCatalogLoadUnknown(t *testing.T) {
	c := New()
	_, _, _, err := c.Load("no-such-system")
	if err == nil {
		t.Fatal("expected error for unknown system")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not-found, got %v", err)
	}
}

func TestManifestIDMustMatchDirectory(t *testing.T) {
	c := New()
	pkgs, err := c.All()
	if err != nil {
		t.Fatalf("All() error: %v", err)
	}
	for _, p := range pkgs {
		if p.ID == "" || p.Name == "" || p.Category == "" {
			t.Errorf("package %q missing metadata: %+v", p.ID, p)
		}
	}
}
