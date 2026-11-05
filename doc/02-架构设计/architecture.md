# Kaelvoris 架构设计文档

> 版本：v1.0（对应产品版本 0.7.3）


## 1. 架构总览

三层结构：**工具注册 → LLM Provider → Agent 引擎**，各宿主只做事件桥接。

```
宿主层（形态各异，内核相同）
  main.go/app.go（Wails 桌面）   cmd/kaelvoris（CLI）   channel_*.go（IM 通道）   remote（服务端）
        └───────────────────────────────┬──────────────────────────────────┘
                                   内核组装（buildKernel / engine 装配）
                                        │
  ┌─────────────┬───────────┬──────────┼───────────┬─────────────┬─────────────┐
internal/agent  internal/llm internal/tools internal/modes internal/skills internal/task
（主循环/提示词） (OpenAI 兼容流式)  (17+ 工具+MCP) (5 内置模式)  (SKILL.md)  (任务栈/调度)
  └─────────────┬───────────┴──────────┴───────────┴─────────────┴─────────────┘
                │  横切能力：condense(上下文治理) · cron(定时任务) · employee(数字员工)
                │            evolve(自进化经验卡) · flow(Eino 流程编排) · mcp(stdio + 远程)
                │            remote(WS Hub) · session(会话持久化) · design(设计系统) · version(自升级)
                └─ pkg/message（ClineMessage ask/say 协议，全宿主共享）
```

设计原则：

1. **一个内核，多种宿主**：桌面/CLI/IM/远程全部复用 `buildKernel` 组装路径，宿主差异只在事件桥（say → 推送，ask → 阻塞应答）。
2. **移植语义对齐**：内核逐字移植 Zoo-Code 3.82.2（Task.ts / system.ts），移植点全部在注释中注明源符号。
3. **接口最小化**：外部能力（eino、MCP、ACP）以适配器桥接为纯 Go 实现，不引重 SDK。
4. **数据自持**：全部状态为 `~/.kaelvoris/` 下 JSON 文件，原子写盘，损坏降级不致命。

---

## 2. 代码地图

仓库根 `D:\gitCode\kaelvoris`，共 308 个 Go 文件（含插件/服务端/CLI）。

### 2.1 根目录（桌面宿主 + 绑定层）

| 文件 | 职责 |
|---|---|
| `main.go` / `main_headless.go` | Wails 启动 / 无窗口 headless 模式 |
| `app.go` | Wails 绑定核心：Config、taskRun 运行态、事件桥、buildTools、runTask |
| `ui_bridge_wails.go` / `ui_bridge_headless.go` | UI 桥接抽象（0.7.3 headless 模式引入） |
| `composer_bridge.go` | 发送框工作区文件列举 |
| `acp_run.go` | ACP 会话运行（acpTurn / acpRunTask / agent 管理） |
| `channel_lark.go` / `channel_wecom.go` / `channel_wecom_ws.go` | 飞书 / 企微通道宿主 |
| `instance_slot.go` + `_unix/_windows` | 多开实例槽位 |
| `signal_headless.go` | headless 信号处理 |
| `log_setup.go` | app.log 日志装配 |
| `session_cleanup.go` | 会话 AI 分析 + 归档清理 |
| `selflearn.go` / `hourlyreview.go` | 任务复盘 / 定时自检 |
| `*_panel.go` | 各功能面板 Wails 绑定（cron/doc/employee/extension/flow_app/git/remote/roadmap/skill/vector/version） |
| `cron_tools.go` / `evolve_tools.go` / `roadmap_tool.go` | Agent 会话内工具（内部 MCP 风格） |
| `flows.go` / `gh_pr.go` | 流程编排绑定 / GitHub PR 绑定 |
| `remote_account.go` / `remote_nodes.go` / `remote_panel.go` | 远程账号 / 节点指挥 / 节点侧客户端 |

### 2.2 internal/ 内核与横切能力

