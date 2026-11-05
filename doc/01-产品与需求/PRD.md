# Kaelvoris 产品需求文档（PRD）

> 版本：v1.0（对应产品版本 0.7.3）　状态：现行
> 关联文档：[architecture.md](../02-架构设计/architecture.md)（架构设计）、[development.md](../03-开发指南/development.md)（开发规范）、[implementation.md](../03-开发指南/implementation.md)（实现说明）、[user-guide.md](../05-用户手册/user-guide.md)（用户手册）、[admin-guide.md](../04-部署与运维/admin-guide.md)（管理手册）

---

## 1. 产品概述

### 1.1 产品定位

Kaelvoris 是一个**以 Go 内核（Agent 引擎）+ 多形态宿主**为架构的 AI Agent 应用，完整移植 Zoo-Code（Cline 系）的编排语义。一句话定位：

> **一个内核，多种宿主**——桌面端、CLI、IM 通道、远程服务中心共用同一套 Agent 引擎，让 AI 在任何终端上替用户干编码活。

### 1.2 解决的问题

| 痛点 | Kaelvoris 的解法 |
|---|---|
| AI 编码工具被锁死在某个 IDE 里 | Go 单二进制内核，桌面端 / CLI / 手机 H5 / IM 通道全形态复用 |
| 桌面上才能用 AI 编码，人离开电脑就断 | 远程服务中心：手机 H5 / 管理后台远程派任务、审批、续聊 |
| 长任务失控（死循环、反复调用同一工具） | MistakeLimit / ToolRepetitionLimit / MaxSubtaskDepth / 流式重试四重防护 |
| 项目知识无法沉淀，每次都重新教 | 自进化经验卡（evolve）+ 自学习复盘（selflearn）+ 向量知识库（vector） |
| 想让 AI 自己规划迭代自己 | 自主迭代规划板（roadmap）：提案 → AI 审核 → 排期 → 自动开发闭环 |

### 1.3 目标用户

| 角色 | 场景 | 主要入口 |
|---|---|---|
| 个人开发者 | 日常编码、重构、修 BUG、写文档 | 桌面端 / CLI |
| 多机协同者 | 家里 PC 挂任务，手机上盯进度、审批 | 桌面端（节点）+ 移动端 H5 |
| 团队管理者 | 派发任务给多台 PC 节点、统一审计 | 管理后台 |
| 自动化运维 | 定时巡检、定时复盘、无人值守批处理 | 定时任务 / 飞书企微通道 |

### 1.4 产品原则

1. **零重依赖**：核心依赖仅 wails/v2、eino、gorilla/websocket、lark SDK 四项；MCP 远程传输纯 `net/http` 手写，不引 SDK。
2. **Windows 优先**：所有脚本 PowerShell 5.1 兼容、路径分隔符、隐藏窗口、BOM 编码坑全部内化。
3. **语义逐字对齐 Zoo-Code**：提示词、模式系统、任务栈、工具集均注明移植来源，改一处同步两处。
4. **数据自持**：全部持久化落在 `~/.kaelvoris/`（桌面）/ `~/.kaelvoris/server/`（服务端）的 JSON 文件，无数据库依赖，损坏按"跳过不致命"降级。

---

## 2. 产品形态

### 2.1 桌面端（主入口）

- Wails v2 + 原生前端（Vue3 + Arco Design，构建产物经 `//go:embed all:frontend/dist` 嵌入单 exe）。
- 三栏布局：图标轨（52px）+ 左侧会话导航（264px 可折叠）+ 中间主区 + 右侧工作面板（320px 可折叠）。
- 面板族：对话 / Git / 技能库 / 流程编排 / 员工 / 定时任务 / 文档 / 向量库 / 自主迭代 / 远程节点 / 版本升级。
- 支持同机多开（实例槽位派生节点身份）。
- 版本自升级：检查更新 → 下载 → 备份 → 原子替换 → 探活回滚。

### 2.2 CLI

- 管道 / 交互式任务执行：`kaelvoris -task "..."` 或 `echo 任务 | kaelvoris`。
- `-y` YOLO 模式（全自动审批）；`-context-window` 启用上下文滑动窗口。

### 2.3 远程服务中心（kaelvoris-server）

- PC 节点经 `/ws/agent` 注册；手机 H5（`web/mobile`）与管理后台（`web/admin`）经 `/ws/client` 查看与派发任务。
- 现部署公网 Linux 服务器（systemd，部署目录 `/opt/kaelvoris`；服务器地址不入库，经发布脚本环境变量注入）。
- 统一账号体系：PC / 移动 / 管理端同一套账号，任意端登录身份互通。

### 2.4 IM 通道

- 飞书（WS 长连接）、企业微信（回调 + WS 双模式）。
- 复用桌面端同一套内核组装路径，文本协议审批（`y` / `n` / 自由文本）。

### 2.5 移动端

