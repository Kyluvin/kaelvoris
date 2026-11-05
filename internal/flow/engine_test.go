package flow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kaelvoris/internal/llm"
)

// reqMessage mock 侧的请求消息解码结构（llm.Message.Content 由自定义
// MarshalJSON 输出且 json:"-"，不能直接 Unmarshal，故独立定义）。
type reqMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// newMockLLM 起一个 httptest 模拟 OpenAI /chat/completions（非流式），
// 回显最后一条 user 消息并断言收到的消息形状。
func newMockLLM(t *testing.T, gotMsgs *[]llm.Message) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []reqMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if gotMsgs != nil {
			// 语义：记录"最后一次请求"收到的消息（多次请求时覆盖）。
			*gotMsgs = (*gotMsgs)[:0]
			for _, m := range body.Messages {
				*gotMsgs = append(*gotMsgs, llm.Message{Role: m.Role, Content: m.Content})
			}
		}
		last := "ok"
		for i := len(body.Messages) - 1; i >= 0; i-- {
			if body.Messages[i].Role == "user" {
				last = body.Messages[i].Content
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": 1700000000,
			"model":   "mock",
			"choices": []map[string]any{
				{"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": "REPLY:" + last}},
			},
		})
	}))
}

// TestRunner_TemplateRender 模板步骤：FString {var} 渲染 + trace 校验。
func TestRunner_TemplateRender(t *testing.T) {
	f := &Flow{
		ID:   "f1",
		Name: "tpl",
		Steps: []Step{
			{Type: StepTemplate, Role: "system", Name: "sys", Template: "你是 {who} 助手。"},
			{Type: StepTemplate, Role: "user", Name: "q", Template: "问题：{question}"},
		},
	}
	var traces []StepTrace
	r := &Runner{OnStep: func(tr StepTrace) { traces = append(traces, tr) }}
	res, err := r.Run(context.Background(), f, map[string]any{"who": "测试", "question": "你是谁"}, true)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.DryRun != true {
		t.Fatalf("expected dryRun=true")
	}
	// dry-run 无 llm 步骤时 output 为空
	if len(res.Steps) != 2 {
		t.Fatalf("want 2 traces, got %d: %+v", len(res.Steps), res.Steps)
	}
	if res.Steps[0].Render != "你是 测试 助手。" {
		t.Fatalf("template render 1: %q", res.Steps[0].Render)
	}
	if res.Steps[1].Render != "问题：你是谁" {
		t.Fatalf("template render 2: %q", res.Steps[1].Render)
	}
	if len(traces) != 2 {
		t.Fatalf("OnStep not called: %d", len(traces))
	}
}

// TestRunner_TemplateMissingVar 缺变量时 FString 渲染失败要报错。
func TestRunner_TemplateMissingVar(t *testing.T) {
	f := &Flow{ID: "f2", Name: "bad", Steps: []Step{
		{Type: StepTemplate, Role: "user", Template: "缺少 {nope}"},
	}}
	r := &Runner{}
	_, err := r.Run(context.Background(), f, map[string]any{}, false)
	if err == nil || !strings.Contains(err.Error(), "模板渲染失败") {
		t.Fatalf("want render error, got %v", err)
	}
}

// TestRunner_FullChain 模板→LLM 完整 Chain：httptest 模拟模型，
// 断言 Eino Chain 顺序执行、上下文正确组装（system+user）、输出回传。
func TestRunner_FullChain(t *testing.T) {
	var gotMsgs []llm.Message
	srv := newMockLLM(t, &gotMsgs)
	defer srv.Close()

	f := &Flow{
		ID:   "f3",
		Name: "chain",
		Steps: []Step{
			{Type: StepTemplate, Role: "system", Name: "系统提示", Template: "你是 {role}。"},
			{Type: StepTemplate, Role: "user", Name: "用户问题", Template: "总结：{topic}"},
			{Type: StepLLM, Name: "模型"},
		},
	}
	r := &Runner{LLM: llm.NewClient(srv.URL, "test-key", "mock-model")}
	res, err := r.Run(context.Background(), f, map[string]any{"role": "测试助手", "topic": "eino 编排"}, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Output != "REPLY:总结：eino 编排" {
		t.Fatalf("output: %q", res.Output)
	}
	if res.Duration <= 0 {
		t.Logf("duration: %dms", res.Duration)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("want 3 traces, got %d", len(res.Steps))
	}
	if res.Steps[2].Type != StepLLM || res.Steps[2].Err != "" {
		t.Fatalf("llm trace: %+v", res.Steps[2])
	}
	// 断言到达模型的上下文：system + user 两条消息
	if len(gotMsgs) != 2 {
		t.Fatalf("model got %d msgs: %+v", len(gotMsgs), gotMsgs)
	}
	if gotMsgs[0].Role != "system" || gotMsgs[0].Content != "你是 测试助手。" {
		t.Fatalf("msg0: %+v", gotMsgs[0])
	}
	if gotMsgs[1].Role != "user" || gotMsgs[1].Content != "总结：eino 编排" {
		t.Fatalf("msg1: %+v", gotMsgs[1])
	}
}

// TestRunner_MultiLLM 两个 llm 步骤串联：第二步应看到第一步的 assistant 回复。
func TestRunner_MultiLLM(t *testing.T) {
	var gotMsgs []llm.Message
	srv := newMockLLM(t, &gotMsgs)
	defer srv.Close()

	f := &Flow{
		ID:   "f4",
		Name: "two-llm",
		Steps: []Step{
			{Type: StepTemplate, Role: "user", Template: "开始：{x}"},
			{Type: StepLLM, Name: "第一轮"},
			{Type: StepLLM, Name: "第二轮"},
		},
	}
	r := &Runner{LLM: llm.NewClient(srv.URL, "k", "m")}
	res, err := r.Run(context.Background(), f, map[string]any{"x": "A"}, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// 第二轮模型的输入 = user 消息 + 第一轮 assistant 回复
	if len(gotMsgs) != 2 {
		t.Fatalf("2nd llm got %d msgs: %+v", len(gotMsgs), gotMsgs)
	}
	if gotMsgs[1].Role != "assistant" || gotMsgs[1].Content != "REPLY:开始：A" {
		t.Fatalf("assistant ctx: %+v", gotMsgs[1])
	}
	// mock 回显最后一条 user 消息，第二轮输出与第一轮相同（符合回显语义）。
	if res.Output != "REPLY:开始：A" {
		t.Fatalf("output: %q", res.Output)
	}
}

// TestTraceVars 变量提取去重。
func TestTraceVars(t *testing.T) {
	f := &Flow{Steps: []Step{
		{Type: StepTemplate, Template: "{a} 和 {b} 与 {a}"},
		{Type: StepLLM},
	}}
	vars := TraceVars(f)
	if len(vars) != 2 || vars[0] != "a" || vars[1] != "b" {
		t.Fatalf("vars: %v", vars)
	}
}
