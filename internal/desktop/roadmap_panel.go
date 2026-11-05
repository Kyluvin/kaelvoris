// roadmap_panel.go —— 自主迭代规划面板（Wails 绑定）。
//
// 用户需求（2026-10）：调研开源自主开发/自主迭代/自主规划模式（AutoGPT
// 目标分解、MetaGPT 规划先行、OpenDevin 任务队列、OpenHands microagents），
// 针对本项目落地"自主规划 → 审批 → 批量开发"闭环。
// 用户需求（2026-10 增强）：AI 审核环节——用户可指定审核提示词，AI 审核通过
// 的提案自动纳入排期开发列表并自动执行；开发队列支持并行/依次（默认并行），
// 并行数可设置；autoDev 全自主模式同样走「规划 → AI 审核 → 排期 → 开发」。
//
//	【规划】planRoadmap：聚合项目结构/git 近况/既有规划，一次非流式 LLM
//	        调用产出 N 条提案（JSON 协议，风格对齐 selflearn/hourlyreview），
//	        默认落 proposed（待审核）；
//	【AI 审核】reviewRoadmapOnce：用户可指定审核提示词（desktop.json
//	        autoDev.reviewPrompt），LLM 逐条产出 verdict：approved（通过 →
//	        纳入排期）/ rejected（驳回留痕）；通过条目自动进入开发；
//	【开发】StartRoadmapDev：把批准条目转为开发会话任务执行
//	        （roadmap_<itemID> 固定会话，复用 SendMessage→runTask 执行链，
//	        任务完成钩子 onRoadmapTaskFinished 回收结果并接力）；
//	        mode=parallel（默认）/ serial，并行数 parallel 可设（1~4）；
//	【自动】autoDev=true 时完全自主：自动规划（节流 30 分钟）→ AI 审核
//	        → 排期 → 并行开发 → 自动复盘，循环间隔默认 2h，每日上限熔断。
//
// 事件约定：kaelvoris:roadmap → 条目列表推送（任何变更后）。
// Agent 工具：roadmap_propose（Agent 会话内直接沉淀提案，见 roadmap_tool.go）。
package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/llm"
	"kaelvoris/internal/roadmap"
	"kaelvoris/internal/session"
	"kaelvoris/internal/sysproc"
	"kaelvoris/pkg/message"
)

// pushRoadmap 推送规划板数据到前端（kaelvoris:roadmap：items + stats + auto 配置）。
// 同时上报远程服务端（agent.roadmap_data → roadmap 广播）：移动端「AR 自主开发」
// 页与 PC 端规划板实时同步（任何状态变更点均经由此函数）。
func (a *App) pushRoadmap() {
	if a.ctx != nil {
		uiEmit(a.ctx, "kaelvoris:roadmap", map[string]any{
			"items": a.ListRoadmapItems(),
			"stats": a.RoadmapStats(),
			"auto":  a.GetAutoDevConfig(),
		})
	}
	a.remoteSendRoadmap()
}

// roadmapDir 规划板持久化目录（~/.kaelvoris/roadmap）。
func roadmapDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "roadmap")
}

// tempRoadmapDir 兜底目录（用户目录不可写时退回临时目录，对齐 evolve/cron）。
func tempRoadmapDir() string {
	return filepath.Join(os.TempDir(), "kaelvoris-roadmap")
}

// AutoDevConfig 自动迭代配置（desktop.json 的 autoDev 字段；nil = 未配置 = 关闭）。
type AutoDevConfig struct {
	// Enabled 自动迭代总开关：true = 系统完全自主决策（规划→AI 审核→排期→开发）。
	Enabled bool `json:"enabled"`
	// IntervalMin 自动循环间隔（分钟；0 = 默认 120）。
	IntervalMin int `json:"intervalMin,omitempty"`
	// DailyLimit 每日自动开发条目上限（0 = 默认 12；防预算风暴熔断）。
	DailyLimit int `json:"dailyLimit,omitempty"`
	// MaxPlanItems 单次自动规划最多提案数（0 = 默认 5）。
	MaxPlanItems int `json:"maxPlanItems,omitempty"`
	// ReviewPrompt AI 审核提示词（用户可指定，作为审核数据的补充判据；
	// 空 = 使用内置审核准则。审核要求/红线/团队规范写这里）。
	ReviewPrompt string `json:"reviewPrompt,omitempty"`
	DevMode      string `json:"devMode,omitempty"`
	// Parallel 并行开发数（1~4；0 = 默认 2；仅 devMode=parallel 时生效）。
	Parallel int `json:"parallel,omitempty"`
	// MaxAttempts 单条目开发尝试次数上限（0 = 默认 2）：failed 条目尝试次数
	// 未超限时自动重试回队，超限置 blocked 不再自动开发（防死循环烧预算；
	// 人工重跑仍可恢复。用户需求 2026-10：失败条目按上限自动重试）。
	MaxAttempts int `json:"maxAttempts,omitempty"`
	// AutoReview 自动审核开关（用户需求 2026-10：是否开启自动审核）：
	// 规划产出后自动交 AI 审核（通过即排期开发，无需人工点「AI 审核」）。
	// Enabled 完全自主模式强制视为开启（闭环不可断）。
	AutoReview bool `json:"autoReview,omitempty"`
	// RejectDelete 审核驳回直接删除（用户需求 2026-10：驳回不留痕，
	// 直接从规划板删除；false = 驳回留痕可回溯）。
	RejectDelete bool `json:"rejectDelete,omitempty"`
	// ReviewMode 审核宽松度（用户需求 2026-10：审核一个不通过的解药——
	// lenient 宽松红线一票否决制（默认，空同默认）| standard 严审 7 维
	// 全过。宽松模式下只有「有据/风险/去重」红线不过才驳回，其余存疑
	// 交给开发阶段验证）。
	ReviewMode string `json:"reviewMode,omitempty"`
	// MinDeveloping 最低并行开发数（0 = 默认 1；用户需求 2026-10：完全自主
	// 模式下开发中条目数低于该值时持续「规划 → AI 审核」补货；超过并行
	// 上限时钳到上限，防目标高于并行数导致永远补不满而空转）。
	MinDeveloping int `json:"minDeveloping,omitempty"`
	// VerifyCommands 独立验证通道命令（验证权分离，对标 MetaGPT X Verifier；
	// 用户需求 2026-10-25）：开发会话收口 done 前，规划板亲自在条目工作区
	// 逐条执行并检查退出码，全过才落 done——不采信会话自报（会话内跑过
	// 但项目实际编译/测试不过的交付同样判失败）。空 = 按工作区类型自动
	// 推断（go.mod → go build ./... + go test ./...；package.json →
	// npm run build；均无 = 跳过复核）。设 ["-"] 显式关闭复核。
	VerifyCommands []string `json:"verifyCommands,omitempty"`
}

// 默认值常量。
const (
	autoDevDefaultIntervalMin  = 120         // 自动循环间隔：2h
	autoDevDefaultDailyLimit   = 12          // 每日自动开发上限
	autoDevDefaultMaxPlanItems = 5           // 单次规划提案上限
	autoDevDefaultParallel     = 2           // 默认并行开发数
	autoDevMaxParallel         = 8           // 并行数硬上限（完全自主弹框默认 5，可调）
	autoDevDefaultMaxAttempts  = 2           // 单条目开发尝试上限（含首次）
	autoDevMaxAttempts         = 5           // 尝试上限硬顶（防配置出死循环）
	autoDevMaxPlanItemsHard    = 20          // 规划任务数硬上限（防提示词膨胀）
	autoDevRetryInterval       = time.Minute // 补货重试短间隔（并行未满持续规划）
	planThrottleInterval       = 30 * time.Minute
)

// 开发执行方式常量（StartRoadmapDev 的 mode 参数；空 = parallel）。
const (
	roadmapDevParallel = "parallel"
	roadmapDevSerial   = "serial"
)

// AI 审核宽松度（用户需求 2026-10：审核一个不通过的解药——默认模式改为
// 宽松红线一票否决制，标准严审模式保留为可选；兼容历史值 strict）。
const (
	roadmapReviewModeStandard = "standard" // 严审：7 维全过才通过（可选）
	roadmapReviewModeLenient  = "lenient"  // 宽松：红线一票否决制（默认）
)

// roadmapDevelopSessionID 开发会话固定 ID（roadmap_<itemID>；重试复用同会话，
// 历史连续可回放；runTask 完成钩子以前缀识别回收）。
func roadmapDevelopSessionID(itemID string) string { return "roadmap_" + itemID }

// verifyEvidenceMarkers 机器验证声明核验的命令模式（小写匹配；done 收口时
// 扫描开发会话 shell/unified_exec 工具卡的 command 字段——对齐 harness
// engineering victory-detector 思想：attempt_completion 自称完成 ≠ 真的完成，
// 收口时必须看到验证命令真实运行过才信「已验证」）。
// 覆盖：Go 构建/测试/vet、npm/pnpm/yarn 构建、python pytest。
var verifyEvidencePatterns = []string{
	"go build", "go test", "go vet", "gofmt", "golangci",
	"npm run build", "npm run lint", "npx tsc", "npm test", "npm run test",
	"pnpm build", "pnpm test", "yarn build", "yarn test", "vitest", "pytest",
}

// hasVerifyEvidence 会话工具调用里是否真实运行过验证命令（非流式最终帧才计入）。
func hasVerifyEvidence(msgs []message.ClineMessage) bool {
	for _, m := range msgs {
		if m.Type != message.TypeSay || m.Say != message.SayTool || m.Partial {
			continue
		}
		var info message.ToolCallInfo
		if json.Unmarshal([]byte(m.Text), &info) != nil {
			continue
		}
		if info.Tool != "shell" && info.Tool != "unified_exec" {
			continue
		}
		cmd := strings.ToLower(info.Cmd)
		for _, p := range verifyEvidencePatterns {
			if p != "" && strings.Contains(cmd, p) {
				return true
			}
		}
	}
	return false
}

// ---------- 独立验证通道（验证权分离，2026-10-25 用户需求） ----------
//
// 与上方「机器验证声明核验」（检查会话里是否运行过验证命令）构成两级防线：
// 第一级防「从未验证就自称完成」，第二级（本通道）防「会话内跑过验证、
// 但项目实际编译/测试不过」——收口 done 前规划板亲自重新执行验证命令并
// 检查退出码，生成者不给自己打分（对标 MetaGPT X 独立 Verifier 角色）。

// verifierMu 串行化验证复核：并行开发多路同时收口时，避免同一工作区
// go build / npm build 并发执行互相踩踏（构建缓存/产物目录并发写冲突）。
var verifierMu sync.Mutex

// verifierCmdTimeout 单条验证命令超时（大型项目构建/测试较慢，给足余量）；
// verifierReportTail 复核失败输出回注截断（回注重试任务与失败原因，过长
// 会挤占开发会话上下文）。
const (
	verifierCmdTimeout = 10 * time.Minute
	verifierReportTail = 1500
)

// verifierAutoDetect 按工作区类型自动推断验证命令（配置为空时的默认通道）：
// go.mod → go build + go test；package.json → npm run build；均无 → nil
// （无公认验证手段的项目跳过复核，不阻塞收口）。
func verifierAutoDetect(ws string) []string {
	if fi, err := os.Stat(filepath.Join(ws, "go.mod")); err == nil && !fi.IsDir() {
		return []string{"go build ./...", "go test ./..."}
	}
	if fi, err := os.Stat(filepath.Join(ws, "package.json")); err == nil && !fi.IsDir() {
		return []string{"npm run build"}
	}
	return nil
}

// normalizeVerifyCommands 用户配置归一化（SetAutoDevConfig 落盘前调用）：
// 逐条 trim、去空、去重；含 "-" 时归一为 ["-"]（显式关闭复核的唯一合法形态）。
func normalizeVerifyCommands(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	var cmds []string
	seen := map[string]bool{}
	for _, c := range raw {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if c == "-" {
			return []string{"-"}
		}
		if !seen[c] {
			seen[c] = true
			cmds = append(cmds, c)
		}
	}
	return cmds
}

