package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"kaelvoris/internal/modes"
)

// isolatedPromptCWD 返回与仓库解耦的提示词 cwd：TempDir 内预置 .git 根标记，
// 使 AGENTS.md 发现链（agents_md.go）仅命中 TempDir 一层且无候选文件。
// 背景：codex 对齐补全 ④ 后，进程 cwd 在本仓库时 SystemPrompt(PromptOptions{})
// 会发现根 AGENTS.md 并注入 USER'S CUSTOM INSTRUCTIONS 包装——注入是设计行为，
// "空选项"断言必须与真实仓库内容解耦才可稳定验证。

// verbatim 锁定 Zoo-Code 3.82.2 原文段落（原样移植回归防线）。
func verbatim(t *testing.T, got, sub, label string) {
	t.Helper()
	if !strings.Contains(got, sub) {
		t.Errorf("prompt %s 段落缺失或被改写:\nwant: %q", label, sub)
	}
}

// isolatedPromptCWD 隔离 SystemPrompt 的 cwd：TempDir 内预置 .git 根标记，
// 使 AGENTS.md 发现链（agents_md.go）仅扫描 TempDir 一层且无候选文件可命中。
// 背景：进程 cwd 在本仓库时，发现链会命中根 AGENTS.md 并注入
// USER'S CUSTOM INSTRUCTIONS 包装（设计行为）；"空选项"类断言必须与
// 真实仓库内容解耦才能稳定验证包装的有无。
func isolatedPromptCWD(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("创建隔离 .git 标记失败: %v", err)
	}
	return dir
}

func TestSystemPrompt_VerbatimSections(t *testing.T) {
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})

	// MARKDOWN RULES（markdown-formatting.ts 原文）。
	verbatim(t, p, "ALL responses MUST show ANY `language construct` OR filename reference as clickable",
		"MARKDOWN RULES")
	// TOOL USE（tool-use.ts 原文开头）。
	verbatim(t, p, "You have access to a set of tools that are executed upon the user's approval. Use the provider-native tool-calling mechanism. Do not include XML markup or examples.",
		"TOOL USE")
	verbatim(t, p, "You must call at least one tool per assistant response.", "TOOL USE one-tool rule")
	// TOOL USE GUIDELINES（tool-use-guidelines.ts 原文；list 示例经 fs_list 适配）。
	verbatim(t, p, "It's critical that you think about each available tool and use the one that best fits the current step in the task.",
		"TOOL USE GUIDELINES")
	verbatim(t, p, "For example using the fs_list tool is more effective than running a command like `ls` in the terminal.",
		"list example (fs_list 适配)")
	// CAPABILITIES（capabilities.ts 原文：clauses 拼接句式）。
	verbatim(t, p, "You have access to tools that let you execute CLI commands on the user's computer, list files, regex search, read files, write and edit files.",
		"CAPABILITIES capability sentence")
	verbatim(t, p, "- These tools help you accomplish tasks.", "CAPABILITIES bullet")
	// MODES（modes.ts + roleDefinition 原文）。
	verbatim(t, p, "You are Kaelvoris, a highly skilled software engineer with extensive knowledge in many programming languages, frameworks, design patterns, and best practices.",
		"roleDefinition")
	verbatim(t, p, "MODES\n\n- These are the currently available modes:", "MODES header")
	// MODES → RULES 之间必须换行（system.ts 模板 ${modesSection}\n${rules}）。
	verbatim(t, p, "expertise areas.\n====\n\nRULES", "modes/rules junction newline")
	// RULES（rules.ts 原文）。
	verbatim(t, p, "The project base directory is:", "RULES cwd")
	verbatim(t, p, "You are STRICTLY FORBIDDEN from starting your messages with",
		"STRICTLY FORBIDDEN rule")
	// SYSTEM INFORMATION（system-info.ts 原文）。
	verbatim(t, p, "Operating System:", "SYSTEM INFORMATION OS")
	verbatim(t, p, "Current Workspace Directory:", "SYSTEM INFORMATION cwd")
	// OBJECTIVE（objective.ts 原文）。
	verbatim(t, p, "You accomplish a given task iteratively, breaking it down into clear steps and working through them methodically.",
		"OBJECTIVE intro")
	verbatim(t, p, "you must use the attempt_completion tool to present the result of the task to the user",
		"OBJECTIVE completion step")

	// 空选项（无语言/全局指令/规则文件）时 addCustomInstructions 返回 ""：
	// 不应输出 USER'S CUSTOM INSTRUCTIONS 包装（对齐 custom-instructions.ts 行为）。
	if strings.Contains(p, "USER'S CUSTOM INSTRUCTIONS") {
		t.Error("空选项不应输出 USER'S CUSTOM INSTRUCTIONS 包装")
	}
}

