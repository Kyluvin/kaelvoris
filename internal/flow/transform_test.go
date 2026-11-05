package flow

// transform 节点（代码转换）与变量池（节点输出引用）测试 —— 2026-10 Dify 语义融入。
//
// 覆盖三类能力：
//  1. renderTransform 脚本语法：变量替换 / 算术 / 内置函数 / if-elif-else / 嵌套引用；
//  2. RunGraph 变量池：transform 输出写回 {nodeID.output}，下游 template 与
//     branch 条件按点分路径引用；
//  3. 条件入边门禁（Dify branch 同构）：入边条件未命中 → 节点及下游跳过。

import (
	"context"
	"strings"
	"testing"
)

// testCtx 测试上下文（各用例共享，避免重复书写）。
func testCtx() context.Context { return context.Background() }

// TestRenderTransform_Basics 脚本语法逐项验证。
func TestRenderTransform_Basics(t *testing.T) {
	vars := map[string]any{
		"name":  "vip-user",
		"score": 85,
		"deep":  map[string]any{"output": "hello"},
	}
	cases := []struct {
		script string
		want   string
	}{
		{"{{name}}", "vip-user"},       // 变量替换
		{"{{score + 1}}", "86"},        // 算术
		{"{{score * 2}}", "170"},       // 乘法
		{"{{upper name}}", "VIP-USER"}, // 内置函数
		{"{{lower name}}", "vip-user"}, // 小写
		{"{{deep.output}}", "hello"},   // 嵌套点分路径（变量池引用）
		{"名字：{{name}}，分：{{score}}", "名字：vip-user，分：85"},                  // 混排
		{"if score > 80:\n及格\nelse:\n不及格\nendif", "及格"},                  // python 条件
		{"if score < 80:\n不及格\nelse:\n及格\nendif", "及格"},                  // else 兜底
		{"if score > 90:\nA\nelif score > 80:\nB\nelse:\nC\nendif", "B"}, // elif
		{"if name contains vip:\n是VIP\nendif", "是VIP"},                   // contains
		{"a{b}c", "a{b}c"}, // 单大括号字面量不替换
	}
	for _, c := range cases {
		got, err := renderTransform(c.script, vars, "python")
		if err != nil {
			t.Fatalf("script %q: %v", c.script, err)
		}
		if got != c.want {
			t.Fatalf("script %q = %q, want %q", c.script, got, c.want)
		}
	}
}

// TestRenderTransform_JSStyle javascript 风格控制流。
func TestRenderTransform_JSStyle(t *testing.T) {
	vars := map[string]any{"score": 85}
	got, err := renderTransform("if (score > 80) {\n及格\n} else {\n不及格\n}", vars, "javascript")
	if err != nil {
		t.Fatalf("js style: %v", err)
	}
	if strings.TrimSpace(got) != "及格" {
		t.Fatalf("js if = %q, want 及格", got)
	}
}

// TestRenderTransform_MissingVar 缺变量快速失败（missingkey=error）。
func TestRenderTransform_MissingVar(t *testing.T) {
	_, err := renderTransform("{{nope}}", map[string]any{}, "python")
	if err == nil || !strings.Contains(err.Error(), "执行失败") {
		t.Fatalf("want missing var error, got %v", err)
	}
}

// mkTransformCanvas 构造 S → T(transform) → A(template) → Z 画布，
// A 的模板引用 {T.output}（变量池跨节点引用）。
func mkTransformCanvas(script string) *Canvas {
	return &Canvas{
		Nodes: []CanvasNode{
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart, Name: "开始"}},
			{ID: "T", Type: StepTransform, Step: Step{Type: StepTransform, Name: "转换", Role: "python", Template: script}},
			{ID: "A", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "结果：{T.output}"}},
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd, Name: "结束"}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "T"},
			{ID: "e2", Source: "T", Target: "A"},
			{ID: "e3", Source: "A", Target: "Z"},
		},
	}
}

// TestRunGraph_TransformVarPool transform 输出写回变量池，下游模板引用。
func TestRunGraph_TransformVarPool(t *testing.T) {
	c := mkTransformCanvas("{{upper name}}")
	res, err := (&Runner{}).RunGraph(testCtx(), &Flow{ID: "tf"}, c, map[string]any{"name": "dify"}, true)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	// 轨迹：T + A（S/Z 透传不计轨迹）
	if len(res.Steps) != 2 {
		t.Fatalf("want 2 traces, got %d: %+v", len(res.Steps), res.Steps)
	}
	if res.Steps[0].Type != StepTransform || res.Steps[0].Render != "DIFY" {
		t.Fatalf("transform trace: %+v", res.Steps[0])
	}
	// 下游模板通过 {T.output} 引用到了 transform 的输出
	if res.Steps[1].Render != "结果：DIFY" {
		t.Fatalf("downstream template: %+v", res.Steps[1])
	}
}

