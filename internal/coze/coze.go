// Package coze Coze Studio（扣子开源版）OpenAPI 适配器（纯 net/http，无 SDK 依赖）。
//
// 职责边界（接口最小化，对齐项目"外部能力以适配器桥接为纯 Go 实现"约定）：
//   - 封装 Coze Studio 自部署实例的 OpenAPI 调用：工作流执行（同步/流式）、
//     对话（chat v3 SSE 流式）、PAT 鉴权。
//   - 仅协议适配：不持有状态、不落盘（配置由 desktop 层持久化 ~/.kaelvoris/coze.json）。
//
// 对应文档：Coze Studio 开源版 OpenAPI（同 SaaS 扣子协议）：
//   - POST /v1/workflow/run          执行工作流（同步，一次性返回结果）
//   - POST /v1/workflow/stream_run   执行工作流（流式 SSE）
//   - POST /v3/chat                  发起对话（SSE 流式，chat v3 协议）
//
// 自部署实例 API 基址即 Studio 地址（如 http://localhost:8888），
// 鉴权用个人访问令牌 PAT（Studio 界面「扣子 Studio PAT」页创建）。
package coze

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client Coze OpenAPI 客户端。
type Client struct {
	baseURL string // Studio 实例基址（如 http://localhost:8888），末尾斜杠归一
	pat     string // 个人访问令牌（Authorization: Bearer <pat>）
	http    *http.Client
}

// New 创建客户端（baseURL 为 Studio 实例地址；pat 为个人访问令牌）。
func New(baseURL, pat string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return &Client{
		baseURL: baseURL,
		pat:     strings.TrimSpace(pat),
		http:    &http.Client{Timeout: 0}, // SSE 长流不设总超时；由 ctx 控制
	}
}

// Ping 探活：对首页发 GET（Studio Web 可达即通）。
// 内部叠加 pingTimeout 兜底超时（调用方传 context.Background() 也不会无限挂起）；
// PAT 合法性不在此校验——首次真实调用时由 API 错误体现。
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	url := c.baseURL + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("构造探活请求失败: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接 Coze Studio（%s）: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("Coze Studio 响应异常（HTTP %d）", resp.StatusCode)
	}
	return nil
}

// postJSON 发起 JSON POST（带 PAT 鉴权），返回原始响应体。
func (c *Client) postJSON(ctx context.Context, path string, body interface{}) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Coze API 失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.apiError(resp.StatusCode, data)
	}
	return data, nil
}

// auth 注入 PAT 鉴权头（自部署与 SaaS 同协议：Authorization: Bearer <pat>）。
func (c *Client) auth(req *http.Request) {
	if c.pat != "" {
		req.Header.Set("Authorization", "Bearer "+c.pat)
	}
}

// apiError 解析 Coze 错误响应（{"code":xxx,"msg":"..."}）为 Go error。
func (c *Client) apiError(status int, body []byte) error {
	var e struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &e); err == nil && (e.Code != 0 || e.Msg != "") {
		return fmt.Errorf("Coze API 错误（HTTP %d code %d）: %s", status, e.Code, e.Msg)
	}
	clip := string(body)
	if len(clip) > 200 {
		clip = clip[:200] + "…"
	}
	return fmt.Errorf("Coze API 错误（HTTP %d）: %s", status, clip)
}

// ---------- 工作流执行（同步） ----------

// WorkflowResult 工作流同步执行结果。
type WorkflowResult struct {
	Data    string            `json:"Data"`    // 结束节点输出（JSON 字符串，结构由工作流定义）
	ExecTime string           `json:"exec_time,omitempty"` // 服务端执行耗时（ms）
	DebugURL string           `json:"debug_url,omitempty"` // 试运行可视化调试链接（有效期 7 天）
}

// RunWorkflow 同步执行已发布的工作流。
// workflowID：工作流 ID（Studio 编排页 URL 中 workflow 参数后的数字）；
// parameters：开始节点入参（map 直接作为 parameters 字段值）。
func (c *Client) RunWorkflow(ctx context.Context, workflowID string, parameters map[string]any) (*WorkflowResult, error) {
	if strings.TrimSpace(workflowID) == "" {
		return nil, fmt.Errorf("工作流 ID 不能为空")
	}
	body := map[string]any{"workflow_id": workflowID}
	if len(parameters) > 0 {
		body["parameters"] = parameters
	}
	data, err := c.postJSON(ctx, "/v1/workflow/run", body)
	if err != nil {
		return nil, err
	}
	var r WorkflowResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("解析工作流结果失败: %w", err)
	}
	return &r, nil
}

// ---------- 工作流执行（流式 SSE） ----------

// StreamEvent 工作流/对话流式事件（SSE 帧）。
// Event 为 SSE event 行（message/done/error/delta...）；Data 为该帧 data 行原文。
type StreamEvent struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

// RunWorkflowStream 流式执行工作流：逐帧回调 onEvent（含输出节点的增量文本）。
// 返回前阻塞至流结束（Done/error/连接断开/ctx 取消）。
func (c *Client) RunWorkflowStream(ctx context.Context, workflowID string, parameters map[string]any, onEvent func(StreamEvent) error) error {
	return c.postStream(ctx, "/v1/workflow/stream_run", map[string]any{
		"workflow_id": workflowID,
		"parameters":  parameters,
	}, onEvent)
}

