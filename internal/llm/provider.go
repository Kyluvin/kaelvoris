// Package llm LLM Provider 抽象层。
//
// P1 先实现 OpenAI 兼容 chat/completions（覆盖 openai/deepseek/doubao-ark/openrouter 等），
// 后续接入 Eino 时以 adapter 包装本接口，保持 Agent 内核不变。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Role 消息角色。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ToolCall LLM 返回的工具调用。
// 序列化为 OpenAI 消息内 tool_calls 元素规范形状：
// {"id":..,"type":"function","function":{"name":..,"arguments":..}}
// （部分严格服务端如 vLLM/Pydantic 校验会拒绝扁平形状）
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

// MarshalJSON 输出 OpenAI 规范的 tool_call 结构。
func (t ToolCall) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}{t.ID, "function", struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{t.Name, t.Arguments}})
}

// Message 对话消息。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"-"`
	Images     []string   `json:"-"`                      // 图片（data URL 或 https URL）；非空时 content 为多模态数组
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant 发起的工具调用
	ToolCallID string     `json:"tool_call_id,omitempty"` // role==tool 时对应调用 ID
}

// MarshalJSON 多模态适配：无图片时 content 为纯文本；有图片时为 OpenAI
// content parts 数组（text + image_url），对齐 vision 兼容接口。
// content 永远序列化（无 omitempty）：OpenAI 规范中 role=tool 的 content 为
// 必填字符串；严格校验的推理后端（vLLM/Pydantic）会把缺失 content 视为
// None 而 400。空结果输出 "" 即合法。
func (m Message) MarshalJSON() ([]byte, error) {
	if len(m.Images) == 0 {
		return json.Marshal(struct {
			Role       string     `json:"role"`
			Content    string     `json:"content"`
			ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
			ToolCallID string     `json:"tool_call_id,omitempty"`
		}{m.Role, m.Content, m.ToolCalls, m.ToolCallID})
	}
	parts := make([]map[string]any, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": img},
		})
	}
	return json.Marshal(struct {
		Role       string           `json:"role"`
		Content    []map[string]any `json:"content"`
		ToolCalls  []ToolCall       `json:"tool_calls,omitempty"`
		ToolCallID string           `json:"tool_call_id,omitempty"`
	}{m.Role, parts, m.ToolCalls, m.ToolCallID})
}

// ToolFunc 工具函数定义（OpenAI function 格式）。
type ToolFunc struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"` // JSONSchema
}

// ToolDef 工具定义。
type ToolDef struct {
	Type     string   `json:"type"` // 恒为 "function"
	Function ToolFunc `json:"function"`
}

// ChatRequest 一次对话请求。
type ChatRequest struct {
	Messages []Message
	Tools    []ToolDef
	// 采样参数（§6.2 数字员工级模型参数）。指针 nil = 使用服务端默认值；
	// 显式指针可表达 temperature=0 等合法零值。
	Temperature *float64
	TopP        *float64
	MaxTokens   *int
}

// StreamDelta 流式增量。
type StreamDelta struct {
	Content   string     // 文本增量
	Reasoning string     // 思考/推理增量（DeepSeek-R1 / OpenAI o 系列等 reasoning_content）
	ToolCalls []ToolCall // 工具调用（流结束后完整回传一次）
	Done      bool       // 流结束
	// Usage 流结束时的真实 token 用量（仅 Done 增量携带；请求开启
	// stream_options.include_usage 且回传 usage 尾包时非 nil）。
	Usage *Usage
}

