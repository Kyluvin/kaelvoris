package desktop

// 流程编排绑定（P2.7）：ListFlows/GetFlow/SaveFlow/DeleteFlow/RunFlow。
// 持久化于 ~/.kaelvoris/flows/<id>.json；运行引擎为 internal/flow（Eino compose.Chain），
// LLM 步骤复用 desktop.json 配置的 *llm.Client（BaseURL/APIKey/Model）。

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
	"kaelvoris/internal/mcp"
	"kaelvoris/internal/plugin"
	"kaelvoris/internal/skills"
	"kaelvoris/internal/tools"
	"kaelvoris/internal/vector"
)

func flowsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "flows")
}

// flowKB knowledge 节点检索后端（Dify knowledge-retrieval 对齐）：桥接
// internal/vector 持久向量库（设置 → 向量库 索引产物）。配置未就绪时返回
// 明确错误（前端轨迹可见），不静默空结果。
func (a *App) flowKB(ctx context.Context, query string, topK int, minScore float64) (string, int, error) {
	vc := a.vectorConfig()
	if vc == nil || !vc.Enabled {
		return "", 0, errors.New("向量化未开启（设置 → 向量库 开启并索引后可用）")
	}
	emb, err := a.vectorEmbedder(vc)
	if err != nil {
		return "", 0, err
	}
	ix := vector.NewIndexer(vc.Dir, a.vectorStore(), emb)
	out, pts, err := ix.Search(ctx, vector.SearchRequest{
		Query:    query,
		MaxRes:   topK,
		MinScore: minScore,
	})
	if err != nil {
		return "", 0, err
	}
	if len(pts) == 0 {
		return "（知识库中无命中内容）", 0, nil
	}
	return out, len(pts), nil
}

// flowLLMFor llm 节点模型选择回调（Dify LLM 节点 model 对齐）：按节点选的
// 模型名构造专用客户端（模型服务同源：BaseURL/APIKey 复用全局配置，仅换
// 模型名）；模型名为空或未配置时返回 nil（flow 引擎回退全局 LLM）。
func (a *App) flowLLMFor(_ context.Context, model string) *llm.Client {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	cfg := a.GetConfig()
	return llm.NewClient(cfg.BaseURL, cfg.APIKey, model)
}

// loadFlows 读取全部流程（文件损坏的条目跳过不致命）。
func loadFlows() ([]*flow.Flow, error) {
	dir := flowsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*flow.Flow
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var f flow.Flow
		if err := json.Unmarshal(b, &f); err != nil || f.ID == "" {
			continue
		}
		out = append(out, &f)
	}
	return out, nil
}

// ListFlows 流程列表（按更新时间倒序）。
func (a *App) ListFlows() []flow.Summary {
	flows, err := loadFlows()
	if err != nil {
		return []flow.Summary{}
	}
	list := make([]flow.Summary, 0, len(flows))
	for _, f := range flows {
		list = append(list, f.Summary())
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	return list
}

// GetFlow 读取单个流程详情。
func (a *App) GetFlow(id string) (*flow.Flow, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("流程 ID 不能为空")
	}
	b, err := os.ReadFile(filepath.Join(flowsDir(), id+".json"))
	if err != nil {
		return nil, fmt.Errorf("读取流程失败: %w", err)
	}
	var f flow.Flow
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("解析流程失败: %w", err)
	}
	return &f, nil
}

// SaveFlow 新建/更新流程（ID 为空时自动生成）；返回保存后的流程。
// 画布模式（Canvas 非空）：校验拓扑并编译回线性 Steps 后统一校验；
// 旧模式（Canvas 为空且 Steps 非空）：保存时自动生成画布（CanvasFromSteps，
// 自动补齐 start/end 结构节点），旧数据无感升级。
func (a *App) SaveFlow(f flow.Flow) (*flow.Flow, error) {
	f.Name = strings.TrimSpace(f.Name)
	if f.Name == "" {
		return nil, errors.New("流程名称不能为空")
	}
	if f.Canvas != nil {
		// 保存前补齐 start/end（旧画布/新建流程无感升级）；补不动由编译校验报明确错误。
		flow.EnsureStartEnd(f.Canvas)
		steps, cerr := flow.CompileCanvas(f.Canvas)
		if cerr != nil {
			return nil, fmt.Errorf("画布校验失败: %w", cerr)
		}
		f.Steps = steps
	}
	if len(f.Steps) == 0 {
		return nil, errors.New("流程至少需要一个步骤（或画布至少一个节点）")
	}
	for i, s := range f.Steps {
		// 9 类官方标准节点类型校验（flow.StepTypes 对齐 Eino components）。
		if !flow.StepTypes[s.Type] {
			return nil, fmt.Errorf("步骤 %d 类型无效: %s", i+1, s.Type)
		}
		if s.Type == flow.StepTemplate && strings.TrimSpace(s.Template) == "" {
			return nil, fmt.Errorf("步骤 %d 模板内容不能为空", i+1)
		}
	}
	now := time.Now().UnixMilli()
	if strings.TrimSpace(f.ID) == "" {
		f.ID = fmt.Sprintf("flow_%d", now)
		f.CreatedAt = now
	}
	f.UpdatedAt = now
	// 旧模式保存时自动生成画布（EnsureStartEnd 已在 CanvasFromSteps 内补齐结构节点）；
	// 画布模式保存时按最新画布归一（Steps 已回编译，branch/start/end 不进线性 Steps）。
	if f.Canvas == nil {
		f.Canvas = flow.CanvasFromSteps(&f)
	}
	_ = os.MkdirAll(flowsDir(), 0o755)
	b, err := json.MarshalIndent(&f, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(flowsDir(), f.ID+".json"), b, 0o644); err != nil {
		return nil, fmt.Errorf("保存流程失败: %w", err)
	}
	// 同步变量提示到前端。
	uiEmit(a.ctx, "kaelvoris:flows", a.ListFlows())
	return &f, nil
}

