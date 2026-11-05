// Package agent 系统提示词组装。
//
// 原样移植自 Zoo-Code 3.82.2 src/core/prompts/system.ts 的组装管线与
// src/core/prompts/sections/* 各段落文本（英文原文逐字保留），组装顺序：
//
//	roleDefinition
//	markdownFormattingSection
//	getSharedToolUseSection
//	getToolUseGuidelinesSection(policy)
//	getCapabilitiesSection(policy)
//	getModesSection
//	getSkillsSection（skill 工具可用且存在技能时）
//	getRulesSection(cwd, policy)
//	getSystemInfoSection(cwd, policy)
//	getObjectiveSection(policy)
//	CURRENT TODO LIST（kaelvoris 扩展）
//	SUBTASK CONTEXT（kaelvoris 扩展）
//	addCustomInstructions
//
// 适配点（"原样复制过来进行适配"）：
//   - 工具名映射：文本中的 Zoo-Code 工具名（execute_command/read_file/
//     write_to_file/apply_diff/list_files/search_files）统一替换为 kaelvoris
//     工具名（shell/fs_read/fs_write/fs_apply_diff/fs_list/fs_search）；
//   - EffectiveToolPolicy：工具相关片段仅在该工具位于当前模式可用集中时输出。
package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"kaelvoris/internal/modes"
)

// PromptOptions 系统提示词组装选项。
type PromptOptions struct {
	// EmployeeRole 数字员工角色段完整文本（§5；空 = 无员工接管）。
	// 非空时置于 roleDefinition 之后，作为第一优先级行为指令注入。
	EmployeeRole string
	ToolNames    []string          // 当前模式可用工具名（已按模式过滤；nil = 不过滤）
	Mode         *modes.ModeConfig // 当前模式（nil = 默认 code 语义）
	// AllModes MODES 段展示的全部模式（空 = modes.DefaultModes()）。
	AllModes []modes.ModeConfig
	// SkillsXML <available_skills> XML（不含协议段；协议由本文件补齐，可为空）。
	SkillsXML string
	// VectorKBDir 向量知识库已索引目录（非空 = 注入 VECTOR KNOWLEDGE BASE
	// 段：写码/答题前先用 codebase_search 语义检索。Zoo-Code code-index
	// 「向量回答」语义；空 = 不注入）。
	VectorKBDir string
	// TodosMD 当前任务清单 markdown（可为空；kaelvoris 扩展段）。
	TodosMD string
	// MemoryMD 会话记忆文档内容（P3.1-C；空 = 无记忆段）。
	MemoryMD string
	// MemoryPath 会话记忆文档绝对路径（提示词中告知引擎可读写维护）。
	MemoryPath string
	// WSDoc / WSPath 项目工作空间记忆内容与绝对路径（跨会话共享；空 = 不注入）。
	WSDoc  string
	WSPath string
	// EvolveSnippet 自进化经验段（<self_evolution>；空 = 不注入。
	// 由通道层按任务文本命中经验卡生成，实现编码能力沉淀闭环）。
	EvolveSnippet string
	// ManualPreview 会话卡片手动摘要（规划板 2026-10 会话卡片信息升级；空 = 不注入。
	// 用户在侧栏会话卡设置的标注，随任务注入提示词，供模型尊重用户标注）。
	ManualPreview string
	// IsSubtask 子任务上下文（new_task 委派）。
	IsSubtask bool
	// AutonomousDev 自主开发守则段覆盖（设置面板「提示词」autonomousDev 段，
	// 非空时整段替换内置 AUTONOMOUS DEVELOPMENT 段；空 = 用内置默认）。
	AutonomousDev string
	// PromptOverride 用户自定义提示词覆盖（设置面板「提示词」配置，非空时
	// 以最高优先级注入系统提示词末尾；空 = 不注入）。
	PromptOverride string
	// ParallelDelegation 并行子任务能力（P2.8 fan-out，通道层装配
	// SubtaskRunner 时为 true）。true 且非子任务时注入 PARALLEL DELEGATION
	// 指引段，引导「分析拆分 → 无依赖子任务同轮并行委派」。
	ParallelDelegation bool
	// AutoDecide 全AI自动决策（用户需求 2026-10；引擎 AutoDecide 同步注入）：
	// true 时 RULES 段追加自主决策指引——不要调用 ask_followup_question 等用户
	// 回答（调用也不会阻塞），结合上下文自主推演选择并在结果中声明假设。
	AutoDecide bool
	// MCPLazyHint MCP 惰性加载提示词段（宿主组装；空 = 不注入）。内容为
	// 未激活 MCP 服务器目录 + mcp_activate 激活指引（非空时由 SystemPrompt
	// 注入 MCP TOOL CATALOG 段，对齐 Claude Code Tool Search deferred loading）。
	MCPLazyHint string
	// CWD 项目根目录（注入 RULES 与 SYSTEM INFORMATION；空 = os.Getwd()）。
	CWD string
	// Language 用户语言偏好代码（如 "zh-CN"；空 = 不注入语言偏好）。
	Language string
	// GlobalCustomInstructions 全局自定义指令（对齐 globalCustomInstructions）。
	GlobalCustomInstructions string
}

// ---------------------------------------------------------------------------
// EffectiveToolPolicy 等价物（对齐 src/core/prompts/tools/effective-tool-policy）：
// 提示词片段按当前模式实际可用工具门控，保证文案与可调用工具一致。
// ---------------------------------------------------------------------------

// canonicalToolAlias kaelvoris 工具名 → Zoo-Code 规范工具名（门控等价映射）。
var canonicalToolAlias = map[string]string{
	"shell":         "execute_command",
	"fs_list":       "list_files",
	"fs_search":     "search_files",
	"fs_read":       "read_file",
	"fs_write":      "write_to_file",
	"fs_apply_diff": "apply_diff",
}

// defaultCanonicalTools 未配置模式管理器时的全量能力（对齐 P1 无过滤语义）。
var defaultCanonicalTools = []string{
	"execute_command", "list_files", "search_files", "read_file",
	"write_to_file", "apply_diff",
	"ask_followup_question", "attempt_completion", "update_todo_list",
	"new_task", "switch_mode", "skill",
}

// editRestriction 编辑约束（对齐 policy.editRestriction）。
type editRestriction struct {
	fileRegex   string
	description string
}

// toolPolicy 请求级有效工具策略。
type toolPolicy struct {
	tools           map[string]bool
	editRestriction *editRestriction
	hasMcpGroup     bool
	hasMcpTools     bool // P4 接入 MCP Hub 后填充
	hasMcpResources bool
}

// buildToolPolicy 由模式可用工具集构造门控策略。
// MCP 修复（对齐 Zoo-Code effective-tool-policy.ts resolveHasMcpTools）：
// 模式含 mcp 组且存在 MCP 动态工具（mcp_ 前缀，注册中心实连后注入）时
// 置 hasMcpTools=true，驱动 CAPABILITIES/RULES 段的 MCP 能力句——此前
// hasMcpTools 恒为 false（"P4 接入 MCP Hub 后填充"），提示词从不向模型
// 声明 MCP 能力，模型自然不会尝试调用。
func buildToolPolicy(toolNames []string, modeCfg *modes.ModeConfig) toolPolicy {
	p := toolPolicy{tools: map[string]bool{}}
	if toolNames == nil {
		// 未过滤 = 注册中心全量能力（对齐无模式管理器语义）。
		for _, t := range defaultCanonicalTools {
			p.tools[t] = true
		}
	} else {
		for _, n := range toolNames {
			c := n
			if a, ok := canonicalToolAlias[n]; ok {
				c = a
			}
			p.tools[c] = true
		}
	}
	// MCP 动态工具在 ToolNames 中以 mcp_ 前缀出现（toolDefs 同步按前缀
	// 放行，见 engine.go toolDefs），此处据此点亮提示词 MCP 能力段。
	for _, n := range toolNames {
		if strings.HasPrefix(n, "mcp_") {
			p.hasMcpTools = true
			break
		}
	}
	if modeCfg != nil {
		for _, g := range modeCfg.Groups {
			name, opts := modes.GroupInfo(g)
			if name == modes.GroupEdit && opts != nil && opts.FileRegex != "" {
				p.editRestriction = &editRestriction{fileRegex: opts.FileRegex, description: opts.Description}
			}
			if name == modes.GroupMCP {
				p.hasMcpGroup = true
			}
		}
	}
	return p
}

// toolRename 将段落文本中的 Zoo-Code 工具名适配为 kaelvoris 工具名。
var toolRename = strings.NewReplacer(
	"execute_command", "shell",
	"list_files", "fs_list",
	"search_files", "fs_search",
	"read_file", "fs_read",
	"write_to_file", "fs_write",
	"apply_diff", "fs_apply_diff",
)

// ==== sections/markdown-formatting.ts（原文逐字）====

func markdownFormattingSection() string {
	return `====

MARKDOWN RULES

ALL responses MUST show ANY ` + "`language construct`" + ` OR filename reference as clickable, exactly as [` + "`filename OR language.declaration()`" + `](relative/file/path.ext:line); line is required for ` + "`syntax`" + ` and optional for filename links. This applies to ALL markdown responses and ALSO those in attempt_completion`
}

// ==== sections/tool-use.ts（原文逐字）====

func getSharedToolUseSection() string {
	return `====

TOOL USE

You have access to a set of tools that are executed upon the user's approval. Use the provider-native tool-calling mechanism. Do not include XML markup or examples. You must call at least one tool per assistant response. Prefer calling as many tools as are reasonably needed in a single response to reduce back-and-forth and complete tasks faster.`
}

// ==== sections/tool-use-guidelines.ts（原文逐字；list 示例按可用性门控）====

