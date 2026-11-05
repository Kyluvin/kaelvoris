// design_template 工具测试：目录浏览、工作流加载、包内文件读取、未知 id 报错。
package tools

import (
	"context"
	"strings"
	"testing"
)

func TestDesignTemplateToolList(t *testing.T) {
	tool := &DesignTemplateTool{}
	res, err := tool.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	if !strings.Contains(res, "Design templates catalog:") {
		t.Errorf("list output should contain catalog header, got:\n%.200s", res)
	}
	// open-design design-templates 代表模板。
	for _, want := range []string{"web-prototype", "html-ppt-pitch-deck", "live-dashboard"} {
		if !strings.Contains(res, want) {
			t.Errorf("list output missing template %s", want)
		}
	}
}

func TestDesignTemplateToolLoad(t *testing.T) {
	tool := &DesignTemplateTool{}
	res, err := tool.Execute(context.Background(), `{"id":"web-prototype"}`)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if !strings.Contains(res, "# Design Template: web-prototype") {
		t.Error("load output should have template header")
	}
	// frontmatter 已剥离，正文含工作流与资源图。
	if strings.Contains(res, "name: web-prototype") {
		t.Error("frontmatter should be stripped from workflow body")
	}
	if !strings.Contains(res, "assets/template.html") {
		t.Error("workflow should reference the seed file")
	}
}

func TestDesignTemplateToolReadFile(t *testing.T) {
	tool := &DesignTemplateTool{}
	res, err := tool.Execute(context.Background(), `{"id":"web-prototype","path":"references/layouts.md"}`)
	if err != nil {
		t.Fatalf("read file error: %v", err)
	}
	if len(res) < 100 {
		t.Errorf("reference file too short: %d bytes", len(res))
	}
	// 路径穿越拒绝。
	if _, err := tool.Execute(context.Background(), `{"id":"web-prototype","path":"../../app.go"}`); err == nil {
		t.Error("expected error for traversal path")
	}
}

func TestDesignTemplateToolUnknown(t *testing.T) {
	tool := &DesignTemplateTool{}
	_, err := tool.Execute(context.Background(), `{"id":"nope"}`)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}
