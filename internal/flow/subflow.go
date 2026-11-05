// 子流程节点（subflow）执行器 —— 2026-10-27 Coze subworkflow / Dify workflow
// 节点对齐（流程复用的正式机制，Coze 对应
// domain/workflow/internal/nodes/subworkflow：Config.WorkflowID 指向子流程，
// 输入映射后执行子图，输出写回）。
//
// Kaelvoris 语义（Step 复用 + Runner.SubFlow 回调注入）：
//   - Step.Template 定义 JSON：
//     {"flow": "<子流程ID>", "vars": {"子流程变量": "{本流程表达式}"}}
//     vars 逐值经 {var}/{nodeID.output} 渲染后作为子流程输入变量。
//   - 执行经 Runner.SubFlow 回调（入参 flowID + 输入变量 + dryRun，返回
//     子流程 RunResult 或错误）；回调为 nil 时报错（与 KB/LLM 同语义）。
//     flow 包不反向依赖宿主流程存储，内核零耦合。
//   - 输出写回变量池：{nodeID.output} = 子流程最终输出；子流程逐步轨迹并入
//     主轨迹（Name 加 "子流程:" 前缀，Index 全局递增，前端可见内部执行过程）。
//   - 防递归：SubFlow 回调由宿主实现自证（加载子流程后经同一 Runner.Start
//     执行，若子流程画布又含 subflow 引用自身，由宿主递归深度上限拦截；
//     本包不做画布级环检测——跨流程环由宿主层级负责，与 Coze 版本化子流程
//     同语义）。
package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// subflowDef 子流程定义（Step.Template 反序列化目标）。
type subflowDef struct {
	Flow string            `json:"flow"`
	Vars map[string]string `json:"vars"`
}

// execSubflow subflow 节点执行：渲染输入映射 → 回调执行子流程 → 输出/轨迹并入。
// 返回（子流程最终输出, 追加帧数, 错误）；追加帧数由调用方并入 pendingAppended
// 推进轨迹 Index（与 iteration 子帧同套路）。
func (r *Runner) execSubflow(ctx context.Context, st *state, step Step, dryRun bool) (string, int, error) {
	def := subflowDef{}
	if s := strings.TrimSpace(step.Template); s != "" {
		if err := json.Unmarshal([]byte(s), &def); err != nil {
			return "", 0, fmt.Errorf("子流程定义 JSON 解析失败: %w", err)
		}
	}
	if strings.TrimSpace(def.Flow) == "" {
		return "", 0, fmt.Errorf("子流程节点未配置目标流程（flow）")
	}
	if r.SubFlow == nil {
		return "", 0, fmt.Errorf("子流程节点不可用：运行器未注入 SubFlow 回调")
	}
	// 输入映射逐值渲染（{var}/{nodeID.output}）；渲染失败按原串（与 http 节点容错一致）。
	mvars := st.mergedVars()
	in := make(map[string]any, len(def.Vars))
	for k, expr := range def.Vars {
		v := expr
		if strings.Contains(v, "{") {
			if out, err := renderVars(ctx, v, mvars); err == nil {
				v = out
			}
		}
		in[k] = v
	}
	res, err := r.SubFlow(ctx, def.Flow, in, dryRun)
	if err != nil {
		return "", 0, err
	}
	// 子流程轨迹并入主轨迹（前缀「子流程:」区分，Index 由调用方统一推进）。
	for _, tr := range res.Steps {
		sub := tr
		sub.Name = "子流程:" + strings.TrimSpace(tr.Name)
		st.steps = append(st.steps, sub)
	}
	return res.Output, len(res.Steps), nil
}
