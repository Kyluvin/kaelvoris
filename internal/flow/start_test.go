// start_test.go —— Runner.Start 统一启动入口分派语义（cron run_flow 与 UI
// RunFlow 共用）：纯线性画布走 Eino Chain，branch 画布走图执行。

package flow

import (
	"context"
	"testing"
)

// TestStartDispatchChain 纯线性画布 → Chain 路径（模板渲染语义验证）。
func TestStartDispatchChain(t *testing.T) {
	f := &Flow{ID: "fs1", Steps: []Step{
		{Type: StepTemplate, Role: "user", Template: "你好 {name}"},
	}}
	res, err := (&Runner{}).Start(context.Background(), f, map[string]any{"name": "入口"}, true)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(res.Steps) != 1 || res.Steps[0].Render != "你好 入口" {
		t.Fatalf("Chain 路径渲染不符: %+v", res.Steps)
	}
}

// TestStartDispatchGraph branch 画布 → 图执行路径（条件路由验证，dryRun 跳过 llm）。
func TestStartDispatchGraph(t *testing.T) {
	c := mkBranchCond("{x} > 10")
	f := &Flow{ID: "fs2", Canvas: c}
	res, err := (&Runner{}).Start(context.Background(), f, map[string]any{"x": 99}, true)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(res.Steps) != 1 || res.Steps[0].NodeID != "B" {
		t.Fatalf("应走图执行分支B且轨迹携 NodeID: %+v", res.Steps)
	}
}
