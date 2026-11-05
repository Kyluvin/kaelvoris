// acp_run.go ACP 任务执行（app 层）：
//
// 会话绑定 ACP agent（session.ACPAgent 字段，任务/续聊固定走该 agent），
// 任务文本经 ACP session/prompt 派发给外部 agent（Claude Code / Gemini CLI /
// Codex 等）执行；session/update 流式经 UpdateMapper 映射为 ClineMessage，
// 接入桌面端统一 say 桥（历史留痕 + EventsEmit 推送 + 会话持久化零改动）。
// 权限审批桥接 run.askCh（复用前端审批卡：tool ask + yes/no 按钮）。
//
// 进程生命周期 = 一次 turn（对齐 MCP client 语义）：每轮 spawn → prompt →
// 收割 → Close；上下文延续经会话恢复摘要（resumeDigest）注入下一轮 prompt，
// ACP 侧无跨进程会话依赖（Claude Code 适配器以 --resume 持久化会话的用法
// 交由用户在命令行参数中配置，进程级隔离更可控）。
package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kaelvoris/internal/acp"
	"kaelvoris/internal/session"
	"kaelvoris/pkg/message"
)

// ACPAgentConfig app 层 agent 配置视图（对齐 acp.AgentConfig）。
type ACPAgentConfig = acp.AgentConfig

// acpAgentsConfigPath 配置文件路径（~/.kaelvoris/acp-agents.json）。
func acpAgentsConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "acp-agents.json")
}

// ListACPAgents 读取 ACP agent 配置列表（损坏/缺失 = 空列表，不致命）。
func (a *App) ListACPAgents() []acp.AgentConfig {
	b, err := os.ReadFile(acpAgentsConfigPath())
	if err != nil {
		return []acp.AgentConfig{}
	}
	var out []acp.AgentConfig
	if json.Unmarshal(b, &out) != nil {
		return []acp.AgentConfig{}
	}
	return out
}

