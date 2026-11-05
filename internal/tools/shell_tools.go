// Package tools shell 与交互工具。
//
// shell / shell_output 移植自 Zoo-Code ExecuteCommandTool 与 ReadCommandOutputTool；
// ask_followup / attempt_completion 移植自同名工具——二者不产生副作用，
// 而是向 Agent 引擎返回特殊标记，由引擎转为 ask 消息阻塞等待通道应答。
package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
		"fmt"
		"io"
		"log/slog"
		"os/exec"
		"runtime"
		"strings"
		"time"
		"unicode/utf16"
		"unicode/utf8"

		"golang.org/x/text/encoding/simplifiedchinese"

		"kaelvoris/internal/sysproc"
	)

// bytesReader 避免直接 import bytes 的别名。
func bytesReader(b []byte) io.Reader { return strings.NewReader(string(b)) }

// ---------- Windows 引号/编码修复（2026-09-27 复盘） ----------
//
// 根因：Windows 下经 cmd /C 转发整行命令时，cmd.exe 按自身规则二次解析
// 引号——提交信息中的双引号被剥离/截断，git 把剩余词当 pathspec：
//
//	error: pathspec '远程中心增强（任务过滤/删除）+' did not match any file(s) known to git
//
// 且 cmd 控制台代码页默认 GBK，中文经管道投递会静默乱码。复现脚本验证：
// 同一条 git commit，cmd /C 报错，argv 参数化直跑与 powershell 均保真。
//
// 修复策略（最小侵入）：对「纯 git 子命令」（本项目最高频的引号/编码受害
// 场景）解析为 argv 后 exec.Command("git", ...) 参数化直跑，完全绕过
// shell 解析层；其余命令维持原 cmd /C 转发（保持既有语义，含管道/重定向）。

// splitGitTokens 把一条纯 git 命令切分为 argv。
// 支持双引号包裹（"" 转义）、单引号包裹（字面量）、\" \\ 转义；
// 任何异常（未闭合引号等）返回 nil 走原 cmd /C 路径，绝不改变失败面。
func splitGitTokens(command string) []string {
	var toks []string
	var cur strings.Builder
	started := false // 空串参数（""）也保留
	inDQ, inSQ := false, false
	i := 0
	for i < len(command) {
		c := command[i]
		switch {
		case inDQ:
			if c == '"' {
				// cmd 风格 "" 转义：双写引号 → 字面量引号
				if i+1 < len(command) && command[i+1] == '"' {
					cur.WriteByte('"')
					i += 2
					continue
				}
				inDQ = false
				i++
			} else if c == '\\' && i+1 < len(command) && (command[i+1] == '"' || command[i+1] == '\\') {
				cur.WriteByte(command[i+1])
				i += 2
			} else {
				cur.WriteByte(c)
				i++
			}
		case inSQ:
			if c == '\'' {
				inSQ = false
				i++
			} else {
				cur.WriteByte(c)
				i++
			}
		case c == ' ' || c == '\t':
			if started {
				toks = append(toks, cur.String())
				cur.Reset()
				started = false
			}
			i++
		case c == '"':
			started = true
			inDQ = true
			i++
		case c == '\'':
			started = true
			inSQ = true
			i++
		case c == '\\' && i+1 < len(command) && (command[i+1] == '"' || command[i+1] == '\\'):
			started = true
			cur.WriteByte(command[i+1])
			i += 2
		default:
			started = true
			cur.WriteByte(c)
			i++
		}
	}
	if inDQ || inSQ {
		return nil // 未闭合引号：不是安全的简单命令，走原路径
	}
	if started || cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	return toks
}