| 包 | 职责 | 关键文件 |
|---|---|---|
| `agent` | 引擎主循环、提示词组装、turndiff、filetracker | engine.go / prompts.go |
| `llm` | OpenAI 兼容流式客户端 | provider.go |
| `tools` | 17 内置工具 + 危险拦截 + 忽略规则 | registry.go / shell_tools.go / fs_tools.go / danger.go / protect.go / ignore.go |
| `modes` | 5 内置模式 + 自定义覆盖 | modes.go |
| `skills` | SKILL.md 三级技能管理 | skills.go / categories.go |
| `task` | 任务注册表（LIFO 栈）/ 调度器 | registry.go / scheduler.go |
| `session` | 会话 JSON 持久化 | session.go / store.go / memory.go |
| `condense` | 上下文滑动窗口治理 | condense.go |
| `cron` | 5 段 cron + 描述符、调度、重试熔断 | cron.go / scheduler.go / store.go |
| `evolve` | 经验卡存储 | store.go |
| `employee` | 数字员工档案 / 网关 / 进化 | employee.go / gateway.go / evolution.go |
| `flow` | Eino 流程编排（画布 / 引擎 / 应用） | flow.go / canvas.go / engine.go |
| `mcp` | MCP 客户端（stdio + 远程双传输） | mcp.go / http.go |
| `acp` | ACP 客户端（spawn 外部 agent + JSON-RPC over stdio） | acp.go / conn.go / run.go / bridge.go |
| `remote` | 服务端 Hub / HTTP+WS Server / 节点 client | hub.go / server.go / client.go / protocol.go |
| `roadmap` | 自主迭代状态机 / 存储 | state.go / store.go |
| `vector` | 向量知识库 | vector.go |
| `design` | 内置设计系统包目录 | design.go + systems/ |
| `extension` | 扩展生态 | extension.go |
| `mentions` | @提及解析 | — |
| `usage` | token 用量 | — |
| `version` | 版本号 / 更新日志 / 自升级 | version.go / apply.go |
| `hooks` / `sysproc` / `condense` | 引擎钩子 / 隐藏窗口进程 / 上下文压缩 | — |

### 2.3 其他宿主与前端

| 目录 | 说明 |
|---|---|
| `cmd/kaelvoris` | CLI 入口（管道/交互任务执行） |
| `cmd/kaelvoris-server` | 远程服务中心入口（accounts.json 自动生成） |
| `cmd/kaelvoris-demo` | mock LLM 端到端演示 |
| `frontend-vue/` | 桌面前端源码（Vue3 + Arco + Vite，产物输出 `frontend/dist`） |
| `frontend/` | Wails embed 目录（dist + wailsjs 自动生成绑定） |
| `web/admin`、`web/mobile` | 服务端静态页（单文件 HTML） |
| `webview-shell/` | Android WebView 壳（Kotlin） |
| `internal/design/systems/` | 内嵌设计系统包（mission-control 等） |

---

## 3. 运行时架构

### 3.1 桌面端单进程模型

```
Wails App（单进程）
 ├─ startup: buildKernel（modes/skills/evolve/cron/roadmap/远程客户端）
 ├─ SendMessage → runTask(sessionID, task)
 │    ├─ taskRun 隔离单元：engine + todo + hist + askCh + cancel
 │    ├─ TaskScheduler 并发门控（4 路）
 │    └─ Engine.Run：LLM 流式 → 工具 → 回填（panic 防护包裹）
 ├─ 事件桥：say → uiEmit("cline:message")；ask → EventsEmit + 阻塞 ApproveAsk
 ├─ 面板绑定（Wails 反向调用）：Git/Cron/Roadmap/... 全部 App 方法
 └─ 横向循环：cron 调度器 / 远程客户端 / IM 通道 / 向量索引 / hourly review
```

**事件桥语义**（各宿主一致）：

- `say` → 事件推送（桌面 `EventsEmit("cline:message", msg)`），partial 帧携带累计全文。
- `ask` → 推送后阻塞等待应答（桌面 ApproveAsk 回传；CLI 读 stdin；IM 用文本协议）。
- 引擎钩子：`AutoApprove`、`onSay`、`onAsk`、防失控五参数（见 §5.1）。

### 3.2 远程服务体系

```
PC 节点（Wails 桌面）  ⇄ /ws/agent ⇄   服务端 Hub   ⇄ /ws/client ⇄  手机 H5 / 管理后台
   internal/remote/client.go     internal/remote/server.go + hub.go       web/mobile, web/admin
   执行 Agent 引擎               注册/路由/持久化/广播                     只读+派发+审批+续聊
```

- 协议：JSON 信封 over WebSocket，`{"id","type","payload","ts"}`，Type 常量 `<方向>.<动作>`。
- 唯一权威源：`internal/remote/protocol.go`；移动端/管理端文档为对照索引。
- Hub 线程模型：全局 RWMutex 保护内存态；每连接独立写锁（gorilla 并发写不安全）；持久化 nodes.json / tasks.json / sessions.json。
- 关键容错：节点断线宽限期 150s 后收敛 running 任务；事件环形历史断线重连补发；节点断线期间任务历史由 PC 端重新上线补传。

### 3.3 会话数据流

```
用户消息 → SendMessage → taskRun（隔离）
  → engine.Run(ctx, task)
      ├─ LLM 流式（partial 累计帧）→ onSay → uiEmit → 前端覆盖渲染
      ├─ ask → onAsk → 阻塞 ApproveAsk → 应答回填
      └─ 工具执行 → 结果回填 → 下一轮
  → attempt_completion → kaelvoris:done
  → selfReview（自动复盘）→ evolve 经验卡
全程：session partial 合并落盘（fsync）
```

