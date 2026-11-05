// Package agent Agent 引擎：移植自 Zoo-Code src/core/task/Task.ts 主循环。
//
// 对应关系：
//   - startTask + initiateTaskLoop（while !abort）→ Engine.Run
//   - recursivelyMakeClineRequests 的重试/纠偏 → loop 内 mistake 处理
//   - ask/say 双类型消息 → pkg/message.ClineMessage
//   - attempt_completion / ask_followup_question → tools 包特殊标记
//   - P2 编排：模式过滤工具集 + new_task 子任务栈 + switch_mode（Task.ts startSubtask / resumeAfterDelegation）
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"kaelvoris/internal/approval"
	"kaelvoris/internal/checkpoint"
	"kaelvoris/internal/condense"
	"kaelvoris/internal/hooks"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/mentions"
	"kaelvoris/internal/modes"
	"kaelvoris/internal/tools"
	"kaelvoris/internal/usage"
	"kaelvoris/internal/verify"
	"kaelvoris/pkg/message"
)

// Engine Agent 主引擎。
type Engine struct {
	LLM      *llm.Client
	Registry *tools.Registry
	// AutoApprove 自动审批钩子（移植 AutoApprovalHandler 简版）：返回 true 跳过人工应答。
	AutoApprove func(msg message.ClineMessage) bool
	// DangerAutoApprove 危险操作自动确认（用户需求 2026-10）：true 时命令命中
	// 危险清单免审放行（auto-select 最优方案继续执行）；敏感文件写入红线
	// 不受影响（sensitiveWriteCall 仍强制人工）。由 App.buildRunEngine 按配置注入。
	DangerAutoApprove bool
	// AutoDecide 全AI自动决策（用户需求 2026-10）：true 时 ask_followup_question
	// 不再阻塞等待用户回答——问题记录留痕后回填「请结合上下文自主决策」指令，
	// 模型自行推演选择。由宿主按 AutoApprove 配置注入（与 AutoApprove 同源，
	// 另立字段避免语义耦合：AutoApprove 只管工具审批，AutoDecide 只管提问工具）。
	AutoDecide bool
	// MaxRequests 单任务 API 请求上限（对齐 MAX_REQUESTS_PER_TASK 防失控）。
	// 达到时交互模式询问是否续期；默认 1000 万轮 ≈ 不设限（40 轮就弹
	// 「是否继续执行」对长任务过于打扰，用户可显式调低恢复防失控语义）。
	MaxRequests int
	// MistakeLimit 连续无工具调用上限（对齐 consecutiveMistakeLimit）。
	MistakeLimit int
	// ToolRepetitionLimit 连续完全相同工具调用的上限（<=0 = 关闭检测；
	// 默认 3，对齐 Zoo-Code ToolRepetitionDetector 防同参调用死循环）。
	ToolRepetitionLimit int
	// StreamRetryLimit LLM 流式请求瞬时错误（流空闲看门狗超时/429/5xx/
	// 连接中断）的自动重试上限（0 = 未设置默认 3；<0 = 禁用自动重试）。
	// 无人值守续跑修复：旧逻辑一次流错误就弹 api_req_failed 卡片阻塞
	// 等用户点重试——定时任务/通道/夜间批量场景无人应答 = 任务假死
	// "运行中"且不再推进。现在瞬时错误按退避自动重试，耗尽才升级卡片。
	StreamRetryLimit int
	// MaxSubtaskDepth 子任务嵌套上限（防递归失控）。
	MaxSubtaskDepth int
	// Modes 模式管理器（nil = 仅默认 code 语义，不裁剪工具）。
	Modes *modes.Manager
	// Todos 任务清单（nil = 不启用）。
	Todos *tools.TodoStore
	// SkillsXML 提供系统提示词 <available_skills> 段（nil = 无技能）。
	SkillsXML func() string
	// VectorKBDir 向量知识库已索引目录（Zoo-Code code-index 对齐；
	// 非空 = 向量回答开启——系统提示词注入 VECTOR KNOWLEDGE BASE 段，
	// 要求写码/答题前先 codebase_search 语义检索。空 = 不注入）。
	VectorKBDir string
	// UserImages 下一轮任务的图片附件（data URL / https URL）。
	// 桌面/通道层在调用 RunInMode 前设置；本轮 apiMessages 构造时消费并清空
	// （模式切换递归重建上下文时不再重发，避免重复占用多模态 token）。
	UserImages []string
	// Workspace 项目根目录（注入提示词 RULES/SYSTEM INFORMATION；空 = os.Getwd()）。
	Workspace string
	// ResumeDigest 会话恢复上下文（P3.1-B：停止后继续时，通道层把最近 N 条
	// 历史消息摘要为一条 user 消息注入；空 = 全新任务不注入）。
	ResumeDigest string
	// MemoryDoc 会话记忆文档内容（P3.1-C：MEMORY.md 主体+指针，注入系统
	// 提示词 SESSION MEMORY 段；空 = 无记忆）。
	MemoryDoc string
	// MemoryPath 会话记忆文档绝对路径（注入提示词，引擎可 fs_read/fs_write 维护）。
	MemoryPath string
	// WSDoc / WSPath 项目工作空间记忆（用户需求 2026-10-31）：跨会话共享、
	// 按工作区路径隔离的 WORKSPACE.md；与 MemoryDoc（会话级）分层注入，
	// 空 = 无工作空间记忆段。
	WSDoc  string
	WSPath string
	// EvolveSnippet 自进化经验段（<self_evolution>；通道层按任务文本命中
	// 经验卡生成；空 = 不注入。实现编码能力沉淀闭环）。
	EvolveSnippet string
	// ManualPreview 会话卡片手动摘要（规划板 2026-10 会话卡片信息升级；通道层
	// 从会话记录读入；空 = 不注入。用户在侧栏会话卡设置的标注随任务注入提示词）。
	ManualPreview string
	// Language 用户语言偏好（注入 USER'S CUSTOM INSTRUCTIONS；空 = 不注入）。
	Language string
	// GlobalCustomInstructions 全局自定义指令（空 = 从规则文件加载）。
	GlobalCustomInstructions string
	Log                      *slog.Logger

	// ---- 数字员工（§5 角色提示词 + §6 模型参数 + §4/§7/§8 权限网关）----
	// EmployeeRole 员工角色段完整文本（employee.Employee.RoleSection 产物，
	// 已渲染变量并含工作目录边界/只读声明/进化提案规则）。空 = 无员工接管。
	// 非空时注入系统提示词 EMPLOYEE ROLE 段（位于 roleDefinition 之后）。
	EmployeeRole string
	// EmpTemperature/EmpTopP/EmpMaxTokens 员工级模型采样参数（§6.2）。
	// nil = 使用服务端默认值（与既有行为一致）。
	EmpTemperature *float64
	EmpTopP        *float64
	EmpMaxTokens   *int
	// EmpMemoryDoc 员工记忆沉淀段（§11.4：审批落地的 BEHAVIOR/TOOLS/
	// WORKFLOWS.md 合集，employee.LoadMemory 产物）。与 MemoryDoc（会话
	// 记忆，通道层每次任务前覆盖赋值）分离注入，避免被通道层覆盖冲掉；
	// 组装提示词时并入 SESSION MEMORY 段。
	EmpMemoryDoc string

	// ---- P3/B5 上下文治理（对齐 Zoo-Code context-management）----
	// ContextWindowTokens 模型上下文窗口（token）。<=0 = 不启用上下文治理
	// （apiMessages 保持只增不减的历史行为）。>0 时每轮请求前估算可见上下文
	// token，超过 阈值 = 窗口*0.9 - 输出预留 即触发滑动窗口截断。
	ContextWindowTokens int
	// ReservedOutputTokens 输出预留 token（对齐 manageContext 的 reserved
	// = maxTokens 兜底；默认 4096）。
	ReservedOutputTokens int
	// ToolResultMaxChars 工具结果单条体积上限（字节）。>0 时超限结果被截断
	// 并附加续读提示（413 防护第二层：单条超大工具结果即使截断旧历史也
	// 无法压入预算，必须在入列前限制体积）。<=0 取默认 40000（≈10k token）。
	ToolResultMaxChars int
	// TruncationFrac 滑动窗口截断比例（0~1；<=0 或 >=1 取 DefaultFrac=0.5，
	// 对齐 Zoo-Code manageContext 兜底截断比例）。
	TruncationFrac float64
	// CondenseTriggerPercent 上下文占用触发百分比（窗口 × percent/100 −
	// 输出预留 = 截断阈值；<=0 取 90，对齐 Zoo-Code TOKEN_BUFFER_PERCENTAGE=0.1）。
	CondenseTriggerPercent int
	// Summarizer LLM 摘要压缩主路径的摘要生成回调（codex 对齐补全 ①）。
	// Summarizer LLM 摘要压缩主路径的摘要生成回调（codex 对齐补全 ①）。
	// 非 nil：上下文超阈值时优先尝试摘要压缩（condense.SummarizeConversation），
	// 失败（nil/错误/空摘要）回退滑动窗口截断（对齐 codex compact.rs 主路径 +
	// Zoo-Code manageContext 截断兑底的双层语义）。
	// nil：保持仅截断行为。桌面端默认 LLMClientSummarizer(e.LLM) 注入。
	Summarizer condense.Summarizer
	// CondenseEnabled 摘要压缩总开关（规划板 2026-10：压缩行为可在设置中关闭）。
	// false：跳过 LLM 摘要与滑动窗口截断（保留硬裁剪兑底，保命防 413），
	// 上下文超阈值仅记录日志不再压缩历史。
	CondenseEnabled bool
	// CondenseKeepRecent 摘要压缩保留最近 N 条原文（0 = 不保留，历史全部
	// 可入摘要；默认 8）：摘要仅覆盖较早历史，近期细节零损失。
	CondenseKeepRecent int
	// OnCondense 摘要压缩成功回调（宿主层接线：摘要正文落会话存储，
	// 前端消息流标注「已压缩」）。参数为被替换消息数、保留条数、LLM 摘要正文。
	// nil = 不回调（CLI/测试场景）。
	OnCondense func(removed, kept int, summary string)
	// CondenseFailedAttempts 摘要压缩连续失败计数（连续失败时本轮任务内
	// 跳过摘要直接截断，避免每轮都白烧一次 LLM 请求）。
	condenseFails int

	// ---- 会话级 Hook（任务结束回调，2026-10 会话总结功能）----
	// 注意：旧声明 OnTaskFinished func(result string, err error) 已被下方扩展签名
	// 取代（需 ctx/userTask/elapsed 供总结生成）——见下方 OnTaskFinished 完整注释。

	// ---- Codex 对齐补全 ⑥：Hooks 事件点（对齐 hook_runtime.rs 简版）----
	// FallbackLLM 主模型失败自动降级备用客户端（E3，第四批，对标 Plandex
	// errorFallback / task-master fallback model）：主模型流式请求瞬时错误
	// 重试耗尽后、进 api_req_failed 卡片之前，自动切换备用模型续跑（req
	// 不耗预算；降级后本任务内持续使用备用模型，防主备反复震荡）。
	// 确定性错误（401/403/404）不降级（备用大概率同配置问题，快速失败）。
	// nil = 无降级（行为与既有 StreamRetryLimit 耗尽进卡片一致）。
	FallbackLLM *llm.Client
	// fallbackActive 本任务已降级标记（降级后本任务持续用备用模型；
	// RunInMode 开始时重置，子任务/新任务恢复主模型探测机会）。
	fallbackActive bool
	// HookPreToolUse 工具执行前钩子：返回 (allow, newArgs, blockMsg)。
	// allow=false 阻断该调用（blockMsg 回填模型）；newArgs 非 "" 时替换参数
	// （可改参注入，对齐 PreToolUseHookResult::Mutate 参数改写语义）。nil = 无钩子。
	HookPreToolUse func(name, args string) (allow bool, newArgs string, blockMsg string)
	// HookPostToolUse 工具执行成功后钩子（结果旁路观察点；对齐
	// run_post_tool_use_hooks 简版，不修改结果）。nil = 无钩子。
	HookPostToolUse func(name, args, result string)

	// ---- Claude Code Hooks 对齐：用户可配置生命周期钩子（hooks.json）----
	// UserHooks 用户钩子管理器（非 nil 时：工具执行前/后与任务收尾按配置
	// 执行外部命令钩子；阻断/改参语义见 internal/hooks）。nil = 无用户钩子。
	// 与 HookPreToolUse（程序内钩子，如数字员工权限网关）叠加执行：先员工
	// 网关后用户钩子，任一阻断即阻断。
	UserHooks *hooks.Manager

	// ---- 用户自定义提示词覆盖（设置面板「提示词」配置，2026-11）----
	// PromptOverride 非空时以最高优先级注入系统提示词末尾（见 prompts.go
	// SystemPrompt 的 USER CUSTOM PROMPT OVERRIDE 段）；空 = 用内置默认。
	PromptOverride string
	// AutonomousDev 自主开发守则段覆盖（设置面板「提示词」autonomousDev 段；
	// 非空时整段替换内置 AUTONOMOUS DEVELOPMENT 段，空 = 用内置默认）。
	AutonomousDev string

	// ---- 会话 hook：任务结束回调（会话总结，2026-10 新增）----
	// OnTaskFinished 任务结束统一回调（完成/出错/上限终止/用户停止均触发，
	// 仅顶层任务（isSubtask=false）触发——子任务由各自宿主路径处理）。
	// 参数：任务文本、最终结果、错误（nil=正常）、任务耗时。nil = 不回调
	//（CLI/测试场景）。宿主接线：任务结束后生成会话总结（调用方自行异步化）。
	OnTaskFinished func(ctx context.Context, userTask, result string, err error, elapsed time.Duration)
	// Usage 会话级 token/cost 计量器（Claude Code /cost 对齐；非 nil 时
	// 每轮请求结束后累计真实/估算用量并推送 say usage 事件）。
	Usage *usage.Counter

	// ---- Codex 对齐补全 ③：turn diff 追踪（对齐 turn_diff_tracker.rs 简版）----
	// diffTracker 本轮任务内文件写入类工具（fs_write / fs_apply_diff）触达
	// 文件的内容基线；attempt_completion 时输出全部被改文件的 net diff
	// （新建/修改/删除统计 + 行级差异，零 git 依赖）。RunInMode 懒初始化，
	// 同引擎跨模式切换/子任务共享同一追踪器（整个任务生命周期统一结算）。
	diffTracker *turnDiffTracker

	// ---- Zoo-Code 对齐：文件上下文追踪（FileContextTracker 简版）----
	// fileTracker 记录 fs_read 读取文件时的 mtime；写入同一文件前对比磁盘
	// mtime，检测"基于过期内容写文件"（外部 shell/用户改动）并注入警告。
	// RunInMode 懒初始化，与 diffTracker 同生命周期。
	fileTracker *fileContextTracker

	// MCP 惰性加载提示词段（非空时注入 MCP TOOL CATALOG/LAZY LOADING 段）：
	// 未连接服务器的目录 + mcp_activate 激活指引（对齐 Claude Code deferred
	// tools / codex 动态工具发现的「目录透出，按需加载」）。
	MCPLazyHint string
	// SubtaskRunner 并行子任务执行钩子（P2.8 多 Agent 并行协作）。
	// 非 nil：一轮允许多个 new_task 并行 fan-out，每个子任务由通道层以独立
	// 运行态执行（独立会话/引擎/待办，对齐 Zoo-Code delegateParentAndOpenChild
	// 的子任务独立运行 + reopenParentFromDelegation 结果回填语义）。
	// nil：保持串行递归 runSubtask（对齐 Zoo-Code 出厂 mustBeLast 串行行为）。
	SubtaskRunner func(ctx context.Context, st tools.SubtaskResult, say SayFn, onAsk AskFn, depth int) (string, error)

	// ---- 中途消息注入（移植 Zoo-Code Task.userMessageContent）----
	// Incoming 每轮 API 请求前由引擎调用，取走（drain）任务执行期间用户
	// 续发的聊天消息，按序注入为 user 消息进入下一次请求——任务不打断、
	// 消息不丢失（对齐 Zoo-Code userMessageContent 在下一次 API 请求
	// 自然携带中途用户反馈的语义）。消息可携带图片附件（多模态）。
	// nil = 无中途消息（CLI/测试场景）。
	Incoming func() []llm.Message

	// ---- 对标增强 B1：任务检查点与回滚（对标 Cline Checkpoints 零 git 简版）----
	// CheckpointStore 非 nil 时：写类工具（fs_write / fs_apply_diff）成功后
	// 记录被改文件的修改前内容快照；任务收尾 say checkpoint 摘要。恢复经
	// checkpoint.Store.Restore（宿主面板/工具调用，不入引擎主循环）。
	CheckpointStore *checkpoint.Store

	// ---- 对标增强第二批（C1/C2/C3，对标 Aider / OpenCode）----
	// Verifier 编辑后验证执行器（C1 对标 Aider auto-lint）：宿主按 verify.json
	// 组装；非 nil 且有规则 → fs_write / fs_apply_diff 成功后跑检查，失败把反馈
	// 附加到工具结果回灌模型。nil = 关闭。
	Verifier *verify.Runner

	// Approval 工具审批存储（D2 升级 C2 矩阵：参数级规则 + 优先级；对标
	// Gemini CLI policy engine）。nil = 全部旧 AutoApprove 逻辑。红线（敏感
	// 写入/危险命令）在更早分支判定，不受本存储任何规则影响。
	// 宿主从 approvals.json 装配（approval.LoadStore，旧形态文件照读）。
	Approval *approval.Store

	// SpawnRunner 后台智能体执行钩子（codex-main 多智能体协作移植，对齐
	// codex-rs collab spawn_agent：派生后立即返回 agent_id，不阻塞当前轮次；
	// 结果稍后由 wait_agent 收割）。通道层以独立运行态执行（独立子会话/
	// 引擎/待办）。nil = 后台智能体不可用（spawn_agent 回填错误提示）。
	SpawnRunner func(ctx context.Context, st tools.SubtaskResult, say SayFn, onAsk AskFn, depth int) (string, error)

	// spawned 本任务派生的后台智能体（agent_id → 完成句柄，wait_agent 收割）。
	spawnMu    sync.Mutex
	spawned    map[string]*spawnHandle
	spawnOrder []string
	spawnSeq   int

	// Messages 本任务全部 ClineMessage（会话记录，供状态机与持久化）。
	Messages []message.ClineMessage

	// AIContextVisible 上下文面板可见性探测（P2.7-0b 热路径优化）：非 nil 且
	// 返回 true 时引擎才做全载荷 JSON 序列化并推 SayAIContext 快照帧；nil 或
	// false 时跳过（长会话每轮 O(载荷) 序列化不再白付）。nil = 兼容旧宿主，
	// 每轮照发（行为与既往一致）。
	AIContextVisible func() bool

	// ---- 长任务心跳（2026-10 复盘 R2「长任务每 3-4 轮一句话回报」内核化）----
	// ProgressEvery 每多少轮 LLM 请求推送一次 SayProgress 心跳（0 = 默认 8；
	// 负值 = 关闭）。心跳内容：轮数 + 当前 in_progress 待办项；仅推送前端
	// 展示不落历史（旁路帧），治「长时间无反馈 → 用户反复追问」。
	ProgressEvery int

	// msgMu 保护 Messages（并行 fan-out 期间 record 会并发追加）。
	msgMu sync.Mutex

	// autoAskCount 全AI自动决策模式下 ask_followup_question 连续提问计数
	//（每次 RunInMode 开始时重置；达到阈值强制回填「停止提问、立即推进」）。
	autoAskCount int

	// autoDecideHelper 全AI自动决策（AutoDecide）下的提问性 ask 统一应答器。
	// 派生模式：自动批准继续（对齐 unattendedAskBridge），提问性 ask 留痕后自主决策。

	// ---- 对标增强第三批 D3（对标 Claude Code skills allowed-tools）----
	// SkillGrants 技能当轮工具授权表：skill 工具执行成功后由宿主
	// OnAuthorize 写入；主循环每轮顶部、构造 LLM 请求前清空（对齐
	// "The grant clears when you send your next message"——本轮工具
	// 调用期间生效，模型下一次输出后失效）。红线不受影响（更早分支）。
	SkillGrants []string
	grantMu     sync.Mutex // 保护 SkillGrants（引擎主循环与工具执行并发）
}

