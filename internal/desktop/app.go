package desktop

// App Wails 绑定层：桌面通道（对齐架构文档 internal/channel/desktop 的宿主形态）。
//
// 事件桥语义：
//   - say → uiEmit("cline:message", msg)（前端流式覆盖渲染）
//   - ask → EventsEmit 后阻塞等待 ApproveAsk 回传（审批按钮/文本输入）
//
// 内核组装与 cmd/kaelvoris（CLI）一致：modes + skills + todos + 12 工具 + 引擎。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/agent"
	"kaelvoris/internal/approval"
	"kaelvoris/internal/autocommit"
	"kaelvoris/internal/checkpoint"
	"kaelvoris/internal/condense"
	"kaelvoris/internal/cozeflow"
	"kaelvoris/internal/cron"
	"kaelvoris/internal/employee"
	"kaelvoris/internal/evolve"
	"kaelvoris/internal/hooks"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/mcp"
	"kaelvoris/internal/modes"
	"kaelvoris/internal/plugin"
	"kaelvoris/internal/promptconfig"
	"kaelvoris/internal/remote"
	"kaelvoris/internal/roadmap"
	"kaelvoris/internal/session"
	"kaelvoris/internal/skills"
	"kaelvoris/internal/task"
	"kaelvoris/internal/tools"
	"kaelvoris/internal/usage"
	"kaelvoris/internal/vector"
	"kaelvoris/internal/verify"
	"kaelvoris/internal/version"
	"kaelvoris/internal/webhook"
	"kaelvoris/pkg/message"
)

// Config 桌面端配置（持久化于 ~/.kaelvoris/desktop.json）。
type Config struct {
	BaseURL     string `json:"baseURL"`
	APIKey      string `json:"apiKey"`
	Model       string `json:"model"`
	Workspace   string `json:"workspace"`
	AutoApprove bool   `json:"autoApprove"`
	// EditorPreference 一键打开项目的目标方式（顶栏项目按钮右侧按钮）：
	// "folder" = 系统文件管理器（默认）；"zed" = 在项目目录执行 zed 命令打开。
	EditorPreference string `json:"editorPreference,omitempty"`
	// RecentProjects 最近打开的项目目录（顶栏项目切换器；当前工作区置顶）。
	RecentProjects []string `json:"recentProjects,omitempty"`
	// P3/B5 上下文治理：ContextWindow 模型上下文窗口（token；0 = 不启用）；
	// AutoCondensePercent 触发百分比（默认 90）；CondenseDisabled 总开关
	//（规划板 2026-10：压缩行为可在设置中关闭，false = 默认开启摘要压缩）。
	ContextWindow       int  `json:"contextWindow,omitempty"`
	AutoCondensePercent int  `json:"autoCondensePercent,omitempty"`
	CondenseDisabled    bool `json:"condenseDisabled,omitempty"`
	// Vector 向量化知识库配置（nil = 未配置；对应 Zoo-Code codebaseIndex*
	// 配置族：开关/目录/自动化时间/embeddings 端点，见 internal/vector）。
	Vector *vector.Config `json:"vector,omitempty"`
	// SelfReview 任务结束自动复盘（自学习闭环，对齐 kaelvoris-agent
	// background_review：nil = 未配置 = 关闭，见 selflearn.go）。
	SelfReview *SelfReviewConfig `json:"selfReview,omitempty"`
	// TaskSummary 任务结束会话总结（会话 hook，2026-10 新增）：nil = 未配置 = 关闭。
	// 开启后每轮任务结束后台生成「会话总结」（目标/状态/产出/下一步），
	// 落会话存储（taskSummary 字段）并在聊天流/会话卡展示，见 task_summary.go。
	TaskSummary *TaskSummaryConfig `json:"taskSummary,omitempty"`
	// AutoDev 自主迭代配置（用户需求 2026-10）：nil = 未配置 = 关闭。
	// 开启后系统完全自主决策：自动规划→自动批准→逐条开发→自动复盘
	//（roadmap_panel.go）；关闭时提案仍落 proposed，用户在面板勾选审核。
	AutoDev *AutoDevConfig `json:"autoDev,omitempty"`
	// 聊天内生图（generate_image 工具）：OpenAI images API 兼容端点。
	// BaseURL/Model 空 = 未启用（工具调用时提示去设置页配置）；
	// APIKey 空 = 不带认证头（本地网关场景）。
	ImageGenBaseURL string `json:"imageGenBaseURL,omitempty"`
	ImageGenModel   string `json:"imageGenModel,omitempty"`
	ImageGenAPIKey  string `json:"imageGenAPIKey,omitempty"`
	// 媒体生成（generate_video / generate_speech 工具）：同为 OpenAI 兼容端点。
	// 视频 POST {base}/videos（Sora 异步任务轮询/同步 url/b64/直落字节三兼容）；
	// 语音 POST {base}/audio/speech；SpeechVoice 为默认音色（可空）。
	VideoGenBaseURL string `json:"videoGenBaseURL,omitempty"`
	VideoGenModel   string `json:"videoGenModel,omitempty"`
	VideoGenAPIKey  string `json:"videoGenAPIKey,omitempty"`
	// VideoGenMode/VideoGenSize：Agnes 系网关必填（mode=text / size=720P）；
	// 空 = 不携带，走 Sora 原生语义（见 internal/tools/media_tools.go）。
	VideoGenMode  string `json:"videoGenMode,omitempty"`
	VideoGenSize  string `json:"videoGenSize,omitempty"`
	SpeechBaseURL string `json:"speechBaseURL,omitempty"`
	SpeechModel   string `json:"speechModel,omitempty"`
	SpeechAPIKey  string `json:"speechAPIKey,omitempty"`
	SpeechVoice   string `json:"speechVoice,omitempty"`
	// 成本估算单价（Claude Code /cost 对齐）：每百万 token 美元单价；
	// 0 = 不计费仅计 token。由用户按所用模型在设置中填写。
	CostPerMPrompt     float64 `json:"costPerMPrompt,omitempty"`
	CostPerMCompletion float64 `json:"costPerMCompletion,omitempty"`
	// DangerAutoApprove 危险操作自动确认（用户需求 2026-10）：开启后命令命中
	// 危险清单（internal/tools/danger.go）不再强制人工审批，直接放行执行；
	// 敏感文件写入（.env/.git 等）仍强制人工确认（红线不随此开关放开）。
	DangerAutoApprove bool `json:"dangerAutoApprove,omitempty"`
	// Sandbox 命令沙箱配置（Windows Job Object 进程树围栏，2026-10 新增）：
	// nil = 未配置（loadConfig 兑底为 job 模式纯杀树保证，limits 全 0）。
	// job 模式：超时/结束时整树终止（含孙进程），可限内存/CPU/进程数；
	// 不隔离文件与网络访问（job object 无此能力）。
	Sandbox *tools.SandboxConfig `json:"sandbox,omitempty"`
	// WSPanelDir 工作区面板操作目录（用户需求 2026-12「工作区」入口：面板级
	// 目录记忆，与全局 Workspace 解耦——面板浏览/执行的目录不改动顶栏项目）。
	WSPanelDir string `json:"wsPanelDir,omitempty"`
	// MaxParallel 最大并行会话数（PC 端同时处理的主任务会话上限；默认 10）。
	// 超过上限的会话任务在调度器排队等待许可（不阻塞 UI，也不报错）；
	// 可在设置界面「通用 → 任务执行」调节，保存后立即生效。
	MaxParallel int `json:"maxParallel,omitempty"`

	// ---- 2026-10 能力补全配置（邮件/Notion/浏览器自动化/STT 复用）----
	// SMTPHost/SMTPPort/SMTPUser/SMTPPass/SMTPFrom：邮件发送（email_send 工具）；
	// NotionAPIKey：Notion 笔记工具；BrowserPath：浏览器自动化可执行文件路径
	//（空 = 自动探测 Edge/Chrome 默认安装位置）；STT 复用 SpeechBaseURL/SpeechModel/SpeechAPIKey；
	// BrowserDisabled 内置浏览器工具开关（设置面板可隐藏/显示 browser_* 组：
	// true 时 buildTools 不注册任何 browser_* 工具，模型不可见；desktop_* 不受
	// 影响，复用既有 xxxDisabled 惯例 CondenseDisabled/VerifyDisabled 同构）。
	SMTPHost        string `json:"smtpHost,omitempty"`
	SMTPPort        int    `json:"smtpPort,omitempty"`
	SMTPUser        string `json:"smtpUser,omitempty"`
	SMTPPass        string `json:"smtpPass,omitempty"`
	SMTPFrom        string `json:"smtpFrom,omitempty"`
	NotionAPIKey    string `json:"notionAPIKey,omitempty"`
	BrowserPath     string `json:"browserPath,omitempty"`
	BrowserDisabled bool   `json:"browserDisabled,omitempty"`
	// InternalBrowser 内嵌浏览器显示开关（2026-10 用户需求）：开启后
	// browser_* 工具经 WebView2 可见窗口执行（Agent 操作实时可见，登录态
	// 跨任务保留）；关闭 = 原 headless 行为。非 Windows 恒 headless。
	InternalBrowser bool `json:"internalBrowser,omitempty"`

	// ---- 对标增强（doc/10-对标增强）----
	// B1 任务检查点（缺省开；CheckpointDisabled=true 关闭）。
	CheckpointDisabled bool `json:"checkpointDisabled,omitempty"`
	// B2 任务运行日志导出（缺省开；TaskLogDisabled=true 关闭）。
	TaskLogDisabled bool `json:"taskLogDisabled,omitempty"`
	// B3 webhook 触发（缺省关；显式开启才监听 127.0.0.1:WebhookPort）。
	WebhookEnabled bool `json:"webhookEnabled,omitempty"`
	WebhookPort    int  `json:"webhookPort,omitempty"` // 0 = 默认 18923
	// C2 工具审批矩阵（对标 OpenCode permissions；settings.json 恒管工具
	// 固化在 ~/.kaelvoris/approvals.json，此处仅留 C1/C3 开关）。
	// C1 编辑后验证（对标 Aider auto-lint；缺省开，VerifyDisabled=true 关闭）。
	VerifyDisabled bool `json:"verifyDisabled,omitempty"`
	// C3 任务收尾 git 自动提交（对标 Aider auto-commits；缺省关——自动改
	// 用户 git 历史属高敏感行为，需显式开启）。
	GitAutocommitEnabled bool `json:"gitAutocommitEnabled,omitempty"`
	// 失焦系统通知总开关（规划板：cron 失败通知 + 回复完成通知共用通道；
	// 缺省关 = 通知开启，对齐 CondenseDisabled/CheckpointDisabled 的 "xxxDisabled" 惯例）。
	BlurNotifyDisabled bool `json:"blurNotifyDisabled,omitempty"`

	// ---- 对标增强第四批（E1 成本护栏 / E2 辅助模型分流 / E3 主模型失败降级）----
	// E1（对标 SWE-agent cost_limit / MSWEA_GLOBAL_COST_LIMIT）：单任务成本
	// 上限与全局日额（美元；0 = 不限，缺省零破坏）。
	CostLimitUSD       float64 `json:"costLimitUSD,omitempty"`
	GlobalCostLimitUSD float64 `json:"globalCostLimitUSD,omitempty"`
	// E2（对标 Plandex summarizer 角色）：辅助环节（摘要压缩/复盘/总结）
	// 独立模型；三项齐备才启用，任一缺失回落主模型。
	AuxBaseURL string `json:"auxBaseURL,omitempty"`
	AuxAPIKey  string `json:"auxAPIKey,omitempty"`
	AuxModel   string `json:"auxModel,omitempty"`
	// E3（对标 Plandex errorFallback）：主模型流式失败重试耗尽后自动降级的
	// 备用模型；三项齐备且与主模型不同时才启用。
	FallbackBaseURL string `json:"fallbackBaseURL,omitempty"`
	FallbackAPIKey  string `json:"fallbackAPIKey,omitempty"`
	FallbackModel   string `json:"fallbackModel,omitempty"`
	// ModelProfiles 每类模型的多条目配置（用户需求：同一类型模型支持添加
	// 多个 + 启停开关）。kind 取值 chat/image/video/speech；条目顺序即优先级，
	// 每类第一个未停用条目经 SaveConfig 投影到上方既有平铺字段——全部消费点
	//（llm.NewClient 主循环 / 生图视频语音 Settings 闭包 / STT / 远程投影）
	// 零改动即生效。平铺字段保留为投影只读面（旧配置文件/远程下发兼容）。
	ModelProfiles []ModelProfile `json:"modelProfiles,omitempty"`
}

// ModelProfile 单条模型配置条目（ModelProfiles 元素）。
// Disabled=true 时该条目停用（不参与投影）；条目全停用/无条目时该类回落
// 平铺字段原值（零破坏：删掉全部条目 = 回到单配置模式）。
// Kind 对应 Config 平铺字段族：chat→BaseURL/APIKey/Model；image→ImageGen*；
// video→VideoGen*（Mode/Size 取第一条未停用 chat/video 条目所属，仍全局单值）；
// speech→Speech*。
// ponytail: video 的 mode/size 仍全局单值（平铺字段），不随条目走——Agnes
// 参数与条目耦合的需求出现时再把 Mode/Size 下放进条目。
type ModelProfile struct {
	Kind     string `json:"kind"` // chat | image | video | speech
	Name     string `json:"name"` // 展示名（可空，空 = model 兜底）
	BaseURL  string `json:"baseURL"`
	APIKey   string `json:"apiKey,omitempty"`
	Model    string `json:"model"`
	Disabled bool   `json:"disabled,omitempty"`
}

// applyModelProfiles 把 modelProfiles 中每类第一个未停用条目投影到对应平铺
// 字段（就地修改 c）。投影规则：
//   - 条目全停用或该类无条目 → 平铺字段保持原值（回落既有单配置）。
//   - 条目字段为空串时对应对应平铺字段写空（显式清空语义；APIKey 空沿用
//     「不带认证头」惯例）。
func applyModelProfiles(c *Config) {
	pick := func(kind string) *ModelProfile {
		for i := range c.ModelProfiles {
			p := &c.ModelProfiles[i]
			if p.Kind == kind && !p.Disabled {
				return p
			}
		}
		return nil
	}
	if p := pick("chat"); p != nil {
		c.BaseURL, c.APIKey, c.Model = p.BaseURL, p.APIKey, p.Model
	}
	if p := pick("image"); p != nil {
		c.ImageGenBaseURL, c.ImageGenModel, c.ImageGenAPIKey = p.BaseURL, p.Model, p.APIKey
	}
	if p := pick("video"); p != nil {
		c.VideoGenBaseURL, c.VideoGenModel, c.VideoGenAPIKey = p.BaseURL, p.Model, p.APIKey
	}
	if p := pick("speech"); p != nil {
		c.SpeechBaseURL, c.SpeechModel, c.SpeechAPIKey = p.BaseURL, p.Model, p.APIKey
	}
}

// migrateFlatToProfiles 旧配置一次性迁移：无 modelProfiles 时把既有平铺字段
// 收敛为单条目（非空才收），使前端条目视图看到与旧单配置完全一致的内容。
// 只读判断不改盘——落盘发生在 SaveConfig。
func migrateFlatToProfiles(c *Config) {
	if len(c.ModelProfiles) > 0 {
		return
	}
	add := func(kind, baseURL, model, apiKey string) {
		if strings.TrimSpace(baseURL) == "" && strings.TrimSpace(model) == "" {
			return
		}
		c.ModelProfiles = append(c.ModelProfiles, ModelProfile{Kind: kind, BaseURL: baseURL, Model: model, APIKey: apiKey})
	}
	add("chat", c.BaseURL, c.Model, c.APIKey)
	add("image", c.ImageGenBaseURL, c.ImageGenModel, c.ImageGenAPIKey)
	add("video", c.VideoGenBaseURL, c.VideoGenModel, c.VideoGenAPIKey)
	add("speech", c.SpeechBaseURL, c.SpeechModel, c.SpeechAPIKey)
}

// condenseDefaultKeepRecent 摘要压缩保留最近 N 条原文（规划板 2026-10）。
// 引擎 CondenseKeepRecent 缺省值：保留最近 8 条消息原文（约 2~4 轮对话），
// 摘要仅覆盖更早历史——近期细节零损失。
const condenseDefaultKeepRecent = 8

// askReply ask 消息的用户应答。
type askReply struct {
	response message.AskResponse
	text     string
}

// queuedMsg 运行中会话排队的用户消息（本轮任务结束后自动续发）。
type queuedMsg struct {
	task   string
	images []string
}

// SkillView 技能列表条目（元数据 + 启用状态）。
type SkillView struct {
	skills.Metadata
	Disabled bool `json:"disabled"`
}

