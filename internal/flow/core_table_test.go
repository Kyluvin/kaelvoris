// flow 引擎核心表驱动单测（规划板条目：拓扑执行顺序 / 变量池注入取值 / 分支门禁）。
//
// mock 策略：节点执行器不直接 mock，全部走 Runner 已有的可注入回调路径
// （OnStep 轨迹收集 + KB 回调假实现 + dry-run 跳过 llm/tools 真实调用），
// 与经验卡「Runner 回调注入」扩展路径一致，零新增依赖。
package flow

import (
	"context"
	"fmt"
	"testing"
)

// ── 一、拓扑执行顺序 ─────────────────────────────────────────────

// runTrace 收集 OnStep 轨迹（执行序）。
func runTrace() (*[]StepTrace, *Runner) {
	traces := &[]StepTrace{}
	r := &Runner{OnStep: func(tr StepTrace) { *traces = append(*traces, tr) }}
	return traces, r
}

// nodeSeq 运行结果轨迹中的节点 ID 执行序列（OnStep 回调轨迹不携 NodeID，
// 引擎在节点成功后才回填 st.steps，故以 RunResult.Steps 为准）。
func nodeSeq(res *RunResult) []string {
	out := make([]string, 0, len(res.Steps))
	for _, tr := range res.Steps {
		out = append(out, tr.NodeID)
	}
	return out
}

// seqEqual 序列全等比较。
func seqEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// mkLineCanvas 构造线性画布：start → n1(t1 类型) → … → end（Nodes 按声明序传入，
// 拓扑由 Edges 连线决定而非声明序，借此验证执行沿连线走）。
func mkLineCanvas(nodes []CanvasNode) *Canvas {
	c := &Canvas{Nodes: nodes}
	for i := 0; i+1 < len(nodes); i++ {
		c.Edges = append(c.Edges, CanvasEdge{
			ID: "e" + nodes[i].ID, Source: nodes[i].ID, Target: nodes[i+1].ID,
		})
	}
	return c
}

// TestRunGraph_TopologicalOrder 表驱动：图执行沿连线拓扑序执行，与节点声明序无关。
// 轨迹只含业务节点（start/end/branch 透传不产轨迹），Index 单调递增。
func TestRunGraph_TopologicalOrder(t *testing.T) {
	// revCanvas 声明序倒置画布：Nodes 数组 [Z,T2,T1,S]，Edges 连线仍为 S→T1→T2→Z。
	revCanvas := &Canvas{
		Nodes: []CanvasNode{
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
			{ID: "T2", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "n2", Template: "二 {v}"}},
			{ID: "T1", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "n1", Template: "一 {v}"}},
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "T1"},
			{ID: "e2", Source: "T1", Target: "T2"},
			{ID: "e3", Source: "T2", Target: "Z"},
		},
	}
	cases := []struct {
		name   string
		canvas *Canvas
		vars   map[string]any
		want   []string // 期望执行序（NodeID）
	}{
		{
			name: "线性链_声明序与执行序一致",
			canvas: mkLineCanvas([]CanvasNode{
				{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
				{ID: "T1", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "n1", Template: "一 {v}"}},
				{ID: "T2", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "n2", Template: "二 {v}"}},
				{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
			}),
			vars: map[string]any{"v": "x"},
			want: []string{"T1", "T2"},
		},
		{
			name:   "声明序倒置_仍按连线拓扑执行",
			canvas: revCanvas,
			vars:   map[string]any{"v": "y"},
			want:   []string{"T1", "T2"},
		},
		{
			name: "passthrough_节点也计入拓扑",
			canvas: mkLineCanvas([]CanvasNode{
				{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
				{ID: "P", Type: StepPassthrough, Step: Step{Type: StepPassthrough, Name: "透传"}},
				{ID: "T", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "n", Template: "{v}"}},
				{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
			}),
			vars: map[string]any{"v": "z"},
			want: []string{"P", "T"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			traces, r := runTrace()
			res, err := r.RunGraph(context.Background(), &Flow{ID: "topo"}, tc.canvas, tc.vars, true)
			if err != nil {
				t.Fatalf("RunGraph: %v", err)
			}
			if got := nodeSeq(res); !seqEqual(got, tc.want) {
				t.Fatalf("执行序 = %v, want %v", got, tc.want)
			}
			if len(res.Steps) != len(tc.want) {
				t.Fatalf("轨迹数 = %d, want %d", len(res.Steps), len(tc.want))
			}
			// Index 按执行序单调递增。
			for i, tr := range res.Steps {
				if tr.Index != i {
					t.Fatalf("轨迹 %d Index = %d，应单调", i, tr.Index)
				}
			}
			// OnStep 每个业务节点回调恰一次。
			if len(*traces) != len(tc.want) {
				t.Fatalf("OnStep 回调 %d 次, want %d", len(*traces), len(tc.want))
			}
		})
	}
}

