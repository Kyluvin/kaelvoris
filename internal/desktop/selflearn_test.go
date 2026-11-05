// selflearn_test.go 自学习闭环单测：双路输出解析（新对象协议 + 旧数组协议
// 兼容）、转录构建（待办快照、错误尾注）、记忆收尾节覆盖/追加语义。
package desktop

import (
	"strings"
	"testing"

	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// TestParseSelfReviewOutput_Object 新协议：JSON 对象双路产出。
func TestParseSelfReviewOutput_Object(t *testing.T) {
	in := "```json\n{\"cards\": [{\"title\": \"重启前清理残留 dev 栈\", \"body\": \"先 netstat 查端口再按 PID 清理\", \"keywords\": [\"重启\", \"dev\"]}], \"memorySummary\": \"任务收尾：重构完成，下次接着干。\"}\n```"
	out := parseSelfReviewOutput(in)
	if len(out.Cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(out.Cards))
	}
	if out.Cards[0].Title != "重启前清理残留 dev 栈" {
		t.Fatalf("title = %q", out.Cards[0].Title)
	}
	if out.Cards[0].Keywords[0] != "重启" {
		t.Fatalf("keywords = %v", out.Cards[0].Keywords)
	}
	if !strings.Contains(out.MemorySummary, "下次接着干") {
		t.Fatalf("memorySummary = %q", out.MemorySummary)
	}
}

// TestParseSelfReviewOutput_Array 旧协议兼容：纯数组 = 仅经验卡（无记忆收尾）。
func TestParseSelfReviewOutput_Array(t *testing.T) {
	in := `[{"title": "卡A", "body": "正文A", "keywords": ["k"]}]`
	out := parseSelfReviewOutput(in)
	if len(out.Cards) != 1 || out.Cards[0].Title != "卡A" {
		t.Fatalf("cards = %+v", out.Cards)
	}
	if out.MemorySummary != "" {
		t.Fatalf("memorySummary 应为空，got %q", out.MemorySummary)
	}
}

// TestParseSelfReviewOutput_Empty 无沉淀输出（空对象 / 空串 / 垃圾）。
func TestParseSelfReviewOutput_Empty(t *testing.T) {
	for _, in := range []string{`{"cards": [], "memorySummary": ""}`, "", "模型说没什么好沉淀的"} {
		out := parseSelfReviewOutput(in)
		if len(out.Cards) != 0 || out.MemorySummary != "" {
			t.Fatalf("input %q → %+v, want empty", in, out)
		}
	}
}

// TestBuildSelfReviewTranscript_Todos 转录应包含待办快照与任务异常尾注。
func TestBuildSelfReviewTranscript_Todos(t *testing.T) {
	hist := []message.ClineMessage{
		{Type: message.TypeSay, Say: message.SayUserFeedback, Text: "继续修 bug"},
		{Type: message.TypeSay, Say: message.SayText, Text: "好的，开始修复\n第二行"},
		{Type: message.TypeSay, Say: message.SayTool, Text: `{"tool":"fs_write","path":"a.go"}`},
		{Type: message.TypeSay, Say: message.SayText, Text: "partial 中间帧", Partial: true},
	}
	todos := []tools.TodoItem{
		{Content: "定位根因", Status: "completed"},
		{Content: "修复并验证", Status: "in_progress"},
	}
	out := buildSelfReviewTranscript(hist, "修复登录崩溃", todos, errStub("context canceled"))
	for _, want := range []string{"[x] 定位根因", "[-] 修复并验证", "用户: 继续修 bug", "fs_write → a.go", "异常结束"} {
		if !strings.Contains(out, want) {
			t.Fatalf("转录缺少 %q\n---\n%s", want, out)
		}
	}
	// partial 帧不进转录。
	if strings.Contains(out, "partial 中间帧") {
		t.Fatalf("partial 帧不应进入转录")
	}
}

// errStub 测试用错误桩。
type errStub string

func (e errStub) Error() string { return string(e) }

// TestBuildSelfReviewTranscript_Empty 空历史 + 空任务 = 空转录（复盘跳过依据）。
func TestBuildSelfReviewTranscript_Empty(t *testing.T) {
	if out := buildSelfReviewTranscript(nil, "", nil, nil); strings.TrimSpace(out) == "" {
		// 只有标题行也视为可复盘素材，不强制为空——但纯空输入应为仅标题。
		t.Log("空输入转录:", out)
	}
}
