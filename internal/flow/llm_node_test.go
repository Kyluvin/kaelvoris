package flow

// llm 节点模型选择 + 自带提示词 + start 输入变量测试（Dify LLM/开始节点语义对齐）。

import (
	"context"
	"strings"
	"testing"

	"kaelvoris/internal/llm"
)

// TestRunner_LLMModelOverride llm 节点 Step.Model 经 LLMFor 解析专用客户端：
// 模型 A/B 路由到不同 mock 服务，各自收到请求。
func TestRunner_LLMModelOverride(t *testing.T) {
	var gotA, gotB []llm.Message
	srvA := newMockLLM(t, &gotA)
	srvB := newMockLLM(t, &gotB)
	defer srvA.Close()
	defer srvB.Close()

	clients := map[string]*llm.Client{
		"model-a": llm.NewClient(srvA.URL, "k", "model-a"),
		"model-b": llm.NewClient(srvB.URL, "k", "model-b"),
	}
	r := &Runner{
		LLMFor: func(_ context.Context, model string) *llm.Client { return clients[model] },
	}
	f := &Flow{ID: "fm", Name: "model-override", Steps: []Step{
		{Type: StepLLM, Name: "A 节点", Model: "model-a", Template: "走 A"},
		{Type: StepLLM, Name: "B 节点", Model: "model-b", Template: "走 B"},
	}}
	res, err := r.Run(context.Background(), f, map[string]any{}, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(gotA) == 0 || len(gotB) == 0 {
		t.Fatalf("model routing failed: gotA=%d gotB=%d", len(gotA), len(gotB))
	}
	// 最后一条 user 消息 = 节点自带提示词（各自服务收到各自的提示词）。
	if gotA[len(gotA)-1].Content != "走 A" {
		t.Fatalf("srvA last user msg: %+v", gotA[len(gotA)-1])
	}
	if gotB[len(gotB)-1].Content != "走 B" {
		t.Fatalf("srvB last user msg: %+v", gotB[len(gotB)-1])
	}
	// 最终输出 = 最后一个 llm 节点的回复。
	if res.Output != "REPLY:走 B" {
		t.Fatalf("output: %q", res.Output)
	}
}

// TestRunner_LLMModelUnknown 未知模型且无全局 LLM → 明确报错。
func TestRunner_LLMModelUnknown(t *testing.T) {
	r := &Runner{LLMFor: func(_ context.Context, _ string) *llm.Client { return nil }}
	f := &Flow{ID: "fx", Name: "unknown-model", Steps: []Step{
		{Type: StepLLM, Name: "坏节点", Model: "nope"},
	}}
	_, err := r.Run(context.Background(), f, map[string]any{}, false)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want unknown model error, got %v", err)
	}
}

// TestRunner_LLMFallbackGlobal Model 为空回退全局 LLM（旧行为保真）。
func TestRunner_LLMFallbackGlobal(t *testing.T) {
	var got []llm.Message
	srv := newMockLLM(t, &got)
	defer srv.Close()
	r := &Runner{LLM: llm.NewClient(srv.URL, "k", "global")}
	f := &Flow{ID: "ff", Name: "fallback", Steps: []Step{
		{Type: StepTemplate, Role: "user", Template: "问题：{q}"},
		{Type: StepLLM, Name: "默认模型"},
	}}
	if _, err := r.Run(context.Background(), f, map[string]any{"q": "你好"}, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 1 || got[0].Content != "问题：你好" {
		t.Fatalf("global llm not used: %d msgs %+v", len(got), got)
	}
}

// TestRunGraph_LLMNodePrompt 图执行路径：llm 节点自带提示词渲染 + 模型覆盖 + 变量池写回。
func TestRunGraph_LLMNodePrompt(t *testing.T) {
	var got []llm.Message
	srv := newMockLLM(t, &got)
	defer srv.Close()

	r := &Runner{LLM: llm.NewClient(srv.URL, "k", "m")}
	c := &Canvas{
		Nodes: []CanvasNode{
			{ID: "_s", Type: StepStart, Step: Step{Type: StepStart, Name: "topic"}},
			{ID: "L", Type: StepLLM, Step: Step{Type: StepLLM, Name: "提示词节点", Model: "", Template: "讲讲 {topic}"}},
			{ID: "_z", Type: StepEnd, Step: Step{Type: StepEnd}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "_s", Target: "L"},
			{ID: "e2", Source: "L", Target: "_z"},
		},
	}
	f := &Flow{ID: "fg", Name: "graph-llm", Canvas: c}
	res, err := r.RunGraph(context.Background(), f, c, map[string]any{"topic": "eino"}, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// 模型收到的最后一条 user 消息 = 渲染后的提示词。
	if got[len(got)-1].Content != "讲讲 eino" {
		t.Fatalf("prompt msg: %+v", got[len(got)-1])
	}
	if !strings.Contains(res.Output, "REPLY:讲讲 eino") {
		t.Fatalf("output: %q", res.Output)
	}
}

// TestTraceVars_StartNode start 节点输入变量定义进变量清单。
func TestTraceVars_StartNode(t *testing.T) {
	c := &Canvas{
		Nodes: []CanvasNode{
			{ID: "_s", Type: StepStart, Step: Step{Type: StepStart, Name: "topic, score，topic"}},
			{ID: "L", Type: StepLLM, Step: Step{Type: StepLLM, Template: "讲讲 {topic}，{L.output}"}},
			{ID: "_z", Type: StepEnd, Step: Step{Type: StepEnd}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "_s", Target: "L"},
			{ID: "e2", Source: "L", Target: "_z"},
		},
	}
	f := &Flow{ID: "fv", Name: "start-vars", Canvas: c}
	vars := TraceVars(f)
	// topic 去重（start 定义 + 模板反扫），score 来自 start 定义，{L.output} 点分路径过滤。
	if len(vars) != 2 || vars[0] != "topic" || vars[1] != "score" {
		t.Fatalf("vars: %v", vars)
	}
}
