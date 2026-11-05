// kaelvoris CLI 入口：P1 最小闭环验证。
//
// 用法：
//
//	kaelvoris -workspace <dir> -task "实现一个函数"
//	echo "任务" | kaelvoris
//
// 环境变量：KAELVORIS_BASE_URL / KAELVORIS_API_KEY / KAELVORIS_MODEL
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"kaelvoris/internal/agent"
	"kaelvoris/internal/condense"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/modes"
	"kaelvoris/internal/skills"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// stdin 共享读取器：避免多处 NewReader 缓冲互相吞噬输入。
var stdin = bufio.NewReader(os.Stdin)

func main() {
	var (
		workspace = flag.String("workspace", ".", "workspace directory for the agent")
		baseURL   = flag.String("base-url", envOr("KAELVORIS_BASE_URL", "https://api.deepseek.com"), "OpenAI-compatible base URL")
		apiKey    = flag.String("api-key", os.Getenv("KAELVORIS_API_KEY"), "API key")
		model     = flag.String("model", envOr("KAELVORIS_MODEL", "deepseek-chat"), "model id")
		autoYes   = flag.Bool("y", false, "auto-approve all asks (YOLO mode)")
		task      = flag.String("task", "", "task description (omit to read from stdin)")
		// P3/B5 上下文治理：-context-window 启用滑动窗口截断（0 = 关闭）。
		contextWindow   = flag.Int("context-window", 0, "model context window in tokens (0 = disable context management)")
		condensePercent = flag.Int("condense-percent", 90, "context usage percent that triggers truncation")
	)
	flag.Parse()

	if *task == "" {
		// stdin 读取任务（管道模式；交互模式请使用 -task）。
		sc := bufio.NewScanner(os.Stdin)
		var lines []string
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		*task = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	if *task == "" {
		fmt.Fprintln(os.Stderr, "usage: kaelvoris -task \"...\"  (or pipe via stdin)")
		os.Exit(2)
	}
	if *apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: API key required: set KAELVORIS_API_KEY or pass -api-key")
		os.Exit(2)
	}

	// ---- 组装内核（对齐架构文档三层结构：工具注册 → LLM Provider → Agent 引擎）----
	// P2：模式系统（.kaelvoris/modes.json / .roomodes 自定义）+ 技能系统 + TodoStore。
	modeMgr := modes.NewManager()
	if err := modeMgr.LoadProject(*workspace); err != nil {
		fmt.Fprintf(os.Stderr, "warn: load custom modes: %v\n", err)
	}
	home, _ := os.UserHomeDir()
	skillMgr := skills.NewManager(home, *workspace)

	todoStore := tools.NewTodoStore()
	registry := tools.NewRegistry()
	must(registry.Register(&tools.ReadFileTool{Workspace: *workspace}))
	must(registry.Register(&tools.WriteFileTool{Workspace: *workspace}))
	must(registry.Register(&tools.ApplyDiffTool{Workspace: *workspace}))
	must(registry.Register(&tools.ListFilesTool{Workspace: *workspace}))
	must(registry.Register(&tools.SearchFilesTool{Workspace: *workspace}))
	// 沙箱：KAELVORIS_SANDBOX=off|job 环境变量覆盖（默认 job，与桌面端一致）。
	sandboxCfg := tools.SandboxConfig{Mode: tools.SandboxModeJob}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KAELVORIS_SANDBOX"))) {
	case "off", "0", "false":
		sandboxCfg.Mode = tools.SandboxModeOff
	case "job", "":
		// 默认 job（纯杀树保证，limits 全 0 = 不限资源）。
	}
	sandbox := tools.NewSandboxFactory(sandboxCfg)
	must(registry.Register(&tools.ShellTool{Runner: &tools.CommandRunner{
		Workspace:  *workspace,
		TimeoutSec: 120,
		DenyList:   tools.DefaultDenyList,
		Sandbox:    sandbox,
	}}))
	// 后台模式 + shell_output：CLI 入口与桌面端一致，支持长驻进程异步执行。
	bgMgr := tools.NewBackgroundManager(*workspace)
	bgMgr.Sandbox = sandbox
	unifiedMgr := tools.NewPersistentShellManager(*workspace)
	unifiedMgr.Sandbox = sandbox
	must(registry.Register(&tools.UnifiedExecTool{Mgr: unifiedMgr, Bg: bgMgr}))
	must(registry.Register(&tools.ShellOutputTool{Mgr: bgMgr}))
	must(registry.Register(&tools.AskFollowupTool{}))
	must(registry.Register(&tools.AttemptCompletionTool{}))
	must(registry.Register(&tools.UpdateTodoListTool{Store: todoStore}))
	// 无 SubtaskRunner（CLI 单进程串行）：new_task 保持 mustBeLastTool 串行语义。
	must(registry.Register(&tools.NewTaskTool{}))
	must(registry.Register(&tools.SpawnAgentTool{}))
	must(registry.Register(&tools.WaitAgentTool{}))
	must(registry.Register(&tools.SwitchModeTool{}))
	must(registry.Register(&tools.DesignSystemTool{}))
	// open-design 全量融合：design_template（design-templates 模板浏览/加载）。
	must(registry.Register(&tools.DesignTemplateTool{}))
	// 诊断三件套：process_list / file_locker / http_request（与桌面端一致）。
	must(registry.Register(&tools.ProcessListTool{}))
	must(registry.Register(&tools.FileLockerTool{}))
	must(registry.Register(&tools.HTTPRequestTool{}))
	// 2026-10 能力补全（与桌面端 buildTools 对齐）：web_fetch 网页正文抓取、
	// web_search 网页搜索、arxiv_search 学术检索、pdf_read PDF 文本提取、
	// speech_to_text 语音转文字（复用 Speech 端点配置）、email_send 邮件发送
	// （env 注入 SMTP 配置）、notion 笔记（env 注入 Notion APIKey）、
	// browser_open/eval/screenshot 浏览器自动化（CDP，本机 Edge/Chrome）。
	must(registry.Register(&tools.WebFetchTool{}))
	must(registry.Register(&tools.WebSearchTool{}))
	must(registry.Register(&tools.ArxivSearchTool{}))
	must(registry.Register(&tools.PDFReadTool{}))
	must(registry.Register(&tools.SpeechToTextTool{Settings: func() tools.SpeechSettings {
		return tools.SpeechSettings{
			BaseURL: os.Getenv("KAELVORIS_SPEECH_BASE_URL"),
			Model:   os.Getenv("KAELVORIS_SPEECH_MODEL"),
			APIKey:  os.Getenv("KAELVORIS_SPEECH_API_KEY"),
		}
	}}))
	// 聊天媒体生成（与桌面端 buildTools 对齐）：generate_image/generate_video/
	// generate_speech，配置经 KAELVORIS_IMAGE_*/KAELVORIS_VIDEO_* 环境变量注入
	//（空 = 未配置时工具报错引导设置，不阻断其余任务）。
	must(registry.Register(&tools.GenerateImageTool{Settings: func() tools.ImageGenSettings {
		return tools.ImageGenSettings{
			BaseURL: os.Getenv("KAELVORIS_IMAGE_BASE_URL"),
			Model:   os.Getenv("KAELVORIS_IMAGE_MODEL"),
			APIKey:  os.Getenv("KAELVORIS_IMAGE_API_KEY"),
		}
	}}))
	must(registry.Register(&tools.GenerateVideoTool{Settings: func() tools.VideoGenSettings {
		return tools.VideoGenSettings{
			BaseURL: os.Getenv("KAELVORIS_VIDEO_BASE_URL"),
			Model:   os.Getenv("KAELVORIS_VIDEO_MODEL"),
			APIKey:  os.Getenv("KAELVORIS_VIDEO_API_KEY"),
			Mode:    os.Getenv("KAELVORIS_VIDEO_MODE"),
			Size:    os.Getenv("KAELVORIS_VIDEO_SIZE"),
		}
	}}))
	must(registry.Register(&tools.GenerateSpeechTool{Settings: func() tools.SpeechSettings {
		return tools.SpeechSettings{
			BaseURL: os.Getenv("KAELVORIS_SPEECH_BASE_URL"),
			Model:   os.Getenv("KAELVORIS_SPEECH_MODEL"),
			APIKey:  os.Getenv("KAELVORIS_SPEECH_API_KEY"),
		}
	}}))
	must(registry.Register(&tools.EmailSendTool{Settings: func() tools.EmailSettings {
		return tools.EmailSettings{
			SMTPHost: os.Getenv("KAELVORIS_SMTP_HOST"),
			SMTPPort: atoiSafe(os.Getenv("KAELVORIS_SMTP_PORT"), 465),
			SMTPUser: os.Getenv("KAELVORIS_SMTP_USER"),
			SMTPPass: os.Getenv("KAELVORIS_SMTP_PASS"),
			FromAddr: os.Getenv("KAELVORIS_SMTP_FROM"),
		}
	}}))
	must(registry.Register(&tools.NotionTool{Settings: func() tools.NotionSettings {
		return tools.NotionSettings{APIKey: os.Getenv("KAELVORIS_NOTION_KEY")}
	}}))
	browserTool := &tools.BrowserTool{}
	must(registry.Register(browserTool))
	must(registry.Register(&tools.BrowserEvalTool{Browser: browserTool}))
	must(registry.Register(&tools.BrowserScreenshotTool{Browser: browserTool}))
	// 2026-10 深度嵌入（与桌面端 buildTools 对齐）：browser_act/snapshot/tabs
	// + desktop_windows/screenshot/act（Windows 桌面自动化）。
	must(registry.Register(&tools.BrowserActTool{Browser: browserTool}))
	must(registry.Register(&tools.BrowserSnapshotTool{Browser: browserTool}))
	must(registry.Register(&tools.BrowserTabsTool{Browser: browserTool}))
	if tools.DesktopSupported() {
		must(registry.Register(&tools.DesktopWindowsTool{}))
		must(registry.Register(&tools.DesktopScreenshotTool{}))
		must(registry.Register(&tools.DesktopActTool{}))
	}
	must(registry.Register(&tools.SkillTool{
		Loader: func(name string) (string, error) {
			c, err := skillMgr.Load(name)
			if err != nil {
				return "", errSkillLoad{err}
			}
			return c.Body, nil
		},
		Names: skillMgr.Names,
		// 技能包内文件读取（内置技能 references/ 二段式加载）。
		ReadFile: skillMgr.ReadFile,
	}))

	client := llm.NewClient(*baseURL, *apiKey, *model)
	engine := agent.New(client, registry, nil)
	engine.Modes = modeMgr
	engine.Todos = todoStore
	engine.MaxSubtaskDepth = 3
	// P3/B5 上下文治理：-context-window 启用滑动窗口截断（0 = 关闭）。
	engine.ContextWindowTokens = *contextWindow
	engine.ReservedOutputTokens = 4096
	engine.TruncationFrac = condense.DefaultFrac
	engine.CondenseTriggerPercent = *condensePercent
	engine.SkillsXML = func() string { return availableSkillsXML(modeMgr, skillMgr) }
	if abs, err := filepath.Abs(*workspace); err == nil {
		engine.Workspace = abs
	}
	engine.Language = "zh-CN"

	// 自动审批（移植 AutoApprovalHandler 简版）：
	//   -y        → 全部自动批准（YOLO）；
	//   交互模式  → 返回 false 交由 onAsk 阻塞询问（CLI 读 Y/N；飞书卡片/微信文本同理替换）。
	if *autoYes {
		engine.AutoApprove = func(message.ClineMessage) bool { return true }
	} else {
		engine.AutoApprove = func(message.ClineMessage) bool { return false }
	}
	// 全AI自动决策：-y 时同步开启（ask_followup 不阻塞，模型自主决策）。
	engine.AutoDecide = *autoYes

	// say 事件渲染（CLI 形态；Wails / 飞书 / 微信通道层替换此渲染器即可复用内核）。
	var printed int // 当前流式块已输出字节数（partial say 携带累计全文 → 差量输出）
	say := func(m message.ClineMessage) {
		switch {
		case m.Type == message.TypeSay && m.Say == message.SayText:
			if m.Partial {
				// 引擎发送累计全文 → 仅输出新增差量（对齐 CLI 流式覆盖渲染语义）。
				if len(m.Text) > printed {
					fmt.Print(m.Text[printed:])
					printed = len(m.Text)
				}
			} else {
				// 定型文本：补齐剩余部分并换行。
				if len(m.Text) > printed {
					fmt.Print(m.Text[printed:])
				}
				fmt.Println()
				printed = 0
			}
		case m.Say == message.SayTool:
			var info message.ToolCallInfo
			_ = json.Unmarshal([]byte(m.Text), &info)
			fmt.Printf("\n[tool] %s path=%s cmd=%q\n", info.Tool, info.Path, info.Cmd)
			printed = 0
		case m.Say == message.SaySubtaskStarted:
			fmt.Printf("\n[orchestrator] %s\n", m.Text)
			printed = 0
		case m.Say == message.SaySubtaskFinished:
			fmt.Printf("\n[orchestrator] subtask finished: %s\n", m.Text)
			printed = 0
		case m.Say == message.SayModeSwitched:
			fmt.Printf("\n[mode] switched to %s\n", m.Text)
			printed = 0
		case m.Type == message.TypeAsk:
			fmt.Printf("\n[ask:%s] %s\n", m.Ask, m.Text)
			printed = 0
		case m.Say == message.SayAPIReqStarted:
			fmt.Print("\n... API request ...")
		case m.Say == message.SayAPIReqFinished:
			fmt.Print(" ...done\n")
			printed = 0
		case m.Say == message.SayError:
			fmt.Printf("\n[error] %s\n", m.Text)
			printed = 0
		}
	}

	// onAsk：CLI 阻塞读入应答（AskFn 接口，多通道时替换为按钮/文本应答路由）。
	onAsk := func(m message.ClineMessage) (message.AskResponse, string) {
		switch m.Ask {
		case message.AskFollowup:
			fmt.Print("> ")
			text, _ := stdin.ReadString('\n')
			return message.ResponseMessageResponse, strings.TrimSpace(text)
		case message.AskCommand, message.AskTool:
			fmt.Print("approve? [y/N]: ")
			text, _ := stdin.ReadString('\n')
			if strings.EqualFold(strings.TrimSpace(text), "y") {
				return message.ResponseYes, ""
			}
			return message.ResponseNo, ""
		default:
			// mistake_limit / api_req_failed / completion：空行继续，输入文本则反馈。
			fmt.Print("[enter=continue, text=feedback, n=stop] ")
			text, _ := stdin.ReadString('\n')
			text = strings.TrimSpace(text)
			switch {
			case text == "":
				return message.ResponseYes, ""
			case strings.EqualFold(text, "n"):
				return message.ResponseNo, ""
			default:
				return message.ResponseMessageResponse, text
			}
		}
	}

	result, err := engine.Run(context.Background(), *task, say, onAsk)
	tools.CloseBrowser() // 浏览器进程随任务结束 kill（防孤儿 Edge）
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n=== 任务完成 ===\n%s\n", result)
}

// errSkillLoad 适配 skills 错误（避免工具层直接依赖 skills 包）。
type errSkillLoad struct{ err error }

func (e errSkillLoad) Error() string { return e.err.Error() }

// availableSkillsXML 生成 <available_skills> 段（对齐 system.ts skills section）。
// 崩溃修复 2026-10：nil 防御（与 app.go 同名函数一致），ForMode 在 nil 接收者上 panic。
func availableSkillsXML(modeMgr *modes.Manager, skillMgr *skills.Manager) string {
	if skillMgr == nil || modeMgr == nil {
		return ""
	}
	list := skillMgr.ForMode(modes.DefaultModeSlug)
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

// envOr 读取环境变量默认值。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// atoiSafe 环境变量端口安全转换（默认值兜底）。
func atoiSafe(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}
