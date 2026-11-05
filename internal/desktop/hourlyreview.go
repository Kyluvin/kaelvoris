// hourlyreview.go —— 每小时「全会话复盘」：聚合近期所有会话，找卡点/困难，
// 沉淀为「工具/提示词/功能改进建议」经验卡（自进化闭环的定时全景入口）。
//
// 与既有两条复盘链路的分工（三层递进，互不替代）：
//   - selflearn.go（任务级）：每轮任务结束即时复盘单会话，双路产出经验卡 +
//     会话记忆收尾，粒度细、频次高；
//   - cron_panel.go CreateSelfReviewJob（每日级）：跑一个 code 模式 Agent
//     会话（提示词驱动，走完整执行链），让 Agent 自查代码沉淀经验卡；
//   - 本文件（每小时级，系统级）：不跑 Agent 引擎（复盘只需"读+判断+产出"，
//     单次非流式 Generate 足够，省掉几十轮工具循环的成本），由宿主进程直接
//     聚合「最近窗口内有活动的全部会话」的转录信号（任务/错误/未完成待办/
//     工具调用频次）+ cron 执行记录，一次 LLM 调用产出「困难清单 + 改进建议
//   - 经验卡」，经 evolveStore 落库——下次任务命中关键词即注入，闭环收口。
//
// 触发链路：定时任务（task = hourlyReviewSentinel 哨兵文本）→ cronRunner →
// isHourlyReviewTask 命中 → 后台 goroutine 执行 hourlyReviewRun（不进引擎，
// 不产生 Agent 轮次）→ 复盘报告落哨兵会话（可回放）+ toast 轻提示。
//
// 约束（对齐 selflearn 的防失控红线）：
//   - 同一时刻至多 1 个复盘 LLM 调用（独立互斥，不复用 selfReviewing）；
//   - 无新会话活动（转录与上次一致）时跳过，不空转 LLM 调用；
//   - 单次最多沉淀 hourlyReviewMaxCards 张卡；用户配置类问题不沉淀成卡。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"kaelvoris/internal/evolve"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/session"
	"kaelvoris/pkg/message"
)

// ---------- 配置与上限 ----------

const (
	// hourlyReviewMaxCards 单次复盘最多落库卡数（对齐 selfReviewMaxCards 防失控）。
	hourlyReviewMaxCards = 3
	// hourlyReviewMaxDigest 聚合转录总上限（rune，超出保留尾部——最近的会话价值最高）。
	hourlyReviewMaxDigest = 20000
	// hourlyReviewMaxSessions 单次复盘最多覆盖会话数（按最近活动排序取前 N）。
	hourlyReviewMaxSessions = 12
	// hourlyReviewWindow 复盘窗口：仅纳入窗口内有更新的会话（26h > 24h，
	// 覆盖"应用退出错过整点"的跨天场景）。
	hourlyReviewWindow = 26 * time.Hour
	// hourlyReviewTimeout 复盘 LLM 调用超时。
	hourlyReviewTimeout = 5 * time.Minute
)

// hourlyReviewSentinel 每小时复盘定时任务的哨兵提示词：cron 任务 task 等于
// 该文本时走系统级复盘（不进引擎）。导出给 cron 面板一键创建复用。
const hourlyReviewSentinel = "【系统任务】每小时复盘全部会话：聚合近期会话活动，找出 Agent 遇到的困难与卡点，沉淀为可落地的改进建议经验卡（改提示词/改工具/新增工具）。"

// isHourlyReviewTask 判断任务文本是否为每小时复盘哨兵（cronRunner 前置分流）。
func isHourlyReviewTask(task string) bool {
	return strings.HasPrefix(strings.TrimSpace(task), "【系统任务】每小时复盘全部会话")
}

// hourlyReviewState 复盘运行态（防并发 LLM 调用 + 空转去重游标）。
type hourlyReviewState struct {
	mu sync.Mutex
	// reviewing 复盘在跑（同一时刻至多一个 LLM 复盘调用）。
	reviewing bool
	// lastDigest 上次复盘的聚合转录（完全一致 = 无新活动，跳过本次）。
	lastDigest string
}

var hourlyReview hourlyReviewState

// ---------- 复盘主流程 ----------

