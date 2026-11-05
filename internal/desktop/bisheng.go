// BISHENG Workflow 桌面端桥接（RunBishengFlow）：把 internal/bisheng 引擎
// 桥接到 kaelvoris 宿主设施 ——
//
//	LLM  ← desktop.json 配置的 llm.Client（Generate 非流式）
//	工具 ← buildTools 注册表（JSON 参数串协议，与 tools.Tool.Execute 同构）
//	事件 ← uiEmit("kaelvoris:bisheng:step") 逐节点推送前端
//
// 画布来源：既有 Flow.Canvas（flow.CanvasNode/CanvasEdge）→ bisheng.WorkflowData
// 转换；未移植节点类型（transform/knowledge 等 internal/flow 专有）报明确错误，
// 提示走 RunFlow（Eino 引擎）。交互中断经返回值 status=INPUT 上抛前端，
// ContinueBishengFlow 回填续跑（每会话独立 Workflow 实例，进程内暂存）。
package desktop

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"kaelvoris/internal/bisheng"
	"kaelvoris/internal/flow"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/skills"
	"kaelvoris/internal/tools"
)

// bishengRun 进程内暂存的运行实例（Wails 无状态绑定，中断续跑需内存态；
// 与 taskRun 同思路：keyed by runID，锁保护）。
var bishengRun struct {
	mu   sync.Mutex
	runs map[string]*bisheng.Workflow
	seq  int
}

// bishengLLM 桥接 llm.Client → bisheng.LLMInvoker。
type bishengLLM struct{ c *llm.Client }

func (b *bishengLLM) Invoke(msgs []bisheng.LLMMessage) (string, error) {
	req := llm.ChatRequest{}
	for _, m := range msgs {
		req.Messages = append(req.Messages, llm.Message{Role: m.Role, Content: m.Content})
	}
	out, err := b.c.Generate(context.Background(), req)
	if err != nil {
		return "", err
	}
	return out.Content, nil
}

// bishengTools 桥接 tools.Registry → bisheng.ToolExecutor。
type bishengTools struct{ r *tools.Registry }

func (b *bishengTools) Has(name string) bool {
	_, ok := b.r.Get(name)
	return ok
}

func (b *bishengTools) Execute(name, argsJSON string) (string, error) {
	t, ok := b.r.Get(name)
	if !ok {
		return "", fmt.Errorf("tool %s 不存在", name)
	}
	return t.Execute(context.Background(), argsJSON)
}

// bishengCB 引擎事件 → 前端（kaelvoris:bisheng:step）。
type bishengCB struct {
	a     *App
	runID string
}

func (c *bishengCB) OnNodeStart(d bisheng.NodeStartData) {
	uiEmit(c.a.ctx, "kaelvoris:bisheng:step", map[string]any{"runId": c.runID, "phase": "start", "nodeId": d.NodeID, "name": d.Name})
}
func (c *bishengCB) OnNodeEnd(d bisheng.NodeEndData) {
	uiEmit(c.a.ctx, "kaelvoris:bisheng:step", map[string]any{"runId": c.runID, "phase": "end", "nodeId": d.NodeID, "name": d.Name, "reason": d.Reason, "log": d.LogData})
}
func (c *bishengCB) OnUserInput(d bisheng.UserInputData) {
	uiEmit(c.a.ctx, "kaelvoris:bisheng:input", map[string]any{"runId": c.runID, "nodeId": d.NodeID, "name": d.Name, "schema": d.InputSchema})
}
func (c *bishengCB) OnGuideWord(d bisheng.GuideWordData) {
	uiEmit(c.a.ctx, "kaelvoris:bisheng:step", map[string]any{"runId": c.runID, "phase": "guide_word", "nodeId": d.NodeID, "text": d.GuideWord})
}
func (c *bishengCB) OnGuideQuestion(d bisheng.GuideQuestionData) {
	uiEmit(c.a.ctx, "kaelvoris:bisheng:step", map[string]any{"runId": c.runID, "phase": "guide_question", "nodeId": d.NodeID, "questions": d.GuideQuestion})
}
func (c *bishengCB) OnStreamMsg(d bisheng.StreamMsgData) {
	uiEmit(c.a.ctx, "kaelvoris:bisheng:step", map[string]any{"runId": c.runID, "phase": "stream", "nodeId": d.NodeID, "msg": d.Msg})
}
func (c *bishengCB) OnOutputMsg(d bisheng.OutputMsgData) {
	uiEmit(c.a.ctx, "kaelvoris:bisheng:step", map[string]any{"runId": c.runID, "phase": "output", "nodeId": d.NodeID, "msg": d.Msg})
}
func (c *bishengCB) OnOutputInput(d bisheng.OutputMsgInputData) {
	uiEmit(c.a.ctx, "kaelvoris:bisheng:input", map[string]any{"runId": c.runID, "nodeId": d.NodeID, "name": d.Name, "schema": map[string]any{"type": "input", "msg": d.Msg, "key": d.Key}})
}
func (c *bishengCB) OnOutputChoose(d bisheng.OutputMsgChooseData) {
	uiEmit(c.a.ctx, "kaelvoris:bisheng:input", map[string]any{"runId": c.runID, "nodeId": d.NodeID, "name": d.Name, "schema": map[string]any{"type": "choose", "msg": d.Msg, "key": d.Key, "options": d.Options}})
}

