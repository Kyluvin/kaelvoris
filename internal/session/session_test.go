package session

import (
	"testing"

	"kaelvoris/pkg/message"
)

// 回归（回复重复修复）：引擎定型顺序 partials → api_req_finished → 定型消息，
// partial 被隔断后定型消息必须清理历史中同类型 partial（旧实现只认尾部相邻，
// 导致同段回复在会话历史存两份 → 回放/实时重复渲染）。
func TestAppendMergedFinalizesDistantPartial(t *testing.T) {
	s := &Session{ID: "t1"}
	s.AppendMerged(message.NewSay(message.SayReasoning, "think...")) // 0
	s.Messages[0].Partial = true
	s.AppendMerged(message.NewSay(message.SayText, "hello")) // 1
	s.Messages[1].Partial = true
	s.AppendMerged(message.NewSay(message.SayAPIReqFinished, "{}")) // 2 隔断帧
	// 定型 reasoning：应替换掉 index 0 的 partial（隔断了也要清）。
	s.AppendMerged(message.NewSay(message.SayReasoning, "think... final"))
	// 定型 text：应替换掉 index 1 的 partial。
	s.AppendMerged(message.NewSay(message.SayText, "hello final"))

	// 定型消息移除同类型 partial 后按引擎发射顺序追加（api_req_finished →
	// reasoning_final → text_final），与实时事件流顺序一致，回放渲染不重复。
	msgs := s.Messages
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (api_finished, reasoning_final, text_final), got %d: %+v", len(msgs), msgs)
	}
	for i, m := range msgs {
		if m.Partial {
			t.Errorf("msg[%d] %s still partial — duplicated streaming frame survived", i, m.Say)
		}
	}
	if msgs[0].Say != message.SayAPIReqFinished {
		t.Errorf("msg[0] = %+v, want api_req_finished intact", msgs[0])
	}
	if msgs[1].Say != message.SayReasoning || msgs[1].Text != "think... final" {
		t.Errorf("msg[1] = %+v, want finalized reasoning", msgs[1])
	}
	if msgs[2].Text != "hello final" {
		t.Errorf("msg[2] = %+v, want finalized text", msgs[2])
	}
}

// 同类型 partial 已被定型替换（闭合周期）后，后续同类型定型消息不再向前越界清理。
func TestAppendMergedClosedCycleUntouched(t *testing.T) {
	s := &Session{ID: "t2"}
	s.AppendMerged(message.NewSay(message.SayText, "round1")) // 定型历史
	s.Messages[0].Partial = false
	s.AppendMerged(message.NewSay(message.SayText, "round2")) // 定型历史
	if len(s.Messages) != 2 {
		t.Fatalf("want 2, got %d", len(s.Messages))
	}
}
