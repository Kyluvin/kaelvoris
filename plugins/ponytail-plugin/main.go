// Package main ponytail 插件入口：MCP stdio 服务器。
//
// 提炼自开源项目 DietrichGebert/ponytail（MIT）——治 agent 话痨 / 过度工程
// 的「懒人资深工程师」守则，以标准 MCP 插件形态接入 Kaelvoris
// （零内核污染，不 import 主仓库任何包）：
//   - initialize / tools/list / tools/call JSON-RPC 2.0 over stdio；
//   - 工具集：ponytail_review / ponytail_audit / ponytail_lint；
//   - 纯标准库实现（无外部依赖），支持 Windows（ponytail.exe）。
//
// 与系统提示词 LAZY SENIOR DEV MODE 段 / 内置技能 ponytail 互补：
// 提示词段 = 常驻守则（无条件注入），技能 = 按需加载完整版，
// 本插件 = 可调用工具（模型每次改代码后可主动跑一遍审查）。
package main

import (
	"fmt"
	"os"
)

func main() {
	// 诊断：PONYTAIL_DEBUG=1 时日志走 stderr（MCP 协议只占 stdout）。
	if os.Getenv("PONYTAIL_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[ponytail] plugin started, args=%v\n", os.Args)
	}
	srv := NewServer()
	if err := srv.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "[ponytail] fatal: %v\n", err)
		os.Exit(1)
	}
}