func TestSystemPrompt_ToolRename(t *testing.T) {
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})
	// 适配后不得残留 Zoo-Code 工具名（fs_apply_diff 内含 apply_diff，需排除前缀匹配）。
	for _, legacy := range []string{"execute_command", "list_files", "search_files", "read_file", "write_to_file", "apply_diff"} {
		re := regexp.MustCompile(`[^_a-zA-Z0-9]` + regexp.QuoteMeta(legacy))
		if re.MatchString(p) {
			t.Errorf("仍残留 Zoo-Code 工具名 %s", legacy)
		}
	}
	// 适配后的 kaelvoris 工具名出现在正文（toolRename 生效处）。
	verbatim(t, p, "Before using the shell tool", "shell rename")
	verbatim(t, p, "you can use the fs_list tool", "fs_list rename")
	verbatim(t, p, "you shouldn't use the fs_read tool", "fs_read rename")
	// 未参与改名的工具名保持原文。
	verbatim(t, p, "attempt_completion tool", "attempt_completion preserved")
	verbatim(t, p, "ask_followup_question tool", "ask_followup_question preserved")
}

func TestSystemPrompt_EditRestriction(t *testing.T) {
	mgr := modes.NewManager()
	arch, ok := mgr.GetBySlug("architect")
	if !ok {
		t.Fatal("architect mode missing")
	}
	allowed := modes.ToolsForMode(arch.Groups)
	p := SystemPrompt(PromptOptions{ToolNames: allowed, Mode: &arch, AllModes: modes.DefaultModes()})

	// architect 模式编辑受限：capabilities 后缀 + RULES FileRestrictionError 规则（原文逐字）。
	verbatim(t, p, " (in this mode only files matching '", "edit restriction capability suffix")
	if !strings.Contains(p, `\.md$`) {
		t.Error("architect edit restriction 缺少 \\.md$ 正则")
	}
	verbatim(t, p, "Some modes have restrictions on which files they can edit. If you attempt to edit a restricted file, the operation will be rejected with a FileRestrictionError",
		"FileRestrictionError rule")

	// 默认全量能力（无编辑约束）不得出现上述片段。
	// （CWD 隔离：防止进程所在仓库 AGENTS.md 内容漂移引发误报。）
	dflt := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})
	if strings.Contains(dflt, "FileRestrictionError") {
		t.Error("默认模式不应输出 FileRestrictionError 规则")
	}
	if strings.Contains(dflt, "in this mode only files matching") {
		t.Error("默认模式不应输出编辑约束后缀")
	}
}

func TestSystemPrompt_NoShellNoCommandSection(t *testing.T) {
	mgr := modes.NewManager()
	arch, _ := mgr.GetBySlug("architect")
	allowed := modes.ToolsForMode(arch.Groups)
	if containsStr(allowed, "shell") {
		t.Fatal("architect 不应包含 shell")
	}
	p := SystemPrompt(PromptOptions{ToolNames: allowed, Mode: &arch, AllModes: modes.DefaultModes(), CWD: isolatedPromptCWD(t)})
	// 无 execute_command 时不得出现命令链操作符提示（gating 生效）。
	if strings.Contains(p, "You should use `&&`") && strings.Contains(p, "for PowerShell command chaining") {
		t.Error("无 shell 工具时不应输出命令链提示")
	}
	if strings.Contains(p, "Before using the shell tool") {
		t.Error("无 shell 工具时不应输出 execute_command 规则")
	}
}