// MCPServer MCP 服务器配置（P2.6 仅配置持久化；P4 接入运行时）。
// Headers / AlwaysAllow 兼容 Cline / Claude Desktop 粘贴配置（streamable-http 认证头等），
// 持久化保留原文，运行时（sse/http 接入后）直接可用。
// Disabled 兼容字段（json:"disabled"，Claude/Cline 粘贴格式习惯）：数组格式下
// 若出现 disabled:true 则视为禁用——此前数组分支只认 enabled，手动编辑/跨工具
// 复制「disabled:true」会被静默忽略，`enabled:true` 残留导致已关闭的服务器仍被连接。
type MCPServer struct {
	Name        string            `json:"name"`
	Transport   string            `json:"transport"` // stdio | sse | http
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	URL         string            `json:"url,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	AlwaysAllow []string          `json:"alwaysAllow,omitempty"`
	Enabled     bool              `json:"enabled"`
	Disabled    bool              `json:"disabled,omitempty"` // 兼容读取（映射格式同义）；保存统一写 enabled
}

// SkillSettings 技能启用设置（持久化 ~/.kaelvoris/skills-settings.json）。
type SkillSettings struct {
	Disabled []string `json:"disabled"`
	// AIAutoCreate AI 分类开关：true = AI 分类时可自动新增分类（默认开）；
	// false = 仅归入既有分类，需要新建分类的技能保持未分类。
	AIAutoCreate bool `json:"aiAutoCreate,omitempty"`
}

// taskRun 一次运行中任务的完整运行态（多会话并行的核心隔离单元）。
// 对齐 Zoo-Code Task 实例：各自持有引擎 / 待办 / 事件历史 / 应答通道 / 取消函数。
type taskRun struct {
	id          string // = 会话 ID
	workspace   string // 会话关联工作区（会话隔离；空 = 全局配置工作区）
	employeeID  string // 绑定数字员工 ID（§9；空 = 普通会话）
	model       string // 会话自有模型（会话隔离；空 = 跟随全局配置）
	mcpDisabled bool   // 会话禁用 MCP（右侧会话设置面板；定义侧不连接 + 执行侧网关拒绝）
	engine      *agent.Engine
	todo        *tools.TodoStore
	// cleanup 任务结束时清理钩子（数字员工网关审计文件关闭等；后进先出）。
	cleanup func()
	histMu  sync.Mutex
	hist    []message.ClineMessage
	askCh   chan askReply
	cancel  context.CancelFunc
	aborted bool // 用户取消（对齐 Task.abort；废弃标记 abandoned 由 registry 语义预留）
	// pendingMsgs 运行中用户续发消息队列（不停止任务：本轮结束后自动继续）。
	// FIFO；每条含任务文本与图片（桌面端 SendMessageWithEmployee 入队）。
	pendingMsgs []queuedMsg
	// suppressFeedback 桌面端发送时前端已本地渲染用户消息（sendTask addMsg），
	// 引擎首条 user_feedback 回显不再实时推送（历史照记，回放/切回仍可见），
	// 否则聊天区同一句话显示两遍。
	suppressFeedback bool
	// feedbackSeen 首条 user_feedback 抑制是否已消费（仅 suppressFeedback=true
	// 会话使用；后续 user_feedback 如续期补充指令仍正常推送）。
	feedbackSeen bool
	// mcpProbe MCP 惰性加载（mcp_lazy.go）：当前轮任务文本，激活判定信号。
	// 各宿主在 buildRunEngine 前赋值（桌面 runTask/IM/飞书/企微/远程/群聊/并行子任务），
	// 空 = 仅按工作区提示词（AGENTS.md 等）判定。
	mcpProbe string
	// cronSkills 定时任务挂载的技能名列表（F4，对齐 Hermes --skill）：cron 会话构建
	// 时预置为可发现（available skills 段）。非 cron 会话为空（零破坏）。
	cronSkills []string
	// cronSession 是否 cron 触发的会话（F5，前缀 cron_）：true 时 buildRunEngine
	// 不注册 cron_job 工具——防 runaway 调度循环（对齐 Hermes 禁用 cron 管理工具）。
	cronSession bool
	// pendingAskLive 当前挂起中的 ask（用户需求 2026-12「不显示审批就卡住」修复）：
	// 引擎 waitAsk 发出 ask 时写入、收到应答时清除（nil = 无挂起）。
	// 供 sessionsWithRunning 提取随会话列表下发，前端切回会话/刷新后恢复审批卡。
	pendingAskMu   sync.Mutex
	pendingAskLive *message.ClineMessage
}

// TaskID 实现 task.Task。
func (r *taskRun) TaskID() string { return r.id }

// IsAborted 实现 task.Task。
func (r *taskRun) IsAborted() bool {
	r.histMu.Lock()
	defer r.histMu.Unlock()
	return r.aborted
}

// IsAbandoned 实现 task.Task。
func (r *taskRun) IsAbandoned() bool { return false }

// App 绑定对象。
type App struct {
	ctx context.Context

	cfg   Config
	cfgMu sync.Mutex

	store          *session.Store                // 会话持久化（内存缓存 + 异步刷盘）
	memory         *session.MemoryStore          // 会话记忆文档（MEMORY.md）
	wsMemory       *session.WorkspaceMemoryStore // 工作空间记忆（跨会话、按项目隔离；2026-10-31）
	currentSession string                        // 前端当前打开的会话 ID（仅影响回放路由）
	sessMu         sync.Mutex

	modeMgr  *modes.Manager
	skillMgr *skills.Manager
	kmu      sync.Mutex // 保护内核组件指针

	currentMode string
	modeMu      sync.Mutex

	// P2.7-7 多会话并行：TaskRegistry（LIFO 栈 + Map）+ TaskScheduler（并发门控）。
	registry   *task.Registry
	scheduler  *task.Scheduler
	runsMu     sync.Mutex
	runs       map[string]*taskRun // sessionID → 运行态
	maxConcurr int                 // 并发任务数（>1 启用 fan-out 并行）

	// P2.8 多 Agent 并行协作：new_task 委派计数（子会话 ID = 父ID-sN，前端侧栏
	// 以缩进呈现父子层级）。子任务直接在引擎 fan-out goroutine 中运行、不占用
	// 会话调度许可，因此父子/兄弟委派可真正并发且无许可互锁。
	subSeq   int
	subSeqMu sync.Mutex

	// D1 检查点恢复通知队列（会话 → 待注入通知；前端回滚发生在任务间隙，
	// run 不存在也记录，下次任务 runTask 启动时 drain 注入模型）。
	restoreMu sync.Mutex
	// mcpProbeText buildRunEngine 每轮从 run.mcpProbe 同步，多会话并行时各轮串行
	// 读自身 run，无需锁（App 级字段仅为 mcp_lazy.go 的 a.mcpWanted() 读取入口）。
	mcpProbeText string
	restoreNotes map[string][]string

	// P3 消息渠道运行时（channel_lark.go 飞书长连接桥 / channel_wecom.go
	// 企微回调桥 / channel_im.go 通用 IM 桥；配置保存后热重载）。
	chanMu        sync.Mutex
	larkChannels  []*LarkChannel
	wecomChannels []*WeComChannel
	imChannels    []*IMChannel

	// P-Remote 远程节点连接器（remote_panel.go：连接 kaelvoris-server；
	// 移动端/管理端发布任务经服务端派发到本节点执行）。
	remoteMu     sync.Mutex
	remoteCfg    RemoteConfig
	remoteClient *remote.Client
	remoteErr    string

	// 多开实例槽位（instance_slot.go：锁文件 + PID 存活检测）。桌面端支持
	// 多进程同开，槽位 1 沿用基础节点身份，槽位 N≥2 派生 "ID-N"/"名称 (N)"，
	// 同令牌多实例在服务端呈现为多个节点（不再互相顶替）。槽位生命周期 =
	// 进程生命周期（acquireSlot 认领 / shutdown→releaseSlot 释放）。
	instanceSlot int    // 当前槽位号（0=未分配）
	slotRelease  func() // 槽位锁释放函数（幂等）

	// P-Account 远程账号登录态（remote_account.go：统一账号体系 PC 端入口；
	// 仅远程开启时展示，登录凭据 ~/.kaelvoris/remote_account.json）。
	accMu          sync.Mutex
	accLoggedIn    bool
	accUser        string
	accDisplayName string
	accRole        string
	accError       string

	// P-Remote-Nodes 远程会话（remote_nodes.go：桌面端以客户端身份连接
	// 服务中心 /ws/client，发布任务到远端 PC 节点执行、事件流/审批/续聊实时渲染）。
	rnMu sync.Mutex
	rn   *rnState

	// 自学习闭环（selflearn.go：任务结束自动复盘 → 沉淀经验卡）。
	// 计数对齐参考项目 nudge_interval 语义；selfReviewing 保证同一时刻
	// 至多一个后台复盘 LLM 调用。
	selfReviewMu    sync.Mutex
	selfReviewCount int  // 已完成任务轮次计数（跨会话累计）
	selfReviewing   bool // 复盘在跑（防并发 LLM 调用）

	// 定时任务（cron_panel.go：应用内调度器，到期经 SendMessage 触发会话任务）。
	cronStore *cron.Store
	cronSched *cron.Scheduler

	// 对标增强 B3（webhook_panel.go）：webhook 触发服务与存储。
	hookMu       sync.Mutex
	hookStoreRef *webhook.Store
	hookLogRef   *webhook.Log
	hookSrv      *webhook.Server

	// promptStore 提示词配置（~/.kaelvoris/prompts.json）：system/summarize/\
	// taskSummary/selfReview/hourlyReview/selfCheckTask 等段为空时使用内置默认。
	promptStore *promptconfig.Store

	// 自主迭代规划板（roadmap_panel.go：规划→审核→逐条开发→复盘闭环）。
	roadmapStore     *roadmap.Store
	roadmapAutoDev   roadmap.AutoDevState // 自动迭代运行态（含每日额度计数）
	roadmapAutoDevMu sync.Mutex           // 保护 Running 标志
	roadmapStopCh    chan struct{}        // 自动循环停止信号（shutdown 关闭）
	roadmapWakeCh    chan struct{}        // 非阻塞唤醒（容量 1：接力/配置变更即时唤醒）
	roadmapDevMu     sync.Mutex           // 开发调度临界区（claimNextApproved 原子认领用）

	// version & upgrade panel state (version_panel.go: check / apply / notify).
	updateMu  sync.Mutex
	lastCheck *version.CheckResult

	// 数字员工（employee_panel.go：CRUD/权限网关/进化提案审批）。
	empStore *employee.Store
	// 组织架构主数据（orgstore.go：部门/岗位/角色字典，EHR 人员与组织管控）。
	orgStore *employee.OrgStore
	// A2A 群聊节点侧存储（remote_group.go；groupMu 保护惰性初始化）。
	groupMu sync.Mutex
	groupSt *groupStore
	// 自进化经验卡（evolve_tools.go：定时自检沉淀 + 提示词命中注入）。
	evolveStore *evolve.Store
	// 向量知识库（vector_panel.go：索引运行态 + 定时调度 + codebase_search 工具）。
	vec *vectorPanel
	// 嵌入式 Coze 工作流内核（cozeflow_panel.go：惰性 Bootstrap，失败缓存错误）。
	coze    *cozeflow.Coze
	cozeMu  sync.Mutex
	cozeErr string
}

// NewApp 创建绑定对象并加载配置。
func NewApp() *App {
	a := &App{
		currentMode: modes.DefaultModeSlug,
		registry:    task.NewRegistry(),
		runs:        map[string]*taskRun{},
	}
	a.cfg = a.loadConfig()
	// 调度器并发上限读自配置（默认 10；可在设置界面调节，保存后 SetMax 生效）。
	a.scheduler = task.NewScheduler(a.cfg.MaxParallel)
	a.maxConcurr = a.cfg.MaxParallel
	a.store = session.NewStore(sessionsDir())
	a.memory = session.NewMemoryStore(sessionsDir())
	a.wsMemory = session.NewWorkspaceMemoryStore(workspaceMemoryDir())
	// 定时任务存储与调度器（到期触发复用 SendMessage 执行链）。
	cs, err := cron.NewStore(cronDir())
	if err != nil {
		println("warn: cron store init:", err.Error())
		cs, _ = cron.NewStore(tempCronDir())
	}
	a.cronStore = cs
	a.cronSched = cron.NewScheduler(cs, a.cronRunner)
	a.cronSched.SetFlowLauncher(a.cronRunFlow) // run_flow 动作分发（cron→flow 联动）
	// 数字员工存储（失败退临时目录，不阻断启动）。
	emp, err := employee.NewStore(employeeDir())
	if err != nil {
		println("warn: employee store init:", err.Error())
		emp, _ = employee.NewStore(tempEmployeeDir())
	}
	a.empStore = emp
	a.promptStore = promptconfig.New(promptconfig.DefaultPath(homeDir()))
	// 组织架构主数据存储（失败退临时目录，不阻断启动）。
	org, err := employee.NewOrgStore(employeeDir())
	if err != nil {
		println("warn: org store init:", err.Error())
		org, _ = employee.NewOrgStore(tempEmployeeDir())
	}
	a.orgStore = org
	// 删除校验共享员工快照（零 IO）：orgStore 引用校验从读盘降为内存遍历。
	a.orgStore.BindEmployeeRef(a.empStore.List)
	// 自进化经验卡存储（失败退临时目录，不阻断启动）。
	ev, err := evolve.NewStore(evolveDir())
	if err != nil {
		println("warn: evolve store init:", err.Error())
		ev, _ = evolve.NewStore(tempEvolveDir())
	}
	a.evolveStore = ev
	// 自主迭代规划板存储（失败退临时目录，不阻断启动）。
	rm, err := roadmap.NewStore(roadmapDir())
	if err != nil {
		println("warn: roadmap store init:", err.Error())
		rm, _ = roadmap.NewStore(tempRoadmapDir())
	}
	a.roadmapStore = rm
	a.roadmapStopCh = make(chan struct{})
	a.roadmapWakeCh = make(chan struct{}, 1)
	// 向量知识库运行态（存储目录懒建；调度循环在 startup 启动）。
	a.vec = newVectorPanel()
	return a
}

// employeeDir 数字员工数据根目录（~/.kaelvoris/employees）。
func employeeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "employees")
}

// tempEmployeeDir 兜底目录（用户目录不可写时退回临时目录）。
func tempEmployeeDir() string {
	return filepath.Join(os.TempDir(), "kaelvoris-employees")
}

// cronDir 定时任务持久化目录（~/.kaelvoris/cron）。
func cronDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "cron")
}

// tempCronDir 兜底目录（用户目录不可写时退回临时目录）。
func tempCronDir() string {
	return filepath.Join(os.TempDir(), "kaelvoris-cron")
}

// evolveDir 自进化经验卡目录（~/.kaelvoris/evolve）。
func evolveDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "evolve")
}

// checkpointDir 任务检查点根目录（~/.kaelvoris/checkpoints；B1 对标增强）。
func checkpointDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "checkpoints")
}

// tasklogDir 任务运行日志目录（~/.kaelvoris/tasklogs；B2 对标增强）。
func tasklogDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "tasklogs")
}

// approvalsPath 工具审批矩阵持久化文件（~/.kaelvoris/approvals.json；C2 对标增强）。
func approvalsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "approvals.json")
}

// verifyPath 编辑后验证规则文件（~/.kaelvoris/verify.json；C1 对标增强）。
func verifyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "verify.json")
}

// hooksTriggerFile webhook 触发器定义文件（~/.kaelvoris/hooks_trigger.json；B3）。
func hooksTriggerPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "hooks_trigger.json")
}

// hooksTriggerLogFile webhook 触发记录文件（jsonl 最近 200 行）。
func hooksTriggerLogPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "hooks_trigger.log.jsonl")
}

// tempEvolveDir 兜底目录。
func tempEvolveDir() string {
	return filepath.Join(os.TempDir(), "kaelvoris-evolve")
}

// roadmapRecoverOrphans 恢复重启前遗留的 developing 条目：上次进程退出时
// 开发会话已死，置回 approved 让队列重新消费（历史会话保留可回溯）。
func (a *App) roadmapRecoverOrphans() {
	// RequeueDeveloping 直接改写状态（developing→approved 在 CanTransition 中非法，
	// 旧 SetStatus 路径恢复永远失败 → 僵尸条目 + 队列恒空；2026-10 修复）。
	recovered := a.roadmapStore.RequeueDeveloping()
	if recovered > 0 {
		slog.Info("roadmap 恢复中断条目", "count", recovered)
	}
}

// ---------- 会话持久化 ----------

// homeDir 用户主目录（提示词配置等 Home 下的存储统一入口）。
func homeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

func sessionsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "sessions")
}

// workspaceMemoryDir 工作空间记忆根目录（~/.kaelvoris/workspace-memory；
// 与 sessionsDir 同级惯例，路径不入库）。
func workspaceMemoryDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "workspace-memory")
}

// ListSessions 会话列表（按更新时间倒序）。
func (a *App) ListSessions() []session.Summary {
	return a.sessionsWithRunning()
}

// sessionsWithRunning 会话列表 + 运行态回填（用户需求 2026-10：侧栏绿点）。
// Summary 由持久化层生成（不含运行态），此处按 runs 注册表补 Running 标记，
// 前端 kaelvoris:sessions 推送与 ListSessions 拉取两条路径数据形态一致。
// ParentID 推导兑底：ParentID 字段引入前的历史子会话（ID 形态 <父ID>-s<N>）
// 按后缀规则反推，保证旧存档在侧栏折叠树中同样归入父会话层级。
func (a *App) sessionsWithRunning() []session.Summary {
	list := a.store.List()
	a.runsMu.Lock()
	defer a.runsMu.Unlock()
	for i := range list {
		run, ok := a.runs[list[i].ID]
		if ok {
			list[i].Running = true
			// 挂起审批卡回填（用户需求 2026-12「不显示审批就卡住」修复）：
			// taskRun.pendingAskLive 由 ask 桥在引擎阻塞等待期间写入、应答时清除，
			// 非空即当前正阻塞等人的 ask——随列表下发，前端据此恢复审批卡。
			run.pendingAskMu.Lock()
			if run.pendingAskLive != nil {
				pa := *run.pendingAskLive
				list[i].PendingAsk = &pa
			}
			run.pendingAskMu.Unlock()
		}
		if list[i].ParentID == "" {
			list[i].ParentID = deriveParentID(list, list[i].ID)
		}
	}
	return list
}

// deriveParentID 旧子会话父 ID 推导（升级兼容）：new_task 委派子会话 ID 形态
// 固定为 <父ID>-s<N>（runParallelSubtask），按后缀规则反推父 ID；父会话必须
// 真实存在于列表中（防普通会话 ID 恰含 -sN 段被误判）。
func deriveParentID(list []session.Summary, id string) string {
	i := strings.LastIndex(id, "-s")
	if i <= 0 {
		return ""
	}
	suffix := id[i+2:]
	if suffix == "" {
		return ""
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return "" // 非 s<数字> 后缀不误判
		}
	}
	parent := id[:i]
	for _, s := range list {
		if s.ID == parent {
			return parent
		}
	}
	return ""
}

// OpenSession 读取会话详情（前端回放消息）。
// P3.1-C：同时返回记忆文档（result.Memory 标记会话目标/进度/文档指针）。
func (a *App) OpenSession(id string) (map[string]any, error) {
	s, err := a.store.Get(id)
	if err != nil {
		return nil, fmt.Errorf("打开会话失败: %w", err)
	}
	a.sessMu.Lock()
	a.currentSession = id
	a.sessMu.Unlock()
	snap := s.Snapshot()
	return map[string]any{
		"id":          snap.ID,
		"title":       snap.Title,
		"mode":        snap.Mode,
		"model":       snap.Model,       // 会话自有模型（Composer 选择器回显；空 = 跟随全局）
		"nodeId":      snap.NodeID,      // 会话自有执行节点（远程会话发布目标；空 = 本机）
		"workspace":   snap.Workspace,   // 会话关联工作区（前端联动切换项目）
		"employeeId":  snap.EmployeeID,  // 数字员工绑定（Composer 选择器回显）
		"acpAgent":    snap.ACPAgent,    // ACP agent 绑定（Composer 选择器回显）
		"mcpDisabled": snap.MCPDisabled, // 会话禁用 MCP（右侧会话设置面板开关回显）
		"messages":    snap.Messages,
		"todos":       snap.Todos,
		"memory":      a.memory.Load(id),
		"memoryPath":  a.memory.Path(id),
		"createdAt":   snap.CreatedAt,
		"updatedAt":   snap.UpdatedAt,
		// 全局当前模式/模型随 meta 一并返回（前端单次 IPC 回填选择器，免再调 GetMode/GetConfig）。
		"globalMode":  a.GetMode(),
		"globalModel": a.GetConfig().Model,
	}, nil
}

// DeleteSession 删除会话。
func (a *App) DeleteSession(id string) error {
	return a.store.Delete(id)
}

// SetSessionWorkspace 会话独立设置工作区（右键会话卡）：校验目录存在后写入
// 会话记录并广播刷新。下一任务起 buildRunEngine 按会话工作区运行，多会话
// 可并行操作各自项目目录；不动全局当前工作区。path 为空 = 清除绑定，
// 恢复「跟随全局」（下轮任务 runTask 会自动补记当时全局工作区）。
func (a *App) SetSessionWorkspace(id, path string) error {
	id = strings.TrimSpace(id)
	p := strings.TrimSpace(path)
	if id == "" {
		return fmt.Errorf("会话不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return fmt.Errorf("会话不存在")
	}
	// 运行中会话不动（本轮已按旧工作区运行，改了也不生效且易误导）。
	if a.IsSessionRunning(id) {
		return fmt.Errorf("任务运行中，请等待完成或停止后再设置")
	}
	if p == "" {
		s.SetWorkspace("")
		a.store.Save(s)
		a.pushSessionsChanged()
		return nil
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return fmt.Errorf("目录不存在: %s", p)
	}
	s.SetWorkspace(p)
	a.store.Save(s)
	a.pushSessionsChanged()
	// 会话处于打开状态：联动切换项目（与打开会话联动语义一致）。
	a.sessMu.Lock()
	opened := a.currentSession == id
	a.sessMu.Unlock()
	if opened && a.cfgWorkspace() != p {
		// 打开中的会话：联动切换全局工作区（SwitchProject 内部会写入
		// RecentProjects，下拉列表同步出现该项目）。
		_ = a.SwitchProject(p)
	}
	return nil
}

// SetSessionModel 绑定/解绑会话自有模型（Composer 模型选择器；model 空 =
// 恢复跟随全局配置）。运行中会话拒绝变更（本轮已按旧配置运行）。
func (a *App) SetSessionModel(id, model string) error {
	id = strings.TrimSpace(id)
	m := strings.TrimSpace(model)
	if id == "" {
		return fmt.Errorf("会话不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return fmt.Errorf("会话不存在")
	}
	if a.IsSessionRunning(id) {
		return fmt.Errorf("任务运行中，请等待完成或停止后再切换")
	}
	s.SetModel(m)
	a.store.Save(s)
	a.pushSessionsChanged()
	return nil
}

// SetSessionNodeID 绑定会话自有远程执行节点（Composer 节点选择器）。
// 写入会话记录持久化，切回联动恢复、重启不丢；空 = 本机执行，"auto" = 服务端自动选在线节点。
// 运行中会话拒绝变更（本轮已按旧节点运行，改了不生效且易误导）。
func (a *App) SetSessionNodeID(id, nodeID string) error {
	id = strings.TrimSpace(id)
	n := strings.TrimSpace(nodeID)
	if id == "" {
		return fmt.Errorf("会话不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return fmt.Errorf("会话不存在")
	}
	if a.IsSessionRunning(id) {
		return fmt.Errorf("任务运行中，请等待完成或停止后再切换")
	}
	s.SetNodeID(n)
	a.store.Save(s)
	a.pushSessionsChanged()
	return nil
}

// SetSessionMode 绑定会话自有模式（执行策略；Composer 模式选择器）。
// 写入会话记录后，本轮及后续任务 runTask 优先按会话模式执行；同时
// 同步全局当前模式（创建中的新任务即刻生效）。
func (a *App) SetSessionMode(id, slug string) error {
	id = strings.TrimSpace(id)
	slug = strings.TrimSpace(slug)
	if id == "" {
		return fmt.Errorf("会话不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return fmt.Errorf("会话不存在")
	}
	if slug != "" {
		found := false
		for _, m := range a.ListModes() {
			if m.Name == slug {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("模式不存在: %s", slug)
		}
	}
	s.SetMode(slug)
	if slug != "" {
		a.SetMode(slug) // 同步全局当前模式（新任务即刻生效）
	}
	a.store.Save(s)
	a.pushSessionsChanged()
	return nil
}

// SetSessionACP 绑定/解绑会话的 ACP agent（Composer agent 选择器；
// name 为空 = 恢复内置引擎）。运行中会话拒绝变更（本轮已按旧配置运行）。
func (a *App) SetSessionACP(id, agentName string) error {
	id = strings.TrimSpace(id)
	name := strings.TrimSpace(agentName)
	if id == "" {
		return fmt.Errorf("会话不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return fmt.Errorf("会话不存在")
	}
	if a.IsSessionRunning(id) {
		return fmt.Errorf("任务运行中，请等待完成或停止后再切换")
	}
	if name != "" {
		found := false
		for _, ag := range a.ListACPAgents() {
			if ag.Name == name && ag.Enabled {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("ACP agent 不存在或未启用: %s", name)
		}
	}
	s.SetACPAgent(name)
	a.store.Save(s)
	a.pushSessionsChanged()
	return nil
}

// SetSessionEmployee 绑定/解绑会话的数字员工（Composer 员工选择器；
// empID 空 = 恢复普通会话）。运行中会话拒绝变更（本轮已按旧配置运行）。
// §9.1 语义约束：仅「新会话/未运行」时可绑；已有任务记录的会话不改写历史绑定。
func (a *App) SetSessionEmployee(id, employeeID string) error {
	id = strings.TrimSpace(id)
	empID := strings.TrimSpace(employeeID)
	if id == "" {
		return fmt.Errorf("会话不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return fmt.Errorf("会话不存在")
	}
	if a.IsSessionRunning(id) {
		return fmt.Errorf("任务运行中，请等待完成或停止后再切换")
	}
	if empID != "" {
		if _, ok := a.empStore.Get(empID); !ok {
			return fmt.Errorf("数字员工不存在: %s", empID)
		}
	}
	s.SetEmployeeID(empID)
	a.store.Save(s)
	a.pushSessionsChanged()
	return nil
}

// SetSessionMCPDisabled 会话级禁用/启用 MCP（右侧会话设置面板开关；
// disabled=true = 本会话不连接不注册任何 mcp_* 工具）。运行中会话拒绝变更
// （本轮已按旧配置运行）。随会话持久化，重启不丢。
func (a *App) SetSessionMCPDisabled(id string, disabled bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("会话不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return fmt.Errorf("会话不存在")
	}
	if a.IsSessionRunning(id) {
		return fmt.Errorf("任务运行中，请等待完成或停止后再设置")
	}
	s.SetMCPDisabled(disabled)
	a.store.Save(s)
	a.pushSessionsChanged()
	return nil
}

// mcpDenyHook 会话级 MCP 禁用的执行侧兑底钩子（HookPreToolUse 签名）：
// disabled=true 时拒绝所有 mcp_* 调用（定义侧 buildMCPTools 已不连接不注册，
// 此处拦截残留调用，防旧配置/竞态）；其余工具透传给已有钩子（如员工网关），
// 不越界拦截。disabled=false 时原样返回 pre（不改变行为）。
func mcpDenyHook(pre func(name, args string) (bool, string, string), disabled bool) func(name, args string) (bool, string, string) {
	if !disabled {
		return pre
	}
	return func(name, args string) (bool, string, string) {
		if strings.HasPrefix(name, "mcp_") {
			return false, "", "本会话已禁用 MCP：工具 " + name + " 不可调用。如确需 MCP 能力，请在右侧会话设置中重新开启。"
		}
		if pre != nil {
			return pre(name, args)
		}
		return true, "", ""
	}
}

// RenameSession 会话重命名（会话卡右键改名）：
//  1. 本地：写入手动名称并置 ManualTitle —— 此后自动命名（任务首行等）
//     不再覆盖，名称在后续任务/续聊中保持不变（用户要求「交货时不变更」）；
//  2. 前端：EventsEmit kaelvoris:sessions 即时刷新侧栏与聊天页标题；
//  3. 服务端：远程连接在线时上行 rename.session，服务端把名称写入该会话
//     全部任务记录的 Task.Title 并广播，移动端/管理端同步显示手动名称。
//     未连接时仅本地生效（服务端记录在下次改名或新任务时自然对齐，
//     本地名称始终以手动命名为权威）。
func (a *App) RenameSession(id, title string) error {
	id = strings.TrimSpace(id)
	title = strings.TrimSpace(title)
	if id == "" {
		return fmt.Errorf("会话不能为空")
	}
	if title == "" {
		return fmt.Errorf("名称不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return fmt.Errorf("会话不存在")
	}
	s.Rename(title)
	a.store.Save(s)
	a.pushSessionsChanged()
	// 同步服务端（移动端/管理端）：连接在线才上行；不在线不阻塞本地改名。
	st := a.ensureRn()
	if st.isConnected() {
		st.send("", remote.TypeRenameSession, remote.RenameSession{SessionID: id, Title: title})
	}
	return nil
}

// SetSessionPinned 会话置顶/取消置顶（会话卡右键菜单；规划板 2026-10 会话卡片
// 信息升级）：写会话 PinnedAt 并广播刷新，列表排序置顶优先（Store.List）。
// 远程在线时上行 pin.session 同步服务端（移动端长按菜单同链路同语义）。
func (a *App) SetSessionPinned(id string, pinned bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("会话不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return fmt.Errorf("会话不存在")
	}
	s.SetPinned(pinned)
	a.store.Save(s)
	a.pushSessionsChanged()
	// 同步服务端（移动端/管理端同链路）：不在线不阻塞本地置顶。
	st := a.ensureRn()
	if st.isConnected() {
		st.send("", remote.TypePinSession, remote.PinSession{SessionID: id, Pinned: pinned})
	}
	return nil
}

// SetSessionManualPreview 会话手动摘要（提示词 manual_preview；规划板 2026-10
// 会话卡片信息升级）：非空时列表卡预览行用它覆盖自动提取（最后一条消息摘要），
// 便于用户给会话标注「重点/上下文」；空串 = 恢复自动提取。仅本地生效
// （预览行是桌面端阅读增强，不入服务端协议；服务端列表仍用任务首行标题）。
func (a *App) SetSessionManualPreview(id, preview string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("会话不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return fmt.Errorf("会话不存在")
	}
	s.SetManualPreview(preview)
	a.store.Save(s)
	a.pushSessionsChanged()
	return nil
}

// Startup Wails 生命周期：保存运行时上下文。
// （导出：根包 main.go 经 OnStartup 引用；internal/desktop 迁包后需跨包可见）
func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	uiEmit(ctx, "kaelvoris:ready")
	// upgrade loop: first-run notice after a successful self-upgrade.
	if notice := pendingUpgradeNotice(); notice != nil {
		uiEmit(ctx, "kaelvoris:upgraded", notice)
	}
	// 消息渠道后台接入（飞书长连接；goroutine 非阻塞，状态经 kaelvoris:channels 推送）。
	a.startChannels()
	// P-Remote 远程节点连接器（配置开启时自动连接服务中心，状态经
	// kaelvoris:remote_status 推送；移动端/管理端任务经服务端派发到本节点）。
	a.remoteCfg = a.loadRemoteConfig()
	go a.startRemoteClient()
	// P-Account 账号自动重登（有已保存凭据且远程开启时；状态经
	// kaelvoris:remote_account 推送，顶栏账号入口渲染数据源）。
	go a.autoReloginAccount()
	// 定时任务调度器后台启动（分钟级 tick；到期任务经 SendMessage 送入执行链）。
	a.cronSched.Start()
	// 自主迭代：配置开启时启动自动循环（完全自主模式）；否则仅在面板
	// 手动触发。恢复重启前遗留的 developing 条目（标记 failed 可重试）。
	a.roadmapRecoverOrphans()
	if c := a.GetConfig(); c.AutoDev != nil && c.AutoDev.Enabled {
		a.roadmapAutoDevStart()
	}
	// 定时向量化调度循环（分钟级 tick；配置开关开启且设置了自动化时间时，
	// 到期自动解析待索引目录并增量向量化——见 vector_panel.go）。
	a.vectorStartScheduler()
	// 对标增强 B3：webhook 触发监听（配置显式开启才监听 127.0.0.1，缺省关）。
	a.startWebhookServer()
	// 嵌入式 Coze 工作流内核后台预热（首次装配含资源释放 + DDL + 模型探测，
	// 耗时秒级；后台执行不阻塞启动，AI Flow 面板首次打开时通常已就绪）。
	go func() {
		if st := a.CozeInit(); st.Error != "" {
			println("warn: cozeflow bootstrap:", st.Error)
		}
	}()
	// update source auto check (when configured; emits kaelvoris:update_available).
	if cfg := loadUpdateConfig(); cfg.Source != "" && (cfg.AutoCheck == nil || *cfg.AutoCheck) {
		go a.checkUpdateAndNotify()
	}
}

// ---------- 配置 ----------

// ConfigPath desktop.json 配置文件路径（导出：无头入口 main_headless.go 命令行覆写用）。
func ConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "desktop.json")
}

// configPath 包内便捷别名（历史调用点保持不动）。
func configPath() string { return ConfigPath() }

// DefaultConfig 缺省配置（导出：无头入口缺省值回退用）。
func DefaultConfig() Config { return defaultConfig() }

func defaultConfig() Config {
	// ContextWindow 默认 200000：上下文治理默认开启（此前默认 0 = 关闭，
	// 长任务 prompt 膨胀到后端窗口上限，completion 空间被挤到十几 token，
	// 输出截断 → 残缺 tool arguments → 严格后端 400 Unterminated string）。
	// Sandbox 默认 job（纯杀树保证，limits 全 0 = 不限资源）。
	return Config{
		BaseURL: "https://api.deepseek.com", Model: "deepseek-chat",
		ContextWindow: 200000, AutoCondensePercent: 90, MaxParallel: 10,
		Sandbox: &tools.SandboxConfig{Mode: tools.SandboxModeJob},
	}
}

func (a *App) loadConfig() Config {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return defaultConfig()
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return defaultConfig()
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = 10 // 旧配置无此字段时兑底默认 10
	}
	// 沙箱兑底（2026-10 行为升级，有意为之）：旧配置无 sandbox 字段 →
	// 默认 job 模式（纯杀树保证：超时/结束时整树终止，修复孙进程泄漏；
	// limits 全 0 = 不施加资源约束，行为面最小化）。用户可经设置面板
	// 或 sandbox.mode="off" 关闭。
	if c.Sandbox == nil {
		c.Sandbox = &tools.SandboxConfig{Mode: tools.SandboxModeJob}
	}
	if c.Sandbox.Mode != tools.SandboxModeOff && c.Sandbox.Mode != tools.SandboxModeJob {
		c.Sandbox.Mode = tools.SandboxModeJob // 未知值按 job（默认开）
	}
	// 旧配置一次性收敛为单条目（modelProfiles 空 → 平铺字段成一条）；
	// 投影幂等（profiles 存在时平铺字段本就是投影结果，双向一致）。
	migrateFlatToProfiles(&c)
	applyModelProfiles(&c)
	return c
}

// GetConfig 读取配置（前端设置页）。
func (a *App) GetConfig() Config {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return a.cfg
}

// SaveConfig 保存配置并标记内核待重建（下一任务生效）。
func (a *App) SaveConfig(c Config) error {
	c.Workspace = strings.TrimSpace(c.Workspace)
	// 条目化模型配置：先裁剪条目（kind 合法/关键字段 TrimSpace），再投影到
	// 平铺字段（每类第一个未停用条目）。平铺字段从投影结果取值，前端不再
	// 需要同步两份；远程 SetConfig 对平铺字段的显式下发走回写条目（见
	// remote_panel.go），不与本投影冲突。
	kinds := map[string]bool{"chat": true, "image": true, "video": true, "speech": true}
	trimmed := make([]ModelProfile, 0, len(c.ModelProfiles))
	for _, p := range c.ModelProfiles {
		if !kinds[p.Kind] {
			continue
		}
		p.Name = strings.TrimSpace(p.Name)
		p.BaseURL = strings.TrimSpace(p.BaseURL)
		p.APIKey = strings.TrimSpace(p.APIKey)
		p.Model = strings.TrimSpace(p.Model)
		trimmed = append(trimmed, p)
	}
	c.ModelProfiles = trimmed
	applyModelProfiles(&c)
	// 投影后的基础校验：chat 类必须存在有效端点（与旧规则一致；条目全停用
	// 时回落旧平铺值，此处统一按平铺字段判定）。
	if c.BaseURL == "" {
		return errors.New("Base URL 不能为空")
	}
	// 并行会话数夹取到 [1,64]，防止前端异常输入导致调度器退化/失控。
	if c.MaxParallel < 1 {
		c.MaxParallel = 1
	}
	if c.MaxParallel > 64 {
		c.MaxParallel = 64
	}
	a.cfgMu.Lock()
	prevMax := a.cfg.MaxParallel
	a.cfg = c
	a.cfgMu.Unlock()
	if c.MaxParallel != prevMax {
		// 动态调整调度器并发上限（立即生效：升高唤醒排队任务；降低不中断运行中任务）。
		a.scheduler.SetMax(c.MaxParallel)
		a.maxConcurr = c.MaxParallel
	}
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath(), b, 0o600); err != nil {
		return err
	}
	a.invalidate()
	return nil
}

// ---------- 提示词配置（~/.kaelvoris/prompts.json）----------

// GetPromptConfig 读取全部提示词配置项（前端设置页「提示词」Tab）。
// 空值表示未覆盖，引擎使用内置默认提示词。
func (a *App) GetPromptConfig() promptconfig.Config {
	return a.promptStore.All()
}

// SavePromptConfig 保存提示词配置（整体覆盖；原子写）。
func (a *App) SavePromptConfig(c promptconfig.Config) error {
	return a.promptStore.Save(c)
}

// PromptDefault 单段内置默认提示词（设置面板「提示词」页展示 + 一键填入编辑）。
type PromptDefault struct {
	Key     string `json:"key"`
	Default string `json:"default"`
}

// GetPromptDefaults 返回各提示词段的内置默认文本（用户配置为空时引擎实际使用的值）。
// 逐段复用引擎真实的默认提示词来源（单一事实源，改内置提示词此处自动同步）：
//
//	system         → agent.SystemPromptTemplate 原版模板（变量以 {$CWD} 等占位符
//	                 呈现、不注入本机 AGENTS.md/规则文件/git 快照；用户基于模板
//	                 自定义保存后，运行时经 agent.ExpandPromptVars 还原占位符）
//	summarize      → condense.DefaultSummarizePrompt
//	taskSummary    → taskSummaryPrompt(空覆盖)
//	selfReview     → selfReviewPrompt(空覆盖)
//	hourlyReview   → hourlyReviewPrompt(空覆盖)
//	roadmapReview  → 无内置默认（补充判据段，空 = 不注入），返回说明文本
//	selfCheckTask  → selfReviewTaskPrompt
//	roadmapSystem  → roadmapSystemPromptDefault（自主开发会话守则，前置注入）
func (a *App) GetPromptDefaults() []PromptDefault {
	return []PromptDefault{
		{Key: "system", Default: agent.SystemPromptTemplate()},
		{Key: "summarize", Default: condense.DefaultSummarizePrompt},
		{Key: "taskSummary", Default: taskSummaryPrompt("", "")},
		{Key: "selfReview", Default: selfReviewPrompt(nil, "")},
		{Key: "hourlyReview", Default: hourlyReviewPrompt(nil, "")},
		{Key: "roadmapReview", Default: "（本段无内置默认。内置审核准则始终生效，本段文本作为最高优先级补充判据追加其后；留空 = 不注入补充判据。）"},
		{Key: "selfCheckTask", Default: selfReviewTaskPrompt},
		{Key: "roadmapSystem", Default: roadmapSystemPromptDefault},
		{Key: "autonomousDev", Default: agent.AutonomousDevSectionDefault},
	}
}

// reviewExtraPrompt 规划板 AI 审核补充判据：用户显式指定优先，空时回退
// promptconfig「提示词」设置（roadmapReview 段），再空 = 无补充判据。
func (a *App) reviewExtraPrompt(user string) string {
	if s := strings.TrimSpace(user); s != "" {
		return s
	}
	return strings.TrimSpace(a.promptStore.Get("roadmapReview"))
}

// invalidate 标记内核需按新配置重建（下一任务生效）。
// P2.7-7 多会话化后内核仅存模式/技能管理器；引擎按任务懒建，无需置空。
func (a *App) invalidate() {
	a.kmu.Lock()
	a.modeMgr = nil
	a.skillMgr = nil
	a.kmu.Unlock()
}

// ---------- 绑定：项目切换 ----------

// ListProjects 返回最近项目清单（当前工作区置顶；含标记）。
func (a *App) ListProjects() []ProjectEntry {
	a.cfgMu.Lock()
	cur := a.cfg.Workspace
	a.cfgMu.Unlock()
	seen := map[string]bool{}
	var out []ProjectEntry
	if cur != "" {
		out = append(out, ProjectEntry{Path: cur, Name: filepath.Base(cur), Current: true})
		seen[cur] = true
	}
	a.cfgMu.Lock()
	for _, p := range a.cfg.RecentProjects {
		if p == "" || seen[p] {
			continue
		}
		out = append(out, ProjectEntry{Path: p, Name: filepath.Base(p)})
		seen[p] = true
	}
	a.cfgMu.Unlock()
	return out
}

// SwitchProject 顶栏切换工作区：更新配置、重建内核组件、通知前端。
// 返回错误时配置不变。
func (a *App) SwitchProject(path string) error {
	p := strings.TrimSpace(path)
	if p == "" {
		return errors.New("项目路径不能为空")
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return fmt.Errorf("目录不存在: %s", p)
	}
	a.cfgMu.Lock()
	cfg := a.cfg
	cfg.Workspace = p
	cfg.RecentProjects = pushRecent(cfg.RecentProjects, p, 8)
	a.cfg = cfg
	a.cfgMu.Unlock()
	if err := saveConfigFile(cfg); err != nil {
		return err
	}
	// 内核重建（模式/技能管理器随新工作区发现）。
	a.invalidate()
	// 通知前端刷新（项目名 / git 状态 / 侧栏）。
	a.emitEvent("kaelvoris:project-changed", p)
	return nil
}

// OpenProject 打开目录选择对话框并切换为当前工作区（顶栏「打开目录」）。
func (a *App) OpenProject() (string, error) {
	dir, err := a.PickWorkspace()
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", nil // 用户取消
	}
	if err := a.SwitchProject(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// OpenProjectInEditor 一键打开当前项目（顶栏项目按钮右侧按钮）。
// editor: "zed" = 在项目目录执行 `zed .`（Config.EditorPreference="zed"）；
// 其他值/空 = 系统文件管理器。错误可读返回（zed 未安装等），前端 toast 展示。
func (a *App) OpenProjectInEditor(editor string) error {
	ws := a.cfgWorkspace()
	if strings.TrimSpace(ws) == "" {
		return errors.New("尚未打开项目（请先在顶栏选择工作区）")
	}
	if st, err := os.Stat(ws); err != nil || !st.IsDir() {
		return fmt.Errorf("项目目录不存在: %s", ws)
	}
	if strings.TrimSpace(strings.ToLower(editor)) == "zed" {
		return openInZed(ws)
	}
	return openInExplorer(ws)
}

// OpenFileAtLine 用系统默认关联程序打开 workspace 下的文件（聊天 markdown
// 代码链接点击）。历史版本曾在此强制拉起 Zed 定位到行（用户反馈「老是自动
// 使用 zed 打开文件」已移除：链接点击不再自动启动 Zed，line 参数保留兼容
// 旧前端调用但不再影响打开行为）。
// file 须为当前工作区下的绝对路径（相对路径先按工作区拼接），防止越权打开
// 工作区外项目；错误可读返回（文件不存在等），前端 toast 展示。
func (a *App) OpenFileAtLine(file string, line int) error {
	ws := a.cfgWorkspace()
	if strings.TrimSpace(ws) == "" {
		return errors.New("尚未打开项目（请先在顶栏选择工作区）")
	}
	p := strings.TrimSpace(file)
	if p == "" {
		return errors.New("文件路径为空")
	}
	// 相对路径按工作区拼接；绝对路径直接使用（仍须落在工作区内）。
	if !filepath.IsAbs(p) {
		p = filepath.Join(ws, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("路径解析失败: %w", err)
	}
	if st, err := os.Stat(abs); err != nil {
		return fmt.Errorf("文件不存在: %s", abs)
	} else if st.IsDir() {
		return fmt.Errorf("不是文件: %s", abs)
	}
	// 越权防护：仅允许工作区内的文件（打开不跳出当前项目）。
	wsAbs, err := filepath.Abs(ws)
	if err != nil {
		return fmt.Errorf("工作区路径解析失败: %w", err)
	}
	relex, relErr := filepath.Rel(wsAbs, abs)
	if relErr != nil || strings.HasPrefix(relex, "..") || filepath.IsAbs(relex) {
		return fmt.Errorf("文件不在当前工作区内: %s", abs)
	}
	_ = line // 兼容旧签名：链接点击统一系统默认程序打开，行号不再参与定位
	return openWithDefault(abs)
}

// OpenFileWithDefault 用系统默认关联程序打开文件（聊天 markdown 非代码
// 文件链接点击：图片/pptx/docx/pdf/视频音频等）。允许两个白名单：当前
// 工作区（AI 产出的报告/图片等）与媒体回放目录 ~/.kaelvoris/media（
// generate_video / generate_speech 产物，聊天消息只存磁盘路径）。
// 越权防护与 OpenFileAtLine 同源（resolveOpenableFile）：白名单外的路径一律拒绝。
func (a *App) OpenFileWithDefault(file string) error {
	abs, err := a.resolveOpenableFile(file)
	if err != nil {
		return err
	}
	return openWithDefault(abs)
}

// SaveFileAs 聊天媒体「另存为」：弹出系统保存对话框，把 AI 生成产物
// （generate_image / generate_video / generate_speech 落盘文件）复制到用户
// 选定路径。白名单校验与 OpenFileWithDefault 同源（resolveOpenableFile），
// 防止把白名单外文件带出；目标覆盖冲突由系统对话框确认（OverwritePrompt）。
func (a *App) SaveFileAs(file string) error {
	abs, err := a.resolveOpenableFile(file)
	if err != nil {
		return err
	}
	ext := strings.TrimPrefix(filepath.Ext(abs), ".")
	target, err := uiSaveFile(a.ctx, "另存为", filepath.Base(abs), ext)
	if err != nil {
		return err
	}
	if strings.TrimSpace(target) == "" {
		return nil // 用户取消：静默返回（非错误）
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("读取源文件失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}
	return os.WriteFile(target, data, 0o644)
}

// resolveOpenableFile 链接点击打开的路径校验（OpenFileAtLine 自建校验 /
// OpenFileWithDefault / SaveFileAs 共用）：相对路径按工作区拼接；白名单 = 工作区内
// 任意文件 + ~/.kaelvoris/media、~/.kaelvoris/images（媒体/图片回放目录，
// 与 main.go serveKaelvorisFile 的静态白名单同源）；其余越权路径拒绝。
func (a *App) resolveOpenableFile(file string) (string, error) {
	ws := a.cfgWorkspace()
	if strings.TrimSpace(ws) == "" {
		return "", errors.New("尚未打开项目（请先在顶栏选择工作区）")
	}
	p := strings.TrimSpace(file)
	if p == "" {
		return "", errors.New("文件路径为空")
	}
	// 相对路径按工作区拼接；绝对路径直接使用（仍须落在白名单内）。
	if !filepath.IsAbs(p) {
		p = filepath.Join(ws, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("路径解析失败: %w", err)
	}
	if st, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("文件不存在: %s", abs)
	} else if st.IsDir() {
		return "", fmt.Errorf("不是文件: %s", abs)
	}
	allowed := func(dir string) bool {
		if dir == "" {
			return false
		}
		d, err := filepath.Abs(dir)
		if err != nil {
			return false
		}
		rel, err := filepath.Rel(d, abs)
		if err != nil {
			return false
		}
		return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
	}
	if allowed(ws) {
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err == nil {
		if allowed(filepath.Join(home, ".kaelvoris", "media")) || allowed(filepath.Join(home, ".kaelvoris", "images")) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("文件不在当前工作区内: %s", abs)
}

// SetEditorPreference 持久化一键打开方式（"folder" | "zed"）。
func (a *App) SetEditorPreference(editor string) error {
	e := strings.TrimSpace(strings.ToLower(editor))
	if e != "zed" {
		e = "folder" // 兜底：未知值归一为文件夹
	}
	a.cfgMu.Lock()
	a.cfg.EditorPreference = e
	c := a.cfg
	a.cfgMu.Unlock()
	return saveConfigFile(c)
}

// saveConfigFile 免锁持久化配置（调用方需已持有 cfgMu 或确认无并发写）。
func saveConfigFile(c Config) error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), b, 0o600)
}

// pushRecent 项目入栈（去重 + 上限截断）。
func pushRecent(list []string, p string, max int) []string {
	var out []string
	out = append(out, p)
	for _, v := range list {
		if v != p && len(out) < max {
			out = append(out, v)
		}
	}
	return out
}

// ProjectEntry 项目切换器条目。
type ProjectEntry struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Current bool   `json:"current"`
}

// emitEvent 包装 uiEmit（ctx 未就绪时静默；无头构建自动降级日志，见 ui_bridge_headless.go）。
func (a *App) emitEvent(name string, data ...interface{}) {
	if a.ctx != nil {
		uiEmit(a.ctx, name, data...)
	}
}

// ---------- 绑定：只读查询 ----------

// ListModes 全部模式（内置 + 自定义覆盖）。
func (a *App) ListModes() []modes.ModeConfig {
	mgr := modes.NewManager()
	_ = mgr.LoadProject(a.cfg.Workspace)
	return mgr.All()
}

// GetMode 当前模式 slug。
func (a *App) GetMode() string {
	a.modeMu.Lock()
	defer a.modeMu.Unlock()
	return a.currentMode
}

// SetMode 设置下一次任务使用的模式。
func (a *App) SetMode(slug string) {
	a.modeMu.Lock()
	a.currentMode = slug
	a.modeMu.Unlock()
}

// ListSkills 已发现技能列表。
func (a *App) ListSkills() []skills.Metadata {
	home, _ := os.UserHomeDir()
	return skills.NewManager(home, a.cfg.Workspace).All()
}

// GetTodos 当前会话的任务清单（P3.1-B：运行中回读内存态；未运行回读会话
// 持久化清单——停止后"继续"也能看到上次进度）。
func (a *App) GetTodos(sessionID string) []tools.TodoItem {
	a.runsMu.Lock()
	run := a.runs[sessionID]
	a.runsMu.Unlock()
	if run != nil {
		return run.todo.Items()
	}
	if sessionID == "" {
		return nil
	}
	if s, err := a.store.Get(sessionID); err == nil {
		return s.TodosSnapshot()
	}
	return nil
}

// GetMessages 回放指定会话的运行期事件（运行中会话实时；否则空——历史回放走 OpenSession）。
func (a *App) GetMessages(sessionID string) []message.ClineMessage {
	a.runsMu.Lock()
	run := a.runs[sessionID]
	a.runsMu.Unlock()
	if run == nil {
		return nil
	}
	run.histMu.Lock()
	defer run.histMu.Unlock()
	out := make([]message.ClineMessage, len(run.hist))
	copy(out, run.hist)
	return out
}

// RunningSessions 运行中/排队的会话 ID（前端多任务状态渲染）。
func (a *App) RunningSessions() []string {
	ids := make([]string, 0)
	for _, t := range a.registry.Running() {
		ids = append(ids, t.TaskID())
	}
	return ids
}

// IsRunning 是否有任意任务运行中。
func (a *App) IsRunning() bool {
	a.runsMu.Lock()
	defer a.runsMu.Unlock()
	return len(a.runs) > 0
}

// IsSessionRunning 指定会话是否有任务运行中（对齐 TaskRegistry.hasRunning）。
func (a *App) IsSessionRunning(sessionID string) bool {
	a.runsMu.Lock()
	_, ok := a.runs[sessionID]
	a.runsMu.Unlock()
	if !ok {
		return false
	}
	return a.registry.HasRunning(sessionID)
}

// PickWorkspace 打开目录选择对话框（无头构建自动降级为报错，见 ui_bridge_headless.go）。
func (a *App) PickWorkspace() (string, error) {
	dir, err := uiPickDir(a.ctx, "选择工作区目录")
	if err != nil {
		return "", err
	}
	return dir, nil
}

// ---------- 绑定：任务与审批 ----------

// SendMessage 提交任务到指定会话（sessionID 为空 = 新会话，自动生成并返回）：
// 异步启动引擎，事件以 sessionId 路由。多会话可同时运行（P2.7-7，TaskScheduler 门控并发）。
// employeeID 绑定数字员工（§9.1：空 = 普通会话；新会话首次绑定后固定生效）。
func (a *App) SendMessage(sessionID, task string, images []string) (string, error) {
	return a.SendMessageWithEmployee(sessionID, task, images, "")
}

// SendMessageUnattended 无人值守任务提交（roadmap 自主开发会话专用）：
// 全部 ask 自动应答（审批放行/followup 续跑/错误重试），无需人工看管——
// 对齐 CLI -y 无人值守语义；敏感文件红线仍强制拒绝（见 unattendedAskBridge）。
// 审批卡仍经 say 桥落会话历史（回放可见自动放行留痕）。
func (a *App) SendMessageUnattended(sessionID, task string, images []string) (string, error) {
	return a.sendMessageOpts(sessionID, task, images, "", unattendedAskBridge)
}

// SendMessageWithACP 带 ACP agent 绑定的任务提交（Composer agent 选择器入口）。
// acpAgent 非空时：新会话创建时直接绑定（消除先建会话再 SetSessionACP 与
// 任务启动的竞态）；续发老会话时改绑（运行中拒绝，空闲时改绑下轮生效）。
// 空串 = 内置引擎（不改变既有会话绑定——续发老 ACP 会话传空仍走 ACP）。
func (a *App) SendMessageWithACP(sessionID, task string, images []string, acpAgent string) (string, error) {
	task = strings.TrimSpace(task)
	if task == "" && len(images) == 0 {
		return "", errors.New("任务内容不能为空")
	}
	agt := strings.TrimSpace(acpAgent)
	// 新会话 + 指定 agent：先建会话壳（绑定 agent），再启动任务。
	if sessionID == "" && agt != "" {
		sid := fmt.Sprintf("%d", time.Now().UnixMilli())
		if err := a.SetSessionACP(sid, agt); err != nil {
			return "", err
		}
		sessionID = sid
	}
	// 续发老会话 + 指定 agent（改绑；运行中会话入队不改绑）。
	if sessionID != "" && agt != "" && !a.IsSessionRunning(sessionID) {
		if err := a.SetSessionACP(sessionID, agt); err != nil {
			return "", err
		}
	}
	return a.SendMessage(sessionID, task, images)
}

// SendMessageWithEmployee 带员工绑定的任务提交（§10.5 渠道路由 / composer 员工选择器入口）。
// 老会话续发时 employeeID 参数被忽略（沿用会话既有绑定，§9.1 生效版本语义）。
// 运行中不拒绝：同会话续发消息进入队列（FIFO），本轮任务结束后自动继续对话。
func (a *App) SendMessageWithEmployee(sessionID, task string, images []string, employeeID string) (string, error) {
	return a.sendMessageOpts(sessionID, task, images, employeeID, nil)
}

// sendMessageOpts 任务提交统一入口（askBridge nil = 普通人工审批桥；非 nil =
// 无人值守自动应答，ask 阻塞全部被代答，任务不等人）。
func (a *App) sendMessageOpts(sessionID, task string, images []string, employeeID string, askBridge agent.AskFn) (string, error) {
	task = strings.TrimSpace(task)
	if task == "" && len(images) == 0 {
		return "", errors.New("任务内容不能为空")
	}
	// 同一会话运行中：消息入队（不停止任务），本轮结束后 runTask 循环自动续发。
	if sessionID != "" && a.IsSessionRunning(sessionID) {
		if qm := a.enqueueSessionMsg(sessionID, task, images); qm {
			return sessionID, nil
		}
		return "", errors.New("该会话已有任务在运行，请等待完成或点击停止")
	}
	if sessionID == "" {
		sessionID = a.resolveContinueTarget(task)
		if sessionID == "" {
			sessionID = fmt.Sprintf("%d", time.Now().UnixMilli())
		}
	}
	go a.runTask(sessionID, task, images, employeeID, askBridge)
	return sessionID, nil
}

// resolveContinueTarget 「继续」意图空会话重定向（用户需求 2026-10：无当前会话时
// 输入「继续」不新建会话，复用最近一个既有会话接续，已完结同样重开续聊）。
// 规则：
//   - 仅「继续」类前缀输入触发（isResumeIntent，复用续接判定，避免误伤普通新任务）；
//   - 从会话列表（已按置顶优先 + 更新时间倒序）取第一个满足条件者：
//     跳过规划板自动迭代会话（roadmap_ 前缀，防打断自主迭代闭环）；
//     跳过委派子会话（ParentID 非空，父会话优先）；
//     跳过远程节点会话（NodeID 非空 = 远端执行，本机续发无意义）。
//   - 无可复用会话 → 返回空串，回落原新建会话路径。
//
// 返回选中会话 ID；仅供 sendMessageOpts 空 sessionID 分支调用。
func (a *App) resolveContinueTarget(task string) string {
	if !isResumeIntent(task) {
		return ""
	}
	for _, s := range a.sessionsWithRunning() {
		if strings.HasPrefix(s.ID, roadmapDevelopSessionIDPrefix) {
			continue // 规划板自动迭代会话不劫持「继续」
		}
		if s.ParentID != "" {
			continue // 子会话不作为续接目标，优先父会话
		}
		if s.NodeID != "" {
			continue // 远程节点会话本机无法续发
		}
		return s.ID
	}
	return ""
}

// enqueueSessionMsg 把消息追加到运行中会话的队列（runTask 每轮结束取出续发）。
// 返回 false = 会话已不在运行（竞态：恰在本检查后任务结束）——调用方回落正常发送。
func (a *App) enqueueSessionMsg(sessionID, task string, images []string) bool {
	a.runsMu.Lock()
	defer a.runsMu.Unlock()
	run, ok := a.runs[sessionID]
	if !ok {
		return false
	}
	run.pendingMsgs = append(run.pendingMsgs, queuedMsg{task: task, images: images})
	uiEmit(a.ctx, "kaelvoris:queued", map[string]any{
		"sessionId": sessionID, "count": len(run.pendingMsgs), "task": task,
	})
	return true
}

// dequeueSessionMsg 取出一条排队消息（FIFO；供 runTask 轮次循环续发）。
func (a *App) dequeueSessionMsg(sessionID string) (queuedMsg, bool) {
	a.runsMu.Lock()
	run, ok := a.runs[sessionID]
	if !ok || len(run.pendingMsgs) == 0 {
		a.runsMu.Unlock()
		return queuedMsg{}, false
	}
	m := run.pendingMsgs[0]
	run.pendingMsgs = run.pendingMsgs[1:]
	remaining := len(run.pendingMsgs)
	a.runsMu.Unlock()
	uiEmit(a.ctx, "kaelvoris:queued", map[string]any{"sessionId": sessionID, "count": remaining})
	return m, true
}

// dropSessionQueue 丢弃会话的全部排队消息（会话删除联动时调用，
// 防止删除后残留在 pendingMsgs 中的续聊被后续轮次复活）。
func (a *App) dropSessionQueue(sessionID string) {
	a.runsMu.Lock()
	if run, ok := a.runs[sessionID]; ok {
		run.pendingMsgs = nil
	}
	a.runsMu.Unlock()
}

// Stop 中止指定会话的任务（对齐 Task.abortTask）。
func (a *App) Stop(sessionID string) {
	a.runsMu.Lock()
	run := a.runs[sessionID]
	a.runsMu.Unlock()
	if run == nil {
		return
	}
	run.histMu.Lock()
	run.aborted = true
	run.histMu.Unlock()
	if run.cancel != nil {
		run.cancel()
	}
	// ask 阻塞立即解除（引擎收到 ResponseNo 走退出分支）。
	select {
	case run.askCh <- askReply{response: message.ResponseNo}:
	default:
	}
}

// StopAll 中止全部运行中/排队的任务。
func (a *App) StopAll() {
	for _, id := range a.registry.TaskIDs() {
		a.Stop(id)
	}
	a.scheduler.CancelQueued()
}

// ApproveAsk 回传 ask 应答（response ∈ yesButtonClicked|noButtonClicked|messageResponse）。
// sessionID 路由到对应会话的应答通道（多会话并行互不串扰）。
func (a *App) ApproveAsk(sessionID, response, text string) error {
	a.runsMu.Lock()
	run := a.runs[sessionID]
	a.runsMu.Unlock()
	if run == nil {
		return errors.New("会话没有正在运行的任务")
	}
	r := askReply{response: message.AskResponse(response), text: text}
	select {
	case run.askCh <- r:
		return nil
	default:
		return errors.New("当前没有等待应答的审批")
	}
}

// ---------- 内核组装与任务运行 ----------

// buildKernel 构建模式/技能管理器（引擎与工具注册中心改为每任务构建——多会话并行隔离）。
func (a *App) buildKernel() error {
	cfg := a.GetConfig()
	if strings.TrimSpace(cfg.APIKey) == "" {
		return errors.New("请先在设置中填写 API Key")
	}
	if strings.TrimSpace(cfg.Workspace) == "" {
		return errors.New("请先在设置中选择工作区目录")
	}

	modeMgr := modes.NewManager()
	if err := modeMgr.LoadProject(cfg.Workspace); err != nil {
		// 自定义模式缺失不致命，沿用内置。
		modeMgr = modes.NewManager()
	}
	home, _ := os.UserHomeDir()
	skillMgr := skills.NewManager(home, cfg.Workspace)

	a.kmu.Lock()
	a.modeMgr = modeMgr
	a.skillMgr = skillMgr
	a.kmu.Unlock()
	return nil
}

// mcpConnectResult TestMCPServer 的连接测试结果（前端「测试」按钮展示）。
type mcpConnectResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Tools int    `json:"tools,omitempty"`
	Error string `json:"error,omitempty"`
}

// TestMCPServer 连接测试单个 MCP 服务器（保存前「测试」按钮）：完整握手
// spawn → initialize → tools/list；失败原因（含服务端 stderr 摘要）作数据
// 返回，供前端在行内直接展示，避免「填了不知道为什么不生效」。
func (a *App) TestMCPServer(s MCPServer) (mcpConnectResult, error) {
	if strings.TrimSpace(s.Name) == "" {
		s.Name = "test"
	}
	// 开关一致性：禁用服务器不做连接测试——此前禁用后点「测试」仍能连上
	// 并返回工具数，用户误以为「没关掉/还能调用」（开关失效感知来源）。
	if !s.Enabled {
		return mcpConnectResult{Name: s.Name, Error: "服务器已禁用（开关关闭），不会在任务中连接。如需测试请先打开开关"}, nil
	}
	switch s.Transport {
	case "", "stdio":
		if strings.TrimSpace(s.Command) == "" {
			return mcpConnectResult{}, errors.New("请填写启动命令")
		}
	case "sse", "http":
		if strings.TrimSpace(s.URL) == "" {
			return mcpConnectResult{}, errors.New("请填写服务端 URL")
		}
	default:
		return mcpConnectResult{}, errors.New("不支持的传输类型: " + s.Transport)
	}
	cfg := mcp.ServerConfig{Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Headers: s.Headers}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cli, n, err := mcp.RegisterAll(ctx, cfg, func(name string, t *mcp.RemoteTool) error { return nil })
	if cli != nil {
		defer cli.Close()
	}
	if err != nil {
		return mcpConnectResult{Name: s.Name, Error: err.Error()}, nil
	}
	return mcpConnectResult{Name: s.Name, OK: true, Tools: n}, nil
}

// pushToast 向前端推送轻提示（kaelvoris:toast；窗口未就绪时静默丢弃）。
func (a *App) pushToast(msg string) {
	if a == nil || a.ctx == nil {
		return
	}
	uiEmit(a.ctx, "kaelvoris:toast", msg)
}

// buildTools 构建任务级工具注册中心（TodoStore 绑定本任务，会话间互不污染）。
// buildMCPTools 连接启用的 stdio MCP 服务器并注册其工具（codex 对齐补全 ⑦
// MCP 运行时最小落地）。返回全部 client 供任务结束时统一关闭。
// 单个 server 连接失败降级跳过不阻断任务（对齐 McpStartupUpdateEvent::Failure
// 语义），失败原因同步 toast 到前端（此前仅日志，用户无从得知）。
//
// 惰性加载（对齐 Claude Code 14.4 Tool Search / codex 懒连接经验）：全量连接
// 导致插件 207 工具全量进模型上下文，一句对话即超限。改为：任务文本/工作区
// 提示词命中服务器名 → 启动即连；未命中 → 只进候选目录（mcp_lazy.go），
// 模型用 mcp_activate 按需热加载。
func (a *App) buildMCPTools(ctx context.Context, registry *tools.Registry, log *slog.Logger, workspace string, emp *employee.Employee, sessionMCPDisabled bool) []mcp.Conn {
	// 合并：手动配置（mcp-servers.json）+ 插件自动发现（~/.kaelvoris/plugins/*/plugin.json）。
	// §4 MCP 总开关（定义侧）：员工禁用或会话级禁用（右侧会话设置面板）时
	// 根本不连接/不注册任何 mcp_* 工具（模型完全不可见；执行侧由 Gateway
	// 拒绝兑底，双保险）。
	if sessionMCPDisabled || (emp != nil && !emp.MCPEnabled()) {
		return nil
	}
	// §4 服务器白名单（定义侧）：MCPServers 非空时仅白名单内服务器参与连接/注册
	// （server 段对齐 mcp.sanitizeToolName 的 mcp_<server>_<tool> 命名）。
	empAllowsServer := func(name string) bool { return emp == nil || emp.ServerAllowed(name) }
	// 惰性激活判定（mcp_lazy.go）：任务文本（a.mcpProbeText）+ 工作区提示词
	// 命中服务器/插件名 → wanted；未命中 → 只进候选目录，不连接。
	wanted := a.mcpWanted()
	wantedSrv := make(map[string]bool, len(wanted.servers))
	for _, n := range wanted.servers {
		wantedSrv[n] = true
	}
	servers := a.loadMCPServers()
	var clients []mcp.Conn
	// 插件自动发现：~/.kaelvoris/plugins/*/plugin.json 声明 MCP 端点（stdio/远程）的插件
	// 追加到连接列表——启用开关（plugins.json）关闭的插件跳过；单个失败降级跳过不阻断任务。
	for _, p := range plugin.List() {
		if !plugin.IsEnabled(p.ID) {
			if log != nil {
				log.Debug("插件已禁用（跳过 MCP 连接）", "plugin", p.ID)
			}
			continue
		}
		name, transport, command, url, args, env, headers, ok := p.MCPServerOf()
		if !ok {
			continue // 纯技能插件：无 MCP 端点
		}
		if !empAllowsServer(name) {
			continue // §4 服务器白名单：该员工未授权此 MCP 服务器
		}
		if (transport == "stdio" && strings.TrimSpace(command) == "") ||
			(transport != "stdio" && strings.TrimSpace(url) == "") {
			continue
		}
		// 惰性加载：未命中当前任务信号的插件不连接（候选目录 + mcp_activate 按需热加载）。
		if !wantedSrv[name] {
			continue
		}
		// 插件工作区：office 插件按 GOFFICE_WORKSPACE 解析草稿目录；
		// 命令相对路径（"goffice.exe"）解析为插件目录内绝对路径。
		if env == nil {
			env = map[string]string{}
		}
		if workspace != "" {
			env["GOFFICE_WORKSPACE"] = workspace
		}
		if (transport == "stdio" || transport == "") && command != "" && !filepath.IsAbs(command) {
			command = filepath.Join(p.Dir, command)
		}
		cfg := mcp.ServerConfig{Name: name, Command: command, Args: args, Env: env, URL: url, Headers: headers}
		mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		cli, n, err := mcp.RegisterAll(mctx, cfg, func(name string, t *mcp.RemoteTool) error {
			return registry.Register(t)
		})
		cancel()
		if err != nil {
			if log != nil {
				log.Warn("插件 MCP 连接失败（已跳过，不阻断任务）", "plugin", p.ID, "err", err)
			}
			a.pushToast("⚠️ 插件 MCP 连接失败（已跳过）: " + p.ID + " — " + truncateRunes(err.Error(), 160))
			continue
		}
		if log != nil {
			log.Info("插件已连接", "plugin", p.ID, "tools", n)
		}
		a.pushToast(fmt.Sprintf("🔗 插件已连接: %s（%d 个工具）", p.Name, n))
		clients = append(clients, cli)
	}
	for _, s := range servers {
		if !s.Enabled {
			if log != nil {
				log.Debug("MCP 服务器已禁用（跳过连接）", "server", s.Name)
			}
			continue
		}
		// 惰性加载：未命中当前任务信号的服务器不连接（候选目录 + mcp_activate 按需热加载）。
		if !wantedSrv[s.Name] {
			continue
		}
		switch s.Transport {
		case "", "stdio":
			if strings.TrimSpace(s.Command) == "" {
				continue
			}
		case "sse", "http":
			if strings.TrimSpace(s.URL) == "" {
				continue
			}
		default:
			continue
		}
		cfg := mcp.ServerConfig{Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Headers: s.Headers}
		// 20s 总预算（initialize + tools/list）：npx 冷启动需解析/下载包，
		// 10s 在 Windows 防病毒扫描叠加时常不够。
		mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		cli, n, err := mcp.RegisterAll(mctx, cfg, func(name string, t *mcp.RemoteTool) error {
			return registry.Register(t)
		})
		cancel()
		if err != nil {
			if log != nil {
				log.Warn("MCP 服务器连接失败（已跳过，不阻断任务）", "server", s.Name, "err", err)
			}
			a.pushToast("⚠️ MCP 服务器连接失败（已跳过）: " + s.Name + " — " + truncateRunes(err.Error(), 160))
			continue
		}
		if log != nil {
			log.Info("MCP 服务器已连接", "server", s.Name, "tools", n)
		}
		a.pushToast(fmt.Sprintf("🔗 MCP 服务器已连接: %s（%d 个工具）", s.Name, n))
		clients = append(clients, cli)
	}
	return clients
}

// loadMCPServers 读取 MCP 服务器配置（兼容双格式）：
//   - kaelvoris 原生数组格式 [{name,transport,command,...}]；
//   - Claude/Cline/Cursor 等主流 mcpServers 映射格式
//     {"mcpServers":{"name":{"command":...,"args":...,"env":...,"disabled":...}}}。
//     映射条目默认 Enabled=true（disabled:true 才禁用）；type/transport 缺省
//     stdio，streamable-http/http/sse 按 mapRemoteTransport 归一（url 存在时
//     按远程传输，http/sse 运行时均自动探测无需区分）。
//
// 此前仅认数组格式：用户从其他工具复制粘贴主流映射格式时解析失败仅写日志
// 「mcp.json 解析失败」，前端无感知 → 表现为「配置后无法识别」。
func (a *App) loadMCPServers() []MCPServer {
	b, err := os.ReadFile(mcpConfigPath())
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("mcp-servers.json 读取失败", "err", err)
		}
		return nil
	}
	// 剥 BOM（PowerShell 5.1 写出的 UTF-8 文件默认带 BOM）。
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	// 统一走 parseMCPConfig（三格式兼容：数组 / mcpServers 映射 / 顶层单服务器映射）。
	if servers, n := parseMCPConfig(b); n > 0 {
		return servers
	}
	slog.Warn("mcp-servers.json 解析失败（已忽略，MCP 不启用）；支持 kaelvoris 数组格式 / {\"mcpServers\":{...}} / 单服务器映射对象三种格式")
	return nil
}

// parseMCPConfig 解析 MCP 配置 JSON 文本，返回解析出的服务器列表。
// 兼容三种格式（任一命中即返回）——
//  1. kaelvoris 原生数组格式 [{name,transport,command,...}]；
//  2. Claude/Cline/Cursor mcpServers 映射格式 {"mcpServers":{"name":{...}}}；
//  3. 顶层单服务器映射 {"b1-pro":{...}}（用户直贴 fragment，MCP 快速添加）。
//
// 数组条目默认 Enabled=true（显式 false 才禁用），Claude 习惯 disabled:true 视为
// 禁用；映射条目默认 Enabled=true（disabled:true 才禁用）。归一化规则与
// loadMCPServers 历史语义一致（type streamable-http/http/sse → 运行时自动探测）。
// 返回 (nil, 0) 表示三种格式均未命中（空对象 {} 返回空列表，不算失败——粘贴
// 空配置不应报错）。
func parseMCPConfig(b []byte) ([]MCPServer, int) {
	// 1) kaelvoris 原生数组格式。丢弃完全无信息条目（name/command/url 全空：
	// 任意 JSON 数组会被 json.Unmarshal 空匹配出零值条目，防御误判）。
	var list []MCPServer
	if err := json.Unmarshal(b, &list); err == nil {
		kept := list[:0]
		for i := range list {
			if list[i].Disabled {
				list[i].Enabled = false
			}
			if list[i].Name == "" && list[i].Command == "" && list[i].URL == "" {
				continue
			}
			kept = append(kept, list[i])
		}
		list = kept
		if len(list) > 0 {
			return list, len(list)
		}
		// 空数组 / 全无效条目继续尝试后续格式（JSON null 会命中此分支）。
	}
	// 2) mcpServers 映射格式（多服务器）。
	if servers, ok := parseMCPServerMap(b); ok {
		return servers, len(servers)
	}
	// 3) 顶层单服务器映射格式（用户从其他工具直贴的 fragment，无 mcpServers 包裹）：
	//    {"b1-pro":{"type":"streamable-http",...}}。与 2) 共用条目解析逻辑。
	if servers, ok := parseMCPServerMapLoose(b); ok {
		return servers, len(servers)
	}
	return nil, 0
}

// mcpEntry 映射格式单服务器条目（Claude/Cline 习惯字段；数组格式不经过此结构）。
// 独立结构体：ServerConfig 运行时无 Enabled/Disabled，MCPServer 为面板形态。
type mcpEntry struct {
	Type        string            `json:"type"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	AlwaysAllow []string          `json:"alwaysAllow"`
	Disabled    bool              `json:"disabled"`
}

