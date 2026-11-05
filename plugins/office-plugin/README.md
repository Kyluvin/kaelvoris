# Kaelvoris Office 插件（goffice）

Excel / Word / PPT 办公文档领域工具，以 **MCP stdio 插件** 形态接入 Kaelvoris：

- **零内核污染**：独立 `go.mod`（`module github.com/kaelvoris/office-plugin`），
  不 import 主仓库任何包；主工程仅通过 MCP 协议（JSON-RPC 2.0 over stdio）
  与插件进程通信，实现语言不限（本插件为 Go，也可用 Python/Node 等实现）。
- **协议**：MCP 2024-11-05 最小子集（initialize / tools/list / tools/call / ping），
  对齐主工程 `internal/mcp` 客户端。
- **工具集**（注册名 `mcp_office_<tool>`）：
  - `office_new`     创建空文档（xlsx/csv/tsv/docx/pptx，绝不覆盖）
  - `office_status`  查看文档/草稿状态（发现 worktree）
  - `office_worktree` 草稿生命周期：create/ready/reopen/merge/discard
  - `office_unit`    添加/列出内容分区
  - `office_inspect` 读取文档结构化内容（表格/段落/页面）
  - `office_execute` 编辑内容（单元格/段落/页面）
  - `office_export`  导出（xlsx/csv/tsv/docx/pptx）
  - `office_verify`  结构读回校验
- **编辑安全**：所有写操作进入隔离草稿（worktree），`merge/discard`
  需用户显式审批（复用 Kaelvoris ask 桥）。

## 构建

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File build.ps1
# 产物: bin/goffice.exe（独立 exe）
```

## 安装（接入 Kaelvoris 桌面端）

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1
# 安装到 ~/.kaelvoris/plugins/office/（plugin.json + goffice.exe）
```

安装后 Kaelvoris 桌面端启动/新任务自动发现并连接该插件，
模型经 `mcp_office_*` 工具直接操作办公文档。

## 插件清单（plugin.json）

```json
{
  "id": "office",
  "name": "Office 办公文档",
  "version": "0.1.0",
  "description": "…",
  "author": "Kaelvoris",
  "source": "builtin",
  "mcp": { "transport": "stdio", "command": "goffice.exe" }
}
```

## 开发调试

- `GOFFICE_DEBUG=1 goffice.exe`：诊断日志走 stderr（协议只占 stdout）。
- 单独测试协议：`echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | goffice.exe`
