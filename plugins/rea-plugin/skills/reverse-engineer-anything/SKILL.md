---
name: reverse-engineer-anything
description: REA 逆向一切工作流。当断言依赖已发布的二进制/安装包、需要反编译、被动运行时取证，或与无源码行为对比时使用。适用于弄清某个 App 功能的实现原理（如 Notes 搜索）、Electron/JS 应用结构分析、原生二进制函数定位与伪代码还原。
---

# REA：逆向一切（Reverse Engineer Anything）

## 何时使用 REA

当断言依赖以下任一情形时使用 REA：
- 已发布的二进制或安装包（无源码可用）
- 反编译/伪代码还原需求
- 被动应用运行时取证（进程、窗口、网络行为）
- 与"仅有二进制的参考实现"做行为对比

对完整源码仓库的普通分析，用常规仓库工具（fs_search / fs_read），**不要**运行 REA 就绪检查或 provider 命令。

## 连接前须知

安装本技能只提供工作流指引，不注册 REA MCP 服务器、不安装分析引擎。若 MCP 工具列表中已可见 `mcp_rea_*` 前缀工具且注册未过期，直接进入目标分析，不要每次调查前先跑诊断或 setup。以连接的服务器实际工具列表与输入 schema 为准。

## 工具速查（经 mcp_rea_* 前缀调用）

| 意图 | 工具 |
|---|---|
| 会话身份 / 工具可用性 / 目录摘要 | `binary_session`（传 `{}` 读 `tool_availability`）|
| 打开分析目标（二进制/目录/ASAR） | `open_binary`（`provider_id` 可选 auto/hopper/ghidra/ida）|
| JS/Electron 应用静态分析 | `analyze_javascript_application` |
| 全局文本搜索（函数名/字符串） | `search` |
| 函数档案 / 反编译伪代码 | `function` / `decompile` |
| 交叉引用 / 运行时追踪 | `xrefs` / `trace` |
| 网页检查与场景捕获对比 | `inspect_web_page` / `capture_browser_scenario` / `compare_web_captures` |

## 标准流程

1. **会话自检（仅首次）**：调 `binary_session {}`，读 `tool_availability` 选择当前目标可用的操作；不可用项按 `remediation` 处理（如未装 Node、缺分析引擎）。
2. **打开目标**：`open_binary {path}`（原生二进制）或 `analyze_javascript_application {path}`（目录/ASAR）。
3. **定位**：`search` 找目标功能相关符号 → `xrefs` 看引用关系 → `function`/`decompile` 取实现。
4. **证据链**：每条结论必须引用工具返回的 evidence 引用；失败结果读 `{ "error": ... }` 诊断中的 partial observations 与 remediation。
5. **复刻**：把还原出的行为转写为本工程实现（走 fs_write/fs_fs_apply_diff），并与证据对照验证。

## 限制与注意

- 原生深度分析需本机已装 Hopper/Ghidra/IDA 之一；静态 JS 分析无需引擎。
- 完整应用 Evidence 可达数百 MB：大结果先落盘，再用 `inspect-analysis-view` 投影 summary/page 视图阅读。
- 分析在本机运行；结论自带证据与局限说明，不得夸大未验证的行为。
- 服务端目录摘要以 `binary_session` 为权威（CLI catalog 描述的是构建时快照）。
