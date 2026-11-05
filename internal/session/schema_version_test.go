package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// schema 版本字段（规划板 2026-10：会话文件 schema 版本化，支撑旧格式归一化）。
// v1 起会话 JSON 顶层携带 schemaVersion：写入打标（Store.write 单点）/
// 读取判别（loadLocked 单点，0 = 旧格式缺省归一为当前版本）。
// 本文件以表驱动用例固化三类现状行为（不改产品逻辑）：
//  1. 版本字段读写回环（空会话/带消息/带待办多形态）；
//  2. 缺版本字段旧文件加载不报错、按当前口径归一化；
//  3. 消息读写 roundtrip 与空会话/越界边界。

// TestSchemaVersionReadWriteTable 版本字段读写回环（表驱动）：Save → FlushAll →
// 直读文件校验顶层 schemaVersion 打标（写入单点 Store.write），再重开 Store
// 加载校验版本随 Snapshot 保留（读取链路保真）。
func TestSchemaVersionReadWriteTable(t *testing.T) {
	cases := []struct {
		name string
		fill func(*Session)
	}{
		{"empty_session", func(s *Session) {}},
		{"single_message", func(s *Session) {
			s.Append(message.NewSay(message.SayText, "hello"))
		}},
		{"messages_and_todos", func(s *Session) {
			s.Append(message.NewSay(message.SayText, "one"))
			s.Append(message.NewAsk(message.AskFollowup, "continue?"))
			s.SetTodos([]tools.TodoItem{{Content: "step", Status: "completed"}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "sessions")
			st := NewStore(dir)
			s := &Session{ID: tc.name, ChannelID: "desktop"}
			tc.fill(s)
			st.Save(s)
			st.FlushAll()
			st.Close()

			// 写入口径：文件顶层含 schemaVersion 字段（omitempty 不吞打标值）。
			b, err := os.ReadFile(filepath.Join(dir, tc.name+".json"))
			if err != nil {
				t.Fatalf("read session file: %v", err)
			}
			var raw map[string]any
			if err := json.Unmarshal(b, &raw); err != nil {
				t.Fatalf("session file invalid json: %v", err)
			}
			v, ok := raw["schemaVersion"].(float64)
			if !ok || int(v) != CurrentSchemaVersion {
				t.Fatalf("file missing schemaVersion stamp: got %v, want %d", raw["schemaVersion"], CurrentSchemaVersion)
			}

			// 读入口径：重开 Store 加载，版本经 loadLocked → IntoSession 保留。
			st2 := NewStore(dir)
			defer st2.Close()
			got, err := st2.Get(tc.name)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if snap := got.Snapshot(); snap.SchemaVersion != CurrentSchemaVersion {
				t.Fatalf("schemaVersion lost on round trip: %d", snap.SchemaVersion)
			}
		})
	}
}

// TestSchemaVersionLegacyLoadTable 缺版本字段旧文件加载（表驱动）：历史会话
// 无 schemaVersion 字段，加载不报错、字段原样保留，loadLocked 缺省分支把版本
// 归一为当前版本（0 = 旧格式 → 当前口径）；损坏 JSON 走既有 .corrupt 隔离
// 路径返回空会话（同为旧格式兼容现状行为）。
func TestSchemaVersionLegacyLoadTable(t *testing.T) {
	cases := []struct {
		name     string // 用例名兼会话 id
		content  string // 直写盘面的 JSON（无 schemaVersion 字段 = 旧格式）
		wantMsgs int
		wantText string
		corrupt  bool // 损坏 JSON：期望隔离为 .corrupt 并返回空会话
	}{
		{
			name: "legacy_with_messages",
			content: `{
				"id": "legacy_with_messages",
				"channelId": "desktop",
				"title": "旧格式会话",
				"messages": [
					{"ts": 1700000000000000000, "type": "say", "say": "text", "text": "历史消息"}
				],
				"createdAt": "2025-06-01T00:00:00Z",
				"updatedAt": "2025-06-01T00:00:00Z"
			}`,
			wantMsgs: 1,
			wantText: "历史消息",
		},
		{
			name:     "legacy_minimal",
			content:  `{"id": "legacy_minimal", "channelId": "cli"}`,
			wantMsgs: 0,
		},
		{
			name:     "corrupt_json",
			content:  `{ not valid json`,
			corrupt:  true,
			wantMsgs: 0,
		},
	}
	dir := t.TempDir()
	st := NewStore(dir)
	defer st.Close()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, tc.name+".json"), []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write legacy file: %v", err)
			}
			got, err := st.Get(tc.name)
			if err != nil {
				t.Fatalf("legacy session should load: %v", err)
			}
			if got.ID != tc.name {
				t.Fatalf("id lost: got %q, want %q", got.ID, tc.name)
			}
			if len(got.Messages) != tc.wantMsgs {
				t.Fatalf("messages: got %d, want %d", len(got.Messages), tc.wantMsgs)
			}
			if !tc.corrupt && tc.wantText != "" && got.Messages[0].Text != tc.wantText {
				t.Fatalf("legacy message text lost: %q", got.Messages[0].Text)
			}
			// 归一化判别依据：旧格式文件经缺省分支补齐版本（运行态永远是最新
			// 代码语义）。损坏隔离路径提前 return（构造空会话绕过归一化分支），
			// 版本保持 0——无害现状：再落盘时 Store.write 统一重新打标。
			wantVersion := CurrentSchemaVersion
			if tc.corrupt {
				wantVersion = 0
			}
			if snap := got.Snapshot(); snap.SchemaVersion != wantVersion {
				t.Fatalf("schemaVersion: got %d, want %d", snap.SchemaVersion, wantVersion)
			}
			if tc.corrupt {
				if _, err := os.Stat(filepath.Join(dir, tc.name+".json.corrupt")); err != nil {
					t.Fatalf("corrupt file not isolated: %v", err)
				}
			}
		})
	}
}

