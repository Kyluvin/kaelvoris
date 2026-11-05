package desktop

// remote_group_test.go 群聊 @提及解析自检（remoteGroupMentionedEmployees 纯逻辑）：
// 2026-12 修复「群发消息没人回」的配套——被 @ 的员工自动应答（复用群任务接力链）。

import (
	"testing"

	"kaelvoris/internal/remote"
)

func TestGroupMentionedEmployees(t *testing.T) {
	grp := &remote.Group{Members: []remote.AgentProfile{
		{ID: "user:remote", Name: "用户", Kind: "user"},
		{ID: "bot:group", Name: "群助手", Kind: "bot"},
		{ID: "emp-a", Name: "张三丰", Kind: "employee"},
		{ID: "emp-b", Name: "张三", Kind: "employee"},
		{ID: "emp-c", Name: "李四", Kind: "employee"},
	}}
	a := &App{}
	// 单提及（长名先匹配，防「张三」吞「张三丰」的子串误命中）
	ids := a.groupMentionedEmployees(grp, "@张三丰 帮我写个周报")
	if len(ids) != 1 || ids[0] != "emp-a" {
		t.Fatalf("got %v, want [emp-a]", ids)
	}
	// 短名提及（名字后接空白 = 边界成立）
	if ids = a.groupMentionedEmployees(grp, "问一下 @张三 今天安排"); len(ids) != 1 || ids[0] != "emp-b" {
		t.Fatalf("got %v, want [emp-b]", ids)
	}
	// 多提及：按消息中出现顺序无关，按成员遍历序去重
	ids = a.groupMentionedEmployees(grp, "@李四 @张三 一起接力")
	if len(ids) != 2 {
		t.Fatalf("got %v, want 2 ids", ids)
	}
	// 无提及
	if ids = a.groupMentionedEmployees(grp, "大家好"); ids != nil {
		t.Fatalf("got %v, want nil", ids)
	}
	// 空文本/空群
	if ids = a.groupMentionedEmployees(grp, ""); ids != nil {
		t.Fatalf("empty text: got %v, want nil", ids)
	}
	if ids = a.groupMentionedEmployees(nil, "@张三"); ids != nil {
		t.Fatalf("nil group: got %v, want nil", ids)
	}
}