// SaveACPAgents 保存 ACP agent 配置（整表替换；逐项校验）。
func (a *App) SaveACPAgents(agents []acp.AgentConfig) error {
	for _, ag := range agents {
		if strings.TrimSpace(ag.Name) == "" {
			return errors.New("ACP agent 名称不能为空")
		}
		if strings.TrimSpace(ag.Command) == "" {
			return errors.New("ACP agent 启动命令不能为空: " + ag.Name)
		}
	}
	if err := os.MkdirAll(filepath.Dir(acpAgentsConfigPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(agents, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(acpAgentsConfigPath(), b, 0o600)
}

// acpTestResult TestACPAgent 连接测试结果（前端「测试」按钮展示）。
type acpTestResult struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	AgentName  string `json:"agentName,omitempty"`
	Version    string `json:"version,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"durationMS,omitempty"`
}

// TestACPAgent 连接测试单个 ACP agent：spawn + initialize 握手 + session/new
// 后立即关闭（不派发任务）。验证命令可启动、协议可握手、工作目录可访问。
func (a *App) TestACPAgent(agt acp.AgentConfig) (acpTestResult, error) {
	start := time.Now()
	res := acpTestResult{Name: agt.Name}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if agt.Protocol == acp.CodexProtocol {
		return a.testCodexAgent(ctx, agt, res, start)
	}
	cli, err := acp.Start(ctx, agt, a.cfgWorkspace(), acp.FsHooks{})
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	defer cli.Close()
	if _, err := cli.NewSession(ctx, a.cfgWorkspace()); err != nil {
		res.Error = err.Error()
		return res, nil
	}
	if cli.AgentInfo != nil {
		res.AgentName = cli.AgentInfo.Name
		res.Version = cli.AgentInfo.Version
	}
	res.OK = true
	res.DurationMS = time.Since(start).Milliseconds()
	return res, nil
}

// testCodexAgent codex app-server 连接测试：start → initialize → thread/start。
func (a *App) testCodexAgent(ctx context.Context, agt acp.AgentConfig, res acpTestResult, start time.Time) (acpTestResult, error) {
	cli, err := acp.StartCodex(ctx, agt, a.cfgWorkspace(), acp.FsHooks{})
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	defer cli.Close()
	if _, err := cli.NewThread(ctx, a.cfgWorkspace()); err != nil {
		res.Error = err.Error()
		return res, nil
	}
	if cli.AgentInfo != nil {
		res.AgentName = cli.AgentInfo.Name
		res.Version = cli.AgentInfo.Version
	}
	res.OK = true
	res.DurationMS = time.Since(start).Milliseconds()
	return res, nil
}

// acpAgentOf 取会话绑定的 ACP agent 配置（未绑定/未启用/已删 = nil → 回落内置引擎）。
func (a *App) acpAgentOf(sessionID string) *acp.AgentConfig {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return nil
	}
	sess, err := a.store.Get(sid)
	if err != nil || sess == nil {
		return nil
	}
	name := sess.Snapshot().ACPAgent
	if name == "" {
		return nil
	}
	for _, ag := range a.ListACPAgents() {
		if ag.Name == name && ag.Enabled {
			agCopy := ag
			return &agCopy
		}
	}
	return nil
}

// acpTurn 执行一次 ACP turn（会话级：spawn → prompt → 流式桥 → 收割）。
// say 流经参数注入（app 层 makeSayBridge 产物），审批经 run.askCh。
func (a *App) acpTurn(ctx context.Context, run *taskRun, say func(message.ClineMessage), agentCfg acp.AgentConfig, task string) (string, error) {
	ws := run.workspace
	if ws == "" {
		ws = a.cfgWorkspace()
	}
	// UpdateMapper：session/update → ClineMessage（流式正文/思考/工具卡），
	// say 出口 = 通道桥（桌面端 EventsEmit + 会话持久化 / 远程任务事件双路）。
	mapper := acp.NewUpdateMapper(say)

	// 权限审批桥：推送 tool ask 卡（say 桥落历史+推送）→ 阻塞等待 askCh 应答
	// （yes = 放行第一个 allow 选项；no = 拒绝）。
	askApproval := func(title, kind string) bool {
		askMsg := message.NewAsk(message.AskTool,
			fmt.Sprintf("ACP agent 请求执行（%s）: %s", kindLabelCN(kind), title))
		say(askMsg)
		select {
		case r := <-run.askCh:
			return r.response == message.ResponseYes
		case <-ctx.Done():
			return false
		}
	}

	// fs 反向请求钩子（agent 请求 client 读写工作区文件）。
	fsHooks := acp.FsHooks{
		ReadTextFile: readWorkspaceFile,
		WriteTextFile: func(path, content string) error {
			return os.WriteFile(path, []byte(content), 0o644)
		},
		RequestPermission: func(sessionID string, p *acp.PermissionRequestPayload) (string, error) {
			allow := askApproval(toolCallTitleOf(p.ToolCall), toolCallKindOf(p.ToolCall))
			if allow {
				if oid := firstAllowOptionOf(p.Options); oid != "" {
					return oid, nil
				}
				return "", fmt.Errorf("无可用放行选项")
			}
			if oid := firstRejectOptionOf(p.Options); oid != "" {
				return oid, nil
			}
			return "", fmt.Errorf("用户拒绝")
		},
	}

	if agentCfg.Protocol == acp.CodexProtocol {
		return a.codexTurn(ctx, run, say, agentCfg, ws, task, mapper, fsHooks)
	}

	cli, err := acp.Start(ctx, agentCfg, ws, fsHooks)
	if cli != nil {
		defer cli.Close()
	}
	if err != nil {
		return "", err
	}
	cli.SetUpdateHandler(mapper.HandleUpdate)
	sessID, err := cli.NewSession(ctx, ws)
	if err != nil {
		return "", fmt.Errorf("acp session/new: %w", err)
	}
	res, err := cli.Prompt(ctx, sessID, task)
	if err != nil {
		// 取消语义：ctx 取消（用户点停止）→ 返回取消错误（上层按中止处理）。
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	finalText := mapper.FinalText()
	// 停止原因兜底：agent 无正文输出时以停止原因作为结果摘要。
	if strings.TrimSpace(finalText) == "" {
		finalText = fmt.Sprintf("（ACP turn 结束：%s）", res.StopReason)
	}
	return finalText, nil
}

// codexTurn 执行一次 Codex app-server turn（复用 UpdateMapper 流式映射 + 审批桥）。
func (a *App) codexTurn(ctx context.Context, run *taskRun, say func(message.ClineMessage), agentCfg acp.AgentConfig, ws string, task string, mapper *acp.UpdateMapper, fsHooks acp.FsHooks) (string, error) {
	cli, err := acp.StartCodex(ctx, agentCfg, ws, fsHooks)
	if cli != nil {
		defer cli.Close()
	}
	if err != nil {
		return "", err
	}
	// codex 通知 → UpdateMapper.HandleCodex（正文 delta / 思考 delta / 工具卡）。
	cli.SetUpdateHandler(func(n acp.CodexNotification) {
		mapper.HandleCodex(n.Method, n.Params)
	})
	threadID, err := cli.NewThread(ctx, ws)
	if err != nil {
		return "", fmt.Errorf("codex thread/start: %w", err)
	}
	turnID, err := cli.StartTurn(ctx, threadID, task)
	if err != nil {
		return "", fmt.Errorf("codex turn/start: %w", err)
	}
	_ = turnID
	turn, err := cli.WaitTurn(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	finalText := mapper.FinalText()
	if strings.TrimSpace(finalText) == "" {
		if turn.Status != "" {
			finalText = fmt.Sprintf("（Codex turn 结束：%s）", turn.Status)
		}
	}
	return finalText, nil
}

// toolCallTitleOf/toolCallKindOf 从 toolCall JSON 提取审批卡展示字段。
func toolCallTitleOf(raw json.RawMessage) string {
	var t struct {
		Title string `json:"title"`
	}
	if json.Unmarshal(raw, &t) == nil {
		return t.Title
	}
	return ""
}

func toolCallKindOf(raw json.RawMessage) string {
	var t struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(raw, &t) == nil {
		return t.Kind
	}
	return ""
}

// firstAllowOptionOf/firstRejectOptionOf 审批选项选取（app 层镜像 acp 包辅助）。
func firstAllowOptionOf(opts []acp.PermissionOption) string {
	for _, o := range opts {
		if strings.HasPrefix(o.Kind, "allow") {
			return o.OptionID
		}
	}
	return ""
}

func firstRejectOptionOf(opts []acp.PermissionOption) string {
	for _, o := range opts {
		if strings.HasPrefix(o.Kind, "reject") {
			return o.OptionID
		}
	}
	return ""
}

// readWorkspaceFile 读取工作区文件（fs/read_text_file 反向请求）。
// line 从 1 计（0 = 从头）；limit>0 限制行数。
func readWorkspaceFile(path string, line, limit int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := string(b)
	if line > 1 || limit > 0 {
		lines := strings.Split(content, "\n")
		start := line - 1
		if start < 0 {
			start = 0
		}
		if start > len(lines) {
			start = len(lines)
		}
		end := len(lines)
		if limit > 0 && start+limit < end {
			end = start + limit
		}
		content = strings.Join(lines[start:end], "\n")
	}
	return content, nil
}

// acpRunTask ACP 会话任务的完整执行（runTask ACP 分支）：
// 轮次循环对齐内置路径（队列续发 / 自学习复盘 / 会话标题兜底 / done 事件），
// 每轮 acpTurn 派发外部 agent；恢复摘要注入 prompt（上下文延续）。
func (a *App) acpRunTask(ctx context.Context, run *taskRun, sess *session.Session, sessionID string, agt acp.AgentConfig, task, resumeDigest string) {
	defer func() {
		if run.cleanup != nil {
			run.cleanup()
		}
		cancel := run.cancel
		if cancel != nil {
			cancel()
		}
		a.runsMu.Lock()
		delete(a.runs, sessionID)
		a.runsMu.Unlock()
		a.registry.Remove(sessionID)
		// 会话级状态修复（对齐内置路径）：完成事件携带本会话运行态。
		uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": false, "sessionId": sessionID})
	}()

	curTask := task
	for {
		turnStart := time.Now()
		say := a.makeSayBridge(sess, run, sessionID)
		prompt := curTask
		if resumeDigest != "" {
			prompt = resumeDigest + "\n\n---- 当前任务 ----\n" + curTask
		}
		result, err := a.acpTurn(ctx, run, say, agt, prompt)
		done := map[string]any{"result": result, "sessionId": sessionID}
		if err != nil {
			// 用户中止：不作为错误展示（对齐内置路径取消语义）。
			if ctx.Err() != nil {
				done["error"] = "任务已停止"
			} else {
				done["error"] = err.Error()
				say(message.NewSay(message.SayError, truncateRunes(err.Error(), 500)))
			}
		}
		// 会话标题儋底（空任务 + 结果摘要）。
		if strings.TrimSpace(sess.Title) == "" && result != "" {
			sess.SetTitle(truncateTitle(result, 40))
			a.store.Save(sess)
		}
		a.store.FlushAll()
		uiEmit(a.ctx, "kaelvoris:done", done)
		uiEmit(a.ctx, "kaelvoris:sessions", a.store.List())

		// 会话总结（与内置路径同源：任务结束后台生成）。
		a.maybeTaskSummary(sessionID, curTask, run, err, time.Since(turnStart))
		// 工作区面板会话：任务结束回写工作区根 README.md 工作日志（用户需求 2026-12）。
		a.maybeWSProjectLog(sessionID, curTask, result, err)

		// 自学习复盘（与内置路径同源：历史快照 + 待办）。
		a.maybeSelfReview(sessionID, curTask, run, err)

		if run.IsAborted() || ctx.Err() != nil {
			return
		}
		next, ok := a.dequeueSessionMsg(sessionID)
		if !ok {
			return
		}
		if next.task == "" && len(next.images) == 0 {
			continue
		}
		curTask = next.task
		resumeDigest = ResumeDigestFor(sess.Snapshot(), a.memory.Load(sessionID), curTask)
		uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": true, "sessionId": sessionID})
		uiEmit(a.ctx, "kaelvoris:queued-continue", map[string]any{
			"sessionId": sessionID, "task": curTask,
		})
	}
}

// kindLabelCN 工具类别中文名（审批卡文案）。
func kindLabelCN(kind string) string {
	switch kind {
	case "read":
		return "读取"
	case "edit":
		return "编辑"
	case "delete":
		return "删除"
	case "move":
		return "移动"
	case "search":
		return "搜索"
	case "execute":
		return "执行命令"
	case "think":
		return "思考"
	case "fetch":
		return "网络请求"
	default:
		return "工具调用"
	}
}
