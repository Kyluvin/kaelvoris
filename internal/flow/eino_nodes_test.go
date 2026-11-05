package flow

// eino_nodes_test.go — Eino 官方组件六节点逐项功能测试（Dify/Coze 对齐查缺补漏收口）：
//   tools / loader / transformer(split|join) / embedding / indexer / retriever
// 此前这 6 类节点仅有实现无直接测试证据，本轮补齐（链路径 Run + 图路径 RunGraph 双覆盖）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kaelvoris/internal/tools"
)

// stubTool 最小工具桩（实现 tools.Tool 接口，回显入参验证 {var} 渲染链路）。
type stubTool struct{ name string }

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "test stub" }
func (s *stubTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]any{}}
}
func (s *stubTool) Execute(ctx context.Context, args string) (string, error) {
	return "TOOL_RESULT:" + args, nil
}

// newToolsRunner 构造带工具注册中心的 Runner。
func newToolsRunner(names ...string) (*Runner, *tools.Registry) {
	reg := tools.NewRegistry()
	for _, n := range names {
		_ = reg.Register(&stubTool{name: n})
	}
	return &Runner{Tools: reg}, reg
}

// mkLinear 生成 start → nodes... → end 的单链画布（触发 RunGraph 图执行路径）。
func mkLinear(t *testing.T, steps ...Step) (*Flow, *Canvas) {
	t.Helper()
	c := &Canvas{Nodes: []CanvasNode{{ID: "S", Type: StepStart}, {ID: "E", Type: StepEnd}},
		Edges: []CanvasEdge{{ID: "e0", Source: "S", Target: "N0"}, {ID: "eN", Source: "N0", Target: "E"}}}
	for i, s := range steps {
		id := fmt.Sprintf("N%d", i)
		c.Nodes = append(c.Nodes, CanvasNode{ID: id, Type: s.Type, Step: s})
	}
	f := &Flow{ID: "f_eino", Name: "eino 六节点", Steps: stepsFromNodes(c)}
	return f, c
}

// stepsFromNodes 画布节点抽 Step 列表（Flow.Steps 保存校验用）。
func stepsFromNodes(c *Canvas) []Step {
	out := make([]Step, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		out = append(out, n.Step)
	}
	return out
}

// TestRunner_ToolNode 工具节点：注册中心取工具 + {var} 入参渲染 + 结果回填 tool 消息。
func TestRunner_ToolNode(t *testing.T) {
	steps := []Step{
		{Type: StepTemplate, Role: "user", Name: "ask", Template: "查 {kw}"},
		{Type: StepTool, Name: "stub_echo", Template: `{"q": "{kw}"}`},
	}
	runner, _ := newToolsRunner("stub_echo")
	f := &Flow{ID: "f_tool", Name: "工具链", Steps: steps}
	// dryRun=false：实际执行工具（dry-run 跳过 llm/tools，另由 TemplateRender 覆盖）。
	res, err := runner.Run(context.Background(), f, map[string]any{"kw": "天气"}, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Steps[1].Type != StepTool {
		t.Fatalf("step1 type = %s, want tools", res.Steps[1].Type)
	}
	if res.Steps[1].Render != `TOOL_RESULT:{"q": "天气"}` {
		t.Fatalf("tool render = %q, want {var} 渲染回显", res.Steps[1].Render)
	}

	// 未注册工具：报错带工具名（dryRun=false 实际执行；dry-run 跳过 llm/tools）。
	runner2, _ := newToolsRunner()
	f2 := &Flow{ID: "f_tool2", Name: "未注册", Steps: []Step{{Type: StepTool, Name: "nope"}}}
	if _, err := runner2.Run(context.Background(), f2, nil, false); err == nil || !strings.Contains(err.Error(), "工具未注册") {
		t.Fatalf("want 工具未注册 err, got %v", err)
	}

	// 未指定工具名。
	runner3, _ := newToolsRunner("stub_echo")
	f3 := &Flow{ID: "f_tool3", Name: "无名", Steps: []Step{{Type: StepTool}}}
	if _, err := runner3.Run(context.Background(), f3, nil, false); err == nil || !strings.Contains(err.Error(), "未指定工具名") {
		t.Fatalf("want 未指定工具名 err, got %v", err)
	}
}

// TestRunner_ToolNodeGraph 工具节点图路径：start→tool→end。
func TestRunner_ToolNodeGraph(t *testing.T) {
	runner, _ := newToolsRunner("stub_echo")
	f, c := mkLinear(t, Step{Type: StepTool, Name: "stub_echo", Template: "{x}"})
	if _, err := runner.RunGraph(context.Background(), f, c, map[string]any{"x": "图路径"}, true); err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
}

// TestRunner_LoaderNode loader 节点：读工作区文件为文档（相对路径拼接 + file:// 前缀）。
func TestRunner_LoaderNode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("第一段\n\n第二段"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Workspace: dir}
	steps := []Step{{Type: StepLoader, Name: "载入", Template: "doc.txt"}}
	f := &Flow{ID: "f_load", Name: "loader", Steps: steps}
	res, err := runner.Run(context.Background(), f, nil, true)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.Steps[0].Render, "载入 1 篇文档") {
		t.Fatalf("loader render = %q", res.Steps[0].Render)
	}

	// 不存在文件：报错。
	f2 := &Flow{ID: "f_load2", Name: "loader err", Steps: []Step{{Type: StepLoader, Template: "nope.txt"}}}
	if _, err := runner.Run(context.Background(), f2, nil, true); err == nil || !strings.Contains(err.Error(), "读取") {
		t.Fatalf("want 读取失败 err, got %v", err)
	}
}

