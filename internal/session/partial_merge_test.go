package session

import (
	"testing"

	"kaelvoris/pkg/message"
)

// 复盘缺陷 1 根因验证：流式 partial 帧（截断文本）被定型帧替换后，会话历史
// 只保留完整文本（回放/续聊读到的不是截断残留）。引擎序列 partial(截断) →
// api_req_finished → 定型(全文)；多帧累计覆盖同一槽位不堆积。
// remote 路径（runRemoteTask / runParallelSubtask）经 makeSayBridge 与桌面共用
// 本合并逻辑，此处直接验证合并结果（落盘快照 = Snapshot().Messages）。
func TestAppendMergedPartialReplacedByFinalText(t *testing.T) {
	s := &Session{ID: "t3"}
	// 第 1 轮：3 帧 partial 累计 + 定型全文。
	s.AppendMerged(message.NewSay(message.SayReasoning, "思"))
	s.Messages[len(s.Messages)-1].Partial = true
	s.AppendMerged(message.NewSay(message.SayReasoning, "思考中"))
	s.Messages[len(s.Messages)-1].Partial = true
	s.AppendMerged(message.NewSay(message.SayReasoning, "思考中..."))
	s.Messages[len(s.Messages)-1].Partial = true
	s.AppendMerged(message.NewSay(message.SayAPIReqFinished, "{}")) // 隔断帧
	// 定型全文到达：应清掉前面的 partial 并追加定型版。
	s.AppendMerged(message.NewSay(message.SayReasoning, "思考中...（完整）"))
	if len(s.Messages) != 2 {
		t.Fatalf("want 2 (api_finished + finalized reasoning), got %d: %+v", len(s.Messages), s.Messages)
	}
	if s.Messages[1].Partial || s.Messages[1].Text != "思考中...（完整）" {
		t.Fatalf("finalized reasoning 应为完整文本: %+v", s.Messages[1])
	}
	// 快照落盘形态：无 partial 残留（复现"流式帧只记录截断文本"缺陷的修复验证）。
	snap := s.Snapshot()
	for i, m := range snap.Messages {
		if m.Partial {
			t.Errorf("snapshot msg[%d] %s 仍为 partial（截断残留）", i, m.Say)
		}
	}
	// 第 2 轮同型周期：不误删第 1 轮定型历史（闭合周期保护）。
	s.AppendMerged(message.NewSay(message.SayReasoning, "第二轮思考"))
	s.Messages[len(s.Messages)-1].Partial = true
	s.AppendMerged(message.NewSay(message.SayAPIReqFinished, "{}"))
	s.AppendMerged(message.NewSay(message.SayReasoning, "第二轮思考（完整）"))
	if len(s.Messages) != 4 {
		t.Fatalf("want 4, got %d: %+v", len(s.Messages), s.Messages)
	}
	for i, m := range s.Messages {
		if m.Partial {
			t.Errorf("msg[%d] %s 仍为 partial（截断残留）", i, m.Say)
		}
	}
}
