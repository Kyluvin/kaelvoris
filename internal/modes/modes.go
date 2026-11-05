// Package modes 模式系统：移植自 Zoo-Code packages/types/src/mode.ts + src/shared/modes.ts。
//
// 核心语义对齐：
//   - 5 个内置模式（architect/code/ask/debug/orchestrator），每组工具组定义可用工具集
//   - 自定义模式覆盖内置模式（.kaelvoris/modes.json 或 .roomodes 兼容格式）
//   - switch_mode / new_task 工具始终可用（对齐 ALWAYS_AVAILABLE_TOOLS）
package modes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ToolGroup 工具组（对齐 toolGroups = ["read","edit","command","mcp","modes"]）。
type ToolGroup = string

const (
	GroupRead    ToolGroup = "read"
	GroupEdit    ToolGroup = "edit"
	GroupCommand ToolGroup = "command"
	GroupMCP     ToolGroup = "mcp"
	GroupModes   ToolGroup = "modes"
)

// GroupOptions 工具组附加选项（对齐 GroupEntry 元组第二元素）。
type GroupOptions struct {
	FileRegex   string `json:"fileRegex,omitempty"`
	Description string `json:"description,omitempty"`
}

// ModeConfig 模式配置（对齐 ModeConfig）。
type ModeConfig struct {
	Slug               string       `json:"slug"`
	Name               string       `json:"name"`
	RoleDefinition     string       `json:"roleDefinition"`
	WhenToUse          string       `json:"whenToUse,omitempty"`
	Description        string       `json:"description,omitempty"`
	Groups             []GroupEntry `json:"groups"`
	CustomInstructions string       `json:"customInstructions,omitempty"`
	Source             string       `json:"source,omitempty"` // "global" | "project"（运行时填充）
}

// GroupEntry 工具组条目：字符串或 [组名, 选项] 元组。
type GroupEntry interface{}

// groupEntry 解析后的组条目。
type groupEntry struct {
	Name    string
	Options *GroupOptions
}