// Usage 一次请求的真实 token 用量（OpenAI 兼容 usage 对象）。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

	// APIError API 非 200 响应。
	type APIError struct {
		Status int
		Body   string
		// RetryAfter 服务端要求的重试等待时长（2026-10 韧性加固：解析
		// Retry-After 响应头，支持秒数与 HTTP 日期两种形态；未携带/非法 = 0，
		// 引擎退避据此优先采用服务端节奏而非固定 3s×n）。
		RetryAfter time.Duration
	}

	func (e *APIError) Error() string {
		if e.RetryAfter > 0 {
			return fmt.Sprintf("api error %d: %s (retry after %s)", e.Status, e.Body, e.RetryAfter)
		}
		return fmt.Sprintf("api error %d: %s", e.Status, e.Body)
	}

	// parseRetryAfter 解析 Retry-After 响应头（HTTP/1.1 §7.1.3 两种形态）：
	// 秒数（"120"）或 HTTP 日期（"Wed, 21 Oct 2015 07:28:00 GMT"）。
	// 缺失/非法/非正 = 0（调用方走默认退避，不因脏头值恐慌）。
	func parseRetryAfter(v string) time.Duration {
		v = strings.TrimSpace(v)
		if v == "" {
			return 0
		}
		if secs, err := strconv.Atoi(v); err == nil {
			if secs <= 0 {
				return 0
			}
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
		return 0
	}

// Client OpenAI 兼容客户端。
type Client struct {
	BaseURL string // 形如 https://api.deepseek.com（不含 /chat/completions）
	APIKey  string
	Model   string
	HTTP    *http.Client
	// FirstChunkTimeout 首包看门狗：请求发出后迟迟收不到第一帧 SSE 数据
	// 的上限（0 = 默认 180s；<0 = 禁用）。覆盖后端排队无响应/连接假死。
	FirstChunkTimeout time.Duration
	// StreamIdleTimeout 流空闲看门狗：流式过程中相邻两帧数据的最大间隔
	// （0 = 默认 120s；<0 = 禁用）。覆盖连接半开/心跳停滞导致的
	// scanner.Scan() 永久阻塞——任务冻结在 api_req_started 的根因。
	StreamIdleTimeout time.Duration
}

// StreamIdleError 流看门狗触发（首包超时或流空闲超时）。属瞬时错误：
// 引擎侧应自动重试而非终止任务（未完成任务无限轮处理语义）。
type StreamIdleError struct {
	Timeout    time.Duration // 触发时使用的超时阈值
	FirstChunk bool          // true = 首包超时；false = 流中途空闲超时
}

func (e *StreamIdleError) Error() string {
	if e.FirstChunk {
		return fmt.Sprintf("llm 流首包超时：%s 内未收到任何响应数据（连接假死已中止）", e.Timeout)
	}
	return fmt.Sprintf("llm 流空闲超时：%s 内未收到任何流数据（停滞连接已中止）", e.Timeout)
}

// NewClient 创建客户端。
// Timeout 30 分钟仅为绝对兜底：真实的假死治理由 StreamChat 双看门狗承担。
// 旧值 300s 会把健康但缓慢的长流式响应（推理模型长思考）拦腰截断，
// 触发 api_req_failed 卡片阻塞等用户点重试——无人值守时即表现为
// "任务运行 N 轮后假死、状态仍显示运行中"。
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: trimRightSlash(baseURL),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 30 * time.Minute},
	}
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// ---------- SSE 请求/响应结构 ----------

type chatRequestBody struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Tools         []ToolDef      `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type streamChunk struct {
	Choices []streamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"` // stream_options.include_usage 尾包
	Error   *apiErrBody    `json:"error,omitempty"`
}

type streamChoice struct {
	Delta streamDelta `json:"delta"`
}

type streamDelta struct {
	Content   string           `json:"content"`
	Reasoning string           `json:"reasoning_content"` // 思考内容（reasoning 模型）
	ToolCalls []streamToolCall `json:"tool_calls"`
}

type streamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type apiErrBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// toolAcc 工具调用流式累积器（对齐 Zoo-Code NativeToolCallParser 的分片聚合语义）。
type toolAcc struct {
	id   string
	name string
	args string
}

// copyToolDefs ChatRequest → wire 形状的工具定义拷贝（StreamChat/Generate 共用，
// 消除两处逐字段重复循环）。
func copyToolDefs(tools []ToolDef) []ToolDef {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ToolDef, len(tools))
	for i, t := range tools {
		out[i] = ToolDef{Type: "function", Function: t.Function}
	}
	return out
}