// entryToServer 映射条目 → 面板 MCPServer（Enabled=!disabled；transport 归一）。
func entryToServer(name string, e mcpEntry) MCPServer {
	return MCPServer{
		Name:        name,
		Transport:   mapRemoteTransport(e.Type, e.Transport, e.URL),
		Command:     e.Command,
		Args:        e.Args,
		URL:         e.URL,
		Env:         e.Env,
		Headers:     e.Headers,
		AlwaysAllow: e.AlwaysAllow,
		Enabled:     !e.Disabled,
	}
}

// parseMCPServerMap {"mcpServers":{"name":{...}}} 多服务器映射格式。
func parseMCPServerMap(b []byte) ([]MCPServer, bool) {
	var m struct {
		McpServers map[string]mcpEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &m); err != nil || len(m.McpServers) == 0 {
		return nil, false
	}
	return serversFromEntryMap(m.McpServers), true
}

// parseMCPServerMapLoose 顶层单服务器映射格式：顶层键（除已知包裹键）的值均为
// 服务器条目对象。空对象返回 false 交由调用方语义（空配置不报错）。
func parseMCPServerMapLoose(b []byte) ([]MCPServer, bool) {
	var m map[string]mcpEntry
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	// 包裹键不是服务器：显式剔除（防御 {"mcpServers":null} 命中松散分支）。
	delete(m, "mcpServers")
	if len(m) == 0 {
		return nil, false
	}
	// 宽松校验：值必须长得像服务器条目（type/transport/command/url 任一存在），
	// 防御任意 JSON 对象（如 {"a":1}）被误判为服务器（解析出零配置占位条目）。
	for _, e := range m {
		if e.Type == "" && e.Transport == "" && e.Command == "" && e.URL == "" {
			return nil, false
		}
	}
	return serversFromEntryMap(m), true
}

