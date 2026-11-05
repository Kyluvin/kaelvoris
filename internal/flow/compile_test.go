package flow

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// mkChain 构造线性链画布辅助（自动加 start/end 结构节点）：types 为业务节点类型序列。
func mkChain(types []string) *Canvas {
	c := &Canvas{}
	ts := timeNowMs()
	start := CanvasNode{ID: "S", Type: StepStart, Pos: Pos{X: -200}, Step: Step{Type: StepStart, Name: "开始"}}
	c.Nodes = append(c.Nodes, start)
	var prev string
	for i, t := range types {
		id := string(rune('A' + i))
		c.Nodes = append(c.Nodes, CanvasNode{ID: id, Type: t, Pos: Pos{X: float64(i) * 200}, Step: Step{Type: t, Name: "步骤" + id}})
		if prev != "" {
			c.Edges = append(c.Edges, CanvasEdge{ID: prev + "-" + id + suffix(ts), Source: prev, Target: id})
		} else {
			c.Edges = append(c.Edges, CanvasEdge{ID: "S-" + id + suffix(ts), Source: "S", Target: id})
		}
		prev = id
	}
	end := CanvasNode{ID: "Z", Type: StepEnd, Pos: Pos{X: float64(len(types)) * 200}, Step: Step{Type: StepEnd, Name: "结束"}}
	c.Nodes = append(c.Nodes, end)
	if prev != "" {
		c.Edges = append(c.Edges, CanvasEdge{ID: prev + "-Z" + suffix(ts), Source: prev, Target: "Z"})
	}
	return c
}

func timeNowMs() int64       { return time.Now().UnixMilli() }
func suffix(ms int64) string { return "_" + fmt.Sprint(ms) }

// TestCompileLinearChain 线性链 OK：编译步骤顺序与配置保真（start/end 剥离）。
func TestCompileLinearChain(t *testing.T) {
	c := mkChain([]string{StepTemplate, StepLLM, StepPassthrough})
	c.Nodes[1].Step.Template = "你好 {name}"
	c.Nodes[1].Step.Role = "user"
	c.Nodes[3].Step.Name = "透传"
	steps, err := CompileCanvas(c)
	if err != nil {
		t.Fatalf("CompileCanvas: %v", err)
	}
	want := []Step{
		{Type: StepTemplate, Role: "user", Template: "你好 {name}", Name: "步骤A"},
		{Type: StepLLM, Name: "步骤B"},
		{Type: StepPassthrough, Name: "透传"},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps mismatch:\n got %+v\nwant %+v", steps, want)
	}
}

// TestCompileCanvasCycle 成环检测：A→B→A。
func TestCompileCanvasCycle(t *testing.T) {
	c := mkChain([]string{StepTemplate, StepLLM})
	c.Edges = append(c.Edges, CanvasEdge{ID: "e-cycle", Source: "B", Target: "A"})
	if _, err := CompileCanvas(c); err == nil {
		t.Fatal("期望报错（环），实际通过")
	}
}

// TestCompileCanvasMultiIn 多入：A→C, B→C。
func TestCompileCanvasMultiIn(t *testing.T) {
	c := mkChain([]string{StepTemplate, StepLLM, StepPassthrough})
	c.Edges = c.Edges[:2] // 保留 S→A, A→B
	c.Edges = append(c.Edges, CanvasEdge{ID: "B-C", Source: "B", Target: "C"},
		CanvasEdge{ID: "A-C", Source: "A", Target: "C"})
	if _, err := CompileCanvas(c); err == nil {
		t.Fatal("期望报错（多入），实际通过")
	}
}

// TestCompileCanvasMultiOut 多出：A→B, A→C。
func TestCompileCanvasMultiOut(t *testing.T) {
	c := mkChain([]string{StepTemplate, StepLLM, StepPassthrough})
	// A→B 保留，A→C 加一条：A 多出（非法）
	c.Edges = c.Edges[:2]
	c.Edges = append(c.Edges, CanvasEdge{ID: "A-C", Source: "A", Target: "C"})
	// C 的入边原本来自 B（B→C, C→Z）——B 也保留；此时 C 多入，先拆掉 B→C
	for i, e := range c.Edges {
		if e.Source == "B" && e.Target == "C" {
			c.Edges = append(c.Edges[:i], c.Edges[i+1:]...)
			break
		}
	}
	if _, err := CompileCanvas(c); err == nil {
		t.Fatal("期望报错（多出），实际通过")
	}
}

// TestCompileCanvasIsland 孤岛：C 无边。
func TestCompileCanvasIsland(t *testing.T) {
	c := mkChain([]string{StepTemplate, StepLLM})
	c.Nodes = append(c.Nodes, CanvasNode{ID: "C", Type: StepPassthrough})
	if _, err := CompileCanvas(c); err == nil {
		t.Fatal("期望报错（孤岛/未连通），实际通过")
	}
}

// TestCompileCanvasBadType 非法节点类型。
func TestCompileCanvasBadType(t *testing.T) {
	c := mkChain([]string{"foo"})
	if _, err := CompileCanvas(c); err == nil {
		t.Fatal("期望报错（类型无效），实际通过")
	}
}