// isPureGitCommand 判断命令是否为一条可 argv 化的纯 git 调用：
//   - 无 shell 语法字符（| & < > ( ) ^ % ! 换行等——出现即走原 cmd 路径）；
//   - 首个 token 为 git（大小写/扩展名不敏感，覆盖 git.exe/git.cmd）；
//   - 第二个 token 非 "help"（帮助走原路径，行为不变）。
//
// 命中时返回切分好的 argv（["git", sub, ...]）。
// 注意：拒绝字符（| & < > 等）仅对「引号外」的片段生效——引号内的内容是
// shell 不解析的字面量（如提交信息里的中文括号/感叹号），不能因此降级。
func isPureGitCommand(command string) []string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return nil
	}
	// 引号外 shell 语法字符检测：解析与检测同步进行，遇引号内片段跳过检测。
	inDQ, inSQ := false, false
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		switch {
		case inDQ:
			if c == '\\' && i+1 < len(trimmed) && (trimmed[i+1] == '"' || trimmed[i+1] == '\\') {
				i++ // 跳过转义对
			} else if c == '"' {
				inDQ = false
			}
		case inSQ:
			if c == '\'' {
				inSQ = false
			}
		case c == '"':
			inDQ = true
		case c == '\'':
			inSQ = true
		default:
			switch c {
			case '|', '&', '<', '>', '(', ')', '^', '%', '!', '\n', '\r':
				return nil // 引号外出现 shell 语法 → 走原 cmd /C 路径
			}
		}
	}
	if inDQ || inSQ {
		return nil
	}
	toks := splitGitTokens(trimmed)
	if len(toks) < 2 {
		return nil
	}
	first := strings.TrimSuffix(strings.ToLower(toks[0]), ".exe")
	first = strings.TrimSuffix(first, ".cmd")
	first = strings.TrimSuffix(first, ".bat")
	if first != "git" {
		return nil
	}
	if strings.EqualFold(toks[1], "help") {
		return nil
	}
	return toks
}

// ---------- PowerShell $ 吞失修复（2026-09-27 复盘） ----------
//
// 根因：Windows 下 shell 工具经 cmd /C 转发整行命令，Agent 常用的
//   powershell -Command "... $env:PATH ..."
// 中内联脚本串的 $ 变量会被 cmd 先行解析——cmd 把 $ 视为普通字符，
// 但若脚本串里出现 %VAR% 或 ^ 等 cmd 元字符则被吞/转义；且双层引号
// 剥离后 PowerShell 收到的脚本已变形。实测：内联 PowerShell 变量
// （$PID、$env: 等）经 cmd /C 中转后字面量失真。
// 修复策略（与 git argv 化同一契约）：对「纯 PowerShell 调用」解析为
// argv 后 exec.Command("powershell", ...) 参数化直跑，脚本串作为单个
// 参数原样送达，完全绕过 cmd /C 解析层；异常形态返回 nil 走原
// cmd /C 路径，绝不扩大失败面。

// isPurePowerShellCommand 判断命令是否为一条可 argv 化的纯 PowerShell
// 调用：
//   - 无引号外 shell 语法字符（| & < > ( ) ^ % ! 换行等——出现即走原
//     cmd /C 路径）；引号内内容是 shell 不解析的字面量（脚本串里的
//     管道、$ 变量），不参与检测；
//   - 首个 token 为 powershell / pwsh（大小写不敏感，覆盖 .exe/.cmd/
//     .bat 后缀；powershell_ise 不命中）；
//   - 非光杆调用（光杆 powershell 会进入交互模式挂住）；
//   - -Command / -File 后必须还有参数（无值的 -Command 同样挂住）。
//
// 命中时返回切分好的 argv（["powershell", "-Command", script, ...]），
// 脚本串保留原样（含 $ 变量、$env:、内嵌引号），由 exec 的参数转义
// 机制重新包裹，PowerShell 端收到的字面量与 Agent 提交时一致。
func isPurePowerShellCommand(command string) []string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return nil
	}
	// 引号外 shell 语法字符检测：解析与检测同步进行，遇引号内片段跳过检测。
	inDQ, inSQ := false, false
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		switch {
		case inDQ:
			if c == '\\' && i+1 < len(trimmed) && (trimmed[i+1] == '"' || trimmed[i+1] == '\\') {
				i++ // 跳过转义对
			} else if c == '"' {
				inDQ = false
			}
		case inSQ:
			if c == '\'' {
				inSQ = false
			}
		case c == '"':
			inDQ = true
		case c == '\'':
			inSQ = true
		default:
			switch c {
			case '|', '&', '<', '>', '(', ')', '^', '%', '!', '\n', '\r':
				return nil // 引号外出现 shell 语法 → 走原 cmd /C 路径
			}
		}
	}
	if inDQ || inSQ {
		return nil // 未闭合引号：不是安全的简单命令，走原 cmd /C 路径
	}
	// splitGitTokens 的切分规则（双/单引号包裹、\" \\ 转义、cmd "" 转义）
	// 同样适用于 PowerShell 命令行形态，直接复用，不再复制一份解析器。
	toks := splitGitTokens(trimmed)
	if len(toks) < 2 {
		return nil // 光杆 powershell 进入交互模式，绝不可 argv 直跑
	}
	first := strings.TrimSuffix(strings.ToLower(toks[0]), ".exe")
	first = strings.TrimSuffix(first, ".cmd")
	first = strings.TrimSuffix(first, ".bat")
	if first != "powershell" && first != "pwsh" {
		return nil
	}
	// -Command / -File 无值时 PowerShell 会等待 stdin/交互，挂住直到超时。
	last := toks[len(toks)-1]
	for _, tok := range toks[1:] {
		lower := strings.ToLower(tok)
		if (lower == "-command" || lower == "-file") && tok == last {
			return nil
		}
	}
	return toks
}

