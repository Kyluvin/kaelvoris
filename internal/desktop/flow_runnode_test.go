package desktop

// RunNode transform 节点调试路径测试（2026-10 Dify 语义融入收尾）：
// transform 需变量池回写，RunNode 对其走 RunGraph 最小画布（S→N→Z），
// 而非 Chain 路径（Run 对 StepTransform 直接报错）。此处验证：
//  1. transform 节点 RunNode 调试可执行且输出正确（dry-run 也真实执行）；
//  2. 轨迹携带原画布 NodeID（前端按节点高亮依赖）。

import (
	"testing"

	"kaelvoris/internal/flow"
)

// TestRunNodeTransform 图路径调试：保存含 transform 节点的画布流程 →
// RunNode 调试该节点 → 输出与 NodeID 正确。
func TestRunNodeTransform(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("USERPROFILE", tmpHome)

	c := &flow.Canvas{
		Nodes: []flow.CanvasNode{
			{ID: "n1", Type: flow.StepStart, Step: flow.Step{Type: flow.StepStart}},
			{ID: "n2", Type: flow.StepTransform, Step: flow.Step{Type: flow.StepTransform, Name: "转换", Role: "python", Template: "{{upper name}}"}},
			{ID: "n3", Type: flow.StepEnd, Step: flow.Step{Type: flow.StepEnd}},
		},
		Edges: []flow.CanvasEdge{
			{ID: "e1", Source: "n1", Target: "n2"},
			{ID: "e2", Source: "n2", Target: "n3"},
		},
	}
	f := &flow.Flow{ID: "tf_node", Name: "transform 调试", Canvas: c}
	a := &App{}
	if _, err := a.SaveFlow(*f); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}

	res, err := a.RunNode(RunNodeRequest{FlowID: f.ID, NodeID: "n2", Vars: map[string]any{"name": "dify"}, DryRun: true})
	if err != nil {
		t.Fatalf("RunNode transform: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("want 1 trace (仅 transform 节点), got %d: %+v", len(res.Steps), res.Steps)
	}
	tr := res.Steps[0]
	if tr.NodeID != "n2" || tr.Type != flow.StepTransform || tr.Render != "DIFY" {
		t.Fatalf("trace = %+v, want NodeID n2 / DIFY", tr)
	}

	// 缺变量快速失败（missingkey=error）：空 vars 下 {{name}} 应报错上抛
	if _, err := a.RunNode(RunNodeRequest{FlowID: f.ID, NodeID: "n2", Vars: map[string]any{}, DryRun: true}); err == nil {
		t.Fatal("缺变量应报错（missingkey=error）")
	}
}
