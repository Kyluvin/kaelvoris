// evolve_tools.go —— 自进化闭环的聊天工具（内部 MCP 风格）。
//
// Agent 在任务中（尤其"定时自检"任务）调用 evolve_record 工具沉淀经验卡：
//   - list   查看全部经验卡（id/标题/关键词/命中数）
//   - add    新增/更新（title + body 必填；keywords 可选触发词）
//   - delete 按 id 删除
//
// 经验卡经 App.evolveSnippet 在后续任务组装提示词时按关键词命中注入
// （<self_evolution> 段），实现"定时复盘 → 沉淀 → 增强编码能力"闭环。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kaelvoris/internal/evolve"
)

// evolveToolArgs evolve_record 工具入参。
type evolveToolArgs struct {
	Action   string   `json:"action"`
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Keywords []string `json:"keywords"`
	Source   string   `json:"source"`
}

// EvolveTool 自进化经验卡工具（实现 tools.Tool 接口）。
type EvolveTool struct {
	Store *evolve.Store
	// DefaultSource 新增卡未指定来源时的默认值（注册处传入会话标识）。
	DefaultSource string
}

func (t *EvolveTool) Name() string { return "evolve_record" }

func (t *EvolveTool) Description() string {
	return "Record self-evolution lessons (自进化经验卡). Actions: list (view all " +
		"cards with id/title/keywords/hits), add (create or update by same title: " +
		"title + body required, keywords optional trigger words), delete (remove by id). " +
		"Use after task retrospectives: distill repeatable coding rules, pitfalls, " +
		"project conventions into short cards (body <= 600 chars). Cards matching the " +
		"next task text are injected into the system prompt automatically, making " +
		"future coding stronger. Call list first before delete. " +
		"Memory rules (CCGS agent-memory 对齐): prefer recording a METHOD over a state — " +
		"'confirm X exists before doing Y; return BLOCKED naming what is missing' is durable, " +
		"'there is no X' is a timestamp that will mislead future sessions. If you must record " +
		"a state, state what invalidates it on the same line (INVALIDATED WHEN: ...). " +
		"Verify a recalled card's file paths/counts before acting on it."
}

func (t *EvolveTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "Operation on evolution cards",
				"enum":        []string{"list", "add", "delete"},
			},
			"id":       map[string]interface{}{"type": "string", "description": "Card id (required for delete; from list)"},
			"title":    map[string]interface{}{"type": "string", "description": "One-line rule (required for add; same title = update)"},
			"body":     map[string]interface{}{"type": "string", "description": "Lesson content, <= 600 chars (required for add)"},
			"keywords": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Trigger keywords (task text containing any keyword injects this card; empty = always inject)"},
			"source":   map[string]interface{}{"type": "string", "description": "Origin marker (optional; defaults to current session)"},
		},
		"required": []string{"action"},
	}
}

func (t *EvolveTool) Execute(ctx context.Context, args string) (string, error) {
	var p evolveToolArgs
	if strings.TrimSpace(args) == "" {
		p.Action = "list"
	} else if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(p.Action)) {
	case "list", "":
		return t.list()
	case "add":
		return t.add(p)
	case "delete":
		return t.del(p.ID)
	default:
		return "", fmt.Errorf("unknown action %q (list/add/delete)", p.Action)
	}
}

// list 全部经验卡概览。
func (t *EvolveTool) list() (string, error) {
	cards := t.Store.List()
	if len(cards) == 0 {
		return "自进化经验卡：暂无。复盘后用 add 沉淀（title/body 必填，keywords 可选触发词）。", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "自进化经验卡共 %d 张（id 用于 delete）：\n", len(cards))
	for i, c := range cards {
		fmt.Fprintf(&b, "\n%d. %s（id: %s，命中 %d 次）\n", i+1, c.Title, c.ID, c.Hits)
		if len(c.Keywords) > 0 {
			fmt.Fprintf(&b, "   关键词: %s\n", strings.Join(c.Keywords, ", "))
		}
		fmt.Fprintf(&b, "   内容: %s\n", c.Body)
	}
	return b.String(), nil
}

// add 新增/更新经验卡（同标题合并）。
func (t *EvolveTool) add(p evolveToolArgs) (string, error) {
	src := strings.TrimSpace(p.Source)
	if src == "" {
		src = t.DefaultSource
	}
	c, err := t.Store.Add(evolve.Card{
		Title:    p.Title,
		Body:     p.Body,
		Keywords: p.Keywords,
		Source:   src,
	})
	if err != nil {
		return "", fmt.Errorf("沉淀失败: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "经验卡已沉淀：%s\n内容: %s\n", c.Title, c.Body)
	if len(c.Keywords) > 0 {
		fmt.Fprintf(&b, "关键词: %s（后续任务文本命中即自动注入）\n", strings.Join(c.Keywords, ", "))
	} else {
		b.WriteString("关键词: 无（常驻卡，所有任务注入）\n")
	}
	// G2 临时状态提醒（CCGS agent-memory 规则）：不把临时缺席记成持久事实。
	// 写入不拦截（状态卡也有价值），但命中时提示改写为方法型或补失效条件。
	if evolve.IsTransientState(c.Body) {
		b.WriteString("\n\n⚠️ 该卡疑似「临时状态型断言」（如“没有 X/不存在 X”）：这类观察必然过期，两周后可能误导后续会话对已建成事实的判断。建议：优先记录方法而非状态（可执行步骤+判据）；若必须记状态，同句写失效条件（如 “INVALIDATED WHEN: <什么变化后此卡不再成立>”），或删除重写。")
	}
	b.WriteString("\n后续任务组装提示词时按命中注入 <self_evolution> 段，编码能力持续增强。")
	return b.String(), nil
}

// del 删除经验卡。
func (t *EvolveTool) del(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("delete 需要 id（先 list 获取）")
	}
	if err := t.Store.Delete(id); err != nil {
		return "", err
	}
	return fmt.Sprintf("已删除经验卡（id: %s）。", id), nil
}
