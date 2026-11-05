// 状态机测试：对齐 Zoo-Code apps/cli/docs/AGENT_LOOP.md 状态推导规则。
package message

import "testing"

func TestDetectState_Empty(t *testing.T) {
	if got := DetectState(nil); got != StateNoTask {
		t.Errorf("DetectState(nil) = %v, want StateNoTask", got)
	}
}

func TestDetectState_Streaming(t *testing.T) {
	msgs := []ClineMessage{
		NewSay(SayText, "hello"),
		func() ClineMessage { m := NewSay(SayText, "hello world"); m.Partial = true; return m }(),
	}
	if got := DetectState(msgs); got != StateStreaming {
		t.Errorf("DetectState = %v, want StateStreaming", got)
	}
}

func TestDetectState_AskTypes(t *testing.T) {
	cases := []struct {
		ask  AskType
		want State
	}{
		{AskFollowup, StateWaitingForInput},
		{AskTool, StateWaitingForInput},
		{AskCommand, StateWaitingForInput},
		{AskCompletionResult, StateIdle},
		{AskAPIReqFailed, StateIdle},
		{AskMistakeLimit, StateIdle},
		{AskAutoApprovalMax, StateIdle},
		{AskResumeTask, StateResumable},
	}
	for _, c := range cases {
		msgs := []ClineMessage{NewAsk(c.ask, "test")}
		if got := DetectState(msgs); got != c.want {
			t.Errorf("DetectState(ask=%s) = %v, want %v", c.ask, got, c.want)
		}
	}
}

func TestDetectState_SayOnly(t *testing.T) {
	msgs := []ClineMessage{NewSay(SayText, "done")}
	if got := DetectState(msgs); got != StateRunning {
		t.Errorf("DetectState = %v, want StateRunning", got)
	}
}