// ---------- 对话（chat v3 SSE） ----------

// ChatMessage 对话消息（历史/本次输入）。
type ChatMessage struct {
	Role        string `json:"role"`                   // user | assistant
	ContentType string `json:"content_type"`           // text（当前仅文本）
	Content     string `json:"content"`
	Type        string `json:"type,omitempty"`         // question | answer（chat v3 additional_messages 语义）
}

// ChatRequest chat v3 对话入参。
type ChatRequest struct {
	BotID      string        `json:"bot_id"`                 // 智能体 ID（必填）
	UserID     string        `json:"user_id"`                // 终端用户标识（用于会话隔离）
	Additional []ChatMessage `json:"additional_messages"`    // 历史消息 + 本次问题（最后一条须为 user）
	AutoSave   bool          `json:"auto_save_history"`      // 是否保存历史（true = 服务端 conversation 托管）
}

// chatV3Event chat v3 SSE data 载荷（event 字段区分 message/done/error/conversation.*）。
type chatV3Event struct {
	Event string `json:"event"`
	Data  struct {
		Message struct {
			Type    string `json:"type"`    // answer | follow_up | verbose | function_call | tool_output...
			Content string `json:"content"` // 文本/JSON（verbose 为 JSON 字符串）
		} `json:"message"`
		ConversationID string `json:"conversation_id"`
		LastMsgError   string `json:"last_error,omitempty"`
		Code           int    `json:"code,omitempty"`
		Msg            string `json:"msg,omitempty"`
	} `json:"data"`
}

// ChatStream 发起 chat v3 流式对话：逐帧回调 onEvent。
// 每帧 onEvent 收到原始 StreamEvent（Event=event 字段，Data=data 行原文）；
// 调用方按需解析（桌面层负责把 answer 增量聚合为消息渲染）。
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, onEvent func(StreamEvent) error) error {
	if strings.TrimSpace(req.BotID) == "" {
		return fmt.Errorf("智能体 ID（bot_id）不能为空")
	}
	if req.Additional == nil || len(req.Additional) == 0 {
		return fmt.Errorf("对话消息不能为空")
	}
	return c.postStream(ctx, "/v3/chat", req, onEvent)
}

// ---------- SSE 基础读流 ----------

// postStream 发起 SSE POST 并逐帧分发（SSE 行协议：event: xxx / data: xxx / 空行分帧）。
//   - 非 200：读完整响应体按 JSON 错误解析（不进入流式路径）。
//   - 帧=连续的 event 行 + data 行，以空行结束；只包含 event 或只包含 data 亦成帧。
func (c *Client) postStream(ctx context.Context, path string, body interface{}, onEvent func(StreamEvent) error) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Coze API 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return c.apiError(resp.StatusCode, data)
	}
	// SSE 逐行解析（bufio.Scanner 默认 64KB 行上限，SSE 帧可能携带大块 JSON，放大缓冲）。
	reader := bufio.NewReaderSize(resp.Body, 256*1024)
	var (
		curEvent string
		curData  strings.Builder
	)
	flush := func() error {
		if curEvent == "" && curData.Len() == 0 {
			return nil
		}
		ev := StreamEvent{Event: curEvent, Data: curData.String()}
		curEvent = ""
		curData.Reset()
		return onEvent(ev)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// 流以最后一帧收尾（无结尾空行）也要 flush。
				line = strings.TrimRight(line, "\r\n")
				if line == "" {
					return flush()
				}
				// EOF 前还有内容：按最后一行处理后 flush。
				parseSSELine(line, &curEvent, &curData)
				return flush()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("读取流失败: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		parseSSELine(line, &curEvent, &curData)
	}
}

// parseSSELine 解析单行 SSE（event:/data: 前缀；其余行忽略——注释/心跳）。
func parseSSELine(line string, curEvent *string, curData *strings.Builder) {
	switch {
	case strings.HasPrefix(line, "event:"):
		*curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		if curData.Len() > 0 {
			curData.WriteString("\n") // 多 data 行拼接（SSE 规范）
		}
		curData.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
}

// ---------- 便捷工具 ----------

// ParseChatAnswer 从 chat v3 事件流中提取 answer 增量（供简单调用方复用）。
// 返回（累计回答文本, conversationID, error）。
func ParseChatAnswer(ev StreamEvent) (delta, conversationID string, done bool, errMsg string) {
	if ev.Event == "done" {
		return "", "", true, ""
	}
	var ce chatV3Event
	if err := json.Unmarshal([]byte(ev.Data), &ce); err != nil {
		return "", "", false, ""
	}
	switch ce.Event {
	case "conversation.message.delta":
		if ce.Data.Message.Type == "answer" {
			return ce.Data.Message.Content, ce.Data.ConversationID, false, ""
		}
	case "conversation.chat.completed":
		return "", ce.Data.ConversationID, true, ""
	case "conversation.chat.failed", "error":
		return "", ce.Data.ConversationID, true, firstNonEmpty(ce.Data.LastMsgError, ce.Data.Msg, fmt.Sprintf("code %d", ce.Data.Code))
	}
	return "", ce.Data.ConversationID, false, ""
}

// firstNonEmpty 返回第一个非空字符串（全空返回空串）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// 默认 HTTP 探测超时（Ping 用）。
const pingTimeout = 8 * time.Second
