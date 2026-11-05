package tools

// 编排工具：移植自 Zoo-Code
//   - new_task            ← src/core/task/tools/new_task/NewTaskTool.ts（子任务委派）
//   - switch_mode         ← src/core/task/tools/SwitchModeTool.ts（模式切换）
//   - skill               ← src/core/task/tools/SkillTool.ts（技能加载）
//   - update_todo_list    ← packages/types 工具清单（任务清单管理）
//
// new_task / switch_mode 通过 error 携带特殊标记（对齐 AskResult/CompletionResult 模式），
// 由引擎捕获后执行编排动作，工具本身不落地。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// ---------- TodoList ----------

// TodoItem 单条任务清单项（对齐 TodoItem：content + status）。
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

// TodoStore 任务清单存储（引擎持有，注入系统提示词）。
type TodoStore struct {
	mu    sync.Mutex
	items []TodoItem
}

// NewTodoStore 创建空清单。
func NewTodoStore() *TodoStore { return &TodoStore{} }

// Set 覆盖清单。
func (s *TodoStore) Set(items []TodoItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = items
}

// Items 返回清单快照。
func (s *TodoStore) Items() []TodoItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TodoItem, len(s.items))
	copy(out, s.items)
	return out
}

// Markdown 渲染为 markdown checklist（注入系统提示词）。
func (s *TodoStore) Markdown() string {
	items := s.Items()
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, it := range items {
		mark := " "
		switch it.Status {
		case "completed":
			mark = "x"
		case "in_progress":
			mark = "-"
		}
		b.WriteString(mark + " " + it.Content + "\n")
	}
	return b.String()
}

// UpdateTodoListTool 任务清单工具。
type UpdateTodoListTool struct{ Store *TodoStore }

func (t *UpdateTodoListTool) Name() string { return "update_todo_list" }

func (t *UpdateTodoListTool) Description() string {
	return "Replace the entire TODO list with an updated checklist reflecting the current state. " +
		"Use a single-level markdown checklist in execution order. " +
		"Status options: pending, in_progress, completed. " +
		"Always provide the full list; the system will overwrite the previous one."
}

func (t *UpdateTodoListTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"todos": map[string]interface{}{
			"type":        "array",
			"description": "Full checklist in execution order",
			"items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"content": strProp("Checklist item text"),
					"status": map[string]interface{}{
						"type": "string",
						"enum": []string{"pending", "in_progress", "completed"},
					},
				},
				"required": []string{"content", "status"},
			},
		},
	}, []string{"todos"})
}

