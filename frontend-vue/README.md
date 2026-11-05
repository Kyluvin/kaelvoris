# Kaelvoris 桌面端 Vue3 + Arco Design 前端

> 2026-10 重构：vanilla JS 前端（`frontend/dist_backup_vanilla/`）→ Vue3 + Arco Design Web-Vue。
> 风格规范：Kaelvoris UI 控制台（简约克制 / 深色优先 / 三栏布局）。

## 目录结构

```
frontend-vue/
├── package.json              # Vue3 + @arco-design/web-vue + Vite
├── vite.config.js            # build.outDir → ../frontend/dist（Wails embed 路径不变）
├── index.html
└── src/
    ├── main.js               # 入口：Arco 注册 + 设计令牌加载 + mock 安装
    ├── App.vue               # 三栏布局壳 + 全局事件装配
    ├── bridge/
    │   ├── wails.js          # Wails 绑定统一入口（bridgeCall / onEvent）
    │   └── mock-install.js   # 浏览器预览桩（window.go 缺失时生效，桌面端自动跳过）
    ├── stores/
    │   └── app.js            # 全局 reactive store：会话/消息/待办/ask 审批 + 事件路由
    ├── components/
    │   ├── RailNav.vue       # 最左图标轨（对话/Git/设置）
    │   ├── Sidebar.vue       # 左栏：项目切换 + 会话列表 + 状态条
    │   ├── MainChat.vue      # 中栏：顶栏(模式/模型) + 消息流 + 审批卡
    │   ├── MessageItem.vue   # 消息分派渲染（user/assistant/reasoning/tool/tag/done/diff/image）
    │   ├── Composer.vue      # 输入区（Enter 发送/图片附件/排队/停止）
    │   ├── RightPanel.vue    # 右栏：待办清单 + AI 上下文（可折叠）
    │   ├── GitPanel.vue      # Git 面板（分支/暂存/diff/历史）
    │   └── SettingsDrawer.vue# 设置抽屉（通用/模型/远程/关于 + 深浅色切换）
    ├── styles/
    │   ├── tokens.css        # 设计令牌（唯一视觉事实源）
    │   ├── arco-overrides.css# Arco 组件全局对齐令牌
    │   └── app.css           # 三栏布局 + 消息流 + 面板样式
    └── utils/
        ├── markdown.js       # 轻量 Markdown 渲染（无外部依赖，兼容原版协议）
        └── format.js         # 时间/耗时/token 格式化
```

## 构建与对接

```powershell
# 开发（浏览器预览，含 mock 数据）
cd D:\code\gocoder\frontend-vue
npm run dev          # http://localhost:5175

# 生产构建（产物直接输出到 frontend/dist，Wails //go:embed 路径不变）
npm run build
```

- Wails 桌面端构建流程不变：`powershell -File D:\code\gocoder\_wailsbuild_run.ps1`
- 原 vanilla 前端完整备份于 `frontend/dist_backup_vanilla/`（index.html / main.js / style.css / theme-v4.css），回滚时复制回 dist 并删除 assets 目录即可。

## 设计系统要点（对齐 Kaelvoris UI 规范）

| 维度 | 值 |
|---|---|
| 页面背景（深色） | `#17171A` |
| 卡片/面板 | `#232329` |
| 边框 | `#33333A`（弱化分割，优先背景色差） |
| 主色 | `#3B82F6` 柔和蓝 |
| 文字层级 | `#E5E6EB` / `#86909C` / `#5D6370` |
| 状态色 | 成功 `#00B42A` / 警告 `#FF7D00` / 错误 `#F53F3F` |
| 圆角 | 全局 8px（下拉小控件 6px，禁止直角/16px+） |
| 阴影 | 卡片 `0 1px 2px rgba(0,0,0,.15)`；浮层 `0 4px 16px rgba(0,0,0,.22)` |
| 字体 | UI：PingFang SC / Microsoft YaHei；代码日志：Menlo / Consolas |
| 字号 | 标题 16 / 正文 14 / 辅助 12，行高 1.6 |
| 间距 | 区块内边距 16/24；组件间距 12/16 |
| 动效 | 200ms ease，禁止弹跳/闪烁/旋转/大缩放 |

三栏布局：图标轨（52px）+ 左侧会话导航（264px，可折叠）+ 中间主区（自适应）
+ 右侧工作面板（320px，可折叠）。窄屏（<1100px）侧栏自动浮层化。

浅色模式：设置 → 通用 → 外观 切换（`html.light` 类切换，令牌自动反转）。

## 消息协议（与后端 app.go 事件桥对齐）

- `cline:message` → `{ sessionId, message }`，message 按 `type/say/ask` 分派：
  - `say:text`（partial 流式累计全文 → 覆盖流式节点；定型 → Markdown 渲染）
  - `say:reasoning`（思考卡片，partial 覆盖，结束自动折叠）
  - `say:tool`（`"工具名 | 摘要"` 或 JSON 载荷含 diff/args → 可折叠工具卡）
  - `say:user_feedback`（用户气泡 + 图片）、`say:error`、`say:image`、`say:turn_diff`
  - `say:api_req_started/finished`（token 用量 + 耗时标签）
  - `ask:*`（审批卡：tool/command/followup/api_req_failed/…）
- `kaelvoris:sessions/status/todos/done/ai_context/toast/queued` 全部按 sessionId 路由。

## 硬性禁止项（提交前自查）

- ❌ Element Plus 风格厚重组件 / 粗黑分割线 / 直角卡片
- ❌ 渐变背景、装饰图片、花哨动画（弹跳/闪烁/旋转/大缩放）
- ❌ 高饱和大红大紫 / 荧光色
- ❌ 控件密密麻麻堆叠（区块 padding 只用 16/24，组件间距只用 12/16）