func getToolUseGuidelinesSection(policy toolPolicy) string {
	listExample := ""
	if policy.tools["list_files"] {
		listExample = " For example using the list_files tool is more effective than running a command like `ls` in the terminal."
	}

	return `# Tool Use Guidelines

1. Assess what information you already have and what information you need to proceed with the task.
2. Choose the most appropriate tool based on the task and the tool descriptions provided. Assess if you need additional information to proceed, and which of the available tools would be most effective for gathering this information.` + listExample + ` It's critical that you think about each available tool and use the one that best fits the current step in the task.
3. If multiple actions are needed, you may use multiple tools in a single message when appropriate, or use tools iteratively across messages. Each tool use should be informed by the results of previous tool uses. Do not assume the outcome of any tool use. Each step must be informed by the previous step's result.

By carefully considering the user's response after tool executions, you can react accordingly and make informed decisions about how to proceed with the task. This iterative process helps ensure the overall success and accuracy of your work.
4. Multiple agent sessions may run concurrently in this workspace. Never issue an edit (fs_apply_diff/fs_write) based on file content you have not read in THIS session, and never retry a failed edit with the identical SEARCH block — if a SEARCH fails, the file has changed: re-read the target region first, then rebuild the edit from the fresh content.`
}

// ==== sections/capabilities.ts（原文逐字；片段按 policy 门控）====

func getCapabilitiesSection(policy toolPolicy) string {
	hasEditTool := policy.tools["write_to_file"] || policy.tools["apply_diff"] || policy.tools["edit"]

	var clauses []string
	if policy.tools["execute_command"] {
		clauses = append(clauses, "execute CLI commands on the user's computer")
	}
	if policy.tools["list_files"] {
		clauses = append(clauses, "list files")
	}
	if policy.tools["codebase_search"] {
		clauses = append(clauses, "semantically search the codebase")
	}
	if policy.tools["search_files"] {
		clauses = append(clauses, "regex search")
	}
	if policy.tools["read_file"] {
		clauses = append(clauses, "read files")
	}
	if hasEditTool {
		clauses = append(clauses, "write and edit files")
	}
	if policy.tools["generate_image"] {
		clauses = append(clauses, "generate images from text or reference images (generate_image: text-to-image & image-to-image)")
	}
	if policy.tools["generate_video"] {
		clauses = append(clauses, "generate videos from text or animate images (generate_video: text-to-video & image-to-video with first/last-frame or reference-image control)")
	}
	if policy.tools["generate_speech"] {
		clauses = append(clauses, "synthesize spoken audio from text (generate_speech / TTS)")
	}

	capabilitySentence := "You have access to a limited set of tools for this mode; only the tools you are provided may be called."
	if len(clauses) > 0 {
		capabilitySentence = "You have access to tools that let you " + strings.Join(clauses, ", ") + "."
	}

	editRestrictionSuffix := ""
	if hasEditTool && policy.editRestriction != nil {
		s := " (in this mode only files matching '" + policy.editRestriction.fileRegex + "' can be edited"
		if policy.editRestriction.description != "" {
			s += " — " + policy.editRestriction.description
		}
		s += ")"
		editRestrictionSuffix = s
	}

	var b strings.Builder
	b.WriteString(capabilitySentence + editRestrictionSuffix + "\n")
	b.WriteString("- These tools help you accomplish tasks.\n")
	if policy.tools["list_files"] {
		b.WriteString(`- If you need to further explore directories such as outside the current workspace directory, you can use the list_files tool. If you pass 'true' for the recursive parameter, it will list files recursively. Otherwise, it will list files at the top level, which is better suited for generic directories where you don't necessarily need the nested structure, like the Desktop.` + "\n")
	}
	if policy.tools["execute_command"] {
		b.WriteString(`- You can use the execute_command tool to run commands on the user's computer whenever you feel it can help accomplish the user's task. When you need to execute a CLI command, you must provide a clear explanation of what the command does. Prefer to execute complex CLI commands over creating executable scripts, since they are more flexible and easier to run. Interactive and long-running commands are allowed, since the commands are run in the user's VSCode terminal. The user may keep commands running in the background and you will be kept updated on their status along the way. Each command you execute is run in a new terminal instance.` + "\n")
	}
	if policy.hasMcpGroup && (policy.hasMcpTools || policy.hasMcpResources) {
		b.WriteString(`- You have access to MCP servers that may provide additional tools and/or resources actually available to this mode. Each server may provide different capabilities that you can use to accomplish tasks more effectively.` + "\n")
	}
	// kaelvoris 扩展：聊天媒体生成能力句（配置了生图/视频/语音模型后，用户在
	// 聊天里说「画一张…/生成一段…视频/朗读这段话」时模型应直接调工具出媒体，
	// 而非只给文字描述或指挥用户去别的页面操作）。
	if policy.tools["generate_image"] || policy.tools["generate_video"] || policy.tools["generate_speech"] {
		b.WriteString(`- Media generation tools (generate_image / generate_video / generate_speech) render their output (image/video/audio) directly in the chat stream. When the user asks to draw/paint/generate a picture, create a video clip, or have text read aloud, call the matching media tool directly — do not just describe the result in words. generate_image accepts an image array (reference images / character consistency), and generate_video accepts mode=keyframe (first_frame/last_frame) or mode=reference (images) to animate generated images into video. For multi-shot story videos (分镜短视频：拆分/文案/人物/场景), load the storyboard-video skill first. When the video model is not configured, generate_video still works: pass frame images (first_frame/last_frame/images) and it stitches them into a looping animated GIF locally for free (the tool result tells you when this fallback fired — mention it to the user). If the tool reports that generation is not configured, tell the user to set it up in 设置 → 模型 (图像生成 / 视频生成 / 语音合成).` + "\n")
	}

	body := strings.TrimSuffix(b.String(), "\n")
	return "====\n\nCAPABILITIES\n\n" + body
}

// ==== kaelvoris 扩展：sections/design-systems（移植自 open-design-main 的
// design-systems 目录组合语义：选中一个设计体系后，其设计上下文进入提示词）====

func getDesignSystemsSection(policy toolPolicy) string {
	if !policy.tools["design_system"] {
		return ""
	}
	// open-design 0.23.1 全量融合（2026-10）：design_system 现为 152 包全量目录
	//（含 components.html 组件样例 / USAGE.md / design-tokens.json 扩展件），
	// 另配套 design_template 模板工作流工具（design-templates 全量）。
	tplBlock := ""
	if policy.tools["design_template"] {
		tplBlock = `

TEMPLATES (design_template tool): when the task matches a template scenario — web prototype, deck/presentation (html-ppt-*), live dashboard, wireframe, social card, mobile flow — call design_template (no parameters) to browse the bundled catalog, then load the matching template with its ` + "`id`" + `. Follow the template's SKILL.md workflow strictly: read its seed (assets/template.html) and references/ via design_template ` + "`id`" + `+` + "`path`" + ` BEFORE writing code, and paste section layouts from its reference library instead of re-authoring CSS from scratch. Combine: design_system supplies the tokens, design_template supplies the structure.`
	}
	return `====

FRONTEND DESIGN SYSTEMS

You have a design_system tool backed by a bundled catalog of portable design-system packages (design philosophy, prompts, semantic style tokens, component specs; most packages also ship components.html sample markup and USAGE.md — read them via design_system id + the package file list when you need concrete component examples).

When the task involves building a website, web page, landing page, dashboard, UI component, or any frontend visuals:

1. BROWSE: call design_system with no parameters to list all bundled systems (grouped by category: Productivity & SaaS, Minimal, Corporate, Creative, ...).
2. SELECT: pick ONE system that best matches the product's personality and the user's intent. Ask the user via ask_followup_question when the choice is not obvious.
3. LOAD: call design_system with the chosen ` + "`id`" + ` to get its full specification — DESIGN.md (visual theme, colors, typography, components, spacing, responsiveness) and tokens.css (semantic CSS custom properties).
4. APPLY: implement the frontend strictly with the loaded system:
   - Use the tokens.css custom properties (--bg, --fg, --accent, --radius-*, --space-*, ...) as the single source of truth; define them once in your stylesheet and reference them everywhere.
   - Follow DESIGN.md component specs (buttons, cards, inputs, badges, navigation) rather than inventing ad-hoc styles.
   - Keep the system's visual language: color roles, type scale, letter-spacing, borders, elevation, whitespace philosophy, and responsive breakpoints.
   - If the system ships a Tailwind v4 mapping and the project uses Tailwind, use its @theme tokens.
5. CONSISTENCY: never mix tokens from multiple design systems in one page unless the user explicitly asks for a blend.` + tplBlock + `

For non-visual work (backend, CLI, tooling, config), do not load a design system.`
}

// ==== sections/modes.ts（原文逐字）====

func getModesSection(all []modes.ModeConfig) string {
	if len(all) == 0 {
		all = modes.DefaultModes()
	}
	lines := make([]string, 0, len(all))
	for _, m := range all {
		var description string
		if s := strings.TrimSpace(m.WhenToUse); s != "" {
			// Use whenToUse as the primary description, indenting subsequent lines for readability
			description = strings.ReplaceAll(s, "\n", "\n    ")
		} else {
			// Fallback to the first sentence of roleDefinition if whenToUse is not available
			description = strings.Split(m.RoleDefinition, ".")[0]
		}
		lines = append(lines, `  * "`+m.Name+`" mode (`+m.Slug+`) - `+description)
	}
	return `====

MODES

- These are the currently available modes:
` + strings.Join(lines, "\n")
}

// ==== sections/skills.ts（原文逐字；engine 侧仅提供 <available_skills> XML）====

