// run.go ACP 任务执行核心：spawn agent → initialize → session/new →
// session/prompt 阻塞至 turn 结束；session/update 流式通知经 UpdateMapper
// 映射为 ClineMessage 走统一事件桥（聊天流/会话持久化零改动）；
// 反向请求（fs 读写/权限审批）桥接宿主钩子（工作区边界 + askCh 审批通道）。
//
// 映射规则（update.sessionUpdate 分流，run/bridge.go 详细协议）：
//   - agent_message_chunk → say text partial 流式
//   - agent_thought_chunk → say reasoning 流式
//   - tool_call / tool_call_update → say tool（JSON 载荷 ToolCallInfo）
//   - plan → say tool（update_todo_list 形状，前端待办联动）
//   - 其余未知 update → 忽略（协议演进兼容）
package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// TurnHooks 一次 ACP turn 的宿主钩子（app 层注入；全部可选，nil = 安全默认）。
type TurnHooks struct {
	// ReadFile 工作区文件读取（agent fs/read_text_file 反向请求；line/limit 从 1 计）。
	ReadFile func(path string, line, limit int) (string, error)
	// WriteFile 工作区文件写入。
	WriteFile func(path, content string) error
	// RequestApproval 权限审批桥（阻塞等待用户应答；true = 放行）。
	// title 为工具标题（如 "Edit src/main.go"），kind ∈ read/edit/execute/...。
	RequestApproval func(title, kind string) bool
}

// TurnResult 一次 ACP turn 的执行结果。
type TurnResult struct {
	// StopReason agent 停止原因（end_turn = 正常完成）。
	StopReason StopReason
	// Text 助手全文（agent_message_chunk 累计全文）。
	Text string
}

// RunTurn 执行一次完整 turn：Start（spawn+initialize）→ session/new →
// prompt → 阻塞至 turn 结束。不自动 Close（调用方 defer cli.Close()），
// 以便同进程内多轮续聊（同 session 多 prompt：ACP 会话上下文由 agent 维持）。
func RunTurn(ctx context.Context, cfg AgentConfig, cwd, prompt string, hooks TurnHooks) (*TurnResult, *Client, error) {
	cli, err := Start(ctx, cfg, cwd, FsHooks{
		ReadTextFile:      readFileHook(hooks, cwd),
		WriteTextFile:     writeHook(hooks),
		RequestPermission: permHook(hooks),
	})
	if err != nil {
		return nil, nil, err
	}
	sessID, err := cli.NewSession(ctx, cwd)
	if err != nil {
		return nil, cli, err
	}
	res, err := cli.Prompt(ctx, sessID, prompt)
	if err != nil {
		return nil, cli, err
	}
	return &TurnResult{StopReason: res.StopReason}, cli, nil
}

// permHook 权限审批适配：RequestApproval 返回 true → 选第一个 allow 选项；
// false → 选第一个 reject 选项（无匹配时错误帧回退，agent 侧按拒绝处理）。
func permHook(h TurnHooks) func(sessionID string, p *PermissionRequestPayload) (string, error) {
	return func(sessionID string, p *PermissionRequestPayload) (string, error) {
		if h.RequestApproval == nil {
			// 无人值守：自动放行。
			if oid := firstAllowOption(p.Options); oid != "" {
				return oid, nil
			}
			return "", fmt.Errorf("无可用放行选项")
		}
		if h.RequestApproval(toolCallTitle(p.ToolCall), toolCallKind(p.ToolCall)) {
			if oid := firstAllowOption(p.Options); oid != "" {
				return oid, nil
			}
			return "", fmt.Errorf("无可用放行选项")
		}
		if oid := firstRejectOption(p.Options); oid != "" {
			return oid, nil
		}
		return "", fmt.Errorf("用户拒绝")
	}
}

// firstRejectOption 取第一个 reject 类选项。
func firstRejectOption(opts []PermissionOption) string {
	for _, o := range opts {
		if strings.HasPrefix(o.Kind, "reject") {
			return o.OptionID
		}
	}
	return ""
}

// toolCallTitle 从 toolCall JSON 提取 title。
func toolCallTitle(raw json.RawMessage) string {
	var t struct {
		Title string `json:"title"`
	}
	if json.Unmarshal(raw, &t) == nil {
		return t.Title
	}
	return ""
}

// toolCallKind 从 toolCall 提取 kind。
func toolCallKind(raw json.RawMessage) string {
	var t struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(raw, &t) == nil {
		return t.Kind
	}
	return ""
}

// resolvePath 工作区相对路径 → 绝对路径（绝对路径原样返回）。
func resolvePath(cwd, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(cwd, p)
}

// readFileHook fs/read_text_file 反向请求适配（工作区相对路径解析）。
func readFileHook(h TurnHooks, cwd string) func(path string, line, limit int) (string, error) {
	return func(path string, line, limit int) (string, error) {
		if h.ReadFile == nil {
			return "", fmt.Errorf("文件读取未启用")
		}
		return h.ReadFile(resolvePath(cwd, path), line, limit)
	}
}

// writeHook fs/write_text_file 反向请求适配。
func writeHook(h TurnHooks) func(path, content string) error {
	return func(path, content string) error {
		if h.WriteFile == nil {
			return fmt.Errorf("文件写入未启用")
		}
		return h.WriteFile(path, content)
	}
}
