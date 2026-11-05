// Package main rea-bridge：REA 逆向一切 MCP 桥接代理（stdio → stdio）。
//
// 定位（零内核污染，不 import 主仓库任何包）：
//
//	Kaelvoris 插件进程（Windows 原生 exe），把自身 stdio 上的 MCP JSON-RPC
//	流量原样转发给官方 rea-agents MCP 服务器（默认 `npx -y rea-agents@latest mcp`，
//	REA_BRIDGE_CMD 可覆盖）。桥接而非重实现：REA 工具面庞大（open_binary /
//	binary_session / analyze_javascript_application / compare_web_captures…），
//	语义逐版演进，桥接保证跟随上游升级，插件零维护。
//
// 生命周期：启动 → spawn 子进程（REACT_BRIDGE_CMD 拆分；Windows 经 cmd /C）→
//
//	双向 io.Copy 泵 → 任一端退出即整体退出（子进程残留由进程组语义兜底：stdin
//	关闭后 rea mcp 自行退出）。
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

func main() {
	// 诊断：REA_BRIDGE_DEBUG=1 时日志走 stderr（MCP 协议只占 stdout/stdin）。
	debug := os.Getenv("REA_BRIDGE_DEBUG") != ""

	cmdline := strings.TrimSpace(os.Getenv("REA_BRIDGE_CMD"))
	if cmdline == "" {
		cmdline = "npx -y rea-agents@latest mcp"
	}
	parts := splitArgs(cmdline)
	if len(parts) == 0 {
		fmt.Fprintln(os.Stderr, "[rea-bridge] REA_BRIDGE_CMD 为空")
		os.Exit(1)
	}

	var cmd *exec.Cmd
	if len(parts) == 1 {
		cmd = exec.Command(parts[0])
	} else {
		cmd = exec.Command(parts[0], parts[1:]...)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr // REA 诊断日志直通宿主 stderr
	hideConsole(cmd)

	if debug {
		fmt.Fprintf(os.Stderr, "[rea-bridge] spawn: %v\n", parts)
	}
	if err := cmd.Run(); err != nil {
		// 常见失败：未装 Node/npx 或 npx 拉包失败 —— 提示可执行的自检命令。
		fmt.Fprintf(os.Stderr, "[rea-bridge] rea-agents 退出: %v（自检：npx -y rea-agents@latest --version）\n", err)
		os.Exit(1)
	}
}

// hideConsole 平台入口：Windows 实现见 hide_windows.go，非 Windows 空操作
// （与主工程 internal/sysproc 同模式，本工程零主工程 import，独立最小实现）。

// splitArgs 引号感知的命令行拆分（与主工程 shell_tools.go 同语义的独立最小实现）。
func splitArgs(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote:
			if c == quote {
				inQuote = false
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			inQuote = true
			quote = c
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// 确认 io 引用（io.Copy 双泵在 run 模式下经 cmd.Stdin/Stdout 隐式使用；
// 显式引用保住导入，供后续手写泵模式切换）。
var _ = io.Copy