// toBishengData 画布转换：flow.Canvas → bisheng.WorkflowData。
// internal/flow 专有节点（template/transform 等）报明确错误。
func toBishengData(c *flow.Canvas) (bisheng.WorkflowData, error) {
	var out bisheng.WorkflowData
	typeMap := map[string]string{
		"start": bisheng.NodeStart, "end": bisheng.NodeEnd,
		"llm": bisheng.NodeLLM, "tools": bisheng.NodeTool, "tool": bisheng.NodeTool,
		"branch": bisheng.NodeCondition, "condition": bisheng.NodeCondition,
		"input": bisheng.NodeInput, "output": bisheng.NodeOutput,
		// passthrough（Eino AppendPassthrough 透传）映射为 message 型输出节点
		//（bisheng 无纯透传节点，语义最近的是 output(message)。
		"passthrough": bisheng.NodeOutput,
	}
	for _, n := range c.Nodes {
		bt, ok := typeMap[n.Type]
		if !ok {
			return out, fmt.Errorf("节点 %s(%s) 不支持 bisheng 引擎（%s 类型请用「运行流程」走 Eino 引擎）", n.ID, n.Step.Name, n.Type)
		}
		g := bisheng.NodeGroupParams{Params: []bisheng.NodeParams{}}
		s := n.Step
		switch bt {
		case bisheng.NodeLLM:
			g.Params = append(g.Params,
				bisheng.NodeParams{Key: "system_prompt", Value: s.Template},
				bisheng.NodeParams{Key: "user_prompt", Value: s.Name},
				bisheng.NodeParams{Key: "model", Value: s.Model},
			)
		case bisheng.NodeTool:
			g.Params = append(g.Params, bisheng.NodeParams{Key: "tool_key", Value: s.Name})
		case bisheng.NodeCondition:
			// branch 节点出边表达式 → 单 case（出边条件在画布边 Expr 上，
			// bisheng 语义为节点参数 condition 数组；此处桥接为逐 case=出边）
		}
		out.Nodes = append(out.Nodes, bisheng.BaseNodeData{
			ID: n.ID, Type: bt, Name: s.Name, ToolKey: s.Name,
			GroupParams: []bisheng.NodeGroupParams{g},
		})
	}
	for _, e := range c.Edges {
		handle := "right_handle"
		if e.Expr != "" {
			handle = "left_handle" // 有条件表达式出边 = case 命中边
		}
		out.Edges = append(out.Edges, bisheng.EdgeBase{
			ID: e.ID, Source: e.Source, Target: e.Target, SourceHandle: handle,
		})
	}
	return out, nil
}

// RunBishengFlowRequest 运行入参。
type RunBishengFlowRequest struct {
	FlowID string         `json:"flowId"`
	Vars   map[string]any `json:"vars,omitempty"`
}