func (t *UpdateTodoListTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Todos json.RawMessage `json:"todos"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	todos, err := parseTodosList(p.Todos)
	if err != nil {
		return "", err
	}
	for i, it := range todos {
		switch it.Status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("todos[%d].status must be pending|in_progress|completed, got %q", i, it.Status)
		}
	}
	if t.Store != nil {
		t.Store.Set(todos)
	}
	return fmt.Sprintf("Todo list updated with %d items.", len(todos)), nil
}

// parseTodosList todos 参数多形态容错（2026-10-04 会话复盘：模型把 todos 传成
// 字符串（内容为 markdown checklist 或 JSON 文本）时，严格 unmarshal 直接报
// "cannot unmarshal string" 类型错。宽容解析三种形态：
//  1. 标准数组 [{content,status}]；
//  2. 字符串化 JSON 数组 → 二次解析；
//  3. markdown checklist 文本 → 按行解析（- [ ] 前缀；行内无状态标记时默认 pending）。
func parseTodosList(raw json.RawMessage) ([]TodoItem, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("todos list must not be empty")
	}
	var items []TodoItem
	if err := json.Unmarshal(raw, &items); err == nil {
		if len(items) == 0 {
			return nil, fmt.Errorf("todos list must not be empty")
		}
		return items, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("todos must be an array of {content,status}")
	}
	// 字符串形态：先试 JSON 数组文本，再退化 markdown checklist 逐行解析。
	if items2 := []TodoItem{}; json.Unmarshal([]byte(s), &items2) == nil && len(items2) > 0 {
		return items2, nil
	}
	items3 := parseMarkdownChecklist(s)
	if len(items3) == 0 {
		return nil, fmt.Errorf("todos must be an array of {content,status} or a markdown checklist")
	}
	return items3, nil
}

// parseMarkdownChecklist 把 markdown checklist 文本解析为清单项：
// 识别 "- [ ]" / "- [x]" 复选框行与普通 "- " 清单行；其余行（标题/说明/空行）跳过。
func parseMarkdownChecklist(s string) []TodoItem {
	var items []TodoItem
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "- [x] ") || strings.HasPrefix(t, "- [X] ") ||
			strings.HasPrefix(t, "* [x] ") || strings.HasPrefix(t, "* [X] "):
			if c := strings.TrimSpace(t[6:]); c != "" {
				items = append(items, TodoItem{Content: c, Status: "completed"})
			}
		case strings.HasPrefix(t, "- [ ] ") || strings.HasPrefix(t, "* [ ] "):
			if c := strings.TrimSpace(t[6:]); c != "" {
				items = append(items, TodoItem{Content: c, Status: "pending"})
			}
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			if c := strings.TrimSpace(t[2:]); c != "" {
				items = append(items, TodoItem{Content: c, Status: "pending"})
			}
		}
	}
	return items
}

// ---------- new_task ----------

// SubtaskResult 特殊结果标记：引擎捕获后启动子任务（对齐 PendingTaskAction.create_subtask）。
// Background=true 时异步派发（codex spawn_agent 语义）：引擎立即返回 agent_id，
// 不阻塞当前轮次；结果稍后由 wait_agent 收割。
type SubtaskResult struct {
	Mode       string
	Message    string
	Todos      []TodoItem
	Background bool
	// Expect 期望产物相对路径列表（G1 对标 CCGS Trigger 0「验证工件而非回复」：
	// 子任务收尾时核验文件在子会话工作区真实存在，缺失记入失败原因回灌父任务；
	// 空 = 不核验，行为与旧一致）。
	Expect []string
}

func (e *SubtaskResult) Error() string {
	if e.Background {
		return "new_task: spawn background agent in mode " + e.Mode
	}
	return "new_task: delegate to mode " + e.Mode
}

type NewTaskTool struct {
	// Parallel 引擎是否支持并行 fan-out（通道层装配 SubtaskRunner 时置 true，
	// 如桌面端 buildRunEngine）。true：同一条消息可携带多个 new_task 并行
	// 执行；false：串行 mustBeLastTool 语义（须独立成轮）。
	Parallel bool
}

func (t *NewTaskTool) Name() string { return "new_task" }

// Description 按并行能力分支（P2.8 fan-out 语义透出给模型）：
//   - Parallel=true（通道层装配了 SubtaskRunner，如桌面端 buildRunEngine）：
//     明确告知模型可在同一条消息内携带多个 new_task 并行执行——引导
//     「用户提任务 → 分析拆分 → 无依赖子任务批量并行委派」工作流；
//   - Parallel=false（CLI/demo/flow 等无 SubtaskRunner 场景）：保持
//     Zoo-Code mustBeLastTool 语义文案（必须独立成轮），与引擎
//     engine.go 串行截断/拒绝行为一致。
func (t *NewTaskTool) Description() string {
	if t.Parallel {
		return "Create a new task instance in the chosen mode using your provided message and initial todo list (if required). " +
			"Use this for complex, multi-step projects that require coordination across different specialties, " +
			"or when you need to break down large tasks into subtasks. " +
			"PARALLEL EXECUTION: you may include MULTIPLE new_task tool calls in a single message - " +
			"they run concurrently, each in its own isolated subtask, and all results are returned to you together. " +
			"Prefer batching independent subtasks into one message; fall back to one-at-a-time only when subtasks depend on each other's results. " +
			"Give parallel subtasks exclusive file/package boundaries (never let two of them edit the same file). " +
			"The subtask runs in isolation; its final attempt_completion result is returned to you when it finishes. " +
			"Set background=true to run it asynchronously (returns an agent_id immediately; " +
			"collect results later with wait_agent). " +
			"If the subtask must produce files, declare them via expect (relative paths): the parent " +
			"verifies those files exist when the subtask finishes — a fluent summary without the " +
			"artifacts on disk fails."
	}
	return "Create a new task instance in the chosen mode using your provided message and initial todo list (if required). " +
		"Use this for complex, multi-step projects that require coordination across different specialties, " +
		"or when you need to break down large tasks into subtasks. " +
		"This tool MUST be called alone in a single message: no other tools may be used alongside it. " +
		"The subtask runs in isolation; its final attempt_completion result is returned to you when it finishes. " +
		"Set background=true to run it asynchronously (returns an agent_id immediately; " +
		"collect results later with wait_agent). " +
		"If the subtask must produce files, declare them via expect (relative paths): the parent " +
		"verifies those files exist when the subtask finishes — a fluent summary without the " +
		"artifacts on disk fails."
}

func (t *NewTaskTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"mode":       strProp("The slug of the mode to start the new task in (e.g. code, architect, debug)"),
		"message":    strProp("Initial user instructions or context for the new task"),
		"background": map[string]interface{}{"type": "boolean", "description": "Optional. If true, spawn the subtask as a background agent and return an agent_id immediately instead of blocking (codex spawn_agent semantics). Collect the result later with wait_agent."},
		"expect":     map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Optional list of relative file paths the subtask must produce (return contract). Verified on disk when the subtask finishes; a missing path fails the delegation with the missing paths named. Declare it whenever the subtask must write files — a fluent summary is not evidence that a phase ran."},
		"todos": map[string]interface{}{
			"type":        "array",
			"description": "Optional initial todo list for the subtask",
			"items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"content": strProp("Checklist item text"),
					"status": map[string]interface{}{
						"type": "string",
						"enum": []string{"pending", "in_progress", "completed"},
					},
				},
				"required": []string{"content", "status"},
			},
		},
	}, []string{"mode", "message"})
}

func (t *NewTaskTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Mode       string     `json:"mode"`
		Message    string     `json:"message"`
		Background bool       `json:"background"`
		Todos      []TodoItem `json:"todos"`
		Expect     []string   `json:"expect"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(p.Mode) == "" {
		return "", fmt.Errorf("missing required parameter: mode")
	}
	if strings.TrimSpace(p.Message) == "" {
		return "", fmt.Errorf("missing required parameter: message")
	}
	return "", &SubtaskResult{Mode: p.Mode, Message: p.Message, Todos: p.Todos, Background: p.Background, Expect: p.Expect}
}