---

## 4. 横切能力设计

### 4.1 上下文治理（condense）

- 配置 `ContextWindow`（token），用量达窗口 ×0.9 触发滑动窗口截断。
- CLI 侧 `-context-window` / `-condense-percent` 同语义。

### 4.2 自进化闭环

- **自学习复盘（`selflearn.go` + `hourlyreview.go`）**：任务结束/每小时自检时，selflearn 复盘产出的经验卡（evolve）注入后续任务系统提示词 `<self_evolution>`。（evolve + selflearn + hourlyreview）

```
定时自检（hourlyreview）┐
每轮任务复盘（selflearn）┼→ evolve 经验卡 → 下次任务提示词按关键词命中注入 <self_evolution>
Agent 主动 evolve_record ┘
```

### 4.2.1 会话总结（task_summary.go，2026-10 新增）

- **触发**：引擎 `OnTaskFinished` 钩子（顶层会话；ACP 外部 agent 路径在 `acp_run.go` 轮末同步触发），任务收尾后后台 goroutine 异步生成，不阻塞收尾流程。
- **数据来源**：任务文本 + 会话对话转录快照（`run.hist` 锁内拷贝）+ 待办清单快照（`run.todo`）+ 任务错误 + 耗时。
- **生成**：主模型单次非流式 `Generate` 调用，输出固定 JSON 协议 `{goal, status, outcome, next}`（status ∈ done/failed/stopped），容错解析（剥代码围栏、取首个 `{` 至最后一个 `}`），字段 rune 钳制 ≤600（默认），转录 ≤8000 rune。
- **存储**：`SessionData.TaskSummary`（`json:"taskSummary,omitempty"`）独立字段，**不注入模型上下文**（resume 仍用 ResumeDigest）；`SetTaskSummary` 随会话 JSON 落盘。
- **展示**：`SayTaskSummary` 消息推送（kind=summary）→ 聊天流尾部「会话总结」卡片；`kaelvoris:sessions` 刷新会话列表「总」角标；可选 toast（`Silent` 抑制）。
- **约束**：会话级互斥（每会话 ≤1 并发总结，`taskSummaryMu`）；5 分钟超时；失败静默（仅 `slog.Warn`，不打扰用户）；panic recover 释放互斥。
- **配置**：`desktop.json` `taskSummary`（enabled / maxLen / silent），nil = 关闭。

### 4.3 自主迭代闭环（roadmap）

规划 → AI 审核 → 排期 → 开发（roadmap_<itemID> 固定会话，复用 SendMessage 执行链）→ 复盘沉淀 → 下一轮。防失控：每日额度 / 单次提案上限 / 并行上限 / 独立 goroutine panic 防护。详见 [roadmap.md](../07-自主迭代/roadmap.md)。

### 4.4 定时任务（cron）

5 段 cron + `@every` 描述符；Store 持久化 + Scheduler 调度（重试/超时/熔断）；面板与 Agent `cron_job` 工具双入口共享同一 jobs.json。

### 4.5 MCP（internal/mcp）

- 统一 `Conn` 接口：ListTools / CallTool / Close。
- stdio 子进程：Windows 经 cmd /C、整行命令拆分。
- 远程 Streamable HTTP：单端点 POST，响应三分支自适应（application/json / SSE 帧流 / 202+legacy 异步），405 自动降级旧版 HTTP+SSE；GET 流无超时专用 client；Mcp-Session-Id 会话保持。
- 工具注册名 `mcp_<server>_<tool>`；协议版本 2024-11-05。

### 4.6 ACP（internal/acp）

- Agent Client Protocol（Zed 开放协议）客户端：spawn Claude Code / Gemini CLI / Codex。
- RunTurn：spawn → initialize → session/new → session/prompt → end_turn。
- 反向请求：fs 读写 / 权限审批钩子；UpdateMapper 把 session/update 流式映射为 ClineMessage（前端零改动）。
- 配置 `~/.kaelvoris/acp-agents.json`；会话绑定 `session.ACPAgent`。

### 4.7 流程编排（internal/flow）

- 云雾 eino 编译：可视化画布节点图 → 编译执行。
- 流程应用（FlowApp）：流程绑定应用入口，RunNode 支持单节点调试。
- 存储 `~/.kaelvoris/flows/`。

### 4.8 版本与升级（internal/version）

- 语义化版本 + 内置 Changelog（版本页 + 「已升级」提示共用）。
- 升级闭环：检查 → 下载 → 备份（保留 10 份）→ 原子替换 → 6s 探活 → 失败自动回滚 + update-failed.txt。
- 升级清单 update.json 支持 sha256 校验防污染。

---

## 5. 关键设计决策

### 5.1 防失控五参数