// ---------- PowerShell 命令经 cmd 中转的 $ 保真（2026-10-06 复盘补齐） ----------
//
// isPurePowerShellCommand 只覆盖了 shell 工具（argv 直跑）。但 unified_exec
// （cmd /K 常驻会话，stdin 投递整行）与 bg_exec（cmd /C 后台）仍把整条
// powershell 调用交给 cmd 解析——cmd 会把 "$env:X" 提前展开/吞失，PowerShell
// 收到变形的脚本，输出为空或字面量（复现：cmd /C powershell -Command
// "Write-Output ('A=' + $env:USERNAME)" 输出 "Write-Output ('A=' + PC)"）。
//
// 保真手段：把 -Command 的内联脚本串换成 -EncodedCommand（UTF-16LE →
// Base64）。Base64 只含 [A-Za-z0-9+/=]，不含 cmd 元字符，任何 cmd 中转层
// （/C 整行 或 /K stdin 投递）都原样放行，PowerShell 端解码后 $ 变量与
// Agent 提交时逐字节一致。

// encodePSCommand 把 PowerShell 脚本编码为 -EncodedCommand 参数
// （UTF-16LE 字节 → Base64；PowerShell 官方推荐形态）。
// 用 unicode/utf16 处理代理对（emoji 等 BMP 外字符），与 Windows 控制台
// 内部的 UTF-16 表示严格一致，杜绝手工移位误码。
func encodePSCommand(script string) string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, 0, len(units)*2)
	for _, u := range units {
		b = append(b, byte(u&0xff), byte((u>>8)&0xff))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// psEncodedInvocation 把纯 PowerShell 调用的 argv 中 "-Command <脚本>" 替换为
// "-EncodedCommand <b64>"（其余参数原样保留），使整条调用可以安全地交给
// cmd 中转而不失真。
// 返回 ok=false 表示无需/不宜转换（调用方维持原样投递）：
//   - 无 -Command 参数（如 -File 场景：脚本在文件里，cmd 不解析文件内容）；
//   - -Command 值为 "-"（stdin 读取：编码会改变语义且交互式下会挂住）；
//   - 已使用 -EncodedCommand（原命令本身无内联 $ 风险）。
func psEncodedInvocation(argv []string) ([]string, bool) {
	if len(argv) < 3 {
		return nil, false
	}
	dst := make([]string, 0, len(argv))
	converted := false
	i := 0
	for i < len(argv) {
		tok := argv[i]
		lower := strings.ToLower(tok)
		if lower == "-command" && i+1 < len(argv) {
			script := argv[i+1]
			if script == "-" {
				return nil, false // stdin 读取：编码改变语义，维持原样
			}
			dst = append(dst, "-EncodedCommand", encodePSCommand(script))
			converted = true
			i += 2
			continue
		}
		if lower == "-encodedcommand" {
			return nil, false // 已是编码形态，无需再转换
		}
		dst = append(dst, tok)
		i++
	}
	// 仅当实际替换了 -Command 才判定转换成功（-Command 与脚本两个 token
	// 替换为 -EncodedCommand 与 b64 两个 token，长度可能相等，不能用长度判）。
	if !converted {
		return nil, false
	}
	return dst, true
}

// psSafeCommand 返回可安全投递给 cmd 中转层的 PowerShell 命令形态：
// 命中纯 PowerShell 调用且可编码时返回 EncodedCommand 形态（$ 保真），
// 否则原样返回（异常/复合命令维持既有行为，绝不扩大失败面）。
func psSafeCommand(command string) string {
	argv := isPurePowerShellCommand(command)
	if argv == nil {
		return command
	}
	if enc, ok := psEncodedInvocation(argv); ok {
		// 组装回命令行串：含空白/特殊字符的参数用双引号包裹（cmd 风格
		// "" 转义），保证 cmd /K 拆词后仍与 argv 一致。
		parts := make([]string, 0, len(enc))
		for _, p := range enc {
			if strings.ContainsAny(p, " \t\"&|<>()^%!") {
				parts = append(parts, `"`+strings.ReplaceAll(p, `"`, `""`)+`"`)
			} else {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, " ")
	}
	return command
}

// ---------- shell ----------

	// CommandRunner 命令执行器：黑白名单、超时由 Agent 引擎注入。
	type CommandRunner struct {
		Workspace  string
		TimeoutSec int
		DenyList   []string // 危险命令黑名单（移植 DCG 思路的简版）
		// Sandbox 沙箱工厂（nil = 不沙箱裸跑）：每条命令 New() 一个执行器，
		// Start 后 Attach 入 job；超时/取消 kill 点整树终止（见 sandbox.go）。
		Sandbox SandboxFactory
	}

	// shellOutLimit shell/unified_exec 输出截断上限：与引擎 clampToolResult(40k)
	// 对齐——此前 10k 在 40k 引擎配额下浪费 3/4 配额，截断标记保留。
	const shellOutLimit = 40000

// DefaultDenyList 移植自 Zoo-Code Destructive Command Guard 的高危命令集（简版）。
var DefaultDenyList = []string{
	"rm -rf /", "rm -rf ~", "format ", "shutdown", "reboot",
	"del /f /s /q c:\\", "rd /s /q c:\\", "mkfs", "dd if=",
	":(){ :|:& };:",
}

// denyListHit DenyList 命中判定：子串匹配 + PowerShell 参数形态豁免。
// 2026-10-10 第 23 轮自检实锤：danger.go 的 isPSParamSwitch 修复只覆盖了
// 引擎审批路径（CheckDangerousCommandAuto），shell/unified_exec 工具内的
// DenyList 检查是第二条并行拦截路径，Get-Date -Format 仍死在这里——
// 小写化后 "-format 'yyyy'" 含子串 "format '" 命中 "format " 黑名单项。
// 豁免语义与 danger.go 保持一致：-format 参数形态（后接边界）视为 PS
// 参数而非磁盘格式化命令，放行；真 format 磁盘命令（format d:）照拦。
func denyListHit(cmd string) bool {
	lower := strings.ToLower(cmd)
	for _, d := range DefaultDenyList {
		if !strings.Contains(lower, strings.ToLower(d)) {
			continue
		}
		// 仅 "format " 一项需要参数形态豁免（其余项无 PS 参数撞名场景）。
		if strings.Contains(d, "format ") {
			blocked := false
			for _, seg := range splitCommandChain(lower) {
				seg = strings.TrimSpace(seg)
				if strings.Contains(seg, "format ") && !isPSParamSwitch(seg, "format") {
					blocked = true
					break
				}
			}
			if !blocked {
				continue
			}
		}
		return true
	}
	return false
}

type ShellTool struct{ Runner *CommandRunner }

func (t *ShellTool) Name() string { return "shell" }

func (t *ShellTool) Description() string {
	return "Execute a CLI command in the workspace and capture combined stdout/stderr. " +
		"Dangerous commands are blocked. Output is truncated to 10000 characters. " +
		"IMPORTANT: This tool BLOCKS until the command (and every process it spawned) exits. " +
		"Never use it for long-running servers or daemons (dev servers, uvicorn, vite, " +
		"tail -f, watch tasks). Instead launch those with unified_exec " +
		"(run_in_background=true) and poll results with shell_output, or detach them " +
		"via a log file redirect. If a command outlives its timeout, the tool force-returns " +
		"after a short grace period with whatever output was captured."
}

	func (t *ShellTool) Parameters() map[string]interface{} {
		return objectSchema(map[string]interface{}{
			"command":     strProp("The shell command to execute"),
			"timeout_sec": numProp("Optional per-command timeout in seconds (default: runner default 120). On timeout the whole process tree is killed when sandbox mode is job."),
		}, []string{"command"})
	}

	func (t *ShellTool) Execute(ctx context.Context, args string) (string, error) {
		var p struct {
			Command    string `json:"command"`
			TimeoutSec int    `json:"timeout_sec"`
		}
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if strings.TrimSpace(p.Command) == "" {
			return "", fmt.Errorf("command is required")
		}

	// 2026-10-10：改用 denyListHit 统一判定（PS -Format 参数形态豁免，
	// 修复 Get-Date -Format 被 "format " 黑名单项误拦；语义与 danger.go
	// isPSParamSwitch 修复一致，见 denyListHit 注释）。
	if denyListHit(p.Command) {
		return "", fmt.Errorf("command blocked by destructive command guard: %q", p.Command)
	}

		// timeout_sec 参数优先（任务 3 加固：与 unified_exec 同名参数语义对齐），
		// 缺省回落 Runner.TimeoutSec；非法值（负数）按 0 处理走回落。
		timeoutSec := p.TimeoutSec
		if timeoutSec <= 0 {
			timeoutSec = t.Runner.TimeoutSec
		}
		timeout := time.Duration(timeoutSec) * time.Second
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// Windows 引号/编码修复：纯 git 命令 argv 参数化直跑，绕过 cmd /C
		// 的引号剥离与 GBK 乱码（修复 git 提交 pathspec 报错；详见上方注释块）。
		if argv := isPureGitCommand(p.Command); argv != nil {
			cmd = exec.CommandContext(cctx, argv[0], argv[1:]...)
		} else if argv := isPurePowerShellCommand(p.Command); argv != nil {
			// PowerShell $ 吞失修复：powershell/pwsh 调用同样 argv 参数化直跑，
			// -Command 脚本串（含 $ 变量、$env:、内嵌引号）原样送达，不被
			// cmd /C 中间层二次解析吞失。
			cmd = exec.CommandContext(cctx, argv[0], argv[1:]...)
		} else {
			cmd = exec.CommandContext(cctx, "cmd", "/C", p.Command)
		}
	} else {
		cmd = exec.CommandContext(cctx, "bash", "-c", p.Command)
	}
	cmd.Dir = t.Runner.Workspace
	sysproc.Hide(cmd) // GUI 进程下隐藏控制台黑框。

	// 修复（移植 Zoo-Code 终端方案结论）：Windows 下 `start /b foo` 等派生的
	// 孙进程会继承 stdout/stderr 管道句柄；os/exec 的 Wait 会等所有管道句柄
	// 关闭，导致常驻服务（如 uvicorn）令 cmd.Run() 永不返回——即使 ctx 超时
	// 已 kill 掉 cmd.exe（孙进程仍持句柄）。WaitDelay 在进程退出/ctx 到期后
	// 再等 5 秒即强制返回，不再被未关闭的管道句柄卡死。
	cmd.WaitDelay = 5 * time.Second

		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out

		// 沙箱接线：Start 后入 job；Attach 失败（如进程已在系统兼容性 job）
		// warn 一次继续裸跑（优雅降级，绝不因沙箱挂掉而让命令失败）。
		// Start→Attach 之间的微秒级逃逸窗口为已知 MVP 限制（见 sandbox.go）。
		var sb SandboxExecutor
		if t.Runner.Sandbox != nil {
			sb = t.Runner.Sandbox.New()
			defer sb.Close()
		}
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("start command: %w", err)
		}
			if sb != nil {
				if err := sb.Attach(cmd); err != nil {
					slog.Warn("sandbox attach failed, fallback to unsandboxed run", "err", err)
				}
			}

		runErr := cmd.Run()

		res := out.String()
		// GBK 兑底：cmd 控制台代码页跟随系统（GBK）时输出非法 UTF-8，
		// 直接返回会乱码；检测到非法序列时用 GBK 解码重试（任务 3 加固）。
		res = ensureUTF8(res)
		if len(res) > shellOutLimit {
			res = truncateMiddle(res, shellOutLimit)
		}
		exitMsg := "exit code 0"
		if runErr != nil {
			exitMsg = fmt.Sprintf("exit code error: %v", runErr)
		}
		if res == "" {
			res = "(no output)"
		}
		return fmt.Sprintf("%s\n\n%s", exitMsg, res), nil
	}

	// ---------- GBK 兑底（任务 3 加固）----------

	// ensureUTF8 输出编码兑底：Windows 下 cmd 控制台代码页跟随系统（简体中文
	// 系统默认 GBK），子进程输出经管道返回时可能是 GBK 字节流；直接当 UTF-8
	// 字符串返回会乱码。检测到非法 UTF-8 序列时用 GBK 解码重试。
	// 采纳条件（无损双保险）：解码结果合法 UTF-8，且回编 GBK 能还原原字节
	//（防二进制垃圾被替换符改写——实测 FF FE 00 81 经 GBK 往返会失真成
	// FF 3F 3F）。不满足则保留原样，绝不因兑底引入二次乱码。
	// x/text 已在 go.sum 间接依赖，import 即用（无新依赖）。
	func ensureUTF8(s string) string {
		if utf8.ValidString(s) {
			return s
		}
		if fixed, ok := gbkToUTF8Lossless(s); ok {
			return fixed
		}
		return s
	}

	// gbkToUTF8Lossless GBK→UTF-8 无损转换：解码 + 回编字节级校验。
	func gbkToUTF8Lossless(s string) (string, bool) {
		decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes([]byte(s))
		if err != nil || !utf8.Valid(decoded) {
			return "", false
		}
		back, err := simplifiedchinese.GBK.NewEncoder().Bytes(decoded)
		if err != nil || string(back) != s {
			return "", false
		}
		return string(decoded), true
	}

	// ---------- truncateMiddle 工具输出截断（对标 mini-swe-agent observation_template）----------