func getSkillsSection(skillsXML, currentMode string, policy toolPolicy) string {
	// The protocol in this section mandates the `skill` tool; if it's not available
	// the section would be unhelpful/unactionable, so emit nothing.
	if !policy.tools["skill"] || strings.TrimSpace(skillsXML) == "" {
		return ""
	}
	return `====

AVAILABLE SKILLS

` + strings.TrimSpace(skillsXML) + `

<mandatory_skill_check>
REQUIRED PRECONDITION

Before producing ANY user-facing response, you MUST perform a skill applicability check.

Step 1: Skill Evaluation
- Evaluate the user's request against ALL available skill <description> entries in <available_skills>.
- Determine whether at least one skill clearly and unambiguously applies.

Step 2: Branching Decision

<if_skill_applies>
- Select EXACTLY ONE skill.
- Prefer the most specific skill when multiple skills match.
- Use the skill tool to load the skill by name.
- Load the skill's instructions fully into context BEFORE continuing.
- Follow the skill instructions precisely.
- Do NOT respond outside the skill-defined flow.
</if_skill_applies>

<if_no_skill_applies>
- Proceed with a normal response.
- Do NOT load any SKILL.md files.
</if_no_skill_applies>

CONSTRAINTS:
- Do NOT load every skill up front.
- Load skills ONLY after a skill is selected.
- Do NOT reload a skill whose instructions already appear in this conversation.
- Do NOT skip this check.
- FAILURE to perform this check is an error.
</mandatory_skill_check>

<linked_file_handling>
- When a skill is loaded, ONLY the skill instructions are present.
- Files linked from the skill are NOT loaded automatically.
- The model MUST explicitly decide to read a linked file based on task relevance.
- Do NOT assume the contents of linked files unless they have been explicitly read.
- Prefer reading the minimum necessary linked file.
- Avoid reading multiple linked files unless required.
- Treat linked files as progressive disclosure, not mandatory context.
</linked_file_handling>

<context_notes>
- The skill list is already filtered for the current mode: "` + currentMode + `".
- Mode-specific skills may come from skills-` + currentMode + `/ with project-level overrides taking precedence over global skills.
</context_notes>

<internal_verification>
This section is for internal control only.
Do NOT include this section in user-facing output.

After completing the evaluation, internally confirm:
<skill_check_completed>true|false</skill_check_completed>
</internal_verification>
`
}

// ==== sections/rules.ts（原文逐字；链式操作符按 shell 平台化）====

// getShellPath 对齐 utils/shell.getShell 的平台语义。
// 2026-10-01 会话复盘修正：Windows 下 shell / unified_exec 工具的实际执行器
// 是 cmd /C（见 shell_tools.go Execute 与 unified_exec.go newShellSession），
// 此前提示词宣称 Default Shell 为 powershell.exe，模型据此写 PowerShell
// 管道语法（Get-ChildItem | Select-Object 等）直接投给 cmd /C 执行，产生
// 大量 "'Select-Object' is not recognized" 类错误且诱导模型反复换写法重试。
// 提示词必须与实际执行器一致：默认 cmd.exe；KAELVORIS_SHELL 环境变量可覆盖。
func getShellPath() string {
	if runtime.GOOS == "windows" {
		if s := strings.TrimSpace(os.Getenv("KAELVORIS_SHELL")); s != "" {
			return s
		}
		return "C:\\Windows\\System32\\cmd.exe"
	}
	if s := strings.TrimSpace(os.Getenv("SHELL")); s != "" {
		return s
	}
	return "/bin/bash"
}

// getCommandChainOperator 对齐 sections/rules.ts:getCommandChainOperator。
// Windows 默认执行器为 cmd.exe（见 getShellPath 注释），链操作符用 &&
// （条件执行：前一条失败则短路，与实测行为一致）。
func getCommandChainOperator() string {
	shell := strings.ToLower(getShellPath())
	if strings.Contains(shell, "powershell") || strings.Contains(shell, "pwsh") {
		return ";"
	}
	return "&&"
}

// getCommandChainNote 对齐 sections/rules.ts:getCommandChainNote。
// Windows 默认执行器为 cmd.exe：链操作符用 &&，同时明确告知 PowerShell
// 单命令（powershell -Command "..."）会被工具层 argv 化直跑（shell_tools.go
// isPurePowerShellCommand），脚本串内可用完整 PowerShell 语法（管道/变量），
// 多语句复杂脚本建议写入 .ps1 临时脚本再执行——这是历史会话中
// "powershell -NoProfile -Command" 调用 663 次成为第一高频工具的根因缓解。
func getCommandChainNote() string {
	shell := strings.ToLower(getShellPath())
	if strings.Contains(shell, "powershell") || strings.Contains(shell, "pwsh") {
		return "Note: Using `;` for PowerShell command chaining. For bash/zsh use `&&`, for cmd.exe use `&&`. IMPORTANT: When using PowerShell, avoid Unix-specific utilities like `sed`, `grep`, `awk`, `cat`, `rm`, `cp`, `mv`. Instead use PowerShell equivalents: `Select-String` for grep, `Get-Content` for cat, `Remove-Item` for rm, `Copy-Item` for cp, `Move-Item` for mv, and PowerShell's `-replace` operator or `[regex]` for sed."
	}
	return "Note: Using `&&` for cmd.exe command chaining (conditional execution). For bash/zsh use `&&`. IMPORTANT: When using cmd.exe, avoid Unix-specific utilities like `sed`, `grep`, `awk`, `cat`, `rm`, `cp`, `mv`. Use built-in commands like `type` for cat, `del` for rm, `copy` for cp, `move` for mv, `find`/`findstr` for grep, or consider using PowerShell commands instead. To run PowerShell syntax (pipelines like `Get-ChildItem | Select-Object`, `$env:` variables), wrap it as a single command: `powershell -NoProfile -Command \"...\"` — the tool layer runs it directly (argv passthrough) so full PowerShell syntax works inside the quotes. For complex multi-statement scripts, write a temporary .ps1 file first, then execute it with `powershell -NoProfile -ExecutionPolicy Bypass -File script.ps1`."
}