// DeleteFlow 删除流程。
func (a *App) DeleteFlow(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("流程 ID 不能为空")
	}
	err := os.Remove(filepath.Join(flowsDir(), id+".json"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除流程失败: %w", err)
	}
	uiEmit(a.ctx, "kaelvoris:flows", a.ListFlows())
	return nil
}

// flowMCPResolver 流程 llm 节点 MCP 引用解析器（Runner.MCPResolver 适配）：
// 按节点引用的服务器名清单，连接已启用的手动 MCP 配置 + 插件自动发现端点，
// 各服务器工具注册进独立 Registry（mcp_<server>_<tool> 全名，与任务级
// buildMCPTools 同名规范）；未引用/已禁用/连接失败的条目跳过不阻断。
// 返回 name→Registry 与统一关闭函数（llm 节点轮次结束后调用方 defer）。
func (a *App) flowMCPResolver(ctx context.Context, servers []string) (map[string]*tools.Registry, func(), error) {
	want := map[string]bool{}
	for _, n := range servers {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	out := map[string]*tools.Registry{}
	var closers []func()
	connect := func(name, command, url string, args []string, env, headers map[string]string) {
		if !want[name] {
			return // 节点未引用该服务器：不连接
		}
		cfg := mcp.ServerConfig{Name: name, Command: command, Args: args, Env: env, URL: url, Headers: headers}
		mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		reg := tools.NewRegistry()
		cli, _, err := mcp.RegisterAll(mctx, cfg, func(full string, t *mcp.RemoteTool) error {
			return reg.Register(t)
		})
		cancel()
		if err != nil {
			return // 单个服务器连接失败降级跳过（对齐任务级语义）
		}
		closers = append(closers, cli.Close)
		out[name] = reg
	}
	// 插件自动发现（启用开关过滤；stdio 相对命令解析到插件目录内，同 buildMCPTools）。
	for _, p := range plugin.List() {
		if !plugin.IsEnabled(p.ID) {
			continue
		}
		name, transport, command, url, args, env, headers, ok := p.MCPServerOf()
		if !ok {
			continue
		}
		if (transport == "stdio" && strings.TrimSpace(command) == "") ||
			(transport != "stdio" && strings.TrimSpace(url) == "") {
			continue
		}
		if env == nil {
			env = map[string]string{}
		}
		if (transport == "stdio" || transport == "") && command != "" && !filepath.IsAbs(command) {
			command = filepath.Join(p.Dir, command)
		}
		connect(name, command, url, args, env, headers)
	}
	// 手动配置（mcp-servers.json，仅 Enabled 条目）。
	for _, s := range a.loadMCPServers() {
		if !s.Enabled {
			continue
		}
		connect(s.Name, s.Command, s.URL, s.Args, s.Env, s.Headers)
	}
	return out, func() {
		for _, c := range closers {
			if c != nil {
				c()
			}
		}
	}, nil
}

// flowSkillLoader 流程 llm 节点技能引用加载器适配（Runner.SkillLoader）：
// Manager.Load 返回完整 Content，引擎只需正文（frontmatter 之后的 markdown）。
func flowSkillLoader(mgr *skills.Manager) func(string) (string, error) {
	return func(name string) (string, error) {
		c, err := mgr.Load(name)
		if err != nil {
			return "", err
		}
		return c.Body, nil
	}
}

// ListFlowMCPServers 流程 llm 节点 MCP 下拉数据源：可引用的 MCP 服务器名
// 清单（手动配置 Enabled 条目 + 已启用插件端点，去重排序）。
func (a *App) ListFlowMCPServers() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range plugin.List() {
		if !plugin.IsEnabled(p.ID) {
			continue
		}
		if name, _, _, _, _, _, _, ok := p.MCPServerOf(); ok && name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, s := range a.loadMCPServers() {
		if s.Enabled && s.Name != "" && !seen[s.Name] {
			seen[s.Name] = true
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// RunFlowRequest 运行流程入参（Wails 绑定用扁平结构）。
type RunFlowRequest struct {
	ID     string         `json:"id"`
	Vars   map[string]any `json:"vars"`
	DryRun bool           `json:"dryRun"`
}

// RunFlow 运行流程（dryRun 只渲染模板不调模型）。OnStep 轨迹经
// "kaelvoris:flow:step" 事件逐步推送前端，最终返回完整 RunResult。
func (a *App) RunFlow(req RunFlowRequest) (*flow.RunResult, error) {
	f, err := a.GetFlow(req.ID)
	if err != nil {
		return nil, err
	}
	return a.runFlowCore(context.Background(), f, req.Vars, req.DryRun, func(t flow.StepTrace) {
		uiEmit(a.ctx, "kaelvoris:flow:step", t)
	})
}

// subflowDepthCtxKey 子流程嵌套深度（context 传递，防循环引用无限递归）。
type subflowDepthCtxKey struct{}

// flowSubFlow subflow 节点执行回调（Coze subworkflow 对齐）：加载目标流程
// 经同一 runFlowCore 链路执行；嵌套深度经 ctx 传递，上限 5 层拦截循环引用。
func (a *App) flowSubFlow(ctx context.Context, flowID string, vars map[string]any, dryRun bool) (*flow.RunResult, error) {
	depth, _ := ctx.Value(subflowDepthCtxKey{}).(int)
	if depth >= 5 {
		return nil, fmt.Errorf("子流程嵌套超过上限 5 层（疑似循环引用，请检查子流程链）")
	}
	f, err := a.GetFlow(flowID)
	if err != nil {
		return nil, fmt.Errorf("子流程 %s 不存在: %w", flowID, err)
	}
	child := context.WithValue(ctx, subflowDepthCtxKey{}, depth+1)
	return a.runFlowCore(child, f, vars, dryRun, nil)
}

// runFlowCore 流程启动共享链路（UI RunFlow 绑定与 cron run_flow 动作同源）：
// 按当前配置装配 Runner（LLM/任务级工具/工作区/知识库回调/模型选择），统一经
// flow.Runner.Start 分派（含 branch/Dify 融入节点画布 → 图执行，其余 →
// Eino Chain 链式执行）。onStep 为空时轨迹仅留在 RunResult（cron 场景无
// 前端会话上下文可推送）。
func (a *App) runFlowCore(ctx context.Context, f *flow.Flow, vars map[string]any, dryRun bool, onStep func(flow.StepTrace)) (*flow.RunResult, error) {
	cfg := a.GetConfig()
	if !dryRun && strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("运行 llm 步骤需先在设置中配置 API Key；或使用试运行（跳过模型）")
	}
	// 应用级全局变量注入（Dify environment-variable 对齐）：FlowApp.Vars 作为
	// 基底，运行时 vars 同名覆盖（UI 填值优先于全局默认）。
	if f.AppID != "" {
		if ap, gerr := a.getApp(f.AppID); gerr == nil && len(ap.Vars) > 0 {
			merged := make(map[string]any, len(ap.Vars)+len(vars))
			for k, v := range ap.Vars {
				merged[k] = v
			}
			for k, v := range vars {
				merged[k] = v
			}
			vars = merged
		}
	}
	ws := a.cfgWorkspace()
	home, _ := os.UserHomeDir()
	skillMgr := skills.NewManager(home, ws)
	r := &flow.Runner{
		LLM:       llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model),
		Tools:     buildTools(ws, tools.NewTodoStore(), skillMgr, a),
		Workspace: ws,
		// llm 节点 MCP / 技能引用装配（step.MCPServers / step.Skills，Coze 语义）。
		MCPResolver: a.flowMCPResolver,
		SkillLoader: flowSkillLoader(skillMgr),
		// knowledge 节点持久向量库检索 + llm 节点级模型选择（Dify 语义）
		// + subflow 子流程执行（Coze subworkflow 语义）。
		KB:      a.flowKB,
		LLMFor:  a.flowLLMFor,
		SubFlow: a.flowSubFlow,
		OnStep: func(t flow.StepTrace) {
			uiEmit(a.ctx, "kaelvoris:flow:step", t)
			if onStep != nil {
				onStep(t)
			}
		},
	}
	return r.Start(ctx, f, vars, dryRun)
}

// FlowVars 返回流程需要的模板变量列表（前端变量填写提示）。
func (a *App) FlowVars(id string) ([]string, error) {
	f, err := a.GetFlow(id)
	if err != nil {
		return nil, err
	}
	vars := flow.TraceVars(f)
	if vars == nil {
		vars = []string{}
	}
	return vars, nil
}

// AssignFlow 设置流程归属应用（appID 为空 = 归入「默认应用」）。
func (a *App) AssignFlow(flowID, appID string) error {
	f, err := a.GetFlow(strings.TrimSpace(flowID))
	if err != nil {
		return err
	}
	f.AppID = strings.TrimSpace(appID)
	if f.AppID != "" {
		if _, gerr := a.getApp(f.AppID); gerr != nil {
			return fmt.Errorf("目标应用不存在: %w", gerr)
		}
	}
	f.UpdatedAt = time.Now().UnixMilli()
	_ = os.MkdirAll(flowsDir(), 0o755)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(flowsDir(), f.ID+".json"), b, 0o644); err != nil {
		return fmt.Errorf("保存流程失败: %w", err)
	}
	a.emitAppsChange()
	return nil
}