// verifierCommands 本次收口使用的验证命令：用户配置优先（已归一化），空则
// 按工作区类型自动推断；["-"] → nil（显式关闭）。
func (a *App) verifierCommands(ws string) []string {
	if cmds := normalizeVerifyCommands(a.autoDevCfg().VerifyCommands); cmds != nil {
		if cmds[0] == "-" {
			return nil
		}
		return cmds
	}
	return verifierAutoDetect(ws)
}

// runVerifier 独立验证通道复核：在条目工作区逐条执行验证命令（cwd=工作区、
// 单条超时 10min、串行），第一条失败即停。返回全部通过与否 + 复核报告
// （逐条命令/结论/失败输出尾部；失败报告回注重试任务定向修复）。
// 命令经 shell 执行（支持管道/重定向/&& 复合语法，与 shell 工具同语义）。
func (a *App) runVerifier(ws string) (bool, string) {
	cmds := a.verifierCommands(ws)
	if len(cmds) == 0 {
		return true, "" // 无验证通道（关闭/无公认手段）：跳过复核不阻塞收口
	}
	verifierMu.Lock()
	defer verifierMu.Unlock()
	var b strings.Builder
	for _, c := range cmds {
		ctx, cancel := context.WithTimeout(context.Background(), verifierCmdTimeout)
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "cmd", "/C", c)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "-c", c)
		}
		sysproc.Hide(cmd) // GUI 进程下隐藏控制台黑框。
		cmd.Dir = ws
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			fmt.Fprintf(&b, "✗ %s（退出失败）\n%s", c, truncateRunes(string(out), verifierReportTail))
			return false, b.String()
		}
		fmt.Fprintf(&b, "✓ %s\n", c)
	}
	return true, b.String()
}

// roadmapItemWorkspace 条目开发工作区（条目规划时的项目；未指定回落当前工作区）。
func (a *App) roadmapItemWorkspace(it roadmap.Item) string {
	if ws := strings.TrimSpace(it.Workspace); ws != "" {
		return ws
	}
	return a.cfgWorkspace()
}

// ---------- 开发批次（主任务） ----------

// roadmapBatchSessionIDPrefix 主任务会话 ID 前缀（roadmap_batch_<UnixMilli>）。
// 与 roadmapDevelopSessionIDPrefix 同以 roadmap_ 开头：完成钩子的前缀识别
// 天然兼容（主任务会话不跑引擎，不会触发钩子，仅作层级挂载点）。
const roadmapBatchSessionIDPrefix = "roadmap_batch_"

// roadmapBatchSessionID 主任务会话 ID（一次「执行」/一轮 AI 审核通过 = 一个批次）。
func roadmapBatchSessionID(ts int64) string {
	return fmt.Sprintf("%s%d", roadmapBatchSessionIDPrefix, ts)
}

// roadmapStartBatch 开启一个新开发批次：把条目归入批次（BatchID/ParentSessionID
// 部分更新落库）。追加语义：条目已有批次时保留原批次（重试路径防树层级漂移）。
// 返回批次主任务会话 ID（侧栏折叠树的主节点）。
func (a *App) roadmapStartBatch(ids []string) string {
	batchID := roadmapBatchSessionID(time.Now().UnixMilli())
	attached := 0
	for _, id := range ids {
		it, ok := a.roadmapStore.Get(id)
		if !ok || it.BatchID != "" {
			continue // 无此条目 / 已有批次（重试路径）：保留原批次
		}
		if err := a.roadmapStore.AttachBatch(id, batchID, batchID); err == nil {
			attached++
		}
	}
	if attached == 0 {
		return "" // 全部沿用原批次：无需新建主任务会话
	}
	return batchID
}

// ensureRoadmapBatchSession 确保批次主任务会话存在（幂等；已存在则复用）。
// 主任务会话 = 一个不跑引擎的普通会话（标题「自主开发批次」+ 本批条目清单），
// 并行开发子会话以 ParentID 指向它——侧栏折叠树按其归组（用户需求 2026-10：
// 并行开发需有主任务，子任务挂主任务下面）。
func (a *App) ensureRoadmapBatchSession(batchID string, all []roadmap.Item) {
	if batchID == "" {
		return
	}
	// 已存在（接力补位/重复执行）：不重建，避免清单重复。
	if _, err := a.store.Get(batchID); err == nil {
		return
	}
	// 清单只取本批条目（其他批次的条目不混入）。
	items := make([]roadmap.Item, 0, len(all))
	for _, it := range all {
		if it.ParentSessionID == batchID {
			items = append(items, it)
		}
	}
	now := time.Now()
	var b strings.Builder
	b.WriteString("【自主开发主任务】本批次并行开发以下条目，子任务会话挂本会话下：\n")
	for i, it := range items {
		detail := strings.ReplaceAll(it.Detail, "\n", " ")
		fmt.Fprintf(&b, "%d. %s（%s）\n", i+1, it.Title, detail)
	}
	b.WriteString("\n各子任务独立会话并行执行；本会话仅作主任务汇总视图，不执行开发。")
	sess := &session.Session{
		ID:        batchID,
		ChannelID: "desktop",
		Title:     fmt.Sprintf("自主开发批次 · %d 条", len(items)),
		Mode:      "orchestrator",
		Workspace: a.cfgWorkspace(),
		CreatedAt: now,
		UpdatedAt: now, // 同设：会话列表按 UpdatedAt 倒序，零值会沉底
	}
	sess.Append(message.NewSay(message.SayText, b.String()))
	a.store.Save(sess)
	uiEmit(a.ctx, "kaelvoris:sessions", a.sessionsWithRunning()) // 侧栏立即出现主任务卡
}

// ensureRoadmapDevSession 确保子开发会话壳存在并挂到批次主任务下（ParentID）。
// 必须先于 SendMessage 建 shell：runTask 复用既有会话时保留 ParentID 字段
// （新会话路径不会自动设父），侧栏折叠树才能把子会话归到主任务下面。
func (a *App) ensureRoadmapDevSession(it roadmap.Item) {
	sid := roadmapDevelopSessionID(it.ID)
	if _, err := a.store.Get(sid); err == nil {
		return // 重试复用同会话：已存在不动
	}
	now := time.Now()
	ws := it.Workspace
	if ws == "" {
		ws = a.cfgWorkspace()
	}
	sess := &session.Session{
		ID:        sid,
		ChannelID: "desktop",
		Title:     "↳ " + truncateTitle(it.Title, 38),
		Mode:      "code",
		Workspace: ws,
		ParentID:  it.ParentSessionID, // 挂批次主任务下（侧栏树层级依据）
		CreatedAt: now,
		UpdatedAt: now,
	}
	a.store.Save(sess)
}

// roadmapAppendBatchProgress 向批次主任务会话追加一条进度摘要（定型 say text）。
// 子任务启动/完成/失败时调用——主任务会话即本批并行开发的进度总览。
func (a *App) roadmapAppendBatchProgress(batchID, line string) {
	if batchID == "" {
		return
	}
	sess, err := a.store.Get(batchID)
	if err != nil || sess == nil {
		return // 批次会话已被用户删除：静默跳过，不影响开发链路
	}
	sess.Append(message.NewSay(message.SayText, line))
	a.store.Save(sess)
}

// ---------- 视图与列表 ----------

// RoadmapStats 面板徽标统计。
type RoadmapStats struct {
	Total      int `json:"total"`
	Proposed   int `json:"proposed"`
	Approved   int `json:"approved"`
	Developing int `json:"developing"`
	Done       int `json:"done"`
	Failed     int `json:"failed"`
	Rejected   int `json:"rejected"`
}

// ListRoadmapItems 全部规划条目（创建时间正序）。
func (a *App) ListRoadmapItems() []roadmap.Item { return a.roadmapStore.List() }

// RoadmapData 规划板聚合数据（items + stats + auto 一次拉取；前端面板挂载
// 首次加载走此方法，推送事件 kaelvoris:roadmap 的增量变更推送配合它使用。
// 2026-10 修复：前端旧版 load() 曾调用不存在的 "RoadmapData" 绑定静默失败，
// 导致规划板永远空态（提案落库成功但面板看不到）——补齐该聚合绑定。
func (a *App) RoadmapData() map[string]any {
	return map[string]any{
		"items": a.ListRoadmapItems(),
		"stats": a.RoadmapStats(),
		"auto":  a.GetAutoDevConfig(),
	}
}

// RoadmapStats 统计（面板徽标）。
func (a *App) RoadmapStats() roadmapStatsView {
	c := a.roadmapStore.Count()
	return roadmapStatsView{
		Total: c["total"], Proposed: c[roadmap.StatusProposed],
		Approved: c[roadmap.StatusApproved], Developing: c[roadmap.StatusDeveloping],
		Done: c[roadmap.StatusDone], Failed: c[roadmap.StatusFailed],
		Blocked: c[roadmap.StatusBlocked], Rejected: c[roadmap.StatusRejected],
	}
}

// roadmapStatsView 统计视图（避免暴露 map 键序不稳定）。
type roadmapStatsView struct {
	Total      int `json:"total"`
	Proposed   int `json:"proposed"`
	Approved   int `json:"approved"`
	Developing int `json:"developing"`
	Done       int `json:"done"`
	Failed     int `json:"failed"`
	Blocked    int `json:"blocked"`
	Rejected   int `json:"rejected"`
}

// GetAutoDevConfig 读取自动迭代配置（前端开关回显）。
func (a *App) GetAutoDevConfig() AutoDevConfig {
	cfg := a.GetConfig()
	if cfg.AutoDev == nil {
		return AutoDevConfig{}
	}
	return *cfg.AutoDev
}

// SetAutoDevConfig 保存自动迭代配置（开关切换即生效；开启时启动循环）。
func (a *App) SetAutoDevConfig(cfg AutoDevConfig) error {
	if cfg.Parallel < 0 {
		cfg.Parallel = 0
	}
	if cfg.Parallel > autoDevMaxParallel {
		cfg.Parallel = autoDevMaxParallel
	}
	if cfg.DevMode != roadmapDevSerial {
		cfg.DevMode = roadmapDevParallel // 非法值归一为默认并行
	}
	if cfg.MinDeveloping < 0 {
		cfg.MinDeveloping = 0
	}
	if cfg.MinDeveloping > autoDevMaxParallel {
		cfg.MinDeveloping = autoDevMaxParallel
	}
	if cfg.MaxPlanItems > autoDevMaxPlanItemsHard {
		cfg.MaxPlanItems = autoDevMaxPlanItemsHard
	}
	if cfg.ReviewMode != "" && cfg.ReviewMode != roadmapReviewModeStandard && cfg.ReviewMode != roadmapReviewModeLenient && cfg.ReviewMode != "strict" {
		cfg.ReviewMode = "" // 非法值归一为默认宽松
	}
	// VerifyCommands 归一化（验证权分离通道，2026-10-25）：逐条 trim、去空、
	// 去重；["-"] = 显式关闭复核。
	cfg.VerifyCommands = normalizeVerifyCommands(cfg.VerifyCommands)
	// 完全自主模式强制自动审核（闭环不可断：无人审核则提案永远停在待审核，
	// 自动开发队列永远空——用户需求 2026-10：自主模式规划产出直接交 AI 审核）。
	if cfg.Enabled {
		cfg.AutoReview = true
	}
	c := a.GetConfig()
	c.AutoDev = &cfg
	if err := a.SaveConfig(c); err != nil {
		return err
	}
	if cfg.Enabled {
		a.roadmapAutoDevStart()
	} else {
		a.roadmapAutoDevStop()
	}
	a.pushRoadmap()
	return nil
}

// AddRoadmapItem 用户手动添加规划条目（面板「+」入口；默认 proposed）。
func (a *App) AddRoadmapItem(title, detail, priority string) (roadmap.Item, error) {
	it, err := a.roadmapStore.Add(roadmap.Item{
		Title: title, Detail: detail, Priority: priority,
		Source: "manual", Workspace: a.cfgWorkspace(),
	})
	if err == nil {
		a.pushRoadmap()
	}
	return it, err
}

