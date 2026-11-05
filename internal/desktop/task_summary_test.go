// task_summary_test.go —— 会话总结（task_summary.go）单测。
package desktop

import (
	"strings"
	"testing"

	"kaelvoris/internal/session"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// TestParseTaskSummaryOutput_Object 新协议：JSON 对象解析（goal/status/outcome/next）。
func TestParseTaskSummaryOutput_Object(t *testing.T) {
	in := `这是总结（含说明文本）：{"goal":"重构登录模块并补齐测试","status":"done","outcome":"完成 5 处改动，产出 internal/auth/*.go","next":"无"}`
	ts := parseTaskSummaryOutput(in)
	if ts == nil {
		t.Fatal("解析失败，应非 nil")
	}
	if ts.Goal != "重构登录模块并补齐测试" {
		t.Fatalf("Goal = %q", ts.Goal)
	}
	if ts.Status != "done" {
		t.Fatalf("Status = %q", ts.Status)
	}
	if ts.Outcome == "" || ts.Next == "" {
		t.Fatalf("Outcome/Next 应非空: %q %q", ts.Outcome, ts.Next)
	}
}

// TestParseTaskSummaryOutput_Empty 无有效输出（空串/垃圾/缺字段）。
func TestParseTaskSummaryOutput_Empty(t *testing.T) {
	for _, in := range []string{"", "垃圾文本无 JSON", `{"goal":""}`} {
		if ts := parseTaskSummaryOutput(in); ts != nil {
			t.Fatalf("输入 %q 应返回 nil, got %+v", in, ts)
		}
	}
}

// TestParseTaskSummaryOutput_CodeFence 代码围栏包裹输出（容错）。
func TestParseTaskSummaryOutput_CodeFence(t *testing.T) {
	in := "```json\n{\"goal\":\"修复崩溃\",\"status\":\"failed\",\"outcome\":\"定位到 nil 指针\",\"next\":\"补 nil 检查\"}\n```"
	ts := parseTaskSummaryOutput(in)
	if ts == nil {
		t.Fatal("围栏包裹应解析成功")
	}
	if ts.Goal != "修复崩溃" {
		t.Fatalf("Goal = %q", ts.Goal)
	}
}

// TestBuildTaskSummaryTranscript_Todos 转录应包含待办快照与任务异常尾注。
func TestBuildTaskSummaryTranscript_Todos(t *testing.T) {
	hist := []message.ClineMessage{
		message.NewSay(message.SayUserFeedback, "帮我重构登录模块"),
		{TS: 1, Type: message.TypeSay, Say: message.SayText, Text: "流式中", Partial: true},
		message.NewSay(message.SayText, "已定位 3 处改动点，开始实现"),
		message.NewSay(message.SayTool, `{"tool":"fs_write","path":"internal/auth/login.go"}`),
		message.NewSay(message.SayError, "编译错误：undefined: auth"),
	}
	todos := []tools.TodoItem{
		{Content: "拆解改动点", Status: "completed"},
		{Content: "实现 token 刷新", Status: "in_progress"},
	}
	out := buildTaskSummaryTranscript(hist, "重构登录模块", todos, nil)
	for _, want := range []string{"用户任务", "用户:", "助手:", "工具:", "错误:", "[任务清单]", "[x] 拆解改动点", "[-] 实现 token 刷新"} {
		if !strings.Contains(out, want) {
			t.Fatalf("转录应包含 %q, got:\n%s", want, out)
		}
	}
}

// TestBuildSummaryMessage 总结消息渲染：状态标签 + 字段。
func TestBuildSummaryMessage(t *testing.T) {
	ts := &session.TaskSummary{
		Goal:    "重构登录模块",
		Status:  "done",
		Outcome: "完成 5 处改动",
		Next:    "无",
	}
	msg := buildSummaryMessage(ts)
	for _, want := range []string{"会话总结", "✅ 已完成", "**目标**：重构登录模块", "**产出**：完成 5 处改动"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("消息应包含 %q, got:\n%s", want, msg)
		}
	}
	if summaryStatusLabel("failed") != "❌ 未完成" {
		t.Fatal("failed 标签错误")
	}
	if summaryStatusLabel("stopped") != "⏹ 已停止" {
		t.Fatal("stopped 标签错误")
	}
}
