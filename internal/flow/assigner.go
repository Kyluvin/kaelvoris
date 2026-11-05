// 变量赋值节点（assigner）执行器 —— 2026-10 Dify variable-assigner/variable-
// aggregator 语义对齐。
//
// Dify 对应节点：
//   - variable-assigner：把变量写入会话变量（写回变量池）；
//   - variable-aggregator：多分支输出聚合为同名变量（Kaelvoris 分支互斥执行，
//     天然只会有一个分支上游产出，聚合语义 = 同名覆盖写，无需多输入合并）。
//
// Kaelvoris 语义（Step 复用）：
//   - Step.Name    目标变量名（用户变量，如 "result"；点分路径无效，写池顶层）
//   - Step.Template 值表达式：{var} / {nodeID.output} / 字面量（渲染后写入）
//
// 输出写回：{nodeID.output} = 所赋值（供链式引用），并同步写入 st.vars 顶层
// （下游 {var} 直接引用目标变量名）。
package flow

import (
	"context"
	"fmt"
	"strings"
)

// execAssigner assigner 节点执行：渲染值表达式 → 写入用户变量 + 节点输出池。
func (r *Runner) execAssigner(ctx context.Context, st *state, step Step) (string, error) {
	name := strings.TrimSpace(step.Name)
	if name == "" || strings.Contains(name, ".") {
		return "", fmt.Errorf("变量赋值节点未配置合法目标变量名（Step.Name）")
	}
	if strings.TrimSpace(step.Template) == "" {
		return "", fmt.Errorf("变量赋值节点未配置值表达式（Step.Template）")
	}
	mvars := st.mergedVars()
	val := step.Template
	if strings.Contains(val, "{") {
		if out, err := renderVars(ctx, val, mvars); err == nil {
			val = out
		}
	}
	// 写回：目标变量进用户变量表（下游 {name} 引用）+ 本节点输出池（{nodeID.output} 链式引用）。
	if st.vars == nil {
		st.vars = map[string]any{}
	}
	st.vars[name] = val
	st.putPool("__assigner_"+name, map[string]any{"output": val})
	return val, nil
}