// TestSystemPrompt_WorkspaceMemorySection 工作空间记忆段回归（2026-10-31 双层记忆）：
// WSDoc 非空时注入 WORKSPACE MEMORY 段（含共享提示与路径），空时不注入；
// 与 SESSION MEMORY 段并存时两层分开（互不混排）。
func TestSystemPrompt_WorkspaceMemorySection(t *testing.T) {
	cwd := isolatedPromptCWD(t)
	p := SystemPrompt(PromptOptions{CWD: cwd, MemoryMD: "# 会话记忆\n- 会话级", WSDoc: "# 项目记忆\n- 项目级", WSPath: "C:\\wm\\abc.memory.md"})
	if !strings.Contains(p, "SESSION MEMORY") || !strings.Contains(p, "会话级") {
		t.Fatal("SESSION MEMORY 段缺失")
	}
	if !strings.Contains(p, "WORKSPACE MEMORY") || !strings.Contains(p, "项目级") {
		t.Fatal("WORKSPACE MEMORY 段缺失")
	}
	if !strings.Contains(p, "shared across all sessions") || !strings.Contains(p, "C:\\wm\\abc.memory.md") {
		t.Fatal("工作空间记忆路径/共享提示缺失")
	}
	if strings.Index(p, "SESSION MEMORY") >= strings.Index(p, "WORKSPACE MEMORY") {
		t.Fatal("WORKSPACE MEMORY 应置于 SESSION MEMORY 之后")
	}
	p2 := SystemPrompt(PromptOptions{CWD: cwd})
	if strings.Contains(p2, "WORKSPACE MEMORY") {
		t.Fatal("WSDoc 为空时不应注入工作空间记忆段")
	}
}

// TestSystemPrompt_WrapupGate 收尾门禁回归（2026-10 会话复盘：go build/test
// 验证经常被留成未完成待办，实现收尾前未验证即 attempt_completion）：
// Go 项目根（存在 go.mod）时 RULES 段必须注入收尾门禁；非 Go 项目不注入。
func TestSystemPrompt_WrapupGate(t *testing.T) {
	dir := t.TempDir()
	// Go 项目根：创建 go.mod → 注入门禁。
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := SystemPrompt(PromptOptions{CWD: dir})
	verbatim(t, p, "Before calling attempt_completion to finish a coding task, you MUST first run the project's build/test gate",
		"wrapup gate rule (go.mod present)")
	// 非 Go 项目根：无 go.mod → 不注入。
	dir2 := t.TempDir()
	p2 := SystemPrompt(PromptOptions{CWD: dir2})
	if strings.Contains(p2, "Before calling attempt_completion to finish a coding task, you MUST first run the project's build/test gate") {
		t.Error("非 Go 项目不应注入收尾门禁")
	}
}

// TestSystemPrompt_LazySeniorDevSection Ponytail 懒人资深工程师守则回归
// （kaelvoris 扩展，移植自开源项目 DietrichGebert/ponytail）：段必须无条件
// 注入（守则类），关键治理点（YAGNI 阶梯 / 修根因 / 输出克制 / 不可懒清单）
// 不得遗漏，避免提示词被后续调整静默削弱。
func TestSystemPrompt_LazySeniorDevSection(t *testing.T) {
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})

	// 段头与核心题旨。
	verbatim(t, p, "LAZY SENIOR DEV MODE", "lazy section header")
	verbatim(t, p, "The best code is the code never written.", "lazy motto")

	// 七级 YAGNI 阶梯的关键梯级（复刻原仓库 ladder）。
	verbatim(t, p, "Does this need to be built at all?", "ladder rung 1")
	verbatim(t, p, "Already in this codebase?", "ladder rung 2")
	verbatim(t, p, "Already-installed dependency solves it?", "ladder rung 5")
	verbatim(t, p, "Only then: write the minimum code that works.", "ladder rung 7")
	verbatim(t, p, "trace the real flow end to end, then climb", "ladder runs after understanding")

	// bug 修复 = 修根因（原仓库核心论断）。
	verbatim(t, p, "BUG FIX = ROOT CAUSE, NOT SYMPTOM", "root cause rule")
	verbatim(t, p, "Grep every caller of the function you touch and fix the shared function once", "root cause detail")

	// 输出克制（治话痨的核心条款）。
	verbatim(t, p, "Code first. Then at most three short lines", "output constraint")
	verbatim(t, p, "If the explanation is longer than the code, delete the explanation", "essay ban")
	verbatim(t, p, "skipped: [X], add when [Y]", "output pattern")

	// 不可懒清单（安全兜底，防止被过度执行）。
	verbatim(t, p, "WHEN NOT TO BE LAZY", "not-lazy guard")
	verbatim(t, p, "never lazy about reading", "comprehension guard")
	verbatim(t, p, "No frameworks, no fixtures", "check constraint") // 一个可运行检查

	// 空选项 cwd 隔离时同样注入（无条件守则，与 AGENTS.md 无关）。
	if !strings.Contains(p, "Ponytail governs what you build") {
		t.Error("守则段应无条件注入")
	}
}

