// Package main office 插件入口：MCP stdio 服务器。
//
// dsh-univer-office 移植（Go 版）：为 Kaelvoris 提供办公内容领域工具。
// 以 MCP stdio 插件形式接入（零内核污染，不 import 主仓库任何包）：
//   - initialize/tools/list/tools/call JSON-RPC 2.0 over stdio；
//   - 工具集：office_new / office_status / office_worktree / office_unit /
//     office_inspect / office_execute / office_export / office_verify；
//   - 核心领域模型：隔离 worktree（draft→ready→merge/discard），
//     所有内容写操作进入草稿，merge/discard 需用户显式审批；
//   - 内容引擎：xlsx（excelize）/ csv / tsv / docx / pptx 的创建、编辑、
//     导入与导出（Go 原生实现，无 Node/浏览器的运行时依赖）。
package main

import (
	"fmt"
	"os"
)

func main() {
	// 诊断：GOFFICE_DEBUG=1 时日志走 stderr（MCP 协议只占 stdout）。
	if os.Getenv("GOFFICE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[goffice] plugin started, args=%v\n", os.Args)
	}
	srv := NewServer()
	if err := srv.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "[goffice] fatal: %v\n", err)
		os.Exit(1)
	}
}
