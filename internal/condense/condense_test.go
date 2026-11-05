// 上下文治理单元测试：truncateConversation 移植语义边界
// （成组性 / 首条保留 / 标记消息 / 渐进截断 / token 估算）。
package condense

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"kaelvoris/internal/llm"
)

// conv 构造 system + n 条交替 assistant/user 单条消息（每组 1 条）。
func conv(n int) []llm.Message {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: "system prompt"}}
	for i := 0; i < n; i++ {
		role := llm.RoleUser
		if i%2 == 0 {
			role = llm.RoleAssistant
		}
		msgs = append(msgs, llm.Message{Role: role, Content: fmt.Sprintf("m%d", i)})
	}
	return msgs
}

// countHidden 统计隐藏条数。
func countHidden(hidden []bool) int {
	n := 0
	for _, h := range hidden {
		if h {
			n++
		}
	}
	return n
}

// TestTruncateConversation_Basic 基础语义：头部保护（system + 首条任务）、
// 组数向下取整、标记消息插入保留边界、非破坏性（返回新数组）。
func TestTruncateConversation_Basic(t *testing.T) {
	msgs := conv(8) // system + 8 条 → 9 组
	res := TruncateConversation(msgs, nil, 0.5)

	// truncatable = 9 - 2（头部保护） = 7 组；remove = floor(7*0.5) = 3。
	if res.MessagesRemoved != 3 {
		t.Fatalf("MessagesRemoved = %d, want 3", res.MessagesRemoved)
	}
	if len(res.Messages) != len(msgs)+1 {
		t.Fatalf("len(Messages) = %d, want %d（原 %d + 标记 1）",
			len(res.Messages), len(msgs)+1, len(msgs))
	}
	if len(res.Hidden) != len(res.Messages) {
		t.Fatalf("len(Hidden) = %d, want %d", len(res.Hidden), len(res.Messages))
	}
	if res.TruncationID == "" {
		t.Fatal("TruncationID 为空")
	}

	// 头部保护：system 与首条任务消息恒可见。
	if res.Hidden[0] || res.Hidden[1] {
		t.Error("system / 首条任务消息不应被隐藏")
	}
	// 最旧 3 条（索引 2..4）被隐藏，其余保留。
	for i := 2; i <= 4; i++ {
		if !res.Hidden[i] {
			t.Errorf("索引 %d 应被隐藏", i)
		}
	}
	for i := 5; i <= 8; i++ {
		if res.Hidden[i] {
			t.Errorf("索引 %d 不应被隐藏", i)
		}
	}
	// 标记消息插入保留边界（索引 5），user 角色、始终可见。
	marker := res.Messages[5]
	if marker.Role != llm.RoleUser || !IsMarker(marker) {
		t.Errorf("标记消息位置/内容错误: %+v", marker)
	}
	if res.Hidden[5] {
		t.Error("标记消息不应被隐藏")
	}
	if !strings.Contains(marker.Content, "3 messages hidden") {
		t.Errorf("标记消息应包含隐藏数量: %q", marker.Content)
	}
	// 原始切片未被修改（非破坏性）：长度不变、内容原样。
	if len(msgs) != 9 || msgs[2].Content != "m1" || msgs[2].Role != llm.RoleUser {
		t.Error("原始消息数组不应被修改")
	}
}

// TestTruncateConversation_NothingToRemove 无可隐藏时不动作。
func TestTruncateConversation_NothingToRemove(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "task"},
	}
	res := TruncateConversation(msgs, nil, 0.5)
	if res.MessagesRemoved != 0 {
		t.Errorf("MessagesRemoved = %d, want 0", res.MessagesRemoved)
	}
	if len(res.Messages) != 2 || len(res.Hidden) != 2 {
		t.Errorf("应原样返回: %d 条", len(res.Messages))
	}
	if res.TruncationID == "" {
		t.Error("TruncationID 仍应生成")
	}
}