// TestSystemPrompt_McpCapability MCP 修复回归（对齐 Zoo-Code effective-tool-policy
// resolveHasMcpTools → capabilities MCP bullet）：模式含 mcp 组且 ToolNames 带
// mcp_ 前缀动态工具（= 注册中心实连后注入）时，提示词必须向模型声明 MCP 能力；
// 无动态工具时（未配置/未连接）不得出现（防模型臆造调用）。
func TestSystemPrompt_McpCapability(t *testing.T) {
	mgr := modes.NewManager()
	code, ok := mgr.GetBySlug("code")
	if !ok {
		t.Fatal("code mode missing")
	}
	allowed := modes.ToolsForMode(code.Groups)
	base := PromptOptions{ToolNames: append(allowed, "mcp_demo_echo"), Mode: &code, AllModes: modes.DefaultModes(), CWD: isolatedPromptCWD(t)}

	p := SystemPrompt(base)
	verbatim(t, p, "You have access to MCP servers", "MCP capability bullet")

	// 无 mcp_ 动态工具：MCP 能力句不出现。
	without := base
	without.ToolNames = allowed
	p2 := SystemPrompt(without)
	if strings.Contains(p2, "You have access to MCP servers") {
		t.Error("无 MCP 动态工具时不应输出 MCP 能力句")
	}
}

func TestSystemPrompt_TodoAndSubtaskExtensions(t *testing.T) {
	p := SystemPrompt(PromptOptions{
		TodosMD:   "- [x] done item\n- [ ] pending item",
		IsSubtask: true,
	})
	verbatim(t, p, "CURRENT TODO LIST", "TODO extension")
	verbatim(t, p, "When task status changes, remember to call the update_todo_list tool to update your progress.",
		"todo tool reminder") // update_todo_list 不在改名映射中
	// todo 纪律（2026-11 对标 Claude Code TodoWrite 2.1.84）：恰好一个 in_progress /
	// 验证完成才标绿 / 即时更新不攒批。
	verbatim(t, p, "keep exactly ONE item in_progress at a time", "todo single in_progress")
	verbatim(t, p, "never while tests are failing or the implementation is partial", "todo verified-complete-only")
	verbatim(t, p, "update immediately after finishing an item instead of batching completions", "todo immediate update")
	verbatim(t, p, "SUBTASK CONTEXT", "subtask extension")
	verbatim(t, p, "Do not use new_task. When the delegated work is done, signal completion via attempt_completion",
		"subtask completion protocol")
}