// TestRunGraph_TransformThenBranch transform 输出作为分支条件输入
// （Dify 典型链路：代码节点加工 → 条件分支按输出路由）。
func TestRunGraph_TransformThenBranch(t *testing.T) {
	c := &Canvas{
		Nodes: []CanvasNode{
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
			{ID: "T", Type: StepTransform, Step: Step{Type: StepTransform, Role: "python", Template: "{{name}}"}},
			{ID: "BR", Type: StepBranch, Step: Step{Type: StepBranch, Name: "分支"}},
			{ID: "B", Type: StepLLM, Step: Step{Type: StepLLM, Name: "VIP 分支"}},
			{ID: "A", Type: StepLLM, Step: Step{Type: StepLLM, Name: "普通分支"}},
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "T"},
			{ID: "e2", Source: "T", Target: "BR"},
			{ID: "e3", Source: "BR", Target: "B", Expr: "{T.output} contains vip"},
			{ID: "e4", Source: "BR", Target: "A"},
			{ID: "e5", Source: "B", Target: "Z"},
			{ID: "e6", Source: "A", Target: "Z"},
		},
	}
	// name=vip-user → T.output="vip-user" → contains vip 命中 → B 分支
	// （轨迹：transform T + 分支选中的 llm B，共 2 条）
	res, err := (&Runner{}).RunGraph(testCtx(), &Flow{ID: "tb"}, c, map[string]any{"name": "vip-user"}, true)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if len(res.Steps) != 2 || res.Steps[0].NodeID != "T" || res.Steps[1].NodeID != "B" {
		t.Fatalf("应执行 T 并路由到 VIP 分支 B: %+v", res.Steps)
	}
	// name=guest → else → A 分支
	res2, err := (&Runner{}).RunGraph(testCtx(), &Flow{ID: "tb"}, c, map[string]any{"name": "guest"}, true)
	if err != nil {
		t.Fatalf("RunGraph2: %v", err)
	}
	if len(res2.Steps) != 2 || res2.Steps[1].NodeID != "A" {
		t.Fatalf("应路由到普通分支 A: %+v", res2.Steps)
	}
}

// TestRunGraph_ConditionalInEdge 条件入边门禁：入边条件未命中 → 节点及下游跳过
// （Dify branch 同构语义；A 的入边无条件，恒可执行）。
func TestRunGraph_ConditionalInEdge(t *testing.T) {
	c := &Canvas{
		Nodes: []CanvasNode{
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
			{ID: "BR", Type: StepBranch, Step: Step{Type: StepBranch}},
			{ID: "B", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "B", Template: "B 路径"}},
			{ID: "A", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "A", Template: "A 路径"}},
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "BR"},
			{ID: "e2", Source: "BR", Target: "B", Expr: "{go} == yes"},
			{ID: "e3", Source: "BR", Target: "A"},
			{ID: "e4", Source: "B", Target: "Z"},
			{ID: "e5", Source: "A", Target: "Z"},
		},
	}
	// go=no → B 入边条件未命中 → 走 A（B 被跳过不产生轨迹）
	res, err := (&Runner{}).RunGraph(testCtx(), &Flow{ID: "ce"}, c, map[string]any{"go": "no"}, true)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if len(res.Steps) != 1 || res.Steps[0].NodeID != "A" {
		t.Fatalf("条件入边未命中应跳过 B 走 A: %+v", res.Steps)
	}
}

// TestTraceVars_Canvas 画布模式下变量提取：覆盖节点模板 + 分支条件，
// 且 {T.output} 节点引用（点分路径）不误列为待填写变量。
func TestTraceVars_Canvas(t *testing.T) {
	c := mkTransformCanvas("{{upper name}}")
	c.Edges = append(c.Edges, CanvasEdge{ID: "e9", Source: "A", Target: "Z", Expr: "{T.output} contains {tag}"})
	f := &Flow{ID: "tv", Canvas: c}
	vars := TraceVars(f)
	// name（模板）与 tag（分支条件）应提取；T.output（点分路径）应过滤
	joined := strings.Join(vars, ",")
	if !strings.Contains(joined, "name") || !strings.Contains(joined, "tag") {
		t.Fatalf("应含 name/tag: %v", vars)
	}
	for _, v := range vars {
		if strings.Contains(v, ".") {
			t.Fatalf("点分路径 %q 不应列为待填写变量", v)
		}
	}
}

// TestExecTransform_LangCheck 语言与空脚本校验。
func TestExecTransform_LangCheck(t *testing.T) {
	r := &Runner{}
	st := &state{vars: map[string]any{}}
	if _, err := r.execTransform(testCtx(), st, Step{Role: "ruby", Template: "x"}); err == nil {
		t.Fatal("不支持的语言应报错")
	}
	if _, err := r.execTransform(testCtx(), st, Step{Role: "python", Template: "  "}); err == nil {
		t.Fatal("空脚本应报错")
	}
}
