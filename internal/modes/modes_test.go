// 模式系统测试：默认模式、工具组推导、自定义模式覆盖。
package modes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultModes(t *testing.T) {
	ms := DefaultModes()
	if len(ms) != 5 {
		t.Fatalf("DefaultModes() = %d, want 5", len(ms))
	}
	slugs := map[string]bool{}
	for _, m := range ms {
		slugs[m.Slug] = true
	}
	for _, want := range []string{"architect", "code", "ask", "debug", "orchestrator"} {
		if !slugs[want] {
			t.Errorf("missing default mode %q", want)
		}
	}
}

func TestToolsForMode(t *testing.T) {
	// code = read+edit+command+mcp + always-available。
	codeTools := ToolsForMode([]GroupEntry{"read", "edit", "command", "mcp"})
	has := func(list []string, name string) bool {
		for _, n := range list {
			if n == name {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"fs_read", "fs_write", "shell", "attempt_completion", "new_task", "switch_mode"} {
		if !has(codeTools, want) {
			t.Errorf("code tools missing %q", want)
		}
	}

	// orchestrator：groups 为空 → 仅 always-available。
	orch := ToolsForMode([]GroupEntry{})
	if has(orch, "fs_read") || has(orch, "shell") {
		t.Errorf("orchestrator should not have fs/shell tools: %v", orch)
	}
	if !has(orch, "new_task") {
		t.Error("orchestrator missing new_task")
	}

	// ask：无 edit/command。
	ask := ToolsForMode([]GroupEntry{"read", "mcp"})
	if has(ask, "fs_write") || has(ask, "shell") {
		t.Errorf("ask mode should be read-only: %v", ask)
	}
}

// TestOrchestratorParallelGuidance orchestrator 提示词引导拆分与并行委派
// （P2.8 fan-out 能力透出：可并行的子任务同轮批量委派）。
func TestOrchestratorParallelGuidance(t *testing.T) {
	for _, m := range DefaultModes() {
		if m.Slug != "orchestrator" {
			continue
		}
		for _, want := range []string{"FIRST analyze it and break it down", "PARALLELIZE", "multiple new_task tool calls", "Never let two parallel subtasks edit the same file"} {
			if !strings.Contains(m.CustomInstructions, want) {
				t.Errorf("orchestrator instructions missing %q", want)
			}
		}
	}
}

func TestManagerCustomOverride(t *testing.T) {
	dir := t.TempDir()
	// 自定义模式覆盖内置 code。
	cfg := `[{"slug":"code","name":"Custom Code","roleDefinition":"custom role","groups":["read"]}]`
	if err := os.WriteFile(filepath.Join(dir, ".roomodes"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	if err := m.LoadProject(dir); err != nil {
		t.Fatalf("LoadProject: %v", err)
	}

	got, ok := m.GetBySlug("code")
	if !ok || got.Name != "Custom Code" {
		t.Fatalf("GetBySlug(code) = %+v ok=%v, want custom override", got, ok)
	}
	tools := m.ToolsFor("code")
	for _, n := range tools {
		if n == "fs_write" || n == "shell" {
			t.Errorf("custom code mode should be read-only, got %q", n)
		}
	}
}

func TestManagerUnknownSlugFallsBack(t *testing.T) {
	m := NewManager()
	tools := m.ToolsFor("nonexistent")
	if len(tools) == 0 {
		t.Error("unknown slug should fall back to always-available tools")
	}
}

func TestArchitectEditRestrictedToMarkdown(t *testing.T) {
	arch := NewManager().ToolsFor("architect")
	hasEdit := false
	for _, n := range arch {
		if n == "fs_write" || n == "fs_apply_diff" {
			hasEdit = true
		}
	}
	// architect 组含 edit 条目（fileRegex 限制 .md，正则约束由执行层遵守）。
	if !hasEdit {
		t.Error("architect should include edit tools (markdown-only via fileRegex)")
	}
}

// TestNewCapabilityToolsVisible 2026-10 能力补全：新工具必须随 read/command
// 组发放，否则模型（getToolsForMode 过滤）永远收不到——曾有 web_tools.go
// 实现但未注册的教训，本测试是发放面的回归护栏。
func TestNewCapabilityToolsVisible(t *testing.T) {
	codeTools := ToolsForMode([]GroupEntry{"read", "edit", "command", "mcp"})
	has := func(list []string, name string) bool {
		for _, n := range list {
			if n == name {
				return true
			}
		}
		return false
	}
		for _, want := range []string{
			"web_search", "web_fetch", "arxiv_search", "pdf_read",
			"speech_to_text", "email_send", "notion",
			"browser_open", "browser_eval", "browser_screenshot",
			"browser_act", "browser_snapshot", "browser_tabs",
			"desktop_windows", "desktop_screenshot", "desktop_act",
		} {
		if !has(codeTools, want) {
			t.Errorf("new capability tool %q not visible in code mode", want)
		}
	}
}