// TestSystemPrompt_FullScopeDeliverySection 完整交付守则回归（2026-11 第二轮开源
// 提示词对标吸收：Claude Code 2.1.218 "Delivering work at full scope" /
// 2.1.293 "Finish the request instead of stopping early" / 2.1.173 "Act when ready"）：
// 段必须无条件注入，关键治理点（范围即交付物 / 卡住先做其余 / 停止点两条件 /
// 任务大小不是先问的理由 / 给推荐不做穷举 / 首工具前一句话预告）不得被静默削弱；
// 段序锁定在 OBJECTIVE 之后、DELIVERY VERIFICATION 之前（交付闭环：做到全 → 验证 → 如实报）。
func TestSystemPrompt_FullScopeDeliverySection(t *testing.T) {
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})

	// 段头与来源条款核心句。
	verbatim(t, p, "FULL-SCOPE DELIVERY", "full-scope section header")
	verbatim(t, p, "The requested scope is the deliverable - do not quietly narrow, widen, or transform it.", "scope is deliverable")
	verbatim(t, p, "Interpret ambiguity the way a careful colleague would", "careful-colleague ambiguity")
	verbatim(t, p, "First do everything that does not depend on the stuck part, then report exactly what is stuck and why.", "blocked-first-rest")
	verbatim(t, p, "Finish the whole task, not just the easy parts", "whole-task rule")
	verbatim(t, p, "Scaling the work down is the user's call, not yours.", "no quiet downscale")
	// 停止点两条件（Claude Code 2.1.293）。
	verbatim(t, p, "End your turn only when the request is done or nothing is left that you can do without the user.", "stop-point two conditions")
	verbatim(t, p, "The size of a task is not a reason to check in first", "size is not a reason to ask")
	// Claude Code 2.1.173 act-when-ready。
	verbatim(t, p, "Give a recommendation instead of an exhaustive survey of options.", "recommend over survey")
	// 进度可见性（Claude Code 2.1.104 text output）。
	verbatim(t, p, "Before your first tool call, state in one sentence what you are about to do", "preamble one-sentence")

	// 段序：OBJECTIVE 之后、DELIVERY VERIFICATION 之前。
	if !(strings.Index(p, "OBJECTIVE") < strings.Index(p, "FULL-SCOPE DELIVERY") &&
		strings.Index(p, "FULL-SCOPE DELIVERY") < strings.Index(p, "DELIVERY VERIFICATION")) {
		t.Error("FULL-SCOPE DELIVERY 应位于 OBJECTIVE 与 DELIVERY VERIFICATION 之间")
	}

	// 无条件注入（守则类）：空选项 cwd 隔离时同样存在，与 AGENTS.md 无关。
	if !strings.Contains(p, "The requested scope is the deliverable") {
		t.Error("完整交付守则段应无条件注入")
	}
}

// TestSystemPrompt_DeliveryVerificationTruthfulness 交付验证「如实汇报」条款回归
// （2026-11 对标 Claude Code 2.1.271 "Action safety and truthful reporting"：
// 测试失败就说失败并附输出、跳过明说、不虚报已验证）。
func TestSystemPrompt_DeliveryVerificationTruthfulness(t *testing.T) {
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})
	verbatim(t, p, "4. 如实汇报：测试失败就说失败并附实际输出；跳过的步骤明说；未验证的部分不要声称已验证——报告与事实一致，不加对冲性含糊。",
		"truthful reporting clause")
}

// TestSystemPrompt_RegressionTestRule G4 回归测试「先看失败」规程回归
// （2026-12 对标 Claude-Code-Game-Studios test-standards：修 bug 前先在未修复代码上
// 跑失败测试亲眼看它失败，再修复并看它通过——从未失败过的测试证明不了任何事）。
func TestSystemPrompt_RegressionTestRule(t *testing.T) {
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})
	verbatim(t, p, "REGRESSION TESTS: when fixing a bug, run the failing test on the UNFIXED code first and watch it fail, then fix and watch it pass - a test that never failed proves nothing (CCGS test-standards).",
		"regression test rule")

	// 段序：位于 LAZY SENIOR DEV 段内（BUG FIX = ROOT CAUSE 之后、RULES: 之前）。
	if !(strings.Index(p, "BUG FIX = ROOT CAUSE") < strings.Index(p, "REGRESSION TESTS") &&
		strings.Index(p, "REGRESSION TESTS") < strings.Index(p, "RULES:")) {
		t.Error("REGRESSION TESTS 应位于 BUG FIX 段与 RULES 之间")
	}
}

