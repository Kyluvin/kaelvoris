// Agent 引擎端到端测试：httptest 模拟 OpenAI 兼容 SSE 服务，
// 验证 P1 最小闭环：流式解析 → 工具调用 → attempt_completion 结束。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kaelvoris/internal/approval"
	"kaelvoris/internal/checkpoint"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/modes"
	"kaelvoris/internal/tools"
	"kaelvoris/internal/usage"
	"kaelvoris/pkg/message"
)

// sseWrite 输出一条 SSE data 行。
func sse(w http.ResponseWriter, v interface{}) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// chunkText 构造文本增量 chunk。
func chunkText(text string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]interface{}{"content": text}},
		},
	}
}

// chunkTool 构造工具调用增量 chunk（模拟流式 tool_calls 聚合）。
func chunkTool(id, name, args string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]interface{}{
				"tool_calls": []map[string]interface{}{
					{"index": 0, "id": id, "type": "function",
						"function": map[string]interface{}{"name": name, "arguments": args}},
				},
			}},
		},
	}
}

// doneChunk 生成结束标志响应序列（单一响应无限复用，避免耗尽后重试死循环）。
func streamOnce(w http.ResponseWriter, chunk map[string]interface{}) {
	w.Header().Set("Content-Type", "text/event-stream")
	sse(w, chunk)
	sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestEngine_NoToolMistakeCorrection 验证无工具调用时 mistake 纠偏与上限退出。
func TestEngine_NoToolMistakeCorrection(t *testing.T) {
	// 模型始终只回文本、不调用工具。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamOnce(w, chunkText("I will just talk."))
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.MaxRequests = 3
	engine.MistakeLimit = 2

	_, err := engine.Run(context.Background(), "hello", nil, nil)
	if err == nil {
		t.Fatal("expected error: no attempt_completion without tool use")
	}
	if !strings.Contains(err.Error(), "max requests") && !strings.Contains(err.Error(), "mistake") {
		t.Errorf("unexpected error: %v", err)
	}

	// 验证 mistake 纠偏消息已进入会话记录。
	foundCorrection := false
	for _, m := range engine.Messages {
		if m.Type == message.TypeSay && m.Say == message.SayError {
			continue
		}
		if m.Type == message.TypeAsk && m.Ask == message.AskMistakeLimit {
			foundCorrection = true
		}
	}
	if !foundCorrection {
		t.Error("mistake_limit ask not recorded")
	}
}

// TestEngine_RequestBudgetRenewal 验证请求预算耗尽不再硬失败：
//   - onAsk != nil：ask 用户续期（AskMistakeLimit）；拒绝 → 预算错误收尾。
//   - messageResponse 文本并入为补充指令（SayUserFeedback 记录）。
//   - onAsk == nil（CLI -y）：自动续期 + 绝对硬顶（4×maxReq）。
func TestEngine_RequestBudgetRenewal(t *testing.T) {
	ws := t.TempDir()

	runEngine := func(maxReq int, ask func(message.ClineMessage) (message.AskResponse, string)) (string, error, []message.ClineMessage) {
		// 模型永远只调 fs_read（永不 attempt_completion）——驱动续期路径。
		readChunk := chunkTool("call_1", "fs_read", `{"path":"a.txt"}`)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			streamOnce(w, readChunk)
		}))
		defer srv.Close()
		client := llm.NewClient(srv.URL, "test-key", "mock-model")
		reg := tools.NewRegistry()
		must(reg.Register(&tools.ReadFileTool{Workspace: ws}))
		must(reg.Register(&tools.AttemptCompletionTool{}))
		engine := New(client, reg, nil)
		engine.AutoApprove = func(message.ClineMessage) bool { return true }
		engine.MaxRequests = maxReq
		res, err := engine.Run(context.Background(), "long task", nil, ask)
		return res, err, engine.Messages
	}

	// 场景 1：拒绝续期 → 预算错误（req==2 时 ask 一次，拒绝即返回）。
	asked := 0
	_, err, _ := runEngine(2, func(m message.ClineMessage) (message.AskResponse, string) {
		if m.Ask == message.AskMistakeLimit {
			asked++
			return message.ResponseNo, ""
		}
		return message.ResponseNo, ""
	})
	if err == nil || !strings.Contains(err.Error(), "request budget reached (2)") {
		t.Fatalf("expected budget decline error, got: %v", err)
	}
	if asked != 1 {
		t.Errorf("ask count = %d, want 1", asked)
	}

	// 场景 2：批准续期 2 次后拒绝 → 证明批准能续跑（ask 发生在 req=2/4/6，
	// maxReq=2；第 3 次 ask 拒绝 → 预算错误收尾于 req=6）。
	approvals := 0
	_, err, _ = runEngine(2, func(m message.ClineMessage) (message.AskResponse, string) {
		if m.Ask == message.AskMistakeLimit {
			approvals++
			if approvals <= 2 {
				return message.ResponseYes, ""
			}
			return message.ResponseNo, ""
		}
		return message.ResponseNo, ""
	})
	if err == nil || !strings.Contains(err.Error(), "request budget reached (6)") {
		t.Fatalf("expected budget decline after 2 renewals (req=6), got: %v", err)
	}
	if approvals != 3 {
		t.Errorf("ask count = %d, want 3 (approve, approve, decline)", approvals)
	}

	// 场景 3：messageResponse 文本并入后拒绝（补充指令以 SayUserFeedback
	// 记录；maxReq=2，req=2 首次 ask → 文本并入续跑一轮，req=4 再 ask 拒绝）。
	answers := 0
	_, _, msgs := runEngine(2, func(m message.ClineMessage) (message.AskResponse, string) {
		if m.Ask == message.AskMistakeLimit {
			answers++
			if answers == 1 {
				return message.ResponseMessageResponse, "please also check b.txt"
			}
			return message.ResponseNo, ""
		}
		return message.ResponseNo, ""
	})
	if answers != 2 {
		t.Errorf("ask count = %d, want 2", answers)
	}
	foundFollowup := false
	for _, m := range msgs {
		if m.Type == message.TypeSay && m.Say == message.SayUserFeedback &&
			strings.Contains(m.Text, "please also check b.txt") {
			foundFollowup = true
		}
	}
	if !foundFollowup {
		t.Error("messageResponse text not recorded as user feedback")
	}
}