// TestCanvasRoundTrip 往返等价：Steps → CanvasFromSteps（自动补 start/end）→ CompileCanvas == 原 Steps。
func TestCanvasRoundTrip(t *testing.T) {
	orig := []Step{
		{Type: StepTemplate, Role: "system", Template: "你是助手"},
		{Type: StepTemplate, Role: "user", Template: "问题：{q}"},
		{Type: StepLLM, Name: "回答"},
		{Type: StepTool, Name: "fs_read", Template: `{"path":"{file}"}`},
		{Type: StepRetriever, TopK: 5},
	}
	f := &Flow{ID: "f1", Steps: orig}
	steps, err := CompileCanvas(CanvasFromSteps(f))
	if err != nil {
		t.Fatalf("往返编译失败: %v", err)
	}
	if !reflect.DeepEqual(steps, orig) {
		t.Fatalf("往返不等价:\n got %+v\nwant %+v", steps, orig)
	}
}

// TestValidateCanvasEmpty 空画布报错。
func TestValidateCanvasEmpty(t *testing.T) {
	if err := ValidateCanvas(&Canvas{}); err == nil {
		t.Fatal("期望报错（空画布），实际通过")
	}
}

// TestValidateCanvasDanglingEdge 边引用不存在节点。
func TestValidateCanvasDanglingEdge(t *testing.T) {
	c := mkChain([]string{StepTemplate, StepLLM})
	c.Edges = append(c.Edges, CanvasEdge{ID: "e-x", Source: "A", Target: "GHOST"})
	if err := ValidateCanvas(c); err == nil {
		t.Fatal("期望报错（目标节点不存在），实际通过")
	}
}

// TestValidateCanvasNoStart 缺开始节点（旧数据直接校验）。
func TestValidateCanvasNoStart(t *testing.T) {
	c := mkChain([]string{StepTemplate})
	for i, n := range c.Nodes {
		if n.Type == StepStart {
			c.Nodes = append(c.Nodes[:i], c.Nodes[i+1:]...)
			break
		}
	}
	var kept []CanvasEdge
	for _, e := range c.Edges {
		if e.Source != "S" {
			kept = append(kept, e)
		}
	}
	c.Edges = kept
	if err := ValidateCanvas(c); err == nil {
		t.Fatal("期望报错（缺开始节点），实际通过")
	}
}

// TestEnsureStartEnd 补齐幂等：已完整的画布不改动。
func TestEnsureStartEnd(t *testing.T) {
	c := mkChain([]string{StepTemplate, StepLLM})
	before := len(c.Nodes)
	if EnsureStartEnd(c) {
		t.Fatal("完整画布不应发生改动")
	}
	if len(c.Nodes) != before {
		t.Fatalf("节点数变化: %d → %d", before, len(c.Nodes))
	}
	// 剥掉 start/end 再补齐
	c2 := &Canvas{Nodes: c.Nodes[1 : len(c.Nodes)-1], Edges: c.Edges[1 : len(c.Edges)-1]}
	if !EnsureStartEnd(c2) {
		t.Fatal("缺 start/end 的画布应被补齐")
	}
	if err := ValidateCanvas(c2); err != nil {
		t.Fatalf("补齐后校验失败: %v", err)
	}
}

// TestValidateCanvasBranch 分支流程：start → branch →(cond) B → end / else A → end。
func TestValidateCanvasBranch(t *testing.T) {
	c := &Canvas{
		Nodes: []CanvasNode{
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
			{ID: "BR", Type: StepBranch, Step: Step{Type: StepBranch}},
			{ID: "A", Type: StepLLM, Step: Step{Type: StepLLM}},
			{ID: "B", Type: StepLLM, Step: Step{Type: StepLLM}},
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "BR"},
			{ID: "e2", Source: "BR", Target: "A", Expr: "{x} > 10"},
			{ID: "e3", Source: "BR", Target: "B"},
			{ID: "e4", Source: "A", Target: "Z"},
			{ID: "e5", Source: "B", Target: "Z"},
		},
	}
	if err := ValidateCanvas(c); err != nil {
		t.Fatalf("分支画布应合法: %v", err)
	}
	steps, err := CompileCanvas(c)
	if err != nil {
		t.Fatalf("CompileCanvas: %v", err)
	}
	// branch 剥离；A、B 两个业务 Steps
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %+v", steps)
	}
}

// TestValidateCanvasBranchSingleOut branch 仅一条出边报错。
func TestValidateCanvasBranchSingleOut(t *testing.T) {
	c := &Canvas{
		Nodes: []CanvasNode{
			{ID: "S", Type: StepStart},
			{ID: "BR", Type: StepBranch},
			{ID: "A", Type: StepLLM},
			{ID: "Z", Type: StepEnd},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "BR"},
			{ID: "e2", Source: "BR", Target: "A"},
			{ID: "e4", Source: "A", Target: "Z"},
		},
	}
	if err := ValidateCanvas(c); err == nil {
		t.Fatal("期望报错（分支仅一条出边），实际通过")
	}
}

// TestValidateCanvasTwoStarts 双 start 拦截。
func TestValidateCanvasTwoStarts(t *testing.T) {
	c := mkChain([]string{StepTemplate})
	c.Nodes = append(c.Nodes, CanvasNode{ID: "S2", Type: StepStart})
	if err := ValidateCanvas(c); err == nil {
		t.Fatal("期望报错（双开始节点），实际通过")
	}
}