// getRulesSection 构建 RULES 段（原文逐字；工具片段按 policy 门控）。
func getRulesSection(cwd string, policy toolPolicy, autoDecide bool) string {
	cwdPosix := filepath.ToSlash(cwd)
	chainOp := getCommandChainOperator()
	chainNote := getCommandChainNote()

	hasExecuteCommand := policy.tools["execute_command"]
	hasAskFollowupQuestion := policy.tools["ask_followup_question"]
	hasListFiles := policy.tools["list_files"]
	hasReadFile := policy.tools["read_file"]
	hasAttemptCompletion := policy.tools["attempt_completion"]
	hasEditTool := policy.tools["apply_diff"] || policy.tools["write_to_file"] || policy.tools["edit"] ||
		policy.tools["search_replace"] || policy.tools["edit_file"] || policy.tools["apply_patch"]

	var rules []string

	rules = append(rules, "The project base directory is: "+cwdPosix)

	if hasExecuteCommand {
		rules = append(rules, "All file paths must be relative to this directory. However, commands may change directories in terminals, so respect working directory specified by the response to execute_command.")
	} else {
		rules = append(rules, "All file paths must be relative to this directory.")
	}

	rules = append(rules, "You are stuck operating from '"+cwdPosix+"', so be sure to pass in the correct 'path' parameter when using tools that require a path.")

	rules = append(rules, "Do not use the ~ character or $HOME to refer to the home directory.")

	if hasExecuteCommand {
		r := "Before using the execute_command tool, you must first think about the SYSTEM INFORMATION context provided to understand the user's environment and tailor your commands to ensure they are compatible with their system. You must also consider if the command you need to run should be executed in a specific directory outside of the current working directory '" + cwdPosix + "', and if so prepend with `cd`'ing into that directory " + chainOp + " then executing the command (as one command since you are stuck operating from '" + cwdPosix + "'). For example, if you needed to run `npm install` in a project outside of '" + cwdPosix + "', you would need to prepend with a `cd` i.e. pseudocode for this would be `cd (path to project) " + chainOp + " (command, in this case npm install)`."
		if chainNote != "" {
			r += " " + chainNote
		}
		rules = append(rules, r)
	}

	// kaelvoris 扩展：收尾门禁（2026-10 会话复盘：go build/test 验证经常被留成
	// 未完成待办，实现收尾前未验证即 attempt_completion）。项目根存在 go.mod
	// 时要求：调用 attempt_completion 前必须先执行 go build ./... 与 go vet/、go test ./...
	// 验证（至少改动涉及的包），未通过不得收尾——防实现与实际验证脱节。
	if hasGoProject(cwd) {
		rules = append(rules, "Before calling attempt_completion to finish a coding task, you MUST first run the project's build/test gate: `go build ./...` (and `go vet ./...` + `go test ./...` for the packages you touched) in the workspace root, and confirm they pass. If the gates fail, fix the issues first - do NOT mark the task complete via attempt_completion while the code does not build or tests do not pass.")
	}

	if hasEditTool && policy.editRestriction != nil {
		rules = append(rules, "Some modes have restrictions on which files they can edit. If you attempt to edit a restricted file, the operation will be rejected with a FileRestrictionError that will specify which file patterns are allowed for the current mode.")
	}

	rules = append(rules,
		"Be sure to consider the type of project (e.g. Python, JavaScript, web application) when determining the appropriate structure and files to include. Also consider what files may be most relevant to accomplishing the task, for example looking at a project's manifest file would help you understand the project's dependencies, which you could incorporate into any code you write.",
		"When making changes to code, always consider the context in which the code is being used. Ensure that your changes are compatible with the existing codebase and that they follow the project's coding standards and best practices.",
	)

	if hasAttemptCompletion {
		rules = append(rules, "Do not ask for more information than necessary. Use the tools provided to accomplish the user's request efficiently and effectively. When you've completed your task, you must use the attempt_completion tool to present the result to the user. The user may provide feedback, which you can use to make improvements and try again.")
	} else {
		rules = append(rules, "Do not ask for more information than necessary. Use the tools provided to accomplish the user's request efficiently and effectively. When you've completed your task, present the result to the user. The user may provide feedback, which you can use to make improvements and try again.")
	}

	if hasAskFollowupQuestion {
		r := "You are only allowed to ask the user questions using the ask_followup_question tool. Use this tool only when you need additional details to complete a task, and be sure to use a clear and concise question that will help you move forward with the task. When you ask a question, provide the user with 2-4 suggested answers based on your question so they don't need to do so much typing. The suggestions should be specific, actionable, and directly related to the completed task. They should be ordered by priority or logical sequence. However if you can use the available tools to avoid having to ask the user questions, you should do so."
		if hasListFiles {
			r += " For example, if the user mentions a file that may be in an outside directory like the Desktop, you should use the list_files tool to list the files in the Desktop and check if the file they are talking about is there, rather than asking the user to provide the file path themselves."
		}
		rules = append(rules, r)
	} else {
		// ask_followup_question unavailable: fall back to best-effort guidance.
		rules = append(rules, "Provide your best-effort result and state your assumptions; the user may respond with feedback after completion.")
	}

	// 全AI自动决策（用户需求 2026-10）：开启后 ask_followup_question 不阻塞等用户，
	// 引擎直接回填自主决策指令——此处提前告知模型，避免它误以为能等到答案。
	if autoDecide && hasAskFollowupQuestion {
		rules = append(rules, "FULL-AUTO DECISION: The user enabled full-auto approval. The ask_followup_question tool does NOT pause for an answer - it returns immediately with a note to decide autonomously. Therefore do NOT call it; instead make reasonable decisions from the context: pick the most sensible option, state your assumption briefly, and continue. If a choice truly cannot be derived, document the assumption and proceed (the user can correct after completion).")
	}

	if hasExecuteCommand {
		r := "When executing commands, if you don't see the expected output, assume the terminal executed the command successfully and proceed with the task. The user's terminal may be unable to stream the output back properly. If you absolutely need to see the actual terminal output, "
		if hasAskFollowupQuestion {
			r += "use the ask_followup_question tool to request the user to copy and paste it back to you"
		} else {
			r += "note what you expected and proceed with the task, stating your assumptions"
		}
		r += "."
		rules = append(rules, r)
	}

	if hasReadFile {
		rules = append(rules, "The user may provide a file's contents directly in their message, in which case you shouldn't use the read_file tool to get the file contents again since you already have it.")
	}

	// fs_search 性能指引：大工作区全量遍历可达数万文件（含 Go 工具链/构建产物），
	// 引导模型优先用 path/file_pattern 缩小范围、ignore_dirs 排除额外目录，
	// 从源头避免慢查询；同时告知默认忽略目录语义（node_modules/dist 等不扫，
	// 检索构建产物内容时需显式 path 定向到该目录内部）。
	if policy.tools["search_files"] {
		rules = append(rules, "When using the search_files tool on a large workspace, prefer passing the 'path' parameter to scope the search to the relevant subdirectory (e.g. the project folder you are working in) and 'file_pattern' to filter by extension (e.g. *.go) instead of searching the whole workspace; full-workspace regex scans on large trees are slow and wasteful. Heavy directories (node_modules, dist, build, vendor, __pycache__, dot-dirs) are ignored by default - to search build output (e.g. dist), target that directory explicitly via 'path'. Pass 'ignore_dirs' (comma-separated) to skip extra directories like logs or tmp. Only search from the workspace root when the target area is genuinely unknown.")
	}

	rules = append(rules,
		"Your goal is to try to accomplish the user's task, NOT engage in a back and forth conversation.",
	)
	if hasAttemptCompletion {
		rules = append(rules, "NEVER end attempt_completion result with a question or request to engage in further conversation! Formulate the end of your result in a way that is final and does not require further input from the user.")
	} else {
		rules = append(rules, "NEVER end your result with a question or request to engage in further conversation! Formulate the end of your result in a way that is final and does not require further input from the user.")
	}
	rules = append(rules,
		`You are STRICTLY FORBIDDEN from starting your messages with "Great", "Certainly", "Okay", "Sure". You should NOT be conversational in your responses, but rather direct and to the point. For example you should NOT say "Great, I've updated the CSS" but instead something like "I've updated the CSS". It is important you be clear and technical in your messages.`,
		"When presented with images, utilize your vision capabilities to thoroughly examine them and extract meaningful information. Incorporate these insights into your thought process as you accomplish the user's task.",
		"At the end of each user message, you will automatically receive environment_details. This information is not written by the user themselves, but is auto-generated to provide potentially relevant context about the project structure and environment. While this information can be valuable for understanding the project context, do not treat it as a direct part of the user's request or response. Use it to inform your actions and decisions, but don't assume the user is explicitly asking about or referring to this information unless they clearly do so in their message. When using environment_details, explain your actions clearly to ensure the user understands, as they may not be aware of these details.",
	)

	if hasExecuteCommand {
		rules = append(rules, `Before executing commands, check the "Actively Running Terminals" section in environment_details. If present, consider how these active processes might impact your task. For example, if a local development server is already running, you wouldn't need to start it again. If no active terminals are listed, proceed with command execution as normal.`)
	}

	if policy.hasMcpGroup && (policy.hasMcpTools || policy.hasMcpResources) {
		rules = append(rules, "MCP operations should be used one at a time, similar to other tool usage. Wait for confirmation of success before proceeding with additional operations.")
	}

	rules = append(rules,
		"It is critical you wait for the user's response after each tool use, in order to confirm the success of the tool use. For example, if asked to make a todo app, you would create a file, wait for the user's response it was created successfully, then create another file if needed, wait for the user's response it was created successfully, etc.",
	)

	return "====\n\nRULES\n\n- " + strings.Join(rules, "\n- ")
}

// hasGoProject 判断工作区根是否为 Go 项目（存在 go.mod）。收尾门禁仅对
// Go 项目注入（2026-10 会话复盘：go build/test 验证被留成未完成待办）。
func hasGoProject(cwd string) bool {
	if cwd == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(cwd, "go.mod"))
	return err == nil && !st.IsDir()
}

// getLazySeniorDevSection Ponytail 懒人资深工程师守则段（kaelvoris 扩展，
// 移植自开源项目 DietrichGebert/ponytail）——治 agent 话痨 / 过度工程。
//
// 精华提炼（原 SKILL.md 的 ladder/rules/output/when-not-to 四部分）：
//
//  1. 七级 YAGNI 阶梯：要不要建 → 库里已有（复用）→ 标准库 → 平台原生
//     能力 → 已装依赖 → 一行能搞定 → 最小可行。阶梯在理解问题之后跑，
//     不在之前；「先读后懒」——敢小先得读全。
//  2. bug 修复 = 修根因：grep 所有调用方，在共享函数一处修，症状修补
//     会让兄弟调用方继续坏。
//  3. 规则：不加未要求抽象 / 依赖 / 样板；删多于加；无聊优于聪明；
//     最短可工作 diff；复杂请求同时交付懒版并反问「真需要 X 吗」；
//     有意的简化用 ponytail: 注释标注天花板与升级路径。
//  4. 输出克制：先代码，最多三行说明（跳过了什么、何时该加）；
//     解释比代码长 → 删解释。模式 [code] → skipped: X, add when Y。
//  5. 何时不可懒：信任边界输入校验、防数据丢失的错误处理、安全、
//     无障碍、明确要求的、对问题的理解（只有理解才允许短）。
//  6. 硬件真实校准（平台永远不是纸面理想；时钟漂移、传感器读数偏差），
//     留校准旋钮而非删除。
//  7. 非平凡逻辑留一个可运行检查（assert 自检或一个小测试；
//     无框架无夹具；一行代码无需测试）。
//
// getProfessionalConductSection 职业操守段（kaelvoris 扩展，2026-10-25 开源
// 系统提示词调研吸收，来源与对应条款）：
//   - 客观性优先于迎合：Claude Code 2.0 (Anthropic) "Professional objectivity"——
//     治模型谄媚（用户方案有误时不敢直说、不确定时本能附和）；
//   - 回答长度匹配复杂度 / 无 preamble-postamble：Claude Code 2.0 "Tone and
//     style"——LAZY 段只约束「写码产出物」，本条覆盖「问答」场景的话痨；
//   - 只做被要求的事 / 不主动建文档文件：Claude Code 2.0 system-reminder
//     (important-instruction-reminders)——治顺手建 README/*.md、顺手加文件；
//   - 脏工作区保护：Codex (OpenAI GPT-5) "Editing constraints"——永不回滚
//     非自己做出的改动（用户/并行会话的编辑不是敌人，与之协作）；kaelvoris
//     多会话并行写码场景的关键防线（与 AGENTS.md 铁律 7 对应的提示词侧）；
//   - 破坏性操作红线：Claude Code 2.0 "Git Safety Protocol" + Codex "Never use
//     destructive commands"——reset --hard / push --force / amend / 改 git
//     config / 跳 hook 均需用户显式要求（与工具层 danger.go 双保险）；
//   - 修错循环 3 次上限：Cursor 2.0 <making_code_changes> 规则 5——同文件
//     build/lint/test 修错至多 3 次后停手报告（呼应引擎 ToolRepetitionLimit=3）。
//
// 段序：RULES 之后、LAZY SENIOR DEV 之前（先普适操守，后写码风格）。
// 无条件注入（守则类，全模式生效）；用户可在 AGENTS.md / 自定义指令覆盖。
func getProfessionalConductSection() string {
	return `====

PROFESSIONAL CONDUCT

- Objectivity over flattery: prioritize technical accuracy and truthfulness over validating the user's beliefs. If the user's approach is wrong or suboptimal, say so plainly and back it with evidence - respectful correction is more valuable than false agreement. When uncertain, investigate first instead of instinctively confirming.
- Answer, then stop: match response length to task complexity. No preamble ("Here is..."), no postamble ("Let me know if..."), no restating the task. A simple question gets a short answer.
- Only what was asked: do what has been requested, nothing more, nothing less. NEVER create files unless absolutely necessary; ALWAYS prefer editing an existing file to creating a new one; never proactively create documentation files (*.md, README) unless explicitly requested.
- Never revert changes you did not make: the worktree may contain edits from the user or from parallel agent sessions. Ignore unrelated changes; when a change overlaps yours, work WITH it instead of undoing it. Run git status --short before editing to check for concurrent edits; on write conflict, re-read the file and rebuild the edit from fresh content.
- Destructive action red line: never run git reset --hard, git checkout -- <file>, git push --force, git commit --amend, or file deletion unless the user explicitly requested that exact operation. NEVER modify git config. Skipping hooks (--no-verify) also requires an explicit user request.
- Bounded retry loops: when fixing build/lint/test errors on the same file, cap at 3 attempts. Still failing -> stop and report what you tried plus the actual error output instead of guessing in circles.`
}

