package desktop

import (
	"strings"
	"testing"

	"kaelvoris/internal/session"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// 规划板 rm_7e7957d06c9e：Agent 续接意图先读会话记忆待办定位断点。
// ResumeDigestFor 意图门控回归：
//   - 「继续」类输入 + 未完成待办/记忆文档 → 注入续接上下文；
//   - 「继续」类输入 + 无记忆/无未完成待办 → 退化为普通对话（空串）；
//   - 非「继续」输入 → 维持既有行为（有历史/待办即注入）。

func TestIsResumeIntent(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"继续", true},
		{"继续 做登录页", true},
		{"接着干", true},
		{"续上之前的任务", true},
		{"continue", true},
		{"Continue the task", true},
		{"go on", true},
		{"keep going", true},
		{"resume", true},
		{"。继续", true}, // 前缀标点容错
		{"！接着", true},
		{"", false},
		{"你好", false},
		{"我不想继续", false}, // 非前缀，不命中
		{"再次继续", false},
		{"做一个登录页", false},
		{"con", false}, // 部分词不命中（避免 continue 前缀过宽）
	}
	for _, c := range cases {
		if got := isResumeIntent(c.in); got != c.want {
			t.Errorf("isResumeIntent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// mockSnap 构造带历史消息 + 待办 + 记忆的会话快照。
func mockSnap(todos []tools.TodoItem, withMsg bool, condensed bool) session.SessionData {
	s := session.SessionData{}
	if withMsg {
		s.Messages = append(s.Messages, message.NewSay(message.SayUserFeedback, "上轮做了一半"))
	}
	s.Todos = todos
	if condensed {
		s.Condense = &session.CondenseRecord{Condensed: true, Summary: "早期摘要"}
	}
	return s
}

func TestResumeDigestFor_ContinueWithTodo(t *testing.T) {
	snap := mockSnap([]tools.TodoItem{
		{Content: "做登录页", Status: "in_progress"},
	}, true, false)
	digest := ResumeDigestFor(snap, "## 任务\n做登录页", "继续")
	if !strings.Contains(digest, "做登录页") { // 待办引用
		t.Fatalf("continue + todo 应注入待办，got: %q", digest)
	}
}

func TestResumeDigestFor_ContinueNoMemoryDegrade(t *testing.T) {
	// 继续 + 无记忆 + 无未完成待办 → 退化为普通对话（空串）。
	snap := mockSnap([]tools.TodoItem{{Content: "已完成项", Status: "completed"}}, true, false)
	if got := ResumeDigestFor(snap, "", "继续"); got != "" {
		t.Fatalf("continue + 无断点应退化空串, got: %q", got)
	}
}

func TestResumeDigestFor_NonContinueWithHistory(t *testing.T) {
	// 非继续 + 有历史 → 维持既有注入行为。
	snap := mockSnap(nil, true, false)
	if got := ResumeDigestFor(snap, "", "帮我改一下 bug"); !strings.Contains(got, "上轮做了一半") {
		t.Fatalf("非继续 + 有历史应注入最近对话, got: %q", got)
	}
}

func TestResumeDigestFor_NonContinueNoHistoryDegrade(t *testing.T) {
	// 非继续 + 新会话（无历史/待办）→ 空串。
	snap := mockSnap(nil, false, false)
	if got := ResumeDigestFor(snap, "", "你好"); got != "" {
		t.Fatalf("新会话应空串, got: %q", got)
	}
}
