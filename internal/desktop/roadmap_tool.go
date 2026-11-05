// roadmap_tool.go —— 自主迭代规划工具（内部 MCP 风格，对齐 EvolveTool/CronTool 形态）。
//
// Agent 在任务中（尤其"规划/复盘/自检"类任务）调用 roadmap_propose 工具把
// 新功能想法沉淀为规划提案：
//   - list     查看规划板全部条目（含状态统计）
//   - propose  新增提案（title 必填；detail/priority/category 可选）→ 落 proposed
//   - status   查看单条条目详情
//
// 提案默认落 proposed（待审核）：用户在规划板面板勾选后点「提交开发」才进入
// 开发队列（默认审核制）；autoDev 开启时由系统自动批准并逐条开发。
// 开发执行链复用 SendMessage（roadmap_<itemID> 固定会话），见 roadmap_panel.go。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kaelvoris/internal/roadmap"
)

// roadmapToolArgs roadmap_propose 工具入参。
type roadmapToolArgs struct {
	Action   string `json:"action"`
	ID       string `json:"id"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Priority string `json:"priority"`
	Category string `json:"category"`
}

// RoadmapProposeTool 规划提案工具（实现 tools.Tool 接口）。
type RoadmapProposeTool struct {
	Store *roadmap.Store
	// DefaultWorkspace 提案默认工作区（注册处传入当前会话工作区）。
	DefaultWorkspace string
	// DefaultSource 提案默认来源标记（session:<id>）。
	DefaultSource string
	// Push 面板推送钩子（提案落库后刷新前端规划板）。
	Push func()
}

func (t *RoadmapProposeTool) Name() string { return "roadmap_propose" }

func (t *RoadmapProposeTool) Description() string {
	return "Propose roadmap items for autonomous iteration (自主迭代规划板). Actions: " +
		"list (view all items with id/title/status/priority), propose (add a new " +
		"proposal: title required, detail/priority/category optional; lands in " +
		"proposed status for user review), status (view one item by id). Use when " +
		"you discover worthwhile improvements during work: new features, bugs, " +
		"refactors, docs, tooling. Keep each item small enough to deliver in one " +
		"coding session. Do NOT propose environment-specific fixes or one-off task " +
		"narratives. Call list first to avoid duplicates."
}

func (t *RoadmapProposeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "Operation on roadmap items",
				"enum":        []string{"list", "propose", "status"},
			},
			"id":       map[string]interface{}{"type": "string", "description": "Item id (required for status; from list)"},
			"title":    map[string]interface{}{"type": "string", "description": "One-line feature point (required for propose, <=40 chars)"},
			"detail":   map[string]interface{}{"type": "string", "description": "Implementation notes: involved modules + acceptance criteria (<=300 chars)"},
			"priority": map[string]interface{}{"type": "string", "enum": []string{"high", "medium", "low"}, "description": "Priority (default medium)"},
			"category": map[string]interface{}{"type": "string", "enum": []string{"feature", "bug", "refactor", "docs", "tooling"}, "description": "Category (default feature)"},
		},
		"required": []string{"action"},
	}
}

func (t *RoadmapProposeTool) Execute(ctx context.Context, args string) (string, error) {
	var p roadmapToolArgs
	if strings.TrimSpace(args) == "" {
		p.Action = "list"
	} else if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(p.Action)) {
	case "list", "":
		return t.list()
	case "propose":
		return t.propose(p)
	case "status":
		return t.status(p.ID)
	default:
		return "", fmt.Errorf("unknown action %q (list/propose/status)", p.Action)
	}
}

// list 全部条目概览（含状态统计）。
func (t *RoadmapProposeTool) list() (string, error) {
	items := t.Store.List()
	stats := t.Store.Count()
	var b strings.Builder
	fmt.Fprintf(&b, "自主迭代规划板共 %d 条（proposed=%d approved=%d developing=%d done=%d failed=%d rejected=%d）：\n",
		stats["total"], stats[roadmap.StatusProposed], stats[roadmap.StatusApproved],
		stats[roadmap.StatusDeveloping], stats[roadmap.StatusDone], stats[roadmap.StatusFailed], stats[roadmap.StatusRejected])
	statusName := map[string]string{
		roadmap.StatusProposed: "待审核", roadmap.StatusApproved: "已批准",
		roadmap.StatusDeveloping: "开发中", roadmap.StatusDone: "已完成",
		roadmap.StatusFailed: "失败", roadmap.StatusRejected: "已拒绝",
	}
	for i, it := range items {
		fmt.Fprintf(&b, "\n%d. [%s] %s（id: %s）\n", i+1, statusName[it.Status], it.Title, it.ID)
		if it.Detail != "" {
			fmt.Fprintf(&b, "   要点: %s\n", it.Detail)
		}
	}
	if len(items) == 0 {
		b.WriteString("（规划板为空：发现值得做的改进时用 propose 提案）")
	}
	return b.String(), nil
}

// propose 新增提案（默认 proposed 待审核）。
func (t *RoadmapProposeTool) propose(p roadmapToolArgs) (string, error) {
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return "", fmt.Errorf("propose 需要 title（一句话功能点）")
	}
	// 去重提醒：同名/近似标题已存在则拒绝（提示更新既有条目）。
	for _, it := range t.Store.List() {
		if it.Title == title && (it.Status == roadmap.StatusProposed || it.Status == roadmap.StatusApproved) {
			return fmt.Sprintf("已有同主题待办条目「%s」（%s），勿重复提案。", it.Title, it.Status), nil
		}
	}
	it, err := t.Store.Add(roadmap.Item{
		Title: p.Title, Detail: p.Detail,
		Priority: p.Priority, Category: p.Category,
		Status:    roadmap.StatusProposed,
		Source:    t.DefaultSource,
		Workspace: t.DefaultWorkspace,
	})
	if err != nil {
		return "", fmt.Errorf("提案落库失败: %w", err)
	}
	if t.Push != nil {
		t.Push()
	}
	return fmt.Sprintf("提案已入规划板（待审核）：%s\nid: %s\n用户在「自主迭代」面板勾选并提交后即开始开发；autoDev 开启时系统自动批准。", it.Title, it.ID), nil
}

// status 单条详情。
func (t *RoadmapProposeTool) status(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("status 需要 id（先 list 获取）")
	}
	it, ok := t.Store.Get(id)
	if !ok {
		return "", fmt.Errorf("条目不存在: %s", id)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "条目 %s\n标题: %s\n状态: %s\n优先级: %s\n类别: %s\n",
		it.ID, it.Title, it.Status, it.Priority, it.Category)
	if it.Detail != "" {
		fmt.Fprintf(&b, "要点: %s\n", it.Detail)
	}
	if it.Result != "" {
		fmt.Fprintf(&b, "完成摘要: %s\n", it.Result)
	}
	if it.Error != "" {
		fmt.Fprintf(&b, "失败原因: %s\n", it.Error)
	}
	if it.SessionID != "" {
		fmt.Fprintf(&b, "开发会话: %s\n", it.SessionID)
	}
	return b.String(), nil
}