// TestRunner_TransformerNode transformer 节点：split 空行拆分 / join 合并。
func TestRunner_TransformerNode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "split.txt"), []byte("甲段\n\n乙段\n\n丙段"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Workspace: dir}
	steps := []Step{
		{Type: StepLoader, Name: "载入", Template: "split.txt"},
		{Type: StepTransformer, Name: "拆分", Template: "split"},
	}
	f := &Flow{ID: "f_tr", Name: "transformer split", Steps: steps}
	res, err := runner.Run(context.Background(), f, nil, true)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.Steps[1].Render, "1 → 3 篇文档") {
		t.Fatalf("split render = %q, want 1 → 3", res.Steps[1].Render)
	}

	// join：合并回 1 篇。
	steps2 := []Step{
		{Type: StepLoader, Name: "载入", Template: "split.txt"},
		{Type: StepTransformer, Name: "拆分", Template: "split"},
		{Type: StepTransformer, Name: "合并", Template: "join"},
	}
	f2 := &Flow{ID: "f_tr2", Name: "transformer join", Steps: steps2}
	res2, err := runner.Run(context.Background(), f2, nil, true)
	if err != nil {
		t.Fatalf("run join: %v", err)
	}
	if !strings.Contains(res2.Steps[2].Render, "3 → 1 篇文档") {
		t.Fatalf("join render = %q, want 3 → 1", res2.Steps[2].Render)
	}
}

// TestRunner_EmbedderIndexerRetriever 向量链三节点端到端：
// loader → embedding → indexer → retriever，检索词直配文档文本（哈希词袋余弦应命中）。
func TestRunner_EmbedderIndexerRetriever(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kb.txt"),
		[]byte("kaelvoris is a go agent engine"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Workspace: dir}
	steps := []Step{
		{Type: StepLoader, Name: "载入", Template: "kb.txt"},
		{Type: StepEmbedding, Name: "向量化"},
		{Type: StepIndexer, Name: "索引"},
		{Type: StepRetriever, Name: "检索", TopK: 2, Template: "go agent engine"},
	}
	f := &Flow{ID: "f_vec", Name: "向量链", Steps: steps}
	res, err := runner.Run(context.Background(), f, nil, true)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// embedding：维度 256 断言（einoEmbedder dim=embedDim）。
	if !strings.Contains(res.Steps[1].Render, "dim=256") {
		t.Fatalf("embedding render = %q", res.Steps[1].Render)
	}
	// indexer：1 篇入索引。
	if !strings.Contains(res.Steps[2].Render, "已索引 1 篇") {
		t.Fatalf("indexer render = %q", res.Steps[2].Render)
	}
	// retriever：命中 1 篇（查询与文档词重合，余弦 > 0）。
	if !strings.Contains(res.Steps[3].Render, `检索 "go agent engine" → 1 篇`) {
		t.Fatalf("retriever render = %q", res.Steps[3].Render)
	}
}

// TestRunner_EmbeddingWithoutDocs 无文档时 embedding/indexer 报错语义。
func TestRunner_EmbeddingWithoutDocs(t *testing.T) {
	runner := &Runner{}
	cases := []Step{
		{Type: StepEmbedding, Name: "向量化"},
		{Type: StepIndexer, Name: "索引"},
	}
	for i, s := range cases {
		f := &Flow{ID: fmt.Sprintf("f_novec%d", i), Name: s.Name, Steps: []Step{s}}
		if _, err := runner.Run(context.Background(), f, nil, true); err == nil {
			t.Fatalf("case %d (%s): want 无文档 err", i, s.Name)
		}
	}
}

// TestRunner_RetrieverWithoutIndex 无索引时 retriever 报错语义。
func TestRunner_RetrieverWithoutIndex(t *testing.T) {
	runner := &Runner{}
	f := &Flow{ID: "f_noix", Name: "检索", Steps: []Step{{Type: StepRetriever, Template: "q"}}}
	if _, err := runner.Run(context.Background(), f, nil, true); err == nil || !strings.Contains(err.Error(), "无可用索引") {
		t.Fatalf("want 无可用索引 err, got %v", err)
	}
}