// RunBishengFlowResult 运行结果（status=INPUT 时前端弹交互面板）。
type RunBishengFlowResult struct {
	RunID  string `json:"runId"`
	Status string `json:"status"` // SUCCESS | FAILED | INPUT
	Reason string `json:"reason,omitempty"`
	// Variables 终态变量池（node_id.key → value，前端轨迹展示）。
	Variables map[string]any `json:"variables,omitempty"`
	// History 会话历史（human/ai 消息）。
	History []bisheng.Msg `json:"history,omitempty"`
}

// RunBishengFlow 以 bisheng 引擎运行既有流程（BISHENG Workflow Go 移植入口）。
func (a *App) RunBishengFlow(req RunBishengFlowRequest) (*RunBishengFlowResult, error) {
	f, err := a.GetFlow(strings.TrimSpace(req.FlowID))
	if err != nil {
		return nil, err
	}
	canvas := f.Canvas
	if canvas == nil {
		canvas = flow.CanvasFromSteps(f)
	}
	data, err := toBishengData(canvas)
	if err != nil {
		return nil, err
	}
	cfg := a.GetConfig()
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("bisheng 引擎 llm 节点需先在设置中配置 API Key")
	}
	ws := a.cfgWorkspace()
	home, _ := os.UserHomeDir()

	// 注入用户变量：并入 start 节点 preset_question（bisheng 变量池语义）
	for _, nd := range data.Nodes {
		if nd.Type == bisheng.NodeStart {
			preset := []any{}
			for k, v := range req.Vars {
				preset = append(preset, map[string]any{"key": k, "value": v})
			}
			nd.GroupParams[0].Params = append(nd.GroupParams[0].Params,
				bisheng.NodeParams{Key: "preset_question", Value: preset})
		}
	}

	bishengRun.mu.Lock()
	bishengRun.seq++
	runID := fmt.Sprintf("bisheng_%d", bishengRun.seq)
	bishengRun.mu.Unlock()

	wf, err := bisheng.New(f.ID, f.Name, data, 20, 0, &bishengCB{a: a, runID: runID}, bisheng.Deps{
		LLM:  &bishengLLM{c: llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)},
		Tool: &bishengTools{r: buildTools(ws, tools.NewTodoStore(), skills.NewManager(home, ws), a)},
	})
	if err != nil {
		return nil, err
	}
	bishengRun.mu.Lock()
	if bishengRun.runs == nil {
		bishengRun.runs = map[string]*bisheng.Workflow{}
	}
	bishengRun.runs[runID] = wf
	bishengRun.mu.Unlock()

	st, reason := wf.Run()
	return bishengResult(runID, wf, st, reason), nil
}

// ContinueBishengFlow 交互回填续跑（status=INPUT 的运行）。
func (a *App) ContinueBishengFlow(runID string, input map[string]map[string]any) (*RunBishengFlowResult, error) {
	bishengRun.mu.Lock()
	wf, ok := bishengRun.runs[runID]
	bishengRun.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("运行实例不存在或已结束: %s", runID)
	}
	st, reason := wf.Continue(input)
	return bishengResult(runID, wf, st, reason), nil
}

// StopBishengFlow 用户停止。
func (a *App) StopBishengFlow(runID string) error {
	bishengRun.mu.Lock()
	wf, ok := bishengRun.runs[runID]
	bishengRun.mu.Unlock()
	if !ok {
		return nil
	}
	wf.Stop()
	return nil
}

// bishengResult 组装返回（终态清理运行实例）。
func bishengResult(runID string, wf *bisheng.Workflow, st, reason string) *RunBishengFlowResult {
	res := &RunBishengFlowResult{
		RunID:     runID,
		Status:    st,
		Reason:    reason,
		Variables: wf.Engine().State().AllVariables(),
		History:   wf.Engine().State().HistoryList(50),
	}
	if st == StatusInputBisheng {
		return res // 挂起实例保留，等 ContinueBishengFlow
	}
	bishengRun.mu.Lock()
	delete(bishengRun.runs, runID)
	bishengRun.mu.Unlock()
	return res
}

// StatusInputBisheng 别名（避免与本地常量冲突）。
const StatusInputBisheng = bisheng.StatusInput
