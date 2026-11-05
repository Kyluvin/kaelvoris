package desktop

// approval_panel.go 工具审批矩阵 Wails 绑定（对标增强 C2，doc/10-对标增强 第二批）。
// 前端（设置抽屉）经 ListApprovals 读取矩阵、SetApproval 增改删规则；
// 持久化于 ~/.kaelvoris/approvals.json（internal/approval.Load/Save）。
// 本批仅落内核机制 + Wails 绑定，前端接入后续批次。

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"

	"kaelvoris/internal/approval"
)

// ListApprovals 返回当前审批矩阵（工具名 → ask/allow/deny；空 = 无规则）。
func (a *App) ListApprovals() map[string]string {
	m := approval.Load(approvalsPath())
	if len(m) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = string(v)
	}
	return out
}

// SetApproval 设置/删除审批规则：action ∈ ask|allow|deny（大小写不敏感），
// 空串 = 删除该工具规则；合并写盘（Save 内部先读盘合并，多窗口安全）。
func (a *App) SetApproval(tool, action string) error {
	if tool == "" {
		return fmt.Errorf("approval: empty tool name")
	}
	m := approval.Load(approvalsPath())
	if m == nil {
		m = approval.Matrix{}
	}
	if action == "" {
		delete(m, tool)
	} else {
		act, err := approval.String(action)
		if err != nil {
			return err
		}
		m[tool] = act
	}
	return approval.Save(approvalsPath(), m)
}

// ---- D2 参数级规则绑定（第三批；前端接入后续批次，对齐 C2 模式）----

// ListApprovalRules 返回参数级规则数组（D2；无规则 = 空数组）。
func (a *App) ListApprovalRules() []approval.Rule {
	s := approval.LoadStore(approvalsPath())
	if s == nil || len(s.Rules) == 0 {
		return []approval.Rule{}
	}
	return s.Rules
}

// SetApprovalRule 新增/更新/删除参数级规则（D2）：id 定位（空 = 自动生成）；
// action 空 = 按 id 删除；pattern 为参数 JSON 原文正则（空 = 不限参数）。
// 非法正则拒绝写入；合并写盘（SaveStore 先读盘合并，多窗口安全）。
func (a *App) SetApprovalRule(id, tool, pattern, action string, priority int) error {
	if tool == "" {
		return fmt.Errorf("approval: empty tool name")
	}
	// 非删除动作必须合法（ask/allow/deny）。
	var act approval.Action
	if action != "" {
		a2, err := approval.String(action)
		if err != nil {
			return err
		}
		act = a2
	}
	// 非法正则拒绝（LoadStore 会静默丢弃，写入侧提前拦截更友好）。
	if pattern != "" {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("approval: invalid pattern: %w", err)
		}
	}
	if id == "" {
		id = newPanelRuleID()
	}
	r := approval.Rule{ID: id, Tool: tool, Pattern: pattern, Action: act, Priority: priority}
	return approval.SaveStore(approvalsPath(), &approval.Store{Rules: []approval.Rule{r}})
}

// newPanelRuleID 规则 ID（随机 hex 16 位；对齐 checkpoint/evolve newID 形态）。
func newPanelRuleID() string {
	b := make([]byte, 8)
	if _, err := crand.Read(b); err != nil {
		return fmt.Sprintf("rule-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