// TestSystemPrompt_ParallelDelegation P2.8 并行委派指引段门控：
// ParallelDelegation=true 且非子任务时注入；子任务/关闭时不注入。
func TestSystemPrompt_ParallelDelegation(t *testing.T) {
	p := SystemPrompt(PromptOptions{ParallelDelegation: true})
	verbatim(t, p, "PARALLEL DELEGATION", "parallel delegation header")
	verbatim(t, p, "Batch ALL independent subtasks into a SINGLE message as multiple new_task tool calls",
		"parallel fan-out instruction")
	verbatim(t, p, "Give parallel subtasks EXCLUSIVE boundaries", "exclusive boundary rule")

	// 子任务上下文不注入（子任务禁止 new_task，避免矛盾指令）。
	sub := SystemPrompt(PromptOptions{ParallelDelegation: true, IsSubtask: true})
	if strings.Contains(sub, "PARALLEL DELEGATION") {
		t.Error("subtask prompt should not contain PARALLEL DELEGATION")
	}

	// 未开启时不注入。
	off := SystemPrompt(PromptOptions{})
	if strings.Contains(off, "PARALLEL DELEGATION") {
		t.Error("prompt should not contain PARALLEL DELEGATION when disabled")
	}
}

func TestSystemPrompt_LanguagePreference(t *testing.T) {
	p := SystemPrompt(PromptOptions{Language: "zh-CN"})
	verbatim(t, p, "Language Preference:", "language preference header")
	verbatim(t, p, `You should always speak and think in the "简体中文" (zh-CN) language`,
		"language value")
}

func TestSystemPrompt_AutoDecideRule(t *testing.T) {
	// AutoDecide=true 且 ask_followup 可用：RULES 段注入 FULL-AUTO DECISION 指引。
	p := SystemPrompt(PromptOptions{AutoDecide: true})
	if !strings.Contains(p, "FULL-AUTO DECISION") {
		t.Error("AutoDecide=true 应注入 FULL-AUTO DECISION 段")
	}
	if !strings.Contains(p, "does NOT pause for an answer") {
		t.Error("AutoDecide 段应说明 ask_followup 不阻塞")
	}

	// AutoDecide=false（默认）：不注入。
	off := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})
	if strings.Contains(off, "FULL-AUTO DECISION") {
		t.Error("AutoDecide=false 不应注入 FULL-AUTO DECISION 段")
	}
}

func TestSystemPrompt_CustomInstructionsWrapper(t *testing.T) {
	p := SystemPrompt(PromptOptions{
		Language:                 "zh-CN",
		GlobalCustomInstructions: "Always answer concisely.",
	})
	verbatim(t, p, "====\n\nUSER'S CUSTOM INSTRUCTIONS", "custom instructions wrapper")
	verbatim(t, p, "The following additional instructions are provided by the user, and should be followed to the best of your ability.",
		"custom instructions intro")
	verbatim(t, p, "Global Instructions:\nAlways answer concisely.", "global instructions section")
	verbatim(t, p, `You should always speak and think in the "简体中文" (zh-CN) language`,
		"language preference section")
}

func TestSystemPrompt_SkillsSection(t *testing.T) {
	xml := "<available_skills>\n  <skill>\n    <name>demo</name>\n    <description>Do demo things.</description>\n    <location>C:/skills/demo/SKILL.md</location>\n  </skill>\n</available_skills>"
	p := SystemPrompt(PromptOptions{SkillsXML: xml})
	verbatim(t, p, "AVAILABLE SKILLS", "skills header")
	verbatim(t, p, "REQUIRED PRECONDITION", "skill precondition")
	verbatim(t, p, "Do NOT load every skill up front.", "skill constraint")
	verbatim(t, p, "<skill_check_completed>true|false</skill_check_completed>", "internal verification marker")
	verbatim(t, p, `The skill list is already filtered for the current mode: "code".`, "context notes")

	// SkillsXML 为空时不输出 AVAILABLE SKILLS 段（skill 工具在但无技能可用）。
	empty := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})
	if strings.Contains(empty, "AVAILABLE SKILLS") {
		t.Error("无技能 XML 时不应输出 AVAILABLE SKILLS 段")
	}
}

