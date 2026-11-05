//go:build windows

// Package sysproc —— Windows 实现：CREATE_NO_WINDOW 隐藏控制台黑框。
package sysproc

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// createNoWindow 对齐 Windows CREATE_NO_WINDOW（0x08000000）：
// 子进程不继承/不创建控制台窗口，stdout/stderr 管道捕获不受影响。
const createNoWindow = 0x08000000

// Hide 隐藏子进程控制台窗口（含 preserveCmdLine 命令行透传，供 shell 工具
// 复合命令直跑路径使用）。
func Hide(cmd *exec.Cmd) {
	hideOnly(cmd)
	preserveCmdLine(cmd)
}

// HideWindowOnly 仅隐藏控制台窗口，不透传 CmdLine：preserveCmdLine 会改变
// cmd /C 的引号解析语义，钩子等 stdout JSON 消费者的引号被 cmd 剥除后
// json.Unmarshal 失败（2026-10-29 TestPreToolUseMutateArgs 回归实锤）。
// 适用于「用户配置命令原样执行」的场景（hooks），shell 工具复合命令仍用 Hide。
func HideWindowOnly(cmd *exec.Cmd) {
	hideOnly(cmd)
}

// hideOnly 仅隐藏窗口，不透传 CmdLine（preserveCmdLine 会改变 cmd /C 的引号
// 解析语义，stdout JSON 类消费者如 hooks 钩子会因引号剥除而解析失败——
// 2026-10-29 TestPreToolUseMutateArgs 回归实锤）。
func hideOnly(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// preserveCmdLine 对 cmd /C|/K 中转命令透传原始命令行（2026-10-10 第 25 轮自检实锤）：
//
// 现象：`cd /d <ws> && powershell -NoProfile -Command "Get-Date -Format '...'"` 等
// 复合命令（&& 在引号外）无法走 isPurePowerShellCommand 的 argv 直跑路径，降级
// exec.Command("cmd", "/C", 整行) —— Go 按 Windows CRT 规则对内层双引号做转义
// （" → \"），cmd 剥外层引号后，PowerShell（不走 CRT argv 解析）把 \" 当字面
// 引号，-Command 收到的是字符串字面量：求值回显原文、exit 0 假成功（复现：
// "Write-Output X" 回显 X 原文；Get-Date 回显命令原文；$PSVersionTable.PSVersion
// 展开后再回显）。isPurePowerShellCommand 单命令形态已 argv 直跑免疫，但复合
// 形态从未覆盖，2026-10-10 23:00 起复合调用全部静默失败。
//
// 修复：SysProcAttr.CmdLine 允许指定完整命令行（Go 1.17+），设置后 os/exec 跳过
// escapeArgs 的 CRT 引号转义，cmd 收到的整行与 Agent 提交时逐字节一致（即用户
// 在 cmd 里手敲的样子），由 cmd 原生规则解析：引号按 cmd 语义剥除、PowerShell
// 按 CRT 规则收到脚本本体。对 PowerShell -EncodedCommand 形态同样原样放行
// （Base64 无元字符，双保险）。条件收窄：仅命中 exe 为 cmd 且参数以 /C 或 /K
// 开头的调用；git/powershell/rg 等 argv 直连命令不满足条件，行为不变。
func preserveCmdLine(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil || len(cmd.Args) < 3 {
		return
	}
	if cmd.SysProcAttr.CmdLine != "" {
		return // 调用方已自行指定命令行，不覆盖
	}
	if exe := strings.ToLower(filepath.Base(cmd.Path)); exe != "cmd.exe" {
		return
	}
	if !strings.EqualFold(cmd.Args[1], "/C") && !strings.EqualFold(cmd.Args[1], "/K") {
		return
	}
	// 合成原始命令行：cmd 系统目录全路径（无空格，无需转义）+ 各参数原文拼接。
	// Args[2] 本身就是命令行语义的整行文本（含引号），原样透传即用户所见。
	var b strings.Builder
	b.WriteString(cmd.Path)
	for _, a := range cmd.Args[1:] {
		b.WriteByte(' ')
		b.WriteString(a)
	}
	cmd.SysProcAttr.CmdLine = b.String()
}
