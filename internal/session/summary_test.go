package session

import (
	"strings"
	"testing"
	"time"

	"kaelvoris/pkg/message"
)

// 会话卡片信息升级（规划板 2026-10）：最后一条消息摘要提取。
// 回归点：跳过流式中间帧与用量帧；媒体卡占位；单行化截断。
func TestLastMessageSummary(t *testing.T) {
	cases := []struct {
		name string
		msgs []message.ClineMessage
		want string
	}{
		{
			name: "空会话",
			msgs: nil,
			want: "",
		},
		{
			name: "取最后一条定型 text",
			msgs: []message.ClineMessage{
				message.NewSay(message.SayText, "第一轮回复"),
				message.NewSay(message.SayUserFeedback, "继续"),
				message.NewSay(message.SayText, "最终结果内容"),
			},
			want: "最终结果内容",
		},
		{
			name: "跳过尾部 partial 与 api_req_finished",
			msgs: []message.ClineMessage{
				message.NewSay(message.SayText, "正文答案"),
				message.NewSay(message.SayAPIReqFinished, "{}"),
			},
			want: "正文答案",
		},
		{
			name: "跳过 usage/ai_context 快照帧",
			msgs: []message.ClineMessage{
				message.NewSay(message.SayText, "实现完成"),
				{TS: 1, Type: message.TypeSay, Say: message.SayUsage, Text: "{\"total\":1}"},
			},
			want: "实现完成",
		},
		{
			name: "媒体卡占位",
			msgs: []message.ClineMessage{
				message.NewSay(message.SayText, "生成中"),
				message.NewSay(message.SayImage, ""),
			},
			want: "🖼 [图片]",
		},
		{
			name: "多行折单行",
			msgs: []message.ClineMessage{
				message.NewSay(message.SayText, "第一行\n  第二行\n\n第三行"),
			},
			want: "第一行 第二行 第三行",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lastMessageSummary(c.msgs); got != c.want {
				t.Fatalf("lastMessageSummary = %q, want %q", got, c.want)
			}
		})
	}
}

// 摘要超长截断：rune 上限 + 省略号（中文按 rune 计数不截半字）。
func TestTruncateSummaryRunes(t *testing.T) {
	long := strings.Repeat("汉", summaryMaxRunes+10)
	got := truncateSummary(long)
	if got == "" || !strings.HasSuffix(got, "…") {
		t.Fatalf("超长摘要应带省略号: %q", got)
	}
	if n := len([]rune(strings.TrimSuffix(got, "…"))); n != summaryMaxRunes {
		t.Fatalf("截断后 rune 数 = %d, want %d", n, summaryMaxRunes)
	}
	if got := truncateSummary("短摘要"); got != "短摘要" {
		t.Fatalf("短摘要不应截断: %q", got)
	}
}

// Summary 输出：lastMessage 摘要、手动摘要优先、错误态与置顶毫秒时间戳。
func TestSummaryFields(t *testing.T) {
	s := &Session{ID: "s1", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	s.AppendMerged(message.NewSay(message.SayText, "会话正文摘要"))
	sm := s.Summary()
	if sm.LastMessage != "会话正文摘要" {
		t.Fatalf("LastMessage = %q, want 自动提取摘要", sm.LastMessage)
	}

	// 手动摘要覆盖自动提取（manual_preview 链路）。
	s.SetManualPreview("  用户标注的摘要  ")
	if sm := s.Summary(); sm.LastMessage != "用户标注的摘要" {
		t.Fatalf("手动摘要未覆盖自动提取: %q", sm.LastMessage)
	}
	s.SetManualPreview("")
	if sm := s.Summary(); sm.LastMessage != "会话正文摘要" {
		t.Fatalf("清空手动摘要后应恢复自动提取: %q", sm.LastMessage)
	}

	// 错误态 + 置顶时间戳。
	if s.Summary().HasError {
		t.Fatal("未置错误时 HasError 应为 false")
	}
	s.SetError(true)
	if sm := s.Summary(); !sm.HasError {
		t.Fatal("SetError(true) 后 HasError 应为 true")
	}
	s.SetError(false)

	// 成功完成态：置完成清除错误（中途已恢复报错不残留红点）；
	// 错误 > 完成的终态语义：错误再次落置时清除完成。
	s.SetError(true)
	s.SetDone(true)
	if sm := s.Summary(); !sm.Done || sm.HasError {
		t.Fatalf("SetDone(true) 后应为 Done 且无 HasError, got done=%v err=%v", sm.Done, sm.HasError)
	}
	s.SetError(true)
	if sm := s.Summary(); sm.Done || !sm.HasError {
		t.Fatalf("SetError(true) 后应清除 Done, got done=%v err=%v", sm.Done, sm.HasError)
	}
	s.SetError(false)
	s.SetDone(false)

	if s.Summary().PinnedAt != 0 {
		t.Fatal("未置顶时 PinnedAt 应为 0")
	}
	s.SetPinned(true)
	if sm := s.Summary(); sm.PinnedAt <= 0 {
		t.Fatalf("置顶后 PinnedAt 应为毫秒时间戳, got %d", sm.PinnedAt)
	}
	s.SetPinned(false)
	if sm := s.Summary(); sm.PinnedAt != 0 {
		t.Fatalf("取消置顶后 PinnedAt 应为 0, got %d", sm.PinnedAt)
	}
}

// Store.List 排序：置顶会话恒排前（新置顶在前），未置顶按更新时间倒序。
func TestStoreListPinnedFirst(t *testing.T) {
	st := NewStore(t.TempDir())
	defer st.Close()

	mk := func(id string, age time.Duration) *Session {
		s := &Session{ID: id, CreatedAt: time.Now().Add(-age), UpdatedAt: time.Now().Add(-age)}
		st.Save(s)
		return s
	}
	mk("old", 3*time.Hour)
	mk("mid", 2*time.Hour)
	p := mk("pinned-old", 4*time.Hour) // 更早但置顶 → 应排最前
	p.SetPinned(true)
	st.Save(p)

	list := st.List()
	if len(list) != 3 {
		t.Fatalf("want 3 sessions, got %d", len(list))
	}
	if list[0].ID != "pinned-old" {
		t.Fatalf("置顶会话应排最前, got %s", list[0].ID)
	}
	if list[1].ID != "mid" || list[2].ID != "old" {
		t.Fatalf("未置顶按更新时间倒序, got [%s, %s]", list[1].ID, list[2].ID)
	}

	// 取消置顶后回到常规排序。
	p.SetPinned(false)
	st.Save(p)
	list = st.List()
	if list[0].ID != "mid" {
		t.Fatalf("取消置顶后应回到更新时间倒序, got %s", list[0].ID)
	}
}