// truncateMiddle 超长输出按「头 head + 尾 head + 省略说明」截断（UTF-8 边界安全）。
//
// 对标 mini-swe-agent：命令输出超限时只留头部会让关键信息（错误汇总、堆栈末帧、
// 测试结果统计）被吞——错误恰恰多在尾部。头尾各留 head 字节 + 中段省略字符数 +
// 可操作指令（head/tail 选择性看 / 缩小检索模式 / 重定向到文件再搜），模型
// 凭报错即可自助获取被省略的中段，不再盲试。
func truncateMiddle(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head := limit / 2
	tail := limit - head
	elided := len(s) - head - tail
	// UTF-8 边界回退（截点落在多字节字符中间时剥掉残字节）。
	headPart := truncUTF8(s, head)
	tailPart := reverseCut(s, tail)
	return headPart + fmt.Sprintf("\n... [%d characters elided] ...\n", elided) + tailPart +
		"\n... (output truncated; use a more selective command — head/tail/sed to view fewer lines, narrower search patterns, or redirect output to a file and search it)"
}

// reverseCut 取字符串尾部 n 字节（截点同样做 UTF-8 边界回退，起点向后找合法字符边界）。
func reverseCut(s string, n int) string {
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	for i := 0; i < 3 && start < len(s); i++ {
		if r, _ := utf8.DecodeRuneInString(s[start:]); r != utf8.RuneError {
			break
		}
		start++
	}
	return s[start:]
}