// serversFromEntryMap 映射 → 列表（按名称排序，保持 loadMCPServers 历史行为）。
func serversFromEntryMap(m map[string]mcpEntry) []MCPServer {
	out := make([]MCPServer, 0, len(m))
	for name, sv := range m {
		out = append(out, entryToServer(name, sv))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// mapRemoteTransport 归一映射格式传输类型：type/transport 字段 + url 存在启发。
// streamable-http/streamablehttp/http → http；sse → sse；其余（含空）→
// url 非空时按 http（远程，协议自动探测）、无 url 按 stdio。
func mapRemoteTransport(typeField, transportField, url string) string {
	t := strings.ToLower(strings.TrimSpace(typeField))
	if t == "" {
		t = strings.ToLower(strings.TrimSpace(transportField))
	}
	switch t {
	case "http", "streamable-http", "streamablehttp":
		return "http"
	case "sse":
		return "sse"
	case "stdio":
		return "stdio"
	}
	if strings.TrimSpace(url) != "" {
		return "http" // url 存在即远程传输（运行时自动探测具体协议）
	}
	return "stdio"
}

func buildTools(workspace string, todo *tools.TodoStore, skillMgr *skills.Manager, a *App) *tools.Registry {
	registry := tools.NewRegistry()
	// 沙箱工厂：由当前配置构造，注入 shell/unified_exec/后台三执行路径
	//（每条命令/会话/后台任务各 New 一个 job 实例，用完 Close；见 sandbox.go）。
	sandboxCfg := tools.SandboxConfig{Mode: tools.SandboxModeJob}
	if a != nil {
		if sc := a.GetConfig().Sandbox; sc != nil {
			sandboxCfg = *sc
		}
	}
	sandbox := tools.NewSandboxFactory(sandboxCfg)
	must(registry.Register(&tools.ReadFileTool{Workspace: workspace}))
	must(registry.Register(&tools.WriteFileTool{Workspace: workspace}))
	must(registry.Register(&tools.ApplyDiffTool{Workspace: workspace}))
	must(registry.Register(&tools.ListFilesTool{Workspace: workspace}))
	must(registry.Register(&tools.SearchFilesTool{Workspace: workspace}))
	// 2026-10 能力补全接线：web_search/web_fetch 早已实现（web_tools.go）但从未注册——模型永远收不到；此处补上，检索类工具随 read 组发放。
	must(registry.Register(&tools.WebSearchTool{}))
	must(registry.Register(&tools.WebFetchTool{}))
	must(registry.Register(&tools.ArxivSearchTool{}))
	must(registry.Register(&tools.ShellTool{Runner: &tools.CommandRunner{
		Workspace:  workspace,
		TimeoutSec: 120,
		DenyList:   tools.DefaultDenyList,
		Sandbox:    sandbox,
	}}))
	// codex 对齐补全 ⑤：unified_exec 持久 shell 会话（cd/env 跨调用延续）；
	// 后台模式 + shell_output：长驻服务启动命令立即返回，Agent 轮询日志。
	bgMgr := tools.NewBackgroundManager(workspace)
	bgMgr.Sandbox = sandbox
	unifiedMgr := tools.NewPersistentShellManager(workspace)
	unifiedMgr.Sandbox = sandbox
	unified := &tools.UnifiedExecTool{Mgr: unifiedMgr, Bg: bgMgr}
	must(registry.Register(unified))
	must(registry.Register(&tools.ShellOutputTool{Mgr: bgMgr}))
	must(registry.Register(&tools.AskFollowupTool{}))
	must(registry.Register(&tools.AttemptCompletionTool{}))
	must(registry.Register(&tools.UpdateTodoListTool{Store: todo}))
	// P2.8 并行委派：桌面端引擎装配 SubtaskRunner（buildRunEngine）支持 fan-out，new_task 描述按并行能力分支。
	must(registry.Register(&tools.NewTaskTool{Parallel: true}))
	must(registry.Register(&tools.SpawnAgentTool{}))
	must(registry.Register(&tools.WaitAgentTool{}))
	must(registry.Register(&tools.SwitchModeTool{}))
	must(registry.Register(&tools.DesignSystemTool{}))
	// open-design 全量融合：design_template（design-templates 模板浏览/加载，
	// 与 design_system 配套——先加载设计体系 token，再套模板种子写页面）。
	must(registry.Register(&tools.DesignTemplateTool{}))
	// 聊天内生图：generate_image（文生图，OpenAI images API 兼容端点）。
	// Settings 闭包热读当前配置（设置保存后下一轮生效）；图片落盘
	// ~/.kaelvoris/images/，载荷经引擎剥离转 say image 消息展示。
	must(registry.Register(&tools.GenerateImageTool{
		Settings: func() tools.ImageGenSettings {
			c := a.GetConfig()
			return tools.ImageGenSettings{
				BaseURL: c.ImageGenBaseURL,
				Model:   c.ImageGenModel,
				APIKey:  c.ImageGenAPIKey,
			}
		},
	}))
	// 聊天内生视频/语音（generate_video / generate_speech）：同生图模式，
	// Settings 闭包热读当前配置；视频落 ~/.kaelvoris/media/video/，音频落
	// media/audio/，载荷经引擎剥离转 say video/audio 消息展示。
	must(registry.Register(&tools.GenerateVideoTool{
		Settings: func() tools.VideoGenSettings {
			c := a.GetConfig()
			return tools.VideoGenSettings{
				BaseURL: c.VideoGenBaseURL,
				Model:   c.VideoGenModel,
				APIKey:  c.VideoGenAPIKey,
				Mode:    c.VideoGenMode,
				Size:    c.VideoGenSize,
			}
		},
		// 图生视频上传钩子：本地首尾帧/参考图 → 服务端图床公网 URL
		//（POST /api/images 节点令牌鉴权，返回 /img/<file>；视频网关要求
		// 媒体公网可访问）。未登录远程服务时 Upload 内部报错，工具回
		// 明确错误引导模型改走文生视频。
		Upload: func(ctx context.Context, data []byte, name string) (string, error) {
			client := a.remoteClientSnapshot()
			if client == nil || client.HTTPBase() == "" {
				return "", fmt.Errorf("server media storage not connected: login to remote server (远程服务) first")
			}
			u, err := client.UploadImageBytes(data, name, "", "")
			if err != nil {
				return "", err
			}
			// 相对 URL /img/<file> 拼基地址成公网绝对 URL。
			if strings.HasPrefix(u, "/") {
				u = client.HTTPBase() + u
			}
			return u, nil
		},
	}))
	must(registry.Register(&tools.GenerateSpeechTool{
		Settings: func() tools.SpeechSettings {
			c := a.GetConfig()
			return tools.SpeechSettings{
				BaseURL: c.SpeechBaseURL,
				Model:   c.SpeechModel,
				APIKey:  c.SpeechAPIKey,
				Voice:   c.SpeechVoice,
			}
		},
	}))
	// 会话复盘新增诊断三件套：process_list / file_locker / http_request
	// （弥补 shell $ 转义吞失、文件占用无从定位、HTTP 探测拼管道三类短板）。
	must(registry.Register(&tools.ProcessListTool{}))
	must(registry.Register(&tools.FileLockerTool{}))
	must(registry.Register(&tools.HTTPRequestTool{}))
	// 2026-10 能力补全：语音转文字（OpenAI /audio/transcriptions 兼容）与邮件发送（net/smtp 标准库）、Notion、浏览器自动化（CDP over ws，Edge/Chrome headless）。
	must(registry.Register(&tools.SpeechToTextTool{Settings: func() tools.SpeechSettings {
		c := a.GetConfig()
		return tools.SpeechSettings{
			BaseURL: c.SpeechBaseURL,
			Model:   c.SpeechModel,
			APIKey:  c.SpeechAPIKey,
		}
	}}))
	must(registry.Register(&tools.EmailSendTool{Settings: func() tools.EmailSettings {
		c := a.GetConfig()
		return tools.EmailSettings{
			SMTPHost: c.SMTPHost,
			SMTPPort: c.SMTPPort,
			SMTPUser: c.SMTPUser,
			SMTPPass: c.SMTPPass,
			FromAddr: c.SMTPFrom,
		}
	}}))
	must(registry.Register(&tools.NotionTool{Settings: func() tools.NotionSettings {
		c := a.GetConfig()
		return tools.NotionSettings{APIKey: c.NotionAPIKey}
	}}))
	// 内置浏览器自动化（CDP over ws，Edge/Chrome headless/内嵌 WebView2 双路径）：
	//   - BrowserDisabled（设置面板「隐藏内置浏览器」）= 不注册任何 browser_*
	//     工具，模型不可见（对齐 SkillsDisabled 定义侧不注册语义）；
	//   - BrowserPath 热读当前配置（修复：设置页「浏览器路径」此前从未接线）；
	//   - InternalBrowser 内嵌显示开关 + WebView2 启动器注入：开关热读设置
	//     （下一任务生效，免重启）；启动器指向 WebView2 宿主（Windows），非
	//     Windows stub 恒失败 → 工具层静默回落 headless。CLI/服务端宿主不注入
	//     两钩子，天然维持 headless 行为。
	if !a.GetConfig().BrowserDisabled {
		browserTool := &tools.BrowserTool{
			BrowserPath:     a.GetConfig().BrowserPath,
			EmbeddedEnabled: func() bool { return a.GetConfig().InternalBrowser },
			EmbeddedLauncher: func() (int, error) {
				return ensureEmbeddedBrowser()
			},
		}
		must(registry.Register(browserTool))
		must(registry.Register(&tools.BrowserEvalTool{Browser: browserTool}))
		must(registry.Register(&tools.BrowserScreenshotTool{Browser: browserTool}))
		// 2026-10 深度嵌入（openai/codex BrowserUse 能力面对齐）：
		// 结构化交互 snapshot→act 工作流 + 多标签页。
		must(registry.Register(&tools.BrowserActTool{Browser: browserTool}))
		must(registry.Register(&tools.BrowserSnapshotTool{Browser: browserTool}))
		must(registry.Register(&tools.BrowserTabsTool{Browser: browserTool}))
	}
	if tools.DesktopSupported() {
		must(registry.Register(&tools.DesktopWindowsTool{}))
		must(registry.Register(&tools.DesktopScreenshotTool{}))
		must(registry.Register(&tools.DesktopActTool{}))
	}
	// §4 技能（SKILLS）权限（定义侧）：SkillsDisabled 时连 skill 工具都不注册
	//（模型完全不可见；执行侧由 Gateway 拒绝兜底，双保险）。
	// buildTools 为包级函数不持有 emp，员工感知由 buildRunEngine 调用者处置：
	// 禁用技能的员工会话里 SkillsXML 为空 → 模型无技能可发现，且网关拒绝兜底。
	must(registry.Register(&tools.SkillTool{
		Loader: func(name string) (string, error) {
			c, err := skillMgr.Load(name)
			if err != nil {
				return "", errSkillLoad{err}
			}
			return c.Body, nil
		},
		Names: skillMgr.Names,
		// 技能包内文件读取：SKILL.md 正文引导模型按需加载 references/ 材料
		//（内置技能如 web-design-guidelines 附带 references/guidelines.md）。
		ReadFile: skillMgr.ReadFile,
		// D3 技能级工具授权（对标 Claude Code allowed-tools）：frontmatter
		// allowed-tools 字段 → 技能执行成功后授权写入当轮引擎授权表
		//（下一轮请求构造前失效；红线在引擎更早分支不受影响）。
		ToolsFor: func(skill string) []string {
			if c, err := skillMgr.Load(skill); err == nil {
				return c.AllowedTools
			}
			return nil
		},
		OnAuthorize: func(skill string, patterns []string) {
			a.grantSkillsToRuns(patterns)
		},
	}))
	return registry
}

// runTask 阻塞执行任务（goroutine 内运行；事件经 EventsEmit 按 sessionId 路由推送）。
// P2.7-7 多会话并行：TaskScheduler 并发门控 + 每会话独立引擎/待办/应答通道
// （对齐 Zoo-Code Task 实例隔离 + TaskRegistry.push 登记）。
// 轮次循环（运行中不停止）：本轮完成后若队列有待续消息，逐条取出作为新任务
// 继续下一轮（不重新走 SendMessage 防重入拒绝；会话保持运行态直至队列清空）。
func (a *App) runTask(sessionID, task string, images []string, employeeID string, unattendedBridge agent.AskFn) {
	// panic 防护（崩溃修复）：本 goroutine 承载整条任务执行链（引擎/工具/
	// LLM 流式/MCP 子进程）。Go 语义：goroutine 内未捕获 panic = 进程立即
	// 退出（用户表现：任务运行到一半窗口闪退）。拦截后转为 kaelvoris:done
	// 错误事件 + app.log 日志；已注册 defer（cancel/清理 runs）照常执行。
	defer func() {
		if r := recover(); r != nil {
			slog.Error("任务执行 panic（已拦截，进程保持运行）",
				"session", sessionID, "panic", r, "stack", string(debug.Stack()))
			uiEmit(a.ctx, "kaelvoris:done", map[string]any{
				"result": "", "sessionId": sessionID,
				"error": "任务内部异常终止（已恢复，详见 ~/.kaelvoris/app.log）: " + fmt.Sprint(r),
			})
		}
	}()
	// 内核组件惰性构建（配置变更后生效）。
	a.kmu.Lock()
	needBuild := a.modeMgr == nil || a.skillMgr == nil
	a.kmu.Unlock()
	if needBuild {
		if err := a.buildKernel(); err != nil {
			uiEmit(a.ctx, "kaelvoris:done", map[string]any{"error": err.Error(), "sessionId": sessionID})
			return
		}
	}
	a.kmu.Lock()
	modeMgr, skillMgr := a.modeMgr, a.skillMgr
	a.kmu.Unlock()

	// E1 全局日额护栏（对标 SWE-agent MSWEA_GLOBAL_COST_LIMIT）：多会话/
	// 定时任务/IM 通道并行叠加的当日总成本熔断——超日额新任务直接拒绝
	// （0 = 不限缺省零破坏；已运行任务不受影响，错误含重置方法）。
	if gerr := usageGuardCheck(a.GetConfig().GlobalCostLimitUSD); gerr != nil {
		uiEmit(a.ctx, "kaelvoris:done", map[string]any{"error": gerr.Error(), "sessionId": sessionID})
		return
	}

	// 会话创建/复用 + 状态事件。P3.1-B：复用会话 = 恢复——加载持久化
	// 待办与记忆文档，构建"最近 N 条 + 待办 + 进度"恢复摘要注入引擎。
	now := time.Now()
	if sessionID == "" {
		sessionID = fmt.Sprintf("%d", now.UnixMilli())
	}
	var sess *session.Session
	var resumeDigest string
	if old, err := a.store.Get(sessionID); err == nil && old != nil {
		sess = old
		if strings.TrimSpace(task) != "" {
			sess.SetTitle(truncateTitle(task, 40))
		}
		// P3.1-B 会话恢复：有历史消息或待办 → 构建 ResumeDigest。
		// 模型凭"最近 N 条对话要点 + 当前待办 + 记忆文档"续接，无需全量回放。
		if snap := sess.Snapshot(); len(snap.Messages) > 0 || len(snap.Todos) > 0 {
			resumeDigest = ResumeDigestFor(snap, a.memory.Load(sessionID), task)
		}
	} else {
		// UpdatedAt 必须同设为 now（会话列表按 UpdatedAt 倒序）：旧实现只设
		// CreatedAt，新会话 UpdatedAt 为零值 → 排在列表最末尾。
		sess = &session.Session{
			ID:         sessionID,
			ChannelID:  "desktop",
			Title:      truncateTitle(task, 40),
			Mode:       a.GetMode(),
			Model:      a.GetConfig().Model, // 会话自有模型（创建时落当前选择，切换会话联动恢复）
			Workspace:  a.cfgWorkspace(),    // 会话关联工作区（切换会话联动切换）
			EmployeeID: employeeID,          // §9.1 新会话首次绑定员工（空 = 普通会话）
			CreatedAt:  now,
			UpdatedAt:  now,
		}
	}
	// 旧会话无工作区记录（升级兼容）：以当前工作区补记。
	if sess.Snapshot().Workspace == "" {
		sess.SetWorkspace(a.cfgWorkspace())
	}
	// 错误态清除（会话卡片信息升级）：新任务开始即重置上一轮错误标记，
	// 红点只在「最近一段执行出错」时可见（会话恢复/续聊同理）。
	sess.SetError(false)
	sess.SetDone(false)
	// （消息去重修复）引擎 RunInMode 首帧会以原始任务文本记录带图 user_feedback
	// （engine.go feedback.Images），此处不再预 append 一条——否则带图任务在
	// 会话历史中出现两条用户消息，回放时聊天区重复。
	a.store.Save(sess)
	a.sessMu.Lock()
	a.currentSession = sessionID
	a.sessMu.Unlock()
	uiEmit(a.ctx, "kaelvoris:sessions", a.store.List())

	// P3.1-B：待办恢复——把持久化清单回灌运行态 TodoStore（新任务开始前）。
	// 会话关联工作区写入运行态：引擎/工具注册中心按会话工作区构建（多会话
	// 并行时各任务操作各自项目目录，不受顶栏切换干扰）。
	run := &taskRun{id: sessionID, workspace: sess.Snapshot().Workspace, employeeID: sess.Snapshot().EmployeeID, model: sess.Snapshot().Model, mcpDisabled: sess.Snapshot().MCPDisabled, todo: tools.NewTodoStore(), askCh: make(chan askReply, 1), suppressFeedback: true}
	run.mcpProbe = task // MCP 惰性加载：桌面主路径任务文本作为激活信号
	if snap := sess.Snapshot(); len(snap.Todos) > 0 {
		run.todo.Set(snap.Todos)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	defer func() {
		if run.cleanup != nil {
			run.cleanup() // 数字员工网关审计文件关闭等（后进先出）
		}
		cancel()
		a.runsMu.Lock()
		delete(a.runs, sessionID)
		a.runsMu.Unlock()
		a.registry.Remove(sessionID)
		// 会话级状态修复（任务完成后自动结束）：完成事件必须携带「本会话」
		// 的运行态——此刻已从 runs/registry 注销，恒为 false；旧实现误填全局
		// IsRunning()，只要并发存在任何其他任务（排队续发/定时任务/企微飞书/
		// 并行子任务），刚完成的会话就被前端重新标成 running=true 且再无事件
		// 纠正，状态条永久卡在「运行中」。前端全局状态由 refreshStatus 按各
		// 会话标志聚合，无需后端代报全局值。
		uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": false, "sessionId": sessionID})
	}()

	a.runsMu.Lock()
	a.runs[sessionID] = run
	a.runsMu.Unlock()
	a.registry.Push(run) // 登记并聚焦（TaskRegistry.push = move-to-top）
	uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": true, "sessionId": sessionID})

	// TaskScheduler 并发门控（对齐 schedule：许可不足则排队，取消则跳过）。
	// 轮次循环：curTask/curImages 为当前轮任务；每轮结束检查队列，有待续消息
	// 且本轮非用户中止 → 以队首消息续发新引擎轮次（会话不退出运行态）。
	a.scheduler.Schedule(run, func() {
		if run.IsAborted() {
			return
		}
		// ACP 会话分支：会话绑定外部 agent（Claude Code / Gemini CLI / Codex
		// 等）→ 任务经 ACP session/prompt 派发，不进内置引擎循环。
		// say 桥在 acpRunTask 内独立接线（历史留痕 + EventsEmit + 远程镜像
		// 语义与内置路径一致），轮次循环/队列续发/自学习复盘全部复用。
		if agt := a.acpAgentOf(sessionID); agt != nil {
			a.acpRunTask(ctx, run, sess, sessionID, *agt, task, resumeDigest)
			return
		}
		for {
			// 每轮独立内核（TodoStore/Registry/Engine）+ 并行子任务执行器（P2.8）。
			engine, mcpClients := a.buildRunEngine(ctx, modeMgr, skillMgr, run, images)
			// E1 全局日额累计基线：本轮任务启动前的会话计量快照（收尾时差值入账）。
			usageBaseCost, usageBaseCalls := 0.0, 0
			if engine.Usage != nil {
				s := engine.Usage.Snapshot()
				usageBaseCost, usageBaseCalls = s.CostUSD, s.Requests
			}
			// codex 对齐补全 ⑦：任务结束关闭全部 MCP 服务器子进程（生命周期 = 本轮）。
			flushMCP := func() {
				for _, c := range mcpClients {
					if c != nil {
						c.Close()
					}
				}
			}

			// D1 检查点恢复通知：上次任务间隙经前端回滚的文件，随本次任务文本注入，
			// 模型基于磁盘当前状态继续（对齐 Gemini CLI rewind "inform the model"；
			// 与 ResumeDigest 同级语义：一次性 user 消息，不入系统提示词）。
			if n := a.drainRestoreNotes(sessionID); n != "" {
				task = task + "\n\n[checkpoint-restore] 检测到文件已通过检查点回滚，以下变更已生效，请基于磁盘当前状态继续，勿假设旧内容仍存在：\n" + n
			}

			// 会话技能注入（用户需求 2026-12：右侧技能 TAB 挂载的技能随任务生效）：
			// 挂载技能的指令全文 + 用户配置参数拼为 [session-skills] 段并入任务文本，
			// 模型在执行任务时遵循已挂载技能的约束与流程；无挂载 = 零注入零开销。
			if snip := a.sessionSkillsSnippet(sessionID); snip != "" {
				task = task + "\n\n" + snip
			}

			// P3.1-C：会话恢复注入引擎（记忆文档 + 恢复摘要 + 记忆文档路径）。
			engine.ResumeDigest = resumeDigest
			engine.MemoryDoc = a.memory.Load(sessionID)
			// 会话卡片手动摘要注入（提示词 SESSION CARD PREVIEW 段；空 = 不注入）。
			engine.ManualPreview = sess.Snapshot().ManualPreview
			engine.MemoryPath = a.memory.Path(sessionID)
			// 工作空间记忆注入（2026-10-31 双层记忆）：跨会话共享、按工作区隔离，
			// 空文档 = 首次运行该项目不注入（注入段全空，零开销）。工作区取
			// 会话运行态记录值（与 buildRunEngine 的 ws 兜底同源：旧会话空记录
			// 已在上方 SetWorkspace 补记）。
			engine.WSDoc = a.wsMemory.Load(run.workspace)
			engine.WSPath = a.wsMemory.Path(run.workspace)
			// 自进化注入：任务文本命中关键词的经验卡生成 <self_evolution> 段。
			engine.EvolveSnippet = a.evolveStore.ContextSnippet(task)

			// say 事件桥：历史留痕 + 前端推送（带 sessionId 路由）+ 会话持久化 + todo 刷新钩子。
			say := a.makeSayBridge(sess, run, sessionID)

			// ask 桥：推送由 waitAsk→say 完成；unattendedBridge 非空 = 无人值守会话
			//（roadmap 自主开发）：全部 ask 自动应答，不阻塞等人。
			onAsk := a.makeAskBridge(run, ctx)
			if unattendedBridge != nil {
				onAsk = unattendedBridge
			}

			// GetMode 持锁读取模式（修复：裸读 a.currentMode 与 SetMode 数据竞争，
			// 任务 goroutine 与用户切换模式并发时属未定义行为，可致崩溃）。
			// 会话自有模式优先（会话切换联动恢复的执行策略）：运行中会话的
			// 模式固定为本会话记录值，不受其他会话的顶栏切换干扰；空 = 跟随全局。
			runMode := a.GetMode()
			if sm := strings.TrimSpace(sess.Snapshot().Mode); sm != "" {
				runMode = sm
			}
			result, err := engine.RunInMode(ctx, runMode, task, false, 0, say, onAsk)
			flushMCP()
			tools.CloseBrowser() // 浏览器进程随本轮任务结束 kill（cdp 生命周期=本轮，防孤儿 Edge）

			// E1 全局日额累计（对标 SWE-agent MSWEA_GLOBAL_COST_LIMIT）：任务
			// 收尾按 Usage 快照与任务前基线的差值入账（任务粒度精度足够防
			// 失控；ponytail：需每轮实时落盘时改为 say usage 事件挂点）。
			if engine.Usage != nil {
				snap := engine.Usage.Snapshot()
				usageGuardRecord(snap.CostUSD-usageBaseCost, snap.Requests-usageBaseCalls)
			}
			done := map[string]any{"result": result, "sessionId": sessionID}
			if err != nil {
				done["error"] = err.Error()
				sess.SetError(true) // 错误态落置（列表卡红点；停止/失败均置位）
			} else {
				sess.SetDone(true) // 成功完成态落置（列表卡绿标题；中途已恢复的报错一并覆盖）
			}
			// 会话标题兜底（空任务+图片时以结果摘要命名）。
			if strings.TrimSpace(sess.Title) == "" && result != "" {
				sess.SetTitle(truncateTitle(result, 40))
				a.store.Save(sess)
			}
			// P3.1-A：任务结束（完成/停止/出错）强制刷盘——关键节点数据不丢。
			a.store.FlushAll()
			uiEmit(a.ctx, "kaelvoris:done", done)
			uiEmit(a.ctx, "kaelvoris:sessions", a.store.List())

			// 自主迭代钩子：roadmap 开发会话结束 → 回收结果并接力下一条。
			// 会话 ID 前缀 roadmap_ 即规划板开发任务（单条开发固定会话）。
			if itemID, ok := strings.CutPrefix(sessionID, roadmapDevelopSessionIDPrefix); ok && itemID != "" {
				a.roadmapDevelopNext(itemID, result, err)
			}

			// 定时任务失败转发：cron 触发的会话（前缀 cron_）运行失败时推送
			// kaelvoris:cron-failed，前端失焦弹系统通知（cronRunner 同步失败路径
			// 在 cron_panel.go 内已接；此处覆盖引擎运行失败如无效模型——异步链路
			// 调度器不可见，必须在此补接）。与回复完成通知共用同一失焦通知通道与设置开关。
			if err != nil {
				if jobID, ok := strings.CutPrefix(sessionID, "cron_"); ok && jobID != "" {
					a.notifyCronFailed(cron.Job{ID: jobID, Name: cronJobName(a.cronStore, jobID)}, err)
				}
			}

			// 自学习闭环（对齐 kaelvoris-agent background_review）：每轮任务
			// 结束后按配置间隔触发后台复盘——非流式调主模型审视本轮对话，
			// JSON 产出经验卡落 evolveStore（同名卡 = 更新），下任务注入。
			a.maybeSelfReview(sessionID, task, run, err)

			// 用户中止 = 退出整条轮次链（排队消息丢弃，对齐 Stop 语义）。
			if run.IsAborted() || ctx.Err() != nil {
				return
			}
			// 队列有消息 → 继续对话（本轮结束后引入）：取出队首作为下一轮任务。
			next, ok := a.dequeueSessionMsg(sessionID)
			if !ok {
				return
			}
			if next.task == "" && len(next.images) == 0 {
				continue
			}
			task = next.task
			images = next.images
			resumeDigest = ResumeDigestFor(sess.Snapshot(), a.memory.Load(sessionID), task)
			// 续发轮运行态重发：上一轮 kaelvoris:done 已把会话标记为空闲，重发
			// running=true 保持状态条与真实执行一致（直至队列清空）。
			uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": true, "sessionId": sessionID})
			// 续发提示：前端聊天区标记队列衔接（不落会话历史）。
			uiEmit(a.ctx, "kaelvoris:queued-continue", map[string]any{
				"sessionId": sessionID, "task": task,
			})
		}
	})
}

// resumeIntentHints「继续」类续接意图前缀（任务文本规范化后按前缀匹配；
// 小写英文 + 中文去空白/标点）。命中即视为用户要求续接上次断点。
var resumeIntentHints = []string{
	"继续", "接着", "续上", "续接", "接续",
	"go on", "continue", "keep going", "resume",
}

// isResumeIntent 识别「继续」类输入（意图路由）：
// 继续/接着/续上/续接/接续 + continue/go on/keep going/resume。
// 仅取前缀匹配（"我不想继续"不命中，"再次继续"不命中），避免误判一般对话。
func isResumeIntent(task string) bool {
	s := strings.ToLower(strings.TrimSpace(task))
	s = strings.TrimLeft(s, " ！!。.？?，,～~、")
	for _, hint := range resumeIntentHints {
		if strings.HasPrefix(s, hint) {
			return true
		}
	}
	return false
}

// ResumeDigestFor 意图门控的恢复摘要入口（6 宿主共享，替换裸 buildResumeDigest 调用）：
//   - 「继续」类输入：存在未完成待办 或 记忆文档非空 → 注入完整续接上下文（待办+记忆+最近对话）；
//     两者皆无 → 返回空串（退化为普通对话，不注入"接着干"语义）；
//   - 非「继续」输入：维持既有行为（复用会话有历史/待办即注入，模型可理解前文）。
func ResumeDigestFor(snap session.SessionData, memoryDoc, task string) string {
	if isResumeIntent(task) {
		if !hasIncompleteTodo(snap.Todos) && strings.TrimSpace(memoryDoc) == "" {
			return "" // 无记忆/无未完成待办：无断点可续，退化为普通对话
		}
		return buildResumeDigest(snap, memoryDoc)
	}
	if len(snap.Messages) == 0 && len(snap.Todos) == 0 {
		return ""
	}
	return buildResumeDigest(snap, memoryDoc)
}

// buildResumeDigest 构建会话恢复摘要（P3.1-B）：
// 最近 N 条对话要点（截断文本，跳过流式 partial 与上下文快照）+ 当前待办清单。
// 与 SESSION MEMORY（记忆文档）配合，模型无需回放全部历史即可续接任务。
func buildResumeDigest(snap session.SessionData, memoryDoc string) string {
	const recentN = 12
	var b strings.Builder
	b.WriteString("---- 会话恢复上下文（本会话曾被停止，以下是历史进度，请接着干，不要重新开始） ----\n")

	// 待办清单（进度状态：哪些完成/哪些进行中/哪些卡住）。
	if len(snap.Todos) > 0 {
		b.WriteString("\n[上次任务清单]\n")
		for _, t := range snap.Todos {
			mark := "[ ]"
			switch t.Status {
			case "completed":
				mark = "[x]"
			case "in_progress":
				mark = "[-]"
			}
			b.WriteString(mark + " " + t.Content + "\n")
		}
		if hasIncompleteTodo(snap.Todos) {
			b.WriteString("（存在未完成项：从中断处的 in_progress/pending 项继续，已完成项不要重复做）\n")
		}
	}

	// 记忆文档（主体+指针；模型可按指针 fs_read 详细内容）。
	if memoryDoc != "" {
		b.WriteString("\n[会话记忆文档（MEMORY.md）]\n" + memoryDoc + "\n")
	}

	// 上下文摘要压缩记录（规划板 2026-10）：发生过摘要压缩时注入摘要正文，
	// 模型凭此可引用被替换的早期历史要点（任务目标/关键决策/文件路径/未完成项）。
	if snap.Condense != nil && snap.Condense.Condensed && strings.TrimSpace(snap.Condense.Summary) != "" {
		b.WriteString("\n[早期上下文摘要（较早对话已压缩为以下要点，可凭此引用早期结论）]\n")
		b.WriteString(snap.Condense.Summary + "\n")
	}

	// 最近 N 条对话要点（倒序取样，跳过 partial/api_req/ai_context 噪声）。
	msgs := snap.Messages
	picked := make([]message.ClineMessage, 0, recentN)
	for i := len(msgs) - 1; i >= 0 && len(picked) < recentN; i-- {
		m := msgs[i]
		if m.Partial || m.Say == message.SayAPIReqStarted || m.Say == message.SayAPIReqFinished ||
			m.Say == message.SayAIContext {
			continue
		}
		picked = append(picked, m)
	}
	if len(picked) > 0 {
		b.WriteString("\n[最近对话要点（旧→新）]\n")
		for i := len(picked) - 1; i >= 0; i-- {
			m := picked[i]
			kind, text := describeMessage(m)
			if text == "" {
				continue
			}
			b.WriteString("- " + kind + ": " + text + "\n")
		}
	}
	b.WriteString("---- 恢复上下文结束 ----\n")
	return b.String()
}

// describeMessage 恢复摘要中单条消息的（类型标签, 截断文本）。
func describeMessage(m message.ClineMessage) (kind, text string) {
	const maxLen = 300
	cut := func(s string) string {
		r := []rune(s)
		if len(r) > maxLen {
			return string(r[:maxLen]) + "…"
		}
		return s
	}
	switch {
	case m.Type == message.TypeAsk:
		return "提问", cut(m.Text)
	case m.Say == message.SayUserFeedback:
		return "用户", cut(m.Text)
	case m.Say == message.SayText:
		return "助手", cut(m.Text)
	case m.Say == message.SayTool:
		var info message.ToolCallInfo
		if json.Unmarshal([]byte(m.Text), &info) == nil {
			label := info.Tool
			if info.Path != "" {
				label += " → " + info.Path
			} else if info.Cmd != "" {
				label += " → " + truncateTitle(info.Cmd, 120)
			}
			return "工具调用", cut(label)
		}
		return "工具调用", cut(m.Text)
	case m.Say == message.SayError:
		return "错误", cut(m.Text)
	case m.Say == message.SaySubtaskStarted:
		return "子任务开始", cut(m.Text)
	case m.Say == message.SaySubtaskFinished:
		return "子任务完成", cut(m.Text)
	case m.Say == message.SayModeSwitched:
		return "模式切换", cut(m.Text)
	default:
		return "", ""
	}
}

// hasIncompleteTodo 是否存在未完成待办。
func hasIncompleteTodo(items []tools.TodoItem) bool {
	for _, t := range items {
		if t.Status != "completed" {
			return true
		}
	}
	return false
}

// buildRunEngine 为任务运行态构建独立引擎（P2.7 每任务独立内核 + P2.8 并行子任务执行器）。
func (a *App) buildRunEngine(ctx context.Context, modeMgr *modes.Manager, skillMgr *skills.Manager, run *taskRun, images []string) (*agent.Engine, []mcp.Conn) {
	// MCP 惰性加载（mcp_lazy.go）：当前轮任务文本同步到 App 级探针，
	// 供 mcpWanted() 激活判定（多会话并行时各轮串行读自身 run，无锁）。
	a.mcpProbeText = run.mcpProbe
	// 会话关联工作区：任务固定运行于会话自己的工作区（多会话并行各归各的项目，
	// 不受顶栏切换干扰）；空 = 旧会话/全局配置工作区兜底。
	ws := run.workspace
	// 数字员工接管（§5/§7/§9，双区语义）：会话绑定员工时，任务仍运行在会话
	// 工作区（ws，工具 cwd），员工专属工作空间（empWS）作为记忆/进化沉淀目录
	// 保持可达——网关同时放行两区（cwd = 会话工作区 + 员工工作空间），角色
	// 提示词声明双区边界；另注入目录/MCP 权限网关 + 员工级模型参数。
	var emp *employee.Employee
	empWS := ""
	if run.employeeID != "" {
		if e, ok := a.empStore.Get(run.employeeID); ok && e.Runnable(false) {
			emp = &e
			empWS = e.Workspace(a.empStore.Dir())
			a.resolveEmpOrgNames(emp)
			// 岗位→角色权限链运行期生效：岗位映射角色与直绑角色按收紧式合并
			// 进员工有效配置（buildMCPTools 门控与执行侧 Gateway 均读有效配置）。
			a.applyEmpRole(emp)
		} else {
			slog.Warn("数字员工不可用，回退普通会话", "employeeId", run.employeeID)
		}
	}
	if ws == "" {
		// 会话无工作区：员工会话以员工工作空间为 cwd（独立员工运行态）；
		// 普通会话退回全局配置工作区。
		if emp != nil {
			ws = empWS
		} else {
			ws = a.cfgWorkspace()
		}
	}
	registry := buildTools(ws, run.todo, skillMgr, a)
	// 内部 MCP 集成：定时任务聊天工具——Agent 可在对话中查看/新增/更新/
	// 删除/启停/立即执行应用内定时任务（与设置面板共享 cronStore；
	// 变更经 pushCron 推送前端列表实时刷新）。
	// F5（对标 Hermes "Cron-run sessions cannot recursively create more cron jobs"）：
	// cron 触发的会话（前缀 cron_）不注册 cron_job——模型不可见、不可调用，
	// 防定时任务内再创建定时任务造成调度循环失控。
	if !run.cronSession {
		must(registry.Register(&CronTool{
			Store:            a.cronStore,
			Sched:            a.cronSched,
			DefaultWorkspace: ws,
			Push:             a.pushCron,
		}))
	}
	// 自进化闭环：经验卡工具——Agent 在定时自检/复盘中沉淀可复用编码经验；
	// 后续任务组装提示词时按关键词命中注入 <self_evolution> 段。
	must(registry.Register(&EvolveTool{
		Store:         a.evolveStore,
		DefaultSource: "session:" + run.id,
	}))
	// 自主迭代：规划提案工具——Agent 会话内发现值得做的改进时沉淀为规划
	// 提案（落 proposed 待审核；autoDev 开启时系统自动批准开发）。
	must(registry.Register(&RoadmapProposeTool{
		Store:            a.roadmapStore,
		DefaultWorkspace: ws,
		DefaultSource:    "agent:" + run.id,
		Push:             a.pushRoadmap,
	}))
	// 向量知识库（Zoo-Code code-index 对齐）：总开关开启时注册
	// codebase_search 语义检索工具——LLM 写码/答题前可自然语言检索
	// 已索引目录（项目文档/既有实现约定），命中片段含路径行号。
	// （VectorKBDir 在 engine 创建后按「向量回答」开关注入，见下方。）
	vc := a.vectorConfig()
	if vc != nil && vc.Enabled {
		must(registry.Register(&CodebaseSearchTool{
			Search: a.vectorSearch,
		}))
	}
	// codex 对齐补全 ⑦：MCP 运行时——惰性加载：仅连接命中当前任务信号的
	// 服务器/插件（失败降级跳过，不阻断任务；失败原因 toast 到前端；
	// client 随任务结束关闭），未命中者以候选目录透出，经 mcp_activate 热加载。
	mcpClients := a.buildMCPTools(ctx, registry, slog.Default(), ws, emp, run.mcpDisabled)
	// MCP 惰性加载（mcp_lazy.go）：存在候选目录且会话/员工允许 MCP 时，
	// 注册 mcp_activate 热加载工具（连接 → 工具注册进本任务 Registry，
	// toolDefs 每轮现算下一轮即可调用；conn 经 addConn 并入 mcpClients
	// 并链入 run.cleanup，任务结束统一关闭）。
	if mw := a.mcpWanted(); len(mw.candidates) > 0 && !run.mcpDisabled && (emp == nil || emp.MCPEnabled()) {
		addConn := func(c mcp.Conn) {
			mcpClients = append(mcpClients, c)
			if run.cleanup == nil {
				run.cleanup = c.Close
			} else {
				prev := run.cleanup
				run.cleanup = func() { c.Close(); prev() }
			}
		}
		must(registry.Register(&mcpActivateTool{
			app: a, workspace: ws, registry: registry, addConn: addConn,
		}))
	}
	client := llm.NewClient(a.GetConfig().BaseURL, a.GetConfig().APIKey, a.GetConfig().Model)
	// 会话自有模型覆盖（会话切换联动恢复的选择器值；空 = 跟随全局配置）：
	// 多会话并行时各自模型互不干扰，切换会话后选中器回显会话自己的模型。
	if m := strings.TrimSpace(run.model); m != "" {
		client.Model = m
	}
	// §6.1 员工级模型覆盖：员工配置了专属模型时替换全局模型（优先级高于会话级）。
	if emp != nil && strings.TrimSpace(emp.Model) != "" {
		client.Model = emp.Model
	}
	engine := agent.New(client, registry, nil)
	engine.Modes = modeMgr
	engine.Todos = run.todo
	// Claude Code Hooks 对齐：用户可配置生命周期钩子（~/.kaelvoris/hooks.json，
	// PreToolUse/PostToolUse/Stop 外部命令；阻断/改参语义见 internal/hooks）。
	// 会话工作区作钩子进程 cwd，每轮任务重读（改配置即时生效）。
	home, _ := os.UserHomeDir()
	if hm := hooks.Load(hooks.DefaultPath(home), ws); hm.Enabled() {
		engine.UserHooks = hm
	}
	// Claude Code /cost 对齐：会话级 token/cost 计量器（单价配置 0 = 仅计 token）。
	cfg := a.GetConfig()
	// E1 单任务成本/调用护栏（对标 SWE-agent cost_limit / call_limit）：
	// costLimitUSD > 0 时注入限额，超限引擎优雅终止（已完成进度保留）。
	engine.Usage = usage.NewWithLimits(cfg.CostPerMPrompt, cfg.CostPerMCompletion, cfg.ContextWindow, cfg.CostLimitUSD, 0)
	engine.MaxSubtaskDepth = 3
	// E3 主模型失败自动降级（对标 Plandex errorFallback）：备用模型三项
	// 齐备且与主模型不同时才注入（相同 = 降级无意义，行为与现状一致）。
	if fb := a.fallbackClient(client); fb != nil {
		engine.FallbackLLM = fb
	}
	// §4 技能（SKILLS）权限（定义侧）：员工禁用技能时不注入 <available_skills>
	// 段；黑名单员工时过滤命中项（执行侧由 Gateway 拒绝 skill 工具兜底，双保险）。
	engine.SkillsXML = func() string {
		xml := availableSkillsXML(modeMgr, skillMgr)
		if emp != nil {
			xml = skillsXMLEmp(xml, emp)
		}
		return xml
	}
	if abs, err := filepath.Abs(ws); err == nil {
		engine.Workspace = abs
	}
	engine.Language = "zh-CN"
	// MCP 惰性加载：候选目录非空时注入提示词段（指引模型用 mcp_activate）。
	// 空目录 = 无候选，不注入（模型不可见，零开销）。
	engine.MCPLazyHint = mcpLazyHint(a.mcpWanted().candidates, run.mcpDisabled, emp)
	// 向量回答（Zoo-Code code-index 对齐）：AnswerWith 开启时注入
	// VECTOR KNOWLEDGE BASE 提示词段——写码/答题前强制 codebase_search
	// 检索已索引目录；仅 Enabled 时工具可用但不强制（不注入段）。
	if vc != nil && vc.Enabled && vc.AnswerWith && strings.TrimSpace(vc.Dir) != "" {
		engine.VectorKBDir = vc.Dir
	}
	if emp != nil {
		// 双区语义（会话工作区 + 员工专属工作空间）：角色段声明双区边界
		//（同目录收敛为单区旧文案，收敛逻辑在 RoleSection 内部）。
		engine.EmployeeRole = emp.RoleSection(empWS, ws)
		// §6.2 采样参数：Employee 用值类型（0 = 未配置 = 服务端默认），
		// Engine 用指针（nil = 未设置）；此处按「0 → nil」语义转换。
		if emp.Temperature > 0 {
			t := emp.Temperature
			engine.EmpTemperature = &t
		}
		if emp.TopP > 0 {
			tp := emp.TopP
			engine.EmpTopP = &tp
		}
		if emp.MaxTokens > 0 {
			mt := emp.MaxTokens
			engine.EmpMaxTokens = &mt
		}
		// 记忆沉淀（§11.4）：审批落地的 BEHAVIOR/TOOLS/WORKFLOWS.md 存入
		// EmpMemoryDoc（引擎组装提示词时并入 SESSION MEMORY，不被通道层
		// 对 MemoryDoc 的覆盖赋值冲掉）。双区语义下仍从员工工作空间读记忆
		//（记忆沉淀固定在员工专属空间，与任务项目目录解耦）。
		empWSLoad := empWS
		if strings.TrimSpace(empWSLoad) == "" {
			empWSLoad = ws
		}
		engine.EmpMemoryDoc = employee.LoadMemory(empWSLoad)
		// 统一权限网关（§4/§7/§8）：工具执行前拦截 + 审计 JSONL（任务结束关闭）。
		// cwd = ws（会话工作区，工具相对路径实际解析基准；空回退员工工作空间）。
		gw := employee.NewGateway(emp, a.empStore.Dir(), ws)
		engine.HookPreToolUse = gw.Hook
		if run.cleanup == nil {
			run.cleanup = gw.Close
		} else {
			prev := run.cleanup
			run.cleanup = func() { gw.Close(); prev() }
		}
	} else if run.mcpDisabled {
		// 会话级禁用 MCP（右侧会话设置面板；普通会话无员工网关时的执行侧兑底）：
		// 定义侧 buildMCPTools 已不连接不注册，此处拦截残留调用（防旧配置/竞态），
		// 语义对齐员工网关 mcpDenyReason。
		engine.HookPreToolUse = mcpDenyHook(engine.HookPreToolUse, true)
	}
	// P3/B5 上下文治理：contextWindow > 0 时启用上下文压缩；
	// 触发阈值 = 窗口 × autoCondensePercent%（默认 90，对齐 Zoo-Code
	// TOKEN_BUFFER_PERCENTAGE=0.1）；输出预留固定 4096。
	// CondenseDisabled 总开关（设置页，规划板 2026-10）：关闭后不再摘要/截断
	// 历史消息（硬裁剪兑底仍保留，保命防 413）。
	cfgNow := a.GetConfig()
	engine.ContextWindowTokens = cfgNow.ContextWindow
	engine.CondenseTriggerPercent = cfgNow.AutoCondensePercent
	engine.CondenseEnabled = !cfgNow.CondenseDisabled
	engine.CondenseKeepRecent = condenseDefaultKeepRecent
	engine.ReservedOutputTokens = 4096
	engine.TruncationFrac = condense.DefaultFrac
	// codex 对齐补全 ①：LLM 摘要压缩主路径——上下文超阈值时优先 LLM 摘要
	//（condense.SummarizeConversationKeep），失败回退滑动窗口截断（双层语义）。
	// 用户可在设置面板「提示词」配置 summarize 段覆盖摘要指令（空 = 内置默认）。
	// E2 辅助模型分流（对标 Plandex summarizer 角色）：摘要压缩高频触发，
	// 配置了辅助模型（auxBaseURL/auxAPIKey/auxModel 三项齐备）时走便宜小
	// 模型；未配置回落主模型（零破坏）。失败回退截断语义不变。
	engine.Summarizer = condense.LLMClientSummarizerWithPrompt(a.auxClient(client), a.promptStore.Get("summarize"))
	// 用户自定义系统提示词覆盖（设置面板「提示词」system 段，2026-11）：
	// 非空时以最高优先级注入系统提示词末尾（见 agent.SystemPrompt）。
	// 面板展示与编辑均基于原版模板（agent.SystemPromptTemplate），此处注入
	// 前还原 {$CWD}/{$HOME} 等占位符为本机运行时值（无占位符的旧配置原样）。
	engine.PromptOverride = agent.ExpandPromptVars(a.promptStore.Get("system"), a.cfgWorkspace())
	// 自主开发守则段覆盖（设置面板「提示词」autonomousDev 段，2026-10-25）：
	// 非空时整段替换内置 AUTONOMOUS DEVELOPMENT 段（见 agent.SystemPrompt）。
	engine.AutonomousDev = a.promptStore.Get("autonomousDev")
	// OnCondense 摘要压缩成功回调：摘要正文落会话存储（CondenseRecord），
	// 续聊经 ResumeDigest 注入模型可引用早期要点；前端经实时流标注「已压缩」。
	// 会话对象按 run.id 从存储取（store 内存缓存命中，无磁盘 IO；Store.Get
	// 返回 (*Session, error)，并发会话曾误当单值赋值致编译错，此处修正）。
	if sessRef, err := a.store.Get(run.id); err == nil && sessRef != nil {
		storeRef := a.store
		engine.OnCondense = func(removed, kept int, summary string) {
			sessRef.SetCondense(&session.CondenseRecord{
				Condensed:       true,
				RemovedMessages: removed,
				KeptMessages:    kept,
				Summary:         summary,
				CondensedAt:     time.Now(),
			})
			storeRef.Save(sessRef) // 异步刷盘（合并缓存 + 后台写盘）
			uiEmit(a.ctx, "cline:message", map[string]any{
				"sessionId": run.id,
				"message":   message.NewSay(message.SayContextCondensed, "历史消息已压缩为结构化摘要（详见上下文摘要记录）"),
			})
		}
	}
	// 会话 hook：任务结束统一回调（引擎 OnTaskFinished，2026-10 会话总结）→
	// 后台生成会话总结（目标/状态/产出/下一步），落会话存储并在聊天流/会话卡展示。
	// 回调在引擎所有收尾路径触发（完成/出错/停止/预算耗尽），不阻塞任务收尾。
	sessIDForHooks := run.id
	filesForCommit := engine.DiffFiles
	autoCommitEnabled := func() bool { return a.GetConfig().GitAutocommitEnabled }
	engine.OnTaskFinished = func(ctx context.Context, userTask, result string, runErr error, elapsed time.Duration) {
			a.maybeTaskSummary(run.id, userTask, run, runErr, elapsed)
			// 工作区面板会话：任务结束回写工作区根 README.md 工作日志（用户需求 2026-12）。
			a.maybeWSProjectLog(run.id, userTask, result, runErr)
			// 对标增强 B2：任务运行日志导出（缺省开，纯本地序列化）。
			a.maybeTaskLog(sessIDForHooks, userTask, result, runErr, elapsed)
		// 对标增强 C3：任务收尾 git 自动提交（对标 Aider auto-commits）——
		// 仅成功收尾 + 显式开启时执行；失败静默（日志）不破坏收尾流程。
		if runErr != nil || !autoCommitEnabled() || filesForCommit == nil {
			return
		}
		files := filesForCommit()
		if len(files) == 0 || !autocommit.IsGitRepo(ws) {
			return
		}
		go func() {
			hash, err := autocommit.Run(ws, files, userTask)
			if err != nil {
				slog.Warn("autocommit 失败（不影响任务收尾）", "err", err)
				return
			}
			uiEmit(a.ctx, "cline:message", map[string]any{
				"sessionId": sessIDForHooks,
				"message":   message.NewSay(message.SayGitAutocommit, fmt.Sprintf("已提交 %s（%d 个文件）", hash, len(files))),
			})
		}()
	}
	engine.AutoApprove = func(message.ClineMessage) bool {
		return a.GetConfig().AutoApprove
	}
	// 全AI自动决策（用户需求 2026-10）：AutoApprove 开启时 ask_followup_question
	// 不阻塞等用户——引擎记录问题后回填自主决策指令，模型结合上下文自行选择。
	engine.AutoDecide = a.GetConfig().AutoApprove
	// 危险操作自动确认（用户需求 2026-10）：设置开关透传引擎，命中危险
	// 命令免审放行（见 engine.go 危险命令分支）；每轮任务重读配置，改开关即生效。
	engine.DangerAutoApprove = a.GetConfig().DangerAutoApprove
	engine.UserImages = images
	// P2.8 多 Agent 并行协作：new_task 委派 → 独立子会话 Agent（并行 fan-out，
	// 对齐 Zoo-Code delegateParentAndOpenChild 子任务独立运行语义）。
	engine.SubtaskRunner = func(ctx context.Context, st tools.SubtaskResult, say agent.SayFn, onAsk agent.AskFn, depth int) (string, error) {
		return a.runParallelSubtask(ctx, run.id, st, depth)
	}
	// codex-main 多智能体协作（spawn_agent/wait_agent）：后台智能体以同一
	// 子会话执行器承载，但引擎侧异步派发（startSpawn goroutine），父任务
	// 不阻塞；结果经 wait_agent 收割（对齐 codex spawn_agent/wait_agent）。
	engine.SpawnRunner = engine.SubtaskRunner
	// 中途消息注入（移植 Zoo-Code userMessageContent）：任务执行期间用户
	// 续发的聊天消息每轮 API 请求前 drain 注入下一次请求——任务不打断、
	// 消息不丢失（对齐 Zoo-Code 中途反馈机制；带图消息同样注入）。
	engine.Incoming = func() []llm.Message {
		return a.drainSessionMsgs(run.id)
	}
	// ---- 对标增强 C1/C2（doc/10-对标增强 第二批）：编辑后验证 + 审批矩阵 ----
	// C1：非 VerifyDisabled 时挂 verify.Runner（fs_write / fs_apply_diff 成功后
	// 运行规则检查，失败反馈回灌模型立即修复；对标 Aider auto-lint）。
	if cfgC1 := a.GetConfig(); !cfgC1.VerifyDisabled {
		home, _ := os.UserHomeDir()
		engine.Verifier = verify.NewRunner(ws, verify.Load(filepath.Join(home, ".kaelvoris", "verify.json")))
	}
	// D2：审批存储（参数级规则 + 工具级矩阵，向后兼容；旧形态文件照读；
	// 红线检查在引擎更早分支，任何规则不可覆盖。对标 Gemini CLI policy engine）。
	engine.Approval = approval.LoadStore(approvalsPath())
	// ---- 对标增强 B1：任务检查点（缺省开，CheckpointDisabled 兼容旧配置）----
	if cfgB1 := a.GetConfig(); !cfgB1.CheckpointDisabled {
		cps := checkpoint.NewStore(checkpointDir(), run.id, 0)
		if abs, err := filepath.Abs(ws); err == nil {
			cps.SetWorkspaceRoot(abs)
		}
		engine.CheckpointStore = cps
	}
	return engine, mcpClients
}

// drainSessionMsgs 一次性取走会话全部排队消息（FIFO drain；供引擎每轮
// API 请求前注入为 user 消息——中途反馈立即生效，不再等整轮结束）。
// 与 runTask 轮次循环的 dequeueSessionMsg 复用同一 pendingMsgs 队列：
// 引擎运行中该循环阻塞在 RunInMode 内，二者不会并发取队；引擎中途
// 消费的消息 runTask 轮末 dequeue 自然取不到（幂等跳过）。
func (a *App) drainSessionMsgs(sessionID string) []llm.Message {
	a.runsMu.Lock()
	run, ok := a.runs[sessionID]
	if !ok || len(run.pendingMsgs) == 0 {
		a.runsMu.Unlock()
		return nil
	}
	msgs := make([]llm.Message, 0, len(run.pendingMsgs))
	for _, m := range run.pendingMsgs {
		txt := strings.TrimSpace(m.task)
		if txt == "" && len(m.images) == 0 {
			continue
		}
		if txt == "" {
			txt = "[图片附件]"
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: txt, Images: m.images})
	}
	run.pendingMsgs = nil
	a.runsMu.Unlock()
	uiEmit(a.ctx, "kaelvoris:queued", map[string]any{"sessionId": sessionID, "count": 0})
	return msgs
}

// makeSayBridge 会话事件桥：历史留痕 + 前端推送（sessionId 路由）+ 会话持久化 +
// todo 刷新钩子 + P3.1-C 里程碑记忆追加。
// P3.1-A 卡顿修复：
//   - AppendMerged：流式 partial 消息在会话历史内合并（不再随 chunk 膨胀）；
//   - store.Save 为异步刷盘（内存缓存 + 200ms 节流），高频事件零磁盘 IO；
//   - partial 事件不落会话（定型消息到达时已含全文，partial 仅供前端覆盖渲染）。
//
// 并行子任务委派标记会从子任务 goroutine 并发调用父桥，bridgeMu 串行化会话读写。
func (a *App) makeSayBridge(sess *session.Session, run *taskRun, sessionID string) agent.SayFn {
	var bridgeMu sync.Mutex
	// 工作空间记忆「最近交互」自动更新（2026-10-31 双层记忆，用户需求：
	// 每次交互都要更新）：拦截 api_req_finished（每轮 LLM 请求结束 = 一次
	// 交互），把工作区路径与最近交互时间写入工作空间记忆头部。纯机械零
	// token；文档体保持指针式不膨胀。同一会话多轮请求只在时间变化时落盘
	//（分钟精度），避免每轮磁盘写。
	lastWsTouch := ""
	// P3.1-A+ 流式节流（切换卡死修复）：partial 每秒至多推送 12 次——
	// 双会话并行流式时逐 chunk EventsEmit 会形成事件洪水（WebView 主线程
	// 被 partial 渲染 + scrollBottom reflow 打满，切换会话即卡死）。
	// 会话历史/持久化仍逐事件完整记录（AppendMerged 合并，零丢失）；
	// 定型消息（Partial=false）立即推送，流式观感不受影响。
	const partialInterval = 80 * time.Millisecond
	// 并行子任务 fan-out 会从多个 goroutine 并发调用父桥，节流时间戳必须加锁
	// （修复：lastPartialEmit 裸读写与 fan-out 并发竞争，属未定义行为）。
	var partialMu sync.Mutex
	var lastPartialEmit time.Time
	return func(m message.ClineMessage) {
		// 双层记忆「最近交互」自动更新（2026-10-31，用户需求：每次交互都更新）：
		// 每轮 LLM 请求结束（api_req_finished）同时刷新两级记忆——会话级
		// MEMORY.md「最近交互」节 + 工作空间记忆首节；分钟精度 + 同值跳过 =
		// 同会话多轮请求不重复落盘。lastWsTouch 经 partialMu 保护（fan-out
		// 并行子任务从多 goroutine 并发调用父桥，裸读写属未定义行为）。
		if m.Say == message.SayAPIReqFinished {
			partialMu.Lock()
			stamp := time.Now().Format("2006-01-02 15:04")
			fresh := stamp != lastWsTouch
			if fresh {
				lastWsTouch = stamp
			}
			partialMu.Unlock()
			if fresh {
				a.touchSessionMemory(sessionID, stamp)
				if run.workspace != "" {
					a.touchWorkspaceMemory(run.workspace, sessionID, stamp)
				}
			}
		}
		// P2.7-0b AI 上下文快照：仅实时推送前端查看，不落历史/会话（对话膨胀可视化）。
		if m.Say == message.SayAIContext {
			uiEmit(a.ctx, "kaelvoris:ai_context", map[string]any{
				"sessionId": sessionID, "messages": json.RawMessage(m.Text),
			})
			return
		}
		// Claude Code /cost 对齐：usage 计量事件同理仅实时推送前端展示，
		// 不落历史/会话（text 为 usage.Totals JSON）。
		if m.Say == message.SayUsage {
			uiEmit(a.ctx, "kaelvoris:usage", map[string]any{
				"sessionId": sessionID, "totals": json.RawMessage(m.Text),
			})
			return
		}
		// 聊天内生图/视频/语音（generate_image / generate_video / generate_speech）：
		// 历史/持久化剥离 base64（会话 JSON 体积控制——data URL 动辄数 MB，路径已在
		// text 载荷 MarshalImageMsgText / MarshalMediaMsgText），实时推送保留 data URL
		// 直显；回放时前端按路径经 /kaelvoris-image/、/kaelvoris-media/ 路由回挂。
		histMsg := m
		if m.Say == message.SayImage || m.Say == message.SayVideo || m.Say == message.SayAudio {
			thin := m
			thin.Images = nil // 剥 base64；text 载荷（路径列表）原样保留
			histMsg = thin
		}
		run.histMu.Lock()
		run.hist = append(run.hist, histMsg)
		run.histMu.Unlock()
		bridgeMu.Lock()
		sess.AppendMerged(histMsg)
		// 错误态标记（规划板 2026-10 会话卡片信息升级）：say error / api_req_failed
		// 落置会话 HasError（列表卡红点；会话存储同持久化）；新任务开始清除。
		if m.Type == message.TypeSay && m.Say == message.SayError {
			sess.SetError(true)
		}
		// 完成态标记（会话列表成功完成绿标题）：引擎完成回执（AskCompletionResult）
		// 到达即置 Done（清除中途报错残留）；新任务开始清除。
		if m.Type == message.TypeAsk && m.Ask == message.AskCompletionResult {
			sess.SetDone(true)
		}
		a.store.Save(sess) // 异步：合并缓存 + 后台刷盘（不阻塞事件流）
		bridgeMu.Unlock()
		// 消息去重：桌面端发送时前端已本地渲染用户消息，引擎首条
		// user_feedback 回显不再实时推送（历史照记，切回/回放正常显示）。
		if run.suppressFeedback && m.Say == message.SayUserFeedback && !m.Partial {
			run.histMu.Lock()
			firstFeedback := !run.feedbackSeen
			run.feedbackSeen = true
			run.histMu.Unlock()
			if firstFeedback {
				return
			}
		}
		if m.Partial {
			partialMu.Lock()
			now := time.Now()
			skip := now.Sub(lastPartialEmit) < partialInterval
			if !skip {
				lastPartialEmit = now
			}
			partialMu.Unlock()
			if skip {
				return // 节流：跳过本次前端推送（内容为累计全文，下一帧即覆盖）
			}
		}
		uiEmit(a.ctx, "cline:message", map[string]any{
			"sessionId": sessionID, "message": m,
		})
		if m.Say == message.SayTool {
			var info message.ToolCallInfo
			if json.Unmarshal([]byte(m.Text), &info) == nil && info.Tool == "update_todo_list" {
				// P3.1-B：待办变更回写会话持久化（停止后"继续"可恢复）。
				items := run.todo.Items()
				sess.SetTodos(items)
				a.store.Save(sess)
				// P3.1-C：里程碑记忆——每次完成一项，总结追加到 MEMORY.md。
				for _, it := range items {
					if it.Status == "completed" {
						a.appendMemoryMilestone(sessionID, it.Content)
					}
				}
				uiEmit(a.ctx, "kaelvoris:todos", map[string]any{
					"sessionId": sessionID, "items": items,
				})
			}
		}
	}
}

// touchSessionMemory 会话记忆「最近交互」自动更新（2026-10-31 双层记忆）：
// say 桥每轮交互调用（api_req_finished）；失败静默（记忆更新不阻塞任务流）。
// 与 touchWorkspaceMemory 同源同精度（分钟 + 同值跳过），镜像
// MemoryStore.Touch 单节覆盖语义：只动「最近交互」节，里程碑/收尾节原样保留。
func (a *App) touchSessionMemory(sessionID, stamp string) {
	if a.memory == nil {
		return
	}
	_ = a.memory.Touch(sessionID, "最近交互："+stamp+" · 每轮请求自动刷新（工作区及产出详见任务流）")
}

// touchWorkspaceMemory 工作空间记忆「最近交互」自动更新（2026-10-31 双层记忆）：
// say 桥每轮交互调用；失败静默（记忆更新不阻塞任务流）。
func (a *App) touchWorkspaceMemory(ws, sessionID, stamp string) {
	if a.wsMemory == nil || strings.TrimSpace(ws) == "" {
		return
	}
	_ = a.wsMemory.Touch(ws, stamp+" · 会话 "+sessionID+" 有交互（时间随每轮请求刷新）")
}

// appendMemoryMilestone 向会话记忆文档追加一条里程碑总结（P3.1-C）。
// 指针式：记录完成项与记忆文档自身位置；详细内容留在产出文件，按需读取。
func (a *App) appendMemoryMilestone(sessionID, item string) {
	doc := a.memory.Load(sessionID)
	if doc == "" {
		doc = "# 会话记忆\n\n任务进行中，以下按里程碑记录（指针式：详情见指向的文件）。\n"
	}
	// 幂等：同一待办项只记录一次。
	marker := "已完成：" + item
	if strings.Contains(doc, marker) {
		return
	}
	entry := "- " + marker + "（详见聊天记录与产出文件；未完成项见会话待办清单）"
	_ = a.memory.Append(sessionID, "里程碑", entry)
}

// makeAskBridge 会话应答桥：推送由 waitAsk→say 完成；阻塞等待本会话按钮应答。
// 自动审批模式超时兑底（2026-12「不显示审批就卡住」修复）：AutoApprove 开启时，
// 审批性 ask 阻塞等待超过 5 分钟未应答（典型场景：用户切走会话/前端刷新导致
// 审批卡丢失，或夜间无人值守）→ 自动批准继续，不永久阻塞任务；say 桥已把
// ask 记入会话历史，用户切回后在历史中可见。红线标记（敏感文件写入）不超时放行，
// 继续阻塞等人工（防凭据被静默改写）。
// pendingAskLive 挂起追踪：发出时写入、收到应答/超时/取消时清除，供
// sessionsWithRunning 提取随会话列表下发，前端恢复审批卡。
func (a *App) makeAskBridge(run *taskRun, ctx context.Context) agent.AskFn {
	return func(m message.ClineMessage) (message.AskResponse, string) {
		// 清理残留应答，防止上一轮超时/取消的应答串扰。
		select {
		case <-run.askCh:
		default:
		}
		// 挂起登记：非完成回执的 ask 都可能阻塞引擎（completion_result 是
		// 任务完成回执，不算审批）。
		if m.Ask != message.AskCompletionResult {
			run.pendingAskMu.Lock()
			run.pendingAskLive = &m
			run.pendingAskMu.Unlock()
			defer func() {
				run.pendingAskMu.Lock()
				run.pendingAskLive = nil
				run.pendingAskMu.Unlock()
			}()
		}
		// 自动审批模式：提问性 ask 直接自主决策（与引擎 AutoDecide 同源语义，
		// 双保险：引擎层拦截 + 桥层兑底）；审批性 ask 带超时兑底。
		var timeoutCh <-chan time.Time
		if a.GetConfig().AutoApprove && m.Type == message.TypeAsk {
			switch m.Ask {
			case message.AskFollowup:
				return message.ResponseMessageResponse, "（自动审批模式）按现有信息自行决策继续；确需人工决策的，在最终摘要中说明卡点。"
			case message.AskTool, message.AskCommand, message.AskAPIReqFailed, message.AskMistakeLimit, message.AskAutoApprovalMax, message.AskResumeTask:
				if !strings.Contains(m.Text, redlineAskMarker) {
					timeoutCh = time.After(5 * time.Minute)
				}
			}
		}
		select {
		case r := <-run.askCh:
			return r.response, r.text
		case <-timeoutCh:
			slog.Warn("审批超时自动决策（自动审批模式，前端可能未展示审批卡）",
				"session", run.id, "ask", m.Ask, "text", truncateTitle(m.Text, 120))
			return message.ResponseYes, ""
		case <-ctx.Done():
			return message.ResponseNo, ""
		}
	}
}

// unattendedAskBridge 无人值守 ask 桥（roadmap 自主开发会话专用，2026-10）：
// 审批全类自动应答（对齐 CLI -y / onAsk==nil 的引擎无人值守语义），无人工看管
// 也不阻塞——「点执行后排期任务不生成新会话」的运行时根因修复：旧实现审批卡
// 阻塞等人，没人应答则条目永久 developing、占满并行额度，后续排期永不拉起。
// 红线保留：敏感文件写入（引擎 sensitiveWriteCall 的 ask 带 "[红线]" 前缀标记）
// 自动拒绝，防凭据被静默改写；followup 提问自动答复继续；流错误/错误上限重试。
func unattendedAskBridge(m message.ClineMessage) (message.AskResponse, string) {
	switch m.Ask {
	case message.AskCompletionResult:
		return message.ResponseYes, "" // 完成确认：自动通过（收尾即返回）
	case message.AskFollowup:
		return message.ResponseMessageResponse, "（无人值守）按现有信息自行决策继续；确需人工决策的，在最终摘要中说明卡点。"
	case message.AskTool, message.AskCommand:
		if strings.Contains(m.Text, redlineAskMarker) {
			return message.ResponseNo, "" // 敏感文件写入：拒绝（防凭据被静默改写）
		}
		return message.ResponseYes, "" // 工具/命令审批：放行
	case message.AskAPIReqFailed, message.AskMistakeLimit:
		return message.ResponseYes, "" // 流错误/错误上限：重试续跑
	default:
		return message.ResponseYes, "" // 其余审批类默认放行
	}
}

// redlineAskMarker 引擎敏感文件写入 ask 的红线标记（unattendedAskBridge 据此
// 拒绝放行；文案同时提示用户红线原因）。
const redlineAskMarker = "[红线] 敏感文件写入"

// runParallelSubtask 以独立子会话运行一个 new_task 委派（P2.8 多 Agent 并行协作）。
// 对齐 Zoo-Code delegateParentAndOpenChild：子任务拥有独立会话/引擎/待办，事件按
// 子 sessionId 实时推送（侧栏独立会话流 + ⏳ 徽标）；完成后结果回传父任务，由引擎
// 按 reopenParentFromDelegation 语义回填 tool_result。
// 运行于引擎 fan-out goroutine（父任务阻塞等待聚合结果），不占用会话调度许可，
// 因此父子/兄弟委派可真正并发且无许可互锁。
func (a *App) runParallelSubtask(ctx context.Context, parentID string, st tools.SubtaskResult, depth int) (string, error) {
	a.subSeqMu.Lock()
	a.subSeq++
	seq := a.subSeq
	a.subSeqMu.Unlock()
	childID := fmt.Sprintf("%s-s%d", parentID, seq)
	// 远程可见性（P2.8 子会话上报）：子会话在服务端会话列表投影，移动端/管理端
	// 可见（带 parentId 缩进，不与他端远程任务平级混淆）；上报经 a.remoteClient
	// outbox 可靠投递（未连接时入队，重连补发）。
	a.remoteMu.Lock()
	remoteClient := a.remoteClient
	a.remoteMu.Unlock()
	if remoteClient != nil {
		remoteClient.SendSubtask(remote.SubtaskEvent{
			SessionID: childID, ParentID: parentID,
			Title:  "↳ " + truncateTitle(st.Message, 38),
			Status: remote.TaskStatusRunning,
			Mode:   st.Mode, TS: time.Now().UnixMilli(),
		})
	}

	now := time.Now()
	// 子会话继承父会话工作区 + 数字员工绑定（§9.2：员工会话的委派子任务
	// 仍受同一员工的工作空间/权限网关约束；父 run.workspace 已是员工工作空间）。
	parentWS, parentEmp, parentModel, parentMCPDisabled := "", "", "", false
	if a.runsMu.TryLock() {
		if p := a.runs[parentID]; p != nil {
			parentWS = p.workspace
			parentEmp = p.employeeID
			parentModel = p.model             // 子会话继承父会话自有模型（同父同模型）
			parentMCPDisabled = p.mcpDisabled // 子会话继承父会话 MCP 禁用态
		}
		a.runsMu.Unlock()
	}
	sess := &session.Session{
		ID:          childID,
		ChannelID:   "desktop",
		Title:       "↳ " + truncateTitle(st.Message, 38),
		Mode:        st.Mode,
		Model:       parentModel,
		Workspace:   parentWS,
		EmployeeID:  parentEmp,
		MCPDisabled: parentMCPDisabled, // 子会话继承父会话 MCP 禁用态（同父同权限）
		ParentID:    parentID,          // 侧栏折叠树层级依据（父会话下拉展开子任务执行情况）
		CreatedAt:   now,
		UpdatedAt:   now, // 同设（会话列表按 UpdatedAt 倒序，零值会沉底）
	}
	a.store.Save(sess)
	uiEmit(a.ctx, "kaelvoris:sessions", a.sessionsWithRunning()) // 带运行态：子会话立即亮绿点

	run := &taskRun{id: childID, workspace: parentWS, employeeID: parentEmp, model: parentModel, mcpDisabled: parentMCPDisabled, todo: tools.NewTodoStore(), askCh: make(chan askReply, 1)}
	run.mcpProbe = st.Message                   // MCP 惰性加载：子任务指令作为激活信号（与父会话隔离判定）
	run.todo.Set(st.Todos)                      // 委派即规划（对齐 startSubtask 的 initialTodos 语义）
	childCtx, cancel := context.WithCancel(ctx) // 父任务取消级联到子任务
	run.cancel = cancel
	defer func() {
		cancel()
		a.runsMu.Lock()
		delete(a.runs, childID)
		a.runsMu.Unlock()
		a.registry.Remove(childID)
		// 会话级状态修复（同 runTask）：子任务完成恒报本会话 false，不报全局，
		// 避免其他并发任务把已完成子会话重新标成运行中。
		uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": false, "sessionId": childID})
	}()
	a.runsMu.Lock()
	a.runs[childID] = run
	a.runsMu.Unlock()
	a.registry.Push(run)
	uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": true, "sessionId": childID})

	a.kmu.Lock()
	modeMgr, skillMgr := a.modeMgr, a.skillMgr
	a.kmu.Unlock()

	engine, mcpClients := a.buildRunEngine(childCtx, modeMgr, skillMgr, run, nil)
	defer func() {
		for _, cli := range mcpClients {
			cli.Close()
		}
	}()
	say := a.makeSayBridge(sess, run, childID)
	onAsk := a.makeAskBridge(run, childCtx)

	result, runErr := engine.RunInMode(childCtx, st.Mode, st.Message, true, depth, say, onAsk)
	done := map[string]any{"result": result, "sessionId": childID}
	if runErr != nil {
		done["error"] = runErr.Error()
	}
	// 子会话结束上报（终态投影：done/failed）：服务端会话列表即时刷新状态徽标。
	if remoteClient != nil {
		status := remote.TaskStatusDone
		if runErr != nil {
			status = remote.TaskStatusFailed
		}
		remoteClient.SendSubtask(remote.SubtaskEvent{
			SessionID: childID, ParentID: parentID,
			Title:  "↳ " + truncateTitle(st.Message, 38),
			Status: status,
			Result: truncateTitle(result, 120), Error: func() string {
				if runErr == nil {
					return ""
				}
				return runErr.Error()
			}(),
			Mode: st.Mode, TS: time.Now().UnixMilli(),
		})
	}
	uiEmit(a.ctx, "kaelvoris:done", done)
	uiEmit(a.ctx, "kaelvoris:sessions", a.sessionsWithRunning()) // 带运行态 + 父子层级：绿点/折叠树即时刷新
	return result, runErr
}

// cfgWorkspace 读取当前配置工作区。
func (a *App) cfgWorkspace() string { return a.GetConfig().Workspace }

// truncateTitle 会话标题截断（取首行非空文本）。
func truncateTitle(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// ---------- 绑定：模型选择 ----------

// ModelItem 模型列表条目。
type ModelItem struct {
	ID      string `json:"id"`
	Current bool   `json:"current"`
}

// ListModels 拉取 OpenAI 兼容 /models 列表；失败时回退当前配置模型。
func (a *App) ListModels() ([]ModelItem, error) {
	cfg := a.GetConfig()
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("请先配置 API Base URL")
	}
	url := strings.TrimRight(cfg.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取模型列表失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取模型列表失败: HTTP %d %s", resp.StatusCode, truncateTitle(string(body), 120))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w", err)
	}
	cur := cfg.Model
	out := make([]ModelItem, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, ModelItem{ID: m.ID, Current: m.ID == cur})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) == 0 && cur != "" {
		out = append(out, ModelItem{ID: cur, Current: true})
	}
	return out, nil
}