| 参数 | 默认 | 语义 | 源对齐 |
|---|---|---|---|
| MaxRequests | 1000 万 ≈ 不设限 | 单任务 API 请求上限 | MAX_REQUESTS_PER_TASK |
| MistakeLimit | 3 | 连续无工具调用上限 | consecutiveMistakeLimit |
| ToolRepetitionLimit | 3 | 完全相同工具调用上限 | ToolRepetitionDetector |
| StreamRetryLimit | 3 | 流式瞬时错误自动重试（退避），耗尽才升级卡片 | 无人值守续跑修复 |
| MaxSubtaskDepth | 3 | 子任务嵌套上限 | 防递归失控 |

### 5.2 多会话隔离

`taskRun` 为隔离单元（各自 engine/todo/事件历史/应答通道/cancel），对齐 Zoo-Code Task 实例隔离 + TaskRegistry.push 登记；共享状态配锁（session.Session RWMutex、task.Registry）。

### 5.3 Windows 优先的 shell 语义

- Windows 下纯 git / 纯 powershell 命令自动 argv 参数化直跑，绕过 cmd /C 引号剥离与 PowerShell `$` 吞失（`shell_tools.go`）。
- 危险命令拦截（danger.go）+ 敏感文件写入红线（protect.go，不随 DangerAutoApprove 放开）。

### 5.4 统一账号

users 为唯一权威账号表（PC/移动/管理端同一套），role 决定权限；同一账号任意端登录 → 同一身份，任务/会话全互通。旧 Admins/Mobiles 双表自动迁移并入。

### 5.5 发布门禁

2026-10 坏包事故后强制：**构建 → 全量测试 → 冒烟测试**三关验证才可替换线上文件；体积门禁 + 可执行头校验 + 启动探活；替换前必须备份，探活失败自动回滚。详见 [deployment.md](../04-部署与运维/deployment.md) §3.1。

---

## 6. 存储布局

| 路径 | 内容 |
|---|---|
| `~/.kaelvoris/desktop.json` | 桌面端主配置（模型/工作区/审批/上下文/媒体/成本/危险确认/autoDev） |
| `~/.kaelvoris/sessions/<id>.json` | 会话持久化（消息 + todos + ACPAgent 等） |
| `~/.kaelvoris/skills/` / `skills-settings.json` | 全局技能 / 启用设置 |
| `~/.kaelvoris/flows/` | 流程编排定义与应用 |
| `~/.kaelvoris/roadmap/items.json` | 自主迭代规划条目 |
| `~/.kaelvoris/evolve/cards.json` | 自进化经验卡 |
| `~/.kaelvoris/cron/jobs.json` | 定时任务 |
| `~/.kaelvoris/employees/` | 数字员工档案 |
| `~/.kaelvoris/remote.json` / `remote-account.json` | 节点连接配置 / 远程账号 |
| `~/.kaelvoris/acp-agents.json` | ACP 外部 agent 配置 |
| `~/.kaelvoris/mcp.json` | MCP 服务器配置 |
| `~/.kaelvoris/images/`、`media/` | 生图 / 生视频 / 语音产物 |
| `~/.kaelvoris/instances/slot-N.lock` | 多开实例槽位锁 |
| `~/.kaelvoris/app.log` | 桌面端日志 |
| `~/.kaelvoris/server/`（服务端） | accounts.json / nodes.json / tasks.json / sessions.json |

---

## 7. 技术选型

| 依赖 | 用途 | 版本 |
|---|---|---|
| wails/v2 | 桌面宿主 + WebView | v2.16.0 |
| cloudwego/eino | 流程编排 | v0.9.20 |
| gorilla/websocket | WS（远程体系） | v1.5.3 |
| larksuite/oapi-sdk-go/v3 | 飞书通道 | v3.12.0 |
| Go | 工具链 | 模块声明 1.25 / 实际 go1.27 |

前端：桌面 Vue3 + @arco-design/web-vue + Vite（无路由/无状态库，单 store）；移动端 Vue3 + Vant4 CDN 单文件；管理后台原生 JS 单文件。

---

## 8. 扩展点

1. **新宿主**：实现 say/ask 桥（参考 `ui_bridge_headless.go`）+ 调 `buildKernel` 即可接入新形态。
2. **新工具**：实现 `tools.Tool` 接口（Name/Description/Parameters/Execute），注册进 `buildTools`；会话级工具（cron/evolve/roadmap 形态）注册进任务级 Registry。
3. **新模式**：`internal/modes` 内置表 + `.kaelvoris/modes.json` 覆盖。
4. **新传输**：MCP `Conn` 接口实现（参考 `http.go`）；ACP 反向请求扩展（`acp/conn.go`）。
5. **技能/设计系统包**：SKILL.md / DESIGN.md + tokens.css 目录约定，落盘即生效。
