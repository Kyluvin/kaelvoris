package flow

import (
	"context"
	"strings"
	"testing"
)

// mkBranch 画布辅助：S → BR →(cond) B → Z / BR →(else) A → Z。
// 类型序列固定 [branch, llm, llm]（A=else 分支、B=条件分支）。
func mkBranchCond(expr string) *Canvas {
	c := &Canvas{
		Nodes: []CanvasNode{
			{ID: "S", Type: StepStart, Pos: Pos{X: -200}, Step: Step{Type: StepStart, Name: "开始"}},
			{ID: "BR", Type: StepBranch, Pos: Pos{X: 0}, Step: Step{Type: StepBranch, Name: "条件分支"}},
			{ID: "A", Type: StepLLM, Pos: Pos{X: 200, Y: 120}, Step: Step{Type: StepLLM, Name: "分支A"}},
			{ID: "B", Type: StepLLM, Pos: Pos{X: 200, Y: -120}, Step: Step{Type: StepLLM, Name: "分支B"}},
			{ID: "Z", Type: StepEnd, Pos: Pos{X: 400}, Step: Step{Type: StepEnd, Name: "结束"}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "BR"},
			{ID: "e2", Source: "BR", Target: "B", Expr: expr},
			{ID: "e3", Source: "BR", Target: "A"},
			{ID: "e4", Source: "A", Target: "Z"},
			{ID: "e5", Source: "B", Target: "Z"},
		},
	}
	return c
}

// TestRunGraph_BranchCondition 图执行条件路由：{x} > 10 命中走 B，未命中走 else（A）。
func TestRunGraph_BranchCondition(t *testing.T) {
	var msgs []StepTrace
	r := &Runner{OnStep: func(tr StepTrace) { msgs = append(msgs, tr) }}
	_ = msgs
	_ = r

	// x=99 → 命中 {x} > 10 → B 分支（dryRun=true：llm 节点跳过真实调用，仅验证路由）
	c := mkBranchCond("{x} > 10")
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "f1"}, c, map[string]any{"x": 99}, true)
	if err != nil {
		t.Fatalf("RunGraph(x=99): %v", err)
	}
	if len(res.Steps) != 1 || res.Steps[0].NodeID != "B" {
		t.Fatalf("x=99 应走分支B且轨迹携 NodeID: %+v", res.Steps)
	}

	// x=3 → 未命中 → else 兜底 → A 分支
	c2 := mkBranchCond("{x} > 10")
	res2, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "f1"}, c2, map[string]any{"x": 3}, true)
	if err != nil {
		t.Fatalf("RunGraph(x=3): %v", err)
	}
	if len(res2.Steps) != 1 || res2.Steps[0].Name != "分支A" {
		t.Fatalf("x=3 应走 else 分支A: %+v", res2.Steps)
	}
}

// TestRunGraph_VarRender 业务节点模板变量渲染：graph 路径与 chain 路径语义一致。
func TestRunGraph_VarRender(t *testing.T) {
	c := &Canvas{
		Nodes: []CanvasNode{
			{ID: "S", Type: StepStart, Pos: Pos{X: -200}, Step: Step{Type: StepStart}},
			{ID: "A", Type: StepTemplate, Pos: Pos{X: 0}, Step: Step{Type: StepTemplate, Role: "user", Template: "你好 {name}"}},
			{ID: "Z", Type: StepEnd, Pos: Pos{X: 200}, Step: Step{Type: StepEnd}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "A"},
			{ID: "e2", Source: "A", Target: "Z"},
		},
	}
	r := &Runner{}
	res, err := r.RunGraph(context.Background(), &Flow{ID: "f2"}, c, map[string]any{"name": "图执行"}, true)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if len(res.Steps) != 1 || res.Steps[0].Render != "你好 图执行" {
		t.Fatalf("模板渲染不符: %+v", res.Steps)
	}
	if res.Steps[0].NodeID != "A" {
		t.Fatalf("NodeID: %+v", res.Steps[0])
	}
}

// TestRunGraph_NoCondition 分支无命中条件且无 else 出边 → 报错（含节点 ID）。
func TestRunGraph_NoCondition(t *testing.T) {
	c := &Canvas{
		Nodes: []CanvasNode{
			{ID: "S", Type: StepStart},
			{ID: "BR", Type: StepBranch},
			{ID: "A", Type: StepLLM},
			{ID: "Z", Type: StepEnd},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "BR"},
			{ID: "e2", Source: "BR", Target: "A", Expr: "{never} == true"},
			{ID: "e4", Source: "A", Target: "Z"},
		},
	}
	_, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "f3"}, c, map[string]any{}, true)
	if err == nil || !strings.Contains(err.Error(), "分支节点 BR") {
		t.Fatalf("期望分支无兜底报错，got %v", err)
	}
}

// TestEvalBranchExpr 条件表达式求值：数值/字符串比较、contains、裸变量真值、else。
func TestEvalBranchExpr(t *testing.T) {
	vars := map[string]any{"score": 85, "name": "vip-user", "flag": "on", "n": "0"}
	cases := []struct {
		expr string
		want bool
	}{
		{"{score} > 80", true},
		{"{score} > 90", false},
		{"{score} >= 85", true},
		{"{score} < 80", false},
		{"{score} <= 85", true},
		{"{score} == 85", true},
		{"{score} != 85", false},
		{"{name} contains vip", true},
		{"{name} contains abc", false},
		{"{flag}", true},
		{"{n}", false}, // "0" 视为假
		{"", true},     // else 恒真
		{"{name} == vip-user", true},
		{"{name} != vip-user", false},
	}
	for _, c := range cases {
		got, err := evalBranchExpr(context.Background(), c.expr, vars)
		if err != nil {
			t.Fatalf("eval %q: %v", c.expr, err)
		}
		if got != c.want {
			t.Fatalf("eval %q = %v, want %v", c.expr, got, c.want)
		}
	}
}

// TestPickBranchOrder 条件按边声明顺序求值，首个命中即路由（多条条件同时为真取前者）。
func TestPickBranchOrder(t *testing.T) {
	edges := []CanvasEdge{
		{ID: "e1", Source: "BR", Target: "X", Expr: "{v} > 5"},
		{ID: "e2", Source: "BR", Target: "Y", Expr: "{v} > 3"},
		{ID: "e3", Source: "BR", Target: "Z"},
	}
	got, err := pickBranch(edges, "BR", map[string]any{"v": 10})
	if err != nil {
		t.Fatalf("pickBranch: %v", err)
	}
	if got != "X" {
		t.Fatalf("want X, got %s", got)
	}
	got, err = pickBranch(edges, "BR", map[string]any{"v": 4})
	if err != nil {
		t.Fatalf("pickBranch: %v", err)
	}
	if got != "Y" {
		t.Fatalf("want Y, got %s", got)
	}
	got, err = pickBranch(edges, "BR", map[string]any{"v": 1})
	if err != nil {
		t.Fatalf("pickBranch: %v", err)
	}
	if got != "Z" {
		t.Fatalf("want Z(else), got %s", got)
	}
}

// TestHasBranch 分支检测：HasBranch 决定 RunFlow 走 RunGraph 还是 Run。
func TestHasBranch(t *testing.T) {
	chain := mkChain([]string{StepTemplate, StepLLM})
	if HasBranch(chain) {
		t.Fatal("线性链不应含 branch")
	}
	if !HasBranch(mkBranchCond("{x} > 1")) {
		t.Fatal("分支画布应检出 branch")
	}
}