// TestTruncateConversation_ToolGroupAtomic 工具组原子性：assistant(tool_calls)
// 与其后 tool 结果整组隐藏；可见视图满足 OpenAI 协议约束（每条 tool 消息
// 的 tool_call_id 均有前置 assistant 声明）。
func TestTruncateConversation_ToolGroupAtomic(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "task"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "a1", Name: "fs_write"}, {ID: "a2", Name: "fs_write"},
		}},
		{Role: llm.RoleTool, ToolCallID: "a1", Content: "ok1"},
		{Role: llm.RoleTool, ToolCallID: "a2", Content: "ok2"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "b1", Name: "shell"}}},
		{Role: llm.RoleTool, ToolCallID: "b1", Content: "ok3"},
		{Role: llm.RoleAssistant, Content: "plain text"},
		{Role: llm.RoleUser, Content: "feedback"},
	}
	res := TruncateConversation(msgs, nil, 0.5)
	// 6 组 - 头部 2 = 4 组可截；remove = 2 组 = [A(a1,a2)+tool+tool] + [A(b1)+tool] = 5 条。
	if res.MessagesRemoved != 5 {
		t.Fatalf("MessagesRemoved = %d, want 5（两条 assistant 组整体隐藏）", res.MessagesRemoved)
	}
	view := VisibleView(res.Messages, res.Hidden)

	// wire 有效性：每条 tool 消息的 tool_call_id 必须由紧邻前置 assistant 声明。
	pending := map[string]bool{}
	prevAssistant := false
	for _, m := range view {
		switch m.Role {
		case llm.RoleAssistant:
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
			}
			prevAssistant = true
		case llm.RoleTool:
			if !prevAssistant || !pending[m.ToolCallID] {
				t.Fatalf("孤儿 tool 消息（协议违规）: %q", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		default:
			prevAssistant = false
		}
	}
	if len(pending) != 0 {
		t.Errorf("存在未应答的 tool_calls: %v", pending)
	}
}

// TestTruncateConversation_Progressive 渐进截断：二次截断继续隐藏可见部分，
// 头部保护与既有标记在多轮后仍然成立。
func TestTruncateConversation_Progressive(t *testing.T) {
	msgs := conv(10) // system + 10 条
	r1 := TruncateConversation(msgs, nil, 0.5)
	r2 := TruncateConversation(r1.Messages, r1.Hidden, 0.5)
	if r2.MessagesRemoved == 0 {
		t.Fatal("第二轮应继续隐藏")
	}
	if r2.Hidden[0] || r2.Hidden[1] {
		t.Error("头部消息多轮后仍不应被隐藏")
	}
	for i, m := range r2.Messages {
		if IsMarker(m) && r2.Hidden[i] {
			t.Errorf("标记消息 %d 不应被隐藏", i)
		}
	}
	// 可见视图始终 wire 合法（无孤儿 tool 消息——本序列无 tool，仅验前缀）。
	view := VisibleView(r2.Messages, r2.Hidden)
	if view[0].Role != llm.RoleSystem {
		t.Error("可见视图应以 system 开头")
	}
}

// TestVisibleView_ZeroCopy 未发生截断时 VisibleView 应原样返回。
func TestVisibleView_ZeroCopy(t *testing.T) {
	msgs := conv(4)
	view := VisibleView(msgs, nil)
	if len(view) != len(msgs) {
		t.Fatalf("无隐藏时应全量可见: %d", len(view))
	}
	view = VisibleView(msgs, []bool{false, false, false, false, false})
	if len(view) != len(msgs) {
		t.Errorf("全 false 隐藏标记应全量可见: %d", len(view))
	}
}

// TestEstimateTokens token 估算：ASCII 4 字符/token、CJK 1 字/token、图片定额。
func TestEstimateTokens(t *testing.T) {
	if got := EstimateText("abcdefgh"); got != 2 {
		t.Errorf("EstimateText ascii = %d, want 2", got)
	}
	if got := EstimateText("一二三四"); got != 4 {
		t.Errorf("EstimateText cjk = %d, want 4", got)
	}
	if got := EstimateText(""); got != 0 {
		t.Errorf("EstimateText empty = %d, want 0", got)
	}
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "hi", Images: []string{"data:image/png;base64,x"}}}
	// 4（消息开销）+ 1（"hi"）+ 800（图片）= 805。
	if got := EstimateTokens(msgs); got != 805 {
		t.Errorf("EstimateTokens = %d, want 805", got)
	}
}

// okSummarizer 固定返回摘要文本的测试桩。
func okSummarizer(text string) Summarizer {
	return func(context.Context, []llm.Message) (string, error) { return text, nil }
}