// New 创建引擎。
func New(llmClient *llm.Client, reg *tools.Registry, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		LLM:                 llmClient,
		Registry:            reg,
		MaxRequests:         10_000_000,
		MistakeLimit:        10,
		ToolRepetitionLimit: 3,
		StreamRetryLimit:    3,
		Log:                 log,
	}
}

// SayFn 事件回调；AskFn 阻塞收集应答（返回应答类型与文本）。
type SayFn func(message.ClineMessage)
type AskFn func(message.ClineMessage) (message.AskResponse, string)

// taskContext 单次任务运行上下文（RunInMode 递归帧；对齐 Task 实例栈）。
type taskContext struct {
	mode      string // 当前模式 slug
	isSubtask bool   // 是否 new_task 委派的子任务
	depth     int    // 子任务深度（0 = 顶层）
}

// Run 执行任务直至 attempt_completion 或达到上限。返回最终结果文本。
func (e *Engine) Run(ctx context.Context, userTask string, say SayFn, onAsk AskFn) (string, error) {
	return e.RunInMode(ctx, modes.DefaultModeSlug, userTask, false, 0, say, onAsk)
}

// RunInMode 以指定模式执行任务（对齐 startSubtask：new_task 委派入口）。
func (e *Engine) RunInMode(ctx context.Context, mode, userTask string, isSubtask bool, depth int, say SayFn, onAsk AskFn) (result string, runErr error) {
	// 会话 hook：任务结束统一回调（仅顶层任务触发）。
	// defer 保证所有退出路径（完成/出错/上限终止/用户停止）都触发，不遗漏；
	// 回调在任务收尾后异步执行——不阻塞返回（宿主侧自行 goroutine 化）。
	if !isSubtask && e.OnTaskFinished != nil {
		start := time.Now()
		defer func() {
			e.OnTaskFinished(ctx, userTask, result, runErr, time.Since(start))
		}()
	}
	// 深度防护（对齐 new_task mustBeLast + 递归上限语义）。
	if e.MaxSubtaskDepth > 0 && depth > e.MaxSubtaskDepth {
		return "", fmt.Errorf("subtask depth limit (%d) exceeded", e.MaxSubtaskDepth)
	}
	// msgMu 保护：并行 fan-out 期间 record 会并发追加 Messages，重置同样必须持锁
	// （修复：裸赋值与 record 数据竞争，属未定义行为）。
	e.msgMu.Lock()
	e.Messages = nil
	e.msgMu.Unlock()

	// 全AI自动决策：每轮计数重置（防上一轮遗留计数影响本轮）。
	e.autoAskCount = 0
	// E3 降级标记重置：新任务/子任务恢复主模型探测机会（fallbackActive
	// 只在单次 RunInMode 生命周期内锁定，防主备反复震荡）。
	e.fallbackActive = false

	// codex 对齐补全 ③：turn diff 追踪器懒初始化（同引擎跨模式切换/子任务
	// 共享同一追踪器——整个任务生命周期内的文件变更统一结算 net diff）。
	if e.diffTracker == nil {
		e.diffTracker = newTurnDiffTracker()
	}
	// Zoo-Code 对齐：文件上下文追踪器懒初始化（stale read 检测，同生命周期）。
	if e.fileTracker == nil {
		e.fileTracker = newFileContextTracker()
	}

	// ---- 模式解析与工具过滤（对齐 getToolsForMode）----
	modeCfg := e.resolveMode(mode)
	allowed := e.allowedTools(modeCfg)
	modeSlug := modes.DefaultModeSlug
	if modeCfg != nil {
		modeSlug = modeCfg.Slug
	}

	var allModes []modes.ModeConfig
	if e.Modes != nil {
		allModes = e.Modes.All()
	}
	// 员工记忆沉淀（§11.4）并入 SESSION MEMORY 段：与 MemoryDoc（通道层
	// 赋值）分离存放，此处合并，防通道层覆盖时丢失员工记忆。
	memoryMD := e.MemoryDoc
	if e.EmpMemoryDoc != "" {
		if memoryMD != "" {
			memoryMD += "\n"
		}
		memoryMD += e.EmpMemoryDoc
	}
	sysPrompt := SystemPrompt(PromptOptions{
		ToolNames:                allowed,
		Mode:                     modeCfg,
		AllModes:                 allModes,
		SkillsXML:                e.skillsXML(),
		VectorKBDir:              e.VectorKBDir,
		TodosMD:                  e.todosMD(),
		MemoryMD:                 memoryMD,
		MemoryPath:               e.MemoryPath,
		WSDoc:                    e.WSDoc,
		WSPath:                   e.WSPath,
		EvolveSnippet:            e.EvolveSnippet,
		ManualPreview:            e.ManualPreview,
		IsSubtask:                isSubtask,
		ParallelDelegation:       e.SubtaskRunner != nil, // P2.8：fan-out 能力透出提示词（非子任务时注入并行委派指引）
		CWD:                      e.Workspace,
		Language:                 e.Language,
		GlobalCustomInstructions: e.GlobalCustomInstructions,
		EmployeeRole:             e.EmployeeRole,
		AutoDecide:               e.AutoDecide,
		PromptOverride:           e.PromptOverride,
		AutonomousDev:            e.AutonomousDev,
		MCPLazyHint:              e.MCPLazyHint,
	})

	// P3.1-B 会话恢复：ResumeDigest 非空 = 停止后继续——把恢复摘要并入首条
	// user 消息（"继续" + 上次进行到哪），模型无需回放全部历史即可续接。
	// mentions 解析（对齐 Zoo-Code parseMentions）：任务文本中的 @/path
	// @url @git-changes 替换为干净引用并把文件/目录/git 内容块并入首帧——
	// 聊天区回显（下方 feedback）仍用原始文本，无重复观感。
	fullTask := mentions.Expand(userTask, e.Workspace)
	if e.ResumeDigest != "" {
		fullTask = fullTask + "\n\n" + e.ResumeDigest
	}
	// 首个任务帧消费图片附件（多模态 user message）；递归/子任务帧不再重发。
	userMsg := llm.Message{Role: llm.RoleUser, Content: fullTask}
	if !isSubtask && len(e.UserImages) > 0 {
		userMsg.Images = e.UserImages
		e.UserImages = nil
	}
	apiMessages := []llm.Message{
		{Role: llm.RoleSystem, Content: sysPrompt},
		userMsg,
	}
	// 任务回显用原始任务文本：fullTask 含 ResumeDigest 恢复摘要（仅供模型
	// 续接上下文），把摘要一并回显会让用户消息在聊天区显得重复冗长；且
	// 桌面端发送时前端已本地渲染原始文本（消息去重修复点）。
	feedbackText := strings.TrimSpace(userTask)
	if feedbackText == "" {
		feedbackText = "[图片附件]"
	}
	feedback := message.NewSay(message.SayUserFeedback, feedbackText)
	if len(userMsg.Images) > 0 {
		feedback.Images = userMsg.Images
	}
	e.record(feedback, say)

	// P3/B5 上下文治理状态：截断隐藏标记（与 apiMessages 平行增长；
	// 非破坏性——完整消息保留，仅标记隐藏，对齐 Zoo-Code truncationParent）。
	var truncHidden []bool

	maxReq := e.MaxRequests
	if maxReq <= 0 {
		maxReq = 10_000_000
	}
	mistakeLimit := e.MistakeLimit
	if mistakeLimit <= 0 {
		mistakeLimit = 10
	}
	mistakes := 0

	// 流式请求瞬时错误自动重试计数（空闲看门狗超时/429/5xx → 自动续跑）。
	streamRetries := 0
	// 第三级兑底降级续跑计数（2026-10 用户需求）：自动重试 + 备用模型
	// 都耗尽后仍失败 → 注入一次降级指引由模型按任务性质处置；仅 1 次
	//（同 fallbackActive 一次性语义），二次失败仍弹 api_req_failed 卡片。
	degradeRetries := 0
	// 上下文触顶型 400 自动收缩重试计数（修复⑤：400 Unterminated string
	// 死循环根因治理——prompt 触顶 → completion 空间耗尽 → 截断输出）。
	overflowRetries := 0

	// 连续相同工具调用检测（对齐 Zoo-Code ToolRepetitionDetector：防死循环）。
	repDet := &toolRepetitionDetector{limit: e.ToolRepetitionLimit}

	var finalResult string

	// 主循环（对齐 initiateTaskLoop 的 while (!abort)）。
	// 请求预算耗尽不再硬失败（大任务如全项目改造，40 轮走不完会把全部
	// 进度连同错误一起丢弃）：对齐 Zoo-Code request-limit 语义——ask 用户
	// 是否续期，批准则续跑下一批（完整上下文保留，续接无缝）；补充文本
	// 亦作为 user 消息并入。onAsk==nil 的 CLI -y 场景自动续期。
	for req := 0; ; req++ {
		e.clearSkillGrants() // D3：上一轮技能授权随模型下一轮输出失效（当轮语义）
		if req > 0 && req%maxReq == 0 {
			if onAsk == nil {
				// 无人值守（CLI -y / onAsk==nil）：无法阻塞询问，自动续期但设
				// 绝对硬顶（4×maxReq）防失控烧预算。
				if req >= maxReq*4 {
					return finalResult, fmt.Errorf("max requests (%d) reached without completion", req)
				}
			} else if e.AutoDecide {
				// 全AI自动决策（用户需求 2026-12）：续期提问不阻塞等人——留痕后自动批准
				// 续跑（完整上下文保留）。仍保留 4×maxReq 硬顶防失控。
				if req >= maxReq*4 {
					return finalResult, fmt.Errorf("max requests (%d) reached (auto-decide hard cap)", req)
				}
				_, _ = e.autoDecideAsk(message.NewAsk(message.AskMistakeLimit,
					fmt.Sprintf("已连续 %d 轮请求未完成任务（上下文与进度均保留）。是否继续执行？", req)), say)
			} else {
				// 交互模式：对齐 Zoo-Code request-limit 语义——ask 用户是否续期
				// （桌面「错误上限」卡片 / 飞书 y 应答）；批准则带着完整上下文
				// 续跑下一批；messageResponse 文本并入为补充指令；拒绝则收尾返回。
				askMsg := message.NewAsk(message.AskMistakeLimit,
					fmt.Sprintf("已连续 %d 轮请求未完成任务（上下文与进度均保留）。是否继续执行？", req))
				resp, text := e.waitAsk(say, onAsk, askMsg)
				if resp == message.ResponseMessageResponse && text != "" {
					// 补充指令并入并回显（对齐 user_feedback say 语义）。
					e.record(message.NewSay(message.SayUserFeedback, text), say)
					apiMessages = append(apiMessages, llm.Message{Role: llm.RoleUser, Content: text})
				} else if resp != message.ResponseYes {
					return finalResult, fmt.Errorf("request budget reached (%d) and user declined to continue", req)
				}
			}
			mistakes = 0
		}
		if ctx.Err() != nil {
			return finalResult, ctx.Err()
		}

		// 长任务心跳（R2 内核化）：每 N 轮请求向用户推送一句话进度（仅前端
		// 展示不落历史/不进 LLM 上下文）。治「任务在跑但用户看不到进展 →
		// 反复追问完成了吗」——复盘样本中该模式占用户反复提交的多数。
		// 内容取当前 in_progress 待办项（模型自己定的计划即真实进展），
		// 无清单时仅报轮数。
		if e.ProgressEvery >= 0 {
			every := e.ProgressEvery
			if every == 0 {
				every = 8
			}
			if req > 0 && req%every == 0 && say != nil {
				step := ""
				if e.Todos != nil {
					for _, it := range e.Todos.Items() {
						if it.Status == "in_progress" {
							step = strings.TrimSpace(strings.SplitN(strings.TrimSpace(it.Content), "\n", 2)[0])
							if n := []rune(step); len(n) > 60 {
								step = string(n[:60]) + "…"
							}
							break
						}
					}
				}
				note := fmt.Sprintf("⏳ 进行中：第 %d 轮请求，未结束请耐心等待", req)
				if step != "" {
					note += " · 当前步骤：" + step
				}
				m := message.NewSay(message.SayProgress, note)
				m.Partial = true
				say(m) // Partial=true：不落会话历史，仅事件桥推前端（对齐 usage/ai_context）
			}
		}

		if mistakes >= mistakeLimit {
			askMsg := message.NewAsk(message.AskMistakeLimit,
				fmt.Sprintf("Consecutive mistake limit reached (%d).", mistakes))
			resp, text := message.AskResponse(""), ""
			if e.AutoDecide {
				// 全AI自动决策（用户需求 2026-12）：错误上限不阻塞等人——留痕后
				// 自动批准续跑，同步清零计数避免同轮内反复弹卡。
				_, _ = e.autoDecideAsk(askMsg, say)
				resp, text = message.ResponseYes, ""
			} else {
				resp, text = e.waitAsk(say, onAsk, askMsg)
			}
			if resp == message.ResponseMessageResponse && text != "" {
				apiMessages = append(apiMessages, llm.Message{Role: llm.RoleUser, Content: text})
			} else if resp != message.ResponseYes {
				return finalResult, fmt.Errorf("mistake limit reached and user declined to continue")
			}
			mistakes = 0
		}

		// 中途消息注入（移植 Zoo-Code userMessageContent）：任务执行期间
		// 用户续发的聊天消息不停止任务也不丢失——每轮 API 请求前 drain
		// 排队队列，按序注入为 user 消息（下一次请求自然携带，模型即刻
		// 获得用户反馈；对齐 Zoo-Code userMessageContent Ready 闸门语义）。
		// say 回显正常推送（suppressFeedback 仅抑制任务首帧回显）。
		if e.Incoming != nil {
			for _, inc := range e.Incoming() {
				txt := strings.TrimSpace(inc.Content)
				if txt == "" && len(inc.Images) == 0 {
					continue
				}
				if txt == "" {
					txt = "[图片附件]"
				}
				fb := message.NewSay(message.SayUserFeedback, txt)
				fb.Images = inc.Images
				e.record(fb, say)
				apiMessages = append(apiMessages, inc)
			}
		}

		// P3/B5 上下文治理（对齐 Zoo-Code manageContext 的截断兜底路径）：
		// 每轮请求前估算可见上下文 token（启发式，无 tokenizer 依赖），超过
		// 阈值（窗口×0.9 − 输出预留）即执行压缩。策略链逐级加强（413 修复）：
		//   1. LLM 摘要压缩主路径（codex compact.rs 语义）；
		//   2. 滑动窗口截断——迭代执行、比例逐轮加大，直至 ≤ 预算或无可隐藏；
		//   3. 强制裁剪——按预算比例裁剪残留超大消息，保证载荷必然进预算。
		// 策略模式收敛：三层策略在 condense.Governor 中组装与执行（2026-12
		// 优化），引擎层仅负责开关/预算计算/文案与回调接线。
		// 非破坏性：完整 apiMessages 保留在引擎侧，仅 hidden 标记决定发送
		// 范围；第 3 层会就地裁剪可见消息文本（1/2 层均无法达标才触发）。
		if e.ContextWindowTokens > 0 {
			reserved := e.ReservedOutputTokens
			if reserved <= 0 {
				reserved = 4096
			}
			triggerPct := e.CondenseTriggerPercent
			if triggerPct <= 0 || triggerPct > 100 {
				triggerPct = 90
			}
			budget := e.ContextWindowTokens*triggerPct/100 - reserved
			if budget < 1024 {
				budget = 1024
			}
			if est := condense.EstimateTokens(apiMessages); est > budget {
				// 规划板 2026-10：压缩行为可在设置中关闭——总开关关闭时跳过
				// 摘要与滑动窗口截断（保留硬裁剪兜底，保命防 413）。
				var steps []condense.Policy
				if e.CondenseEnabled {
					// 第 1 层：LLM 摘要压缩主路径优先（连续失败 2 次后本轮
					// 任务内跳过摘要，避免每轮白烧一次 LLM 请求）。
					if e.Summarizer != nil && e.condenseFails < 2 {
						steps = append(steps, &condense.SummarizePolicy{Summarizer: e.Summarizer, KeepRecent: e.CondenseKeepRecent})
					}
					// 第 2 层：滑动窗口截断（413 修复迭代语义）。
					steps = append(steps, &condense.TruncatePolicy{Frac: e.TruncationFrac})
				}
				// 第 3 层：硬裁剪兜底（不受总开关控制，保命路径）。
				steps = append(steps, &condense.HardTrimPolicy{})
				for _, st := range condense.NewGovernor(steps...).Run(apiMessages, truncHidden, budget) {
					switch st.Status {
					case condense.StepApplied:
						apiMessages, truncHidden = st.Messages, st.Hidden
						switch st.Kind {
						case condense.StepSummarize:
							e.condenseFails = 0
							note := fmt.Sprintf("上下文超过阈值（估算 %d > %d token），LLM 摘要压缩替换 %d 条历史消息，保留最近 %d 条原文",
								est, budget, st.Removed, st.Kept)
							if st.Kept == 0 {
								note = fmt.Sprintf("上下文超过阈值（估算 %d > %d token），LLM 摘要压缩替换 %d 条历史消息",
									est, budget, st.Removed)
							}
							e.record(message.NewSay(message.SayContextCondensed, note), say)
							if e.OnCondense != nil {
								e.OnCondense(st.Removed, st.Kept, st.Summary)
							}
						case condense.StepTruncate:
							e.record(message.NewSay(message.SayContextTruncated,
								fmt.Sprintf("上下文超过阈值（估算 %d > %d token），滑动窗口隐藏 %d 条消息（第 %d 轮压缩）",
									est, budget, st.Removed, st.Passes)), say)
						case condense.StepHardTrim:
							e.record(message.NewSay(message.SayContextTruncated,
								fmt.Sprintf("上下文压缩后仍超过阈值（预算 %d token），已强制裁剪超大消息内容", budget)), say)
						}
					case condense.StepFailed:
						// 摘要失败：计数后由后续策略兜底（原 else 分支语义）。
						if st.Kind == condense.StepSummarize {
							e.condenseFails++
							e.logDebug("condense summarize failed, will fall back to truncation")
						}
					}
				}
			}
		}

		// 可见视图：过滤截断隐藏消息后的实际 API 载荷（无截断时零开销原样返回）。
		visibleAPI := condense.VisibleView(apiMessages, truncHidden)
		// 纵深防御：兜底修复历史消息中可能存在的畸形 tool arguments
		//（非法 JSON 会被严格后端重新解析并返回 400 "Unterminated string"）。
		sanitizeToolCallArgs(visibleAPI)

		// api_req_started（cost 未回填表示进行中——流式检测锚点）。
		e.record(message.NewSay(message.SayAPIReqStarted, `{"apiProtocol":"openai"}`), say)

		// P2.7-0b：AI 交互上下文快照——仅推送前端查看（say 桥不落历史/会话）。
		// 对话膨胀可视化：用户可随时核对模型实际收到的完整上下文。
		// 热路径惰性化：仅当用户已打开上下文面板（AIContextVisible 由宿主按需置位）
		// 才做全载荷 JSON 序列化——长会话每轮 O(载荷) 的序列化不再白付（面板关闭
		// 时 store.aiContext 置空，无需停更；打开时下一轮自动恢复）。
		if say != nil && e.AIContextVisible != nil && e.AIContextVisible() {
			if b, err := json.Marshal(visibleAPI); err == nil {
				say(message.NewSay(message.SayAIContext, string(b)))
			}
		}

		// 一次 LLM 流式请求（工具定义按模式过滤）。
		var textBuf strings.Builder
		var reasonBuf strings.Builder // P2.9 思考内容累计（对齐 Zoo-Code reasoningMessage）
		var toolCalls []llm.ToolCall
		var streamUsage *llm.Usage // include_usage 尾包的真实 token 用量
		var splitter thinkSplitter // <think>…</think> 流式分割（跨 chunk 安全）

		streamErr := e.LLM.StreamChat(ctx, llm.ChatRequest{
			Messages:    visibleAPI,
			Tools:       toolDefs(e.Registry, allowed),
			Temperature: e.EmpTemperature,
			TopP:        e.EmpTopP,
			MaxTokens:   e.EmpMaxTokens,
		}, func(d llm.StreamDelta) error {
			switch {
			case d.Done:
				if d.Usage != nil {
					streamUsage = d.Usage
				}
				return nil
			case len(d.ToolCalls) > 0:
				toolCalls = d.ToolCalls
				return nil
			case d.Reasoning != "":
				// 思考内容：累计全文以 partial say 推送（对齐 Zoo-Code
				// reasoning 事件 partial 语义），通道层覆盖渲染为可折叠卡片。
				reasonBuf.WriteString(d.Reasoning)
				if say != nil {
					m := message.NewSay(message.SayReasoning, reasonBuf.String())
					m.Partial = true
					say(m)
				}
				return nil
			default:
				// 正文增量可能内嵌 <think>…</think> 思考块（vLLM 推理后端
				// 常见输出）：思考内容路由到卡片，正文照常显示——否则思考
				// 会以普通文字重复打印在聊天窗口。
				textPart, reasonPart := splitter.feed(d.Content)
				if reasonPart != "" {
					reasonBuf.WriteString(reasonPart)
					if say != nil {
						m := message.NewSay(message.SayReasoning, reasonBuf.String())
						m.Partial = true
						say(m)
					}
				}
				if textPart != "" {
					textBuf.WriteString(textPart)
					if say != nil {
						m := message.NewSay(message.SayText, textBuf.String())
						m.Partial = true
						say(m)
					}
				}
				return nil
			}
		})

		if streamErr != nil {
			// 取消导致的流中断直接退出（点停止 = ctx canceled）：不弹
			// API 失败审批卡（用户已明确要求停止，多一层 ask 徒增噪音，
			// 且审批卡还可能盖住"已停止"的观感——停止无响应修复点之一）。
			if ctx.Err() != nil {
				return finalResult, ctx.Err()
			}
			// 上下文触顶型 400 自动恢复：prompt 超过后端窗口上限时
			// completion 空间被挤没 → 输出截断 → 残缺 tool arguments →
			// 严格后端重新解析 JSON 失败返回 400。这类 400 值得自动收缩
			// 上下文后重试（而非弹卡死等），最多收缩 2 次。
			var apiErr *llm.APIError
			if errors.As(streamErr, &apiErr) && apiErr.Status == http.StatusBadRequest &&
				overflowRetries < 2 && e.ContextWindowTokens > 0 {
				overflowRetries++
				// 主动把触发阈值下调 25%（等价于强制走压缩路径），
				// 回退计数清零允许重新走摘要压缩。
				e.CondenseTriggerPercent = e.CondenseTriggerPercent * 3 / 4
				e.condenseFails = 0
				e.record(message.NewSay(message.SayContextTruncated,
					fmt.Sprintf("收到 400（疑似上下文超限），已下调压缩阈值并重试（第 %d/2 次）", overflowRetries)), say)
				req-- // 不消耗请求预算
				continue
			}
			// 瞬时错误自动重试（无限轮处理修复）：流空闲看门狗超时/429/5xx/
			// 连接中断按退避自动重试，未完成任务不因无人值守而假死停止；
			// 耗尽自动重试后才升级为 api_req_failed 卡片。确定性错误
			// （401/403/404/400）直接进卡片，不烧重试轮次。
			// 对标增强 E3（第四批，对标 Plandex errorFallback）：主模型瞬时
			// 错误重试耗尽且配置了备用模型时，自动降级续跑——无人值守场景
			// 不再因主模型抖动弹卡假死。req-- 不耗预算；SayFallback 告知切换；
			// fallbackActive 本任务内一次性锁定（防主备反复震荡）。
			if e.FallbackLLM != nil && !e.fallbackActive && isTransientStreamError(streamErr) {
				e.fallbackActive = true
				e.LLM = e.FallbackLLM
				e.record(message.NewSay(message.SayFallback, fmt.Sprintf(
					"主模型连续失败（%v），已切换备用模型 %s 继续", streamErr, e.FallbackLLM.Model)), say)
				req--
				continue
			}
			if retryLimit := e.streamRetryLimit(); isTransientStreamError(streamErr) && streamRetries < retryLimit {
				streamRetries++
				wait := time.Duration(streamRetries) * 3 * time.Second
				if wait > 30*time.Second {
					wait = 30 * time.Second
				}
				e.record(message.NewSay(message.SayAPIReqRetried, fmt.Sprintf(
					"%v（%s 后自动重试，第 %d/%d 次）", streamErr, wait, streamRetries, retryLimit)), say)
				select {
				case <-ctx.Done():
					return finalResult, ctx.Err()
				case <-time.After(wait):
				}
				req-- // 自动重试不消耗请求预算
				continue
			}
			// 第三级兑底（降级续跑，2026-10 用户需求）：自动重试与备用模型都
			// 失效后不弹卡停摆——注入一次性降级指引（user 消息），由模型按
			// 任务性质自处置：验证/测试/冒烟类任务说明卡点后跳过收口；开发类
			// 任务换实现思路绕开故障链路。降级仅 1 次，二次失败仍弹卡片交人工
			//（有人值守路径同源受益：先自动处置，真不行才打扰用户）。
			if degradeRetries < 1 && isTransientStreamError(streamErr) {
				degradeRetries++
				chain := fmt.Sprintf("自动重试 %d 次未恢复", e.streamRetryLimit())
				if e.FallbackLLM != nil {
					chain += "、备用模型亦失败"
				}
				e.record(message.NewSay(message.SayError, streamErr.Error()), say)
				e.record(message.NewSay(message.SayFallback,
					"LLM 链路故障（"+streamErr.Error()+"），"+chain+"；已注入降级指引续跑：测试/验证类任务将跳过收口，开发类任务将换思路（再次失败转人工）"), say)
				apiMessages = append(apiMessages, llm.Message{Role: llm.RoleUser, Content: degradePrompt(streamErr, e.streamRetryLimit())})
				req-- // 降级续跑不消耗请求预算
				continue
			}
			e.record(message.NewSay(message.SayError, streamErr.Error()), say)
			askMsg := message.NewAsk(message.AskAPIReqFailed, streamErr.Error())
			if e.AutoDecide {
				// 全AI自动决策（用户需求 2026-12）：流错误卡片不阻塞等人——留痕后
				// 自动批准重试（等价于卡片上点「重试」）；重试计数同步清零重新计。
				_, _ = e.autoDecideAsk(askMsg, say)
				streamRetries = 0
				req-- // 重试不消耗请求预算
				continue
			}
			resp, text := e.waitAsk(say, onAsk, askMsg)
			switch resp {
			case message.ResponseYes:
				streamRetries = 0
				req-- // 重试不消耗请求预算
				continue
			case message.ResponseMessageResponse:
				if text != "" {
					apiMessages = append(apiMessages, llm.Message{Role: llm.RoleUser, Content: text})
					continue
				}
				return finalResult, streamErr
			default:
				return finalResult, streamErr
			}
		}
		streamRetries = 0 // 流成功：自动重试计数清零

		// 真实用量回填（对齐 Zoo-Code api_req_finished 的 token/cost 载荷）：
		// 服务端 include_usage 尾包 → 会话记录，前端展示本次请求真实 token 用量；
		// 未回传时保持空文本（估算兜底路径，行为不变）。
		finText := ""
		if streamUsage != nil {
			if b, err := json.Marshal(map[string]int{
				"prompt_tokens":     streamUsage.PromptTokens,
				"completion_tokens": streamUsage.CompletionTokens,
				"total_tokens":      streamUsage.TotalTokens,
			}); err == nil {
				finText = string(b)
			}
		}
		e.record(message.NewSay(message.SayAPIReqFinished, finText), say)

		// Claude Code /cost 对齐：会话级 token/cost 累计 + say usage 事件推送。
		// 真实尾包优先，缺失时按本轮可见载荷/输出文本估算兑底（估算量独立
		// 记账，前端可区分真实/估算）；usage 事件仅推送前端展示，不落会话历史。
		if e.Usage != nil {
			// 估算兑底输入：惰性拼接——有真实尾包时跳过全载荷拼接（长会话
			// 每轮 O(载荷) 的 strings.Builder 开销不再白付；估算兜底时才拼）。
			var promptText string
			if streamUsage == nil {
				var sb strings.Builder
				for _, m := range visibleAPI {
					sb.WriteString(m.Content)
				}
				promptText = sb.String()
			}
			e.Usage.Record(streamUsage, promptText, textBuf.String())
			if say != nil {
				if b, err := json.Marshal(e.Usage.Snapshot()); err == nil {
					say(message.NewSay(message.SayUsage, string(b)))
				}
			}
			// 对标增强 E1（第四批，对标 SWE-agent cost_limit / LimitsExceeded）：
			// 每轮请求后检查成本/调用护栏，超限时优雅终止——已完成的进度
			// （finalResult）保留返回，OnTaskFinished 收尾链路照常触发，
			// 任务可经 ResumeDigest 续接。
			if kind, limit, over := e.Usage.OverLimit(); over {
				e.record(message.NewSay(message.SayCostLimit, fmt.Sprintf(
					"成本护栏触发：%s 用量已达上限（%.2f），任务在此收尾（已完成进度保留，可继续会话续接）", kind, limit)), say)
				return finalResult, fmt.Errorf("usage limit reached (%s limit %.2f)", kind, limit)
			}
		}

		// 流结束：分割器残余（被切断的标签尾巴/未闭合思考块）按状态归位后定型。
		textRest, reasonRest := splitter.flush()
		if reasonRest != "" {
			reasonBuf.WriteString(reasonRest)
		}
		if textRest != "" {
			textBuf.WriteString(textRest)
		}

		// 定型思考消息：partial=false 落会话历史（回放渲染为折叠卡片）。
		if reasonBuf.Len() > 0 {
			e.record(message.NewSay(message.SayReasoning, reasonBuf.String()), say)
		}

		// 定型 assistant 文本消息。
		content := textBuf.String()
		assistantMsg := llm.Message{Role: llm.RoleAssistant, Content: content}
		if content != "" {
			e.record(message.NewSay(message.SayText, content), say)
		}

		// 无工具调用 → mistake 纠偏（对齐 formatResponse.noToolsUsed 循环）。
		if len(toolCalls) == 0 {
			mistakes++
			apiMessages = append(apiMessages, assistantMsg, llm.Message{
				Role: llm.RoleUser,
				Content: "You did not use any tool in your last response. " +
					"If the task is complete, call attempt_completion; otherwise use a tool to make progress.",
			})
			continue
		}
		mistakes = 0

		// 执行工具调用。残缺 arguments（流中断 / finish_reason=length 截断 /
		// 模型畸形输出产生）绝不能进入历史：严格后端（vLLM/Pydantic）会重新
		// 解析 tool_calls[].function.arguments 内嵌 JSON 字符串，非法即 400，
		// 且该消息每轮请求都携带 → 任务永久卡死。此处就地替换为合法占位
		// JSON 并标记（badArgs），执行阶段回填错误 tool_result 引导模型重试。
		badArgs := map[int]bool{}
		for i, tc := range toolCalls {
			if !json.Valid([]byte(tc.Arguments)) {
				badArgs[i] = true
				tc.Arguments = fmt.Sprintf(
					`{"malformed_arguments":true,"original_chars":%d}`, len(tc.Arguments))
			}
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, tc)
		}
		apiMessages = append(apiMessages, assistantMsg)

		// new_task 编排（对齐 Zoo-Code mustBeLastTool + P2.8 多 Agent 并行扩展）：
		//   - 串行模式（SubtaskRunner == nil）：new_task 必须独立成轮——首个为
		//     new_task 时截断本轮其余调用；非首个时仅拒绝该调用（普通工具照常执行）。
		//   - 并行模式（SubtaskRunner != nil）：一轮可携带多个 new_task 并行
		//     fan-out，不做截断（工具循环收集后统一并发执行）。
		// spawn_agent / wait_agent（codex collab 对齐）：无论并行与否都必须
		// 独立成轮（对齐 codex collab 工具 MUST be called alone 语义）。
		skip := map[int]bool{}
		if len(toolCalls) > 0 && (toolCalls[0].Name == "spawn_agent" || toolCalls[0].Name == "wait_agent") {
			for i := 1; i < len(toolCalls); i++ {
				skip[i] = true
				apiMessages = append(apiMessages, llm.Message{
					Role: llm.RoleTool, ToolCallID: toolCalls[i].ID,
					Content: "Error: " + toolCalls[0].Name + " must be called alone in a single message. " +
						"No other tools may be used alongside it. This call was not executed.",
				})
			}
		} else {
			for i, tc := range toolCalls {
				if tc.Name == "spawn_agent" || tc.Name == "wait_agent" {
					skip[i] = true
					apiMessages = append(apiMessages, llm.Message{
						Role: llm.RoleTool, ToolCallID: tc.ID,
						Content: "Error: " + tc.Name + " must be called alone in a single message. " +
							"No other tools may be used alongside it. Retry with it as the only tool call.",
					})
				}
			}
		}
		if e.SubtaskRunner == nil {
			if len(toolCalls) > 0 && toolCalls[0].Name == "new_task" {
				for i := 1; i < len(toolCalls); i++ {
					skip[i] = true
					apiMessages = append(apiMessages, llm.Message{
						Role: llm.RoleTool, ToolCallID: toolCalls[i].ID,
						Content: "Error: new_task must be called alone in a single message. " +
							"No other tools may be used alongside it. This call was not executed.",
					})
				}
			} else {
				for i, tc := range toolCalls {
					if tc.Name == "new_task" {
						skip[i] = true
						apiMessages = append(apiMessages, llm.Message{
							Role: llm.RoleTool, ToolCallID: tc.ID,
							Content: "Error: new_task must be called alone in a single message. " +
								"No other tools may be used alongside it. Retry with new_task as the only tool call.",
						})
					}
				}
			}
		}

		completed := false
		var subs []subAssign // P2.8：本轮收集的并行子任务委派
		for i, tc := range toolCalls {
			if skip[i] {
				continue
			}
			// 取消检查点：停止后不再继续执行剩余工具调用（此前仅主循环
			// 顶部检查，长工具序列中点停止仍会把整轮调用跑完）。
			if ctx.Err() != nil {
				return finalResult, ctx.Err()
			}
			// 残缺参数隔离：流式聚合出的 arguments 非法（已在上方替换为
			// 合法占位），不执行工具，回填错误 tool_result 引导模型重新调用。
			if badArgs[i] {
				mistakes++
				e.record(message.NewSay(message.SayError,
					"Malformed tool arguments (invalid JSON, likely truncated): "+tc.Name), say)
				apiMessages = append(apiMessages, llm.Message{
					Role: llm.RoleTool, ToolCallID: tc.ID,
					Content: "Error: the tool call arguments were malformed JSON " +
						"(likely truncated by a stream interruption) and the call was NOT executed. " +
						"Re-issue the tool call with complete, valid JSON arguments.",
				})
				continue
			}
			// 连续完全相同的工具调用检测（对齐 ToolRepetitionDetector）：
			// 达到上限即拒绝执行并回填纠偏提示，防同参调用死循环烧请求预算。
			// shell_output 特例：日志轮询是合法场景，拦截提示引导用 wait_ms
			// 等待新输出，而非笼统「换方法」（2026-10-01 会话复盘：空轮询被拦
			// 后模型常继续同参重试，没有可执行的纠偏方向）。
			if !repDet.allow(tc.Name, tc.Arguments) {
				mistakes++
				retry := "Stop repeating it — inspect the current state and take " +
					"a different approach."
				switch tc.Name {
				case "shell_output":
					retry = "Do not poll with the same offset. Use the wait_ms parameter " +
						"(e.g. wait_ms=10000) to block until new output arrives, and pass " +
						"the next_offset from the previous response."
				case "update_todo_list":
					// 2026-10-04 会话复盘：同一清单被同参重发 11 次（模型在等待外部
					// 结果时习惯性重发清单）。引导：清单未变化时不要重发；仅状态变化
					// 时更新对应条目。
					retry = "The todo list is already up to date with this exact content. " +
						"Do NOT re-send it. Only call update_todo_list again when an item's " +
						"content or status actually changes; otherwise proceed with the next " +
						"in_progress item or call a tool to make progress."
				case "fs_read":
					// 复盘样本：同文件同参数重读 9 次（前次结果已在上下文中）。
					retry = "You already read this exact file range and its content is in the " +
						"conversation above. Re-reading returns the same bytes. If the file may " +
						"have changed on disk, read with different offset/limit, or move on " +
						"and act on the content you already have."
				case "fs_list":
					retry = "You already listed this exact directory in the conversation above. " +
						"The listing will not change between calls. Use fs_read on a specific " +
						"file from that listing to move forward."
				case "fs_apply_diff":
					// 2026-10-05 会话复盘：SEARCH 失败后同参数原样重发居首位
					// （单会话最多 40+ 条，同一错误 internal/remote/protocol.go
					// 出现同参数重发）。SEARCH 未命中意味着文件内容与模型记忆
					// 不一致，原样重发必然再次失败，必须重读。
					retry = "The same SEARCH block already failed to match this file. " +
						"Retrying the identical block will fail identically - the file " +
						"content differs from what you remember (changed by you earlier " +
						"or by a concurrent agent/process). fs_read the target region " +
						"(start from the line given in the error hint), then rebuild the " +
						"edit from the fresh content, or use fs_write for a full rewrite."
				case "fs_search":
					retry = "You already ran this exact search and the result is in the " +
						"conversation above. The results will not change between calls. " +
						"Act on the existing results, or narrow the query with a different " +
						"path/file_pattern/regex if the previous one was too broad."
				}
				// 专属纠偏指引同步进 say（2026-10-05 复盘：SayError 只含短句时
				// 会话留痕与自学习无法区分「被拦后是否给了可执行方向」；模型
				// 看到的完整指引在下方 tool_result 中）。
				e.record(message.NewSay(message.SayError,
					"Repeated identical tool call blocked: "+tc.Name+" — "+retry), say)
				apiMessages = append(apiMessages, llm.Message{
					Role: llm.RoleTool, ToolCallID: tc.ID,
					Content: "Error: the exact same tool call has been made too many times " +
						"in a row. " + retry,
				})
				continue
			}
			// say tool：展示调用。
			toolMsg := message.NewSay(message.SayTool, marshalToolInfo(tc.Name, tc.Arguments))
			e.record(toolMsg, say)

			// B1 检查点：写类工具执行**前**读取旧内容快照（成功后才落盘记录，
			// 对标 Cline Checkpoints；失败调用不记录避免污染检查点）。解析失败
			// 静默跳过：检查点永不阻塞主流程。
			var cpPending *cpPending
			if e.CheckpointStore != nil && (tc.Name == "fs_write" || tc.Name == "fs_apply_diff") {
				cpPending = captureCheckpointBefore(e.Workspace, tc.Name, tc.Arguments)
			}

			completion, subtask, switchReq, spawnReq, waitReq, toolResult, toolErr := e.execTool(ctx, tc, say, onAsk, modeSlug, depth, allowed)

			// attempt_completion → 任务结束。
			if completion != nil {
				finalResult = completion.Result
				completed = true
				break
			}
			// new_task 子任务委派（对齐 PendingTaskAction.create_subtask → startSubtask）。
			if subtask != nil {
				// codex spawn_agent 语义（background=true）：异步派发立即返回
				// agent_id，不阻塞本轮；结果稍后经 wait_agent 收割。
				if subtask.Background && e.SpawnRunner != nil {
					st := *subtask
					if strings.TrimSpace(st.Mode) == "" {
						st.Mode = modeSlug
					}
					agentID := e.startSpawn(ctx, st, say, onAsk, depth)
					mistakes = 0
					apiMessages = append(apiMessages, llm.Message{
						Role: llm.RoleTool, ToolCallID: tc.ID,
						Content: "Background agent spawned: " + agentID + " (mode " + st.Mode + "). " +
							"Continue your own work; call wait_agent later to collect its result.",
					})
					continue
				}
				// P2.8 并行协作：收集委派，工具循环结束后统一并行 fan-out
				// （每个子任务经 SubtaskRunner 在独立子会话运行态中执行）。
				if e.SubtaskRunner != nil {
					subs = append(subs, subAssign{callID: tc.ID, st: *subtask})
					continue
				}
				subResult, err := e.runSubtask(ctx, *subtask, say, onAsk, depth)
				if err != nil {
					mistakes++
					apiMessages = append(apiMessages, llm.Message{
						Role: llm.RoleTool, ToolCallID: tc.ID,
						Content: "Error: subtask failed: " + err.Error(),
					})
					continue
				}
				// 子任务结果回填父任务（对齐 resumeAfterDelegation）。
				apiMessages = append(apiMessages, llm.Message{
					Role: llm.RoleTool, ToolCallID: tc.ID,
					Content: "Subtask completed. Result from subtask (" + subtask.Mode + " mode):\n\n" + subResult,
				})
				mistakes = 0
				continue
			}
			// spawn_agent 后台智能体派生（codex collab spawn_agent 对齐）：
			// 异步派生立即返回 agent_id，当前任务继续推进不被阻塞。
			if spawnReq != nil {
				if e.SpawnRunner == nil {
					mistakes++
					apiMessages = append(apiMessages, llm.Message{
						Role: llm.RoleTool, ToolCallID: tc.ID,
						Content: "Error: background agents are not available in this context " +
							"(no SpawnRunner configured). Delegate with a plain new_task instead.",
					})
					continue
				}
				st := tools.SubtaskResult{Mode: spawnReq.Mode, Message: spawnReq.Message, Todos: spawnReq.Todos}
				if strings.TrimSpace(st.Mode) == "" {
					st.Mode = modeSlug // 省略 mode = 继承当前模式
				}
				agentID := e.startSpawn(ctx, st, say, onAsk, depth)
				mistakes = 0
				apiMessages = append(apiMessages, llm.Message{
					Role: llm.RoleTool, ToolCallID: tc.ID,
					Content: "Background agent spawned: " + agentID + " (mode " + st.Mode + "). " +
						"Continue your own work; call wait_agent later to collect its result.",
				})
				continue
			}
			// wait_agent 收割后台智能体结果（codex collab wait_agent 语义：
			// 阻塞至目标智能体全部完成，聚合结果按 agent_id 回填）。
			if waitReq != nil {
				out := e.waitSpawns(ctx, waitReq.IDs)
				mistakes = 0
				apiMessages = append(apiMessages, llm.Message{
					Role: llm.RoleTool, ToolCallID: tc.ID,
					Content: e.clampToolResult(out),
				})
				continue
			}
			// switch_mode 模式切换（对齐 SwitchModeTool：ask 用户批准后切换）。
			if switchReq != nil {
				cfg := e.resolveMode(switchReq.ModeSlug)
				newSlug := switchReq.ModeSlug
				if cfg != nil {
					newSlug = cfg.Slug
				}
				askMsg := message.NewAsk(message.AskTool,
					"Switch to mode "+newSlug+"? Reason: "+switchReq.Reason)
				if e.AutoDecide {
					// 全AI自动决策（用户需求 2026-12）：模式切换提问不阻塞等人——留痕后自动批准
					//（对齐 AutoDecide 字段注释：提问性的都由 AI 自主决策）。
					_, _ = e.autoDecideAsk(message.NewAsk(message.AskMistakeLimit, askMsg.Text), say)
				} else if resp, _ := e.waitAsk(say, onAsk, askMsg); resp != message.ResponseYes {
					apiMessages = append(apiMessages, llm.Message{
						Role: llm.RoleTool, ToolCallID: tc.ID,
						Content: "Mode switch rejected by user. Continue in current mode.",
					})
					continue
				}
				e.record(message.NewSay(message.SayModeSwitched, newSlug), say)
				// 模式切换 = 以新模式重启本轮任务（上下文延续）。
				return e.RunInMode(ctx, newSlug, userTask, isSubtask, depth, say, onAsk)
			}
			// 工具失败 → 回填错误，计入 mistake；同时补记 SayError（复盘素材修复：
			// selflearn/hourlyReview 只读 SayError 统计错误——旧实现工具失败只进
			// API 消息不落 say，自学习/每小时复盘对工具层失败全盲）。
			if toolErr != nil {
				mistakes++
				errText := toolErr.Error()
				e.record(message.NewSay(message.SayError, tc.Name+": "+errText), say)
				apiMessages = append(apiMessages, llm.Message{
					Role: llm.RoleTool, ToolCallID: tc.ID,
					Content: "Error: " + errText,
				})
				continue
			}
			// B1 检查点：写类工具成功后落盘执行前捕获的旧内容快照
			//（captureCheckpointBefore 已读取执行前内容；失败调用不记录）。
			if cpPending != nil {
				_, _ = e.CheckpointStore.Record(tc.Name, cpPending.path, cpPending.old, cpPending.isNew)
				// C1 编辑后验证（对标 Aider auto-lint）：写类工具成功后按 verify.json
				// 规则跑检查，失败把错误附加到工具结果回灌模型立即修复，并推
				// verify 事件供前端展示。检查失败不改变工具成功状态（增强非拦截）。
				if e.Verifier != nil {
					if fb := e.Verifier.AfterEdit(cpPending.path); fb != "" {
						toolResult += "\n" + fb
						e.record(message.NewSay(message.SayVerify, fb), say)
					}
				}
			}
			mistakes = 0
			apiMessages = append(apiMessages, llm.Message{
				Role: llm.RoleTool, ToolCallID: tc.ID,
				Content: e.clampToolResult(toolResult),
			})
		}

		// P2.8 并行 fan-out：并发执行本轮收集的 new_task 委派，结果按委派顺序回填
		// （对齐 Zoo-Code resumeAfterDelegation 的 tool_result 文本）。
		if !completed && len(subs) > 0 {
			for _, o := range e.runSubtasksParallel(ctx, subs, say, onAsk, depth) {
				if o.err != nil {
					mistakes++
					apiMessages = append(apiMessages, llm.Message{
						Role: llm.RoleTool, ToolCallID: o.callID,
						Content: "Error: subtask failed: " + o.err.Error(),
					})
					continue
				}
				mistakes = 0
				apiMessages = append(apiMessages, llm.Message{
					Role: llm.RoleTool, ToolCallID: o.callID,
					Content: e.clampToolResult("Subtask completed. Result from subtask (" + o.mode + " mode):\n\n" + o.result),
				})
			}
		}

		if completed {
			// codex 对齐补全 ③：任务完成时输出本轮文件变更 net diff
			// （say 事件推送前端 + 追加到完成结果供会话记录；无变更零开销）。
			if d := e.diffTracker.summary(e.Workspace); d != "" {
				e.record(message.NewSay(message.SayTurnDiff, d), say)
				finalResult = finalResult + "\n\n" + d
			}
			// B1 检查点摘要：任务收尾报告本任务记录的快照（前端可展示可回滚清单）。
			if csum := e.CheckpointStore.Summary(); csum != "" {
				e.record(message.NewSay(message.SayCheckpoint, csum), say)
			}
			// Claude Code Hooks 对齐：Stop 生命周期钩子（任务收尾审计/通知；
			// 失败忽略不致命）。无论完成/上限终止都触发（对齐 Stop hook 语义）。
			if e.UserHooks != nil {
				e.UserHooks.Stop(ctx)
			}
			return finalResult, nil
		}
	}
}

