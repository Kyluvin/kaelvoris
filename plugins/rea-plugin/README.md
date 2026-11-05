# REA 插件（逆向一切）

[REA (Reverse Engineer Anything)](https://github.com/morluto/rea)（morluto/rea，MIT）的 Kaelvoris 插件化整合。

## 形态

- **MCP 桥接代理**（`bin/rea-bridge.exe`，Go 原生 exe）：stdio → stdio 转发，
  把 MCP 流量委托给官方 `npx -y rea-agents@latest mcp` 服务器（零重实现，
  跟随上游升级；Windows 下 npx 经主工程 cmd /C 路径拉起）。
- **工作流技能**（`skills/reverse-engineer-anything/SKILL.md`）：移植自
  skills.sh 收录的官方技能（1.6K 安装量），教模型何时以及如何调用 REA 工具。

## 前置条件

- Node.js 22.19+ / 24.11+ / 26+（`npx` 可用）
- 静态 JS/Electron 分析无需其他引擎；原生二进制深度分析需本机
  Hopper / Ghidra / IDA 之一（可选）。

自检：`npx -y rea-agents@latest --version`

## 工具面（经 mcp_rea_* 注册）

`binary_session`（会话/可用性目录）、`open_binary`、
`analyze_javascript_application`、`search`、`function`、`decompile`、
`xrefs`、`trace`、`inspect_web_page`、`capture_browser_scenario` 等。
完整目录见上游 docs/mcp-contracts.md。

## 构建

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File plugins\rea-plugin\build.ps1
```

## 安装

插件市场（服务端）一键安装，或本地目录安装指向 `plugins/rea-plugin`。
启用后任务装配自动连接（`mcp_rea_<tool>` 注册给模型），技能随包部署。