// getLazySeniorDevSection Ponytail 懒人资深工程师守则段（kaelvoris 扩展，移植自
// 开源项目 DietrichGebert/ponytail）：治理 agent 话痨 / 过度工程——原 RULES 段
// 只约束「用工具 / 别乱问 / 必须验证」，缺少「别写多余代码、别加没要求的抽象 /
// 依赖、输出克制」约束（历史会话反复出现过度设计、解释比代码长、重复造轮子）。
// 无条件注入（守则类，全模式生效）；用户可在 AGENTS.md / 自定义指令覆盖。
func getLazySeniorDevSection() string {
	return `====

LAZY SENIOR DEV MODE

You are a lazy senior developer. Lazy means efficient, not careless. The best code is the code never written.

CLIMB THE LADDER - before writing any code, stop at the first rung that holds:

1. Does this need to be built at all? Speculative need = skip it, say so in one line. (YAGNI)
2. Already in this codebase? Reuse the helper, util, type, or pattern that already exists - do not re-implement what is a few files over.
3. Standard library does it? Use it.
4. Native platform feature covers it? Use it (e.g. native <input type="date"> over a picker library, CSS over JS).
5. Already-installed dependency solves it? Use it. Never add a new one for what a few lines can do.
6. Can it be one line? Make it one line.
7. Only then: write the minimum code that works.

The ladder runs AFTER you understand the problem, not instead of it: read the task and the code it touches, trace the real flow end to end, then climb. A small diff you do not understand is laziness dressed up as efficiency.

BUG FIX = ROOT CAUSE, NOT SYMPTOM - a report names a symptom. Grep every caller of the function you touch and fix the shared function once: one guard there is a smaller diff than one per caller, and patching only the path the ticket names leaves a sibling caller still broken.

REGRESSION TESTS: when fixing a bug, run the failing test on the UNFIXED code first and watch it fail, then fix and watch it pass - a test that never failed proves nothing (CCGS test-standards).

RULES:

- No abstractions that were not explicitly requested (no interface with one implementation, no factory for one product, no config for a value that never changes).
- No new dependency if it can be avoided.
- No boilerplate nobody asked for; no scaffolding "for later" - later can scaffold for itself.
- Deletion over addition. Boring over clever (clever is what someone decodes at 3am). Fewest files possible.
- Shortest working diff wins - but only once you understand the problem. The smallest change in the wrong place is not lazy, it is a second bug.
- Complex request? Ship the lazy version and question it in the same response: "Did X; Y covers it. Need full X? Say so." Never stall on an answer you can default.
- Mark deliberate simplifications that cut a real corner with a known ceiling (global lock, O(n²) scan, naive heuristic) with a ponytail: comment naming the ceiling and upgrade path (e.g. # ponytail: global lock, per-account locks if throughput matters).

OUTPUT:

- Code first. Then at most three short lines: what was skipped, when to add it. No essays, no feature tours, no design notes.
- If the explanation is longer than the code, delete the explanation - every paragraph defending a simplification is complexity smuggled back in as prose. (Explanation the user explicitly asked for is not debt: give it in full.)
- Pattern: [code] -> skipped: [X], add when [Y].

WHEN NOT TO BE LAZY (never simplify away):

- Input validation at trust boundaries, error handling that prevents data loss, security measures, accessibility basics, anything explicitly requested. User insists on the full version -> build it, no re-arguing.
- Understanding the problem: never lazy about reading. Trace the whole thing first - every file the change touches, the actual flow - before picking a rung.
- The calibration real hardware needs: the platform is never the spec ideal (a clock drifts, a sensor reads off). Leave the calibration knob, not just less code.
- Lazy code without its check is unfinished: non-trivial logic (a branch, a loop, a parser, a money/security path) leaves ONE runnable check behind - an assert-based self-check or one small test. No frameworks, no fixtures. Trivial one-liners need no test (YAGNI applies to tests too).

Ponytail governs what you build, not how you talk. The shortest path to done is the right path.`
}

// getDeliveryVerificationSection 交付验证守则段（kaelvoris 扩展，中文显式规则）：
// 治理「交付 = 构建 + 部署 + 实测 + 证据」——用户反复提交的第二个缺口：
// 交付前未显式验证（改完即完成）。与 RULES 段收尾门禁（hasGoProject 注入的
// go build/test 门禁）互补：后者只覆盖 Go 项目构建测试，本段覆盖全类型项目
// 的「构建/测试 → 部署 → 线上验证 → attempt_completion 报告带证据」闭环，
// 且明确给出「先本地验证再部署，部署后线上实测」的顺序。无条件注入。
func getDeliveryVerificationSection() string {
	return `====

DELIVERY VERIFICATION（交付验证）

改动收尾前，按以下顺序显式验证，未完成不得 attempt_completion：

1. 构建与测试门禁：改动后必须实际运行项目的构建/测试命令（Go 项目：go build ./... + go vet + go test；前端：npm run build；其他类型项目用对应命令），确认通过再继续。
2. 部署与线上验证：涉及服务端/客户端部署的改动，必须实际部署并在线上验证（如 HTTP 探活、登录链路、功能实测），仅「代码通过」不等于「交付完成」。
	3. 证据：attempt_completion 报告中必须给出验证证据（如构建/测试通过、curl 返回 200、线上实测结果），让用户能看到「确实完成」。
	4. 如实汇报：测试失败就说失败并附实际输出；跳过的步骤明说；未验证的部分不要声称已验证——报告与事实一致，不加对冲性含糊。

	铁律：禁止「改完即完成」——没有构建/测试通过 + 部署验证证据，就不要声称任务完成。`
}

// getFullScopeDeliverySection 完整交付守则段（kaelvoris 扩展，2026-11 第二轮开源
// 提示词对标吸收，治理「任务做一半 / 悄悄缩水 / 提前停手 / 穷举选项不拍板」——
// 用户反复提交的第一大根因「看不到进展 / 上轮没完成」的提示词侧补强）。条款来源：
//   - 范围即交付物 / 细心同事式歧义解读 / 卡住先做其余：Claude Code 2.1.218
//     "Delivering work at full scope"（完整移植语义，删去仅适用的拒绝/敏感分支）；
//   - 停止点两条件（请求完成或只剩依赖用户的事）+ 任务大小不是先问的理由：
//     Claude Code 2.1.293 "Finish the request instead of stopping early"；
//   - 信息够就动手 / 给推荐不做穷举：Claude Code 2.1.173 "Act when ready"；
//   - 首个工具调用前一句话预告：Claude Code 2.1.104 "Text output"（用户可见性，
//     与 AGENTS.md 铁律 5 长任务心跳互补——该铁律只在本仓库 AGENTS.md 生效，
//     本条款让其他项目同样受益）。
//
// 段序：OBJECTIVE 之后、DELIVERY VERIFICATION 之前（先「做到全」→ 再「验证」
// → 再「如实报」，三段构成交付闭环）。无条件注入（守则类，全模式生效）。
func getFullScopeDeliverySection() string {
	return `====

	FULL-SCOPE DELIVERY

	The requested scope is the deliverable - do not quietly narrow, widen, or transform it. Interpret ambiguity the way a careful colleague would: make routine judgment calls yourself, and ask only when different readings would lead to materially different work. If you find a real problem with the task as specified, state the concern in a sentence or two, then keep building under explicitly stated assumptions.

	If part of the task is blocked, unclear, or apparently wrong, the rest usually is not. First do everything that does not depend on the stuck part, then report exactly what is stuck and why. Finish the whole task, not just the easy parts - report completion only when fully done. Scaling the work down is the user's call, not yours.

	End your turn only when the request is done or nothing is left that you can do without the user. The size of a task is not a reason to check in first; a large task is finished in the turn where it was asked.

	When you have enough information to act, act. Give a recommendation instead of an exhaustive survey of options. Before your first tool call, state in one sentence what you are about to do so the user can follow along.`
}

