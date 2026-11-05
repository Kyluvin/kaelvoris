// hardtrim.go —— 413 防护第三层（最后兜底）：摘要与滑动窗口都无法把估算
// 压入预算时（典型：尾部残留单条超大消息，组粒度隐藏无法移除），按预算
// 比例就地裁剪可见消息的正文/工具参数/图片。自 engine.go 迁入 condense 包
// （策略化治理的组成部分，HardTrimPolicy 调用）。
package condense

import (
	"fmt"
	"unicode/utf8"

	"kaelvoris/internal/llm"
)

// hardTrimVisible 强制裁剪可见消息使其估算落入预算。system 提示永远保留；
// 首条用户任务消息单独裁到预算一半（防止一次性粘贴超大文档让载荷永远超标）。
// 返回是否发生了裁剪。
//
// 收敛性：内容字符预算按「估算 token 比例」折算（token 估算随内容长度
// 近似线性），裁剪后单条消息估算 ≤ 其分配额，整体必然 ≤ 0.9×预算。
func hardTrimVisible(msgs []llm.Message, hidden []bool, budget int) bool {
	if len(msgs) == 0 {
		return false
	}
	if len(hidden) < len(msgs) {
		h := make([]bool, len(msgs))
		copy(h, hidden)
		hidden = h
	}
	target := budget * 9 / 10
	if target < 256 {
		target = 256
	}
	changed := false

	// 首条可见用户消息 = 任务消息。
	firstUser := -1
	for i, m := range msgs {
		if !hidden[i] && m.Role == llm.RoleUser && !IsMarker(m) {
			firstUser = i
			break
		}
	}
	// 任务消息本身超大（如一次性粘贴整份大文档）：裁到预算一半。
	if firstUser >= 0 {
		fuEst := EstimateTokens(msgs[firstUser : firstUser+1])
		if half := budget / 2; fuEst > half {
			charBudget := len(msgs[firstUser].Content) * half / fuEst
			if charBudget < 8 {
				charBudget = 8
			}
			note := fmt.Sprintf("\n[task message trimmed: context over hard budget; showing %d of %d chars]",
				charBudget, len(msgs[firstUser].Content))
			msgs[firstUser].Content = truncBytes(msgs[firstUser].Content, charBudget) + note
			changed = true
		}
	}

	// 统计受保护消息（system/任务/标记）与可裁剪消息的估算 token。
	protectedEst, trimEst := 0, 0
	trimIdx := make([]int, 0, len(msgs))
	for i := range msgs {
		if hidden[i] {
			continue
		}
		if IsMarker(msgs[i]) || i == firstUser || msgs[i].Role == llm.RoleSystem {
			protectedEst += EstimateTokens(msgs[i : i+1])
			continue
		}
		trimIdx = append(trimIdx, i)
		trimEst += EstimateTokens(msgs[i : i+1])
	}
	budgetLeft := target - protectedEst
	if trimEst == 0 || budgetLeft >= trimEst {
		return changed
	}
	k := float64(budgetLeft) / float64(trimEst)
	for _, i := range trimIdx {
		m := &msgs[i]
		msgEst := EstimateTokens([]llm.Message{*m})
		allow := int(float64(msgEst) * k)
		if allow >= msgEst {
			continue
		}
		// 图片：超预算直接丢弃（历史图片信息密度低，估算 800 token/张）。
		if imgCost := 800 * len(m.Images); imgCost > 0 {
			m.Images = nil
			changed = true
		}
		// 正文：token 估算随内容长度近似线性，按比例折算字符预算。
		if cEst := EstimateText(m.Content); cEst > 0 && cEst > allow {
			charBudget := len(m.Content) * allow / cEst
			if charBudget < 8 {
				charBudget = 8
			}
			note := fmt.Sprintf("\n[message trimmed: context over hard budget; showing %d of %d chars]",
				charBudget, len(m.Content))
			m.Content = truncBytes(m.Content, charBudget) + note
			changed = true
		}
		// 工具调用参数：保持 JSON 合法，整体替换为占位对象。
		for j := range m.ToolCalls {
			if m.ToolCalls[j].Arguments == "" {
				continue
			}
			aEst := EstimateText(m.ToolCalls[j].Arguments)
			if aAllow := int(float64(aEst) * k); aEst > 0 && aAllow < aEst {
				m.ToolCalls[j].Arguments = fmt.Sprintf(
					`{"truncated":true,"original_chars":%d}`, len(m.ToolCalls[j].Arguments))
				changed = true
			}
		}
	}
	return changed
}

// truncBytes 按字节上限截断字符串并保证 UTF-8 边界安全（最多回退 3 字节，
// 避免截断在多字节字符中间产生乱码）。与 agent 包同名函数同语义（各自包内
// 私有，避免跨包导出纯工具函数）。
func truncBytes(s string, maxBytes int) string {
	if maxBytes >= len(s) {
		return s
	}
	cut := s[:maxBytes]
	for i := 0; i < 3 && len(cut) > 0; i++ {
		if r, _ := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}