// DeleteRoadmapItem 删除条目。
func (a *App) DeleteRoadmapItem(id string) error {
	// 开发中条目先停止会话任务（防孤儿开发进程）。
	if it, ok := a.roadmapStore.Get(id); ok && it.Status == roadmap.StatusDeveloping && it.SessionID != "" {
		a.Stop(it.SessionID)
	}
	err := a.roadmapStore.Delete(id)
	if err == nil {
		a.pushRoadmap()
	}
	return err
}

// DeleteRoadmapItems 批量删除条目（用户需求 2026-10：「已完成」全选清理）。
// 开发中条目先停止会话任务（与单删同语义）；返回实际删除数（含跳过）。
func (a *App) DeleteRoadmapItems(ids []string) error {
	if len(ids) == 0 {
		return errors.New("请先勾选要删除的条目")
	}
	// 开发中条目先停止会话任务（防孤儿开发进程）。
	for _, id := range ids {
		if it, ok := a.roadmapStore.Get(id); ok && it.Status == roadmap.StatusDeveloping && it.SessionID != "" {
			a.Stop(it.SessionID)
		}
	}
	n := a.roadmapStore.DeleteMany(ids)
	if n > 0 {
		a.pushRoadmap()
	}
	return nil
}

// ApproveRoadmapItems 人工审核入口：把勾选的提案批量置为 approved（回拨 failed 条目同此）。
func (a *App) ApproveRoadmapItems(ids []string) error {
	if len(ids) == 0 {
		return errors.New("请先勾选要开发的条目")
	}
	n := 0
	for _, id := range ids {
		it, ok := a.roadmapStore.Get(id)
		if !ok {
			continue
		}
		// 状态机校验：proposed/failed → approved。
		if it.Status != roadmap.StatusProposed && it.Status != roadmap.StatusFailed {
			continue
		}
		if _, err := a.roadmapStore.SetStatus(id, roadmap.StatusApproved, "", ""); err == nil {
			n++
		}
	}
	if n == 0 {
		return errors.New("没有可批准的条目（仅待审核/失败状态可批准）")
	}
	a.pushRoadmap()
	return nil
}

// UpdateRoadmapItem 编辑规划条目（用户需求 2026-10：AI 自动规划后，
// 用户选中任务可修改标题/实现要点/优先级/类别，再走审批开发）。
// 复用 Store.Update 整体更新（保留不可变字段 ID/CreatedAt）；工作区/来源
// 不改：规划条目 Workspace 由规划时锚定，开发会话固定目录执行。
func (a *App) UpdateRoadmapItem(id, title, detail, priority, category string) (roadmap.Item, error) {
	it, ok := a.roadmapStore.Get(id)
	if !ok {
		return roadmap.Item{}, errors.New("条目不存在: " + id)
	}
	if strings.TrimSpace(title) == "" {
		return roadmap.Item{}, errors.New("规划条目标题不能为空")
	}
	it.Title = title
	it.Detail = detail
	it.Priority = priority
	it.Category = category
	up, err := a.roadmapStore.Update(it)
	if err == nil {
		a.pushRoadmap()
	}
	return up, err
}

// DeleteRoadmapRejected 拒绝提案（留痕）。
func (a *App) DeleteRoadmapRejected(id string) error { return a.DeleteRoadmapItem(id) }

// RejectRoadmapItem 用户拒绝提案。
func (a *App) RejectRoadmapItem(id string) error {
	it, ok := a.roadmapStore.Get(id)
	if !ok {
		return errors.New("条目不存在: " + id)
	}
	if it.Status != roadmap.StatusProposed {
		return errors.New("仅待审核条目可拒绝")
	}
	if _, err := a.roadmapStore.SetStatus(id, roadmap.StatusRejected, "", ""); err != nil {
		return err
	}
	a.pushRoadmap()
	return nil
}

// RetryRoadmapItem 一键重跑（failed/blocked → approved 回队）。执行日志
// 保留（失败原因可追溯）；重跑后不再自动重试兜底封顶（默认尝试上限 2 次，
// 配置 autoDev.maxAttempts），再次失败仍按同规则封存 blocked。
func (a *App) RetryRoadmapItem(id string) error {
	it, ok := a.roadmapStore.Get(id)
	if !ok {
		return errors.New("条目不存在: " + id)
	}
	if it.Status != roadmap.StatusFailed && it.Status != roadmap.StatusBlocked {
		return errors.New("仅失败/暂停条目可重跑")
	}
	if _, err := a.roadmapStore.MarkRetry(id); err != nil {
		return err
	}
	// 重试即回队即拉起开发调度（2026-10 修复：旧实现仅置状态，需等下一次
	// 「执行」/AI 审核路过才消费，重试条目可能长期滞留排期无会话）。
	go a.roadmapDevelopLoop()
	a.pushRoadmap()
	return nil
}

// StartRoadmapDev 开发入口（面板右侧执行栏「执行」）：批准勾选项后启动开发。
// mode = parallel（并行，默认）| serial（依次）；parallel 仅并行模式生效（1~4）。
// 空 ids = 仅启动调度（自动模式续跑用）；非空 = 批量批准后立即开发。
// 2026-10 修复（排期任务不生成新会话的入口侧根因）：
//  1. 「无可批准条目」不再整体报错——条目可能已被 AI 审核置 approved，
//     旧实现直接 return 错误、开发循环不启动，排期队列永远无人消费；
//  2. mode/parallel 先落盘再拉起循环（旧实现循环先跑、前端事后才写
//     autoDev 配置，首轮消费的是上一次的执行方式）。
//
// 2026-10 增强（用户需求：并行开发需有主任务，子任务挂主任务下面）：
//
//	非空 ids 时先归入一个开发批次（主任务会话 roadmap_batch_<ts>），
//	并行子开发会话以 ParentID 挂其下——侧栏折叠树「主任务 → 并行子任务」
//	层级展示，主任务会话内滚动汇总各子任务进度。
func (a *App) StartRoadmapDev(ids []string, mode string, parallel int) error {
	if len(ids) > 0 {
		if err := a.ApproveRoadmapItems(ids); err != nil {
			slog.Info("roadmap 开发入口：批准跳过（条目可能已在排期）", "err", err)
		}
		// 归批次 + 建主任务会话（无新增挂载时返回空 = 全部沿用原批次，不重建）。
		if batchID := a.roadmapStartBatch(ids); batchID != "" {
			a.ensureRoadmapBatchSession(batchID, a.roadmapStore.List())
			a.pushRoadmap()
		}
	}
	a.persistRoadmapDevExec(mode, parallel)
	go a.roadmapDevelopLoop()
	return nil
}

// persistRoadmapDevExec 把执行方式（并行/依次 + 并行数）落盘到 autoDev
// 配置（开发循环 claimNextApproved 读同一配置；仅显式传入合法 mode 时写入，
// 深拷贝后整体保存防共享指针竞争）。落盘先于调度启动，保证首轮即生效。
func (a *App) persistRoadmapDevExec(mode string, parallel int) {
	if mode != roadmapDevSerial && mode != roadmapDevParallel {
		return
	}
	c := a.GetConfig()
	dev := AutoDevConfig{}
	if c.AutoDev != nil {
		dev = *c.AutoDev // 深拷贝：避免改动共享 AutoDev 指针字段
	}
	if dev.DevMode == mode && dev.Parallel == parallel {
		return // 无变化不写盘
	}
	dev.DevMode = mode
	if parallel > 0 {
		if parallel > autoDevMaxParallel {
			parallel = autoDevMaxParallel
		}
		dev.Parallel = parallel
	}
	c.AutoDev = &dev
	if err := a.SaveConfig(c); err != nil {
		slog.Warn("roadmap 执行配置落盘失败", "err", err)
	}
	a.pushRoadmap()
}

// ---------- AI 审核（LLM 审查提案 → 自动纳入排期） ----------

// reviewVerdictItem AI 审核单条产出（JSON 协议）。
type reviewVerdict struct {
	ID      string `json:"id"`      // 提案 ID
	Verdict string `json:"verdict"` // approved | rejected
	Reason  string `json:"reason"`  // 审核意见（通过条件/驳回理由）
}

// reviewOutput AI 审核整体产出。
type reviewOutput struct {
	Reviews []reviewVerdict `json:"reviews"`
	Summary string          `json:"summary"`
}

// ReviewRoadmapProposals AI 审核入口（面板「AI 审核」按钮）：待审核提案逐条
// 交 LLM 按审核提示词审查——verdict=approved 的自动纳入排期（approved，随后
// 由开发循环并行执行）；rejected 留痕驳回理由。
// extraPrompt 用户指定的审核提示词（空 = 用 autoDev.ReviewPrompt 配置）。
func (a *App) ReviewRoadmapProposals(ids []string, extraPrompt string) error {
	if strings.TrimSpace(a.GetConfig().APIKey) == "" {
		return errors.New("请先在设置中填写 API Key")
	}
	// ids 为空 = 审核全部待审核提案。
	targets := a.reviewTargets(ids)
	if len(targets) == 0 {
		return errors.New("没有待审核的提案")
	}
	go func() {
		if err := a.roadmapReviewOnce(targets, extraPrompt); err != nil {
			a.pushToast("🧐 AI 审核失败：" + truncateRunes(err.Error(), 120))
			slog.Warn("roadmap AI 审核失败", "err", err)
		}
	}()
	return nil
}

// reviewTargets AI 审核目标（空 ids = 全部 proposed；已审核/开发中跳过）。
func (a *App) reviewTargets(ids []string) []roadmap.Item {
	var out []roadmap.Item
	if len(ids) == 0 {
		for _, it := range a.roadmapStore.List() {
			if it.Status == roadmap.StatusProposed {
				out = append(out, it)
			}
		}
		return out
	}
	for _, id := range ids {
		if it, ok := a.roadmapStore.Get(id); ok && it.Status == roadmap.StatusProposed {
			out = append(out, it)
		}
	}
	return out
}