// ---------- ask_followup ----------

// AskResult 特殊结果标记：引擎捕获后转 ask 消息。
type AskResult struct {
	Question string
}

func (e *AskResult) Error() string { return "ask_followup: " + e.Question }

type AskFollowupTool struct{}

func (t *AskFollowupTool) Name() string { return "ask_followup_question" }

func (t *AskFollowupTool) Description() string {
	return "Ask the user a question to gather additional information needed to complete the task. " +
		"The agent pauses until the user answers. " +
		"Under full-auto approval the question is NOT forwarded; decide autonomously " +
		"from context instead."
}

func (t *AskFollowupTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"question": strProp("The question to ask the user"),
	}, []string{"question"})
}

func (t *AskFollowupTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	return "", &AskResult{Question: p.Question}
}

// ---------- attempt_completion ----------

// CompletionResult 特殊结果标记：引擎捕获后结束任务并转 completion_result ask。
type CompletionResult struct {
	Result string
}

func (e *CompletionResult) Error() string { return "attempt_completion: " + e.Result }

type AttemptCompletionTool struct{}

func (t *AttemptCompletionTool) Name() string { return "attempt_completion" }

func (t *AttemptCompletionTool) Description() string {
	return "Present the final result of the task to the user once all work is complete. " +
		"This ends the current task."
}

func (t *AttemptCompletionTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"result": strProp("The final result message summarizing what was accomplished"),
	}, []string{"result"})
}

func (t *AttemptCompletionTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	return "", &CompletionResult{Result: p.Result}
}