// cpPending 写类工具执行前捕获的检查点素材（执行成功后才落盘 Record）。
type cpPending struct {
	path  string
	old   string
	isNew bool
}

// captureCheckpointBefore 工具执行前读取目标文件旧内容（B1；失败静默返回 nil）。
func captureCheckpointBefore(workspace, tool, args string) *cpPending {
	p := writeTargetPath(tool, args)
	if p == "" {
		return nil
	}
	abs := absWorkspacePath(workspace, p)
	if abs == "" {
		return nil
	}
	old, err := os.ReadFile(abs)
	if err == nil {
		return &cpPending{path: p, old: string(old), isNew: false}
	}
	if os.IsNotExist(err) {
		return &cpPending{path: p, old: "", isNew: true}
	}
	return nil // 读失败（权限等）：跳过本条快照
}

// absWorkspacePath 把工具参数中的相对路径解析为工作区内绝对路径
// （与 statWorkspace 同样的边界校验：越界返回空串）。
func absWorkspacePath(workspace, rel string) string {
	rel = normPath(rel)
	if rel == "" || rel == "." {
		return ""
	}
	ws, err := filepath.Abs(workspace)
	if err != nil {
		return ""
	}
	abs := filepath.Clean(filepath.Join(ws, filepath.FromSlash(rel)))
	if abs != ws && !strings.HasPrefix(abs, ws+string(os.PathSeparator)) {
		return ""
	}
	return abs
}

