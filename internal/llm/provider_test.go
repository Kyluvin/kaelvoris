package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 验证 assistant 消息中的 tool_calls 序列化为 OpenAI 规范形状：
// {"id","type":"function","function":{"name","arguments"}}。
// 背景：严格校验后端（vLLM/Pydantic）拒绝扁平 {id,name,arguments} 形状。
func TestToolCallMarshalOpenAIShape(t *testing.T) {
	m := Message{
		Role:    RoleAssistant,
		Content: "calling tool",
		ToolCalls: []ToolCall{
			{ID: "call_1", Name: "fs_read", Arguments: `{"path":"a.go"}`},
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"type":"function"`,
		`"function":{"name":"fs_read","arguments":"{\"path\":\"a.go\"}"}`,
		`"id":"call_1"`,
		`"tool_calls":[`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %s\ngot: %s", want, s)
		}
	}
	// 扁平形状禁止出现（name/arguments 不得在 function 外）。
	var probe struct {
		ToolCalls []map[string]any `json:"tool_calls"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(probe.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(probe.ToolCalls))
	}
	tc := probe.ToolCalls[0]
	if tc["type"] != "function" {
		t.Errorf("tool_calls[0].type = %v, want function", tc["type"])
	}
	fn, ok := tc["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool_calls[0].function missing: %v", tc)
	}
	if fn["name"] != "fs_read" || fn["arguments"] != `{"path":"a.go"}` {
		t.Errorf("function payload wrong: %v", fn)
	}
	if _, exists := tc["name"]; exists {
		t.Error("flat shape leaked: top-level name should not exist")
	}
}

// 多模态消息：content 应为 text + image_url parts 数组。
func TestMessageMultimodalMarshal(t *testing.T) {
	m := Message{
		Role:    RoleUser,
		Content: "看这张图",
		Images:  []string{"data:image/png;base64,AAAA"},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"image_url"`) || !strings.Contains(s, `"text"`) {
		t.Errorf("expected multimodal parts, got: %s", s)
	}
}

// P2.9 思考内容流式解析：SSE delta.reasoning_content 应实时 emit 为
// StreamDelta{Reasoning}，且与 content/tool_calls 互不干扰。
func TestStreamChatReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"choices":[{"delta":{"reasoning_content":"先想"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"再想"}}]}`,
			`{"choices":[{"delta":{"content":"答案文本"}}]}`,
			`{"choices":[{"delta":{}}]}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "k", "reasoner-model")
	var reason, content strings.Builder
	err := client.StreamChat(context.Background(), ChatRequest{}, func(d StreamDelta) error {
		reason.WriteString(d.Reasoning)
		content.WriteString(d.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if reason.String() != "先想再想" {
		t.Errorf("reasoning = %q, want %q", reason.String(), "先想再想")
	}
	if content.String() != "答案文本" {
		t.Errorf("content = %q, want %q", content.String(), "答案文本")
	}
}

// 优化4 回归：stream_options.include_usage 尾包 → Done 增量携带真实用量。
func TestStreamChatUsageBackfill(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 校验请求确实开启了 include_usage。
		var body struct {
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
			t.Error("request must set stream_options.include_usage=true")
		}
		chunks := []string{
			`{"choices":[{"delta":{"content":"hi"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":34,"total_tokens":154}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "k", "mock-model")
	var gotUsage *Usage
	err := client.StreamChat(context.Background(), ChatRequest{}, func(d StreamDelta) error {
		if d.Done && d.Usage != nil {
			gotUsage = d.Usage
		}
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if gotUsage == nil {
		t.Fatal("Done delta missing usage")
	}
	if gotUsage.PromptTokens != 120 || gotUsage.CompletionTokens != 34 || gotUsage.TotalTokens != 154 {
		t.Errorf("usage = %+v, want {120 34 154}", gotUsage)
	}
}

// 冻结根因回归测试 A：流中途停滞（假死连接保持打开但不再发数据）。
// 修复前：scanner.Scan() 永久阻塞 → 引擎冻结在 api_req_started →
// 前端状态永远"运行中"。修复后：流空闲看门狗在 StreamIdleTimeout 内
// cancel 派生 ctx 强制断流，StreamChat 返回 *StreamIdleError。
func TestStreamChatIdleWatchdog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"首帧"}}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 之后永不发数据、永不关闭连接 = 模拟半开假死。
		select {
		case <-r.Context().Done(): // 客户端断开（看门狗 cancel 生效）
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "k", "mock-model")
	client.StreamIdleTimeout = 1200 * time.Millisecond // 测试用短阈值
	client.FirstChunkTimeout = 5 * time.Second

	start := time.Now()
	err := client.StreamChat(context.Background(), ChatRequest{}, func(d StreamDelta) error { return nil })
	elapsed := time.Since(start)

	var idleErr *StreamIdleError
	if !errors.As(err, &idleErr) {
		t.Fatalf("want StreamIdleError, got %v", err)
	}
	if idleErr.FirstChunk {
		t.Errorf("want idle (FirstChunk=false), got first-chunk timeout: %v", idleErr)
	}
	if elapsed > 5*time.Second {
		t.Errorf("watchdog did not fire promptly: %v", elapsed)
	}
}

// 冻结根因回归测试 B：连接假死永不回包（服务端 accept 后挂起）。
// 首包看门狗应在 FirstChunkTimeout 内中止并返回明确错误（而非挂满
// Client 兜底超时 30 分钟）。
func TestStreamChatFirstChunkWatchdog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 必须读完请求体：Go http server 仅在 body 读完（或无 body 残留）时
		// 才启动后台断连检测——否则客户端 cancel 后 r.Context() 永不取消，
		// handler 挂死 → httptest.Server.Close() 阻塞 → 测试超时。
		_, _ = io.Copy(io.Discard, r.Body)
		// 不写响应头/不写体，等客户端断开（看门狗 cancel 生效）。
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "k", "mock-model")
	client.FirstChunkTimeout = 800 * time.Millisecond
	client.StreamIdleTimeout = 5 * time.Second

	start := time.Now()
	err := client.StreamChat(context.Background(), ChatRequest{}, func(d StreamDelta) error { return nil })
	elapsed := time.Since(start)

	var idleErr *StreamIdleError
	if !errors.As(err, &idleErr) {
		t.Fatalf("want StreamIdleError, got %v", err)
	}
	if !idleErr.FirstChunk {
		t.Errorf("want first-chunk timeout, got: %v", idleErr)
	}
	if elapsed > 5*time.Second {
		t.Errorf("watchdog did not fire promptly: %v", elapsed)
	}
}

// 健康流不受看门狗误伤：相邻帧间隔 < StreamIdleTimeout 时正常收流完成。
func TestStreamChatWatchdogNotFiringOnHealthyStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: "+`{"choices":[{"delta":{"content":"块%d"}}]}`+"\n\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(80 * time.Millisecond) // 帧间隔远小于阈值
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "k", "mock-model")
	client.StreamIdleTimeout = 2 * time.Second
	client.FirstChunkTimeout = 2 * time.Second

	var content strings.Builder
	err := client.StreamChat(context.Background(), ChatRequest{}, func(d StreamDelta) error {
		content.WriteString(d.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("healthy stream must not trigger watchdog: %v", err)
	}
	if !strings.Contains(content.String(), "块2") {
		t.Errorf("content = %q, want 块0块1块2", content.String())
	}
}

// BUG-1 回归：role=tool 消息 content 永远序列化（不得因 omitempty 缺失）。
// 严格校验后端（vLLM/Pydantic）把缺失 content 视为 None
// 而拒绝整个请求（400: Input should be a valid string ... input: None）。
func TestToolMessageEmptyContentAlwaysSerialized(t *testing.T) {
	m := Message{Role: RoleTool, Content: "", ToolCallID: "chatcmpl-tool-9a5452e1bba07016"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"content":""`) {
		t.Errorf("empty tool content must serialize as \"content\":\"\", got: %s", s)
	}
	if !strings.Contains(s, `"tool_call_id":"chatcmpl-tool-9a5452e1bba07016"`) {
		t.Errorf("tool_call_id missing: %s", s)
	}
	// assistant 空文本同理不得缺 content 字段（部分后端校验同样严格）。
	b2, _ := json.Marshal(Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "fs_read", Arguments: "{}"}}})
	if !strings.Contains(string(b2), `"content":""`) {
		t.Errorf("assistant content must serialize even when empty: %s", b2)
	}
}