// ── 二、变量池注入取值 ───────────────────────────────────────────

// TestMergedVars 表驱动：用户变量与节点输出变量池合并语义（池优先、浅拷贝不污染原值）。
func TestMergedVars(t *testing.T) {
	cases := []struct {
		name  string
		vars  map[string]any
		pools map[string]map[string]any
		key   string
		want  any
	}{
		{
			name:  "仅用户变量",
			vars:  map[string]any{"x": 1},
			pools: nil,
			key:   "x",
			want:  1,
		},
		{
			name:  "池优先于用户变量",
			vars:  map[string]any{"n": "user"},
			pools: map[string]map[string]any{"A": {"output": "node"}},
			key:   "A",
			want:  map[string]any{"output": "node"},
		},
		{
			name:  "同名键池覆盖用户变量",
			vars:  map[string]any{"v": "user"},
			pools: map[string]map[string]any{"v": {"output": "pool"}},
			key:   "v",
			want:  map[string]any{"output": "pool"},
		},
		{
			name:  "空池返回用户变量",
			vars:  map[string]any{"v": 42},
			pools: map[string]map[string]any{},
			key:   "v",
			want:  42,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &state{vars: tc.vars, pools: tc.pools}
			m := st.mergedVars()
			got, ok := m[tc.key]
			if !ok {
				t.Fatalf("mergedVars 缺键 %q", tc.key)
			}
			if fmtSprint(got) != fmtSprint(tc.want) {
				t.Fatalf("mergedVars[%q] = %v, want %v", tc.key, got, tc.want)
			}
			// 合并产生新 map：写入合并结果不回写污染用户变量表与池
			//（transform 脚本取值幂等的前提）。
			m["__probe__"] = 1
			if _, hit := tc.vars["__probe__"]; hit {
				t.Fatal("合并结果写入了用户变量表（应产生新 map）")
			}
		})
	}
}

// fmtSprint 统一字符串化比较。
func fmtSprint(v any) string { return fmt.Sprint(v) }

