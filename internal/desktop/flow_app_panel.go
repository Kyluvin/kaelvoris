package desktop

// 流程编排应用绑定（P1）：应用（工作流容器）CRUD + 按应用列工作流 + 单节点调试。
// 应用持久化于 ~/.kaelvoris/flowapps/<id>.json（独立目录，不动既有 flows 数据）；
// 流程归属用 Flow.AppID（空 = 「默认应用」虚拟分组，无需落盘）。
// 单节点调试 RunNode 复用 internal/flow.Runner：取画布目标节点装入单步临时链执行，
// 不落盘、不改原 Flow；LLM/工具/工作区构建与 RunFlow（flows.go）同源。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kaelvoris/internal/flow"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/skills"
	"kaelvoris/internal/tools"
)

func flowAppsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "flowapps")
}

// loadApps 读取全部应用（文件损坏的条目跳过不致命，同 loadFlows 约定）。
func loadApps() ([]*flow.FlowApp, error) {
	dir := flowAppsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*flow.FlowApp
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var ap flow.FlowApp
		if err := json.Unmarshal(b, &ap); err != nil || ap.ID == "" {
			continue
		}
		out = append(out, &ap)
	}
	return out, nil
}

// emitAppsChange 应用列表变更后同步前端（事件约定同 kaelvoris:flows）。
func (a *App) emitAppsChange() {
	uiEmit(a.ctx, "kaelvoris:flowapps", a.ListApps())
}

// ListApps 应用列表（按更新时间倒序，附每应用下工作流数量）。
func (a *App) ListApps() []flow.AppSummary {
	apps, err := loadApps()
	if err != nil {
		return []flow.AppSummary{}
	}
	flows, _ := loadFlows()
	count := map[string]int{}
	for _, f := range flows {
		count[f.AppID]++
	}
	list := make([]flow.AppSummary, 0, len(apps))
	for _, ap := range apps {
		s := ap.Summary()
		s.FlowCount = count[ap.ID]
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	return list
}

// CreateApp 新建应用。
func (a *App) CreateApp(name, desc, icon string) (*flow.FlowApp, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("应用名称不能为空")
	}
	now := time.Now().UnixMilli()
	ap := &flow.FlowApp{
		ID:        fmt.Sprintf("app_%d", now),
		Name:      name,
		Desc:      strings.TrimSpace(desc),
		Icon:      strings.TrimSpace(icon),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := a.saveApp(ap); err != nil {
		return nil, err
	}
	return ap, nil
}

// UpdateApp 更新应用名称/描述/图标（Vars/Config/AppKey 为 P2/P3 范围，暂不开放）。
func (a *App) UpdateApp(id, name, desc, icon string) error {
	ap, err := a.getApp(id)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("应用名称不能为空")
	}
	ap.Name = name
	ap.Desc = strings.TrimSpace(desc)
	ap.Icon = strings.TrimSpace(icon)
	ap.UpdatedAt = time.Now().UnixMilli()
	if err := a.saveApp(ap); err != nil {
		return err
	}
	return nil
}

// GetAppVars 应用全局变量（Dify environment variables 对齐）：键值对 map。
func (a *App) GetAppVars(id string) (map[string]string, error) {
	ap, err := a.getApp(id)
	if err != nil {
		return nil, err
	}
	if ap.Vars == nil {
		return map[string]string{}, nil
	}
	return ap.Vars, nil
}

// UpdateAppVars 保存应用全局变量（整表替换；运行时注入基底，同名被运行时
// 变量覆盖）。前端应用设置面板调用。
func (a *App) UpdateAppVars(id string, vars map[string]string) error {
	ap, err := a.getApp(id)
	if err != nil {
		return err
	}
	if vars == nil {
		vars = map[string]string{}
	}
	ap.Vars = vars
	ap.UpdatedAt = time.Now().UnixMilli()
	return a.saveApp(ap)
}

// DeleteApp 删除应用：其下工作流 AppID 置空（回归「默认应用」），不删流程数据。
func (a *App) DeleteApp(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("应用 ID 不能为空")
	}
	if err := os.Remove(filepath.Join(flowAppsDir(), id+".json")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除应用失败: %w", err)
	}
	flows, _ := loadFlows()
	for _, f := range flows {
		if f.AppID == id {
			f.AppID = ""
			if b, err := json.MarshalIndent(f, "", "  "); err == nil {
				_ = os.WriteFile(filepath.Join(flowsDir(), f.ID+".json"), b, 0o644)
			}
		}
	}
	a.emitAppsChange()
	return nil
}

// ListFlowTools 工具节点下拉数据源：任务级工具注册中心注册名列表（同 RunFlow buildTools）。
func (a *App) ListFlowTools() []string {
	ws := a.cfgWorkspace()
	home, _ := os.UserHomeDir()
	names := buildTools(ws, tools.NewTodoStore(), skills.NewManager(home, ws), a).Names()
	if names == nil {
		names = []string{}
	}
	return names
}