// runSubtask 执行子任务（对齐 Task.startSubtask + resumeAfterDelegation）。
// 子任务独立消息流；say 事件以 new_task_started/finished 包裹。
// initialTodos 语义（对齐 startSubtask todos 参数）：委派时把 LLM 给出的
// 初始清单写入 TodoStore，子任务提示词即携带计划；子任务结束后恢复父任务清单。
func (e *Engine) runSubtask(ctx context.Context, st tools.SubtaskResult, say SayFn, onAsk AskFn, depth int) (string, error) {
	if e.MaxSubtaskDepth > 0 && depth >= e.MaxSubtaskDepth {
		return "", fmt.Errorf("subtask depth limit (%d) reached", e.MaxSubtaskDepth)
	}
	// 待办切换：保存父清单 → 写入子清单（P2.7-8 规划能力：委派即规划）。
	var parentTodos []tools.TodoItem
	if e.Todos != nil {
		parentTodos = e.Todos.Items()
		e.Todos.Set(st.Todos)
	}
		e.record(message.NewSay(message.SaySubtaskStarted,
			"Delegating subtask to mode "+st.Mode+": "+st.Message), say)
		result, err := e.RunInMode(ctx, st.Mode, st.Message, true, depth+1, say, onAsk)
		// 恢复父任务清单（子任务待办隔离，互不污染）。
		if e.Todos != nil {
			e.Todos.Set(parentTodos)
		}
		if err != nil {
			return "", err
		}
		// G1 工件核验：串行委派与并行路径（runSubtasksParallel）同语义——
		// 声明了 expect 的委派收尾先查产物存在性，流畅回复不是证据。
		if missing := checkExpectArtifacts(e.Workspace, st.Expect); len(missing) > 0 {
			err := fmt.Errorf("subtask return contract unmet — expected artifacts missing on disk: %s", strings.Join(missing, ", "))
			e.record(message.NewSay(message.SayError,
				"subtask ("+st.Mode+") failed: return contract unmet (missing artifacts: "+strings.Join(missing, ", ")+")"), say)
			return "", err
		}
		e.record(message.NewSay(message.SaySubtaskFinished, result), say)
		return result, nil
	}