// hourlyReviewRun 执行一次全会话复盘：聚合 → 单次 LLM 产出 → 经验卡落库。
// 报告作为助手消息落哨兵会话（会话列表可回放完整复盘结果）。
func (a *App) hourlyReviewRun(sessionID string) {
	// 互斥：同一时刻至多一个复盘 LLM 调用（独立于 selflearn 的任务级复盘）。
	hourlyReview.mu.Lock()
	if hourlyReview.reviewing {
		hourlyReview.mu.Unlock()
		slog.Info("每小时复盘：已有复盘在跑，跳过本次", "session", sessionID)
		return
	}
	hourlyReview.reviewing = true
	hourlyReview.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("每小时复盘 panic（已拦截）", "session", sessionID, "panic", r)
		}
		hourlyReview.mu.Lock()
		hourlyReview.reviewing = false
		hourlyReview.mu.Unlock()
	}()

	digest := a.buildHourlyDigest()
	if strings.TrimSpace(digest) == "" {
		a.pushHourlyReviewResult(sessionID, "本次复盘：窗口内无会话活动，跳过。")
		return
	}
	// 空转去重：聚合转录与上次完全一致 = 窗口内没有新会话活动。
	hourlyReview.mu.Lock()
	if digest == hourlyReview.lastDigest {
		hourlyReview.mu.Unlock()
		slog.Info("每小时复盘：无新会话活动，跳过", "session", sessionID)
		a.pushHourlyReviewResult(sessionID, "本次复盘：无新会话活动，跳过。")
		return
	}
	hourlyReview.lastDigest = digest
	hourlyReview.mu.Unlock()

	// 既有卡标题：同名输出 = 更新该卡（复用 Store.Add 同标题合并语义）。
	var existing []string
	for _, c := range a.evolveStore.List() {
		existing = append(existing, c.Title)
	}

	cfg := a.GetConfig()
	client := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	ctx, cancel := context.WithTimeout(context.Background(), hourlyReviewTimeout)
	defer cancel()
	resp, err := client.Generate(ctx, llm.ChatRequest{Messages: []llm.Message{{
		Role:    llm.RoleUser,
		Content: digest + "\n\n" + hourlyReviewPrompt(existing, a.promptStore.Get("hourlyReview")),
	}}})
	if err != nil {
		// 复盘是尽力而为的后台增强：失败不炸调度链，日志 + 会话内可见提示。
		slog.Warn("每小时复盘调用失败（跳过）", "session", sessionID, "err", err)
		a.pushHourlyReviewResult(sessionID, "每小时复盘调用失败："+err.Error())
		return
	}
	out := parseHourlyReviewOutput(resp.Content)

	// 产出双落点：完整报告 → 哨兵会话（回放查看）；经验卡 → evolveStore
	// （全局回路：下次任务命中关键词注入 <self_evolution>）。
	saved := 0
	for i, rc := range out.Cards {
		if i >= hourlyReviewMaxCards {
			break
		}
		if strings.TrimSpace(rc.Title) == "" || strings.TrimSpace(rc.Body) == "" {
			continue
		}
		if _, err := a.evolveStore.Add(evolve.Card{
			Title:    rc.Title,
			Body:     rc.Body,
			Keywords: rc.Keywords,
			Source:   "hourlyreview:" + sessionID,
		}); err == nil {
			saved++
		}
	}
	a.pushHourlyReviewResult(sessionID, renderHourlyReviewReport(out, saved))
	if saved > 0 || len(out.Struggles) > 0 {
		a.pushToast(fmt.Sprintf("🧠 每小时复盘：%d 个困难点，沉淀 %d 条改进建议", len(out.Struggles), saved))
	}
}

// renderHourlyReviewReport 复盘报告（落哨兵会话的全文）。
func renderHourlyReviewReport(out hourlyReviewOutput, saved int) string {
	var b strings.Builder
	b.WriteString("🧠 每小时会话复盘（系统级，全景）\n")
	if len(out.Struggles) > 0 {
		b.WriteString("\n【遇到的困难】\n")
		for _, s := range out.Struggles {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(s))
		}
	} else {
		b.WriteString("\n【遇到的困难】未发现明显困难。\n")
	}
	if len(out.Improvements) > 0 {
		b.WriteString("\n【改进建议】\n")
		for _, im := range out.Improvements {
			kind := strings.TrimSpace(im.Kind)
			if kind == "" {
				kind = "改进"
			}
			fmt.Fprintf(&b, "- [%s] %s\n", kind, strings.TrimSpace(im.Title))
		}
	}
	if saved > 0 {
		fmt.Fprintf(&b, "\n已沉淀 %d 条改进建议为经验卡（下次任务命中关键词自动注入）。\n", saved)
	} else if len(out.Cards) == 0 {
		b.WriteString("\n本次无可沉淀的新改进点。\n")
	}
	return b.String()
}