// ListFlowsByApp 某应用下的工作流列表（appID 为空 = 「默认应用」，按更新时间倒序）。
func (a *App) ListFlowsByApp(appID string) []flow.Summary {
	flows, err := loadFlows()
	if err != nil {
		return []flow.Summary{}
	}
	appID = strings.TrimSpace(appID)
	list := make([]flow.Summary, 0, len(flows))
	for _, f := range flows {
		if f.AppID == appID {
			list = append(list, f.Summary())
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	return list
}

// RunNodeRequest 单节点调试入参。
type RunNodeRequest struct {
	FlowID string         `json:"flowId"`
	NodeID string         `json:"nodeId"`
	Vars   map[string]any `json:"vars"`
	DryRun bool           `json:"dryRun"`
}

// RunNode 单节点调试：取 Flow.Canvas 中目标节点装入单步临时链执行
// （StepTrace.Index 恒为 0）。旧格式流程按 Steps 顺序现场生成画布 ID
// （n<i>_<ts>，同 CanvasFromSteps 规则）后再定位，不落盘。
func (a *App) RunNode(req RunNodeRequest) (*flow.RunResult, error) {
	f, err := a.GetFlow(req.FlowID)
	if err != nil {
		return nil, err
	}
	canvas := f.Canvas
	if canvas == nil {
		canvas = flow.CanvasFromSteps(f)
	}
	var node *flow.CanvasNode
	for i := range canvas.Nodes {
		if canvas.Nodes[i].ID == req.NodeID {
			node = &canvas.Nodes[i]
			break
		}
	}
	if node == nil {
		return nil, fmt.Errorf("画布节点不存在: %s", req.NodeID)
	}
	step := node.Step
	step.Type = node.Type // 节点类型为准

	cfg := a.GetConfig()
	if !req.DryRun && strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("调试 llm 步骤需先在设置中配置 API Key；或使用试运行（跳过模型）")
	}
	ws := a.cfgWorkspace()
	home, _ := os.UserHomeDir()
	skillMgr := skills.NewManager(home, ws)
	r := &flow.Runner{
		LLM:       llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model),
		Tools:     buildTools(ws, tools.NewTodoStore(), skillMgr, a),
		Workspace: ws,
		// llm 节点 MCP / 技能引用装配（同 RunFlow，单节点调试路径一致）。
		MCPResolver: a.flowMCPResolver,
		SkillLoader: flowSkillLoader(skillMgr),
		OnStep: func(t flow.StepTrace) {
			uiEmit(a.ctx, "kaelvoris:flow:step", t)
		},
		KB:     a.flowKB,
		LLMFor: a.flowLLMFor,
	}
	// Dify 融入节点（transform/http/assigner/extractor/iteration/knowledge）
	// 与 branch 走图执行路径（Chain 路径不认这 6 类，且变量池回写语义需
	// 与正式运行一致）：构造 S → N → Z 最小画布复用 RunGraph，校验/
	// 入边门禁/变量池写回与正式运行完全一致；transform 纯计算 dry-run
	// 也真实执行，http/extractor/llm dry-run 仅渲染不外呼。
	tmpCanvas := &flow.Canvas{
		Nodes: []flow.CanvasNode{
			{ID: "_s", Type: flow.StepStart, Step: flow.Step{Type: flow.StepStart}},
			{ID: req.NodeID, Type: step.Type, Step: step},
			{ID: "_z", Type: flow.StepEnd, Step: flow.Step{Type: flow.StepEnd}},
		},
		Edges: []flow.CanvasEdge{
			{ID: "_e1", Source: "_s", Target: req.NodeID},
			{ID: "_e2", Source: req.NodeID, Target: "_z"},
		},
	}
	if flow.NeedsGraph(tmpCanvas) {
		tmpF := &flow.Flow{ID: f.ID, Name: f.Name, Canvas: tmpCanvas}
		return r.RunGraph(context.Background(), tmpF, tmpCanvas, req.Vars, req.DryRun)
	}
	tmp := &flow.Flow{ID: f.ID, Name: f.Name, Steps: []flow.Step{step}}
	return r.Run(context.Background(), tmp, req.Vars, req.DryRun)
}

// getApp 读取单个应用（内部用）。
func (a *App) getApp(id string) (*flow.FlowApp, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("应用 ID 不能为空")
	}
	b, err := os.ReadFile(filepath.Join(flowAppsDir(), id+".json"))
	if err != nil {
		return nil, fmt.Errorf("读取应用失败: %w", err)
	}
	var ap flow.FlowApp
	if err := json.Unmarshal(b, &ap); err != nil {
		return nil, fmt.Errorf("解析应用失败: %w", err)
	}
	return &ap, nil
}

// saveApp 原子落盘应用并广播列表变更。
func (a *App) saveApp(ap *flow.FlowApp) error {
	_ = os.MkdirAll(flowAppsDir(), 0o755)
	b, err := json.MarshalIndent(ap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(flowAppsDir(), ap.ID+".json"), b, 0o644); err != nil {
		return fmt.Errorf("保存应用失败: %w", err)
	}
	a.emitAppsChange()
	return nil
}
