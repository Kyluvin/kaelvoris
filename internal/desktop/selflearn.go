// selflearn.go —— 任务结束后自动后台复盘（自学习闭环，memory + skill 双路）。
//
// 对齐 kaelvoris-agent（参考/kaelvoris-agent-2026.6.19）agent/background_review.py
// 语义的最小落地（纯 Go，不引外部依赖）：
//   - 参考项目在每轮 turn 结束后 spawn 后台线程，fork 一个继承主运行时的
//     review agent，回放对话并自问"该不该存记忆/更新技能"，写直落存储；
//   - kaelvoris 已有 evolve 经验卡存储（internal/evolve）与关键词命中注入
//     （<self_evolution> 段），本文件补上缺失的"自动触发复盘"环节。
//
// 触发链路：runTask 轮次循环结束（非中止）→ maybeSelfReview 按配置间隔
// 计数 → goroutine 后台执行 selfReviewRun → 对话转录 + 待办快照 + 既有卡
// 标题 + 复盘提示词发给主模型（非流式 Generate，单次调用双路产出）→
//   - cards：经验卡 → evolveStore.Add 落库（同名卡 = 更新正文，即"在使用中
//     改进"；全局级回路：未来任务文本命中关键词时注入 <self_evolution>）；
//   - memorySummary：会话记忆收尾 → 覆盖式更新会话 MEMORY.md 的「任务收尾」
//     节（会话级回路：每次任务前记忆文档注入 SESSION MEMORY 段，下次任务/
//     恢复会话即见——对齐用户需求"记住任务是否实现、卡在哪里，下次接着干"）。
//
// 约束（对齐参考项目 review prompt 的红线）：
//   - 不沉淀环境性失败（缺依赖/未配凭据）与工具负面断言——会固化成有害约束；
//   - 不沉淀一次性任务叙事；单次复盘最多落 3 张卡（防失控）；
//   - 记忆收尾指针式精简、单节覆盖（防 MEMORY.md 膨胀）；
//   - 同一时刻至多 1 个复盘 LLM 调用（selfReviewing 互斥），失败静默跳过。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"kaelvoris/internal/evolve"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// SelfReviewConfig 任务结束自动复盘配置（desktop.json 的 selfReview 字段）。
// nil = 未配置 = 关闭（对齐 Config.Vector 的可选配置族风格）。
type SelfReviewConfig struct {
	Enabled bool `json:"enabled"`
	// EveryN 每 N 轮已完成任务复盘一次（<1 视为 1 = 每轮；对齐参考项目
	// _memory_nudge_interval / _skill_nudge_interval 的"间隔 nudge"语义）。
	EveryN int `json:"everyN,omitempty"`
}

// selfReview 复盘参数上限（防提示词膨胀与失控写入）。
const (
	selfReviewMaxTranscript    = 10000 // 对话转录上限（rune，超出保留尾部——最近的对话价值最高）
	selfReviewMaxCards         = 3     // 单次复盘最多落库卡数
	selfReviewMaxMemorySummary = 600   // 会话记忆收尾单节上限（rune，指针式精简）
	selfReviewTimeout          = 5 * time.Minute
)

// selfReviewConfig 加锁读取自学习配置。
func (a *App) selfReviewConfig() *SelfReviewConfig {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return a.cfg.SelfReview
}

// maybeSelfReview 任务轮次结束后的复盘入口（不阻塞轮次链）。
// 计数达到 EveryN 间隔且无复盘在跑时，快照对话历史与待办清单并后台执行复盘。
func (a *App) maybeSelfReview(sessionID, task string, run *taskRun, taskErr error) {
	sr := a.selfReviewConfig()
	if sr == nil || !sr.Enabled {
		return
	}
	a.selfReviewMu.Lock()
	a.selfReviewCount++
	n := sr.EveryN
	if n < 1 {
		n = 1
	}
	// 间隔未到 / 已有复盘在跑（同一时刻至多一个 LLM 复盘调用）→ 跳过。
	if a.selfReviewCount%n != 0 || a.selfReviewing {
		a.selfReviewMu.Unlock()
		return
	}
	a.selfReviewing = true
	a.selfReviewMu.Unlock()

	// 快照对话历史与待办清单（hist 由 say 桥在任务 goroutine 持续追加，
	// todo 由引擎并发更新，都必须拷贝快照）。
	run.histMu.Lock()
	hist := append([]message.ClineMessage(nil), run.hist...)
	run.histMu.Unlock()
	todos := run.todo.Items()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("自学习复盘 panic（已拦截）", "session", sessionID, "panic", r)
			}
			a.selfReviewMu.Lock()
			a.selfReviewing = false
			a.selfReviewMu.Unlock()
		}()
		a.selfReviewRun(sessionID, task, hist, todos, taskErr)
	}()
}

// selfReviewCard 复盘产出的经验卡（JSON 协议）。
type selfReviewCard struct {
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Keywords []string `json:"keywords"`
}

