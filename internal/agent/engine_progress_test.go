// engine_progress_test.go 长任务心跳回归（2026-10-10 复盘 R2 内核化）：
//   - 每 N 轮 LLM 请求推送一条 SayProgress 心跳（Partial=true 不落历史）；
//   - 心跳内容含当前 in_progress 待办项；
//   - ProgressEvery<0 关闭心跳；不触阈值时不推送。
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"kaelvoris/internal/llm"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// TestEngine_ProgressHeartbeat 验证心跳触发周期、内容与不入历史语义。
func TestEngine_ProgressHeartbeat(t *testing.T) {
	// 前 2 轮返回普通文本（消耗轮次），第 3 轮 attempt_completion。
	var round atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if round.Add(1) <= 2 {
			streamOnce(w, chunkText("working..."))
			return
		}
		streamOnce(w, chunkTool("call_done", "attempt_completion", `{"result":"ok"}`))
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.ProgressEvery = 2 // 每 2 轮请求心跳一次
	engine.Todos = tools.NewTodoStore()
	engine.Todos.Set([]tools.TodoItem{{Content: "实现心跳推送", Status: "in_progress"}})

	var progressMsgs []message.ClineMessage
	say := func(m message.ClineMessage) {
		if m.Type == message.TypeSay && m.Say == message.SayProgress {
			progressMsgs = append(progressMsgs, m)
		}
	}
	if _, err := engine.Run(context.Background(), "long task", say, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// req=2 触发一次心跳（req=0 首轮不推；req=1 未达周期；req=2 命中）。
	if len(progressMsgs) != 1 {
		t.Fatalf("progress say count = %d, want 1", len(progressMsgs))
	}
	p := progressMsgs[0]
	if !p.Partial {
		t.Error("progress message must be Partial=true (not persisted to history)")
	}
	if !containsString(p.Text, "第 2 轮") || !containsString(p.Text, "实现心跳推送") {
		t.Errorf("progress text missing round/todo: %q", p.Text)
	}
	// 不落会话历史（Partial 帧不进 engine.Messages）。
	for _, m := range engine.Messages {
		if m.Say == message.SayProgress {
			t.Error("SayProgress must not be recorded into Messages (bypass frame)")
		}
	}
}

// TestEngine_ProgressDisabled 验证 ProgressEvery<0 关闭心跳。
func TestEngine_ProgressDisabled(t *testing.T) {
	var round atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if round.Add(1) <= 2 {
			streamOnce(w, chunkText("working..."))
			return
		}
		streamOnce(w, chunkTool("call_done", "attempt_completion", `{"result":"ok"}`))
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.ProgressEvery = -1
	saw := false
	say := func(m message.ClineMessage) {
		if m.Say == message.SayProgress {
			saw = true
		}
	}
	if _, err := engine.Run(context.Background(), "task", say, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if saw {
		t.Error("ProgressEvery<0 must disable heartbeat")
	}
}

// containsString rune 级子串查找（心跳文案含中文多字节）。
func containsString(s, sub string) bool {
	rs, ss := []rune(s), []rune(sub)
	for i := 0; i+len(ss) <= len(rs); i++ {
		match := true
		for j := range ss {
			if rs[i+j] != ss[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