// pushHourlyReviewResult 复盘报告落哨兵会话（不存在则创建）+ toast 轻提示。
// 哨兵会话不进引擎（无 say 桥并发），此处直接落历史安全。
func (a *App) pushHourlyReviewResult(sessionID, text string) {
	s, err := a.store.Get(sessionID)
	if err != nil || s == nil {
		now := time.Now()
		s = &session.Session{
			ID:        sessionID,
			ChannelID: "desktop",
			Title:     "每小时会话复盘",
			Mode:      "ask",
			CreatedAt: now,
			UpdatedAt: now,
		}
	}
	s.Append(message.NewSay(message.SayText, text))
	a.store.Save(s)
	a.pushToast("🧠 每小时复盘：" + firstLine(text))
}

// ---------- 会话聚合 ----------

// buildHourlyDigest 聚合最近窗口内有活动的全部会话为紧凑复盘素材。
// 每会话四类信号（对齐"看这个会话遇到哪些困难"）：
//   - 标题 + 首条用户任务（卡什么需求）；
//   - 错误消息（前 2 条 + 总数：失败模式的直接信号）；
//   - 未完成待办（停滞任务的卡因）；
//   - 工具调用频次 top5（同一工具反复调用 = 工具能力不足或参数设计差）。
func (a *App) buildHourlyDigest() string {
	summaries := a.store.List()
	if len(summaries) == 0 {
		return ""
	}
	cutoff := time.Now().Add(-hourlyReviewWindow)
	picked := make([]session.Summary, 0, len(summaries))
	for _, s := range summaries {
		if strings.HasPrefix(s.ID, "cron_") {
			continue // 定时任务会话（含哨兵会话自身）不入素材，防自我复盘滚雪球
		}
		up := s.UpdatedAt
		if up.IsZero() {
			up = s.CreatedAt
		}
		if up.After(cutoff) {
			picked = append(picked, s)
		}
	}
	if len(picked) == 0 {
		return ""
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].UpdatedAt.After(picked[j].UpdatedAt) })
	if len(picked) > hourlyReviewMaxSessions {
		picked = picked[:hourlyReviewMaxSessions]
	}

	var b strings.Builder
	b.WriteString("---- 近期会话活动聚合（每小时复盘素材） ----\n")
	for _, sum := range picked {
		sessData, err := a.store.Get(sum.ID)
		if err != nil || sessData == nil {
			continue
		}
		snap := sessData.Snapshot()
		fmt.Fprintf(&b, "\n### 会话「%s」（%s，%d 条消息，更新于 %s）\n",
			sum.Title, snap.Workspace, len(snap.Messages), sum.UpdatedAt.Format("01-02 15:04"))
		// 未完成待办：停滞任务的直接信号（最多列 3 项）。
		pending := 0
		for _, it := range snap.Todos {
			if it.Status != "completed" {
				pending++
			}
		}
		if pending > 0 {
			fmt.Fprintf(&b, "未完成待办 %d 项：", pending)
			shown := 0
			for _, it := range snap.Todos {
				if it.Status == "completed" {
					continue
				}
				fmt.Fprintf(&b, "%s；", selfReviewTruncate(selfReviewOneLine(it.Content), 80))
				shown++
				if shown >= 3 {
					break
				}
			}
			b.WriteString("\n")
		}
		// 消息流信号：任务 + 错误 + 工具统计 + 用户重复提交检测
		// （partial 中间帧跳过）。
		toolStats := map[string]int{}
		errCount := 0
		userSaid := ""
		var userMsgs []message.ClineMessage
		for _, m := range snap.Messages {
			if m.Partial || m.Type != message.TypeSay {
				continue
			}
			switch m.Say {
			case message.SayUserFeedback:
				userMsgs = append(userMsgs, m)
				if userSaid == "" {
					userSaid = selfReviewTruncate(selfReviewOneLine(m.Text), 200)
				}
			case message.SayError:
				errCount++
				if errCount <= 2 {
					fmt.Fprintf(&b, "错误: %s\n", selfReviewTruncate(selfReviewOneLine(m.Text), 200))
				}
			case message.SayTool:
				var info message.ToolCallInfo
				if json.Unmarshal([]byte(m.Text), &info) == nil && info.Tool != "" {
					toolStats[info.Tool]++
				}
			}
		}
		if userSaid != "" {
			fmt.Fprintf(&b, "任务: %s\n", userSaid)
		}
		if errCount > 2 {
			fmt.Fprintf(&b, "错误共 %d 条（上方仅列前 2 条）\n", errCount)
		}
		if len(toolStats) > 0 {
			fmt.Fprintf(&b, "工具调用: %s\n", topToolStats(toolStats, 5))
		}
		// 卡点信号：用户 3 分钟内重复提交 ≥2 次（看不到进展才会反复重发）。
		if rep := detectUserRepeat(userMsgs, hourlyReviewRepeatWindow, hourlyReviewRepeatMaxMsgs); rep.Count >= 2 {
			text := selfReviewTruncate(selfReviewOneLine(rep.Text), 80)
			fmt.Fprintf(&b, "疑似卡点：用户重复提交 %d 次（%s）——当前卡点可能是「%s」，先定位阻塞。\n",
				rep.Count, text, text)
		}
	}
	out := b.String()
	if r := []rune(out); len(r) > hourlyReviewMaxDigest {
		out = string(r[len(r)-hourlyReviewMaxDigest:])
	}
	return out
}