// TestEngine_UsageLimitGracefulStop E1 成本/调用护栏：引擎主循环每轮请求后
// 检查 OverLimit，超限优雅终止（finalResult 保留 + SayCostLimit 事件 + 错误归因）。
// 模型永远只调 fs_read（不结束），护栏按调用次数在第 2 次请求后熔断。
func TestEngine_UsageLimitGracefulStop(t *testing.T) {
	ws := t.TempDir()
	readChunk := chunkTool("call_1", "fs_read", `{"path":"a.txt"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamOnce(w, readChunk)
	}))
	defer srv.Close()
	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.ReadFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))
	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	// 调用次数上限 2（估算路径：usage 尾包缺失时 Record 按字符估算也累加 Requests）。
	engine.Usage = usage.NewWithLimits(0, 0, 0, 0, 2)
	_, err := engine.Run(context.Background(), "long task", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "usage limit reached") {
		t.Fatalf("expected usage limit error, got: %v", err)
	}
	found := false
	for _, m := range engine.Messages {
		if m.Type == message.TypeSay && m.Say == message.SayCostLimit {
			found = true
		}
	}
	if !found {
		t.Error("SayCostLimit event not recorded")
	}
}

// TestEngine_FallbackOnStreamFailure E3 主模型失败自动降级：主模型连续 500
// （瞬时错误重试耗尽）→ 切换备用模型续跑 → 备用模型正常完成工具闭环；
// 降级事件 SayFallback 记录，后续轮持续走备用模型（fallbackActive 锁定）。
func TestEngine_FallbackOnStreamFailure(t *testing.T) {
	// 主模型：始终 500（瞬时错误，自动重试耗尽后触发降级）。
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer mainSrv.Close()
	// 备用模型：直接 attempt_completion 正常收尾。
	var fallbackReqs atomic.Int64
	compChunk := chunkTool("call_1", "attempt_completion", `{"result":"done via fallback"}`)
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackReqs.Add(1)
		streamOnce(w, compChunk)
	}))
	defer fallbackSrv.Close()

	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))
	engine := New(llm.NewClient(mainSrv.URL, "k", "main-model"), reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.StreamRetryLimit = 1 // 快速耗尽主模型重试
	engine.FallbackLLM = llm.NewClient(fallbackSrv.URL, "k", "fallback-model")

	res, err := engine.Run(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatalf("expected fallback to complete task, got: %v", err)
	}
	if !strings.Contains(res, "done via fallback") {
		t.Errorf("result = %q, want fallback completion", res)
	}
	foundFallback := false
	for _, m := range engine.Messages {
		if m.Type == message.TypeSay && m.Say == message.SayFallback {
			foundFallback = true
		}
	}
	if !foundFallback {
		t.Error("SayFallback event not recorded")
	}
	if fallbackReqs.Load() == 0 {
		t.Error("fallback model was never called")
	}
}

// TestEngine_FallbackNotOnDeterministicError E3 降级边界：确定性错误（401）
// 不降级（备用大概率同配置问题），直接进 api_req_failed 路径（onAsk==nil
// 时返回错误）。
func TestEngine_FallbackNotOnDeterministicError(t *testing.T) {
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer mainSrv.Close()
	fallbackCalled := false
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		streamOnce(w, chunkText("should not happen"))
	}))
	defer fallbackSrv.Close()

	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))
	engine := New(llm.NewClient(mainSrv.URL, "k", "main-model"), reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.FallbackLLM = llm.NewClient(fallbackSrv.URL, "k", "fallback-model")

	// 401 确定性错误进 api_req_failed 卡片：onAsk 拒绝应答以退出（既有语义，
	// onAsk==nil 返回 ResponseYes = 重试，会无限循环——确定性错误本就如此）。
	_, err := engine.Run(context.Background(), "task", nil, func(m message.ClineMessage) (message.AskResponse, string) {
		return message.ResponseNo, ""
	})
	if err == nil {
		t.Fatal("expected deterministic error to fail fast")
	}
	if fallbackCalled {
		t.Error("fallback model must not be called on deterministic error (401)")
	}
	for _, m := range engine.Messages {
		if m.Type == message.TypeSay && m.Say == message.SayFallback {
			t.Error("SayFallback must not be emitted on deterministic error")
		}
	}
}

// TestEngine_DegradedContinueAfterRetriesExhausted 第三级兑底（降级续跑）：
// 主模型前 N 次 500（自动重试耗尽）→ 收到降级指引后换思路（直接 attempt_completion
// 说明卡点）→ 任务不弹卡停摆、正常返回；SayFallback 降级事件记录。
func TestEngine_DegradedContinueAfterRetriesExhausted(t *testing.T) {
	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) <= 2 { // 前 2 次 500：耗尽 StreamRetryLimit=1 的自动重试
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		// 降级续跑后：模型换思路——直接收口说明卡点（测试类任务跳过语义）。
		streamOnce(w, chunkTool("call_1", "attempt_completion", `{"result":"LLM 链路故障，跳过本验证步骤"}`))
	}))
	defer srv.Close()

	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))
	engine := New(llm.NewClient(srv.URL, "k", "main-model"), reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.StreamRetryLimit = 1

	res, err := engine.Run(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatalf("expected degraded continue to finish task, got: %v", err)
	}
	if !strings.Contains(res, "跳过本验证步骤") {
		t.Errorf("result = %q, want degraded skip completion", res)
	}
	foundFallback := false
	for _, m := range engine.Messages {
		if m.Type == message.TypeSay && m.Say == message.SayFallback {
			foundFallback = true
		}
	}
	if !foundFallback {
		t.Error("SayFallback (degrade) event not recorded")
	}
	if reqs.Load() < 3 {
		t.Errorf("expected >=3 upstream requests (2 fail + 1 degraded), got %d", reqs.Load())
	}
}

// TestEngine_DegradedOnlyOnce 第三级兑底一次性：降级续跑后模型仍持续 500
// → 不再重复注入（防无限降级循环），直接进 api_req_failed 卡片路径
// （onAsk 拒绝应答退出，验证引擎返回错误）。
func TestEngine_DegradedOnlyOnce(t *testing.T) {
	var degradePrompts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 收到含降级指引的请求后计数（指引以 user 消息注入后续轮载荷）。
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "LLM 链路连续故障") {
			degradePrompts.Add(1)
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))
	engine := New(llm.NewClient(srv.URL, "k", "main-model"), reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.StreamRetryLimit = 1

	_, err := engine.Run(context.Background(), "task", nil, func(m message.ClineMessage) (message.AskResponse, string) {
		return message.ResponseNo, "" // 弹卡后拒绝：退出
	})
	if err == nil {
		t.Fatal("expected final error after degrade exhausted")
	}
	if n := degradePrompts.Load(); n != 1 {
		t.Errorf("degrade prompt injected %d times, want exactly 1", n)
	}
}

// TestEngine_CompletionLoop 完整闭环：模型调用 attempt_completion → 引擎捕获并结束。
func TestEngine_CompletionLoop(t *testing.T) {
	toolChunk := chunkTool("call_1", "attempt_completion", `{"result":"all done"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamOnce(w, toolChunk)
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }

	var sawCompletion bool
	say := func(m message.ClineMessage) {
		// attempt_completion 触发 ask:completion_result（对齐 AGENT_LOOP.md Idle Ask）。
		if m.Type == message.TypeAsk && m.Ask == message.AskCompletionResult {
			sawCompletion = true
		}
	}
	result, err := engine.Run(context.Background(), "finish it", say, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "all done" {
		t.Errorf("result = %q, want %q", result, "all done")
	}
	if !sawCompletion {
		t.Error("completion_result ask not emitted")
	}

	// 会话记录包含 completion ask。
	last := engine.Messages[len(engine.Messages)-1]
	if last.Type != message.TypeAsk || last.Ask != message.AskCompletionResult {
		t.Errorf("last message = %v/%v, want ask/completion_result", last.Type, last.Ask)
	}
}

// TestEngine_ToolExecutionRound 验证普通工具（fs_write）ᛎ᚛</think>与结果回填。
// TestEngine_AutoDecideAskFollowup 验证全AI自动决策（用户需求 2026-10）：
// AutoDecide 开启时 ask_followup_question 不阻塞等用户——引擎记录
// SayAutoDecision 留痕并回填自主决策指令，模型继续推进直至完成。
func TestEngine_AutoDecideAskFollowup(t *testing.T) {
	// 第 1 轮：ask_followup_question（自动决策模式不得阻塞）；第 2 轮：完成。
	askChunk := chunkTool("call_1", "ask_followup_question", `{"question":"Which port should I use?"}`)
	completeChunk := chunkTool("call_2", "attempt_completion", `{"result":"done by myself"}`)
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if round == 0 {
			streamOnce(w, askChunk)
		} else {
			streamOnce(w, completeChunk)
		}
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.AskFollowupTool{}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.AutoDecide = true

	// onAsk 计数器：自动决策模式下 ask_followup 不得走到 onAsk（无阻塞提问）。
	askCalls := 0
	onAsk := func(m message.ClineMessage) (message.AskResponse, string) {
		askCalls++
		return message.ResponseYes, "yes"
	}
	result, err := engine.Run(context.Background(), "choose and finish", nil, onAsk)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "done by myself" {
		t.Errorf("result = %q, want %q", result, "done by myself")
	}
	// 无阻塞提问：ask_followup 不触发 onAsk。
	if askCalls != 0 {
		t.Errorf("onAsk called %d times in full-auto mode, want 0", askCalls)
	}
	// 留痕：SayAutoDecision 已记录（问题原文可见）。
	found := false
	for _, m := range engine.Messages {
		if m.Type == message.TypeSay && m.Say == message.SayAutoDecision {
			if strings.Contains(m.Text, "Which port should I use?") {
				found = true
			}
		}
	}
	if !found {
		t.Error("SayAutoDecision 留痕缺失（问题未记录）")
	}
}

// TestEngine_AutoDecideOffAskFollowup 关闭时仍阻塞（回归保障）：AutoDecide=false
// 时 ask_followup 走原 waitAsk 路径——onAsk 被调用并回填用户答案。
func TestEngine_AutoDecideOffAskFollowup(t *testing.T) {
	askChunk := chunkTool("call_1", "ask_followup_question", `{"question":"Proceed?"}`)
	completeChunk := chunkTool("call_2", "attempt_completion", `{"result":"ok"}`)
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if round == 0 {
			streamOnce(w, askChunk)
		} else {
			streamOnce(w, completeChunk)
		}
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.AskFollowupTool{}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.AutoDecide = false

	askCalls := 0
	onAsk := func(m message.ClineMessage) (message.AskResponse, string) {
		if m.Ask == message.AskFollowup {
			askCalls++
			return message.ResponseMessageResponse, "user answer"
		}
		return message.ResponseYes, ""
	}
	result, err := engine.Run(context.Background(), "ask me", nil, onAsk)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "ok" {
		t.Errorf("result = %q, want %q", result, "ok")
	}
	if askCalls != 1 {
		t.Errorf("onAsk(AskFollowup) called %d times, want 1", askCalls)
	}
}

// TestEngine_AutoDecideMistakeLimit 验证错误上限提问在 AutoDecide 下不阻塞：
// AutoDecide=true 时 mistakes 达上限弹出的 AskMistakeLimit 卡不再等人——
// 留痕（SayAutoDecision）后自动批准续跑（用户需求 2026-12「提问性的都由
// AI 自主决策」：旧实现此处阻塞等人，自动审批模式下无人应答即假死）。
func TestEngine_AutoDecideMistakeLimit(t *testing.T) {
	// 模型永远只回文本不调工具 → mistakes 持续累积触发上限；MaxRequests
	// 设 2 让错误上限（MistakeLimit=1）先于请求预算触发。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamOnce(w, chunkText("just talking"))
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.AutoDecide = true
	engine.MistakeLimit = 1
	engine.MaxRequests = 3

	// onAsk 计数器：错误上限 ask 不得到达桥（不阻塞）。
	askCalls := 0
	onAsk := func(m message.ClineMessage) (message.AskResponse, string) {
		askCalls++
		return message.ResponseNo, ""
	}
	_, err := engine.Run(context.Background(), "keep failing", nil, onAsk)
	if err == nil {
		t.Fatal("expected error (budget exhausted), got nil")
	}
	// 不阻塞等人：onAsk 从未被调用。
	if askCalls != 0 {
		t.Errorf("onAsk called %d times in auto-decide mode, want 0", askCalls)
	}
	// 留痕：SayAutoDecision 已记录。
	found := false
	for _, m := range engine.Messages {
		if m.Type == message.TypeSay && m.Say == message.SayAutoDecision {
			found = true
		}
	}
	if !found {
		t.Error("SayAutoDecision 留痕缺失（错误上限未留痕即自动批准）")
	}
}

// TestEngine_AutoDecideStreamErrorRetry 验证流错误卡片在 AutoDecide 下不阻塞：
// 瞬时错误自动重试耗尽后，旧实现弹 AskAPIReqFailed 卡片阻塞等人；AutoDecide=true
// 时留痕后自动批准重试（等价于用户点「重试」），任务继续推进不假死。
func TestEngine_AutoDecideStreamErrorRetry(t *testing.T) {
	// 前 2 次请求返回 500（瞬时错误，自动重试耗尽后进 AutoDecide 分支），
	// 第 3 次成功返回完成——验证「耗尽→自动批准重试→成功」链路。
	var round int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&round, 1) <= 2 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		streamOnce(w, chunkTool("call_done", "attempt_completion", `{"result":"recovered"}`))
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.AutoDecide = true
	engine.StreamRetryLimit = 1 // 第 1 次自动重试后即进 AutoDecide 分支（缩短用例）

	askCalls := 0
	onAsk := func(m message.ClineMessage) (message.AskResponse, string) {
		if m.Ask == message.AskAPIReqFailed {
			askCalls++
		}
		return message.ResponseYes, ""
	}
	result, err := engine.Run(context.Background(), "flaky stream", nil, onAsk)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "recovered" {
		t.Errorf("result = %q, want %q", result, "recovered")
	}
	// 流错误卡片不得阻塞等人。
	if askCalls != 0 {
		t.Errorf("onAsk(AskAPIReqFailed) called %d times in auto-decide mode, want 0", askCalls)
	}
}

func TestEngine_ToolExecutionRound(t *testing.T) {
	ws := t.TempDir()
	// 第 1 轮：调用 fs_write 写文件；第 2 轮：attempt_completion。
	writeChunk := chunkTool("call_1", "fs_write", `{"path":"out.txt","content":"engine-test"}`)
	completeChunk := chunkTool("call_2", "attempt_completion", `{"result":"written"}`)
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if round == 0 {
			streamOnce(w, writeChunk)
		} else {
			streamOnce(w, completeChunk)
		}
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.WriteFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }

	result, err := engine.Run(context.Background(), "write the file", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "written" {
		t.Errorf("result = %q, want %q", result, "written")
	}
	b, err := os.ReadFile(ws + "/out.txt")
	if err != nil || string(b) != "engine-test" {
		t.Errorf("file not written by tool: %q err=%v", b, err)
	}

	// say tool 事件已记录。
	foundToolSay := false
	for _, m := range engine.Messages {
		if m.Say == message.SayTool && strings.Contains(m.Text, "fs_write") {
			foundToolSay = true
		}
	}
	if !foundToolSay {
		t.Error("say tool message not recorded")
	}
}

// TestEngine_SubtaskDelegation 编排端到端：父任务 new_task 委派 → 子任务 attempt_completion →
// 结果回传父任务 → 父任务完成（对齐 Task.startSubtask + resumeAfterDelegation）。
func TestEngine_SubtaskDelegation(t *testing.T) {
	newTaskChunk := chunkTool("call_1", "new_task",
		`{"mode":"code","message":"write hello.txt with hi"}`)
	parentComplete := chunkTool("call_3", "attempt_completion", `{"result":"parent done via subtask"}`)

	request := 0
	var subtaskSysPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 解析请求体抓取 system 提示词（校验子任务 SUBTASK CONTEXT 注入）。
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if len(body.Messages) > 0 && body.Messages[0].Role == "system" {
			if request == 1 { // 第 2 次请求 = 子任务
				subtaskSysPrompt = body.Messages[0].Content
			}
		}
		switch request {
		case 0: // 父任务：new_task
			streamOnce(w, newTaskChunk)
		case 1: // 子任务：fs_write
			streamOnce(w, chunkTool("call_s1", "fs_write", `{"path":"hello.txt","content":"hi"}`))
		case 2: // 子任务：attempt_completion（子任务结果）
			streamOnce(w, chunkTool("call_s2", "attempt_completion", `{"result":"subtask wrote hello.txt"}`))
		default: // 父任务收尾
			streamOnce(w, parentComplete)
		}
		request++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.NewTaskTool{}))
	must(reg.Register(&tools.WriteFileTool{Workspace: t.TempDir()}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.MaxRequests = 10
	engine.MaxSubtaskDepth = 2

	var sawSubtaskStarted, sawSubtaskFinished bool
	say := func(m message.ClineMessage) {
		switch m.Say {
		case message.SaySubtaskStarted:
			sawSubtaskStarted = true
		case message.SaySubtaskFinished:
			sawSubtaskFinished = true
		}
	}
	result, err := engine.RunInMode(context.Background(), "code", "orchestrate it", false, 0, say, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "parent done via subtask" {
		t.Errorf("result = %q", result)
	}
	if !sawSubtaskStarted || !sawSubtaskFinished {
		t.Errorf("subtask events: started=%v finished=%v", sawSubtaskStarted, sawSubtaskFinished)
	}
	if !strings.Contains(subtaskSysPrompt, "SUBTASK CONTEXT") {
		t.Error("subtask system prompt missing SUBTASK CONTEXT section")
	}
}

// TestEngine_ModeToolFiltering 模式工具过滤：ask 模式下 LLM 调用 fs_write 被拒绝。
func TestEngine_ModeToolFiltering(t *testing.T) {
	// 第 1 轮：ask 模式下越权调用 fs_write；第 2 轮：直接完成。
	writeChunk := chunkTool("call_1", "fs_write", `{"path":"x.txt","content":"no"}`)
	completeChunk := chunkTool("call_2", "attempt_completion", `{"result":"done readonly"}`)
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if round == 0 {
			streamOnce(w, writeChunk)
		} else {
			streamOnce(w, completeChunk)
		}
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.WriteFileTool{Workspace: t.TempDir()}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.Modes = modes.NewManager() // 启用模式系统

	_, err := engine.RunInMode(context.Background(), "ask", "just read", false, 0, nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 验证错误回填消息存在（tool 被拒）。
	foundRejection := false
	for _, m := range engine.Messages {
		if m.Say == message.SayTool && strings.Contains(m.Text, "fs_write") {
			foundRejection = true
		}
	}
	if !foundRejection {
		t.Error("fs_write call should have been attempted then rejected in ask mode")
	}
}

// TestToolDefs_McpPrefixBypassModeFilter MCP 修复回归（对齐 Zoo-Code
// build-tools.ts：MCP 工具定义独立于 native 模式过滤注入）：启用模式系统后
// mcp_ 前缀动态工具必须随请求发给模型——此前被模式白名单过滤掉，模型每轮
// 都收不到 MCP 工具定义，表现为「配置了 MCP 却无法识别、不自动调用」。
func TestToolDefs_McpPrefixBypassModeFilter(t *testing.T) {
	reg := tools.NewRegistry()
	must(reg.Register(&tools.AttemptCompletionTool{}))
	must(reg.Register(&fakeMcpTool{name: "mcp_demo_echo", desc: "echo via mcp"}))

	// allowed = ask 模式组（不含任何 mcp_ 工具）。
	allowed := []string{"attempt_completion"}
	defs := toolDefs(reg, allowed)
	var names []string
	for _, d := range defs {
		names = append(names, d.Function.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "mcp_demo_echo") {
		t.Fatalf("mcp_ tool should bypass mode filter in toolDefs, got defs: %v", names)
	}
}

// fakeMcpTool 测试桩：模拟 mcp.RemoteTool（仅暴露名称/描述/参数）。
type fakeMcpTool struct {
	name, desc string
}

// TestExecTool_UnknownMcpHintsActivation MCP 惰性加载回归：未激活的 mcp_* 工具
// 调用错误必须引导先 mcp_activate（而非裸 unknown tool 让模型空转）；
// MCPLazyHint 为空（非惰性宿主）时维持原语义。
func TestExecTool_UnknownMcpHintsActivation(t *testing.T) {
	e := New(&llm.Client{}, tools.NewRegistry(), nil)
	tc := llm.ToolCall{Name: "mcp_demo_echo", Arguments: "{}"}
	say := func(m message.ClineMessage) {}
	onAsk := func(m message.ClineMessage) (message.AskResponse, string) { return message.ResponseYes, "" }

	// 惰性宿主：错误提示激活路径。
	e.MCPLazyHint = "==== MCP TOOL CATALOG"
	_, _, _, _, _, _, err := e.execTool(context.Background(), tc, say, onAsk, "code", 0, nil)
	if err == nil || !strings.Contains(err.Error(), "mcp_activate") {
		t.Fatalf("unknown mcp tool should hint mcp_activate, got: %v", err)
	}

	// 非惰性宿主：维持原 unknown tool 语义。
	e2 := New(&llm.Client{}, tools.NewRegistry(), nil)
	_, _, _, _, _, _, err = e2.execTool(context.Background(), tc, say, onAsk, "code", 0, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") || strings.Contains(err.Error(), "mcp_activate") {
		t.Fatalf("without lazy hint error should stay plain, got: %v", err)
	}
}

// TestSystemPrompt_MCPLazyHintSection MCPLazyHint 非空时注入目录段，空时不注入。
func TestSystemPrompt_MCPLazyHintSection(t *testing.T) {
	cwd := isolatedPromptCWD(t)
	p := SystemPrompt(PromptOptions{CWD: cwd, MCPLazyHint: "MCP TOOL CATALOG (LAZY LOADING)\n- office"})
	if !strings.Contains(p, "MCP TOOL CATALOG (LAZY LOADING)") || !strings.Contains(p, "- office") {
		t.Fatal("lazy hint should be injected when provided")
	}
	p2 := SystemPrompt(PromptOptions{CWD: cwd})
	if strings.Contains(p2, "MCP TOOL CATALOG") {
		t.Fatal("lazy hint section must be absent when empty")
	}
}

func (f *fakeMcpTool) Name() string        { return f.name }
func (f *fakeMcpTool) Description() string { return f.desc }
func (f *fakeMcpTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (f *fakeMcpTool) Execute(ctx context.Context, args string) (string, error) {
	return "ok", nil
}

// TestEngine_ParallelSubtaskDelegation P2.8 多 Agent 并行协作：单轮多个 new_task
// 并行 fan-out（对齐 Zoo-Code 委派语义 + reopenParentFromDelegation 回填）。
// 并行 fan-out（对齐 Zoo-Code 委派语义 + reopenParentFromDelegation 回填）。
// 验证：① 两个委派真并发ᛎ᚛</think>（活跃重叠观测）；② 结果按委派顺序回填
// tool_result；③ SaySubtaskStarted/Finished 成对触发；④ 第二轮请求携带回填。
func TestEngine_ParallelSubtaskDelegation(t *testing.T) {
	// 父任务第一轮：一条 assistant 消息携带两个 new_task tool_calls（index 0/1）。
	parentDelegate := map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]interface{}{
				"tool_calls": []map[string]interface{}{
					{"index": 0, "id": "call_a", "type": "function",
						"function": map[string]interface{}{"name": "new_task",
							"arguments": `{"mode":"code","message":"A"}`}},
					{"index": 1, "id": "call_b", "type": "function",
						"function": map[string]interface{}{"name": "new_task",
							"arguments": `{"mode":"code","message":"B"}`}},
				},
			}},
		},
	}
	parentComplete := chunkTool("call_z", "attempt_completion", `{"result":"parent done"}`)

	var mu sync.Mutex
	request := 0
	var secondRaw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		req := request
		request++
		if req == 1 {
			secondRaw = b
		}
		mu.Unlock()
		if req == 0 {
			// 父任务：单轮双 new_task。
			w.Header().Set("Content-Type", "text/event-stream")
			sse(w, parentDelegate)
			sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		streamOnce(w, parentComplete)
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.NewTaskTool{}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.MaxRequests = 10
	engine.MaxSubtaskDepth = 3

	// 模拟通道层 SubtaskRunner（app.go runParallelSubtask 的引擎侧契约）：
	// 记录并发重叠峰值，返回确定性结果。
	var trackMu sync.Mutex
	active, peak := 0, 0
	engine.SubtaskRunner = func(ctx context.Context, st tools.SubtaskResult, _ SayFn, _ AskFn, depth int) (string, error) {
		trackMu.Lock()
		active++
		if active > peak {
			peak = active
		}
		trackMu.Unlock()
		time.Sleep(150 * time.Millisecond) // 制造ᛎ᚛</think>重叠窗口
		trackMu.Lock()
		active--
		trackMu.Unlock()
		return "done-" + st.Message, nil
	}

	var started, finished int32
	say := func(m message.ClineMessage) {
		switch m.Say {
		case message.SaySubtaskStarted:
			atomic.AddInt32(&started, 1)
		case message.SaySubtaskFinished:
			atomic.AddInt32(&finished, 1)
		}
	}
	result, err := engine.RunInMode(context.Background(), "code", "fan out", false, 0, say, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "parent done" {
		t.Errorf("result = %q", result)
	}
	if peak < 2 {
		t.Errorf("subtasks did not run in parallel: peak concurrency = %d", peak)
	}
	if started != 2 || finished != 2 {
		t.Errorf("subtask events: started=%d finished=%d, want 2/2", started, finished)
	}
	// 两个委派的 tool_result 回填（对齐 reopenParentFromDelegation 注入格式）。
	// 解析第二轮请求 JSON 后断言（请求体 JSON 中换行已转义，不能直接子串匹配）。
	var second struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(secondRaw, &second); err != nil {
		t.Fatalf("unmarshal second request: %v", err)
	}
	backfills := map[string]bool{}
	for _, m := range second.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "Subtask completed. Result from subtask (code mode):") {
			backfills[m.ToolCallID] = true
			switch {
			case strings.HasSuffix(m.Content, "done-A"):
				backfills["A"] = true
			case strings.HasSuffix(m.Content, "done-B"):
				backfills["B"] = true
			}
		}
	}
	if len(backfills) != 4 { // call_a, call_b 两个 tool_result + A、B 两个结果文本
		t.Errorf("parent follow-up request missing backfills: %v", backfills)
	}
}

// TestThinkSplitter 流式分割内嵌思考块：标签跨 chunk 安全（BUG-2 回归）。
// 标签字面量用码点程序化构造，避免显示/编码层干扰。
func TestThinkSplitter(t *testing.T) {
	openTag := string([]rune{0x3C}) + "think" + string([]rune{0x3E})
	closeTag := string([]rune{0x3C}) + "/think" + string([]rune{0x3E})
	runSplit := func(chunks []string) (text, reason string) {
		var s thinkSplitter
		var tb, rb strings.Builder
		for _, c := range chunks {
			tp, rp := s.feed(c)
			tb.WriteString(tp)
			rb.WriteString(rp)
		}
		tp, rp := s.flush()
		tb.WriteString(tp)
		rb.WriteString(rp)
		return tb.String(), rb.String()
	}
	cases := []struct {
		name   string
		chunks []string
		text   string
		reason string
	}{
		{name: "full tags in one chunk", chunks: []string{openTag + "abc" + closeTag + "answer"}, text: "answer", reason: "abc"},
		{name: "open tag split across chunks", chunks: []string{openTag[:3], openTag[3:] + "abc" + closeTag + "answer"}, text: "answer", reason: "abc"},
		{name: "close tag split across chunks", chunks: []string{openTag + "x", closeTag[:4], closeTag[4:] + "tail"}, text: "tail", reason: "x"},
		{name: "unclosed think flushed as reason", chunks: []string{openTag + "unfinished"}, text: "", reason: "unfinished"},
		{name: "stray angle brackets stay text", chunks: []string{"a < b && c > d ok"}, text: "a < b && c > d ok", reason: ""},
		{name: "false tag prefix degrades to text", chunks: []string{"abc" + openTag[:4], "nkgone"}, text: "abc" + openTag[:4] + "nkgone", reason: ""},
		{name: "multiple think blocks", chunks: []string{openTag + "A" + closeTag + "B" + openTag + "C" + closeTag + "tail"}, text: "Btail", reason: "AC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, reason := runSplit(tc.chunks)
			if text != tc.text {
				t.Errorf("text = %q, want %q", text, tc.text)
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// TestEngine_ContextTruncation P3/B5 端到端：极小上下文窗口 + 多轮工具调用，
// 请求载荷超过触发阈值时滑动窗口截断生效——
//  1. context_truncated 通知已发出；
//  2. API 实际收到的载荷受控（历史消息被隐藏）；
//  3. 完整历史保留在引擎侧（非破坏性）。
func TestEngine_ContextTruncation(t *testing.T) {
	ws := t.TempDir()
	// 前 3 轮：大文本 + fs_write 工具调用 → apiMessages 快速膨胀；
	// 第 4 轮 attempt_completion 收尾。
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if round < 3 {
			sse(w, chunkText(strings.Repeat("pad", 2000))) // ~6KB 文本撑大上下文
			sse(w, chunkTool(fmt.Sprintf("call_%d", round), "fs_write",
				fmt.Sprintf(`{"path":"f%d.txt","content":"x"}`, round)))
		} else {
			sse(w, chunkTool("call_done", "attempt_completion", `{"result":"done after truncation"}`))
		}
		sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.WriteFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	// 窗口 2000 token：预算 = 2000*0.9 - 0 = 1800，单轮 ~1500+ token，
	// 两轮后必触发摘要压缩（keepRecent=1 保留最近一条原文）。
	engine.ContextWindowTokens = 2000
	engine.ReservedOutputTokens = 0
	engine.CondenseTriggerPercent = 90
	engine.CondenseEnabled = true
	engine.CondenseKeepRecent = 1

	var truncatedSeen bool
	say := func(m message.ClineMessage) {
		if m.Say == message.SayContextTruncated {
			truncatedSeen = true
		}
	}
	result, err := engine.Run(context.Background(), "write several files", say, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "done after truncation" {
		t.Errorf("result = %q, want %q", result, "done after truncation")
	}
	if !truncatedSeen {
		t.Error("context_truncated say 未触发（截断未生效）")
	}
}

// 优化3 回归：ToolRepetitionDetector——同一 (tool, args) 连续调用达到上限
// （默认 3）后第 4 次被拒绝并回填纠偏错误；换参数后恢复。
func TestEngine_ToolRepetitionDetector(t *testing.T) {
	ws := t.TempDir()
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if round < 4 {
			// 连续 4 轮完全相同的 fs_read 调用：第 1-3 次放行（count 0,1,2），
			// 第 4 次（count 达 limit 3）被拒绝执行。
			sse(w, chunkTool(fmt.Sprintf("call_%d", round), "fs_read", `{"path":"a.txt"}`))
		} else {
			// 第 5 轮：检测器已重置，换工具收尾。
			sse(w, chunkTool("call_done", "attempt_completion", `{"result":"recovered"}`))
		}
		sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.ReadFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.ToolRepetitionLimit = 3
	engine.MaxRequests = 10

	var blockedSeen bool
	say := func(m message.ClineMessage) {
		if m.Say == message.SayError && strings.Contains(m.Text, "Repeated identical tool call blocked") {
			blockedSeen = true
		}
	}
	result, err := engine.Run(context.Background(), "read the same file repeatedly", say, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "recovered" {
		t.Errorf("result = %q, want %q", result, "recovered")
	}
	if !blockedSeen {
		t.Error("repetition block 未触发（检测器未生效）")
	}
	// 检测语义：第 4 次连续相同（count 达 limit 3）才拒绝，之后重置可恢复。
}

// TestEngine_RepetitionGuidance_FsApplyDiff 同参重发拦截的专属纠偏文案
// （2026-10-05 会话复盘：fs_apply_diff 失败后同参数原样重发居首位，
// 旧文案泛泛的「换方法」无具体动作，模型继续同参重试）。回归：
// 拒绝回填应携带 fs_apply_diff 专属指引（重读 + 禁止同参重发）。
func TestEngine_RepetitionGuidance_FsApplyDiff(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if round < 4 {
			// 同一 SEARCH 块连续重发 4 轮（SEARCH 内容在文件中不存在 →
			// 每次都失败并回填带定位提示的错误）。
			sse(w, chunkTool(fmt.Sprintf("call_%d", round), "fs_apply_diff",
				`{"path":"a.txt","edits":[{"search":"never existed","replace":"x"}]}`))
		} else {
			sse(w, chunkTool("call_done", "attempt_completion", `{"result":"done"}`))
		}
		sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.ApplyDiffTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.ToolRepetitionLimit = 3
	engine.MaxRequests = 10

	var blockedText string
	say := func(m message.ClineMessage) {
		if m.Say == message.SayError && strings.Contains(m.Text, "Repeated identical tool call blocked: fs_apply_diff") {
			blockedText = m.Text
		}
	}
	if _, err := engine.Run(context.Background(), "edit repeatedly", say, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if blockedText == "" {
		t.Fatal("fs_apply_diff 同参重发未被拦截")
	}
	// 验证修复核心：拦截时回填专属纠偏文案（引导重读而非笼统换方法）。
	if !strings.Contains(blockedText, "SEARCH block already failed") ||
		!strings.Contains(blockedText, "fs_read") {
		t.Errorf("纠偏提示缺少 fs_apply_diff 专属指引: %q", blockedText)
	}
}

// TestCanonicalToolKey 规范化标识：等价 JSON（键序不同/空白差异）应归一。
func TestCanonicalToolKey(t *testing.T) {
	a := canonicalToolKey("fs_write", `{"path":"a.txt","content":"x"}`)
	b := canonicalToolKey("fs_write", `{ "content" : "x" , "path" : "a.txt" }`)
	if a != b {
		t.Errorf("canonical keys differ:\n a=%q\n b=%q", a, b)
	}
	if canonicalToolKey("fs_write", `{"path":"a.txt","content":"y"}`) == a {
		t.Error("different args must produce different keys")
	}
	// 解析失败退化为原文拼接（不同原文仍可区分）。
	if canonicalToolKey("fs_read", "{bad") == canonicalToolKey("fs_read", "{worse") {
		t.Error("fallback keys must differ for different raw args")
	}
}

// 优化6 回归：敏感文件写保护——AutoApprove 开启时 .env 写入仍强制走人工审批。
func TestEngine_SensitiveWriteGuard(t *testing.T) {
	ws := t.TempDir()
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if round == 0 {
			// 首轮：写 .env（命中保护清单 → 强制人工审批 → 用户拒绝）。
			sse(w, chunkTool("call_w", "fs_write", `{"path":".env","content":"SECRET=1"}`))
		} else {
			// 被拒后的下一轮：模型改口收尾。
			sse(w, chunkTool("call_done", "attempt_completion", `{"result":"env write denied"}`))
		}
		sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.WriteFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true } // 自动审批开启
	var asked bool
	onAsk := func(m message.ClineMessage) (message.AskResponse, string) {
		asked = true
		return message.ResponseNo, ""
	}
	result, err := engine.Run(context.Background(), "write .env", nil, onAsk)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "env write denied" {
		t.Errorf("result = %q, want %q", result, "env write denied")
	}
	if !asked {
		t.Error("protected write must ask user even with AutoApprove")
	}
	if b, rerr := os.ReadFile(filepath.Join(ws, ".env")); rerr == nil && len(b) > 0 {
		t.Errorf(".env must not be written on rejection, got %q", b)
	}
}

// TestSensitiveWriteCall 单元：仅 fs_write/fs_apply_diff 且路径命中保护清单时触发。
func TestSensitiveWriteCall(t *testing.T) {
	if !sensitiveWriteCall("fs_write", `{"path":".env","content":"x"}`) {
		t.Error(".env fs_write should be sensitive")
	}
	if !sensitiveWriteCall("fs_apply_diff", `{"path":".git/config","edits":[]}`) {
		t.Error(".git/config fs_apply_diff should be sensitive")
	}
	if sensitiveWriteCall("fs_read", `{"path":".env"}`) {
		t.Error("fs_read must never be sensitive")
	}
	if sensitiveWriteCall("fs_write", `{"path":"main.go","content":"x"}`) {
		t.Error("plain source file fs_write must not be sensitive")
	}
}

// 413 修复回归：尾部单条超大工具结果无法被组粒度滑动窗口移除 →
//  1. clampToolResult 在入列前截断超大结果（fs_read 分页提示）；
//  2. 迭代截断 + hardTrimVisible 强制裁剪后，API 实际收到的载荷估算
//     必须落在预算内（模拟 nginx 413 检查）。
func TestEngine_413TailToolResultClamp(t *testing.T) {
	ws := t.TempDir()
	// 构造 200KB 单行文件 → fs_read 单次结果 ~200KB（约 50k token）。
	var b strings.Builder
	b.WriteString(strings.Repeat("A", 200000))
	b.WriteString("\n")
	if err := os.WriteFile(filepath.Join(ws, "huge.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	round := 0
	var visiblePayload int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		visiblePayload = len(body)
		w.Header().Set("Content-Type", "text/event-stream")
		if round == 0 {
			sse(w, chunkTool("call_huge", "fs_read", `{"path":"huge.txt"}`))
		} else {
			sse(w, chunkTool("call_done", "attempt_completion", `{"result":"413 survived"}`))
		}
		sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.ReadFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	// 窗口 8000 token，预算 = 8000*0.9 - 0 = 7200；巨型 fs_read 结果 ~50k token
	// （clamp 后 ~10k）仍超预算 → 迭代截断（无组可隐藏时）→ hardTrim 兜底。
	engine.ContextWindowTokens = 8000
	engine.ReservedOutputTokens = 0
	engine.CondenseTriggerPercent = 90
	engine.MaxRequests = 10

	var hardTrimSeen bool
	say := func(m message.ClineMessage) {
		if m.Say == message.SayContextTruncated && strings.Contains(m.Text, "强制裁剪") {
			hardTrimSeen = true
		}
	}
	result, err := engine.Run(context.Background(), "read the huge file", say, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "413 survived" {
		t.Errorf("result = %q, want %q", result, "413 survived")
	}
	if !hardTrimSeen {
		t.Error("强制裁剪 say 未触发（第三层兜底未生效）")
	}
	// 模拟 nginx 检查：估算可见载荷 token 必须 ≤ 预算 + 余量（含 wire 开销）。
	if visiblePayload > 60*1024 {
		t.Errorf("API payload too large: %d bytes (>60KB indicates 413 not prevented)", visiblePayload)
	}
}

// TestEngine_ContextCondense 摘要压缩主路径端到端（规划板 2026-10）：极小窗口
// + 摘要服务 → 超阈值时 LLM 摘要替换早期历史，最近 N 条原文保留；
// context_condensed 通知与 OnCondense 回调（摘要正文落会话存储入口）均触发。
func TestEngine_ContextCondense(t *testing.T) {
	ws := t.TempDir()
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if round < 3 {
			sse(w, chunkText(strings.Repeat("pad", 2000))) // ~6KB 文本撑大上下文
			sse(w, chunkTool(fmt.Sprintf("call_%d", round), "fs_write",
				fmt.Sprintf(`{"path":"f%d.txt","content":"x"}`, round)))
		} else {
			sse(w, chunkTool("call_done", "attempt_completion", `{"result":"done after condense"}`))
		}
		sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.WriteFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.ContextWindowTokens = 2000
	engine.ReservedOutputTokens = 0
	engine.CondenseTriggerPercent = 90
	engine.CondenseEnabled = true
	// keepRecent=0：极小窗口下阈值在首轮即触发，保留区必须为 0 才有摘要空间
	//（keepRecent>0 的保留语义由 condense 包单测覆盖）。
	engine.CondenseKeepRecent = 0
	engine.Summarizer = func(context.Context, []llm.Message) (string, error) {
		return "早期历史摘要：写入了 f0/f1/f2 三个文件", nil
	}

	condensedSeen := false
	cbRemoved, cbKept := 0, 0
	cbSummary := ""
	engine.OnCondense = func(removed, kept int, summary string) {
		condensedSeen = true
		cbRemoved, cbKept, cbSummary = removed, kept, summary
	}
	say := func(m message.ClineMessage) {
		if m.Say == message.SayContextCondensed && strings.Contains(m.Text, "摘要压缩") {
			condensedSeen = true
		}
	}
	result, err := engine.Run(context.Background(), "write several files", say, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "done after condense" {
		t.Errorf("result = %q, want %q", result, "done after condense")
	}
	if !condensedSeen {
		t.Error("context_condensed say 未触发（摘要压缩未生效）")
	}
	// 摘要正文经 OnCondense 回调交付宿主层（落会话存储的数据源）；
	// say 通知仅含统计摘要，正文不进 say 流。
	if cbSummary == "" || !strings.Contains(cbSummary, "早期历史摘要") {
		t.Errorf("OnCondense 回调摘要正文异常: %q", cbSummary)
	}
	if cbRemoved <= 0 || cbKept < 0 {
		t.Errorf("OnCondense 回调计数异常: removed=%d kept=%d", cbRemoved, cbKept)
	}
}

// TestEngine_CondenseDisabled 总开关关闭：超阈值不再触发摘要/截断压缩，
// 任务仍正常完成（硬裁剪兑底保留）。
func TestEngine_CondenseDisabled(t *testing.T) {
	ws := t.TempDir()
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if round < 3 {
			sse(w, chunkText(strings.Repeat("pad", 2000)))
			sse(w, chunkTool(fmt.Sprintf("call_%d", round), "fs_write",
				fmt.Sprintf(`{"path":"f%d.txt","content":"x"}`, round)))
		} else {
			sse(w, chunkTool("call_done", "attempt_completion", `{"result":"done no condense"}`))
		}
		sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.WriteFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.ContextWindowTokens = 2000
	engine.ReservedOutputTokens = 0
	engine.CondenseTriggerPercent = 90
	engine.CondenseEnabled = false // 设置中关闭压缩
	engine.Summarizer = func(context.Context, []llm.Message) (string, error) {
		return "不应被调用", nil
	}

	condensedSeen := false
	say := func(m message.ClineMessage) {
		if m.Say == message.SayContextCondensed {
			condensedSeen = true
		}
	}
	result, err := engine.Run(context.Background(), "write several files", say, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "done no condense" {
		t.Errorf("result = %q, want %q", result, "done no condense")
	}
	if condensedSeen {
		t.Error("开关关闭时不应触发摘要压缩")
	}
}

// 400 修复回归：流中断 / finish_reason=length 截断会产生残缺 arguments
// （非法 JSON）。严格后端（vLLM/Pydantic）重新解析 tool_calls 内嵌 JSON
// 字符串失败 → 400 "Unterminated string"，且该消息每轮请求都携带 →
// 任务永久卡死。验证：
//  1. 残缺调用被隔离：不执行工具、回填错误 tool_result 引导重试；
//  2. 后续请求载荷中所有 arguments 均为合法 JSON（占位替换生效）。
func TestEngine_MalformedToolArgsQuarantined(t *testing.T) {
	round := 0
	var roundBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		roundBodies = append(roundBodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		if round == 0 {
			// 模拟流中断：arguments 在字符串中间被切断（非法 JSON）。
			sse(w, chunkTool("call_bad", "fs_read", `{"path": "d:/cod`))
		} else {
			sse(w, chunkTool("call_ok", "attempt_completion", `{"result":"recovered"}`))
		}
		sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.ReadFileTool{Workspace: t.TempDir()}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.MaxRequests = 5

	result, err := engine.Run(context.Background(), "read a file", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "recovered" {
		t.Errorf("result = %q, want %q", result, "recovered")
	}
	if round != 2 {
		t.Errorf("round = %d, want 2 (malformed call quarantined, then completion)", round)
	}

	// ① 残缺调用被隔离：SayError 记录存在，且 fs_read 未被执行
	//（工作区为空目录，若执行必然产生错误结果而非直接进入下一轮）。
	foundQuarantine := false
	for _, m := range engine.Messages {
		if m.Say == message.SayError && strings.Contains(m.Text, "Malformed tool arguments") {
			foundQuarantine = true
		}
	}
	if !foundQuarantine {
		t.Error("malformed args quarantine SayError not recorded")
	}

	// ② 第二轮请求载荷：所有 tool_calls arguments 均为合法 JSON。
	var req struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(roundBodies[1]), &req); err != nil {
		t.Fatalf("round-1 payload is not valid JSON: %v", err)
	}
	sawPlaceholder := false
	for _, msg := range req.Messages {
		for _, tc := range msg.ToolCalls {
			if !json.Valid([]byte(tc.Function.Arguments)) {
				t.Errorf("malformed arguments reached API: %q", tc.Function.Arguments)
			}
			if strings.Contains(tc.Function.Arguments, "malformed_arguments") {
				sawPlaceholder = true
			}
		}
	}
	if !sawPlaceholder {
		t.Error("assistant tool_calls with placeholder arguments not present in round-1 payload")
	}
}

// sanitizeToolCallArgs 单元测试：合法参数不动、残缺参数替换为合法占位、
// 空参数归一为 {}。
func TestSanitizeToolCallArgs(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "1", Name: "fs_read", Arguments: `{"path":"a.txt"}`},
			{ID: "2", Name: "fs_read", Arguments: `{"path": "d:/cod`},
			{ID: "3", Name: "shell", Arguments: ""},
		}},
	}
	sanitizeToolCallArgs(msgs)
	if msgs[0].ToolCalls[0].Arguments != `{"path":"a.txt"}` {
		t.Errorf("valid args mutated: %q", msgs[0].ToolCalls[0].Arguments)
	}
	if !json.Valid([]byte(msgs[0].ToolCalls[1].Arguments)) ||
		!strings.Contains(msgs[0].ToolCalls[1].Arguments, "malformed_arguments") {
		t.Errorf("malformed args not repaired: %q", msgs[0].ToolCalls[1].Arguments)
	}
	if msgs[0].ToolCalls[2].Arguments != "{}" {
		t.Errorf("empty args not normalized: %q", msgs[0].ToolCalls[2].Arguments)
	}
}

// TestEngine_CheckpointOnWrite B1 接线端到端：fs_write 成功后检查点被记录，
// attempt_completion 收尾产出 say checkpoint 摘要。
func TestEngine_CheckpointOnWrite(t *testing.T) {
	ws := t.TempDir()
	// 预置已存在文件：验证「修改前内容」快照而非新建语义。
	if err := os.WriteFile(filepath.Join(ws, "cp.txt"), []byte("old-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeChunk := chunkTool("call_1", "fs_write", `{"path":"cp.txt","content":"new-content"}`)
	completeChunk := chunkTool("call_2", "attempt_completion", `{"result":"done"}`)
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if round == 0 {
			streamOnce(w, writeChunk)
		} else {
			streamOnce(w, completeChunk)
		}
		round++
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "mock-model")
	reg := tools.NewRegistry()
	must(reg.Register(&tools.WriteFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))

	engine := New(client, reg, nil)
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.Workspace = ws
	cps := newCheckpointStore(t)
	cps.SetWorkspaceRoot(ws)
	engine.CheckpointStore = cps

	var sawCheckpointSay bool
	say := func(m message.ClineMessage) {
		if m.Say == message.SayCheckpoint {
			sawCheckpointSay = true
		}
	}
	if _, err := engine.Run(context.Background(), "write cp", say, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries := cps.List()
	if len(entries) != 1 {
		t.Fatalf("want 1 checkpoint entry, got %d", len(entries))
	}
	if entries[0].Path != "cp.txt" || entries[0].IsNew {
		t.Errorf("entry mismatch: %+v", entries[0])
	}
	if !sawCheckpointSay {
		t.Error("say checkpoint summary not emitted at completion")
	}
	// 恢复验证：文件回滚为旧内容。
	if _, err := cps.Restore(entries[0].ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(ws, "cp.txt"))
	if string(b) != "old-content" {
		t.Errorf("restore got %q, want old-content", b)
	}
}

// newCheckpointStore 测试辅助：临时目录 Store（workspace 绑定到 ws）。
func newCheckpointStore(t *testing.T) *checkpoint.Store {
	t.Helper()
	return checkpoint.NewStore(t.TempDir(), "test-sess", 0)
}

// ---- D2/D3 引擎集成（第三批对标增强）----

// TestEngine_ApprovalParamRule 验证 DecideEx 引擎集成：参数级规则命中
// allow 直通 / deny 拒绝回填 / 未命中回退 AutoApprove。
func TestEngine_ApprovalParamRule(t *testing.T) {
	ws := t.TempDir()
	// runFlow 每场景独立 mock server（两轮：fs_write → attempt_completion）。
	runFlow := func() (*llm.Client, *httptest.Server) {
		writeChunk := chunkTool("call_1", "fs_write", `{"path":"out.txt","content":"x"}`)
		completeChunk := chunkTool("call_2", "attempt_completion", `{"result":"done"}`)
		round := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if round == 0 {
				streamOnce(w, writeChunk)
			} else {
				streamOnce(w, completeChunk)
			}
			round++
		}))
		return llm.NewClient(srv.URL, "test-key", "mock-model"), srv
	}

	// 场景 1：规则 pattern 命中 out.txt → allow 直通（AutoApprove 关闭仍写入）。
	client, srv := runFlow()
	defer srv.Close()
	reg := tools.NewRegistry()
	must(reg.Register(&tools.WriteFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))
	engine := New(client, reg, nil) // AutoApprove=nil：无规则命中时旧逻辑不弹卡直接执行
	engine.Approval = &approval.Store{Rules: []approval.Rule{
		{Tool: "fs_write", Pattern: "out.txt", Action: approval.ActionAllow},
	}}
	if _, err := engine.Run(context.Background(), "t", nil, nil); err != nil {
		t.Fatalf("rule allow: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "out.txt")); err != nil || string(b) != "x" {
		t.Errorf("file should be written via rule allow: %q %v", b, err)
	}

	// 场景 2：规则 deny → 工具拒绝错误回填，任务仍以 completion 收尾。
	ws2 := t.TempDir()
	client2, srv2 := runFlow()
	defer srv2.Close()
	reg2 := tools.NewRegistry()
	must(reg2.Register(&tools.WriteFileTool{Workspace: ws2}))
	must(reg2.Register(&tools.AttemptCompletionTool{}))
	engine2 := New(client2, reg2, nil)
	engine2.Approval = &approval.Store{Rules: []approval.Rule{
		{Tool: "fs_write", Action: approval.ActionDeny},
	}}
	if _, err := engine2.Run(context.Background(), "t", nil, nil); err != nil {
		t.Fatalf("deny flow should complete with tool error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws2, "out.txt")); !os.IsNotExist(err) {
		t.Error("deny: file must not be written")
	}

	// 场景 3：未命中 → nil store 完全走旧逻辑（AutoApprove=nil → 旧分支直接执行）。
	ws3 := t.TempDir()
	client3, srv3 := runFlow()
	defer srv3.Close()
	reg3 := tools.NewRegistry()
	must(reg3.Register(&tools.WriteFileTool{Workspace: ws3}))
	must(reg3.Register(&tools.AttemptCompletionTool{}))
	engine3 := New(client3, reg3, nil)
	engine3.Approval = nil
	if _, err := engine3.Run(context.Background(), "t", nil, nil); err != nil {
		t.Fatalf("nil store fallback: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(ws3, "out.txt")); err != nil || string(b) != "x" {
		t.Errorf("fallback write: %q %v", b, err)
	}
}

// TestEngine_SkillGrantsRound 验证 D3 当轮授权语义：授权后本轮同工具免审；
// 下一轮（clearSkillGrants 后）恢复审批路径。
func TestEngine_SkillGrantsRound(t *testing.T) {
	// 模型连续两轮调 fs_write（同参），第 3 轮 attempt_completion。
	w1 := chunkTool("c1", "fs_write", `{"path":"g.txt","content":"1"}`)
	w2 := chunkTool("c2", "fs_write", `{"path":"g.txt","content":"2"}`)
	done := chunkTool("c3", "attempt_completion", `{"result":"ok"}`)
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch round {
		case 0:
			streamOnce(w, w1)
		case 1:
			streamOnce(w, w2)
		default:
			streamOnce(w, done)
		}
		round++
	}))
	defer srv.Close()
	ws := t.TempDir()
	reg := tools.NewRegistry()
	must(reg.Register(&tools.WriteFileTool{Workspace: ws}))
	must(reg.Register(&tools.AttemptCompletionTool{}))
	engine := New(llm.NewClient(srv.URL, "k", "m"), reg, nil)
	// AutoApprove=nil（严格模式）：无授权时 fs_write 走 waitAsk，onAsk=nil 自动 yes。
	// 注：本测试验证授权表生命周期而非弹卡行为，用 toolRepetition 容忍同参重写
	//（ToolRepetitionLimit 默认 3，两次不同内容不算重复）。
	engine.GrantSkills([]string{"fs_write"})
	if !engine.skillGranted("fs_write", `{}`) {
		t.Fatal("exact-name grant should match")
	}
	if _, err := engine.Run(context.Background(), "t", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 主循环每轮顶部已清空 → 任务结束后授权表必为空（下一轮恢复审批）。
	if len(engine.SkillGrants) != 0 {
		t.Errorf("SkillGrants should be cleared per round, got %v", engine.SkillGrants)
	}
	// 前缀形态匹配语义单测（不跑引擎）：Name(prefix *)
	e2 := New(llm.NewClient(srv.URL, "k", "m"), reg, nil)
	e2.GrantSkills([]string{"shell(git add *)", "fs_read"})
	if !e2.skillGranted("shell", `{"command":"git add ."}`) {
		t.Error("prefix grant should match shell git add")
	}
	if e2.skillGranted("shell", `{"command":"git push"}`) {
		t.Error("prefix grant must not match git push")
	}
	if e2.skillGranted("fs_write", `{}`) {
		t.Error("ungranted tool must not match")
	}
}

// TestEngine_ExpectArtifactContract G1 工件核验端到端（CCGS Trigger 0
// 「验证工件而非回复」）：声明了 expect 的并行委派，子任务返回流利摘要但
// 未落盘产物 → 父任务收到失败（含缺失路径），子任务 Finished 不触发；
// 产物真实存在 → 正常完成；不声明 expect → 零行为变化。
	func TestEngine_ExpectArtifactContract(t *testing.T) {
		ws := t.TempDir()
		mk := func(delegateArgs string, wantContractFail bool) {
		parentDelegate := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]interface{}{
					"tool_calls": []map[string]interface{}{
						{"index": 0, "id": "call_a", "type": "function",
							"function": map[string]interface{}{"name": "new_task", "arguments": delegateArgs}},
					},
				}},
			},
		}
		parentComplete := chunkTool("call_z", "attempt_completion", `{"result":"parent done"}`)
		request := 0
		var secondRaw []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if request == 0 {
				w.Header().Set("Content-Type", "text/event-stream")
				sse(w, parentDelegate)
				sse(w, map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]interface{}{}}}})
				fmt.Fprint(w, "data: [DONE]\n\n")
				request++
				return
			}
			// 第二轮：父模型收到 tool_result 后收尾；抓取请求体验证回灌内容。
			if b, err := io.ReadAll(r.Body); err == nil {
				secondRaw = b
			}
			streamOnce(w, parentComplete)
		}))
		defer srv.Close()

		reg := tools.NewRegistry()
		must(reg.Register(&tools.NewTaskTool{}))
		must(reg.Register(&tools.AttemptCompletionTool{}))
		engine := New(llm.NewClient(srv.URL, "k", "m"), reg, nil)
		engine.Workspace = ws
		engine.AutoApprove = func(message.ClineMessage) bool { return true }
		engine.MaxRequests = 10
		engine.MaxSubtaskDepth = 3
		// 通道层 mock：子任务「声称完成」但不落盘——验证父任务靠 expect 拦截。
		engine.SubtaskRunner = func(ctx context.Context, st tools.SubtaskResult, _ SayFn, _ AskFn, depth int) (string, error) {
			return "all done, artifacts written (fluent preamble, nothing on disk)", nil
		}

		var finished, contractErr int32
		say := func(m message.ClineMessage) {
			if m.Say == message.SaySubtaskFinished {
				atomic.AddInt32(&finished, 1)
			}
			if m.Say == message.SayError && strings.Contains(m.Text, "return contract unmet") {
				atomic.AddInt32(&contractErr, 1)
			}
		}
		_, err := engine.RunInMode(context.Background(), "code", "fan out", false, 0, say, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
			return
		}

		// 解析第二轮请求体，断言父模型收到的 tool_result（失败原因回灌父任务的
		// 真实路径：引擎把子任务成败以 tool 消息回灌，由父模型自我修复/继续）。
		var second struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(secondRaw, &second); err != nil {
			t.Fatalf("unmarshal second request: %v", err)
			return
		}
		var toolMsg string
		for _, m := range second.Messages {
			if m.Role == "tool" {
				toolMsg = m.Content
			}
		}

			if wantContractFail {
				// ① 声明 expect + 未落盘：失败原因回灌（含缺失路径），Finished 不触发。
				if !strings.Contains(toolMsg, "expected artifacts missing on disk") || !strings.Contains(toolMsg, "reports/plan.md") {
					t.Fatalf("contract violation should backfill missing paths to parent, got tool msg: %q", toolMsg)
				}
				if finished != 0 {
					t.Error("subtask finished event must not fire on contract violation")
				}
				if contractErr == 0 {
					t.Error("missing SayError card for contract violation")
				}
			} else {
				// ② 产物存在 / ③ 未声明 expect：成功回传，零行为变化。
				if !strings.Contains(toolMsg, "Subtask completed") {
					t.Fatalf("delegation should pass, got tool msg: %q", toolMsg)
				}
				if finished != 1 {
					t.Errorf("delegation should finish once, got %d", finished)
				}
			}
		}

		// ① 声明 expect + 产物缺失 → 失败含缺失路径。
		mk(`{"mode":"code","message":"plan","expect":["reports/plan.md"]}`, true)

		// ② 声明 expect + 产物真实存在 → 通过。
		must2(os.MkdirAll(filepath.Join(ws, "reports"), 0o755))
		must2(os.WriteFile(filepath.Join(ws, "reports", "plan.md"), []byte("# plan"), 0o644))
		mk(`{"mode":"code","message":"plan","expect":["reports/plan.md"]}`, false)

		// ③ 不声明 expect → 行为与旧一致。
		mk(`{"mode":"code","message":"plan"}`, false)
	}

// must2 测试辅助：包级错误不吞（G1 契约测试用）。
func must2(err error) {
	if err != nil {
		panic(err)
	}
}
