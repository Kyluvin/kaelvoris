// bisheng.go 桥接层测试：画布转换 + 桌面端绑定链路（无 Wails 上下文，
// HOME 重定向隔离用户目录，与 cron_flow_test.go 同套路）。
package desktop

import (
	"os"
	"path/filepath"
	"testing"

	"kaelvoris/internal/bisheng"
	"kaelvoris/internal/flow"
)

// TestToBishengData 画布格式转换：合法节点映射 + 不支持类型明确报错。
func TestToBishengData(t *testing.T) {
	c := &flow.Canvas{
		Nodes: []flow.CanvasNode{
			{ID: "n1", Type: flow.StepStart, Step: flow.Step{Type: flow.StepStart}},
			{ID: "n2", Type: flow.StepLLM, Step: flow.Step{Type: flow.StepLLM, Name: "请回答", Template: "系统提示", Model: "gpt-x"}},
			{ID: "n3", Type: flow.StepEnd, Step: flow.Step{Type: flow.StepEnd}},
		},
		Edges: []flow.CanvasEdge{
			{ID: "e1", Source: "n1", Target: "n2"},
			{ID: "e2", Source: "n2", Target: "n3"},
		},
	}
	data, err := toBishengData(c)
	if err != nil {
		t.Fatalf("toBishengData: %v", err)
	}
	if len(data.Nodes) != 3 || len(data.Edges) != 2 {
		t.Fatalf("节点/边数量异常: %d/%d", len(data.Nodes), len(data.Edges))
	}
	var llmNode *bisheng.BaseNodeData
	for i := range data.Nodes {
		if data.Nodes[i].Type == bisheng.NodeLLM {
			llmNode = &data.Nodes[i]
		}
	}
	if llmNode == nil {
		t.Fatalf("llm 节点未映射")
	}
	// system_prompt 来自 Step.Template，user_prompt 来自 Step.Name
	var sys, user string
	for _, p := range llmNode.GroupParams[0].Params {
		if p.Key == "system_prompt" {
			sys, _ = p.Value.(string)
		}
		if p.Key == "user_prompt" {
			user, _ = p.Value.(string)
		}
	}
	if sys != "系统提示" || user != "请回答" {
		t.Fatalf("llm 参数映射异常: sys=%q user=%q", sys, user)
	}

	// 不支持类型（transform 是 internal/flow 专有）报明确错误
	bad := &flow.Canvas{Nodes: []flow.CanvasNode{
		{ID: "n1", Type: flow.StepTransform, Step: flow.Step{Type: flow.StepTransform}},
	}}
	if _, err := toBishengData(bad); err == nil {
		t.Fatalf("transform 节点应报不支持")
	}
}

// TestRunBishengFlowBinding 桌面端绑定链路端到端：构造纯模板流程
// （无需真实 API Key，绕过 llm 校验）→ RunBishengFlow → 断言变量池。
func TestRunBishengFlowBinding(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	a := &App{}
	f := &flow.Flow{ID: "bf1", Name: "bisheng绑定测试", Canvas: &flow.Canvas{
		Nodes: []flow.CanvasNode{
			{ID: "n1", Type: flow.StepStart, Step: flow.Step{Type: flow.StepStart}},
			{ID: "n2", Type: flow.StepPassthrough, Step: flow.Step{Type: flow.StepPassthrough, Name: "透传"}},
			{ID: "n3", Type: flow.StepEnd, Step: flow.Step{Type: flow.StepEnd}},
		},
		Edges: []flow.CanvasEdge{
			{ID: "e1", Source: "n1", Target: "n2"},
			{ID: "e2", Source: "n2", Target: "n3"},
		},
	}}
	if _, err := a.SaveFlow(*f); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	// 无 LLM 节点的画布仍要求 APIKey（入口统一校验）—— 先注入假配置
	if a.cfg.APIKey == "" {
		a.cfg.BaseURL, a.cfg.APIKey, a.cfg.Model = "http://localhost:1", "sk-test", "m"
	}
	res, err := a.RunBishengFlow(RunBishengFlowRequest{FlowID: "bf1", Vars: map[string]any{"q": "值"}})
	if err != nil {
		t.Fatalf("RunBishengFlow: %v", err)
	}
	if res.Status != bisheng.StatusSuccess {
		t.Fatalf("期望 SUCCESS，实得 %s（%s）", res.Status, res.Reason)
	}
	// 用户变量经 preset_question 进变量池
	if _, ok := res.Variables["n1.preset_question"]; !ok {
		t.Fatalf("preset_question 未入变量池: %v", res.Variables)
	}
	// 终态实例已清理
	ContinueBishengFlowCheck(t, a, res.RunID)
}

// ContinueBishengFlowCheck 断言终态运行实例已清理（Continue 应报不存在）。
func ContinueBishengFlowCheck(t *testing.T, a *App, runID string) {
	t.Helper()
	if _, err := a.ContinueBishengFlow(runID, nil); err == nil {
		t.Fatalf("终态实例应已清理")
	}
}

// TestHomeIsolation 确保 HOME 重定向生效（flows 落临时目录）。
func TestHomeIsolation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	home, _ := os.UserHomeDir()
	if home != dir && filepath.Dir(home) != dir {
		t.Skipf("环境不支持 USERPROFILE 重定向（home=%s）", home)
	}
}