// roadmapReviewOnce 执行一轮 AI 审核：聚合待审提案 → 一次非流式 LLM 调用 →
// 逐条落审核意见 → 通过的置 approved（ApprovedBy=ai_review，自动纳入排期）。
// 2026-10 增强（治「审核一个不通过」）：
//  1. 宽松度默认宽松（红线一票否决制），标准严审降为可选；
//  2. 全驳回且非用户提示词驱动时自动重试一次（换宽松模式重审，消除单次
//     审核偶然全驳；限额与零通过熔断只按实际结果计一次，重试不重复计费额度）；
//  3. 新旧解析兼容：同时接受逐条成对 JSON 与旧版 {reviews:[...]} 单对象。
func (a *App) roadmapReviewOnce(items []roadmap.Item, extraPrompt string) error {
	cfg := a.GetConfig()
	client := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	// promptconfig「提示词」设置：roadmapReview 段（设置面板可配）作为补充
	// 判据：用户显式传入的 extraPrompt 优先，空时回退全局配置；均空 = 内置默认。
	extraPrompt = a.reviewExtraPrompt(extraPrompt)
	mode := a.roadmapReviewMode()
	prompt := roadmapReviewPrompt(items, extraPrompt, a.existingRoadmapTitles(), mode)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	resp, err := client.Generate(ctx, llm.ChatRequest{Messages: []llm.Message{{
		Role: llm.RoleUser, Content: prompt,
	}}})
	if err != nil {
		return fmt.Errorf("审核模型调用失败: %w", err)
	}
	out := parseRoadmapReview(resp2Content(resp))
	// 兼容旧协议（部分模型仍回 {reviews:[...]}）：抽回 reviews 结构。
	if len(out.Reviews) == 0 {
		out = compatReviewOutput(resp2Content(resp), out.Summary)
	}
	if len(out.Reviews) == 0 {
		return errors.New("AI 审核未产出有效结论（模型输出无法解析）")
	}
	approved, rejected := a.applyRoadmapReview(items, out)
	// 全驳回自动重审（仅自动路径：extraPrompt 为空时，避免用户显式点按钮
	// 触发双次计费；重试换宽松模式——单次全驳多发生在标准模式偶然过严）。
	if approved == 0 && rejected > 0 && extraPrompt == "" && mode != roadmapReviewModeLenient {
		slog.Info("roadmap AI 审核全驳回，自动换宽松模式重审一次", "items", len(items))
		retryItems := a.reviewTargets(itemIDs(items))
		if len(retryItems) > 0 {
			retryPrompt := roadmapReviewPrompt(retryItems, extraPrompt, a.existingRoadmapTitles(), roadmapReviewModeLenient)
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel2()
			if resp2, err2 := client.Generate(ctx2, llm.ChatRequest{Messages: []llm.Message{{
				Role: llm.RoleUser, Content: retryPrompt,
			}}}); err2 == nil {
				out2 := parseRoadmapReview(resp2Content(resp2))
				if len(out2.Reviews) == 0 {
					out2 = compatReviewOutput(resp2Content(resp2), out2.Summary)
				}
				if len(out2.Reviews) > 0 {
					approved, rejected = a.applyRoadmapReview(retryItems, out2)
				}
			}
		}
	}
	a.pushToast(fmt.Sprintf("🧐 AI 审核：通过 %d 条（已纳入排期），驳回 %d 条", approved, rejected))
	// 额度计量（用户需求 2026-10：只有审核通过才算有效数量）：审核通过立即计入
	// 每日额度，而非开发完成时才计——否则全驳回轮次零消耗，自动模式会不停重复
	// 规划（审核驳回不烧开发预算，但每次规划+审核都是真金白银的 LLM 调用）。
	// 手动审核路径（用户勾选）不计额度：每日上限仅约束全自主循环。
	if approved > 0 {
		for _, r := range out.Reviews {
			if strings.EqualFold(strings.TrimSpace(r.Verdict), "approved") {
				a.roadmapAutoDev.RecordDev()
			}
		}
		a.roadmapAutoDev.ResetZeroRounds()
	} else if rejected > 0 {
		// 本轮全驳回：累计零通过轮数（连续超限当日暂停自动规划，见 PlanBlockedByZero）。
		if n := a.roadmapAutoDev.RecordZeroRound(); n >= roadmap.AutoDevMaxZeroRounds {
			a.pushToast(fmt.Sprintf("⛔ 连续 %d 轮规划全部被驳回：今日暂停自动规划（可调整审核提示词或人工审核后恢复）", n))
		}
	}
	// 审核通过即自动执行：本批通过条目归入一个开发批次（主任务会话
	// 挂载并行子会话，侧栏树「主任务 → 子任务」层级），再启动开发循环。
	if approved > 0 {
		var approvedIDs []string
		for _, r := range out.Reviews {
			if strings.EqualFold(strings.TrimSpace(r.Verdict), "approved") {
				approvedIDs = append(approvedIDs, r.ID)
			}
		}
		if batchID := a.roadmapStartBatch(approvedIDs); batchID != "" {
			a.ensureRoadmapBatchSession(batchID, a.roadmapStore.List())
		}
		go a.roadmapDevelopLoop()
	}
	a.pushRoadmap()
	return nil
}

// itemIDs 提案 ID 列表（重审目标提取用）。
func itemIDs(items []roadmap.Item) []string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

// applyRoadmapReview 把审核结论落库（通过→approved+批次排期准备；驳回→
// 留痕或直接删除）；返回通过/驳回条数。规划与重审路径共用（原 roadmapReviewOnce
// 内联逻辑抽出：重审需要二次落库，不再重复展开）。
func (a *App) applyRoadmapReview(items []roadmap.Item, out reviewOutput) (approved, rejected int) {
	for _, r := range out.Reviews {
		it, ok := a.roadmapStore.Get(r.ID)
		if !ok || it.Status != roadmap.StatusProposed {
			continue // 已被人工处理/删除：跳过
		}
		switch strings.ToLower(strings.TrimSpace(r.Verdict)) {
		case "approved", "pass", "通过":
			verdict := "✅ 通过：" + strings.TrimSpace(r.Reason)
			if _, err := a.roadmapStore.SetReview(r.ID, verdict, roadmap.StatusApproved, "ai_review"); err == nil {
				approved++
			}
		case "rejected", "reject", "驳回":
			// 驳回理由先入环形日志（rejectDelete 直接删除后 store 里已无
			// rejected 条目，recentRejectionFeedback 扫不到——反馈必须独立
			// 持久化，规划器才有负反馈可用）。
			reason := strings.TrimSpace(r.Reason)
			if a.roadmapRejectDelete() {
				_ = a.roadmapStore.Delete(r.ID)
				a.roadmapStore.AppendRejection(it.Title, reason)
				rejected++
				continue
			}
			verdict := "❌ 驳回：" + reason
			if _, err := a.roadmapStore.SetReview(r.ID, verdict, roadmap.StatusRejected, ""); err == nil {
				a.roadmapStore.AppendRejection(it.Title, reason) // 留痕路径同写：反馈源统一
			}
			rejected++
		}
	}
	return approved, rejected
}

// compatReviewOutput 旧版审核协议兼容：{"reviews":[...]} 单对象里 reason 与
// verdict 同对象——抽取为逐条 verdict 结构（reason 从同对象带过来；宽松度
// 自动路径下旧协议也走同一套落库逻辑）。
func compatReviewOutput(s, summary string) reviewOutput {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			var legacy struct {
				Reviews []reviewVerdict `json:"reviews"`
			}
			if json.Unmarshal([]byte(s[i:j+1]), &legacy) == nil {
				return reviewOutput{Reviews: legacy.Reviews, Summary: summary}
			}
		}
	}
	return reviewOutput{Summary: summary}
}

// resp2Content 规划/审核共用的回复内容提取（nil 安全）。
func resp2Content(m *llm.Message) string {
	if m == nil {
		return ""
	}
	return m.Content
}

// roadmapReviewCriteria 规划预筛与 AI 审核共用的准则（同一把尺子）：
// 规划侧产出前按同一标准自查（gate 前移，提升一次通过率——审核驳回的提案
// 大多因为规划器不知道审核标准）；审核侧按同一标准终审。
// 2026-10 重写（治「AI 审核一个不通过」）：驳回根因是提案基于猜测而非证据
// ——审核官看到「要改 X」但不知道 X 现状如何，按保守策略宁可驳回；且旧
// 提示词未禁止输出细节，长篇说明会稀释 JSON 协议（对齐 LLM-as-a-judge
// 研究：判定与理由混排显著降低可解析率）。新准则强调每条断言挂证据锚点。
const roadmapReviewCriteria = `- 有据：实现要点里的每个关键断言（现状缺陷/已有机制/改动落点）都能在规划素材中找到出处（文件名、近期提交、既有条目状态），凭空推测现状的驳回；
		- 有价值：解决真实痛点 / 阻塞性缺陷 / 高频路径体验，收益可一句话说清；
		- 可定位：指明具体文件/模块/组件落点，且该落点与断言的现状一致；
		- 可验收：给出可执行验证命令或可核验检查点（说不清怎么算完成的驳回）；
		- 粒度：单条 1~2 小时可独立交付（过大应驳回，理由注明需拆分）；
		- 去重：与既有条目（见末尾清单）重复的驳回；
		- 风险：改动面过大 / 涉及安全红线（密钥、删除数据、支付等）的驳回。`

// roadmapReviewPrompt AI 审核提示词（2026-10 重写：CoT-then-verdict 输出
// 协议——判定前先逐维打勾并给出证据引用，再下 verdict；对齐 Anthropic
// 《Building Effective Agents》evaluator-optimizer 模式与 LLM-as-a-judge
// rubric 研究：结构化逐维核验显著优于整段自由评判；禁止长篇输出细节，
// 防 JSON 协议被稀释导致解析失败）。
// extraPrompt 用户指定提示词（补充判据，最高优先级）；mode 审核宽松度
// （标准=严审 7 维全过，宽松=红线一票否决制：只有据/风险/去重不过才驳）。
func roadmapReviewPrompt(items []roadmap.Item, extraPrompt string, existing []string, mode string) string {
	var b strings.Builder
	b.WriteString(`你是本项目的自主迭代审核官。请逐条审核下面的开发提案，判定是否纳入排期开发。

	审核方法（每条提案按此流程）：
	1. 逐条核对审核准则（下方 7 维），每维标注 ✓/✗ 并写明依据（引用提案原文或素材出处）；
	2. 核对完所有维度后再给 verdict——禁止未核验就下结论，禁止凭直觉；
	3. reason 只写 1~2 句关键结论（通过的写最有把握的依据，驳回的写第一个 ✗ 维度及依据）。

	审核准则：
	` + roadmapReviewCriteria + `

	判定规则：`)
	if mode == roadmapReviewModeLenient {
		b.WriteString(`宽松模式（红线一票否决制）：仅当「有据 / 风险 / 去重」任一维度不过时驳回；
	其余维度（价值/可定位/可验收/粒度）存疑视为可接受——把握不大但方向正确的提案应通过，交给开发阶段验证。`)
	} else {
		b.WriteString(`标准模式（严审）：全部 7 个维度均 ✓ 才通过，任一 ✗ 即驳回；拿不准视为不过。`)
	}
	if ep := strings.TrimSpace(extraPrompt); ep != "" {
		b.WriteString("\n\n---- 用户指定的审核提示词（最高优先级判据，与内置准则冲突时以用户为准） ----\n")
		b.WriteString(ep + "\n")
	}
	b.WriteString(`

	输出协议（严格遵守，便于机器解析）：对每条提案输出一个 JSON 对象；同一提案的判定对象与理由对象相邻成对；最后输出汇总对象。仅输出 JSON 对象序列，不要任何解释、注释或代码围栏。reason 不得超过 80 字。

	{"id": "提案id", "checks": [{"dim": "有据", "ok": true}, {"dim": "有价值", "ok": false}], "verdict": "approved"}
	{"id": "提案id", "reason": "驳回理由：第一个不过的维度及依据（<=80字）"}
	{"summary": "一句话总体评价"}

	---- 待审核提案 ----
`)
	for _, it := range items {
		fmt.Fprintf(&b, "\n[id=%s] %s\n", it.ID, it.Title)
		if it.Detail != "" {
			b.WriteString("实现要点：" + it.Detail + "\n")
		}
		if it.Verification != "" {
			b.WriteString("验收方式：" + it.Verification + "\n")
		}
		if it.Priority != "" {
			b.WriteString("优先级：" + it.Priority + "  类别：" + it.Category + "\n")
		}
	}
	if len(existing) > 0 {
		b.WriteString("\n---- 既有条目（重复提案应驳回） ----\n")
		for _, t := range existing {
			b.WriteString("- " + t + "\n")
		}
	}
	return b.String()
}

