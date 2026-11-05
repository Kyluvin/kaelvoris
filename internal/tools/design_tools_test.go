// design_system 工具测试：无参浏览目录、带 id 加载规范、未知 id 报错。
package tools

import (
	"context"
	"strings"
	"testing"
)

func TestDesignSystemToolList(t *testing.T) {
	tool := &DesignSystemTool{}
	res, err := tool.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	if !strings.Contains(res, "Design systems catalog:") {
		t.Errorf("list output should contain catalog header, got:\n%.200s", res)
	}
	if !strings.Contains(res, "linear-app") {
		t.Error("list output should contain linear-app")
	}
	if !strings.Contains(res, "id: `") {
		t.Error("list output should contain id references")
	}
}

func TestDesignSystemToolLoad(t *testing.T) {
	tool := &DesignSystemTool{}
	res, err := tool.Execute(context.Background(), `{"id":"linear-app"}`)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if !strings.Contains(res, "DESIGN.md") || !strings.Contains(res, "tokens.css") {
		t.Error("load output should include both DESIGN.md and tokens.css sections")
	}
	if !strings.Contains(res, "Tailwind v4 Mapping") {
		t.Error("linear-app load output should include tailwind mapping")
	}
}

func TestDesignSystemToolUnknown(t *testing.T) {
	tool := &DesignSystemTool{}
	_, err := tool.Execute(context.Background(), `{"id":"nope"}`)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}