// selfReviewOutput 复盘双路产出（JSON 协议对象）：经验卡（全局回路）+
// 会话记忆收尾（会话回路）。兼容旧协议：纯数组输出 = 仅经验卡。
type selfReviewOutput struct {
	Cards         []selfReviewCard `json:"cards"`
	MemorySummary string           `json:"memorySummary"`
}

// selfReviewRun 执行一次复盘：转录对话 + 待办快照 → 主模型双路产出 →
// 经验卡落库 evolveStore + 会话记忆收尾落 MEMORY.md「任务收尾」节。
func (a *App) selfReviewRun(sessionID, task string, hist []message.ClineMessage, todos []tools.TodoItem, taskErr error) {
	transcript := buildSelfReviewTranscript(hist, task, todos, taskErr)
	if strings.TrimSpace(transcript) == "" {
		return
	}
	// 既有卡标题：同名输出 = 更新该卡（复用 Store.Add 的同标题合并语义，
	// 对齐参考项目"UPDATE AN EXISTING UMBRELLA 优先于新建"的偏好序）。
	var existing []string
	for _, c := range a.evolveStore.List() {
		existing = append(existing, c.Title)
	}

	// E2 辅助模型分流（对标 Plandex summarizer 角色）：复盘属辅助环节，
	// 配置了辅助模型时走便宜小模型，未配置回落主模型（零破坏）。
	client := a.auxClient(nil)
	ctx, cancel := context.WithTimeout(context.Background(), selfReviewTimeout)
	defer cancel()
	resp, err := client.Generate(ctx, llm.ChatRequest{Messages: []llm.Message{{
		Role:    llm.RoleUser,
		Content: transcript + "\n\n" + selfReviewPrompt(existing, a.promptStore.Get("selfReview")),
	}}})
	if err != nil {
		// 复盘是尽力而为的后台增强：失败静默（不干扰用户），仅留日志。
		slog.Warn("自学习复盘调用失败（跳过）", "session", sessionID, "err", err)
		return
	}
	out := parseSelfReviewOutput(resp.Content)

	// 双路一：经验卡 → evolveStore（全局回路，未来同类任务命中关键词注入）。
	saved := 0
	for i, rc := range out.Cards {
		if i >= selfReviewMaxCards {
			break
		}
		if strings.TrimSpace(rc.Title) == "" || strings.TrimSpace(rc.Body) == "" {
			continue
		}
		if _, err := a.evolveStore.Add(evolve.Card{
			Title:    rc.Title,
			Body:     rc.Body,
			Keywords: rc.Keywords,
			Source:   "selfreview:" + sessionID,
		}); err == nil {
			saved++
		}
	}

	// 双路二：会话记忆收尾 → 覆盖式更新 MEMORY.md「任务收尾」节（会话回路）。
	memoryDone := false
	if sum := strings.TrimSpace(out.MemorySummary); sum != "" {
		if err := a.memory.AppendWrapUp(sessionID, selfReviewClampRunes(sum, selfReviewMaxMemorySummary)); err == nil {
			memoryDone = true
		}
	}

	if saved > 0 && memoryDone {
		a.pushToast(fmt.Sprintf("🧠 自学习：沉淀 %d 张经验卡 + 更新会话记忆收尾", saved))
	} else if saved > 0 {
		a.pushToast(fmt.Sprintf("🧠 自学习：已从会话复盘沉淀 %d 张经验卡", saved))
	} else if memoryDone {
		a.pushToast("🧠 自学习：已更新会话记忆收尾")
	}
}

// buildSelfReviewTranscript 把会话消息历史转成紧凑对话文本（复盘素材）。
// 跳过流式 partial 中间帧与 ask 事件；仅保留对复盘有信号量的四类消息；
// 附待办清单快照（记忆收尾需据此判断"任务是否实现、卡在哪里"）。
func buildSelfReviewTranscript(hist []message.ClineMessage, task string, todos []tools.TodoItem, taskErr error) string {
	var b strings.Builder
	b.WriteString("---- 本会话任务对话转录（复盘素材） ----\n")
	if s := strings.TrimSpace(task); s != "" {
		fmt.Fprintf(&b, "用户: %s\n", selfReviewTruncate(s, 500))
	}
	// 待办清单快照：记忆收尾的进度判断依据。
	if len(todos) > 0 {
		b.WriteString("[任务清单]\n")
		for _, it := range todos {
			mark := "[ ]"
			switch it.Status {
			case "completed":
				mark = "[x]"
			case "in_progress":
				mark = "[-]"
			}
			fmt.Fprintf(&b, "%s %s\n", mark, it.Content)
		}
	}
	for _, m := range hist {
		if m.Partial || m.Type != message.TypeSay {
			continue
		}
		switch m.Say {
		case message.SayUserFeedback:
			fmt.Fprintf(&b, "用户: %s\n", selfReviewTruncate(selfReviewOneLine(m.Text), 500))
		case message.SayText:
			fmt.Fprintf(&b, "助手: %s\n", selfReviewTruncate(selfReviewOneLine(m.Text), 600))
		case message.SayTool:
			var info message.ToolCallInfo
			if json.Unmarshal([]byte(m.Text), &info) == nil {
				label := info.Tool
				if info.Path != "" {
					label += " → " + info.Path
				} else if info.Cmd != "" {
					label += " → " + selfReviewTruncate(info.Cmd, 120)
				}
				fmt.Fprintf(&b, "工具: %s\n", label)
			}
		case message.SayError:
			fmt.Fprintf(&b, "错误: %s\n", selfReviewTruncate(selfReviewOneLine(m.Text), 300))
		}
	}
	if taskErr != nil {
		fmt.Fprintf(&b, "任务结果: 异常结束（%s）\n", selfReviewTruncate(taskErr.Error(), 200))
	}
	out := b.String()
	if r := []rune(out); len(r) > selfReviewMaxTranscript {
		out = string(r[len(r)-selfReviewMaxTranscript:])
	}
	return out
}