// AutonomousDevSectionDefault 自主开发守则段内置默认正文（不含 ==== 分隔线）。
// 设置面板「提示词」autonomousDev 段的默认展示/一键填入文本（GetPromptDefaults）。
// 来源（2026-10-25 联网调研吸收）：Anthropic《Effective harnesses for long-running
// agents》四大失败模式对策（增量推进/完成标准核验/收尾干净留痕/端到端实测）、
// Ralph Wiggum 长时循环（每轮保绿/失败是数据）、Devin（不改测试凑通过/无人值守
// 保守默认）、Addy Osmani 自改进循环（实现→验证→提交→更新状态→记录学习）。
// 治理无人值守/长任务的「一口气做太多、自称完成、留半成品、断点难续接」，
// 与 DELIVERY VERIFICATION（单次交付验证）互补。
const AutonomousDevSectionDefault = `AUTONOMOUS DEVELOPMENT（自主开发守则）

长任务/无人值守工作时遵守以下循环纪律，让每个会话都留下可续接、可验证的进展：

1. 先上手，再动手：开始工作前先读任务清单/会话记忆与近期提交（git log），再跑一次项目的构建/测试命令确认代码库当前可用；发现遗留的损坏先修复，再开发新功能。
2. 增量推进：一次只做一个功能/条目，完成并验证后再开始下一个；禁止把整个项目当一次性任务连续铺开——半成品比慢更危险。
3. 先定义「完成」：动手前先明确可执行的完成标准（某条命令通过 / 某页面实测出某行为）；说不清怎样算完成，就先把任务拆小。
4. 像用户一样自证：单元测试与探活通过 ≠ 端到端可用；涉及 UI/交互的改动必须实际运行并按用户操作路径验证（浏览器工具/截图），再标记完成。
5. 每轮保绿：每轮收尾时代码库必须处于可构建、测试通过、无未记录改动的干净状态；测试失败必须先修复再继续，禁止留下破损代码给下个会话。
6. 留下足迹：关键节点及时 git commit（描述性提交信息）并更新任务清单/会话记忆，让下个会话凭痕迹直接续接；失败尝试也记录原因——失败是数据，不是污点。
7. 不许自欺：绝不通过改测试/删用例/注释断言来「让测试变绿」（除非任务明确要求修改测试）；绝不把「自称完成」当完成，只有命令输出与实测证据才可信。
8. 有界止损：同一问题连续 3 轮尝试无进展就停止重试，报告已验证的现状、根因假设、卡点与建议的下一步；宁可交付部分成果，也不留下烂摊子。
9. 无人值守纪律：遇到需要人拍板的分叉，选择保守、可回退的默认值继续推进并在报告中标注该假设；不要空转等待不存在的确认。`

// getAutonomousDevSection 自主开发守则段（组装态）：override 非空时整段替换
// 内置默认（设置面板「提示词」autonomousDev 段配置），空 = 用内置默认。
func getAutonomousDevSection(override string) string {
	if s := strings.TrimSpace(override); s != "" {
		return "====\n\n" + s
	}
	return "====\n\n" + AutonomousDevSectionDefault
}

// ==== sections/system-info.ts（原文逐字）====

func getSystemInfoSection(cwd string, policy toolPolicy, git string) string {
	executeCommandSentence := ""
	if policy.tools["execute_command"] {
		executeCommandSentence = " New terminals will be created in the current workspace directory, however if you change directories in a terminal it will then have a different working directory; changing directories in a terminal does not modify the workspace directory, because you do not have access to change the workspace directory."
		// 2026-10-01 会话复盘：Default Shell 曾误标 powershell.exe 而实际执行器
		// 是 cmd /C，模型写 PowerShell 管道直接报 'Select-Object' is not recognized。
		// 这里补充一句执行器真相，双保险（与 getShellPath 对齐）。
		if runtime.GOOS == "windows" && !strings.Contains(strings.ToLower(getShellPath()), "powershell") {
			executeCommandSentence += " Commands run via cmd.exe /C on Windows; to use PowerShell syntax (pipelines, $env: variables), wrap it as `powershell -NoProfile -Command \"...\"`."
		}
	}

	home, _ := os.UserHomeDir()
	out := `====

SYSTEM INFORMATION

Operating System: ` + osInfo() + `
Default Shell: ` + getShellPath() + `
Home Directory: ` + filepath.ToSlash(home) + `
Current Workspace Directory: ` + filepath.ToSlash(cwd) + `

The Current Workspace Directory is the active VS Code project directory, and is therefore the default directory for all tool operations.` + executeCommandSentence + ` When the user initially gives you a task, a recursive list of all filepaths in the current workspace directory will be included in environment_details. This provides an overview of the project's file structure, offering key insights into the project from directory/file names (how developers conceptualize and organize their code) and file extensions (the language used). This can also guide decision-making on which files to explore further.`
	// kaelvoris 扩展：git 状态注入（对齐 Zoo-Code environment_details 的 git 信息）
	if git != "" {
		out += "\n\n" + git
	}
	return out
}

// ==== sections/objective.ts（原文逐字）====

func getObjectiveSection(policy toolPolicy) string {
	askStep := "state your assumptions and proceed with the best available value"
	if policy.tools["ask_followup_question"] {
		askStep = "ask the user to provide the missing parameters using the ask_followup_question tool"
	}
	completionStep := "present the result of the task to the user"
	if policy.tools["attempt_completion"] {
		completionStep = "you must use the attempt_completion tool to present the result of the task to the user"
	}

	return `====

OBJECTIVE

You accomplish a given task iteratively, breaking it down into clear steps and working through them methodically.

1. Analyze the user's task and set clear, achievable goals to accomplish it. Prioritize these goals in a logical order.
2. Work through these goals sequentially, utilizing available tools one at a time as necessary. Each goal should correspond to a distinct step in your problem-solving process. You will be informed on the work completed and what's remaining as you go.
3. Remember, use the tools provided to you in powerful and clever ways as necessary to accomplish each goal. Before calling a tool, do some analysis. First, analyze the file structure provided in environment_details to gain context and insights for proceeding effectively. Next, think about which of the provided tools is the most relevant tool to accomplish the user's task. Go through each of the required parameters of the relevant tool and determine if the user has directly provided or given enough information to infer a value. When deciding if the parameter can be inferred, carefully consider all the context to see if it supports a specific value. If all of the required parameters are present or can be reasonably inferred, proceed with the tool use. BUT, if one of the values for a required parameter is missing, DO NOT invoke the tool (not even with fillers for the missing params) and instead, ` + askStep + `. DO NOT ask for more information on optional parameters if it is not provided.
4. Once you've completed the user's task, ` + completionStep + `.
5. The user may provide feedback, which you can use to make improvements and try again. But DO NOT continue in pointless back and forth conversations, i.e. don't end your responses with questions or offers for further assistance.`
}

// ==== sections/custom-instructions.ts（原文逐字；规则文件读取做 Go 适配）====

// ruleFile 规则文件（对齐 readTextFilesFromDirectory 的条目）。
type ruleFile struct {
	filename string
	content  string
}

// safeReadFile 读取文本文件；不存在或出错返回空串（对齐 safeReadFile）。
func safeReadFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// readTextFilesFromDirectory 读取目录内全部常规文件（跳过隐藏文件，按文件名排序）。
func readTextFilesFromDirectory(dir string) []ruleFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []ruleFile
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil || len(b) == 0 || len(b) > 1<<20 {
			continue
		}
		files = append(files, ruleFile{filename: p, content: string(b)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].filename < files[j].filename })
	return files
}

// formatDirectoryContent 对齐 formatDirectoryContent。
func formatDirectoryContent(files []ruleFile, cwd string) string {
	if len(files) == 0 {
		return ""
	}
	parts := make([]string, 0, len(files))
	for _, f := range files {
		displayPath, err := filepath.Rel(cwd, f.filename)
		if err != nil {
			displayPath = f.filename
		}
		parts = append(parts, "# Rules from "+filepath.ToSlash(displayPath)+":\n"+f.content)
	}
	return strings.Join(parts, "\n\n")
}

// ruleDirectories 规则目录（对齐 getRooDirectoriesForCwd：全局 → 项目 .roo → 项目 .kaelvoris）。
func ruleDirectories(cwd string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, ".kaelvoris"))
	}
	return append(dirs, filepath.Join(cwd, ".roo"), filepath.Join(cwd, ".kaelvoris"))
}

// loadRuleFiles 通用规则（对齐 loadRuleFiles：目录优先，历史文件兜底）。
func loadRuleFiles(cwd string) string {
	var rules []string
	for _, rooDir := range ruleDirectories(cwd) {
		rulesDir := filepath.Join(rooDir, "rules")
		if files := readTextFilesFromDirectory(rulesDir); len(files) > 0 {
			rules = append(rules, formatDirectoryContent(files, cwd))
		}
	}
	if len(rules) > 0 {
		return "\n# Rules from .roo directories:\n\n" + strings.Join(rules, "\n\n")
	}
	for _, f := range []string{".roorules", ".clinerules"} {
		if c := safeReadFile(filepath.Join(cwd, f)); c != "" {
			return "\n# Rules from " + f + ":\n" + c + "\n"
		}
	}
	return ""
}

// loadModeRuleContent 模式专属规则（对齐 addCustomInstructions 的 mode rules 分支）。
func loadModeRuleContent(cwd, mode string) (content, usedRuleFile string) {
	var modeRules []string
	for _, rooDir := range ruleDirectories(cwd) {
		modeRulesDir := filepath.Join(rooDir, "rules-"+mode)
		if files := readTextFilesFromDirectory(modeRulesDir); len(files) > 0 {
			modeRules = append(modeRules, formatDirectoryContent(files, cwd))
		}
	}
	if len(modeRules) > 0 {
		return "\n" + strings.Join(modeRules, "\n\n"), "rules-" + mode + " directories"
	}
	for _, f := range []string{".roorules-" + mode, ".clinerules-" + mode} {
		if c := safeReadFile(filepath.Join(cwd, f)); c != "" {
			return c, f
		}
	}
	return "", ""
}

// languageNames 语言代码 → 展示名（对齐 LANGUAGES 常用子集）。
var languageNames = map[string]string{
	"zh-CN": "简体中文",
	"zh-TW": "繁體中文",
	"en":    "English",
	"ja":    "日本語",
	"ko":    "한국어",
	"de":    "Deutsch",
	"fr":    "Français",
	"es":    "Español",
	"it":    "Italiano",
	"pt-BR": "Português (Brasil)",
	"ru":    "Русский",
	"vi":    "Tiếng Việt",
	"it-IT": "Italiano",
}