- `web/mobile` 单文件 HTML（Vue3 + Vant4 CDN，无构建链，Mission Control 深空主题）。
- WebView 壳 APK（Kotlin 零三方依赖，约 44KB）。
- 能力：查看节点/任务/会话、派发任务、流式观看、审批 ask、续聊、切模式。

### 2.6 演示程序

- `cmd/kaelvoris-demo`：内置 mock LLM 的端到端编排演示（orchestrator → new_task → code 子任务）。

---

## 3. 核心功能需求

### 3.1 Agent 执行内核（P1，已完成）

| 需求 | 说明 | 状态 |
|---|---|---|
| 主循环 | 移植 Zoo-Code `Task.ts`：while 循环 → LLM 流式 → 工具执行 → 结果回填 | ✅ |
| 事件协议 | `pkg/message` ClineMessage ask/say 双类型，全宿主共享 | ✅ |
| 系统提示词 | `prompts.go` 逐字移植 `system.ts` 组装管线，工具名映射为 kaelvoris 名 | ✅ |
| 防失控 | MaxRequests / MistakeLimit / ToolRepetitionLimit=3 / StreamRetryLimit / MaxSubtaskDepth=3 | ✅ |
| 流式自愈 | 流空闲看门狗 + 429/5xx/断流自动重试（默认 3 次退避），耗尽才升级人工卡片 | ✅ |

### 3.2 工具系统（P2，已完成）

17 内置工具 + 扩展（详见 [architecture.md](../02-架构设计/architecture.md) §5）：

- 文件五件套：fs_read / fs_write / fs_apply_diff / fs_list / fs_search
- 执行：shell / unified_exec（持久会话）/ shell_output（后台轮询）
- 编排：update_todo_list / new_task / spawn_agent / wait_agent / switch_mode / attempt_completion
- 交互：ask_followup_question
- 扩展：design_system / skill / cron_job / evolve_record / roadmap_propose
- 媒体：generate_image / generate_video / generate_speech
- 诊断三件套：process_list / file_locker / http_request

安全机制：危险命令拦截（danger.go）、敏感文件红线（protect.go）、忽略规则（ignore.go）、Windows 下纯 git/powershell 命令 argv 参数化直跑。

### 3.3 模式系统（P2，已完成）

5 内置模式：architect / code / ask / debug / orchestrator；`GroupCommand` 组含 shell 类与诊断工具。
自定义覆盖：`.kaelvoris/modes.json` / `.roomodes`。会话内经 `switch_mode` 切换。

### 3.4 会话管理（P2，已完成）

- JSON 持久化 `~/.kaelvoris/sessions/<id>.json`：partial 流式合并、Todos 随会话落盘。
- fsync 落盘 + 失败重试 + 损坏文件隔离为 `.corrupt`（不阻塞列表/回放）。
- 多会话并行：TaskScheduler 并发门控（4 路），每会话独立引擎/待办/应答通道。
- **任务结束会话总结（会话 hook，2026-10 新增）**：任务结束时引擎 `OnTaskFinished` 钩子触发 → 后台主模型生成会话总结（目标/状态/产出/下一步）→ 落会话存储 → 聊天流总结卡片 + 会话列表「总」角标；设置可开关（`taskSummary.enabled`），失败静默不打扰用户。详见实现文档 §6.1.1。

### 3.5 上下文治理（P3，已完成）

- 滑动窗口截断（ContextWindow 配置，窗口 ×0.9 触发 condense）。
- 会话 AI 分析 + 批量清理（cleanup.sessions：分析 → 文档 → 归档删除）。

### 3.6 远程控制面（P-Remote，已完成）

| 需求 | 说明 |
|---|---|
| 节点注册 | PC 端 WS 长连接注册，心跳含运行中任务数；断线宽限期 150s 自动收敛任务 |
| 远程派发 | 移动端/管理后台/桌面端（远程节点面板）三入口派发，指定节点与模式 |
| 事件流 | task.event 六类事件流式推送；断线重连补发事件环形历史 |
| 远程审批 | ask.reply 远程审批；审批/停止/续聊远程生效 |
| 远程配置 | get/set.config 远端读写 PC 端 desktop.json（APIKey 掩码） |
| 技能库同步 | 节点技能元数据缓存，远端可查/新建/删除（builtin 禁删） |
| 任务核实 | verify.tasks 按节点本地会话真实状态校正远端任务状态 |
| 存储限额 | store-limit（默认 512MB）超限按日期从旧到新清理 |
| 实例槽位 | 多开实例按槽位派生节点身份（slot-N.lock + PID 检测），服务端呈现为多节点 |

### 3.7 定时任务（P-Cron，已完成）

- 5 段 cron + `@every` 等描述符解析；持久化 jobs.json。
- 失败自动重试（0~3 次，退避 5m）、超时解除占用、连续失败熔断。
- 双入口：设置面板管理 + Agent 会话内 `cron_job` 工具管理。

### 3.8 自进化（P-Evolve，已完成）