// subAssign 一轮请求中收集的并行子任务委派（callID 用于 tool_result 回填定位）。
type subAssign struct {
	callID string
	st     tools.SubtaskResult
}

// subOut 并行子任务执行结果（对齐 Zoo-Code reopenParentFromDelegation 回填载荷）。
type subOut struct {
	callID string
	mode   string
	result string
	err    error
}

// maxParallelSubtasks 单轮并行子任务上限（超出部分排队执行）。
const maxParallelSubtasks = 4

// runSubtasksParallel 并行执行多个子任务委派（P2.8 多 Agent 并行协作）。
// 每个子任务经 SubtaskRunner 在通道层提供的独立运行态中执行（独立会话/引擎/
// 待办，事件以子 sessionId 推送）；父任务阻塞至全部完成，结果按委派顺序返回。
// 子任务可各自继续委派（嵌套深度受 MaxSubtaskDepth 约束）。
func (e *Engine) runSubtasksParallel(ctx context.Context, subs []subAssign, say SayFn, onAsk AskFn, depth int) []subOut {
	outs := make([]subOut, len(subs))
	gate := make(chan struct{}, maxParallelSubtasks)
	var wg sync.WaitGroup
	for i, sa := range subs {
		wg.Add(1)
		go func(i int, sa subAssign) {
			defer wg.Done()
			out := subOut{callID: sa.callID, mode: sa.st.Mode}
			defer func() { outs[i] = out }()
			// panic 防护（崩溃修复）：并行子任务 goroutine 的未捕获 panic 会
			// 击穿整个进程（父任务的 wg.Wait 也无法拦截）；拦截后记为该
			// 子任务失败，父任务正常回收结果继续运行。
			defer func() {
				if r := recover(); r != nil {
					slog.Error("并行子任务 panic（已拦截）",
						"mode", sa.st.Mode, "panic", r, "stack", string(debug.Stack()))
					out.err = fmt.Errorf("subtask panic: %v", r)
				}
			}()
			if e.MaxSubtaskDepth > 0 && depth >= e.MaxSubtaskDepth {
				out.err = fmt.Errorf("subtask depth limit (%d) reached", e.MaxSubtaskDepth)
				return
			}
			select {
			case gate <- struct{}{}:
				defer func() { <-gate }()
			case <-ctx.Done():
				out.err = ctx.Err()
				return
			}
			e.record(message.NewSay(message.SaySubtaskStarted,
				"Delegating subtask to mode "+sa.st.Mode+": "+sa.st.Message), say)
			res, err := e.SubtaskRunner(ctx, sa.st, say, onAsk, depth+1)
			if err != nil {
				out.err = err
				e.record(message.NewSay(message.SayError,
					"subtask ("+sa.st.Mode+") failed: "+err.Error()), say)
				return
			}
			// G1 工件核验（CCGS Trigger 0「验证工件而非回复」）：声明了 expect 的
			// 委派，收尾前先查产物文件是否在子任务工作区真实存在——流畅的回复
			// 不是阶段跑过的证据，工件才是。缺失即失败（含缺失清单，可续做）。
			if missing := checkExpectArtifacts(e.Workspace, sa.st.Expect); len(missing) > 0 {
				out.err = fmt.Errorf("subtask return contract unmet — expected artifacts missing on disk: %s", strings.Join(missing, ", "))
				e.record(message.NewSay(message.SayError,
					"subtask ("+sa.st.Mode+") failed: return contract unmet (missing artifacts: "+strings.Join(missing, ", ")+")"), say)
				return
			}
			out.result = res
			e.record(message.NewSay(message.SaySubtaskFinished, res), say)
		}(i, sa)
	}
	wg.Wait()
	return outs
}