func TestOSInfo_WindowsVersionFormat(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 平台验证 cmd /c ver 解析")
	}
	got := osInfo()
	// 中文系统输出 GBK "Microsoft Windows [版本 10.0.19045.3930]"，
	// 修复后应稳定输出 "Windows 10.0.19045.3930" 形式，不出现乱码。
	if !regexp.MustCompile(`^Windows \d+(\.\d+)+$`).MatchString(got) {
		t.Errorf("OS 描述格式异常: %q", got)
	}
}

// TestSystemPrompt_ShellExecutorAlignment 会话复盘回归（2026-10-01）：
// Windows 下 shell/unified_exec 实际执行器是 cmd /C（shell_tools.go /
// unified_exec.go），提示词必须如实声明，否则模型按 PowerShell 管道语法
// 写命令直接报 "'Select-Object' is not recognized"（历史高频错误源）。
func TestSystemPrompt_ShellExecutorAlignment(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 验证 cmd /C 执行器语义")
	}
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})
	// Default Shell 必须是 cmd.exe（与工具层实际执行器一致），不得再宣称 powershell。
	if strings.Contains(p, "Default Shell: C:\\Windows\\System32\\WindowsPowerShell") {
		t.Error("SYSTEM INFORMATION 不应再宣称 Default Shell 为 powershell.exe（实际执行器是 cmd /C）")
	}
	verbatim(t, p, "Default Shell: C:\\Windows\\System32\\cmd.exe", "cmd.exe declared")
	// 链操作符提示：cmd 下必须推荐 && 且给出 PowerShell 包裹指引。
	verbatim(t, p, "Using `&&` for cmd.exe command chaining", "cmd chain operator")
	verbatim(t, p, "powershell -NoProfile -Command \"...\"", "powershell wrap guidance")
	// RULES 里不应再出现 PowerShell 链操作符 `;` 指引（cmd 语义下误导）。
	if strings.Contains(p, "Using `;` for PowerShell command chaining") {
		t.Error("cmd 执行器下不应输出 PowerShell ; 链提示")
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestSystemPrompt_ProfessionalConduct 职业操守段回归（kaelvoris 扩展，2026-10-25
// 开源系统提示词调研吸收：Claude Code 2.0 / Codex GPT-5 / Cursor 2.0）：段必须
// 无条件注入；六条关键治理点——客观性、回答克制、只做被要求的事、脏工作区
// 保护、破坏性操作红线、修错循环上限——不得缺失，避免被后续调整静默削弱。
func TestSystemPrompt_ProfessionalConduct(t *testing.T) {
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})

	// 段头与六条核心条款（对应开源来源见 getProfessionalConductSection 注释）。
	verbatim(t, p, "PROFESSIONAL CONDUCT", "conduct header")
	verbatim(t, p, "Objectivity over flattery", "anti-sycophancy (Claude Code 2.0)")
	verbatim(t, p, "Answer, then stop", "tone/length rule (Claude Code 2.0)")
	verbatim(t, p, "Only what was asked", "do-what-asked (Claude Code 2.0 reminders)")
	verbatim(t, p, "never proactively create documentation files", "no proactive docs")
	verbatim(t, p, "Never revert changes you did not make", "dirty worktree (Codex GPT-5)")
	verbatim(t, p, "re-read the file and rebuild the edit from fresh content", "write-conflict protocol")
	verbatim(t, p, "Destructive action red line", "git safety (Claude Code 2.0 + Codex)")
	verbatim(t, p, "Bounded retry loops", "3-attempt cap (Cursor 2.0)")

	// 段序：RULES 之后、LAZY SENIOR DEV 之前（先普适操守，后写码风格）。
	conductIdx := strings.Index(p, "PROFESSIONAL CONDUCT")
	lazyIdx := strings.Index(p, "LAZY SENIOR DEV MODE")
	if conductIdx < 0 || lazyIdx < 0 || conductIdx > lazyIdx {
		t.Errorf("段序错误：PROFESSIONAL CONDUCT 应位于 LAZY SENIOR DEV 之前 (conduct=%d, lazy=%d)", conductIdx, lazyIdx)
	}
}