// parseRoadmapReview 解析审核输出（2026-10 协议升级：逐条成对 JSON 对象——
// 判定对象 {id,checks,verdict} 与理由对象 {id,reason} 相邻成对，最后
// {summary}；容错：剥代码围栏后逐个扫描平衡花括号片段；兼容旧版单对象
// {"reviews":[...]}。reason 从成对理由对象合并进同 ID 的 verdict。
// 对齐 LLM-as-a-judge 工程实践：逐条短对象比单个大数组更抗截断/抗稀释）。
func parseRoadmapReview(s string) reviewOutput {
	s = strings.TrimSpace(s)
	var out reviewOutput
	if s == "" {
		return out
	}
	// 逐个扫描顶层 JSON 对象（字符串内的花括号/引号/转义感知的深度计数）。
	var frags []string
	depth, inStr, esc := 0, false, false
	start := -1
	for i, r := range s {
		switch {
		case esc:
			esc = false
		case r == '\\' && inStr:
			esc = true
		case r == '"':
			inStr = !inStr
		case inStr:
			// 字符串内普通字符：不计深度
		case r == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case r == '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					frags = append(frags, s[start:i+1])
					start = -1
				}
			}
		}
	}
	// 单片段且含 reviews 键：旧版单对象协议。
	if len(frags) == 1 && strings.Contains(frags[0], "\"reviews\"") {
		_ = json.Unmarshal([]byte(frags[0]), &out)
		return out
	}
	// 新版逐条成对协议：判定对象与理由对象按 ID 合并。
	byID := map[string]*reviewVerdict{}
	order := []string{}
	for _, f := range frags {
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(f), &probe) != nil {
			continue
		}
		if _, ok := probe["verdict"]; ok {
			var rv reviewVerdict
			if json.Unmarshal([]byte(f), &rv) == nil && rv.ID != "" {
				if _, seen := byID[rv.ID]; !seen {
					order = append(order, rv.ID)
				}
				if byID[rv.ID] == nil {
					byID[rv.ID] = &reviewVerdict{ID: rv.ID}
				}
				byID[rv.ID].Verdict = rv.Verdict
				if rv.Reason != "" {
					byID[rv.ID].Reason = rv.Reason
				}
			}
			continue
		}
		if v, ok := probe["reason"]; ok && !strings.HasPrefix(string(v), "null") {
			var rr struct {
				ID     string `json:"id"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal([]byte(f), &rr) == nil && rr.ID != "" {
				if byID[rr.ID] == nil {
					byID[rr.ID] = &reviewVerdict{ID: rr.ID}
					order = append(order, rr.ID)
				}
				byID[rr.ID].Reason = rr.Reason
			}
			continue
		}
		if v, ok := probe["summary"]; ok && !strings.HasPrefix(string(v), "null") {
			var sm struct {
				Summary string `json:"summary"`
			}
			if json.Unmarshal([]byte(f), &sm) == nil {
				out.Summary = sm.Summary
			}
		}
	}
	for _, id := range order {
		if rv := byID[id]; rv != nil && strings.TrimSpace(rv.Verdict) != "" {
			out.Reviews = append(out.Reviews, *rv)
		}
	}
	return out
}

// ---------- 开发循环（并行/依次） ----------

// roadmapDevelopSessionID 开发会话的固定会话 ID 前缀。
const roadmapDevelopSessionIDPrefix = "roadmap_"

// roadmapMaxParallel 当前并行开发数（autoDev.parallel，1~4；默认 2）。
func (a *App) roadmapMaxParallel() int {
	c := a.autoDevCfg()
	if c.Parallel > 0 {
		if c.Parallel > autoDevMaxParallel {
			return autoDevMaxParallel
		}
		return c.Parallel
	}
	return autoDevDefaultParallel
}

// roadmapDevIsSerial 当前是否串行模式（autoDev.devMode = serial）。
func (a *App) roadmapDevIsSerial() bool { return a.autoDevCfg().DevMode == roadmapDevSerial }

// pickNextApproved 已由 claimNextApproved 取代（2026-10）——见下。
//
// claimNextApproved 原子认领下一条待开发条目（approved → developing 的
// 状态流转与并发额度判定在同一把 roadmapDevMu 内完成，消除双取竞态：
// 旧实现 pickNextApproved 只读返回、落库在循环里另行执行，手动执行/
// AI 审核/autoDev 心跳/完成钩子多个调度 goroutine 并发时可能取到同一条目，
// 同一固定会话被重复拉起（第二次 SendMessage 退化为消息入队重复执行）。
// 额度语义：串行模式同一时刻仅一条（存在任意 developing 即拒绝）；
// 并行模式 developing 条目数 < parallel（1~4）时放行。
func (a *App) claimNextApproved() (roadmap.Item, bool) {
	a.roadmapDevMu.Lock()
	limit := 1
	if !a.roadmapDevIsSerial() {
		limit = a.roadmapMaxParallel()
	}
	var claimed roadmap.Item
	if a.roadmapDevRunningCountLocked() < limit {
		for _, it := range a.roadmapStore.List() {
			if it.Status != roadmap.StatusApproved {
				continue
			}
			// 认领即流转（同一临界区内：另一调度方再进来时该条目已是 developing）。
			if _, err := a.roadmapStore.SetStatus(it.ID, roadmap.StatusDeveloping, "", ""); err == nil {
				claimed = it
				break
			}
		}
	}
	a.roadmapDevMu.Unlock()
	if claimed.ID == "" {
		return roadmap.Item{}, false
	}
	// 会话固定 ID 落库（SessionID 投影到移动端「AR 自主开发」页，点开发卡
	// 直接回看会话过程；部分更新仅写 SessionID，防整体替换清空其他字段）。
	_ = a.roadmapStore.SetSession(claimed.ID, roadmapDevelopSessionID(claimed.ID))
	// 子会话挂批次主任务下面：先建带 ParentID 的会话壳，再回写主任务进度。
	a.ensureRoadmapDevSession(claimed)
	if claimed.ParentSessionID != "" {
		a.ensureRoadmapBatchSession(claimed.ParentSessionID, a.roadmapStore.List())
		a.roadmapAppendBatchProgress(claimed.ParentSessionID, "▶ 启动子任务：「"+claimed.Title+"」")
	}
	a.pushRoadmap()
	uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": true, "sessionId": roadmapDevelopSessionID(claimed.ID)})
	slog.Info("roadmap 开发认领", "item", claimed.ID, "title", claimed.Title,
		"session", roadmapDevelopSessionID(claimed.ID))
	return claimed, true
}

// roadmapDevRunningCountLocked 统计进行中的开发条目数（持 roadmapDevMu 调用）。
func (a *App) roadmapDevRunningCountLocked() int {
	n := 0
	for _, it := range a.roadmapStore.List() {
		if it.Status == roadmap.StatusDeveloping {
			n++
		}
	}
	return n
}

// roadmapSystemPromptDefault 自主开发会话系统提示词内置默认（无人值守守则，
// 用户需求 2026-11：自主开发模式要有系统内置的系统提示词）。每次拉起开发
// 会话时前置注入到任务文本之前；设置-提示词 roadmapSystem 段非空时覆盖。
const roadmapSystemPromptDefault = `# 自主开发守则（无人值守）

你是自主迭代规划板的开发执行 Agent，正在无人值守环境下独立完成规划板已批准的功能点。没有人会实时回答你的问题，请遵守以下守则：

1. 自主决策：不要使用 ask_followup 提问。信息不足时基于代码现状做出最合理的假设并继续推进，把关键假设与理由写进最终交付说明。
2. 改前必读、最小聚焦：动手前先阅读相关代码与项目 AGENTS.md 编码规范；只做本条目范围内的改动，不夹带无关重构，不扩大失败面。
3. 真实验证纪律：完成前必须实际运行项目约定的验证命令（go build ./... / go test ./... 或前端构建等）。从未运行过的「已验证」声明视为伪造，会被机器核验降级为失败并自动重试；收口后规划板还会亲自重新执行验证命令做独立复核——项目实际编译/测试不过同样判失败。
4. 诚实收口：完成后用 attempt_completion 交付，摘要包含改动文件清单与实际执行的验证命令及其结果；遇到无法逾越的卡点（依赖缺失、验证持续失败），如实说明卡点后 attempt_completion，绝不伪造完成。
5. 验收测试先行：动手实现前先确认验收方式真实可执行——条目已给验收方式的照办；未给的先编写最小验收测试或确定验证命令，再进入实现。先有验收标尺，后有实现；实现完成后先跑验收命令，再跑项目级验证命令。`

// roadmapSystemPrompt 自主开发会话系统提示词：设置-提示词 roadmapSystem 段
// 非空时使用用户配置（覆盖式，同 selfCheckTask 语义），空时回退内置默认守则。
func (a *App) roadmapSystemPrompt() string {
	if s := strings.TrimSpace(a.promptStore.Get("roadmapSystem")); s != "" {
		return s
	}
	return roadmapSystemPromptDefault
}

// buildRoadmapLaunchTask 拼装开发会话任务全文：系统守则前置（非空时，与任务
// 文本空行分隔）+ 单条开发任务；守则为空则原样透传（不产生空头）。
func buildRoadmapLaunchTask(sysPrompt, devTask string) string {
	if s := strings.TrimSpace(sysPrompt); s != "" {
		return s + "\n\n" + devTask
	}
	return devTask
}

// launchDevelop 拉起单条开发会话执行链（复用 SendMessage→runTask；会话固定
// ID：重试复用同会话，历史连续可回放）。返回错误 = 启动失败（调用方置 failed）。
// 任务全文 = 系统守则（roadmapSystemPrompt）前置 + 开发任务文本。
func (a *App) launchDevelop(it roadmap.Item) error {
	_, err := a.SendMessage(roadmapDevelopSessionID(it.ID),
		buildRoadmapLaunchTask(a.roadmapSystemPrompt(), buildRoadmapDevTask(it)), nil)
	return err
}

// finishDevelopRun 开发终局统一收口（原 finishDevelop 扩展版，2026-10 执行轨迹）。
// 请求链：失败 → 尝试次数未超上限则自动重试回队（approved），超限置 blocked
// 不再自动开发（防死循环烧预算；人工重跑仍可恢复）；成功即 done。
// 记录链：无论成败均写执行日志（耗时 + 结论 + 失败原因/结果摘要 + 本会话
// 工具调用解析出的改动文件清单）——失败条目原因可追溯。
// 一次流转到位：developing → done/failed/blocked 直接判定终态（不在
// failed 上做二次跳转，避免双跳中间态闪现与状态机校验冲突）。
// 2026-10 harness 增强（对齐 victory-detector「生成者不给自己打分」）：
// 自称完成（err == nil）但会话里从未运行过任何验证命令的收口视为
// 未验证完成——降级为 failed（错误信息注明），由既有重试链路自动重跑，
// 防止「声称完成但从未验证」的虚假胜利进入 done 终态。
func (a *App) finishDevelopRun(it roadmap.Item, result string, runErr error) {
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
	}
	files := a.collectRoadmapRunFiles(roadmapDevelopSessionID(it.ID))
	status := roadmap.StatusDone
	if runErr != nil {
		// 尝试次数超上限：直接置 blocked（不再经过 failed，不自动回队）。
		// Attempts 在认领时已 +1（含本次），直接与上限比较；
		// 未超限：failed 落库后自动重试回队。
		if fresh, ok := a.roadmapStore.Get(it.ID); ok && fresh.Attempts >= a.autoDevMaxAttempts() {
			status = roadmap.StatusBlocked
		} else {
			status = roadmap.StatusFailed
		}
	} else if !a.sessionHasVerifyEvidence(roadmapDevelopSessionID(it.ID)) {
		// 虚假胜利拦截：会话无验证证据 → 不计完成，降级 failed 走重试。
		status = roadmap.StatusFailed
		runErr = fmt.Errorf("完成声明未经机器核验：会话中未运行任何验证命令（需 go build/go test/前端构建等），请实际执行验证后重新收口")
		errMsg = runErr.Error()
		result = "" // 伪造完成不采信结果摘要
		slog.Warn("roadmap 完成声明未验证，降级失败走重试", "item", it.ID, "title", it.Title)
	} else if ok, report := a.runVerifier(a.roadmapItemWorkspace(it)); !ok {
		// 独立验证复核未过（验证权分离第二级防线）：会话内虽跑过验证命令，
		// 但规划板亲自重跑时项目实际编译/测试不过 → 不计完成，降级 failed
		// 走重试，复核报告随失败原因落库并回注重试任务（定向修复）。
		status = roadmap.StatusFailed
		runErr = fmt.Errorf("独立验证复核未通过（规划板亲自执行验证命令，不采信会话自报）：\n%s", report)
		errMsg = runErr.Error()
		result = "" // 复核未过不采信自报摘要
		slog.Warn("roadmap 独立验证复核未过，降级失败走重试", "item", it.ID, "title", it.Title)
	}
	var errMsgForStore string
	if status == roadmap.StatusBlocked {
		errMsgForStore = fmt.Sprintf("重试超限（已尝试 %d 次）："+errMsg, a.autoDevMaxAttempts())
	}
	up, err := a.roadmapStore.SetStatus(it.ID, status, result, errMsgForStore)
	if err == nil {
		// 日志终写：补改动文件清单（与状态变更同一次落盘）。
		_ = a.roadmapStore.AppendRunFiles(it.ID, files)
	}
	switch {
	case err != nil:
		// 状态落库失败（条目已被删等）：仅记录，不再重试/推送。
		slog.Warn("roadmap 开发收口状态落库失败", "item", it.ID, "err", err)
	case status == roadmap.StatusDone:
		a.pushToast("📋 自主迭代：「" + it.Title + "」开发完成")
		if batchID := it.ParentSessionID; batchID != "" {
			a.roadmapAppendBatchProgress(batchID, "✅ 子任务完成：「"+it.Title+"」 — "+truncateRunes(result, 120))
		}
	case status == roadmap.StatusBlocked:
		a.pushToast("⛔ 自主迭代：「" + it.Title + "」连续失败已暂停（尝试 " +
			fmt.Sprint(a.autoDevMaxAttempts()) + " 次），可在面板一键重跑")
		if batchID := it.ParentSessionID; batchID != "" {
			a.roadmapAppendBatchProgress(batchID, "⛔ 子任务重试超限：「"+it.Title+"」 — 已暂停，等待人工重跑")
		}
	default: // failed：未超限，自动重试回队
		_, _ = a.roadmapStore.SetStatus(it.ID, roadmap.StatusApproved, "", "")
		a.pushToast("🔄 自主迭代：「" + it.Title + "」第 " + fmt.Sprint(up.Attempts) + " 次尝试失败，将自动重试")
		if batchID := it.ParentSessionID; batchID != "" {
			a.roadmapAppendBatchProgress(batchID, "🔁 子任务失败待重试：「"+it.Title+"」（尝试 "+fmt.Sprint(up.Attempts)+" 次）— "+truncateRunes(errMsg, 100))
		}
	}
	a.pushRoadmap()
}

// collectRoadmapRunFiles 从开发会话历史解析本次改动文件清单（执行轨迹
// 「改动文件」维度；fs_write/fs_apply_diff 工具卡路径提取，相对工作区路径）。
// 会话不存在/无写操作返回 nil——尽力而为不阻塞收口。
func (a *App) collectRoadmapRunFiles(sessionID string) []string {
	if a.store == nil {
		return nil
	}
	sess, err := a.store.Get(sessionID)
	if err != nil || sess == nil {
		return nil
	}
	type toolPath struct {
		Tool string `json:"tool"`
		Path string `json:"path"`
	}
	var files []string
	for _, m := range sess.Snapshot().Messages {
		if m.Type != message.TypeSay || m.Say != message.SayTool || m.Partial {
			continue
		}
		var tp toolPath
		if json.Unmarshal([]byte(m.Text), &tp) != nil {
			continue
		}
		if (tp.Tool == "fs_write" || tp.Tool == "fs_apply_diff") && strings.TrimSpace(tp.Path) != "" {
			files = append(files, strings.TrimSpace(tp.Path))
		}
	}
	return files
}

// sessionHasVerifyEvidence 开发会话里是否真实运行过验证命令（机器验证
// 声明核验；会话不存在/无 shell 工具卡返回 false——不信任空会话的「完成」）。
func (a *App) sessionHasVerifyEvidence(sessionID string) bool {
	if a.store == nil {
		return false
	}
	sess, err := a.store.Get(sessionID)
	if err != nil || sess == nil {
		return false
	}
	return hasVerifyEvidence(sess.Snapshot().Messages)
}

// buildRoadmapDevTask 拼装单条开发任务提示词（对齐 Zoo-Code 子任务委派语义：
// 自包含上下文 + 明确验收边界 + 收尾要求）。
// 2026-10 harness 增强（对齐 learn-harness-engineering goal-template）：
// 提案验收方式（it.Verification）注入为「完成判据」，生成者不得自判完成。
// 2026-10-25 增强（验收测试先行，AlphaCodium 锚定思想）：
//   - 失败原因回注：自动重试链路保留上次 Error（MarkRetry 人工重跑才清空），
//     尝试次数 >1 且有失败原因时注入任务——run-fix 迭代要有定向修复输入；
//   - 要求第 2 条：动手前先确认/构造可执行验收手段（先验收后实现）；
//   - 要求第 3 条：告知独立复核的存在（收口后规划板亲自重跑验证命令）。
func buildRoadmapDevTask(it roadmap.Item) string {
	var b strings.Builder
	b.WriteString("【自主迭代开发任务】请完成以下功能点（规划板已批准条目）：\n\n")
	b.WriteString("目标：" + it.Title + "\n")
	if d := strings.TrimSpace(it.Detail); d != "" {
		b.WriteString("实现要点：\n" + d + "\n")
	}
	if v := strings.TrimSpace(it.Verification); v != "" {
		b.WriteString("验收方式（完成前逐项核验，收口时机器核验声明）：\n" + v + "\n")
	}
	// 重试回注：上次失败原因（验证复核报告/运行错误在此，AlphaCodium
	// run-fix 迭代思想——失败输出是定向修复的输入，不是重新开始的理由）。
	if it.Attempts > 1 {
		if e := strings.TrimSpace(it.Error); e != "" {
			b.WriteString("上次尝试失败原因（本会话必须针对性解决，勿重蹈）：\n" + e + "\n")
		}
	}
	b.WriteString(`
	要求：
	1. 先阅读相关代码理解现状，再动手实现；遵循项目 AGENTS.md 的编码规范与验证清单；
	2. 验收测试先行：动手前先确认验收方式真实可执行；条目未给验收方式时，先编写最小验收测试或确定验证命令，再进入实现——先有验收标尺，后有实现；
	3. 改动保持最小聚焦，不夹带无关重构；完成后运行项目约定的验证命令（go build ./... / go test ./... 或前端构建）——收口后规划板会亲自重新执行验证命令做独立复核，会话内跑过但项目实际编译/测试不过的交付同样判失败；
	4. 完成后用 attempt_completion 给出一句话改动摘要（涉及文件 + 验证结果，必须注明实际运行的验证命令及其结果要点）；
	5. 验收方式中的验证命令必须真实执行：验证声明未经机器核验、或独立复核未通过的任务将被降级为失败并自动重试。
	若任务无法完成（信息不足/依赖缺失），明确说明卡点后 attempt_completion，不要伪造完成。`)
	return b.String()
}

// roadmapDevelopLoop 开发调度循环：原子认领 approved 队列逐条拉起开发会话
// （并行 N 条 / 串行 1 条）。每条完成（done/failed）后由 runTask 完成钩子
// （roadmapDevelopNext）触发补位；队列空或并发满时退出，由 StartRoadmapDev /
// AI 审核 / 重试 / autoDev 心跳再次拉起。
func (a *App) roadmapDevelopLoop() {
	for {
		it, ok := a.claimNextApproved()
		if !ok {
			// 队列空/并发已满：唤醒自动循环（若开启）触发下一轮评估。
			a.roadmapAutoDevMaybe()
			return
		}
		sessID := roadmapDevelopSessionID(it.ID)
		slog.Info("roadmap 开发启动", "item", it.ID, "title", it.Title, "session", sessID)
		if err := a.launchDevelop(it); err != nil {
			// 会话已在运行（固定 ID 防重入）等启动失败 → 标记失败并继续下一条。
			a.finishDevelopRun(it, "", err)
			continue
		}
		// 串行模式：首条已拉起，后续由完成钩子接力（roadmapDevelopNext）；
		// 并行模式：继续认领下一条，直至占满并行额度或队列空。
		if a.roadmapDevIsSerial() {
			return
		}
	}
}

// roadmapDevelopNext 开发收口：回收当前条目结果并接力下一条（runTask 完成钩子调用）。
func (a *App) roadmapDevelopNext(itemID string, result string, err error) {
	it, ok := a.roadmapStore.Get(itemID)
	if !ok {
		return
	}
	// 只处理 developing 状态（防重复收口）。
	if it.Status != roadmap.StatusDeveloping {
		return
	}
	a.finishDevelopRun(it, result, err)
	// 接力下一条（队列空时 roadmapDevelopLoop 内部自动唤醒规划）。
	// 额度不在此计数（2026-10：审核通过时已计入有效数量，开发完成重复计数
	// 会让每日上限减半）。
	go a.roadmapDevelopLoop()
}

// ---------- 规划（LLM 调用） ----------

// planRoadmapItem LLM 规划产出条目（JSON 协议）。
type planRoadmapItem struct {
	Title        string `json:"title"`
	Detail       string `json:"detail"`
	Verification string `json:"verification,omitempty"` // 验收方式（完成判据，开发收口机器核验依据）
	Priority     string `json:"priority"`
	Category     string `json:"category"`
}

// planRoadmapOutput 规划双产出：提案清单 + 一句话现状摘要。
type planRoadmapOutput struct {
	Items   []planRoadmapItem `json:"items"`
	Summary string            `json:"summary"`
}

// planResult 规划结果（返回前端：提案列表 + 摘要，供详情区直接展示选中）。
type planResult struct {
	Items   []roadmap.Item `json:"items"`
	Summary string         `json:"summary"`
}

// PlanRoadmap 手动触发规划（面板「AI 规划」按钮）：聚合项目上下文 →
// 一次非流式 LLM 调用产出提案 → 落 proposed（默认待审核）→ 推送面板。
func (a *App) PlanRoadmap() error {
	return a.PlanRoadmapWithGuidance("")
}

// PlanRoadmapWithGuidance 带用户引导意见的规划（面板「AI 规划」入口）：
// 用户可先输入方向性意见（如「优先优化移动端体验」「下阶段主攻性能」），
// 意见会作为最高优先级约束注入规划提示词——产出提案围绕该意图展开，
// 仍落 proposed（用户选择方案勾选同意后才开发）。
func (a *App) PlanRoadmapWithGuidance(guidance string) error {
	guidance = strings.TrimSpace(guidance)
	go func() {
		if err := a.roadmapPlanOnce("manual", a.autoDevMaxPlanItems(), guidance); err != nil {
			a.pushToast("📋 规划失败：" + truncateRunes(err.Error(), 120))
			slog.Warn("roadmap 规划失败", "err", err)
			return
		}
		// 自动审核开启：规划产出直接交 AI 审核（通过即排期开发，
		// 无需人工点「AI 审核」；用户需求 2026-10：是否开启自动审核可设）。
		if a.autoDevCfg().AutoReview {
			a.roadmapAutoAIReview()
		}
	}()
	return nil
}

// roadmapPlanOnce 执行一次规划（source 标记来源：manual | autodev；
// guidance 用户引导意见，非空时注入提示词作为最高优先级方向约束）。
func (a *App) roadmapPlanOnce(source string, maxItems int, guidance string) error {
	if strings.TrimSpace(a.GetConfig().APIKey) == "" {
		return errors.New("请先在设置中填写 API Key")
	}
	digest := a.buildRoadmapDigest()
	if g := strings.TrimSpace(guidance); g != "" {
		digest += "\n\n---- 用户引导意见（本轮规划的最高优先级方向，提案需围绕它展开） ----\n" + g + "\n"
	}
	// 驳回反馈回注：近期被驳回提案（标题+理由）注入规划提示词（Reflexion 式闭环）。
	// 2026-10 增强：同时注入近期审核通过条目（标题+通过理由）——规划器只有
	// 正负样本都见过，才知道审核官的口味；只看驳回会保守化。
	// 全驳回后自动升级：上一轮全驳时注入审核升级注入语（加大证据锚点要求，
	// 防「规划—审核」之间反复同一层次空转，对齐 ISR-LLM 迭代自修正）。
	var reviewMem []string
	upgraded := a.roadmapAutoDev.PlanBlockedByZero()
	reviewMem = append(reviewMem, a.recentApprovedFeedback(4)...)
	reviewMem = append(reviewMem, a.recentRejectionFeedback(6)...)
	prompt := roadmapPlanPrompt(digest, a.existingRoadmapTitles(), reviewMem, maxItems, upgraded)
	cfg := a.GetConfig()
	client := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	resp, err := client.Generate(ctx, llm.ChatRequest{Messages: []llm.Message{{
		Role: llm.RoleUser, Content: digest + "\n\n" + prompt,
	}}})
	if err != nil {
		return fmt.Errorf("规划模型调用失败: %w", err)
	}
	out := parseRoadmapPlan(resp.Content)
	if len(out.Items) == 0 {
		return errors.New("规划未产出有效提案（模型输出无法解析）")
	}
	if maxItems > 0 && len(out.Items) > maxItems {
		out.Items = out.Items[:maxItems]
	}
	ws := a.cfgWorkspace()
	added := 0
	for _, p := range out.Items {
		title := strings.TrimSpace(p.Title)
		if title == "" {
			continue
		}
		if _, err := a.roadmapStore.Add(roadmap.Item{
			Title: title, Detail: p.Detail, Verification: p.Verification,
			Priority: p.Priority, Category: p.Category,
			Status: roadmap.StatusProposed, // 默认待审核（用户勾选后开发）
			Source: source, Workspace: ws,
		}); err == nil {
			added++
		}
	}
	if added > 0 {
		a.pushToast(fmt.Sprintf("📋 自主规划：新增 %d 条待审核提案（去面板勾选开发）", added))
	}
	a.pushRoadmap()
	return nil
}

// buildRoadmapDigest 聚合规划素材：项目结构 + git 近况 + 既有规划 + 自进化经验。
func (a *App) buildRoadmapDigest() string {
	var b strings.Builder
	ws := a.cfgWorkspace()
	b.WriteString("---- 规划素材（项目现状） ----\n\n")
	// 1) 项目结构（浅层目录 + 关键文件，tools.ListFilesTool 同款轻量语义）。
	if ws != "" {
		b.WriteString("[项目结构]（顶层目录与文件）\n")
		if entries, err := os.ReadDir(ws); err == nil {
			n := 0
			for _, e := range entries {
				if e.IsDir() && (e.Name() == "node_modules" || e.Name() == ".git" || e.Name() == "vendor" || e.Name() == "dist" || e.Name() == "build") {
					continue
				}
				suffix := ""
				if e.IsDir() {
					suffix = "/"
				}
				fmt.Fprintf(&b, "  %s%s\n", e.Name(), suffix)
				n++
				if n >= 40 {
					break
				}
			}
		}
		// 1b) Go 包清单（模块级地图：规划器提「可行性/涉及模块」才有依据）。
		if pkgs := goPackageList(ws); pkgs != "" {
			b.WriteString("\n[Go 包清单]（internal/* 模块职责地图）\n" + pkgs + "\n")
		}
		// 1c) 前端组件清单（UI 类提案的落点依据）。
		if comps := vueComponentList(ws); comps != "" {
			b.WriteString("\n[前端组件清单]（frontend-vue/src/components）\n" + comps + "\n")
		}
		// 2) git 近况（最近提交主题：了解迭代脉络）。
		if out := gitLogSubject(ws, 12); out != "" {
			b.WriteString("\n[git 最近提交]\n" + out + "\n")
		}
	}
	// 3) 既有规划条目（状态概览：避免重复提案）。
	if items := a.roadmapStore.List(); len(items) > 0 {
		b.WriteString("\n[规划板既有条目]\n")
		for _, it := range items {
			fmt.Fprintf(&b, "- [%s] %s\n", it.Status, it.Title)
		}
	}
	// 4) 自进化经验卡标题（项目约定的痛点沉淀，规划时参考）。
	if cards := a.evolveStore.List(); len(cards) > 0 {
		b.WriteString("\n[项目经验卡（近期踩坑沉淀）]\n")
		for i, c := range cards {
			if i >= 8 {
				break
			}
			fmt.Fprintf(&b, "- %s\n", c.Title)
		}
	}
	out := b.String()
	if r := []rune(out); len(r) > 16000 {
		out = string(r[len(r)-16000:])
	}
	return out
}

// goPackageList internal/* Go 包清单（包名 + 文件数；非 Go 项目/目录不存在返回空）。
func goPackageList(ws string) string {
	dir := filepath.Join(ws, "internal")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, e := range entries {
		if !e.IsDir() || n >= 25 {
			continue
		}
		if files, err := os.ReadDir(filepath.Join(dir, e.Name())); err == nil {
			goFiles := 0
			for _, f := range files {
				if strings.HasSuffix(f.Name(), ".go") {
					goFiles++
				}
			}
			fmt.Fprintf(&b, "  internal/%s（%d 个 go 文件）\n", e.Name(), goFiles)
			n++
		}
	}
	return b.String()
}

// vueComponentList 前端组件清单（frontend-vue/src/components/*.vue 文件名）。
func vueComponentList(ws string) string {
	dir := filepath.Join(ws, "frontend-vue", "src", "components")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".vue") {
			names = append(names, e.Name())
		}
		if len(names) >= 30 {
			break
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "  " + strings.Join(names, ", ")
}

// existingRoadmapTitles 既有条目标题（规划去重）。
func (a *App) existingRoadmapTitles() []string {
	var out []string
	for _, it := range a.roadmapStore.List() {
		out = append(out, it.Title)
	}
	return out
}

// recentRejectionFeedback 近期被驳回提案的标题与审核理由（最新在前，最多 n 条）。
// Reflexion 式反馈回注：规划器看到「什么被驳回、为什么」，下一轮不再提同类
// 提案——驳回理由不回注是「反复提同类被反复驳回」的根本原因。
// 2026-10 修复：反馈源从「扫 store rejected 条目」改为 store 驳回环形日志
// （rejectDelete=true 时条目直接删除，旧实现扫不到任何内容，回注恒空）。
func (a *App) recentRejectionFeedback(n int) []string {
	return a.roadmapStore.RejectionFeedback(n)
}

// recentApprovedFeedback 近期 AI 审核通过条目的标题与通过理由（正样本回注：
// 规划器对齐审核官口味用；取最新 n 条，最新在前）。
func (a *App) recentApprovedFeedback(n int) []string {
	if n <= 0 {
		return nil
	}
	items := a.roadmapStore.List()
	var out []string
	for i := len(items) - 1; i >= 0 && len(out) < n; i-- {
		it := items[i]
		if it.ApprovedBy == "ai_review" && strings.TrimSpace(it.Review) != "" {
			out = append(out, "「"+it.Title+"」——"+it.Review)
		}
	}
	return out
}

// roadmapPlanPrompt 规划提示词（2026-10 重写，治「提案全被审核驳回」）。
// 核心思路：旧提示词让规划器「凭素材想象问题」，产出的提案断言无法被审核官
// 核验（有据维度必挂）——新提示词要求每条提案从素材出发：先列出素材中
// 真实存在的问题信号（文件/提交/条目/经验卡），再基于信号提条目，关键断言
// 必须能指回素材出处（对齐 Anthropic《Building Effective Agents》研究-计划
// 模式与 ISR-LLM 迭代自修正；拒绝凭空规划）。
// rejected 正负样本记忆（近期审核通过+驳回的提案与理由）；upgraded 全驳回
// 后自动升级模式（加大证据要求：证据不足宁可不提）。
func roadmapPlanPrompt(digest string, existing []string, rejected []string, maxItems int, upgraded bool) string {
	if maxItems <= 0 {
		maxItems = autoDevDefaultMaxPlanItems
	}
	var b strings.Builder
	b.WriteString(`你是本项目的自主迭代规划器。基于下面的项目素材，规划下一批最值得做的条目。

	规划方法（严格按此顺序）：
	第一步：从素材中提取真实的问题信号（bug 修复记录中反复出现的模块、git 提交主题里的未竟方向、
	既有条目的 failed/blocked 原因、经验卡踩坑点、目录结构中明显缺失的能力），列成清单；
	第二步：只为这些信号设计提案——每条提案的「现状断言」必须能指出素材出处（文件名/提交主题/条目状态/经验卡），
	审核官会按出处核验，无出处的断言视为臆测；
	第三步：为每条提案写出可执行的验证方式（验证命令或可核验检查点），自己先预演一遍能否验收。

	规划原则：
	- 只提可独立交付的小粒度条目（单条开发任务 1~2 小时可完成；大功能拆多条）；
	- 优先级排序依据：用户痛点 > 阻塞性缺陷 > 高频使用路径的体验 > 代码健康度 > 文档；
	- 与既有条目重复的直接跳过（既有清单见下方）；
	- 每条给出可执行的实现要点（文件/模块级方向，不写代码）；

	规划维度（用户要求四维均衡覆盖，一轮提案应尽量兼顾多维度，单维度最多占一半）：
	- framework（框架/架构）：模块解耦、性能与稳定性、工程化短板（tooling 类）；
	- feature（功能）：用户可感知的新能力，必须指明落在哪个模块/面板；
	- ui（UI/体验）：交互改进、布局优化、视觉一致性，必须指明具体页面/组件；
	- bug（缺陷）：已知问题、边界处理、错误处理缺失，必须写明复现路径或缺陷现象。

	自查门槛（产出前逐条预检，与 AI 审核同一把尺子，预检不过的提案必被驳回）：
	` + roadmapReviewCriteria + `
	- 宁少勿滥：数量不重要，通过率重要；没有高把握的就少提或不提。`)
	if upgraded {
		b.WriteString(`

	【升级模式】上一轮提案全部被审核驳回。本轮必须更保守：
	只提你能在素材中找到直接证据支撑的条目（引用具体文件名/提交主题/条目状态），
	证据不足的方向宁可不提（给空数组也比被全驳好）；
	每条提案的 detail 第一句先写证据出处（如「素材[git 最近提交]显示…」「素材[项目结构]中 internal/xxx 存在…」）。`)
	}
	b.WriteString(`

	输出协议：仅输出一个 JSON 对象，不要任何解释、注释或代码围栏。
	{"items": [{"title": "一句话功能点（<=40字）", "detail": "实现要点（<=300字：现状依据（素材出处）+ 涉及文件/模块 + 具体做法 + 验收标准）", "verification": "验收方式（可执行验证命令或可核验检查点，<=200字，如：go build ./... && go test ./... 通过；或：面板列表页展示XX且无报错）", "priority": "high|medium|low", "category": "feature|bug|refactor|docs|tooling"}], "summary": "一句话项目现状评估"}
	最多 `)
	fmt.Fprintf(&b, "%d", maxItems)
	b.WriteString(" 条；没有值得做的就给空数组。")

	b.WriteString("\n\n既有条目（勿重复）：\n")
	for _, t := range existing {
		b.WriteString("- " + t + "\n")
	}
	// 正负样本记忆回注（Reflexion）：近期审核通过与驳回的提案及理由，
	// 让规划器知道审核官的口味（只看负样本会过度保守化）。
	if len(rejected) > 0 {
		b.WriteString("\n近期审核反馈（同类提案勿重复被驳；通过样本代表审核官口味，可参考同类方向）：\n")
		for _, r := range rejected {
			b.WriteString("- " + r + "\n")
		}
	}
	return digest + "\n\n" + b.String()
}

// parseRoadmapPlan 解析规划输出（容错：剥代码围栏，截取首尾花括号）。
func parseRoadmapPlan(s string) planRoadmapOutput {
	s = strings.TrimSpace(s)
	var out planRoadmapOutput
	if s == "" {
		return out
	}
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			_ = json.Unmarshal([]byte(s[i:j+1]), &out)
		}
	}
	return out
}

// gitLogSubject 最近提交主题（规划素材；非 git 仓库/git 不可用返回空）。
func gitLogSubject(ws string, n int) string {
	if ws == "" || n <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", ws, "log", "--oneline", "-n", fmt.Sprint(n))
	sysproc.Hide(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// gitLogRecent 最近提交主题（规划素材；非 git 仓库返回空）。
func gitLogRecent(ws string, n int) string {
	return gitLogSubject(ws, n)
}

// ---------- 自动迭代（autoDev 全自主模式） ----------

// roadmapAutoDevStart 启动自动迭代循环（幂等；已跑则跳过）。
func (a *App) roadmapAutoDevStart() {
	a.roadmapAutoDevMu.Lock()
	defer a.roadmapAutoDevMu.Unlock()
	if a.roadmapAutoDev.Running {
		return
	}
	a.roadmapAutoDev.Running = true
	go a.roadmapAutoDevLoop()
	a.pushToast("🤖 自主迭代已开启：系统将自动规划、AI 审核并持续开发（面板可查看进度）")
}

// roadmapAutoDevStop 停止自动循环（不中断正在跑的开发会话）。
func (a *App) roadmapAutoDevStop() {
	a.roadmapAutoDevMu.Lock()
	a.roadmapAutoDev.Running = false
	a.roadmapAutoDevMu.Unlock()
}

// roadmapAutoDevLoop 自动迭代主循环：每轮 = 补货判定（开发中 < 最低并行数
// 时持续规划 + AI 审核，不受 30 分钟节流限制）→ 存量提案 AI 审核 → 常规规划
// → 消费开发队列 → 等待下一周期。配置关闭即退出；开发接力由完成钩子驱动，
// 本循环仅做心跳。
// 2026-10 重构（用户反馈「开启完全自主后无任何执行」）：
//   - 存量 proposed 始终可被 AI 审核：旧实现 hasPending 时跳过规划+审核，
//     排期队列恒空永不消费（10-04 起 2 条 proposed 未被处理）；
//   - 审核独立于规划：规划节流（30 分钟）只约束新提案产出，审核/开发每轮都跑；
//   - 启动即醒：StartRoadmapDev/AI 审核/重试后 wake 立即进入下一轮，
//     不再等 2h 心跳（用户反馈「重启后没有新会话，按说开启就该有任务在跑」）。
//
// 2026-10 补货（用户需求：开发中任务数为 0/低于最低并行数时，一直规划任务
// 进行审核，审核失败直接删除）：
//   - 开发中 + 排期 < 最低并行数 = 产能不足：立即规划（豁免 30 分钟节流）
//     → AI 审核；全驳回时置 LastPlanAt = now（短间隔 autoDevRetryInterval=1min
//     重试，配合零通过熔断防空转烧钱——连续 3 轮全驳当日停规划）；
//   - 补货不受 hasPending 约束（排期有货但低于阈值仍需补）；
//   - 驳回直接删除（rejectDelete）在本轮审核内生效，驳回反馈仍回注规划器。
func (a *App) roadmapAutoDevLoop() {
	for {
		a.roadmapAutoDevMu.Lock()
		running := a.roadmapAutoDev.Running
		a.roadmapAutoDevMu.Unlock()
		if !running {
			return
		}
		cfg := a.autoDevCfg()
		quotaOK := a.roadmapAutoDev.AllowAutoDev(a.autoDevDailyLimit())

		// 0) 补货判定（用户需求 2026-10）：开发中 + 排期数 < 最低并行数 =
		//    产能不足 → 立即规划 + AI 审核（豁免节流；额度与零通过熔断照常生效）。
		//    注意用「开发中 + 排期」而非仅开发中：只看开发中会导致并行拉满
		//    但排期空时下轮完成钩子接力后无货可补。
		minDev := a.autoDevMinDeveloping()
		developing, approved := a.roadmapPendingCounts()
		if quotaOK && developing+approved < minDev {
			slog.Info("roadmap 补货触发：管线存粮低于最低并行数", "developing", developing,
				"approved", approved, "min", minDev)
			a.roadmapAutoDevMu.Lock()
			a.roadmapAutoDev.LastPlanAt = time.Now()
			a.roadmapAutoDevMu.Unlock()
			if err := a.roadmapPlanOnce("autodev", a.autoDevMaxPlanItems(), ""); err != nil {
				slog.Warn("roadmap 自动规划失败", "err", err)
			}
			a.roadmapAutoAIReview() // 驳回直接删除（rejectDelete）在审核内生效
			a.roadmapAutoDevMu.Lock()
			refilled := false
			for _, it := range a.roadmapStore.List() {
				if it.Status == roadmap.StatusApproved || it.Status == roadmap.StatusDeveloping {
					refilled = true
					break
				}
			}
			a.roadmapAutoDevMu.Unlock()
			if !refilled {
				// 本轮补货无产出（全驳回/规划空）：推迟到短间隔后重试，
				// 防立即重试空转；零通过熔断（连续 3 轮全驳）照常拦截。
				a.roadmapAutoDevMu.Lock()
				a.roadmapAutoDev.LastPlanAt = time.Now()
				a.roadmapAutoDevMu.Unlock()
			}
			// 补货后直接消费队列再等待（不落入下方常规规划：本轮已规划过）。
			a.roadmapAutoConsumeQueue()
			// 未补满时用短间隔（autoDevRetryInterval）持续重试——「一直规划」
			// 语义：不降低到正常周期；补满/零通过熔断后自然回到正常间隔
			//（下一轮补货判定不再触发，或熔断拦截规划只等审核/开发）。
			wait := intervalOf(cfg.IntervalMin)
			if !refilled && !a.roadmapAutoDev.PlanBlockedByZero() {
				wait = autoDevRetryInterval
			}
			if !a.roadmapAutoDevWait(wait) {
				return
			}
			continue
		}

		// 1) 存量待审核提案 → AI 审核（无规划节流：审核只消费已有提案；
		//    空队列时不调 LLM 空转）。用户引导意见在 manual 路径注入。
		if quotaOK {
			a.roadmapAutoAIReview()
		}

		// 2) 无待办时才触发一轮自主规划（节流 30 分钟 + 每日额度 + 零通过熔断）。
		_, approvedCnt := a.roadmapPendingCounts()
		hasPending := approvedCnt > 0
		a.roadmapAutoDevMu.Lock()
		planDue := time.Since(a.roadmapAutoDev.LastPlanAt) >= planThrottleInterval
		a.roadmapAutoDevMu.Unlock()
		// 零通过熔断：连续多轮规划产出全被驳回时当日不再自动规划（防「规划→
		// 全驳回→零消耗→立刻再规划」无限空转；审核/开发不受影响，用户调整
		// 审核提示词或人工通过后自动恢复）。驳回反馈回注规划提示词治本 + 本熔断治标。
		zeroBlocked := a.roadmapAutoDev.PlanBlockedByZero()
		if zeroBlocked {
			slog.Info("roadmap 零通过熔断生效：连续多轮规划全被驳回，当日暂停自动规划")
		}
		if !hasPending && planDue && !zeroBlocked && a.roadmapAutoDev.AllowAutoDev(a.autoDevDailyLimit()) {
			a.roadmapAutoDevMu.Lock()
			a.roadmapAutoDev.LastPlanAt = time.Now()
			a.roadmapAutoDevMu.Unlock()
			if err := a.roadmapPlanOnce("autodev", a.autoDevMaxPlanItems(), ""); err != nil {
				slog.Warn("roadmap 自动规划失败", "err", err)
			}
			// 自动模式：新提案交 AI 审核（用户指定提示词作补充判据）——
			// 通过的自动纳入排期并触发并行开发；驳回的留痕/删除。
			a.roadmapAutoAIReview()
		} else if zeroBlocked && !hasPending {
			// 零通过熔断中且无事可做：等下一心跳再检查，不高频空转。
			slog.Info("roadmap 自动规划已熔断（连续全驳回），等待次日恢复或人工干预")
		}

		// 3) 消费开发队列。
		a.roadmapAutoConsumeQueue()

		// 4) 等待下一周期（关闭信号或间隔到点）。
		if !a.roadmapAutoDevWait(intervalOf(cfg.IntervalMin)) {
			return
		}
	}
}

// roadmapPendingCounts 管线计数：开发中 + 排期中条目数（补货判定用：
// 两者之和 < 最低并行数 = 产能不足，需持续规划补货）。
func (a *App) roadmapPendingCounts() (developing, approved int) {
	for _, it := range a.roadmapStore.List() {
		switch it.Status {
		case roadmap.StatusDeveloping:
			developing++
		case roadmap.StatusApproved:
			approved++
		}
	}
	return developing, approved
}

// roadmapAutoConsumeQueue 消费开发队列（原主循环第 3 步抽出复用：补货路径
// 与常规路径同语义——approved 条目原子认领后并行开发，完成钩子接力）。
func (a *App) roadmapAutoConsumeQueue() {
	if !a.roadmapAutoDev.AllowAutoDev(a.autoDevDailyLimit()) {
		return
	}
	for {
		if it, ok := a.claimNextApproved(); ok {
			if err := a.launchDevelop(it); err != nil {
				a.finishDevelopRun(it, "", err)
			} else {
				break // 并行拉满或已拉起；其余由完成钩子接力
			}
		} else {
			break
		}
	}
}

// roadmapAutoDevWait 等待下一周期（关闭返回 false；唤醒/超时返回 true）。
// 补货未满时用短间隔（autoDevRetryInterval）轮询，正常周期用配置间隔。
func (a *App) roadmapAutoDevWait(interval time.Duration) bool {
	select {
	case <-a.roadmapStopCh:
		return false
	case <-a.roadmapWakeCh:
		// 配置变更/开发接力/手动触发：立即进入下一轮。
		return true
	case <-time.After(interval):
		return true
	}
}

// intervalOf 配置间隔归一（分钟；0 = 默认 2h；低于补货重试间隔时抬到重试间隔，
// 防 intervalMin=1 配置与 1 分钟补货重试互相踩踏）。
func intervalOf(min int) time.Duration {
	if min <= 0 {
		return autoDevDefaultIntervalMin * time.Minute
	}
	d := time.Duration(min) * time.Minute
	if d < autoDevRetryInterval {
		return autoDevRetryInterval
	}
	return d
}

// roadmapAutoAIReview 自动模式的 AI 审核入口（异步；审核提示词取配置）。
func (a *App) roadmapAutoAIReview() {
	cfg := a.GetAutoDevConfig()
	items := a.reviewTargets(nil)
	if len(items) == 0 {
		return
	}
	if err := a.roadmapReviewOnce(items, cfg.ReviewPrompt); err != nil {
		slog.Warn("roadmap AI 审核失败", "err", err)
	}
}

// roadmapAutoDevMaybe 队列排空后的规划入口（开发循环调用）：自动模式开启时
// 唤醒主循环立即评估（节流由主循环 LastPlanAt 保证）。
func (a *App) roadmapAutoDevMaybe() { a.wakeRoadmapAutoDev() }

// wakeRoadmapAutoDev 非阻塞唤醒自动循环（容量 1 通道，重复唤醒合并）。
func (a *App) wakeRoadmapAutoDev() {
	select {
	case a.roadmapWakeCh <- struct{}{}:
	default:
	}
}

// autoDevCfg 便捷读取（内部用）。
func (a *App) autoDevCfg() AutoDevConfig { return a.GetAutoDevConfig() }

// autoDevMaxAttempts 单条目开发尝试上限（0 = 默认 2；上限封顶 autoDevMaxAttempts）。
func (a *App) autoDevMaxAttempts() int {
	c := a.autoDevCfg()
	if c.MaxAttempts > 0 {
		if c.MaxAttempts > autoDevMaxAttempts {
			return autoDevMaxAttempts
		}
		return c.MaxAttempts
	}
	return autoDevDefaultMaxAttempts
}

// autoDevDailyLimit 每日开发上限（0 = 默认）。
func (a *App) autoDevDailyLimit() int {
	c := a.autoDevCfg()
	if c.DailyLimit > 0 {
		return c.DailyLimit
	}
	return autoDevDefaultDailyLimit
}

// autoDevMaxPlanItems 单次规划提案数上限（0 = 默认 5；硬顶 autoDevMaxPlanItemsHard）。
// 手动/远端规划路径同源生效（用户需求 2026-10：规划任务数可设置）。
func (a *App) autoDevMaxPlanItems() int {
	c := a.autoDevCfg()
	if c.MaxPlanItems > 0 {
		if c.MaxPlanItems > autoDevMaxPlanItemsHard {
			return autoDevMaxPlanItemsHard
		}
		return c.MaxPlanItems
	}
	return autoDevDefaultMaxPlanItems
}

// autoDevMinDeveloping 最低并行开发数（0 = 默认 1；超过并行上限时钳到上限，
// 防补货目标高于并行容量导致永远补不满——每次审核通过后额度计数+1，
// 额度耗尽当日补货自然停）。
func (a *App) autoDevMinDeveloping() int {
	c := a.autoDevCfg()
	if c.MinDeveloping <= 0 {
		return 1
	}
	if c.MinDeveloping > autoDevMaxParallel {
		return autoDevMaxParallel
	}
	return c.MinDeveloping
}

// roadmapRejectDelete 审核驳回是否直接删除（autoDev.rejectDelete）。
func (a *App) roadmapRejectDelete() bool { return a.autoDevCfg().RejectDelete }

// roadmapReviewMode 审核宽松度（autoDev.reviewMode：lenient 宽松红线一票
// 否决制（默认）| standard 严审 7 维全过；历史值 strict 归一为 standard）。
func (a *App) roadmapReviewMode() string {
	switch strings.ToLower(strings.TrimSpace(a.autoDevCfg().ReviewMode)) {
	case roadmapReviewModeStandard, "strict":
		return roadmapReviewModeStandard
	default:
		return roadmapReviewModeLenient
	}
}