// TestSummarizeConversation_Basic 摘要压缩主路径：待压缩区整体隐藏、摘要
// 标记消息插入保留边界、头部保护恒保留。
func TestSummarizeConversation_Basic(t *testing.T) {
	msgs := conv(6) // system + 6 条 → 7 组
	res := SummarizeConversation(msgs, nil, okSummarizer("SUMMARY"))
	if !res.Condensed {
		t.Fatal("Condensed = false, want true")
	}
	if res.MessagesRemoved != 5 { // 头部保护 2 组，其余 5 条进待压缩区
		t.Fatalf("MessagesRemoved = %d, want 5", res.MessagesRemoved)
	}
	if res.SummaryText != "SUMMARY" {
		t.Errorf("SummaryText = %q", res.SummaryText)
	}
	if res.Hidden[0] || res.Hidden[1] {
		t.Error("system / 首条任务消息不应被隐藏")
	}
	for i := 2; i <= 6; i++ {
		if !res.Hidden[i] {
			t.Errorf("索引 %d 应被摘要隐藏", i)
		}
	}
	// 摘要标记消息插入保留边界（keepRecent=0 时在末尾），user 角色、始终可见。
	marker := res.Messages[len(res.Messages)-1]
	if !IsSummarizeMarker(marker) || !strings.Contains(marker.Content, "SUMMARY") {
		t.Errorf("摘要标记消息错误: %+v", marker)
	}
	if res.Hidden[len(res.Hidden)-1] {
		t.Error("摘要标记消息不应被隐藏")
	}
	// 可见视图：system + 任务消息 + 摘要标记（近期消息在 keepRecent=0 时也被摘要）。
	view := VisibleView(res.Messages, res.Hidden)
	if len(view) != 3 {
		t.Fatalf("可见视图 %d 条, want 3（system+任务+摘要标记）", len(view))
	}
}

// TestSummarizeConversation_KeepRecent 保留最近 N 条原文语义（规划板 2026-10）：
// 摘要仅覆盖较早历史，最近 keepRecent 组保持原文；消息不足时原样返回。
func TestSummarizeConversation_KeepRecent(t *testing.T) {
	msgs := conv(8) // system + 8 条 → 9 组
	res := SummarizeConversationKeep(msgs, nil, okSummarizer("SUM"), 4)
	if !res.Condensed {
		t.Fatal("Condensed = false, want true")
	}
	if res.MessagesRemoved != 3 {
		t.Fatalf("MessagesRemoved = %d, want 3（头部 2 + 保留 4，余 3 组入摘要）", res.MessagesRemoved)
	}
	if res.KeptRecent != 4 {
		t.Errorf("KeptRecent = %d, want 4", res.KeptRecent)
	}
	// 最近 4 组（索引 5..8）保持原文可见。
	for i := 5; i <= 8; i++ {
		if res.Hidden[i] {
			t.Errorf("最近消息索引 %d 不应被隐藏", i)
		}
	}
	// 较早历史（索引 2..4）被摘要隐藏。
	for i := 2; i <= 4; i++ {
		if !res.Hidden[i] {
			t.Errorf("较早消息索引 %d 应被摘要隐藏", i)
		}
	}
	// 摘要标记消息插入保留边界（索引 5 前），且保留消息顺序不变。
	marker := res.Messages[5]
	if !IsSummarizeMarker(marker) {
		t.Fatalf("保留边界缺少摘要标记: %+v", marker)
	}
	view := VisibleView(res.Messages, res.Hidden)
	if len(view) != 3+4 { // system + 首条任务 + 摘要标记 + 保留 4 条
		t.Fatalf("可见视图 %d 条, want 7", len(view))
	}
	last := view[len(view)-1]
	if last.Content != "m7" {
		t.Errorf("末尾保留消息错误: %+v", last)
	}
}

// TestSummarizeConversation_KeepRecentInsufficient 保留区吃掉全部可压缩区时
// 原样返回（无可摘要内容，不烧 LLM）。
func TestSummarizeConversation_KeepRecentInsufficient(t *testing.T) {
	msgs := conv(6)
	called := false
	s := func(context.Context, []llm.Message) (string, error) { called = true; return "SUM", nil }
	res := SummarizeConversationKeep(msgs, nil, s, 99)
	if res.Condensed || called {
		t.Error("保留区覆盖全部分组时不应执行摘要")
	}
}

// TestSummarizeConversation_FailPaths 摘要失败路径：nil/错误/空摘要均原样返回。
func TestSummarizeConversation_FailPaths(t *testing.T) {
	msgs := conv(4)
	for name, s := range map[string]Summarizer{
		"nil":   nil,
		"err":   func(context.Context, []llm.Message) (string, error) { return "", fmt.Errorf("boom") },
		"empty": func(context.Context, []llm.Message) (string, error) { return "  ", nil },
	} {
		res := SummarizeConversation(msgs, nil, s)
		if res.Condensed || res.MessagesRemoved != 0 {
			t.Errorf("%s: 摘要失败应原样返回", name)
		}
	}
}
