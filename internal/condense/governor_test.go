// governor_test.go —— 上下文治理策略链单元测试：策略顺序执行、
// 预算内跳过（no-op）、摘要失败回退截断、硬裁剪兜底、Applied 视图向后传递。
package condense

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"kaelvoris/internal/llm"
)

// fakeSummarizer 可控 Summarizer 桩：返回固定摘要或错误。
func fakeSummarizer(summary string, err error) Summarizer {
	return func(ctx context.Context, msgs []llm.Message) (string, error) {
		return summary, err
	}
}

// TestGovernor_BudgetIdle 载荷已在预算内：全部策略 skipped，视图零改动。
func TestGovernor_BudgetIdle(t *testing.T) {
	msgs := conv(4)
	hidden := make([]bool, len(msgs))
	g := NewGovernor(
		&SummarizePolicy{Summarizer: fakeSummarizer("should not be called", nil)},
		&TruncatePolicy{Frac: 0.5},
		&HardTrimPolicy{},
	)
	// 4 条短消息 ≈ 每条 4+1 token，远低于 1024 预算。
	outs := g.Run(msgs, hidden, 1024)
	if len(outs) != 3 {
		t.Fatalf("steps = %d, want 3", len(outs))
	}
	for _, o := range outs {
		if o.Status != StepSkipped {
			t.Errorf("step %s status = %s, want skipped", o.Kind, o.Status)
		}
	}
	if countHidden(outs[2].Hidden) != 0 {
		t.Error("budget-idle run must not hide anything")
	}
}

// TestGovernor_SummarizeApplied 摘要成功：第一步 Applied、视图替换、
// 第二步（预算已满足）skipped。
func TestGovernor_SummarizeApplied(t *testing.T) {
	msgs := conv(10)
	hidden := make([]bool, len(msgs))
	g := NewGovernor(
		&SummarizePolicy{Summarizer: fakeSummarizer("summary of early history", nil), KeepRecent: 2},
		&TruncatePolicy{Frac: 0.5},
	)
	// conv(10) 估算 ≈55 token > 40 预算触发摘要；摘要后可见视图 ≈31 token < 40，
	// 后续截断策略应 skipped。
	outs := g.Run(msgs, hidden, 40)
	if outs[0].Status != StepApplied {
		t.Fatalf("summarize status = %s, want applied", outs[0].Status)
	}
	if outs[0].Summary != "summary of early history" {
		t.Errorf("summary = %q", outs[0].Summary)
	}
	if outs[0].Removed == 0 {
		t.Error("summarize must remove messages")
	}
	// 摘要后视图应明显变小（标记消息 + 最近 2 条）。
	visible := VisibleView(outs[0].Messages, outs[0].Hidden)
	if len(visible) > 4 {
		t.Errorf("visible after summarize = %d, want small", len(visible))
	}
}

// TestGovernor_SummarizeFailFallsToTruncate 摘要失败：计 failed，视图原样
// 传递给后续截断策略，最终 Applied 的是截断。
func TestGovernor_SummarizeFailFallsToTruncate(t *testing.T) {
	msgs := conv(10)
	hidden := make([]bool, len(msgs))
	g := NewGovernor(
		&SummarizePolicy{Summarizer: fakeSummarizer("", fmt.Errorf("llm down"))},
		&TruncatePolicy{Frac: 0.5},
	)
	outs := g.Run(msgs, hidden, 40)
	if outs[0].Status != StepFailed {
		t.Fatalf("summarize status = %s, want failed", outs[0].Status)
	}
	if outs[1].Status != StepApplied {
		t.Fatalf("truncate status = %s, want applied", outs[1].Status)
	}
	if outs[1].Removed == 0 {
		t.Error("truncate must hide messages")
	}
	if countHidden(outs[1].Hidden) == 0 {
		t.Error("hidden flags not propagated to truncate step")
	}
}

// TestGovernor_HardTrimFallback 巨型尾部消息组无法被组粒度截断移除：
// 截断到极限后硬裁剪 Applied，最终估算必须落入预算（413 兑底语义）。
func TestGovernor_HardTrimFallback(t *testing.T) {
	huge := strings.Repeat("A", 20000) // ~5000 token
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "system"},
		{Role: llm.RoleUser, Content: "task"},
		{Role: llm.RoleAssistant, Content: "reading"},
		{Role: llm.RoleTool, Content: huge, ToolCallID: "c1"}, // 与 assistant 原子成组
	}
	hidden := make([]bool, len(msgs))
	g := NewGovernor(&TruncatePolicy{Frac: 0.5}, &HardTrimPolicy{})
	outs := g.Run(msgs, hidden, 2000)
	final := outs[len(outs)-1]
	if final.Status != StepApplied || final.Kind != StepHardTrim {
		t.Fatalf("final step = %s/%s, want hard_trim/applied", final.Kind, final.Status)
	}
	if est := EstimateTokens(VisibleView(final.Messages, final.Hidden)); est > 2000 {
		t.Errorf("estimate after hard trim = %d, want <= 2000", est)
	}
}

// TestGovernor_ZeroPolicies 空策略链：返回空结果且不 panic（防御）。
func TestGovernor_ZeroPolicies(t *testing.T) {
	msgs := conv(2)
	outs := NewGovernor().Run(msgs, nil, 100)
	if len(outs) != 0 {
		t.Errorf("outs = %d, want 0", len(outs))
	}
}