// ---- codex-main 多智能体协作（spawn_agent / wait_agent 移植）----

// spawnHandle 后台智能体句柄（对齐 codex AgentThreadHandle：id + 完成信号）。
type spawnHandle struct {
	id     string
	done   chan struct{}
	result string
	err    error
}

// startSpawn 异步派生后台智能体（codex spawn_agent 语义）：立即返回 agent_id，
// 子任务在 goroutine 中经 SpawnRunner 以独立运行态执行；wait_agent 收割结果。
// 智能体生命周期经 subtask_started/finished say 事件推送前端
// （对齐 codex SubAgentActivity 事件简版）。
func (e *Engine) startSpawn(ctx context.Context, st tools.SubtaskResult, say SayFn, onAsk AskFn, depth int) string {
	e.spawnMu.Lock()
	if e.spawned == nil {
		e.spawned = map[string]*spawnHandle{}
	}
	e.spawnSeq++
	id := fmt.Sprintf("agent-%d", e.spawnSeq)
	h := &spawnHandle{id: id, done: make(chan struct{})}
	e.spawned[id] = h
	e.spawnOrder = append(e.spawnOrder, id)
	e.spawnMu.Unlock()

	go func() {
		// panic 防护（崩溃修复）：后台智能体 goroutine 的未捕获 panic 会击穿
		// 整个进程；拦截后记为该智能体失败，父任务经 wait_agent 正常回收。
		defer func() {
			if r := recover(); r != nil {
				slog.Error("后台智能体 panic（已拦截）",
					"agent", id, "panic", r, "stack", string(debug.Stack()))
				h.err = fmt.Errorf("agent panic: %v", r)
			}
			close(h.done)
		}()
		if e.SpawnRunner == nil {
			h.err = fmt.Errorf("background agents not configured")
			return
		}
		if e.MaxSubtaskDepth > 0 && depth+1 > e.MaxSubtaskDepth {
			h.err = fmt.Errorf("subtask depth limit (%d) reached", e.MaxSubtaskDepth)
			return
		}
		e.record(message.NewSay(message.SaySubtaskStarted,
			"Background agent "+id+" started in mode "+st.Mode+": "+st.Message), say)
		res, err := e.SpawnRunner(ctx, st, say, onAsk, depth+1)
		if err != nil {
			h.err = err
			e.record(message.NewSay(message.SayError,
				"Background agent "+id+" ("+st.Mode+") failed: "+err.Error()), say)
			return
		}
		// G1 工件核验：与并行子任务同语义（声明 expect 的后台智能体收尾
		// 先查产物存在性；流畅回复不是证据，工件才是）。
		if missing := checkExpectArtifacts(e.Workspace, st.Expect); len(missing) > 0 {
			h.err = fmt.Errorf("return contract unmet — expected artifacts missing on disk: %s", strings.Join(missing, ", "))
			e.record(message.NewSay(message.SayError,
				"Background agent "+id+" failed: return contract unmet (missing artifacts: "+strings.Join(missing, ", ")+")"), say)
			return
		}
		h.result = res
		e.record(message.NewSay(message.SaySubtaskFinished,
			"Background agent "+id+" finished:\n\n"+res), say)
	}()
	return id
}

// waitSpawns 阻塞等待指定后台智能体完成并聚合结果（codex wait_agent 语义）。
// ids 为空 = 等待全部已派生智能体；ctx 取消立即返回剩余未完成清单；
// 已完成智能体的结果可重复收割（幂等读，对齐 codex wait_agent 查询语义）。
func (e *Engine) waitSpawns(ctx context.Context, ids []string) string {
	e.spawnMu.Lock()
	var handles []*spawnHandle
	if len(ids) == 0 {
		for _, id := range e.spawnOrder { // 按派生顺序（map 遍历随机，order 保序）
			handles = append(handles, e.spawned[id])
		}
	} else {
		for _, id := range ids {
			if h, ok := e.spawned[strings.TrimSpace(id)]; ok {
				handles = append(handles, h)
			}
		}
	}
	e.spawnMu.Unlock()

	if len(handles) == 0 {
		if len(ids) == 0 {
			return "No background agents have been spawned yet. " +
				"Use spawn_agent (or new_task with background=true) first."
		}
		return "No background agents matched the requested ids (" + strings.Join(ids, ", ") + ")."
	}

	var b strings.Builder
	for _, h := range handles {
		select {
		case <-h.done:
		case <-ctx.Done():
			// 任务停止：不再等待，报告未完成智能体清单（结果不丢，可再查）。
			fmt.Fprintf(&b, "agent %s: still running (wait canceled: %v)\n\n", h.id, ctx.Err())
			continue
		}
		if h.err != nil {
			fmt.Fprintf(&b, "agent %s: failed: %s\n\n", h.id, h.err.Error())
			continue
		}
		fmt.Fprintf(&b, "agent %s completed:\n%s\n\n", h.id, h.result)
	}
	return strings.TrimSpace(b.String())
}

// resolveMode 解析模式配置（nil Manager 或未知 slug → nil，用默认语义）。
func (e *Engine) resolveMode(slug string) *modes.ModeConfig {
	if e.Modes == nil {
		if cfg, ok := (&modes.Manager{}).GetBySlug(slug); ok {
			return &cfg
		}
		return nil
	}
	if cfg, ok := e.Modes.GetBySlug(slug); ok {
		return &cfg
	}
	return nil
}

// allowedTools 计算当前模式可用工具（对齐 getToolsForMode + ALWAYS_AVAILABLE_TOOLS）。
// Manager 未配置时返回 nil（= 不过滤，注册中心全量工具）。
func (e *Engine) allowedTools(modeCfg *modes.ModeConfig) []string {
	if e.Modes == nil {
		return nil
	}
	var groups []modes.GroupEntry
	if modeCfg != nil {
		groups = modeCfg.Groups
	}
	return modes.ToolsForMode(groups)
}

// skillsXML 生成 <available_skills> 段。
func (e *Engine) skillsXML() string {
	if e.SkillsXML == nil {
		return ""
	}
	return e.SkillsXML()
}

// todosMD 当前任务清单 markdown。
func (e *Engine) todosMD() string {
	if e.Todos == nil {
		return ""
	}
	return e.Todos.Markdown()
}

// logDebug 引擎日志便捷方法（nil 安全）。
func (e *Engine) logDebug(msg string) {
	if e.Log != nil {
		e.Log.Debug(msg)
	}
}