// TestSystemPrompt_DeliveryVerification 交付验证守则段回归（kaelvoris 扩展，
// 中文显式规则，治「改完即完成」）：段必须无条件注入；关键条款——构建/测试
// 门禁、部署后线上验证、证据入报告——不得缺失，避免被后续调整静默削弱。
func TestSystemPrompt_DeliveryVerification(t *testing.T) {
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})

	verbatim(t, p, "DELIVERY VERIFICATION（交付验证）", "delivery verification header")
	verbatim(t, p, "构建与测试门禁", "build/test gate clause")
	verbatim(t, p, "go build ./...", "go build gate")
	verbatim(t, p, "部署与线上验证", "deploy & online verify clause")
	verbatim(t, p, "禁止「改完即完成」", "no edited-equal-done rule")
}

// TestSystemPrompt_AutonomousDev 自主开发守则段回归（kaelvoris 扩展，2026-10-25
// 联网调研吸收 Anthropic long-running agents / Ralph Wiggum / Devin 精华）：段必须
// 无条件注入；九条循环纪律的关键条款不得缺失，避免被后续调整静默削弱。
func TestSystemPrompt_AutonomousDev(t *testing.T) {
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})

	verbatim(t, p, "AUTONOMOUS DEVELOPMENT（自主开发守则）", "autonomous dev header")
	// 段序：DELIVERY VERIFICATION 之后、PARALLEL DELEGATION 之前（先单次交付验证，后多轮循环纪律）。
	deliveryIdx := strings.Index(p, "DELIVERY VERIFICATION")
	autoIdx := strings.Index(p, "AUTONOMOUS DEVELOPMENT")
	if deliveryIdx < 0 || autoIdx < 0 || autoIdx < deliveryIdx {
		t.Errorf("段序错误：AUTONOMOUS DEVELOPMENT 应位于 DELIVERY VERIFICATION 之后 (delivery=%d, auto=%d)", deliveryIdx, autoIdx)
	}
	// 九条纪律锚点（每条一个关键短语）。
	verbatim(t, p, "先上手，再动手", "get-your-bearings clause")
	verbatim(t, p, "增量推进", "incremental progress clause")
	verbatim(t, p, "先定义「完成」", "definition-of-done clause")
	verbatim(t, p, "像用户一样自证", "end-to-end verify clause")
	verbatim(t, p, "每轮保绿", "keep-it-green clause")
	verbatim(t, p, "留下足迹", "leave-a-trail clause")
	verbatim(t, p, "不许自欺", "no-self-deception clause")
	verbatim(t, p, "有界止损", "bounded-stop clause")
	verbatim(t, p, "无人值守纪律", "unattended-discipline clause")
}

// TestSystemPrompt_AutonomousDevOverride 自主开发守则段覆盖回归（设置面板
// 「提示词」autonomousDev 段）：配置非空 = 整段替换内置默认；配置空 = 内置默认。
func TestSystemPrompt_AutonomousDevOverride(t *testing.T) {
	// 非空覆盖：出现用户文本，内置默认正文不再出现
	p := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t), AutonomousDev: "自定义自主开发守则 ABC"})
	verbatim(t, p, "自定义自主开发守则 ABC", "autonomous dev override text")
	if strings.Contains(p, "先上手，再动手") {
		t.Errorf("override should replace builtin section, but builtin clause still present")
	}
	// 空配置：内置默认生效
	d := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t)})
	verbatim(t, d, "AUTONOMOUS DEVELOPMENT（自主开发守则）", "autonomous dev default header")
	// 纯空白配置：等同空（TrimSpace 兜底，不注入空段）
	w := SystemPrompt(PromptOptions{CWD: isolatedPromptCWD(t), AutonomousDev: "  \n "})
	verbatim(t, w, "AUTONOMOUS DEVELOPMENT（自主开发守则）", "whitespace override falls back to default")
}