- 定时自检任务沉淀经验卡（evolve_record 工具）。
- 每轮任务结束自动复盘（selflearn.go）→ 经验卡。
- 下次任务提示词按关键词命中注入 `<self_evolution>` 段。

### 3.9 自主迭代规划板（Roadmap，已完成）

完整 plan-and-execute 闭环：**规划 → AI 审核 → 排期 → 并行开发 → 复盘**。
状态机 proposed → approved → developing → done/failed；完全自主模式（autoDev）带每日额度熔断（默认 12 条/天）。详见 [roadmap.md](../07-自主迭代/roadmap.md)。

### 3.10 知识增强（已完成）

| 能力 | 说明 |
|---|---|
| 向量知识库 | 目录索引 + embeddings 端点 + codebase_search 语义检索，定时自动增量索引 |
| 技能库 | SKILL.md 三级（builtin/global/project）管理，skill 工具加载，分类绑定 |
| MCP | stdio + 远程双传输客户端，Streamable HTTP（JSON/SSE 帧/202 异步三分支）+ legacy SSE 降级 |
| ACP | Agent Client Protocol 客户端，spawn Claude Code / Gemini CLI / Codex 等外部 agent |
| 流程编排 | Eino 编译可视化流程画布 + 流程应用（App）绑定 + RunNode 单节点调试 |
| 设计系统 | 内置设计系统包目录，design_system 工具供 Agent 加载前端规范 |
| 媒体生成 | 聊天内生图/视频/语音（OpenAI 兼容端点），载荷剥离转 say 展示 |
| 数字员工 | 员工档案 + 角色提示词接管会话 + 网关审计 |

### 3.11 IM 通道（已完成）

飞书 WS 长连接、企业微信回调 + WS 双模式；复用桌面端内核组装路径；文本协议审批。

### 3.12 任务结束会话总结（P-Summary，已完成）

任务结束后由会话 hook 自动生成面向用户的会话总结（区别于 selflearn 的模型续接上下文记忆）：

| 能力 | 说明 |
|---|---|
| 自动触发 | 引擎 `OnTaskFinished` 钩子（含 ACP 外部 agent 路径），任务收尾后后台异步生成，不阻塞收尾 |
| 生成内容 | 主模型单次生成：目标 / 状态（done / failed / stopped）/ 产出 / 下一步，结构化 Markdown 卡片 |
| 数据来源 | 任务文本 + 会话对话转录（快照）+ 待办清单（快照）+ 任务错误 + 耗时 |
| 展示 | 聊天流尾部「会话总结」卡片（kind=summary）+ 会话列表「总」角标 + 可选 toast |
| 存储 | 会话 JSON `taskSummary` 字段（独立于 ResumeDigest，不注入模型上下文） |
| 配置 | desktop.json `taskSummary`（enabled / maxLen / silent），nil = 关闭；失败静默（仅日志） |
| 约束 | 每会话最多 1 个并发总结（会话级互斥）；5 分钟超时；字段钳制 ≤600 rune；转录 ≤8000 rune |

---

## 4. 非功能需求

| 维度 | 要求 | 现状 |
|---|---|---|
| 性能 | 桌面端冷启动 < 2s；流式首帧 < 3s | ✅ |
| 体积 | 桌面端 exe ≈ 38.5MB；服务端 ≈ 10.8MB（Linux）；APK ≈ 44KB | ✅ |
| 可靠性 | 会话 fsync 落盘；服务端 systemd 自动重启；发布门禁三关验证 | ✅ |
| 安全 | REST API 强制鉴权；节点令牌防冒充；危险命令拦截；敏感文件红线 | ✅ |
| 兼容 | Windows 10/11（主）；Linux 服务端（Ubuntu 24.04）；Android ≥ 7（minSdk 21） | ✅ |
| 可观测 | ~/.kaelvoris/app.log（桌面）；journald（服务端）；token 用量 + 成本估算 | ✅ |

---

## 5. 版本里程碑

| 版本 | 里程碑 | 关键交付 |
|---|---|---|
| 0.6.x | 远程化 | 版本升级闭环 / 统一账号 / 实例槽位 / 任务核实 / 会话清理 |
| 0.7.0 | 协同化 | 远程节点组网 / 数字员工 / 设置抽屉扩展 / 运行态绿点 |
| 0.7.1 | 自主化 | 自主迭代规划板 / 危险操作自动确认 / 会话落盘加固 |
| 0.7.3 | 平台化 | 技能库面板 / 流程编排 / headless 模式 / 会话可靠性加固 |

后续方向（见规划板）：扩展生态（extension 面板已落地骨架）、多节点调度策略、移动端图片入口开放。

---

## 6. 需求追溯

本文各功能模块与代码实现、开发验证的对应关系见 [implementation.md](../03-开发指南/implementation.md)（实现文档逐模块对照）；开发规范与验证清单见 [development.md](../03-开发指南/development.md)。