// execTool 执行工具；捕获 ask_followup / attempt_completion / new_task /
// switch_mode / spawn_agent / wait_agent 特殊标记。
// allowed 为本次任务已解析的模式工具白名单（RunInMode 顶部计算一次传入，
// 不再逐调用重复 resolveMode+allowedTools——每次调用会重建整张名单切片）。
func (e *Engine) execTool(ctx context.Context, tc llm.ToolCall, say SayFn, onAsk AskFn, modeSlug string, depth int, allowed []string,
) (completion *tools.CompletionResult, subtask *tools.SubtaskResult, switchReq *tools.SwitchModeResult,
	spawnReq *tools.SpawnAgentResult, waitReq *tools.WaitAgentResult, result string, err error) {

	t, ok := e.Registry.Get(tc.Name)
	if !ok {
		// MCP 惰性加载：mcp_* 工具未注册时引导先 mcp_activate 激活服务器
		//（目录与激活语义见系统提示词 MCP TOOL CATALOG 段），而非让模型
		// 在 unknown tool 错误上空转。
		if strings.HasPrefix(tc.Name, "mcp_") && e.MCPLazyHint != "" {
			return nil, nil, nil, nil, nil, "", fmt.Errorf(
				"unknown tool %q — 该 MCP 工具所属服务器尚未激活（惰性加载）。"+
					"先调用 mcp_activate 工具（参数 server 填服务器名，见系统提示词 MCP TOOL CATALOG 段）激活后，下一轮即可调用。", tc.Name)
		}
		return nil, nil, nil, nil, nil, "", fmt.Errorf("unknown tool %q", tc.Name)
	}

	// 模式工具过滤校验（越权工具直接拒绝；allowed==nil 表示不过滤）。
	// codex 对齐补全 ⑦：MCP 动态工具以 mcp_ 前缀注册，跨模式可用
	// （对齐 codex MCP 工具不受模式组限制的语义），按前缀放行。
	if allowed != nil {
		found := strings.HasPrefix(tc.Name, "mcp_")
		for _, n := range allowed {
			if n == tc.Name {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, nil, nil, nil, "", fmt.Errorf(
				"tool %q is not available in mode %q (allowed: %s)",
				tc.Name, modeSlug, strings.Join(allowed, ", "))
		}
	}

	// 敏感文件保护（对齐 RooProtectedController 简版）：.env/密钥/.git 等路径
	// 的写入强制人工审批——AutoApprove 直通在此失效，防凭据被静默改写。
	// 无人值守会话（如 roadmap 自主开发）依据文本内 "[红线] 敏感文件写入"
	// 标记自动拒绝（无人值守 ask 桥不打开卡片）；普通交互会话照常弹卡。
	if sensitiveWriteCall(tc.Name, tc.Arguments) {
		askMsg := message.NewAsk(message.AskTool,
			marshalToolInfo(tc.Name, tc.Arguments)+"\n"+RedlineAskMarker+"（受保护路径：.env/密钥/.git，无人值守自动拒绝；如需写入请在人工会话中确认）")
		if resp, _ := e.waitAsk(say, onAsk, askMsg); resp != message.ResponseYes {
			return nil, nil, nil, nil, nil, "", fmt.Errorf("write to protected file rejected (redline)")
		}
	}

	// codex 对齐补全 ②：危险命令检测——命中平台化危险清单的 shell 调用
	// 强制人工审批（AutoApprove 直通失效），审批卡片附风险说明
	// （对齐 exec_policy.rs DangerousCommand + BANNED_PREFIX_SUGGESTIONS 语义）。
	// say 通知（不落会话阻塞，仅前端可见风险提示）。
	if d := tools.CheckDangerousCommandAuto(shellCommandOf(tc.Name, tc.Arguments), e.Workspace, e.DangerAutoApprove); d.Danger {
		e.record(message.NewSay(message.SayDangerBlocked,
			fmt.Sprintf("危险命令检测命中：%s（%s）", shellCommandOf(tc.Name, tc.Arguments), d.Reason)), say)
		askMsg := message.NewAsk(message.AskCommand,
			marshalToolInfo(tc.Name, tc.Arguments)+"\n[危险命令] "+d.Reason)
		// 危险操作自动确认（用户需求 2026-10）：设置开启后命中项免审放行，
		// 「自动选择最优方案继续执行」——风险说明已随 say 入会话历史留痕。
		// 优先级：全量自动确认 > 工作区内临时产物降级 > 强制人工审批；
		// e.AutoApprove 为 nil（严格模式）时仍走 waitAsk，行为不变。
		if d.AutoConfirm ||
			(d.WorkspaceScoped && e.AutoApprove != nil && e.AutoApprove(askMsg)) {
			// 自动放行：降级为普通工具审批路径（不阻塞）。
		} else if resp, _ := e.waitAsk(say, onAsk, askMsg); resp != message.ResponseYes {
			return nil, nil, nil, nil, nil, "", fmt.Errorf("dangerous command rejected by user: %s", d.Reason)
		}
	}

	// codex 对齐补全 ⑥：PreToolUse 钩子（对齐 hook_runtime.rs run_pre_tool_use_hooks
	// 简版：可改参 / 阻断）。allow=false 时该调用被拒绝，blockMsg 回填模型。
	args := tc.Arguments
	if e.HookPreToolUse != nil {
		allow, newArgs, blockMsg := e.HookPreToolUse(tc.Name, tc.Arguments)
		if !allow {
			return nil, nil, nil, nil, nil, "", fmt.Errorf("blocked by PreToolUse hook: %s", blockMsg)
		}
		if newArgs != "" {
			args = newArgs
		}
	}
	// Claude Code Hooks 对齐：用户可配置外部命令钩子（hooks.json）。
	// 阻断语义与员工网关叠加：先程序内钩子后用户钩子，任一阻断即阻断；
	// 钩子可改参（返回 {"args":...}），改写后参数供审批/执行/追踪全链路一致。
	if e.UserHooks != nil {
		var blockMsg string
		args, blockMsg = e.UserHooks.PreToolUse(ctx, tc.Name, args)
		if blockMsg != "" {
			return nil, nil, nil, nil, nil, "", fmt.Errorf("blocked by user hook: %s", blockMsg)
		}
	}

	// 工具审批：写文件/命令类转 ask（P1 CLI 直通自动批准；多通道时渲染审批按钮）。
	// 聊天媒体生成免审直通（2026-10 对话支持）：generate_image/generate_video/
	// generate_speech 只读外呼 + 落盘用户媒体目录，无破坏性，弹审批卡会打断
	// 「说一句就出图」的对话体验（用户语义：配置了模型就直接用）。
	if tc.Name == "generate_image" || tc.Name == "generate_video" || tc.Name == "generate_speech" {
		// 直通：跳过下方 AutoApprove 弹卡路径（引擎行为注释，无状态）。
	} else if allow, forcedAsk, decided := e.Approval.DecideEx(tc.Name, args, e.AutoApprove != nil); decided {
		// D2 审批判定（对标 Gemini CLI policy engine）：参数级规则优先 →
		// 工具级矩阵（C2 兼容）。红线（敏感写入/危险命令）已在更早分支
		// 判定，本存储任何规则不能覆盖红线。
		switch {
		case !allow && !forcedAsk: // deny
			return nil, nil, nil, nil, nil, "", fmt.Errorf("tool %q denied by approval rules", tc.Name)
		case forcedAsk: // ask：强制弹卡（即使 AutoApprove 已开启），no 则拒绝。
			askMsg := message.NewAsk(message.AskTool, marshalToolInfo(tc.Name, args))
			if resp, _ := e.waitAsk(say, onAsk, askMsg); resp != message.ResponseYes {
				return nil, nil, nil, nil, nil, "", fmt.Errorf("tool %q rejected by user", tc.Name)
			}
		default: // allow：直通（即使 AutoApprove 未开启）。
		}
	} else if e.skillGranted(tc.Name, args) {
		// D3 技能当轮授权：免审直通（红线已在此前分支判定，不受影响）。
	} else if e.AutoApprove != nil {
		// 全部未命中 → 落回原有全局 AutoApprove 逻辑（零破坏）。
		askMsg := message.NewAsk(message.AskTool, marshalToolInfo(tc.Name, args))
		if !e.AutoApprove(askMsg) {
			resp, _ := e.waitAsk(say, onAsk, askMsg)
			if resp != message.ResponseYes {
				return nil, nil, nil, nil, nil, "", fmt.Errorf("tool %q rejected by user", tc.Name)
			}
		}
	}

	// codex 对齐补全 ③：文件写入类工具执行前快照基线（重复触达保留最早
	// 基线 = 任务起点 vs 最终状态的 net diff 语义；基线快照用钩子改参后
	// 的 args，保证与实际执行一致）。
	if p := writeTargetPath(tc.Name, args); p != "" {
		e.diffTracker.track(e.Workspace, p)
	}

	// Zoo-Code 对齐（FileContextTracker 简版）：写入前 stale read 检测——
	// 文件在本任务内被 fs_read 过、且此后磁盘 mtime 变化（外部 shell/用户
	// 改动）→ 警告文本附加到工具结果，提醒模型重读后再改。
	staleWarn := ""
	if p := writeTargetPath(tc.Name, args); p != "" {
		staleWarn = e.fileTracker.checkStale(e.Workspace, p)
	}

	res, err := t.Execute(ctx, args)
	if err == nil {
		switch tc.Name {
		case "fs_read":
			if p := readTargetPath(tc.Name, args); p != "" {
				e.fileTracker.markRead(e.Workspace, p)
			}
		case "fs_write", "fs_apply_diff":
			if p := writeTargetPath(tc.Name, args); p != "" {
				e.fileTracker.markWritten(e.Workspace, p)
			}
		case "generate_image", "generate_video", "generate_speech":
			// 媒体生成（图片/视频/语音）：generate_image 工具已统一走
			// KAELVORIS_MEDIA 载荷通道（kind=image 复用，见 image_payload.go
			// imgsToMedia）——剥离结果尾部的标记，纯文本回填模型，按 kind 分流
			// 转 SayImage / SayVideo / SayAudio say 消息推送聊天流（base64 不进
			// 模型上下文；say 桥把实时 dataUrl 保留在消息 Images、历史只落 text
			// 载荷里的磁盘路径）。
			if clean, medias := tools.ExtractMediaPayload(res); len(medias) > 0 {
				res = clean
				sayType := message.SayVideo
				switch medias[0].Kind {
				case "image":
					sayType = message.SayImage
				case "audio":
					sayType = message.SayAudio
				}
				mediaMsg := message.NewSay(sayType, tools.MarshalMediaMsgText(medias))
				for _, md := range medias {
					if md.DataURL != "" {
						mediaMsg.Images = append(mediaMsg.Images, md.DataURL)
					}
				}
				e.record(mediaMsg, say)
			}
		}
	}
	if staleWarn != "" && err == nil {
		res = staleNotice(staleWarn, res)
		// 用 SayText 而非 SayTool：SayTool 的 text 约定为工具 JSON 载荷，
		// 非 JSON 警告混入后自学习/每小时复盘的工具解析持续报错（本轮复盘
		// 实证 4 次 parse fail），ACP 桥还会把它渲染成假工具卡。
		e.record(message.NewSay(message.SayText, "[file-context] "+staleWarn), say)
	}
	if err == nil && e.HookPostToolUse != nil {
		// codex 对齐补全 ⑥：PostToolUse 钩子（对齐 hook_runtime.rs
		// run_post_tool_use_hooks 简版：结果旁路观察点；不修改结果）。
		e.HookPostToolUse(tc.Name, args, res)
	}
	if err == nil && e.UserHooks != nil {
		// Claude Code Hooks 对齐：用户可配置 PostToolUse 外部命令（观察点）。
		e.UserHooks.PostToolUse(ctx, tc.Name, args, res)
	}
	if err != nil {
		switch v := err.(type) {
		case *tools.AskResult:
			// ask_followup_question：默认阻塞等待用户回答。
			// 全AI自动决策开启（AutoDecide，用户需求 2026-10）：不阻塞等用户——
			// 问题记录 say 留痕（对齐 danger_blocked 语义：前端可见、会话可复盘），
			// tool_result 回填「自主决策」指令，模型自行结合上下文推演选择。
			// 防死循环：同一轮内连续提问超过 2 次即视为无意义追问，回填强指令
			// 要求立即推进（模型常把无法从上下文确定的事情反复提问）。
			if e.AutoDecide {
				e.autoAskCount++
				e.record(message.NewSay(message.SayAutoDecision,
					"[全AI自动决策] 模型提问（未等待用户，已记录，由 AI 结合上下文自主决策）：\n"+v.Question), say)
				if e.autoAskCount >= 3 {
					return nil, nil, nil, nil, nil, "(full-auto mode) Your repeated follow-up questions cannot be forwarded to the user. Stop asking. Make a reasonable decision based on the context and continue the task. If truly necessary, note the assumption; the user can correct it after completion.", nil
				}
				return nil, nil, nil, nil, nil, "(full-auto mode) The user cannot answer right now. Decide autonomously based on the context: pick the most reasonable option, state your assumption, and continue. Do not ask the user again.", nil
			}
			askMsg := message.NewAsk(message.AskFollowup, v.Question)
			_, text := e.waitAsk(say, onAsk, askMsg)
			return nil, nil, nil, nil, nil, "User answered: " + text, nil
		case *tools.CompletionResult:
			e.record(message.NewAsk(message.AskCompletionResult, v.Result), say)
			return v, nil, nil, nil, nil, "", nil
		case *tools.SubtaskResult:
			return nil, v, nil, nil, nil, "", nil
		case *tools.SpawnAgentResult:
			return nil, nil, nil, v, nil, "", nil
		case *tools.WaitAgentResult:
			return nil, nil, nil, nil, v, "", nil
		case *tools.SwitchModeResult:
			return nil, nil, v, nil, nil, "", nil
		default:
			return nil, nil, nil, nil, nil, "", err
		}
	}
	return nil, nil, nil, nil, nil, res, nil
}

// shellCommandOf 提取 shell 类工具调用的命令文本（非 shell 工具返回空）。
func shellCommandOf(tool, args string) string {
	if tool != "shell" {
		return ""
	}
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return ""
	}
	return p.Command
}

// writeTargetPath 提取文件写入类工具（fs_write / fs_apply_diff）的目标相对
// 路径（其他工具返回空；供 turn diff 追踪器定位快照文件）。
func writeTargetPath(tool, args string) string {
	if tool != "fs_write" && tool != "fs_apply_diff" {
		return ""
	}
	return pathArgOf(args)
}

// readTargetPath 提取文件读取类工具（fs_read）的目标相对路径（其他工具返回
// 空；供 FileContextTracker 记录读取基线）。
func readTargetPath(tool, args string) string {
	if tool != "fs_read" {
		return ""
	}
	return pathArgOf(args)
}

// pathArgOf 从工具 JSON 参数提取 path 字段（解析失败返回空）。
func pathArgOf(args string) string {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return ""
	}
	return p.Path
}

// checkExpectArtifacts G1 工件核验（CCGS Trigger 0「验证工件而非回复」）：
// 逐一检查声明的期望产物（相对路径）在工作区磁盘上真实存在；返回缺失清单
// （空 = 全部存在或无声明）。非文件型产物（目录）也算存在——委派方声明的是
// 「可核验的产物路径」，目录产物同样可核验。工作区为空时按当前目录解析。
func checkExpectArtifacts(workspace string, expect []string) []string {
	var missing []string
	for _, rel := range expect {
		p := strings.TrimSpace(rel)
		if p == "" {
			continue
		}
		base := workspace
		if base == "" {
			base, _ = os.Getwd()
		}
		if _, err := os.Stat(filepath.Join(base, filepath.FromSlash(p))); err != nil {
			missing = append(missing, p)
		}
	}
	return missing
}

// waitAsk 发出 ask（阻塞语义）并收集应答；同时记录到会话。
func (e *Engine) waitAsk(say SayFn, onAsk AskFn, ask message.ClineMessage) (message.AskResponse, string) {
	e.record(ask, say)
	if onAsk == nil {
		return message.ResponseYes, ""
	}
	resp, text := onAsk(ask)
	return resp, text
}

// autoDecideAsk 全AI自动决策（AutoDecide）下的提问性 ask 统一应答器。
// 用户需求 2026-12：开启「自动审批 + AI 自主决策」后，提问性的 ask 不再阻塞等
// 人——问题留痕（SayAutoDecision 卡）后返回应答，模型结合上下文自主决策推进。
// 覆盖：模式切换 switch_mode、请求预算续期（mistake_limit 型）、流错误重试、
// 错误上限续跑（对齐 ask_followup 的 AutoDecide 分支与 unattendedAskBridge 语义）。
// 返回 (false, "") = 该 ask 不属提问性（调用方落回原阻塞逻辑）。
func (e *Engine) autoDecideAsk(ask message.ClineMessage, say SayFn) (message.AskResponse, string) {
	switch ask.Ask {
	case message.AskMistakeLimit, message.AskAPIReqFailed, message.AskAutoApprovalMax, message.AskResumeTask:
		e.record(message.NewSay(message.SayAutoDecision,
			"[全AI自动决策] "+ask.Text+"\n→ 已自动批准/继续执行（未等待用户）"), say)
		return message.ResponseYes, ""
	}
	return "", ""
}