// selfReviewPrompt 复盘提示词（对齐参考项目 _SKILL_REVIEW_PROMPT：
// Active 基调 + 信号清单 + 反模式禁令 + JSON 输出协议，适配经验卡模型）。
// userPrompt 为用户配置覆盖（promptconfig；空 = 内置默认）。
func selfReviewPrompt(existing []string, userPrompt string) string {
	var b strings.Builder
	if s := strings.TrimSpace(userPrompt); s != "" {
		b.WriteString(s + "\n")
	} else {
		b.WriteString(`你是主 Agent 的后台复盘器（self-improvement review）。复盘上面的任务对话，产出两路沉淀：

【第一路：经验卡 cards】——可复用的经验（全局回路：未来任务文本命中关键词时自动注入系统提示词，强化同类任务）。

积极复盘——多数任务至少能产出一张卡；"无可沉淀"不该是默认结论。但确实没有就给空数组。

值得沉淀的信号（任一即可）：
- 用户纠正了你的风格、格式、流程或结果（"太啰嗦""不要这样""以后先 X"）——一等信号；
- 出现非平凡的技巧、修复方案、绕过手段、调试路径或工具用法模式；
- 值得固化的工程约定/项目规范（命名、目录结构、验证步骤）；
- 失败的根因与可复用的修复方法（注意：只记录修复方法本身）。

不要沉淀（会固化成有害的自我约束，环境变化后反噬）：
- 环境性失败：缺依赖、未配置凭据、路径不匹配等——用户可修复，不是持久规则；
- 对工具/功能的负面断言（"X 工具坏了"）——问题修复后会误导后续会话；
- 当次任务的叙事性内容（做了什么需求、分析了什么 PR）——一次性；
- 已重试解决的偶发错误——该记的是重试模式，不是原始失败。

【第二路：会话记忆收尾 memorySummary】——本会话任务的进度快照（会话回路：下次任务/恢复会话自动注入，续接上下文）。指针式精简，不超过 600 字符，必须覆盖：
- 任务目标一句话 + 当前状态（已完成 / 进行到哪一步 / 卡在哪里、卡因）；
- 关键产出物路径（文件指针，详情让引擎按需读取，不复制正文）；
- 存在未完成项时：明确"下次接着干"的切入点。
仅记录本会话任务的进度事实，不写经验教训（那是 cards 的事）。

输出协议：仅输出一个 JSON 对象，不要任何解释、注释或代码围栏。
{"cards": [{"title": "一句话规则（同类任务通用，不超过40字；与既有卡同名即更新该卡）", "body": "经验正文，不超过600字符，含步骤/坑点/示例", "keywords": ["触发关键词2-5个，短小具体"]}], "memorySummary": "任务收尾：……（不超过600字符，指针式）"}
	无沉淀时给空数组与空串：{"cards": [], "memorySummary": ""}。
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

// parseSelfReviewOutput 解析模型输出（容错：剥离代码围栏）。
// 新协议 = JSON 对象 {cards, memorySummary}；旧协议兼容 = 纯数组 = 仅经验卡。
func parseSelfReviewOutput(s string) selfReviewOutput {
	s = strings.TrimSpace(s)
	if s == "" {
		return selfReviewOutput{}
	}
	// 旧协议兼容：首个有效 JSON 标记为数组 → 仅经验卡。
	if i := strings.Index(s, "["); i >= 0 && (strings.Index(s, "{") < 0 || i < strings.Index(s, "{")) {
		if j := strings.LastIndex(s, "]"); j > i {
			var cards []selfReviewCard
			if err := json.Unmarshal([]byte(s[i:j+1]), &cards); err == nil {
				return selfReviewOutput{Cards: cards}
			}
		}
	}
	// 新协议：截取首个 { 到最后一个 }。
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			var out selfReviewOutput
			if err := json.Unmarshal([]byte(s[i:j+1]), &out); err == nil {
				return out
			}
		}
	}
	return selfReviewOutput{}
}

// selfReviewClampRunes 按 rune 截断（无省略号，硬上限钳制）。
func selfReviewClampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// selfReviewOneLine 压缩为单行（复盘转录紧凑性）。
func selfReviewOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// selfReviewTruncate 按 rune 截断（超长加省略号）。
func selfReviewTruncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