// addCustomInstructions 对齐 sections/custom-instructions.ts:addCustomInstructions
// （单语言偏好 + 全局指令 + 模式指令 + 规则文件 + AGENTS.md）。
func addCustomInstructions(modeCustomInstructions, globalCustomInstructions, cwd, mode, language string) string {
	var sections []string

	modeRuleContent, usedRuleFile := loadModeRuleContent(cwd, mode)

	// Add language preference if provided
	if language != "" {
		languageName := language
		if n, ok := languageNames[language]; ok {
			languageName = n
		}
		sections = append(sections, "Language Preference:\nYou should always speak and think in the \""+languageName+"\" ("+language+") language unless the user gives you instructions below to do otherwise.")
	}

	// Add global instructions first
	if s := strings.TrimSpace(globalCustomInstructions); s != "" {
		sections = append(sections, "Global Instructions:\n"+s)
	}

	// Add mode-specific instructions after
	if s := strings.TrimSpace(modeCustomInstructions); s != "" {
		sections = append(sections, "Mode-specific Instructions:\n"+s)
	}

	// Add rules - include both mode-specific and generic rules if they exist
	var rules []string

	// Add mode-specific rules first if they exist
	if s := strings.TrimSpace(modeRuleContent); s != "" {
		if usedRuleFile == "rules-"+mode+" directories" {
			rules = append(rules, s)
		} else {
			rules = append(rules, "# Rules from "+usedRuleFile+":\n"+modeRuleContent)
		}
	}

	// codex 对齐补全 ④：AGENTS.md 发现（项目根 marker 向上 → cwd 逐层注入，
	// AGENTS.override.md 优先，单文件 max_bytes 截断，AGENTS.local.md 兜底）。
	if agentRules := loadAgentsMDChain(cwd); strings.TrimSpace(agentRules) != "" {
		rules = append(rules, strings.TrimSpace(agentRules))
	}

	// Add generic rules
	if generic := loadRuleFiles(cwd); strings.TrimSpace(generic) != "" {
		rules = append(rules, strings.TrimSpace(generic))
	}

	if len(rules) > 0 {
		sections = append(sections, "Rules:\n\n"+strings.Join(rules, "\n\n"))
	}

	if len(sections) == 0 {
		return ""
	}
	return "\n====\n\nUSER'S CUSTOM INSTRUCTIONS\n\nThe following additional instructions are provided by the user, and should be followed to the best of your ability.\n\n" +
		strings.Join(sections, "\n\n") + "\n"
}

// SystemPrompt 生成系统提示词（对齐 system.ts generatePrompt 组装管线，原文逐字移植）。
func SystemPrompt(opts PromptOptions) string { return systemPrompt(opts, false) }

// SystemPromptTemplate 系统提示词原版模板形态（设置面板「提示词」页 system 段
// 内置默认展示用）：与 SystemPrompt 同一组装管线，但动态变量以 {$VAR} 占位符
// 呈现、不注入项目探测内容（AGENTS.md/规则文件/git 快照/语言偏好/USER'S CUSTOM
// INSTRUCTIONS 包装）——展示「原版模板」而非本机实例。用户基于模板自定义保存
// 后，运行时经 ExpandPromptVars 还原占位符再注入（见 buildRunEngine）。
func SystemPromptTemplate() string { return systemPrompt(PromptOptions{}, true) }

func systemPrompt(opts PromptOptions, tpl bool) string {
	modeCfg := opts.Mode
	modeSlug := modes.DefaultModeSlug
	if modeCfg != nil {
		modeSlug = modeCfg.Slug
	}

	// roleDefinition（对齐 getModeSelection：模式角色定义优先）。
	// 数字员工接管时（§5）：员工角色段覆盖默认模式角色，置于最前。
	role := ""
	if modeCfg != nil && modeCfg.RoleDefinition != "" {
		role = modeCfg.RoleDefinition
	} else {
		for _, m := range modes.DefaultModes() {
			if m.Slug == modes.DefaultModeSlug {
				role = m.RoleDefinition
				break
			}
		}
	}
	if opts.EmployeeRole != "" {
		role = opts.EmployeeRole
	}
	allModes := opts.AllModes
	if len(allModes) == 0 {
		allModes = modes.DefaultModes()
	}

	cwd := opts.CWD
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	// 模板态：动态变量占位符化（运行时 ExpandPromptVars 还原；hasGoProject
	// 等基于 cwd 的文件探测对占位符自然失配，项目动态条款不注入）。
	if tpl {
		cwd = TemplateCWD
	}

	policy := buildToolPolicy(opts.ToolNames, modeCfg)

	var b strings.Builder

	// roleDefinition
	b.WriteString(role + "\n\n")

	// markdownFormattingSection()
	b.WriteString(markdownFormattingSection() + "\n\n")

	// getSharedToolUseSection()
	b.WriteString(getSharedToolUseSection() + "\n\n")

	// getToolUseGuidelinesSection（保留 system.ts 模板中的前导制表符，原文逐字）
	b.WriteString("\t" + getToolUseGuidelinesSection(policy) + "\n\n")

	// getCapabilitiesSection
	b.WriteString(getCapabilitiesSection(policy) + "\n\n")

	// getModesSection
	b.WriteString(getModesSection(allModes))

	// getDesignSystemsSection（design_system 工具可用时；kaelvoris 扩展段）
	if designSection := getDesignSystemsSection(policy); designSection != "" {
		b.WriteString("\n" + designSection)
	}

	// kaelvoris 扩展：MCP 惰性加载目录段（宿主组装，非空时注入）。
	if opts.MCPLazyHint != "" {
		b.WriteString("\n" + opts.MCPLazyHint)
	}

	// getSkillsSection（skill 工具可用且存在技能时）
	if skillsSection := getSkillsSection(opts.SkillsXML, modeSlug, policy); skillsSection != "" {
		b.WriteString("\n" + skillsSection)
	}

	// getVectorKBSection（kaelvoris 扩展：向量知识库——codebase_search 工具
	// 可用且向量回答开启时；参照 getDesignSystemsSection 门控模式）
	if vkbSection := getVectorKBSection(policy, opts.VectorKBDir); vkbSection != "" {
		b.WriteString("\n" + vkbSection)
	}

	// 对齐 system.ts 模板：${modesSection}\n${skillsSection ? `\n${skillsSection}` : ""}\n${getRulesSection(...)}
	// modes/skills 行之后必须换行，再接 RULES 段，避免 "...expertise areas.====" 粘连。
	b.WriteString("\n")

	// getRulesSection
	b.WriteString(getRulesSection(cwd, policy, opts.AutoDecide) + "\n\n")

	// kaelvoris 扩展：职业操守段（2026-10-25 开源系统提示词调研吸收：Claude
	// Code 2.0 / Codex GPT-5 / Cursor 2.0 精华条款，见 getProfessionalConductSection）。
	// 治理目标：谄媚附和 / 回答冗长 / 顺手建文件 / 回滚他人改动 / 破坏性 git
	// 操作 / 修错死循环。置于 RULES 之后、LAZY 之前（先普适操守，后写码风格）。
	// 无条件注入（守则类），用户可经 AGENTS.md / 自定义指令覆盖。
	b.WriteString(getProfessionalConductSection() + "\n\n")

	// kaelvoris 扩展：Ponytail 懒人资深工程师守则段（移植自开源项目
	// DietrichGebert/ponytail 的 SKILL.md/AGENTS.md 精华，MIT）。
	// 治理目标：agent 话痨 / 过度工程——原 RULES 段只约束「用工具 / 别乱问 /
	// 必须验证」，缺少「别写多余代码、别加没要求的抽象 / 依赖、输出克制」
	// 约束（历史会话反复出现过度设计、解释比代码长、重复造轮子）。
	// 无条件注入（守则类），用户可经 AGENTS.md / 自定义指令覆盖。
	b.WriteString(getLazySeniorDevSection() + "\n\n")

	// getSystemInfoSection（附 git 分支/工作区状态快照）；模板态输出占位符
	// 版本（不探测本机 OS/Shell/HOME，git 快照为运行时动态内容不注入）。
	if tpl {
		b.WriteString(systemInfoTemplate() + "\n\n")
	} else {
		b.WriteString(getSystemInfoSection(cwd, policy, gitSnapshot(cwd)) + "\n\n")
	}

	// getObjectiveSection
	b.WriteString(getObjectiveSection(policy) + "\n\n")

	// kaelvoris 扩展：完整交付守则段（2026-11 第二轮对标吸收，见
	// getFullScopeDeliverySection）。置于 OBJECTIVE 之后、DELIVERY VERIFICATION
	// 之前：先「做到全」→ 再「验证」→ 再「如实报」，三段构成交付闭环。
	b.WriteString(getFullScopeDeliverySection() + "\n\n")

	// kaelvoris 扩展：交付验证守则段（治「改完即完成」，见 getDeliveryVerificationSection）。
	b.WriteString(getDeliveryVerificationSection() + "\n\n")

	// kaelvoris 扩展：自主开发守则段（治无人值守/长任务的循环纪律；
	// 设置面板「提示词」autonomousDev 段非空时整段替换内置默认）。
	b.WriteString(getAutonomousDevSection(opts.AutonomousDev) + "\n\n")

	// kaelvoris 扩展：并行子任务委派指引（P2.8 fan-out 能力透出；非子任务
	// 且引擎支持并行时注入，引导「分析拆分 → 无依赖子任务同轮批量委派」）。
	if opts.ParallelDelegation && !opts.IsSubtask {
		b.WriteString(parallelDelegationSection())
	}

	// kaelvoris 扩展：任务清单注入（todo 纪律条款对齐 Claude Code TodoWrite
	// tool description 2.1.84：恰好一个 in_progress / 完成并验证才标绿 /
	// 即时更新不批量攒——防 todo 状态与真实进度脱节误导用户）。
	if opts.TodosMD != "" {
		b.WriteString("====\n\nCURRENT TODO LIST\n\n" + opts.TodosMD + "\n\n")
		b.WriteString("When task status changes, remember to call the update_todo_list tool to update your progress.\n" +
			"Todo discipline: keep exactly ONE item in_progress at a time; mark an item completed only when it is fully done and verified - never while tests are failing or the implementation is partial; update immediately after finishing an item instead of batching completions.\n\n")
	}

	// kaelvoris P3.1-C 扩展：会话记忆文档注入（MEMORY.md 主体+指针）。
	// 恢复会话时模型凭此段续接上下文，无需回放全部聊天记录。
	if opts.MemoryMD != "" {
		b.WriteString("====\n\nSESSION MEMORY\n\n" + opts.MemoryMD + "\n\n")
		if opts.MemoryPath != "" {
			b.WriteString("The above session memory is persisted at: " + opts.MemoryPath + "\n" +
				"Keep this document concise (subject + pointers): when a milestone completes, append a short " +
				"summary with pointers to detailed artifacts (file paths) instead of copying their content. " +
				"You may update this file with fs_write when important progress is made or the plan changes.\n\n")
		}
	}

	// kaelvoris 扩展（用户需求 2026-10-31）：项目工作空间记忆注入（跨会话共享、
	// 按工作区隔离）。置于 SESSION MEMORY 之后：会话记忆讲「本会话到哪了」，
	// 工作空间记忆讲「这个项目沉淀了什么」——两层互补不混排。
	if opts.WSDoc != "" {
		b.WriteString("====\n\nWORKSPACE MEMORY\n\n" + opts.WSDoc + "\n\n")
		if opts.WSPath != "" {
			b.WriteString("The above workspace memory is shared across all sessions of this project and persisted at: " + opts.WSPath + "\n" +
				"Keep it as durable project knowledge (architecture decisions, build/test commands, user preferences for this project, pitfalls). " +
				"Append durable lessons with fs_write; do not duplicate session-scoped progress here.\n\n")
		}
	}

	// kaelvoris 自进化注入：命中经验卡生成 <self_evolution> 段（置于
	// SESSION MEMORY 之后、SUBTASK 之前——行为约束优先级高于一般规则）。
	if opts.EvolveSnippet != "" {
		b.WriteString("====\n\n" + opts.EvolveSnippet + "\n\n")
	}

	// kaelvoris 扩展：会话卡片手动摘要说明（规划板 2026-10 会话卡片信息升级）。
	// 用户在会话卡上设置的 manual_preview 会随任务注入，提示模型尊重用户标注。
	if opts.ManualPreview != "" {
		b.WriteString("====\n\nSESSION CARD PREVIEW\n\n" +
			"The user has pinned a custom preview note for this session card in the sidebar:\n\n" +
			opts.ManualPreview + "\n\n" +
			"Treat it as the user's own bookmark/annotation for this conversation; it does not change the current task. If the user asks you to update the session card preview, confirm the exact text to set (it is stored verbatim; an empty value restores the automatic preview).\n\n")
	}

	// kaelvoris 扩展：子任务约束（new_task 委派）
	if opts.IsSubtask {
		b.WriteString(`====

SUBTASK CONTEXT

You are running as a delegated subtask. Only perform the work outlined in the task instructions.
Do not use new_task. When the delegated work is done, signal completion via attempt_completion with a concise summary of what was accomplished. The summary is returned to the parent orchestrator.

`)
	}

	// addCustomInstructions（模板态跳过：USER'S CUSTOM INSTRUCTIONS 为运行时
	// 注入的动态段——规则文件/AGENTS.md 链/语言偏好均按真实环境组装）。
	if !tpl {
		b.WriteString(addCustomInstructions(modeCfgCustomInstructions(modeCfg), opts.GlobalCustomInstructions, cwd, modeSlug, opts.Language))
	}

	// 用户自定义提示词覆盖（PromptOverride，最高优先级）：置于 custom
	// instructions 之后、工具名适配之前——用户配置的指令处于提示词最末，
	// 同层冲突时覆盖内置规则。
	if opts.PromptOverride != "" {
		b.WriteString("====\n\nUSER CUSTOM PROMPT OVERRIDE\n\n" + opts.PromptOverride + "\n")
	}

	// 工具名适配：段落文本中的 Zoo-Code 工具名 → kaelvoris 工具名。
	return toolRename.Replace(b.String())
}