// TestMessagesRoundTripTable 消息读写 roundtrip（表驱动）：多形态消息
// （空/单条/ask+say+partial 混合/含图片附件）经落盘 → 重载后逐条保真。
func TestMessagesRoundTripTable(t *testing.T) {
	cases := []struct {
		name string
		msgs []message.ClineMessage
	}{
		{"empty", nil},
		{"single", []message.ClineMessage{message.NewSay(message.SayText, "a")}},
		{"mixed", []message.ClineMessage{
			message.NewAsk(message.AskFollowup, "question"),
			message.NewSay(message.SayReasoning, "think..."),
			message.NewSay(message.SayText, "final"),
			{TS: 42, Type: message.TypeSay, Say: message.SayText, Text: "streaming", Partial: true},
		}},
		{"with_images", []message.ClineMessage{
			{TS: 43, Type: message.TypeSay, Say: message.SayImage, Text: "img", Images: []string{"data:image/png;base64,xx"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "sessions")
			st := NewStore(dir)
			s := &Session{ID: tc.name, ChannelID: "desktop"}
			s.AppendMessages(tc.msgs)
			st.Save(s)
			st.FlushAll()
			st.Close()

			st2 := NewStore(dir)
			defer st2.Close()
			got, err := st2.Get(tc.name)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			snap := got.Snapshot()
			if len(snap.Messages) != len(tc.msgs) {
				t.Fatalf("message count: got %d, want %d", len(snap.Messages), len(tc.msgs))
			}
			for i, want := range tc.msgs {
				have := snap.Messages[i]
				if have.Type != want.Type || have.Say != want.Say || have.Ask != want.Ask ||
					have.Text != want.Text || have.Partial != want.Partial {
					t.Fatalf("msg[%d] roundtrip drift:\n got %+v\nwant %+v", i, have, want)
				}
				if len(have.Images) != len(want.Images) {
					t.Fatalf("msg[%d] images: got %d, want %d", i, len(have.Images), len(want.Images))
				}
			}
		})
	}
}

// TestGetMissingSessionIsError 越界边界：Get 不存在的会话 id 返回错误且不落盘。
func TestGetMissingSessionIsError(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), "sessions"))
	defer st.Close()
	if _, err := st.Get("no-such-session"); err == nil {
		t.Fatal("Get missing session should return error")
	}
	if _, err := os.Stat(filepath.Join(st.dir, "no-such-session.json")); !os.IsNotExist(err) {
		t.Fatal("missing session should not be materialized on disk")
	}
}

// TestEmptySessionSummary 边界：空会话（无消息）Summary 不越界——预览行为空、
// 消息数 0（lastMessageSummary 倒扫空切片安全返回）。
func TestEmptySessionSummary(t *testing.T) {
	s := &Session{ID: "blank", ChannelID: "desktop"}
	sum := s.Summary()
	if sum.MessageCount != 0 || sum.LastMessage != "" {
		t.Fatalf("empty summary drift: %+v", sum)
	}
	if got := lastMessageSummary(nil); got != "" {
		t.Fatalf("nil messages summary: got %q", got)
	}
	if got := truncateSummary(strings.Repeat("长", summaryMaxRunes+1)); !strings.HasSuffix(got, "…") {
		t.Fatalf("truncate should append ellipsis beyond %d runes: %q", summaryMaxRunes, got)
	}
}