// topToolStats 工具调用频次 top n（"工具×次数"逗号连接）。
func topToolStats(stats map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	arr := make([]kv, 0, len(stats))
	for k, v := range stats {
		arr = append(arr, kv{k, v})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].v > arr[j].v })
	parts := make([]string, 0, n)
	for i, x := range arr {
		if i >= n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s×%d", x.k, x.v))
	}
	return strings.Join(parts, ", ")
}

// ---------- 用户重复提交检测 ----------

// hourlyReviewRepeatWindow 用户重复提交检测时间窗：3 分钟内同一文本（或规范化后
// 相同）出现 ≥2 次即视为「卡点信号」——用户看不到进展才会反复重发同一诉求。
const hourlyReviewRepeatWindow = 3 * time.Minute

// hourlyReviewRepeatMaxMsgs 参与重复检测的最新用户消息条数（卡点发生在近期，只
// 取尾部 N 条足够；检测成本 O(N²)，N=10 可忽略）。
const hourlyReviewRepeatMaxMsgs = 10

// userRepeatInfo 用户重复提交检测结果（零值 = 无信号）。
type userRepeatInfo struct {
	Text  string // 重复的最近一条用户消息原文（截断展示用）
	Count int    // 时间窗内该文本出现次数（>=2 为命中）
}

