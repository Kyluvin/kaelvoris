// 迭代节点（iteration）执行器 —— 2026-10 Dify iteration / Coze loop 对齐。
//
// Dify 对应节点：api/core/workflow/nodes/iteration（对数组逐项跑子流程，
// 每项输出聚合为数组）。Kaelvoris 语义（Step 复用，零新增结构）：
//   - Step.Template 迭代定义 JSON：
//     {"list": "{var 或 {nodeID.output}}", "itemVar": "item", "steps": [...Step]}
//     list 渲染后按行/JSON 数组切分为元素数组；steps 为子步骤线性链。
//   - 每个子项构造独立 state（继承用户变量 + 迭代项写入 itemVar），顺序执行
//     子步骤（复用 execStepNode），最后一项输出收集为结果数组。
//   - 输出写回变量池：{nodeID.output} = 子步骤输出按 "\\n" 拼接（模板可直引），
//     {nodeID.items} = JSON 数组（transform/下游模板可再加工）。
//   - 子步骤轨迹以 trace 回调透出（Index 用全局递增序，Name 加 [i] 前缀）。
//   - 上限：单次迭代最多 100 项 × 子步骤内不再嵌套迭代（防失控，Dify 同有上限）。
package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// iterDef 迭代定义（Step.Template 反序列化目标）。
type iterDef struct {
	List    string `json:"list"`
	ItemVar string `json:"itemVar"`
	Steps   []Step `json:"steps"`
}

// execIteration 迭代节点执行：切分数组 → 逐项跑子步骤 → 聚合输出。
func (r *Runner) execIteration(ctx context.Context, st *state, step Step, idx int, dryRun bool) (string, []string, error) {
	def := iterDef{ItemVar: "item"}
	if s := strings.TrimSpace(step.Template); s != "" {
		if err := json.Unmarshal([]byte(s), &def); err != nil {
			return "", nil, fmt.Errorf("迭代定义 JSON 解析失败: %w", err)
		}
	}
	if strings.TrimSpace(def.List) == "" {
		return "", nil, fmt.Errorf("迭代节点未配置列表表达式（list）")
	}
		if len(def.Steps) == 0 {
			return "", nil, fmt.Errorf("迭代节点未配置子步骤（steps）")
		}
		// 嵌套迭代拦截（Dify iteration 同语义：迭代体内不允许再嵌迭代，防失控）。
		for _, ss := range def.Steps {
			if ss.Type == StepIteration {
				return "", nil, fmt.Errorf("迭代子步骤不允许嵌套 iteration 节点（如需多层循环请展开为多节点或用 tools 节点）")
			}
		}
	// 列表表达式渲染后切分：优先 JSON 数组，退化为按行切分。
	listExpr := def.List
	if strings.Contains(listExpr, "{") {
		if out, err := renderVars(ctx, listExpr, st.mergedVars()); err == nil {
			listExpr = out
		}
	}
	items := splitIterList(listExpr)
	if len(items) == 0 {
		return "", nil, fmt.Errorf("迭代列表为空（list 渲染结果 %.80s）", listExpr)
	}
	const iterMaxItems = 100 // ponytail: 单次迭代上限 100 项，超大列表需要时再分批
	if len(items) > iterMaxItems {
		return "", nil, fmt.Errorf("迭代列表 %d 项超出上限 %d", len(items), iterMaxItems)
	}

	itemVar := strings.TrimSpace(def.ItemVar)
	if itemVar == "" {
		itemVar = "item"
	}
	outs := make([]string, 0, len(items))
	gidx := idx
	for i, item := range items {
		// 子项独立状态：继承用户变量与池快照（子步骤 {nodeID.output} 引用上游）
		// + 迭代项写入 itemVar（子步骤 {item} 引用当前项）。
		sub := &state{vars: map[string]any{}, pools: map[string]map[string]any{}}
		for k, v := range st.vars {
			sub.vars[k] = v
		}
		for k, v := range st.pools {
			sub.pools[k] = v
		}
		sub.vars[itemVar] = item
		sub.msgs = append(sub.msgs, st.msgs...)
		var lastOut string
		for j, ss := range def.Steps {
			s2 := ss
			if s2.Name == "" {
				s2.Name = fmt.Sprintf("%s[%d]#%d", step.Name, i+1, j+1)
			} else {
				s2.Name = fmt.Sprintf("%s[%d]", s2.Name, i+1)
			}
			if !dryRun || s2.Type == StepTemplate || s2.Type == StepTransform {
				if err := r.execStepNode(ctx, sub, s2, gidx, dryRun); err != nil {
					return "", nil, fmt.Errorf("迭代第 %d 项子步骤 %s 失败: %w", i+1, s2.Name, err)
				}
			} else {
				tr := StepTrace{Index: gidx, Type: s2.Type, Name: s2.Name, Render: "(dry-run 跳过)"}
				sub.steps = append(sub.steps, tr)
				r.trace(tr)
			}
			gidx++
		}
		// 子项子轨迹并入主轨迹（前端可见每项每步执行过程）；追加帧数累计
		//（主循环据此推进 idx，保持轨迹 Index 单调）。
		st.pendingAppended += len(sub.steps)
		st.steps = append(st.steps, sub.steps...)
		// 子项最终输出：取最后一步 Render。
		if k := len(sub.steps); k > 0 {
			lastOut = sub.steps[k-1].Render
		}
		outs = append(outs, lastOut)
	}
	return strings.Join(outs, "\n"), outs, nil
}

	// splitIterList 列表切分：JSON 数组（字符串/数值元素）优先，否则按行去空。
func splitIterList(s string) []string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		var arr []any
		if err := json.Unmarshal([]byte(s), &arr); err == nil {
			out := make([]string, 0, len(arr))
			for _, v := range arr {
				out = append(out, strings.TrimSpace(fmt.Sprint(v)))
			}
			return out
		}
	}
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}