// StreamChat 流式对话：文本增量实时 emit；工具调用分片聚合后在流结束前完整 emit 一次。
func (c *Client) StreamChat(ctx context.Context, req ChatRequest, emit func(StreamDelta) error) error {
	body := chatRequestBody{
		Model:         c.Model,
		Messages:      req.Messages,
		Tools:         copyToolDefs(req.Tools),
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		MaxTokens:     req.MaxTokens,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	// 派生流上下文：看门狗可独立于调用方 ctx 中止本次流（解除 scanner 阻塞）。
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	firstTimeout := c.FirstChunkTimeout
	if firstTimeout == 0 {
		firstTimeout = 180 * time.Second
	}
	idleTimeout := c.StreamIdleTimeout
	if idleTimeout == 0 {
		idleTimeout = 120 * time.Second
	}

	// 流看门狗（冻结根因修复）：以最后活动时间为基准计时——首包前用
	// firstTimeout、之后用 idleTimeout；超时即 cancel(sctx) 强制断流，
	// scanner.Scan() 的阻塞读立即报错返回。调用方（引擎）据此走
	// 瞬时错误自动重试路径续跑，任务不再冻结在 api_req_started。
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var gotFirstChunk atomic.Bool
	wdErr := make(chan error, 1)

	// 看门狗 goroutine：先于请求发出启动（首包超时覆盖整个「建连→响应头→
	// 首帧」阶段）；随 defer cancel()（正常结束/父 ctx 取消）退出。
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-ticker.C:
				limit := idleTimeout
				if !gotFirstChunk.Load() {
					limit = firstTimeout
				}
				idle := time.Since(time.Unix(0, lastActivity.Load()))
				if idle > limit {
					kind := "流空闲"
					if !gotFirstChunk.Load() {
						kind = "首包"
					}
					slog.Error("llm 流看门狗触发（已中止假死流）",
						"kind", kind, "idle", idle.Round(time.Second),
						"limit", limit, "model", c.Model, "baseURL", c.BaseURL)
					wdErr <- &StreamIdleError{Timeout: limit, FirstChunk: !gotFirstChunk.Load()}
					cancel() // 关键：关闭底层连接，解除 scanner.Scan() 阻塞
					return
				}
			}
		}
	}()

	httpReq, err := http.NewRequestWithContext(sctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		// 看门狗在 Do 阶段触发（连接/响应头假死）时给出明确错误。
		select {
		case werr := <-wdErr:
			return werr
		default:
		}
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return &APIError{
				Status:     resp.StatusCode,
				Body:       string(b),
				RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			}
		}
	// 响应头已到：转入流空闲计时（首帧重新获得完整 firstTimeout 窗口）。
	lastActivity.Store(time.Now().UnixNano())

	// SSE 解析：按行读取 "data: {...}"
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 4*1024*1024)

	accs := map[int]*toolAcc{} // tool_calls index → 聚合器
	var finalUsage *Usage      // include_usage 尾包的真实 token 用量

	for scanner.Scan() {
		line := scanner.Text()
		lastActivity.Store(time.Now().UnixNano()) // 每帧数据刷新活动时间
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		gotFirstChunk.Store(true)
		data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 忽略心跳/注释行
		}
		if chunk.Error != nil {
			return &APIError{Status: resp.StatusCode, Body: chunk.Error.Message}
		}
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
		}

		for _, ch := range chunk.Choices {
			if ch.Delta.Reasoning != "" {
				if err := emit(StreamDelta{Reasoning: ch.Delta.Reasoning}); err != nil {
					return err
				}
			}
			if ch.Delta.Content != "" {
				if err := emit(StreamDelta{Content: ch.Delta.Content}); err != nil {
					return err
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc := accs[tc.Index]
				if acc == nil {
					acc = &toolAcc{}
					accs[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name += tc.Function.Name
				}
				acc.args += tc.Function.Arguments
			}
		}
	}

	// 看门狗触发优先归因（ctx 取消会让底层读报模糊错误）。
	select {
	case werr := <-wdErr:
		return werr
	default:
	}
	if err := scanner.Err(); err != nil {
		select {
		case werr := <-wdErr:
			return werr
		default:
		}
		return fmt.Errorf("read stream: %w", err)
	}

	// 汇总工具调用（按 index 稳定顺序）
	var calls []ToolCall
	for i := 0; i < len(accs); i++ {
		a, ok := accs[i]
		if !ok || a.name == "" {
			continue
		}
		calls = append(calls, ToolCall{ID: a.id, Name: a.name, Arguments: a.args})
	}
	if len(calls) > 0 {
		if err := emit(StreamDelta{ToolCalls: calls}); err != nil {
			return err
		}
	}
	// 真实用量随 Done 增量回传（服务端未回传 usage 时为 nil，走估算兜底）。
	return emit(StreamDelta{Done: true, Usage: finalUsage})
}

// Generate 非流式对话：一次请求返回完整回复（文本 + 工具调用）。
// 供 Eino ChatModel 适配器与流程编排节点使用。
func (c *Client) Generate(ctx context.Context, req ChatRequest) (*Message, error) {
	body := chatRequestBody{
		Model:       c.Model,
		Messages:    req.Messages,
		Tools:       copyToolDefs(req.Tools),
		Stream:      false,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	// 非流式请求级超时（假死治理对齐 StreamChat 看门狗思路）：condense
	// 摘要等调用方无自带超时，裸 ctx 会挂满 Client 兜底超时（30 分钟）。
	gctx, gcancel := context.WithTimeout(ctx, 10*time.Minute)
	defer gcancel()
	httpReq, err := http.NewRequestWithContext(gctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Status: resp.StatusCode, Body: string(b)}
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content   string           `json:"content"`
				ToolCalls []streamToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error *apiErrBody `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if out.Error != nil {
		return nil, &APIError{Status: resp.StatusCode, Body: out.Error.Message}
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("empty choices")
	}
	msg := &Message{Role: RoleAssistant, Content: out.Choices[0].Message.Content}
	for _, tc := range out.Choices[0].Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return msg, nil
}