// normalizeUserText 用户消息规范化：小写 + 去空白 + 去标点（含全角），用于
// 「去掉标点/空格后相同」的近似比较。刻意不做编辑距离/模糊匹配——宁可漏报不可误报，
// 避免把正常追问当卡点；纯标点/空白消息规范化后为空，不参与判定。
func normalizeUserText(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// detectUserRepeat 扫描会话消息，检测最新 maxN 条用户反馈（say/user_feedback、
// 非 partial、带时间戳）中，时间窗 window 内相同/规范化后相同文本出现 ≥2 次的
// 卡点信号。纯读不改消息；无信号返回零值。
func detectUserRepeat(msgs []message.ClineMessage, window time.Duration, maxN int) userRepeatInfo {
	var users []message.ClineMessage
	for _, m := range msgs {
		if m.Partial || m.Type != message.TypeSay || m.Say != message.SayUserFeedback {
			continue
		}
		if m.TS <= 0 {
			continue // 无时间戳无法判定窗口，跳过（宁可漏报）
		}
		users = append(users, m)
	}
	if len(users) < 2 {
		return userRepeatInfo{}
	}
	// 时间升序，取最新 maxN 条参与判定（卡点发生在近期）。
	sort.Slice(users, func(i, j int) bool { return users[i].TS < users[j].TS })
	if maxN > 0 && len(users) > maxN {
		users = users[len(users)-maxN:]
	}
	// 规范化后文本相同归同组；组内按时间比较相邻间隔。
	type occurrence struct {
		ts   int64
		text string
	}
	groups := map[string][]occurrence{}
	for _, m := range users {
		norm := normalizeUserText(m.Text)
		if norm == "" {
			continue
		}
		groups[norm] = append(groups[norm], occurrence{ts: m.TS, text: m.Text})
	}
	best := userRepeatInfo{}
	for _, occs := range groups {
		sort.Slice(occs, func(i, j int) bool { return occs[i].ts < occs[j].ts })
		// 相邻时间差 <= window 即存在窗口内重复：若任两消息落在窗口内，
		// 其间必有一对相邻间隔 <= window（相邻检查等价且更简单）。
		for i := 1; i < len(occs); i++ {
			if time.Duration(occs[i].ts-occs[i-1].ts) > window {
				continue
			}
			// 以 occs[i] 为窗口右端点回溯统计窗口内条数。
			cnt := 1
			for j := i - 1; j >= 0 && time.Duration(occs[i].ts-occs[j].ts) <= window; j-- {
				cnt++
			}
			if cnt > best.Count {
				best = userRepeatInfo{Text: occs[i].text, Count: cnt}
			}
		}
	}
	return best
}

// ---------- 提示词与协议 ----------

// hourlyReviewPrompt 每小时复盘提示词：找困难 → 归因 → 给可落地的改进建议 →
// 沉淀经验卡。与 selfReviewPrompt 的差异：面向多会话全景，重点是跨会话重复
// 出现的系统性缺口（= 产品/工具层面问题），并强制区分"改代码能解决"与
// "用户配置问题"（后者不沉淀，防固化有害约束）。
// userPrompt 为用户配置覆盖（promptconfig；空 = 内置默认）。
func hourlyReviewPrompt(existing []string, userPrompt string) string {
	var b strings.Builder
	if s := strings.TrimSpace(userPrompt); s != "" {
		b.WriteString(s + "\n")
	} else {
		b.WriteString(`你是 Kaelvoris 桌面 Agent 的每小时复盘器。上面聚合了近期全部会话的活动信号（任务、错误、未完成待办、工具调用统计）。请系统性复盘：

【第一步：找困难】逐会话识别"卡在哪里"：
- 错误消息反映的失败模式（环境/工具/流程/权限哪一类）；
- 未完成待办与停滞任务的卡因（缺工具？缺上下文？提示词指引不清？）；
- 同一工具高频反复调用（工具能力不足或参数/返回设计差的信号）；
- 跨会话重复出现的同类困难（= 系统性缺口，优先级最高）。

【第二步：给改进建议】每个值得解决的困难给一条建议，kind 四选一：
- promptFix：改内置提示词/工具描述即可解决；
- toolFix：改现有工具代码可解决（注明工具名与缺陷）；
- newTool：需要新增工具/功能（注明工具名、参数、能力）；
- config：用户配置问题（如何配置即可，不需要改代码——此类不要沉淀成卡）。

【第三步：沉淀经验卡 cards】仅对 promptFix/toolFix/newTool 类建议各沉淀一张卡（最多 3 张，选影响最大的）：
- title：改进点一句话（不超过 40 字；与既有卡同名 = 更新该卡）；
- body：困难现象 + 根因 + 具体改进方案（改哪个文件/工具/提示词、期望行为），不超过 600 字符；
- keywords：2-5 个触发关键词（下次同类任务命中即注入该经验）。

不沉淀（会固化成有害约束或无效卡）：环境性失败（缺依赖/未配凭据）、对工具的负面断言（只记改进方案本身）、一次性任务叙事、config 类问题。确实无困难就给空数组。

输出协议：仅输出一个 JSON 对象，不要任何解释、注释或代码围栏。
{"struggles": ["困难点一句话", ...], "improvements": [{"kind": "promptFix|toolFix|newTool|config", "title": "建议一句话"}], "cards": [{"title": "改进点", "body": "现象+根因+方案", "keywords": ["触发词"]}]}
	无困难时：{"struggles": [], "improvements": [], "cards": []}。
	`)
	}
	if len(existing) > 0 {
		b.WriteString("\n既有经验卡标题（同名输出 = 更新该卡正文；勿重复沉淀已覆盖内容）：\n")
		for _, t := range existing {
			fmt.Fprintf(&b, "- %s\n", t)
		}
	}
	return b.String()
}

// hourlyReviewImprovement 一条改进建议（报告展示用）。
type hourlyReviewImprovement struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// hourlyReviewCard 复盘产出的经验卡（JSON 协议，与 selfReviewCard 同构独立声明）。
type hourlyReviewCard struct {
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Keywords []string `json:"keywords"`
}

// hourlyReviewOutput 复盘产出：困难清单 + 改进建议 + 经验卡。
type hourlyReviewOutput struct {
	Struggles    []string                  `json:"struggles"`
	Improvements []hourlyReviewImprovement `json:"improvements"`
	Cards        []hourlyReviewCard        `json:"cards"`
}

// parseHourlyReviewOutput 解析模型输出（容错：截取首个 { 到最后一个 }）。
func parseHourlyReviewOutput(s string) hourlyReviewOutput {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			var out hourlyReviewOutput
			if err := json.Unmarshal([]byte(s[i:j+1]), &out); err == nil {
				return out
			}
		}
	}
	return hourlyReviewOutput{}
}
