// knowledge 节点测试（Dify knowledge-retrieval 对齐，2026-10 融入）：
// KB 回调检索 → 变量池写回（{nodeID.output} 供下游引用）/ 未配置 KB 报错 /
// 未配置查询报错 / dry-run 仅渲染不调用 / KB 报错上抛。
package flow

import (
	"context"
	"strings"
	"testing"
)

// kbTestRunner 构造带假 KB 回调的运行器：query 含 "dify" 时命中 2 条，
// 否则命中 1 条（验证查询渲染与参数透传）。
func kbTestRunner(calls *[]string) *Runner {
	return &Runner{
		KB: func(ctx context.Context, query string, topK int, minScore float64) (string, int, error) {
			*calls = append(*calls, query)
			n := 1
			if strings.Contains(query, "dify") {
				n = 2
			}
			return "命中内容：" + query, n, nil
		},
	}
}

// mkKnowledgeCanvas 构造 S → K(knowledge) → A(template) → Z 画布，
// A 的模板引用 {K.output}（变量池跨节点引用）。
func mkKnowledgeCanvas(query string) *Canvas {
	return &Canvas{
		Nodes: []CanvasNode{
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart, Name: "开始"}},
			{ID: "K", Type: StepKnowledge, Step: Step{Type: StepKnowledge, Name: "知识库", Template: query, TopK: 5}},
			{ID: "A", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "结果：{K.output}"}},
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd, Name: "结束"}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "K"},
			{ID: "e2", Source: "K", Target: "A"},
			{ID: "e3", Source: "A", Target: "Z"},
		},
	}
}

// TestKnowledgeNode_RetrieveAndPool 知识库节点：查询渲染（{var}）→ KB 检索 →
// 输出写回变量池 → 下游模板以 {K.output} 引用命中内容。
func TestKnowledgeNode_RetrieveAndPool(t *testing.T) {
	var calls []string
	r := kbTestRunner(&calls)
	c := mkKnowledgeCanvas("什么是 {topic}？")
	res, err := r.RunGraph(testCtx(), &Flow{ID: "kb1"}, c, map[string]any{"topic": "dify"}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	// KB 回调收到渲染后的查询（{topic} → dify）。
	if len(calls) != 1 || !strings.Contains(calls[0], "dify") {
		t.Fatalf("KB 查询渲染错误: %v", calls)
	}
	// 下游模板引用 {K.output} 成功。
	if len(res.Steps) < 2 || !strings.Contains(res.Steps[1].Render, "命中内容") {
		t.Fatalf("下游未引用到知识库输出: %+v", res.Steps)
	}
	// 轨迹携带命中数。
	if res.Steps[0].Docs != 2 {
		t.Fatalf("命中数应为 2，实际 %d", res.Steps[0].Docs)
	}
}

// TestKnowledgeNode_NoKB 知识库节点未配置 KB 回调 → 明确报错（不静默）。
func TestKnowledgeNode_NoKB(t *testing.T) {
	c := &Canvas{
		Nodes: []CanvasNode{
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
			{ID: "K", Type: StepKnowledge, Step: Step{Type: StepKnowledge, Template: "q"}},
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
		},
		Edges: []CanvasEdge{
			{ID: "e1", Source: "S", Target: "K"},
			{ID: "e2", Source: "K", Target: "Z"},
		},
	}
	_, err := (&Runner{}).RunGraph(testCtx(), &Flow{ID: "kb2"}, c, map[string]any{}, true)
	if err == nil || !strings.Contains(err.Error(), "知识库回调") {
		t.Fatalf("未配置 KB 应报错，实际: %v", err)
	}
}

// TestKnowledgeNode_NoQuery 知识库节点查询语句为空 → 报错。
func TestKnowledgeNode_NoQuery(t *testing.T) {
	var calls []string
	r := kbTestRunner(&calls)
	c := mkKnowledgeCanvas("")
	_, err := r.RunGraph(testCtx(), &Flow{ID: "kb3"}, c, map[string]any{}, true)
	if err == nil || !strings.Contains(err.Error(), "查询语句") {
		t.Fatalf("空查询应报错，实际: %v", err)
	}
}

// TestKnowledgeNode_DryRun dry-run 仅渲染查询不调用 KB（检索外部库属副作用）。
func TestKnowledgeNode_DryRun(t *testing.T) {
	var calls []string
	r := kbTestRunner(&calls)
	c := mkKnowledgeCanvas("问题 {x}")
	res, err := r.RunGraph(testCtx(), &Flow{ID: "kb4"}, c, map[string]any{"x": "1"}, true)
	if err != nil {
		t.Fatalf("dry-run 应通过: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("dry-run 不应调用 KB，实际 %d 次", len(calls))
	}
	if len(res.Steps) == 0 || !strings.Contains(res.Steps[0].Render, "dry-run") {
		t.Fatalf("dry-run 轨迹应标注未调用: %+v", res.Steps)
	}
}

// TestKnowledgeNode_KBError KB 回调报错 → 上抛且轨迹记录。
func TestKnowledgeNode_KBError(t *testing.T) {
	r := &Runner{
		KB: func(ctx context.Context, query string, topK int, minScore float64) (string, int, error) {
			return "", 0, context.DeadlineExceeded
		},
	}
	c := mkKnowledgeCanvas("q")
	_, err := r.RunGraph(testCtx(), &Flow{ID: "kb5"}, c, map[string]any{}, false)
	if err == nil || !strings.Contains(err.Error(), "知识库检索失败") {
		t.Fatalf("KB 报错应上抛，实际: %v", err)
	}
}