// SetModel 热切换模型：更新配置并标记内核重建（下一任务生效）。
func (a *App) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("模型不能为空")
	}
	cfg := a.GetConfig()
	cfg.Model = model
	return a.SaveConfig(cfg)
}

// ---------- 绑定：MCP 面板设置 ----------

func mcpConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "mcp-servers.json")
}

// ListMCPServers 读取 MCP 服务器配置列表（经 loadMCPServers 三格式兼容：
// kaelvoris 数组格式 + Claude/Cline mcpServers 映射格式 + 顶层单服务器映射，
// 面板与运行时一致）。
func (a *App) ListMCPServers() []MCPServer {
	if out := a.loadMCPServers(); out != nil {
		return out
	}
	return []MCPServer{}
}

// ImportMCPServers 快速添加：解析粘贴的 MCP 配置 JSON 并直接落盘（整表合并，
// 同名覆盖、异名保留——用户已在面板里配好其他服务器时粘贴导入不应清空它们）。
// 支持三种格式（见 parseMCPConfig）：kaelvoris 数组 / {"mcpServers":{...}} 映射 /
// 顶层单服务器映射 fragment（用户从 Claude/Cline 等工具直贴，如
// {"b1-pro":{"type":"streamable-http","url":...}}）。同名服务器按新配置覆盖
// （用户重贴即更新），其余已配置服务器保持不变。返回保存后的完整列表供前端
// 直接刷新（与 ListMCPServers 一致）。
func (a *App) ImportMCPServers(jsonText string) ([]MCPServer, error) {
	b := []byte(strings.TrimSpace(jsonText))
	// 剥 BOM（粘贴自 PowerShell 写出文件等场景）。
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	servers, n := parseMCPConfig(b)
	if n == 0 {
		return nil, errors.New("无法识别的 MCP 配置格式：支持 kaelvoris 数组 / {\"mcpServers\":{...}} / 顶层单服务器映射（如 {\"名字\":{\"type\":\"streamable-http\",...}}）")
	}
	// 合并：已有配置（磁盘当前态）+ 粘贴导入（同名覆盖）。
	existing := a.loadMCPServers()
	byName := make(map[string]MCPServer, len(existing)+n)
	order := make([]string, 0, len(existing)+n)
	for _, s := range existing {
		if _, dup := byName[s.Name]; !dup {
			order = append(order, s.Name)
		}
		byName[s.Name] = s
	}
	for _, s := range servers {
		if _, dup := byName[s.Name]; !dup {
			order = append(order, s.Name)
		}
		byName[s.Name] = s
	}
	merged := make([]MCPServer, 0, len(order))
	for _, name := range order {
		merged = append(merged, byName[name])
	}
	if err := a.SaveMCPServers(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// MCPToolListResult 工具清单查询结果（MCPServerTools：连接失败时 OK=false 携带原因）。
// 工具条目复用 MCPToolInfo（employee_panel.go 权限目录同型，含 InputSchema）。
type MCPToolListResult struct {
	Name  string        `json:"name"`
	OK    bool          `json:"ok"`
	Tools []MCPToolInfo `json:"tools,omitempty"`
	Error string        `json:"error,omitempty"`
}

// MCPServerTools 查看单个 MCP 服务器支持的工具列表（连接 → 握手 → tools/list，
// 不注册不执行，仅枚举；工具含名称/方法描述/参数 schema）。与 TestMCPServer
// 同一套握手路径（RegisterAll 复用，register 回调只收集不注册），连接即关，
// 不影响运行中的任务。
func (a *App) MCPServerTools(s MCPServer) (MCPToolListResult, error) {
	if strings.TrimSpace(s.Name) == "" {
		s.Name = s.URL // 远程无名称时用 URL 标识（错误信息可读性）
		if strings.TrimSpace(s.Name) == "" {
			s.Name = "?"
		}
	}
	if !s.Enabled {
		return MCPToolListResult{Name: s.Name, Error: "服务器已禁用（开关关闭），如需查看请先打开开关"}, nil
	}
	switch s.Transport {
	case "", "stdio":
		if strings.TrimSpace(s.Command) == "" {
			return MCPToolListResult{}, errors.New("请填写启动命令")
		}
	case "sse", "http":
		if strings.TrimSpace(s.URL) == "" {
			return MCPToolListResult{}, errors.New("请填写服务端 URL")
		}
	default:
		return MCPToolListResult{}, errors.New("不支持的传输类型: " + s.Transport)
	}
	cfg := mcp.ServerConfig{Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Headers: s.Headers}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var toolsList []MCPToolInfo
	cli, _, err := mcp.RegisterAll(ctx, cfg, func(name string, t *mcp.RemoteTool) error {
		toolsList = append(toolsList, MCPToolInfo{
			Name:        t.ServerTool,
			FullName:    name,
			Desc:        t.Desc,
			InputSchema: t.Schema,
		})
		return nil // 只收集不注册：查看语义，不影响任务 Registry
	})
	if cli != nil {
		defer cli.Close()
	}
	if err != nil {
		return MCPToolListResult{Name: s.Name, Error: err.Error()}, nil
	}
	if toolsList == nil {
		toolsList = []MCPToolInfo{}
	}
	return MCPToolListResult{Name: s.Name, OK: true, Tools: toolsList}, nil
}

// SaveMCPServers 保存 MCP 服务器配置（整表替换；P4 接入运行时后生效）。
// 校验只约束启用条目：禁用条目（开关关闭）允许 command/url 为空——用户关闭
// 开关后可能保留占位配置待日后填充，或者只想临时停用；若仍强制校验必填，
// 保存会报错 → 用户误以为已保存（实际未落盘）→ 旧启用配置继续生效，
// 表现为「开关关闭后还是会调用」。（2026-10 MCP 开关失效修复）
func (a *App) SaveMCPServers(servers []MCPServer) error {
	for _, s := range servers {
		if !s.Enabled {
			continue // 禁用条目不校验必填（不会运行）
		}
		if strings.TrimSpace(s.Name) == "" {
			return errors.New("MCP 服务器名称不能为空")
		}
		switch s.Transport {
		case "stdio":
			if strings.TrimSpace(s.Command) == "" {
				return errors.New("stdio 传输需要填写启动命令: " + s.Name)
			}
		case "sse", "http":
			if strings.TrimSpace(s.URL) == "" {
				return errors.New("sse/http 传输需要填写 URL: " + s.Name)
			}
		default:
			return errors.New("不支持的传输类型（stdio|sse|http）: " + s.Name)
		}
	}
	if err := os.MkdirAll(filepath.Dir(mcpConfigPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(servers, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(mcpConfigPath(), b, 0o600)
}

// ---------- 绑定：消息渠道设置（P3 通道：飞书长连接 + 企微回调桥已接入运行时） ----------
//
// 消息渠道规划（doc/01-需求文档 FR-7）：飞书（channel_lark.go 长连接）、
// 企业微信（channel_wecom.go 本地回调 + webhook 推送）、个人微信（仅配置面，
// wechat-robot-go 运行时后续版本接入）。渠道凭据保存后热重载生效。

// ChannelConfig 单个消息渠道的接入配置。字段按渠道类型取用：
//   - lark（飞书机器人）：AppID/AppSecret（长连接接收事件）
//   - wecom（企业微信）：BotID+BotSecret（智能机器人 WS 长连接，免公网回调，
//     优先级最高）或 WebhookKey（群机器人推送）或 CorpID+CorpSecret+Token+
//     EncodingAESKey（应用消息双向交互，本地回调接收企微推送，CallbackPort 选填）
//   - wechat（个人微信，wechat-robot-go）：DeviceID（登录设备标识，风控敏感，默认关闭）
//   - im（通用 IM 接入，平台无关）：平台地址（APIBase/WebhookURL）
//     与收发格式（GroupInMap/OutTemplate）全部面板可配，代码不写死；
//     PublicID+PubSecret（公共号双向交互，回调接收用户消息 + pubsend 定向回传）；
//     群机器人（WebhookToken/WebhookURL 单向推送）与群消息接收（GroupHookIDs
//     HTTP 端点，hookID 分配群组）同渠道可任意配置其一或全配置（全配置时
//     群消息进入后回复经群机器人 Webhook 推回群，sendText 群会话分流）
type ChannelConfig struct {
	Type           string `json:"type"`                     // lark | wecom | wechat | im
	Name           string `json:"name"`                     // 渠道显示名
	Enabled        bool   `json:"enabled"`                  // 启用（运行时接入后生效）
	AppID          string `json:"appId,omitempty"`          // lark：应用 App ID
	AppSecret      string `json:"appSecret,omitempty"`      // lark：应用 App Secret
	WebhookKey     string `json:"webhookKey,omitempty"`     // wecom：群机器人 Webhook Key
	BotID          string `json:"botId,omitempty"`          // wecom：智能机器人 Bot ID（WS 长连接模式）
	BotSecret      string `json:"botSecret,omitempty"`      // wecom：智能机器人 Secret（WS 长连接模式）
	CorpID         string `json:"corpId,omitempty"`         // wecom：企业 ID（应用消息模式）
	AgentID        string `json:"agentId,omitempty"`        // wecom：应用 AgentID
	CorpSecret     string `json:"corpSecret,omitempty"`     // wecom：应用 Secret
	Token          string `json:"token,omitempty"`          // wecom：应用消息回调 Token（企微后台「接收消息」设置）
	EncodingAESKey string `json:"encodingAesKey,omitempty"` // wecom：回调消息加解密密钥（43 位）
	CallbackPort   int    `json:"callbackPort,omitempty"`   // wecom：本地回调监听端口（默认 9880）
	DeviceID       string `json:"deviceId,omitempty"`       // wechat：设备标识（wechat-robot-go）
	// im（通用 IM 接入；平台地址与收发格式全部可配，不写死）
	APIBase      string            `json:"apiBase,omitempty"`      // im：平台 API 根（必填，代码不写死平台地址）
	RemoteRelay  bool              `json:"remoteRelay,omitempty"`  // im：公网中继接收（本地 PC 无外网：服务端代持回调入口 → WS 下发本节点执行）
	WebhookURL   string            `json:"webhookUrl,omitempty"`   // im：群机器人完整 Webhook URL（任意平台通用；空 = 回退 APIBase+WebhookToken 形态）
	PublicID     string            `json:"publicId,omitempty"`     // im：公共号 ID（pub，XT-xxx，公共号模式必需）
	PubSecret    string            `json:"pubSecret,omitempty"`    // im：公共号密钥（pubkey，pubtoken 签名用）
	EID          string            `json:"eid,omitempty"`          // im：企业注册号 no（from/to 用，公共号模式必需）
	WebhookToken string            `json:"webhookToken,omitempty"` // im：群机器人授权码（yzjtoken，APIBase 回退形态用）
	GroupHookIDs string            `json:"groupHookIds,omitempty"` // im：群消息接收端点 hookID 列表（逗号分隔，每个 ID 对应一个群组，URL /im/group/<hookID>）
	GroupInMap   map[string]string `json:"groupInMap,omitempty"`   // im：群消息入参字段映射（目标=源路径，如 text=Payload.Content；未配置字段回退宽松解析）
	OutTemplate  string            `json:"outTemplate,omitempty"`  // im：Webhook 发送出参模板（{{text}} 占位符；空 = 默认 {"content":"..."}）
	AllowedUsers string            `json:"allowedUsers,omitempty"` // 允许的用户标识（逗号分隔，空 = 不限制）
	EmployeeID   string            `json:"employeeId,omitempty"`   // §10.3 绑定唯一数字员工（启用渠道必填）
	// ---- 岗位绑定（doc/09-设计方案 §3：绑定字段只存资源项配置侧）----
	// PositionID 绑定岗位快照：绑定员工保存时自动回填员工当前岗位（§3
	// 「员工调岗不自动跟随」）；仅绑岗位时按 AssignRule 从岗位池分配员工。
	PositionID string `json:"positionId,omitempty"`
	// AssignRule 岗位池分配规则：random（默认）/ round_robin /
	// least_loaded（二期枚举位）；空值 = random（§2.1）。
	AssignRule string `json:"assignRule,omitempty"`
	// LastAssignedIndex round_robin 轮转游标（§2.1：存渠道配置内，随
	// channels.json 原子落盘；重启丢内存态可接受）。0 = 尚未分配（零值
	// 兼容旧 JSON），round_robin 分配后由执行链路回写并随 SaveChannels 落盘。
	LastAssignedIndex int `json:"lastAssignedIndex,omitempty"`
}

// channelsConfigPathOverride 测试钩子：非空时替代默认路径（binding_model_test.go
// 用临时目录验证 SaveChannels 落盘往返，避免碰真实 ~/.kaelvoris/channels.json）。
var channelsConfigPathOverride string

func channelsConfigPath() string {
	if channelsConfigPathOverride != "" {
		return channelsConfigPathOverride
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "channels.json")
}

// ListChannels 读取消息渠道配置列表。
func (a *App) ListChannels() []ChannelConfig {
	b, err := os.ReadFile(channelsConfigPath())
	if err != nil {
		return []ChannelConfig{}
	}
	var out []ChannelConfig
	if json.Unmarshal(b, &out) != nil {
		return []ChannelConfig{}
	}
	return out
}

// SaveChannels 保存消息渠道配置（整表替换）。校验规则按渠道类型：
// lark 必填 AppID+AppSecret；wecom 必填 BotID+BotSecret（智能机器人 WS）、
// WebhookKey 或 CorpID+CorpSecret（三者其一）；
// wechat 必填 DeviceID；im（通用 IM）需公共号三要素（PublicID+PubSecret+EID）
// 或「群机器人 + 群消息接收」组合（WebhookToken/WebhookURL 与 GroupHookIDs
// 任意配置其一或全配置）至少一项；InMap 源路径不得为空（配置了映射但源为空 = 静默失效，提前拦）；
// 启用渠道绑定校验：doc/09-设计方案 §4.4（员工或岗位至少其一；员工存在且
// 可运行且有岗位；绑员工自动回填岗位快照；分配规则枚举合法），见函数内注释。
func (a *App) SaveChannels(channels []ChannelConfig) error {
	// 索引遍历：启用渠道绑定员工时需回填岗位快照到切片（§3 保存回填）。
	for i := range channels {
		c := channels[i]
		if strings.TrimSpace(c.Name) == "" {
			return errors.New("渠道名称不能为空")
		}
		switch c.Type {
		case "lark":
			if strings.TrimSpace(c.AppID) == "" || strings.TrimSpace(c.AppSecret) == "" {
				return errors.New("飞书渠道需要填写 App ID 与 App Secret: " + c.Name)
			}
		case "wecom":
			hasBot := strings.TrimSpace(c.BotID) != "" && strings.TrimSpace(c.BotSecret) != ""
			hasApp := strings.TrimSpace(c.CorpID) != "" && strings.TrimSpace(c.CorpSecret) != ""
			if strings.TrimSpace(c.WebhookKey) == "" && !hasBot && !hasApp {
				return errors.New("企业微信渠道需要填写 Bot ID+Secret（智能机器人）、Webhook Key（群机器人）或 CorpID+CorpSecret（应用消息）: " + c.Name)
			}
			// 智能机器人 WS 模式仅需 BotID+Secret（免公网回调），不校验回调三元组。
			if hasBot {
				break
			}
			// 应用消息模式（CorpID+CorpSecret）必须配置完整回调三元组 + AgentID：
			// 缺 Token/EncodingAESKey 时企微后台「接收消息」验证必失败、推送
			// 消息解密必失败——此前静默放行，用户侧表现为「配置后不生效」。
			if strings.TrimSpace(c.CorpID) != "" && strings.TrimSpace(c.CorpSecret) != "" {
				missing := ""
				if strings.TrimSpace(c.AgentID) == "" {
					missing = "Agent ID"
				}
				if strings.TrimSpace(c.Token) == "" {
					if missing != "" {
						missing += "、"
					}
					missing += "回调 Token"
				}
				if k := len(strings.TrimSpace(c.EncodingAESKey)); k != 43 && k != 44 {
					if missing != "" {
						missing += "、"
					}
					missing += "EncodingAESKey（43 位）"
				}
				if missing != "" {
					return errors.New("企业微信应用消息模式缺少必填项 " + missing + "（企微后台「接收消息」设置中生成）: " + c.Name)
				}
			}
		case "wechat":
			if strings.TrimSpace(c.DeviceID) == "" {
				return errors.New("个人微信渠道需要填写设备标识: " + c.Name)
			}
		case "im":
			// API 根必填：代码不内置任何平台地址（URL 全部配置驱动），
			// 群机器人动态拼接与公共号 pubsend 都依赖它。
			if strings.TrimSpace(c.APIBase) == "" && strings.TrimSpace(c.WebhookURL) == "" {
				return errors.New("IM 渠道需要填写 API 根（平台 API 地址，如 https://im.example.com）或完整 Webhook URL: " + c.Name)
			}
			// 公共号双向（PublicID+PubSecret+EID）或「群机器人 + 群消息接收」
			// 组合：群机器人（WebhookURL 或 WebhookToken，单向推送）与群消息
			// 接收（GroupHookIDs，HTTP 端点 hookID 分配群组）同渠道可任意
			// 配置其一或全配置（全配置时群消息进入后回复经 Webhook 推回群，
			// 见 channel_im.go Start/sendText 群会话分流）；至少配其一。
			hasPub := strings.TrimSpace(c.PublicID) != "" && strings.TrimSpace(c.PubSecret) != "" && strings.TrimSpace(c.EID) != ""
			hasWebhook := strings.TrimSpace(c.WebhookURL) != "" || strings.TrimSpace(c.WebhookToken) != ""
			hasGroup := strings.TrimSpace(c.GroupHookIDs) != ""
			if !hasPub && !hasWebhook && !hasGroup {
				return errors.New("IM 渠道需要配置以下任意一项：公共号三要素 Public ID+Pub Secret+企业 EID（公共号）；群机器人 Webhook URL 或 Token、群接收 Hook ID（两者可任意配置其一或全配置）: " + c.Name)
			}
			// 入参映射：源路径不得为空（配置了映射但源为空 = 静默失效）。
			for k, v := range c.GroupInMap {
				if strings.TrimSpace(v) == "" {
					return errors.New("IM 渠道入参映射字段 " + k + " 的源路径为空（删除该行或填写推送报文中的字段路径）: " + c.Name)
				}
			}
			// 出参模板：允许空（默认 content 形态），但若填写须含 {{text}} 占位符，
			// 否则回复文本无处安放（发出去永远是固定报文）。
			if t := strings.TrimSpace(c.OutTemplate); t != "" && !strings.Contains(t, "{{text}}") {
				return errors.New("IM 渠道出参模板需包含 {{text}} 占位符（回复文本替换位置）: " + c.Name)
			}
		default:
			return errors.New("不支持的渠道类型（lark|wecom|wechat|im）: " + c.Name)
		}
		// doc/09-设计方案 §4.4 保存校验（负路径，权限类功能铁律；对启用与
		// 停用渠道的绑定字段都生效，存量旧渠道未重新保存不受影响）：
		//   - 绑定员工时自动回填员工当前岗位为 positionId 快照（§3 调岗不
		//     自动跟随，下次保存重新校验）；员工未分配岗位 = 硬错误（§4.4
		//     「指定员工须有岗位」，面板待办清单引导补岗位）；
		//   - 分配规则枚举合法 + 绑定组合：复用 cron.ValidateBinding，不另起
		//     平行校验（§11-4）；
		//   - 指定岗位须存在（池空允许保存但触发时兑底报错，§4.4 括号语义）；
		//   - 启用渠道必须「员工或岗位」至少其一（全空 = 裸跑，拒绝保存）。
		empID := strings.TrimSpace(c.EmployeeID)
		posID := strings.TrimSpace(c.PositionID)
		if empID != "" {
			emp, ok := a.empStore.Get(empID)
			if !ok {
				return errors.New("渠道绑定的数字员工不存在: " + c.Name + " → " + empID)
			}
			if posID == "" {
				if strings.TrimSpace(emp.PositionID) == "" {
					return errors.New("渠道绑定的数字员工未分配岗位，请先在员工表单中指定岗位（保存时将自动回填岗位快照）: " + c.Name + " → " + emp.Name)
				}
				c.PositionID = emp.PositionID // 回填岗位快照（§3）
				posID = emp.PositionID
			} else if posID != strings.TrimSpace(emp.PositionID) {
				return errors.New("渠道绑定岗位与员工当前岗位不一致（员工已调岗，请重新选择员工以刷新快照）: " + c.Name + " → " + emp.Name)
			}
			if c.Enabled && !emp.Runnable(false) {
				return errors.New("渠道绑定的数字员工未启用，渠道无法就绪: " + c.Name + " → " + emp.Name)
			}
		}
		if err := cron.ValidateBinding(empID, posID, strings.TrimSpace(c.AssignRule)); err != nil {
			return errors.New("渠道绑定校验失败: " + c.Name + " → " + err.Error())
		}
		if posID != "" && !a.positionExists(posID) {
			return errors.New("渠道绑定的岗位不存在: " + c.Name + " → " + posID)
		}
		if c.Enabled && empID == "" && posID == "" {
			return errors.New("启用渠道必须绑定数字员工或岗位（裸跑渠道不就绪）: " + c.Name)
		}
		channels[i] = c
	}
	if err := os.MkdirAll(filepath.Dir(channelsConfigPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(channels, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(channelsConfigPath(), b, 0o600); err != nil {
		return err
	}
	// 运行时桥热重载：按新配置重启飞书长连接（channel_lark.go）。
	a.reloadChannels()
	// IM 中继：渠道表变更后同步服务端路由投影（agent.im_state；中继渠道
	// 五元组/启停变更 → 指纹与注册表刷新，回调 URL 随指纹更新）。
	a.remoteSendIMRelayState()
	return nil
}

// ---------- 绑定：Skills 面板设置 ----------

func skillSettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "skills-settings.json")
}

// loadSkillSettings 读取禁用清单。
func loadSkillSettings() SkillSettings {
	b, err := os.ReadFile(skillSettingsPath())
	if err != nil {
		return SkillSettings{}
	}
	var s SkillSettings
	if json.Unmarshal(b, &s) != nil {
		return SkillSettings{}
	}
	return s
}

// ListSkillViews 技能列表（含启用状态）。
func (a *App) ListSkillViews() []SkillView {
	all := a.ListSkills()
	disSet := map[string]bool{}
	for _, n := range loadSkillSettings().Disabled {
		disSet[n] = true
	}
	out := make([]SkillView, 0, len(all))
	for _, s := range all {
		out = append(out, SkillView{Metadata: s, Disabled: disSet[s.Name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SetSkillEnabled 启用/禁用技能（禁用后系统提示词 available_skills 段将排除；
// 变更标记内核重建，下一任务生效）。
func (a *App) SetSkillEnabled(name string, enabled bool) error {
	var kept []string
	for _, n := range loadSkillSettings().Disabled {
		if n != name {
			kept = append(kept, n)
		}
	}
	if !enabled {
		kept = append(kept, name)
	}
	set := SkillSettings{Disabled: dedup(kept)}
	b, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(skillSettingsPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(skillSettingsPath(), b, 0o600); err != nil {
		return err
	}
	a.invalidate()
	return nil
}

// dedup 字符串去重保序。
func dedup(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ---------- 适配器（与 CLI main.go 相同） ----------

// errSkillLoad 适配 skills 错误。
type errSkillLoad struct{ err error }

func (e errSkillLoad) Error() string { return e.err.Error() }

// availableSkillsXML 生成 <available_skills> 段（对齐 system.ts skills section）；
// 排除用户禁用的技能（skills-settings.json disabled 清单）。
// 崩溃修复 2026-10：nil 防御——ForMode 上 m.mu.RLock() 在 nil 接收者上是解引用
// panic；任何宿主漏传 nil 内核时此处直接返回空，不让 panic 上穿。
func availableSkillsXML(modeMgr *modes.Manager, skillMgr *skills.Manager) string {
	if skillMgr == nil || modeMgr == nil {
		return ""
	}
	disabled := map[string]bool{}
	for _, n := range loadSkillSettings().Disabled {
		disabled[n] = true
	}
	var list []skills.Metadata
	for _, s := range skillMgr.ForMode(modes.DefaultModeSlug) {
		if !disabled[s.Name] {
			list = append(list, s)
		}
	}
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<available_skills>\n")
	for _, s := range list {
		b.WriteString("  <skill>\n")
		b.WriteString("    <name>" + s.Name + "</name>\n")
		b.WriteString("    <description>" + s.Description + "</description>\n")
		b.WriteString("    <location>" + filepath.ToSlash(s.Path) + "</location>\n")
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</available_skills>")
	return b.String()
}

// skillsXMLEmp 员工级技能过滤（§4 技能权限定义侧）：SkillsDisabled 时整段
// 不注入（skill 工具已不注册，提示词侧同步封死）；否则剔除黑名单命中项。
// emp 为 nil 或无过滤配置时原样返回。按整段 <skill>…</skill> 块过滤，
// 块内 <name> 与 SkillsDeny 规则做 MatchGlob 匹配。
func skillsXMLEmp(xml string, emp *employee.Employee) string {
	if emp == nil || xml == "" {
		return xml
	}
	if emp.SkillsDisabled {
		return ""
	}
	if len(emp.SkillsDeny) == 0 {
		return xml
	}
	const blockHead = "  <skill>\n"
	blocks := strings.Split(xml, blockHead)
	var kept []string
	kept = append(kept, blocks[0])
	for _, blk := range blocks[1:] {
		name := skillNameFromBlock(blk)
		if name != "" && !emp.SkillAllowed(name) {
			continue
		}
		kept = append(kept, blockHead+blk)
	}
	out := strings.Join(kept, "")
	// 过滤后无任何技能项：整段移除（对齐 availableSkillsXML 空列表语义）
	if !strings.Contains(out, blockHead) {
		return ""
	}
	return out
}

// skillNameFromBlock 从单个 <skill> 块文本中提取 <name> 标签内容。
func skillNameFromBlock(blk string) string {
	i := strings.Index(blk, "<name>")
	if i < 0 {
		return ""
	}
	j := strings.Index(blk[i:], "</name>")
	if j < 0 {
		return ""
	}
	return blk[i+len("<name>") : i+j]
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