// DefaultModes 内置 5 模式（移植 DEFAULT_MODES）。
func DefaultModes() []ModeConfig {
	return []ModeConfig{
		{
			Slug: "architect",
			Name: "🏗️ Architect",
			RoleDefinition: "You are Kaelvoris, an experienced technical leader who is inquisitive and an excellent planner. " +
				"Your goal is to gather information and get context to create a detailed plan for accomplishing the user's task, " +
				"which the user will review and approve before they switch into another mode to implement the solution.",
			WhenToUse: "Use this mode when you need to plan, design, or strategize before implementation. " +
				"Perfect for breaking down complex problems, creating technical specifications, designing system architecture, or brainstorming solutions before coding.",
			Description: "Plan and design before implementation",
			Groups:      []GroupEntry{"read", []interface{}{"edit", GroupOptions{FileRegex: `\.md$`, Description: "Markdown files only"}}, "mcp"},
			CustomInstructions: "1. Do some information gathering (using provided tools) to get more context about the task.\n\n" +
				"2. You should also ask the user clarifying questions to get a better understanding of the task.\n\n" +
				"3. Once you've gained more context about the user's request, break down the task into clear, actionable steps and create a todo list using the `update_todo_list` tool. Each todo item should be specific, actionable, listed in logical execution order, and clear enough that another mode could execute it independently.\n\n" +
				"4. As you gather more information or discover new requirements, update the todo list to reflect the current understanding.\n\n" +
				"5. Ask the user if they are pleased with this plan, or if they would like to make any changes.\n\n" +
				"6. Include Mermaid diagrams if they help clarify complex workflows or system architecture.\n\n" +
				"7. Use the switch_mode tool to request that the user switch to another mode to implement the solution.\n\n" +
				"Unless told otherwise, if you want to save a plan file, put it in the ./plans directory.",
		},
		{
			Slug: "code",
			Name: "💻 Code",
			RoleDefinition: "You are Kaelvoris, a highly skilled software engineer with extensive knowledge in many programming languages, " +
				"frameworks, design patterns, and best practices.",
			WhenToUse:   "Use this mode when you need to write, modify, or refactor code. Ideal for implementing features, fixing bugs, creating new files, or making code improvements across any programming language or framework.",
			Description: "Write, modify, and refactor code",
			Groups:      []GroupEntry{"read", "edit", "command", "mcp"},
		},
		{
			Slug: "ask",
			Name: "❓ Ask",
			RoleDefinition: "You are Kaelvoris, a knowledgeable technical assistant focused on answering questions and providing information " +
				"about software development, technology, and related topics.",
			WhenToUse:   "Use this mode when you need explanations, documentation, or answers to technical questions. Best for understanding concepts, analyzing existing code, getting recommendations, or learning about technologies without making changes.",
			Description: "Get answers and explanations",
			Groups:      []GroupEntry{"read", "mcp"},
			CustomInstructions: "You can analyze code, explain concepts, and access external resources. " +
				"Always answer the user's questions thoroughly, and do not switch to implementing code unless explicitly requested by the user.",
		},
		{
			Slug:           "debug",
			Name:           "🪲 Debug",
			RoleDefinition: "You are Kaelvoris, an expert software debugger specializing in systematic problem diagnosis and resolution.",
			WhenToUse:      "Use this mode when you're troubleshooting issues, investigating errors, or diagnosing problems. Specialized in systematic debugging, adding logging, analyzing stack traces, and identifying root causes before applying fixes.",
			Description:    "Diagnose and fix software issues",
			Groups:         []GroupEntry{"read", "edit", "command", "mcp"},
			CustomInstructions: "Reflect on 5-7 different possible sources of the problem, distill those down to 1-2 most likely sources, " +
				"and then add logs to validate your assumptions. Explicitly ask the user to confirm the diagnosis before fixing the problem.\n\n" +
				"Reproduce before you fix (SWE-agent workflow): 1) find and read the relevant code first; " +
				"2) write a minimal reproduction (script or failing test) and run it to confirm the error; " +
				"3) apply the fix; 4) re-run the reproduction to confirm it is resolved; " +
				"5) consider edge cases and make sure the fix handles them. " +
				"If the bug cannot be reproduced, say so and rely on the user-provided evidence instead of guessing.",
		},
		{
			Slug: "orchestrator",
			Name: "🪃 Orchestrator",
			RoleDefinition: "You are Kaelvoris, a strategic workflow orchestrator who coordinates complex tasks by delegating them to appropriate specialized modes. " +
				"You have a comprehensive understanding of each mode's capabilities and limitations, allowing you to effectively break down complex problems into discrete tasks.",
			WhenToUse:   "Use this mode for complex, multi-step projects that require coordination across different specialties. Ideal when you need to break down large tasks into subtasks, manage workflows, or coordinate work that spans multiple domains or expertise areas.",
			Description: "Coordinate tasks across multiple modes",
			Groups:      []GroupEntry{},
			CustomInstructions: "Your role is to coordinate complex workflows by delegating tasks to specialized modes. As an orchestrator, you should:\n\n" +
				"1. When given a complex task, FIRST analyze it and break it down into logical subtasks that can be delegated to appropriate specialized modes.\n\n" +
				"2. For each subtask, use the `new_task` tool to delegate. Provide comprehensive instructions including all necessary context, a clearly defined scope, " +
				"and an explicit statement that the subtask should only perform the work outlined and signal completion via attempt_completion with a concise summary.\n\n" +
				"3. PARALLELIZE: subtasks with no dependency on each other MUST be delegated together in a single message as multiple new_task tool calls - they run concurrently and results return together. Only chain new_task calls one-at-a-time when a subtask needs a previous subtask's result. Never let two parallel subtasks edit the same file; give each an exclusive file/package boundary.\n\n" +
				"4. Track and manage the progress of all subtasks. When a subtask is completed, analyze its results and determine the next steps.\n\n" +
				"5. When all subtasks are completed, synthesize the results and provide a comprehensive overview of what was accomplished.\n\n" +
				"Use subtasks to maintain clarity. If a request significantly shifts focus or requires a different expertise (mode), consider creating a subtask.",
		},
	}
}

// DefaultModeSlug 默认模式（对齐 modes[0].slug）。
const DefaultModeSlug = "code"

// toolGroupsMap 工具组 → 工具名（对齐 TOOL_GROUPS；P1/P2 工具名 fs_* 前缀）。
// design_system 随 read 组发放（浏览/加载设计体系只读，网站与前端开发用）。
// codebase_search 随 read 组发放（Zoo-Code CodebaseSearchTool 属只读检索；
// 工具仅向量化开关开启时注册，未注册时组内名单多出无副作用）。
var toolGroupsMap = map[ToolGroup][]string{
	GroupRead:    {"fs_read", "fs_list", "fs_search", "design_system", "design_template", "codebase_search", "web_search", "web_fetch", "arxiv_search", "pdf_read", "generate_image", "generate_video", "generate_speech"}, // 2026-10 wiring: web_tools.go was implemented but never registered; search tools (incl. academic) go with read group; design_template 随 read 组发放（open-design 0.23.1 全量融合，与 design_system 配套只读）// 2026-10 聊天对话支持：generate_image/generate_video/generate_speech 随 read 组发放——所有含 read 的模式（含 ask/architect）都能对话生图/视频/语音；命令执行类工具仍留在 command 组
	GroupEdit:    {"fs_write", "fs_apply_diff"},
		GroupCommand: {"shell", "unified_exec", "shell_output", "cron_job", "process_list", "file_locker", "http_request", "speech_to_text", "email_send", "notion", "browser_open", "browser_eval", "browser_screenshot", "browser_act", "browser_snapshot", "browser_tabs", "desktop_windows", "desktop_screenshot", "desktop_act"}, // 2026-10 capability gap-fill: STT/mail/notion/browser-automation with command group // codex 对齐补全 ⑤：持久 shell 会话；shell_output 轮询后台命令输出；cron_job 定时任务聊天管理（内部 MCP 集成）；诊断三件套随命令组发放（进程/端口、文件占用、HTTP 探测均属运维诊断场景）// 2026-10 深度嵌入：browser_act/snapshot/tabs（Codex BrowserUse 能力面）与 desktop_windows/screenshot/act（Codex ComputerUse Windows 面）随 command 组发放
	GroupMCP:     {},                                                                                                                                                                                                   // MCP 动态工具以 mcp_ 前缀注册，运行时按前缀放行（⑦）
	GroupModes:   {"switch_mode", "new_task", "spawn_agent", "wait_agent"},                                                                                                                                             // codex collab 对齐：多智能体协作工具
}