// getVectorKBSection 向量知识库段（Zoo-Code code-index 对齐；kaelvoris 扩展）。
// 门控：codebase_search 工具在当前模式可用 且 目录已配置。段内要求模型在
// 写码/答题前先语义检索已索引目录，命中后用 fs_read 读取引用行号区间补全上下文。
func getVectorKBSection(policy toolPolicy, kbDir string) string {
	if !policy.tools["codebase_search"] || strings.TrimSpace(kbDir) == "" {
		return ""
	}
	return `====

VECTOR KNOWLEDGE BASE

A semantic (vector) search index is available over the directory: ` + kbDir + `

You have the codebase_search tool to query this index with natural language.

MANDATORY PRECONDITION: Before writing or modifying code, and before answering
questions that may depend on project-specific knowledge, FIRST call
codebase_search with a concise natural-language query (e.g. existing conventions,
similar implementations, design decisions, API contracts). Use the returned
snippets ("relative/path:start-end" + content) directly; call fs_read on the
cited ranges when you need fuller context.

- Search early and iteratively: distinct aspects of a task deserve separate queries.
- Do NOT guess project conventions if the index can answer them.
- If the index returns no relevant hits, fall back to fs_list/fs_search/fs_read.
- Exact keyword matching is not required; describe the concept instead.`
}

// parallelDelegationSection 并行子任务委派指引段（kaelvoris 扩展，P2.8
// fan-out 能力透出）。引导：分析拆分 → 无依赖子任务同轮批量并行委派 →
// 收割整合；强调并行子任务的独占文件边界（防并发编辑冲突，沉淀自
// 2026-10 多会话并行写码的复盘经验）。子任务上下文不注入。
func parallelDelegationSection() string {
	return `====

PARALLEL DELEGATION

When the user's task involves multiple independent pieces of work, FIRST analyze it and break it down into subtasks. Then delegate in parallel for speed:

- Identify which subtasks are independent (no data/control dependency between them) and which are sequential.
- Batch ALL independent subtasks into a SINGLE message as multiple new_task tool calls - they run concurrently and all results are returned together.
- Use the todos parameter to give each subtask a clear internal plan; assign it an appropriate mode (architect for design/planning, code for implementation, debug for troubleshooting, ask for research).
- Give parallel subtasks EXCLUSIVE boundaries: each subtask must own its own files/packages/modules. Never let two parallel subtasks edit the same file (concurrent edits conflict). Shared or overlapping changes belong to the same subtask, or run them sequentially.
- After collecting results, integrate them: resolve any integration gaps yourself, or delegate follow-up work sequentially.
- Keep sequential work (result-dependent steps) in one-at-a-time new_task calls.

`
}

// modeCfgCustomInstructions 取模式自定义指令（nil 安全）。
func modeCfgCustomInstructions(m *modes.ModeConfig) string {
	if m == nil {
		return ""
	}
	return m.CustomInstructions
}

// ---------------------------------------------------------------------------
// 系统提示词模板态：占位符定义与运行时还原（设置面板「提示词」system 段
// 展示原版模板 → 用户自定义文本注入时还原变量，2026-10-25 用户需求）。
// ---------------------------------------------------------------------------

// 模板态占位符（与 ExpandPromptVars 替换表一一对应；{$...} 形式避免与
// 提示词正文中普通 {var} 流程变量混淆）。
const (
	// TemplateCWD 模板态工作区目录占位符（systemPrompt 内部把 cwd 置为该值，
	// RULES 段 "The project base directory is: {$CWD}" 等句子随之占位化）。
	TemplateCWD = "{$CWD}"
	// TemplateHOME 模板态用户主目录占位符（SYSTEM INFORMATION 段）。
	TemplateHOME = "{$HOME}"
	// TemplateOS 模板态操作系统占位符。
	TemplateOS = "{$OS}"
	// TemplateShell 模板态默认 Shell 占位符。
	TemplateShell = "{$SHELL}"
)

// systemInfoTemplate SYSTEM INFORMATION 段模板版（变量占位，不探测本机；
// git 快照为运行时动态内容，模板态不注入）。
func systemInfoTemplate() string {
	executeCommandSentence := " New terminals will be created in the current workspace directory, however if you change directories in a terminal it will then have a different working directory; changing directories in a terminal does not modify the workspace directory, because you do not have access to change the workspace directory."
	return `====

SYSTEM INFORMATION

Operating System: ` + TemplateOS + `
Default Shell: ` + TemplateShell + `
Home Directory: ` + TemplateHOME + `
Current Workspace Directory: ` + TemplateCWD + `

The Current Workspace Directory is the active VS Code project directory, and is therefore the default directory for all tool operations.` + executeCommandSentence + ` When the user initially gives you a task, a recursive list of all filepaths in the current workspace directory will be included in environment_details. This provides an overview of the project's file structure, offering key insights into the project from directory/file names (how developers conceptualize and organize their code) and file extensions (the language used). This can also guide decision-making on which files to explore further.`
}

// ExpandPromptVars 还原用户自定义系统提示词中的模板占位符（设置面板基于
// SystemPromptTemplate 模板自定义的文本在运行时注入前调用；与模板占位符
// 一一对应，无占位符的旧配置原样返回零破坏）。
func ExpandPromptVars(text, cwd string) string {
	if text == "" {
		return text
	}
	home, _ := os.UserHomeDir()
	rep := strings.NewReplacer(
		TemplateOS, osInfo(),
		TemplateShell, getShellPath(),
		TemplateHOME, filepath.ToSlash(home),
		TemplateCWD, filepath.ToSlash(cwd),
	)
	return rep.Replace(text)
}