// ---- D3 技能当轮工具授权（对标 Claude Code skills allowed-tools）----

// GrantSkills 追加当轮授权模式串（去重）。宿主经 SkillTool.OnAuthorize 在
// skill 工具执行成功后调用；下一轮请求构造前被 clearSkillGrants 清空。
// 匹配语义（skillGranted）：
//   - "shell"　　　　= 工具名精确匹配；
//   - "Bash(git add *)" = 工具名 Bash（kaelvoris 内即 shell）且参数含前缀
//
// "git add"——对 Claude Code `Tool(prefix *)` 形态的简版对齐，
// 不做完整 glob 引擎（ponytail：需要时升级为规则引擎）。
func (e *Engine) GrantSkills(patterns []string) {
	e.grantMu.Lock()
	defer e.grantMu.Unlock()
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		dup := false
		for _, g := range e.SkillGrants {
			if g == p {
				dup = true
				break
			}
		}
		if !dup {
			e.SkillGrants = append(e.SkillGrants, p)
		}
	}
}

// skillGranted 判定工具调用是否命中当轮授权表（精确名或 Name(prefix *) 前缀形态）。
func (e *Engine) skillGranted(tool, args string) bool {
	e.grantMu.Lock()
	defer e.grantMu.Unlock()
	for _, g := range e.SkillGrants {
		if idx := strings.Index(g, "("); idx > 0 && strings.HasSuffix(g, "*)") {
			// Name(prefix *) 形态：工具名相等且参数包含 prefix。
			name := g[:idx]
			prefix := strings.TrimSpace(g[idx+1 : len(g)-2])
			if name == tool && strings.Contains(args, prefix) {
				return true
			}
			continue
		}
		if g == tool {
			return true
		}
	}
	return false
}

// clearSkillGrants 清空当轮授权表（主循环每轮顶部、构造 LLM 请求前调用——
// 对齐 "The grant clears when you send your next message"）。
func (e *Engine) clearSkillGrants() {
	e.grantMu.Lock()
	e.SkillGrants = nil
	e.grantMu.Unlock()
}

// record 记录消息并推送事件（并行子任务 fan-out 期间并发调用，加锁保护）。
func (e *Engine) record(m message.ClineMessage, say SayFn) {
	e.msgMu.Lock()
	e.Messages = append(e.Messages, m)
	e.msgMu.Unlock()
	if say != nil {
		say(m)
	}
}

// streamRetryLimit 流式请求自动重试上限（0 = 未设置用默认 3；<0 = 禁用）。
func (e *Engine) streamRetryLimit() int {
	if e.StreamRetryLimit > 0 {
		return e.StreamRetryLimit
	}
	if e.StreamRetryLimit < 0 {
		return 0 // 显式禁用：错误直接进 api_req_failed 卡片
	}
	return 3
}

// isTransientStreamError 判断流式请求错误是否值得自动重试：
// 非结构化错误（流空闲看门狗超时 / 连接中断 / 网络重置）一律按瞬时处理；
// 结构化 APIError 仅 429/408/5xx 视为瞬时——401/403/404/400 等确定性
// 配置错误快速失败（直接进 ask 卡片），不做无意义的自动重试。
func isTransientStreamError(err error) bool {
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusTooManyRequests, http.StatusRequestTimeout:
			return true
		}
		return apiErr.Status >= 500
	}
	return true
}

// degradePrompt 第三级兑底注入的降级续跑指引（user 消息文本）。
// 按任务性质分两路处置（用户需求 2026-10：测试的就跳过、开发的换思路）——
// 引擎不猜任务类型，把判定与决策交给模型（上下文最全），只约定输出边界：
// 不许原样重试、不许伪造成功、必须明确说明处置原因。
func degradePrompt(streamErr error, retried int) string {
	return fmt.Sprintf(`[系统提示] LLM 链路连续故障：%v（自动重试 %d 次未恢复，备用模型亦不可用）。请勿原样重试同一请求，按任务性质处置：
1. 若本任务是验证/测试/冒烟类（跑脚本、检查环境、执行验证）：当前步骤因模型链路故障无法继续，直接用 attempt_completion 收口，注明「因 LLM 链路故障跳过」与卡点原因，不伪造成功；
2. 若本任务是开发/改造类：换一条实现思路继续（更换工具、调整实现路径、缩小改动范围），绕开刚才失败的路径完成任务后再收口。`, streamErr, retried)
}

// toolDefs 注册中心 → LLM 工具定义（allowed 为 nil 时不过滤）。
// MCP 修复（对齐 Zoo-Code build-tools.ts 语义）：native 工具经模式过滤，
// MCP 动态工具（mcp_ 前缀）由 getMcpServerTools 独立构建后直接并入——
// 不受模式组白名单限制（GroupMCP 为空集，且 execTool 执行侧本就按前缀
// 放行）。此前定义侧漏放行导致模型每轮请求都收不到 MCP 工具定义，
// 表现为「配置了 MCP 却无法识别、不自动调用」。
func toolDefs(r *tools.Registry, allowed []string) []llm.ToolDef {
	names := r.Names()
	if allowed != nil {
		filter := map[string]bool{}
		for _, n := range allowed {
			filter[n] = true
		}
		filtered := make([]string, 0, len(names))
		for _, n := range names {
			if filter[n] || strings.HasPrefix(n, "mcp_") {
				filtered = append(filtered, n)
			}
		}
		names = filtered
	}
	defs := make([]llm.ToolDef, 0, len(names))
	for _, n := range names {
		t, ok := r.Get(n)
		if !ok {
			continue
		}
		defs = append(defs, llm.ToolDef{
			Type: "function",
			Function: llm.ToolFunc{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return defs
}

// sanitizeToolCallArgs 纵深防御：遍历消息，把 tool_calls[].function.arguments
// 中的非法 JSON 就地替换为合法占位（ToolCalls 切片共享底层数组，原位修复即
// 持久生效，可治愈已被污染的运行中会话）。新产生的调用已在入列前校验，
// 此处兜底覆盖其他路径，防止严格后端（vLLM/Pydantic 重新解析 arguments
// 字符串）返回 400 Unterminated string 导致任务永久卡死。
func sanitizeToolCallArgs(msgs []llm.Message) {
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			args := msgs[i].ToolCalls[j].Arguments
			switch {
			case args == "":
				msgs[i].ToolCalls[j].Arguments = "{}"
			case !json.Valid([]byte(args)):
				msgs[i].ToolCalls[j].Arguments = fmt.Sprintf(
					`{"malformed_arguments":true,"original_chars":%d}`, len(args))
			}
		}
	}
}

// marshalToolInfo 序列化工具调用展示载荷（Args 保留原始参数 JSON 供通道层渲染 diff）。
func marshalToolInfo(name, args string) string {
	info := message.ToolCallInfo{Tool: name, Args: args}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(args), &m); err == nil {
		s, _ := m["path"].(string)
		info.Path = s
		s, _ = m["command"].(string)
		info.Cmd = s
		s, _ = m["regex"].(string)
		info.Regex = s
		s, _ = m["query"].(string)
		info.Query = s
	}
	b, _ := json.Marshal(info)
	return string(b)
}

// defaultToolResultMaxChars 工具结果单条默认体积上限（字符，约 40KB ≈ 10k token）。
// 413 防护第二层：滑动窗口按「消息组」粒度截断，尾部单条超大工具结果
// （如一次性读取整份大文件）无法被移除，导致载荷始终超限 → 413。
// 在入列前把单条结果压到上限内，配合 fs_read 分页兜底。
const defaultToolResultMaxChars = 40000

// clampToolResult 将单条工具结果限制在 ToolResultMaxChars（默认 40k 字符）内。
// 超限时按 UTF-8 字符边界截断，并附加续读提示（fs_read 支持 offset/limit 分页，
// 默认每次 100 行、单次上限 1000 行，可循环调用读完整个文件）。
func (e *Engine) clampToolResult(result string) string {
	maxChars := e.ToolResultMaxChars
	if maxChars <= 0 {
		maxChars = defaultToolResultMaxChars
	}
	if len(result) <= maxChars {
		return result
	}
	cut := truncBytes(result, maxChars)
	return cut + fmt.Sprintf(
		"\n\n[tool result truncated: %d chars total, showing first %d. "+
			"If this is file content, re-read it in pages with fs_read "+
			"(offset/limit params: default 2000 lines per call).]",
		len(result), len(cut))
}

// truncBytes 按字节上限截断字符串并保证 UTF-8 边界安全（最多回退 3 字节，
// 避免截断在多字节字符中间产生乱码）。
func truncBytes(s string, maxBytes int) string {
	if maxBytes >= len(s) {
		return s
	}
	cut := s[:maxBytes]
	for i := 0; i < 3 && len(cut) > 0; i++ {
		if r, _ := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

// toolRepetitionDetector 连续完全相同工具调用检测（移植 Zoo-Code
// ToolRepetitionDetector：同 (name, args) 连续达到 limit 次即拒绝执行并重置，
// 用户纠偏后可恢复；limit<=0 关闭）。
type toolRepetitionDetector struct {
	prev  string // 上一次调用的规范化标识
	count int    // 连续相同计数
	limit int
}

// allow 判定本次调用是否放行。
func (d *toolRepetitionDetector) allow(name, args string) bool {
	if d.limit <= 0 {
		return true
	}
	key := canonicalToolKey(name, args)
	if d.prev == key {
		d.count++
	} else {
		d.prev = key
		d.count = 0
	}
	if d.count >= d.limit {
		d.prev, d.count = "", 0 // 重置：纠偏后允许恢复
		return false
	}
	return true
}

// canonicalToolKey 规范化调用标识：参数 JSON 解析后重序列化（encoding/json
// 对 map 按键排序输出，等价 safe-stable-stringify 的键序规范化）；解析失败
// 退化为原文拼接。
func canonicalToolKey(name, args string) string {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return name + "|" + strings.TrimSpace(args)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return name + "|" + args
	}
	return name + "|" + string(b)
}

// RedlineAskMarker 敏感文件写入 ask 的红线标记（引擎 sensitiveWriteCall
// 在 ask 文本中携带；宿主层无人值守 ask 桥据此自动拒绝放行，防凭据被
// 静默改写。导出供宿主层同源引用，避免双份字面量漂移）。
const RedlineAskMarker = "[红线] 敏感文件写入"

// sensitiveWriteCall 判定是否敏感文件写入类调用（fs_write / fs_apply_diff）。
func sensitiveWriteCall(tool, args string) bool {
	if tool != "fs_write" && tool != "fs_apply_diff" {
		return false
	}
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil || p.Path == "" {
		return false
	}
	return tools.IsProtectedRelPath(p.Path)
}

// thinkOpen/thinkClose vLLM 等推理后端在 content 内嵌思考块的标签。
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// thinkSplitter 流式分割正文内嵌的 <think>…</think> 思考块（BUG-2 修复：
// 否则思考内容会以普通文字重复打印在聊天窗口，与思考卡片内容重复）。
// 标签可能被 SSE 流切成多段（如 "</thi"+"nk>"），缓冲状态机保证跨 chunk 安全：
// feed 返回本轮 (正文, 思考) 增量；流结束后调用 flush 把残余缓冲按状态归位。
type thinkSplitter struct {
	buf     strings.Builder // 跨 chunk 缓冲（仅保留可能构成标签前缀的尾巴）
	inThink bool
}

// holdback 返回 data 尾部可能是 tag 未完成前缀的起始下标（-1 表示无可疑尾巴）。
// 例如 data 以 "</thi" 结尾时返回该下标，剩余部分留待下一 chunk 拼合判定。
func holdback(data, tag string) int {
	max := len(tag) - 1
	if len(data) < max {
		max = len(data)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(data, tag[:n]) {
			return len(data) - n
		}
	}
	return -1
}

// feed 消费一个正文增量，返回其中的 (正文, 思考) 两部分。
func (s *thinkSplitter) feed(text string) (textOut, reasonOut string) {
	if text != "" {
		s.buf.WriteString(text)
	}
	data := s.buf.String()
	s.buf.Reset()
	var textB, reasonB strings.Builder
	for data != "" {
		if !s.inThink {
			if i := strings.Index(data, thinkOpen); i >= 0 {
				// 开标签：之前是正文，之后进入思考态。
				textB.WriteString(data[:i])
				data = data[i+len(thinkOpen):]
				s.inThink = true
				continue
			}
			// 无完整开标签：正文整体放行，仅扣留可能是半个 "<think>" 的尾巴。
			if i := holdback(data, thinkOpen); i >= 0 {
				textB.WriteString(data[:i])
				s.buf.WriteString(data[i:])
				break
			}
			textB.WriteString(data)
			data = ""
		} else {
			if i := strings.Index(data, thinkClose); i >= 0 {
				// 闭标签：之前是思考内容，之后回到正文态。
				reasonB.WriteString(data[:i])
				data = data[i+len(thinkClose):]
				s.inThink = false
				continue
			}
			// 无完整闭标签：思考内容整体放行，仅扣留可能是半个 "</think>" 的尾巴。
			if i := holdback(data, thinkClose); i >= 0 {
				reasonB.WriteString(data[:i])
				s.buf.WriteString(data[i:])
				break
			}
			reasonB.WriteString(data)
			data = ""
		}
	}
	return textB.String(), reasonB.String()
}

// flush 在流结束时调用：残余缓冲按当前状态归入思考（未闭合）或正文（未成标签）。
func (s *thinkSplitter) flush() (textOut, reasonOut string) {
	data := s.buf.String()
	s.buf.Reset()
	if data == "" {
		return "", ""
	}
	if s.inThink {
		return "", data
	}
	return data, ""
}