// ---------- spawn_agent / wait_agent（codex-main 多智能体协作移植） ----------

// SpawnAgentResult 特殊结果标记：引擎捕获后异步派生后台子智能体，
// 立即返回 agent_id（对齐 codex-rs collab spawn_agent：返回 AgentThreadHandle）。
type SpawnAgentResult struct {
	Mode    string
	Message string
	Todos   []TodoItem
}

func (e *SpawnAgentResult) Error() string { return "spawn_agent: " + e.Mode }

type SpawnAgentTool struct{}

func (t *SpawnAgentTool) Name() string { return "spawn_agent" }

func (t *SpawnAgentTool) Description() string {
	return "Spawn a background agent that runs a task independently while you continue working " +
		"(multi-agent collaboration, ported from codex spawn_agent). " +
		"Returns an agent_id immediately; the agent runs in a child session of the current mode. " +
		"Later collect its result with wait_agent (blocks until the agent finishes). " +
		"Use for parallelizable, independent work: research, multi-file refactors, test batches. " +
		"Avoid for tiny tasks where delegation overhead exceeds the work. " +
		"This tool MUST be called alone in a single message."
}

func (t *SpawnAgentTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"mode":    strProp("The slug of the mode the agent runs in (defaults to the current mode's peer, e.g. code, ask, debug)"),
		"message": strProp("Task instructions for the agent"),
		"todos": map[string]interface{}{
			"type":        "array",
			"description": "Optional initial todo list for the agent",
			"items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"content": strProp("Checklist item text"),
					"status": map[string]interface{}{
						"type": "string",
						"enum": []string{"pending", "in_progress", "completed"},
					},
				},
				"required": []string{"content", "status"},
			},
		},
	}, []string{"message"})
}