// AlwaysAvailable 始终可用工具（对齐 ALWAYS_AVAILABLE_TOOLS）。
// 注意：仅注册（buildRunEngine 注册 EvolveTool/RoadmapProposeTool）但不进工具组
// 的“内部 MCP 风格”工具，Agent 永远收不到——本列表是唯一的兜底发放点。
// 实测缺陷：evolve_record 曾仅在 buildRunEngine 注册，cron 定时自检会话工具列表
// 无此工具，Agent 只能直写 cards.json，被进程内陈旧内存态覆盖（反复回滚），
// 2026-10-08 复盘修复：加入本列表发放。
var AlwaysAvailable = []string{
	"ask_followup_question",
	"attempt_completion",
	"update_todo_list",
	"skill",
	"switch_mode",
	"new_task",
	"spawn_agent",
	"wait_agent",
	"evolve_record",
}

// GroupInfo 提取 GroupEntry 的组名与组选项（对齐 getGroupName + group options 解包）。
// entry 可以是 "read" 或 []interface{}{"edit", GroupOptions{FileRegex, Description}}。
func GroupInfo(entry GroupEntry) (string, *GroupOptions) {
	switch v := entry.(type) {
	case string:
		return v, nil
	case []interface{}:
		if len(v) >= 1 {
			name, _ := v[0].(string)
			var opts *GroupOptions
			if len(v) >= 2 {
				if o, ok := v[1].(GroupOptions); ok {
					opts = &o
				}
			}
			return name, opts
		}
	}
	return "", nil
}

// groupName 提取 GroupEntry 的组名。
func groupName(entry GroupEntry) string {
	name, _ := GroupInfo(entry)
	return name
}

// ToolsForMode 返回模式可用工具（对齐 getToolsForMode）。
func ToolsForMode(groups []GroupEntry) []string {
	set := map[string]bool{}
	for _, g := range groups {
		name := groupName(g)
		for _, t := range toolGroupsMap[name] {
			set[t] = true
		}
	}
	for _, t := range AlwaysAvailable {
		set[t] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	return out
}

// Manager 模式管理器：内置模式 + 自定义覆盖。
type Manager struct {
	custom []ModeConfig
}

// NewManager 创建管理器。
func NewManager() *Manager {
	return &Manager{}
}

// LoadProject 加载工作区自定义模式（.kaelvoris/modes.json 或 .roomodes 兼容）。
func (m *Manager) LoadProject(workspace string) error {
	candidates := []string{
		filepath.Join(workspace, ".kaelvoris", "modes.json"),
		filepath.Join(workspace, ".roomodes"),
	}
	for _, p := range candidates {
		b, err := readIfExist(p)
		if err != nil {
			return err
		}
		if b == nil {
			continue
		}
		var customs []ModeConfig
		if err := json.Unmarshal(b, &customs); err != nil {
			return fmt.Errorf("parse %s: %w", p, err)
		}
		for i := range customs {
			customs[i].Source = "project"
		}
		m.custom = append(m.custom, customs...)
	}
	return nil
}

// GetBySlug 按 slug 查找（自定义优先，对齐 getModeBySlug）。
func (m *Manager) GetBySlug(slug string) (ModeConfig, bool) {
	for _, c := range m.custom {
		if c.Slug == slug {
			return c, true
		}
	}
	for _, d := range DefaultModes() {
		if d.Slug == slug {
			return d, true
		}
	}
	return ModeConfig{}, false
}

// All 返回全部模式（自定义覆盖内置，对齐 getAllModes）。
func (m *Manager) All() []ModeConfig {
	all := DefaultModes()
	for _, c := range m.custom {
		replaced := false
		for i := range all {
			if all[i].Slug == c.Slug {
				all[i] = c
				replaced = true
				break
			}
		}
		if !replaced {
			all = append(all, c)
		}
	}
	return all
}

// ToolsFor 返回指定模式的可用工具名列表。
func (m *Manager) ToolsFor(slug string) []string {
	cfg, ok := m.GetBySlug(slug)
	if !ok {
		return ToolsForMode(nil)
	}
	return ToolsForMode(cfg.Groups)
}

// readIfExist 读取文件（不存在返回 nil）。
func readIfExist(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}
