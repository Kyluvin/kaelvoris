package desktop

// roadmap_system_prompt_test.go 自主开发会话系统提示词（用户需求 2026-11）单测：
//  1. roadmapSystemPrompt 兜底链：配置空 = 内置默认守则；配置非空 = 覆盖（同
//     selfCheckTask 语义）；
//  2. buildRoadmapLaunchTask 拼装：守则前置 + 空行分隔 + 任务全文；守则空 = 原样
//     透传（不产生空头）；
//  3. 内置守则内容自包含：覆盖无人值守自主决策 / 真实验证 / attempt_completion
//     诚实收口三个关键面（与 finishDevelopRun 虚假胜利拦截形成呼应）。

import (
	"path/filepath"
	"strings"
	"testing"

	"kaelvoris/internal/promptconfig"
)

func TestRoadmapSystemPromptFallback(t *testing.T) {
	a := &App{promptStore: promptconfig.New(filepath.Join(t.TempDir(), "prompts.json"))}
	// 配置空 → 内置默认守则
	if got := a.roadmapSystemPrompt(); got != roadmapSystemPromptDefault {
		t.Errorf("empty config should fall back to builtin default")
	}
	// 配置非空 → 覆盖（含空白裁剪）
	if err := a.promptStore.Save(promptconfig.Config{RoadmapSystem: "  自定义守则  "}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := a.roadmapSystemPrompt(); got != "自定义守则" {
		t.Errorf("config override = %q, want trimmed custom text", got)
	}
}

func TestBuildRoadmapLaunchTask(t *testing.T) {
	got := buildRoadmapLaunchTask("守则A", "任务B")
	if !strings.HasPrefix(got, "守则A\n\n任务B") {
		t.Errorf("launch task = %q, want prompt-first then blank-line separated task", got)
	}
	// 守则为空/纯空白 → 原样透传
	if got := buildRoadmapLaunchTask("  \n ", "任务B"); got != "任务B" {
		t.Errorf("empty prompt passthrough = %q, want task as-is", got)
	}
}

func TestRoadmapSystemPromptDefaultContent(t *testing.T) {
	for _, want := range []string{"ask_followup", "attempt_completion", "go build"} {
		if !strings.Contains(roadmapSystemPromptDefault, want) {
			t.Errorf("builtin default missing key clause %q", want)
		}
	}
}