func (t *SpawnAgentTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Mode    string     `json:"mode"`
		Message string     `json:"message"`
		Todos   []TodoItem `json:"todos"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(p.Message) == "" {
		return "", fmt.Errorf("missing required parameter: message")
	}
	return "", &SpawnAgentResult{Mode: p.Mode, Message: p.Message, Todos: p.Todos}
}

// WaitAgentResult 特殊结果标记：引擎捕获后阻塞收割后台智能体结果。
type WaitAgentResult struct {
	IDs []string // 空 = 等待全部在跑智能体
}

func (e *WaitAgentResult) Error() string {
	if len(e.IDs) == 0 {
		return "wait_agent: all running agents"
	}
	return "wait_agent: " + strings.Join(e.IDs, ",")
}

type WaitAgentTool struct{}

func (t *WaitAgentTool) Name() string { return "wait_agent" }

func (t *WaitAgentTool) Description() string {
	return "Wait for one or more background agents (spawned via spawn_agent or new_task background=true) " +
		"to finish and collect their results. Blocks until all requested agents complete, " +
		"then returns each agent's final attempt_completion result keyed by agent_id. " +
		"Omit agent_ids to wait for ALL currently running agents (codex wait_agent semantics). " +
		"This tool MUST be called alone in a single message."
}

func (t *WaitAgentTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"agent_ids": map[string]interface{}{
			"type":        "array",
			"description": "Agent ids to wait for. Omit or pass an empty array to wait for all running agents.",
			"items":       map[string]interface{}{"type": "string"},
		},
	}, nil)
}

func (t *WaitAgentTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		IDs []string `json:"agent_ids"`
	}
	if len(strings.TrimSpace(args)) > 0 {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	return "", &WaitAgentResult{IDs: p.IDs}
}

// ---------- switch_mode ----------

// SwitchModeResult 特殊结果标记：引擎捕获后切换当前模式。
type SwitchModeResult struct {
	ModeSlug string
	Reason   string
}

func (e *SwitchModeResult) Error() string { return "switch_mode: " + e.ModeSlug }

type SwitchModeTool struct{}

func (t *SwitchModeTool) Name() string { return "switch_mode" }

func (t *SwitchModeTool) Description() string {
	return "Request to switch to a different mode. This tool allows modes to request switching to another mode " +
		"when needed, such as switching to Code mode to make code changes. The user must approve the mode switch."
}

func (t *SwitchModeTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"mode_slug": strProp("The slug of the mode to switch to (e.g. code, architect, ask, debug, orchestrator)"),
		"reason":    strProp("Explanation for why the mode switch is needed"),
	}, []string{"mode_slug"})
}

func (t *SwitchModeTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		ModeSlug string `json:"mode_slug"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(p.ModeSlug) == "" {
		return "", fmt.Errorf("missing required parameter: mode_slug")
	}
	return "", &SwitchModeResult{ModeSlug: p.ModeSlug, Reason: p.Reason}
}

// ---------- skill ----------

// SkillLoader 技能加载函数（由 skills.Manager 适配注入，避免 tools→skills 反向依赖）。
type SkillLoader func(name string) (body string, err error)

// SkillFileReader 技能包内文件读取函数（读取 SKILL.md 指引的 references/ 材料）。
type SkillFileReader func(skill, rel string) (string, error)

