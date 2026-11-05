// Package version 版本号概念与升级闭环：
//
//   - 版本号：语义化版本（Number，构建期可由 -ldflags 覆盖）+ 内置更新日志
//     （Changelog），版本页展示 + 新版本功能提示的数据源；
//   - 升级检查：从升级源（http/https/file/UNC 均可）拉取 update.json 清单，
//     与当前版本比较，发现新版本推送事件提示（含新版新增功能说明）；
//   - 一键升级：下载新版 exe → 同盘暂存 → 备份旧版 → 原子替换 → 重启进程；
//     升级标记落盘，新进程启动时提示「已升级到 vX.Y.Z」并列出新功能。
package version

import (
	"strings"
)

// Number 当前版本号（语义化版本；wails build -ldflags 注入覆盖）。
var Number = "1.6.0"

// BuildDate 构建时间（构建期注入；空 = 开发构建）。
var BuildDate = ""

// BuildCommit 构建提交（可选）。
var BuildCommit = ""

// Entry 一个版本的更新日志条目。
type Entry struct {
	Version string   `json:"version"`
	Date    string   `json:"date,omitempty"`
	Notes   []string `json:"notes"` // 本版新增/修复要点（新版本功能提示）
}

// Changelog 内置更新日志（新版本在前）。
// 约定：每次发布前把新版本条目插到最前（升级面板 + 「已升级」提示共用）。
var Changelog = []Entry{
	{
		Version: "1.6.0",
		Date:    "2026-10-31",
		Notes: []string{
			"双层记忆体系：新增项目工作空间记忆（跨会话共享、按工作区隔离，~/.kaelvoris/workspace-memory/<hash>.memory.md），与会话记忆互补——会话记忆讲「本会话到哪了」，工作空间记忆讲「这个项目沉淀了什么」",
			"最近交互自动刷新：每轮 LLM 请求结束自动更新两级记忆的「最近交互」时间戳（分钟精度、同值跳过不重复落盘），零 token 机械更新",
			"远程服务中心认证精简：移除节点令牌（node_token）机器指纹换发机制，PC 节点注册统一走账号密码（User/Pass，与客户端同一 accounts.json 验证源），凭据为空=匿名注册（在线展示但不可被指挥）",
			"定时任务与右栏面板打磨：cron 任务挂载会话能力增强、技能面板交互细节优化",
		},
	},
	{
		Version: "1.5.2",
		Date:    "2026-10-29",
		Notes: []string{
			"会话技能面板：聊天页右侧新增「技能」TAB，把技能库技能显式添加进会话，支持按 SKILL.md params 声明动态表单配置参数，一键以任务形式运行（随会话持久化，重启/续聊不丢）",
			"rea-plugin 插件升级：MCP stdio→stdio 桥接代理接 REA 逆向工具面（open_binary/compare_web_captures 等），新增隐藏其他窗口辅助能力",
			"发布链防护加固：publish-server 资产清单核对（镜像缺 apk 即中止提示回填）+ SSH 预检实测连通，防静默失败发布",
			"浏览器快照与文件读取打磨：browser_snapshot 结构化引用树增强、fs 工具忽略规则修正",
		},
	},
	{
		Version: "1.5.1",
		Date:    "2026-10-27",
		Notes: []string{
			"定时任务能力面扩展（对标 Hermes --script / no_agent / --skill）：前置脚本执行（stdout/stderr 注入任务上下文，脚本失败不阻断）、no-agent 纯脚本模式（零 LLM，stdout verbatim 即结果，[SILENT] 静默抑制成功通知）、任务挂载技能、脚本超时与参数配置",
			"cron 触发会话禁用 cron_job 工具（对齐 Hermes Cron-run sessions cannot recursively create more cron jobs 约束），防调度循环失控",
			"管理后台新增账号设置视图：所有用户可查看账号信息并自助修改密码（复用 account.passwd，改密后本端凭据同步刷新）",
			"管理后台退出登录修复：侧边栏退出入口接入（此前顶栏按钮无绑定、凭据残留导致重连后无法退出）",
			"桌面端 UI 打磨：用量徽标胶囊化、会话空态副标题层级、右栏标签悬停态、用户消息导航按钮间距、侧栏底部卡片底色",
		},
	},
	{
		Version: "1.5.0",
		Date:    "2026-10-25",
		Notes: []string{
			"自主迭代验证权分离（对标 MetaGPT X Verifier）：条目收口 done 前规划板先核验会话是否真跑过验证命令，再亲自重跑独立验证命令复核退出码（规划板可配置，留空自动识别 go.mod/package.json，减号关闭），未过一律降级失败重试",
			"验收测试先行（对齐 AlphaCodium）：开发任务要求先有验收标尺后有实现；重试时自动回注上次失败原因，针对性解决勿重蹈",
			"自主开发守则提示词（九条）内置，支持在设置→提示词中整段自定义",
			"模型配置条目化 ModelProfiles：多模型档案管理与切换",
		},
	},
	{
		Version: "1.4.0",
		Date:    "2026-10-21",
		Notes: []string{
			"浏览器结构化交互能力面（对齐 Codex BrowserUse）：browser_act 点击/输入/按键/滚动/等待、browser_snapshot 可交互元素引用树（ref→act 工作流）、browser_tabs 标签页生命周期；桌面端与 CLI 双路注册",
			"Windows 桌面自动化能力面（对齐 Codex ComputerUse）：desktop_windows 窗口枚举 / desktop_screenshot 全屏或窗口截图 / desktop_act 真实键鼠 SendInput，纯 user32/gdi32 syscall 零依赖（非 Windows 平台自动跳过）",
			"新工具随 command 工具组发放（含 CLI 模式），设置页能力说明同步更新",
		},
	},
	{
		Version: "1.3.0",
		Date:    "2026-10-19",
		Notes: []string{
			"流程画布 P3 完全重写（n8n 式编辑器）：横向拓扑 + 右侧停靠参数面板 + 「下一步做什么」节点面板 + 连线中点插入/快删 + 分支条件 chip 可视化 + 节点运行状态徽标 + 缩放控制条 + 键盘快捷键（Delete/Ctrl+Z/Ctrl+S/Esc）",
			"generate_video 视频模型未配置时自动降级：帧图本地合成循环 GIF 动图（免费零网络，无需图床上传）",
			"聊天媒体卡支持 GIF 渲染：GIF 降级产物用 <img> 播放（<video> 无法播放 GIF）",
		},
	},
	{
		Version: "1.2.2",
		Date:    "2026-10-18",
		Notes: []string{
			"自主迭代规划器证据驱动重写：提案现状断言必须指回素材出处，拒绝臆测式规划",
			"AI 审核升级：CoT-then-verdict 逐维核验协议 + 宽松/严审宽松度可选（默认宽松红线一票否决）",
			"审核全驳回自动换宽松模式重审一次；正负样本双回注 + 全驳升级模式破同层空转",
			"修复用户消息向下导航卡死：容差 2px→10px 大于定位偏移 8px",
		},
	},
	{
		Version: "1.2.1",
		Date:    "2026-10-18",
		Notes: []string{
			"generate_video 支持 Agnes 系网关（mode/size 参数 + /agnesapi 轮询；Sora 兼容端点不受影响）",
			"聊天媒体卡新增「另存为/打开」：AI 生成的图片/视频/音频可直接保存到本地或系统程序打开",
			"对话区新增用户消息导航：双箭头快速定位上一条/下一条自己的消息",
		},
	},
	{
		Version: "1.2.0",
		Date:    "2026-10-18",
		Notes: []string{
			"流程编排 LLM 节点支持按节点选择模型（空 = 全局默认，Dify LLM 节点 model 对齐）",
			"start 节点支持定义输入变量，流程运行时按定义注入",
			"新增 pespy PE 静态侦察 CLI（PE 头画像/区段熵/导入导出表/RTTI 扫描，零第三方依赖）",
		},
	},
	{
		Version: "1.1.0",
		Date:    "2026-10-17",
		Notes: []string{
			"流程编排 Dify 语义融入：新增 transform 代码转换节点（Python/JavaScript 代码执行，template-transform 对齐）",
			"节点输出变量池：下游节点以 {节点ID.变量} 点分路径引用上游输出",
			"画布分支条件增强：入边条件按变量池求值门禁，未命中自动跳过下游子图",
		},
	},
	{
		Version: "1.0.0",
		Notes: []string{
			"首个正式版本：Kaelvoris 桌面端发布（会话 / 任务编排 / 工具体系 / 定时任务 / 消息渠道等全量能力）",
		},
	},
}

// NotesFor 查询某版本的更新说明（未找到返回 nil）。
func NotesFor(v string) []string {
	for _, e := range Changelog {
		if e.Version == strings.TrimPrefix(strings.TrimSpace(v), "v") {
			return e.Notes
		}
	}
	return nil
}

// LatestEntry 最新内置版本条目。
func LatestEntry() Entry {
	if len(Changelog) == 0 {
		return Entry{Version: Number}
	}
	return Changelog[0]
}

// splitVer 把版本号拆成数字段（忽略 v/V 前缀与 -预发布/+构建 元数据）。
// 非数字开头段按前导数字取值（"6a" → 6，缺省 0）。
func splitVer(v string) []int {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		num := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				break
			}
			num = num*10 + int(r-'0')
		}
		out = append(out, num)
	}
	return out
}

// Compare 语义化版本比较：a>b 返回 1，a<b 返回 -1，相等返回 0。
// 逐数字段比较，段数不同时短侧补 0（1.2 < 1.2.0 = 1.2.0.0）。
func Compare(a, b string) int {
	as, bs := splitVer(a), splitVer(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		x, y := 0, 0
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// IsNewer 判断 target 是否比 base 更新。
func IsNewer(base, target string) bool { return Compare(base, target) < 0 }