// TestRunGraph_VarPoolInject 表驱动：上游节点输出写回变量池，下游经
// {nodeID.output} 点分路径取值（template 渲染 / branch 条件 / transform 脚本三消费方）。
func TestRunGraph_VarPoolInject(t *testing.T) {
	// 假 KB：回显查询，模拟「检索结果进池，下游引用」。
	kbRunner := func(calls *[]string) *Runner {
		return &Runner{
			KB: func(_ context.Context, query string, _ int, _ float64) (string, int, error) {
				*calls = append(*calls, query)
				return "KB命中:" + query, 1, nil
			},
		}
	}
	cases := []struct {
		name    string
		canvas  *Canvas
		vars    map[string]any
		r       func() *Runner
		wantSeq []string
		check   func(t *testing.T, res *RunResult)
	}{
		{
			name: "transform输出进池_下游模板引用",
			canvas: mkLineCanvas([]CanvasNode{
				{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
				{ID: "TR", Type: StepTransform, Step: Step{Type: StepTransform, Role: "python", Name: "转换", Template: "{{name}}"}},
				{ID: "TP", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "池值:{TR.output}"}},
				{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
			}),
			vars:    map[string]any{"name": "pool-value"},
			r:       func() *Runner { return &Runner{} },
			wantSeq: []string{"TR", "TP"},
			check: func(t *testing.T, res *RunResult) {
				if res.Steps[1].Render != "池值:pool-value" {
					t.Fatalf("下游模板 = %q, want %q", res.Steps[1].Render, "池值:pool-value")
				}
			},
		},
		{
			name: "knowledge输出进池_下游模板引用",
			canvas: mkLineCanvas([]CanvasNode{
				{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
				{ID: "K", Type: StepKnowledge, Step: Step{Type: StepKnowledge, Name: "检索", Template: "查{q}"}},
				{ID: "TP", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "上游:{K.output}"}},
				{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
			}),
			vars: map[string]any{"q": "dify"},
			r: func() *Runner {
				return kbRunner(&[]string{})
			},
			wantSeq: []string{"K", "TP"},
			check: func(t *testing.T, res *RunResult) {
				if res.Steps[1].Render != "上游:KB命中:查dify" {
					t.Fatalf("下游模板 = %q", res.Steps[1].Render)
				}
			},
		},
		{
			name: "transform输出进池_分支条件点分取值",
			canvas: func() *Canvas {
				c := &Canvas{
					Nodes: []CanvasNode{
						{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
						{ID: "TR", Type: StepTransform, Step: Step{Type: StepTransform, Role: "python", Name: "转换", Template: "{{level}}"}},
						{ID: "BR", Type: StepBranch, Step: Step{Type: StepBranch}},
						{ID: "HI", Type: StepPassthrough, Step: Step{Type: StepPassthrough, Name: "高"}},
						{ID: "LO", Type: StepPassthrough, Step: Step{Type: StepPassthrough, Name: "低"}},
						{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
					},
					Edges: []CanvasEdge{
						{ID: "e1", Source: "S", Target: "TR"},
						{ID: "e2", Source: "TR", Target: "BR"},
						{ID: "e3", Source: "BR", Target: "HI", Expr: "{TR.output} > 10"},
						{ID: "e4", Source: "BR", Target: "LO"},
						{ID: "e5", Source: "HI", Target: "Z"},
						{ID: "e6", Source: "LO", Target: "Z"},
					},
				}
				return c
			}(),
			vars: map[string]any{"level": "15"},
			r:    func() *Runner { return &Runner{} },
			check: func(t *testing.T, res *RunResult) {
				if len(res.Steps) != 2 || res.Steps[1].Name != "高" {
					t.Fatalf("应按池值 15>10 走高分支: %+v", res.Steps)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.r()
			res, err := r.RunGraph(context.Background(), &Flow{ID: "pool"}, tc.canvas, tc.vars, false)
			if err != nil {
				t.Fatalf("RunGraph: %v", err)
			}
			if tc.wantSeq != nil {
				if got := res.Steps; len(got) != len(tc.wantSeq) {
					t.Fatalf("轨迹数 = %d, want %d: %+v", len(got), len(tc.wantSeq), got)
				}
			}
			if tc.check != nil {
				tc.check(t, res)
			}
		})
	}
}

// ── 三、分支门禁（branch 出边路由 + 条件入边门禁）────────────────

// TestBranchGate 表驱动：分支节点出边按条件路由（真/假分支与 else 兜底），
// 以及条件入边门禁（未命中 → 节点及其下游跳过）。
func TestBranchGate(t *testing.T) {
	// mkGate S → BR →(expr) G → Z / BR →(else) E → Z；
	// G/E 为 passthrough（dry-run 无 llm 依赖，恒可执行）。
	mkGate := func(expr string) *Canvas {
		return &Canvas{
			Nodes: []CanvasNode{
				{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
				{ID: "BR", Type: StepBranch, Step: Step{Type: StepBranch}},
				{ID: "G", Type: StepPassthrough, Step: Step{Type: StepPassthrough, Name: "真分支"}},
				{ID: "E", Type: StepPassthrough, Step: Step{Type: StepPassthrough, Name: "假分支"}},
				{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
			},
			Edges: []CanvasEdge{
				{ID: "e1", Source: "S", Target: "BR"},
				{ID: "e2", Source: "BR", Target: "G", Expr: expr},
				{ID: "e3", Source: "BR", Target: "E"},
				{ID: "e4", Source: "G", Target: "Z"},
				{ID: "e5", Source: "E", Target: "Z"},
			},
		}
	}
	// mkInEdgeGate start → T →(条件入边) G → Z：门禁命中与否直接决定 G 是否执行。
	mkInEdgeGate := func(expr string) *Canvas {
		return &Canvas{
			Nodes: []CanvasNode{
				{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
				{ID: "T", Type: StepPassthrough, Step: Step{Type: StepPassthrough, Name: "前置"}},
				{ID: "G", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "门禁节点", Template: "通过:{v}"}},
				{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
			},
			Edges: []CanvasEdge{
				{ID: "e1", Source: "S", Target: "T"},
				{ID: "e2", Source: "T", Target: "G", Expr: expr},
				{ID: "e3", Source: "G", Target: "Z"},
			},
		}
	}
	cases := []struct {
		name    string
		expr    string
		vars    map[string]any
		canvas  func(expr string) *Canvas
		wantSeq []string
	}{
		{
			name: "出边条件命中走真分支", expr: "{x} > 10",
			vars: map[string]any{"x": 99}, canvas: mkGate,
			wantSeq: []string{"G"},
		},
		{
			name: "出边条件未命中走else", expr: "{x} > 10",
			vars: map[string]any{"x": 3}, canvas: mkGate,
			wantSeq: []string{"E"},
		},
		{
			name: "contains命中", expr: "{tag} contains vip",
			vars: map[string]any{"tag": "vip-user"}, canvas: mkGate,
			wantSeq: []string{"G"},
		},
		{
			name: "字符串相等命中", expr: "{mode} == on",
			vars: map[string]any{"mode": "on"}, canvas: mkGate,
			wantSeq: []string{"G"},
		},
		{
			name: "入边门禁命中执行", expr: "{go} == yes",
			vars: map[string]any{"go": "yes", "v": "ok"}, canvas: mkInEdgeGate,
			wantSeq: []string{"T", "G"},
		},
		{
			name: "入边门禁未命中跳过节点及下游", expr: "{go} == yes",
			vars: map[string]any{"go": "no", "v": "ok"}, canvas: mkInEdgeGate,
			wantSeq: []string{"T"},
		},
		{
			name: "入边无条件恒真", expr: "",
			vars: map[string]any{"v": "free"}, canvas: mkInEdgeGate,
			wantSeq: []string{"T", "G"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, r := runTrace()
			res, err := r.RunGraph(context.Background(), &Flow{ID: "gate"}, tc.canvas(tc.expr), tc.vars, true)
			if err != nil {
				t.Fatalf("RunGraph: %v", err)
			}
			if got := nodeSeq(res); !seqEqual(got, tc.wantSeq) {
				t.Fatalf("执行序 = %v, want %v", got, tc.wantSeq)
			}
			if res.DryRun != true {
				t.Fatal("dryRun 标志应透传到结果")
			}
		})
	}
}

// TestCondSatisfied 表驱动：condSatisfied 门禁求值器直接验证
// （无条件入边恒真 / 条件命中 / 条件未命中 / 求值失败视为未命中不报错）。
func TestCondSatisfied(t *testing.T) {
	r := &Runner{}
	cases := []struct {
		name   string
		edges  []CanvasEdge
		target string
		st     *state
		want   bool
	}{
		{
			name:   "无入边恒真",
			edges:  nil,
			target: "G",
			st:     &state{vars: map[string]any{}},
			want:   true,
		},
		{
			name:   "无条件入边恒真",
			edges:  []CanvasEdge{{ID: "e1", Source: "A", Target: "G"}},
			target: "G",
			st:     &state{vars: map[string]any{}},
			want:   true,
		},
		{
			name:   "条件命中",
			edges:  []CanvasEdge{{ID: "e1", Source: "A", Target: "G", Expr: "{x} > 5"}},
			target: "G",
			st:     &state{vars: map[string]any{"x": 9}},
			want:   true,
		},
		{
			name:   "条件未命中",
			edges:  []CanvasEdge{{ID: "e1", Source: "A", Target: "G", Expr: "{x} > 5"}},
			target: "G",
			st:     &state{vars: map[string]any{"x": 2}},
			want:   false,
		},
		{
			name:   "多条入边其一未命中即不满足",
			edges:  []CanvasEdge{{ID: "e1", Source: "A", Target: "G", Expr: "{x} > 5"}, {ID: "e2", Source: "B", Target: "G", Expr: "{y} > 1"}},
			target: "G",
			st:     &state{vars: map[string]any{"x": 9, "y": 0}},
			want:   false,
		},
		{
			name:   "其他目标节点的边不影响",
			edges:  []CanvasEdge{{ID: "e1", Source: "A", Target: "OTHER", Expr: "{x} > 999"}},
			target: "G",
			st:     &state{vars: map[string]any{"x": 1}},
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.condSatisfied(tc.edges, tc.target, tc.st); got != tc.want {
				t.Fatalf("condSatisfied = %v, want %v", got, tc.want)
			}
		})
	}
}