// SkillNamesFn 可用技能名列表（错误提示用）。
type SkillNamesFn func() []string

// SkillToolsProvider 技能 → 当轮免审工具清单（D3 对标 Claude Code
// allowed-tools；nil = 无授权能力）。返回模式串原样传递
// （"shell" 精确 / "Bash(git add *)" 前缀形态，匹配在引擎授权判定处统一处理）。
type SkillToolsProvider func(skill string) []string

// SkillTool 技能加载工具（对齐 SkillTool.ts：按名加载 SKILL.md 正文进入上下文）。
type SkillTool struct {
	Loader SkillLoader
	Names  SkillNamesFn
	// ReadFile 可选：读取技能包内任意文件（如 references/guidelines.md）。
	// 提供后 Parameters 增加 file 参数，支持技能正文引导的二段式加载。
	ReadFile SkillFileReader
	// ToolsFor 可选：技能 → 当轮免审工具清单（D3）。与 OnAuthorize 配套：
	// Execute 成功且清单非空时调用 OnAuthorize 把授权写入引擎当轮授权表。
	ToolsFor SkillToolsProvider
	// OnAuthorize 可选：授权回调（引擎/宿主注入；skill=触发技能名）。
	OnAuthorize func(skill string, patterns []string)
}

func (t *SkillTool) Name() string { return "skill" }

func (t *SkillTool) Description() string {
	return "Load and execute a skill by name. Skills provide specialized instructions for common tasks. " +
		"Use this tool when you need to follow specific procedures documented in a skill. " +
		"Must match a skill name from the available skills list."
}

func (t *SkillTool) Parameters() map[string]interface{} {
	props := map[string]interface{}{
		"skill": strProp("Name of the skill to load"),
		"args":  strProp("Optional context or arguments to pass to the skill"),
	}
	if t.ReadFile != nil {
		props["file"] = strProp("Optional file inside the skill package (relative to the skill directory, e.g. references/guidelines.md). Omit to load the main SKILL.md body.")
	}
	return objectSchema(props, []string{"skill"})
}

func (t *SkillTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Skill string `json:"skill"`
		Args  string `json:"args"`
		File  string `json:"file"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if t.Loader == nil {
		return "", fmt.Errorf("skills system not configured")
	}
	// file 参数：读取技能包内指定文件（技能正文引导的第二段加载）。
	if strings.TrimSpace(p.File) != "" {
		if t.ReadFile == nil {
			return "", fmt.Errorf("skill file access not configured")
		}
		content, err := t.ReadFile(p.Skill, p.File)
		if err != nil {
			return "", fmt.Errorf("read skill file %q/%q: %w", p.Skill, p.File, err)
		}
		return content, nil
	}
	body, err := t.Loader(p.Skill)
	if err != nil {
		avail := "none"
		if t.Names != nil {
			if names := t.Names(); len(names) > 0 {
				avail = strings.Join(names, ", ")
			}
		}
		return "", fmt.Errorf("skill %q not found; available skills: %s", p.Skill, avail)
	}
	// D3 $ARGUMENTS 参数化（对标 Claude Code skills）：正文含占位符且参数非空
	// → 全量替换后直接返回（占位符已消费，不再尾部拼接参数说明，避免双份）。
	if p.Args != "" && strings.Contains(body, "$ARGUMENTS") {
		body = strings.ReplaceAll(body, "$ARGUMENTS", p.Args)
		t.grantTools(p.Skill)
		return body, nil
	}
	t.grantTools(p.Skill)
	return body, nil
}

// grantTools 技能执行成功后触发当轮工具授权（ToolsFor/OnAuthorize 均配置且
// 清单非空时调用；nil 安全，未配置 = 不授权，行为与旧版一致）。
func (t *SkillTool) grantTools(skill string) {
	if t.OnAuthorize != nil && t.ToolsFor != nil {
		if pats := t.ToolsFor(skill); len(pats) > 0 {
			t.OnAuthorize(skill, pats)
		}
	}
}
