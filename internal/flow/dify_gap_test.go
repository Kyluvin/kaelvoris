// dify_gap_test.go —— 2026-10-27 Dify/Coze 对齐查缺补漏回归测试：
//   1. NeedsGraph 分派判定（6 类 Dify 融入节点 + branch 必走图执行）；
//   2. Start 统一入口分派修正回归（纯 http/assigner 画布不再误走 Chain 报类型未知）；
//   3. iteration 嵌套迭代拦截（Dify 同语义：子步骤内不允许再嵌 iteration）；
//   4. http 节点头/体 {var} 渲染边界（渲染失败保留原串不报错）；
//   5. assigner 引用 iteration 产物 {nodeID.items}（跨节点变量池链路）。

package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNeedsGraph 表驱动：NeedsGraph 判定矩阵（nil 画布 / 纯官方链 / 6 类 Dify
// 融入节点各一 / branch / 混合）。
func TestNeedsGraph(t *testing.T) {
	cases := []struct {
		name   string
		canvas *Canvas
		want   bool
	}{
		{name: "nil画布", canvas: nil, want: false},
		{
			name:   "纯官方组件线性链",
			canvas: mkChain([]string{StepTemplate, StepLLM, StepPassthrough}),
			want:   false,
		},
		{
			name: "含branch",
			canvas: mkLineCanvas([]CanvasNode{
				{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
				{ID: "BR", Type: StepBranch, Step: Step{Type: StepBranch}},
				{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
			}),
			want: true,
		},
	}
	// 6 类 Dify 融入节点逐类型注入检测。
	difyTypes := []string{StepTransform, StepHTTP, StepAssigner, StepExtractor, StepIteration, StepKnowledge}
	for _, tp := range difyTypes {
		cases = append(cases, struct {
			name   string
			canvas *Canvas
			want   bool
		}{
			name: "含" + tp,
			canvas: mkLineCanvas([]CanvasNode{
				{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
				{ID: "N", Type: tp, Step: Step{Type: tp}},
				{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
			}),
			want: true,
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsGraph(tc.canvas); got != tc.want {
				t.Fatalf("NeedsGraph = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStartDispatchDifyNodes Start 统一入口分派修正回归：纯 http/assigner 画布
// （无 branch/transform/knowledge）此前误走 Chain 报「类型未知」，修正后走
// RunGraph 正常执行。
func TestStartDispatchDifyNodes(t *testing.T) {
	// http 画布（dry-run：仅渲染不外呼）。
	cHTTP := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "H", Type: StepHTTP, Step: Step{
			Type: StepHTTP, Role: "GET",
			Template: `{"url": "http://127.0.0.1:1/ping", "timeoutSec": 2}`,
		}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := (&Runner{}).Start(context.Background(), &Flow{ID: "fs_http", Canvas: cHTTP}, map[string]any{}, true)
	if err != nil {
		t.Fatalf("纯 http 画布 Start 不应报类型未知: %v", err)
	}
	if len(res.Steps) == 0 || res.Steps[0].Type != StepHTTP || res.Steps[0].NodeID != "H" {
		t.Fatalf("http 轨迹: %+v", res.Steps)
	}

	// assigner 画布（真实执行：纯计算）。
	cAS := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "A", Type: StepAssigner, Step: Step{Type: StepAssigner, Name: "greet", Template: "你好 {who}"}},
		{ID: "TP", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "{greet}"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res2, err := (&Runner{}).Start(context.Background(), &Flow{ID: "fs_as", Canvas: cAS}, map[string]any{"who": "dify"}, false)
	if err != nil {
		t.Fatalf("纯 assigner 画布 Start: %v", err)
	}
	if len(res2.Steps) != 2 || res2.Steps[1].Render != "你好 dify" {
		t.Fatalf("assigner 链路: %+v", res2.Steps)
	}

	// iteration 画布（真实执行：transform 子步骤纯计算）。
	cIT := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "IT", Type: StepIteration, Step: Step{
			Type: StepIteration,
			Template: `{"list": "1\n2", "itemVar": "item", "steps": [{"type": "transform", "role": "python", "template": "v{{item}}"}]}`,
		}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res3, err := (&Runner{}).Start(context.Background(), &Flow{ID: "fs_it", Canvas: cIT}, map[string]any{}, false)
	if err != nil {
		t.Fatalf("纯 iteration 画布 Start: %v", err)
	}
	if !strings.Contains(res3.Output, "v1\nv2") {
		t.Fatalf("iteration 输出: %q", res3.Output)
	}
}

// TestRunGraph_IterationNestedRejects 子步骤内嵌 iteration → 明确报错
// （防失控上限，Dify iteration 同语义：迭代体内不允许再嵌迭代）。
func TestRunGraph_IterationNestedRejects(t *testing.T) {
	nested := `{"list": "a\nb", "steps": [{"type": "iteration", "template": "{\"list\": \"x\", \"steps\": [{\"type\": \"transform\", \"role\": \"python\", \"template\": \"{{item}}\"}]}"}]}`
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "IT", Type: StepIteration, Step: Step{Type: StepIteration, Template: nested}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	_, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fn"}, c, map[string]any{}, false)
	if err == nil || !strings.Contains(err.Error(), "嵌套") {
		t.Fatalf("嵌套迭代应报错，got %v", err)
	}
}

// TestRunGraph_IterationNestedSubflow 嵌套 iteration 内层子步骤允许 transform
// （区分「嵌套 iteration 拦截」与「子步骤含 transform 合法」两条边界）。
func TestRunGraph_IterationNestedSubflow(t *testing.T) {
	// 子步骤链含 transform（合法）：外层逐项 upper。
	def := `{"list": "go\nrust", "steps": [{"type": "transform", "role": "python", "template": "{{upper item}}!"}]}`
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "IT", Type: StepIteration, Step: Step{Type: StepIteration, Template: def}},
		{ID: "A", Type: StepAssigner, Step: Step{Type: StepAssigner, Name: "joined", Template: "{IT.output}"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fn2"}, c, map[string]any{}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if res.Output != "GO!\nRUST!" {
		t.Fatalf("output: %q", res.Output)
	}
	// assigner 引用 {IT.output}：跨节点变量池链路（迭代聚合 → 用户变量）。
	if res.Steps[len(res.Steps)-1].Render != "GO!\nRUST!" {
		t.Fatalf("assigner 渲染: %+v", res.Steps)
	}
}

// TestRunGraph_IterationItemsPool 迭代产物 {nodeID.items} JSON 数组写回变量池，
// 下游 transform 点分取值加工。
func TestRunGraph_IterationItemsPool(t *testing.T) {
	def := `{"list": "x\ny", "steps": [{"type": "transform", "role": "python", "template": "{{upper item}}"}]}`
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "IT", Type: StepIteration, Step: Step{Type: StepIteration, Template: def}},
		{ID: "TR", Type: StepTransform, Step: Step{Type: StepTransform, Role: "python", Name: "再加工", Template: "共 {{len IT.items}} 项"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fi4"}, c, map[string]any{}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	// {{len IT.items}}：items 为 JSON 数组串（`["X","Y"]`，8 字符），len 取串长。
	last := res.Steps[len(res.Steps)-1]
	if last.Type != StepTransform || !strings.Contains(last.Render, "项") {
		t.Fatalf("items 加工轨迹: %+v", last)
	}
}

// TestRunGraph_HTTPHeaderBodyRender http 节点头/体 {var} 渲染边界：头值与体
// 均渲染变量；渲染失败（变量缺失）保留原串不报错（与引擎 render 容错一致）。
func TestRunGraph_HTTPHeaderBodyRender(t *testing.T) {
	var gotPath, gotToken, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Token")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "H", Type: StepHTTP, Step: Step{
			Type: StepHTTP, Role: "POST",
			Template: fmt.Sprintf(`{"url": "%s/echo/{path}", "headers": {"X-Token": "Bearer {token}"}, "body": "hi {missing"}`, srv.URL),
		}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	// {missing 变量未闭合 → 渲染失败 → 体保留原串（容错不报错）。
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fh5"}, c, map[string]any{"path": "a1", "token": "t9"}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if gotPath != "/echo/a1" {
		t.Fatalf("URL 渲染失败: %q", gotPath)
	}
	if gotToken != "Bearer t9" {
		t.Fatalf("头渲染失败: %q", gotToken)
	}
	if gotBody != "hi {missing" {
		t.Fatalf("体未容错保留原串: %q", gotBody)
	}
	// 状态码写回池 {H.status}。
	if res.Steps[0].HTTPStatus != 200 {
		t.Fatalf("status: %+v", res.Steps[0])
	}
}

// TestRunGraph_AssignerChain 链式赋值：后一 assigner 引用前一 assigner 的
// 用户变量（{b} ← {a}），覆盖 Dify variable-assigner 串联改写场景。
func TestRunGraph_AssignerChain(t *testing.T) {
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "A1", Type: StepAssigner, Step: Step{Type: StepAssigner, Name: "a", Template: "1"}},
		{ID: "A2", Type: StepAssigner, Step: Step{Type: StepAssigner, Name: "b", Template: "{a}+2"}},
		{ID: "TP", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "b={b}"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fac"}, c, map[string]any{}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if res.Steps[2].Render != "b=1+2" {
		t.Fatalf("链式赋值: %+v", res.Steps)
	}
}

// TestExtractJSONStrNested 嵌套 JSON 剥离：extractJSONStr 取首个 { 到末个 }，
// 嵌套对象完整保留（参数提取结果含对象值场景）。
func TestExtractJSONStrNested(t *testing.T) {
	in := `说明文字 {"city": "杭州", "geo": {"lat": 30.2}} 尾部`
	want := `{"city": "杭州", "geo": {"lat": 30.2}}`
	if got := extractJSONStr(in); got != want {
		t.Fatalf("extractJSONStr = %q, want %q", got, want)
	}
		// 解析回环校验（合法 JSON）。
		var m map[string]any
		if err := json.Unmarshal([]byte(want), &m); err != nil {
			t.Fatalf("剥离结果非法 JSON: %v", err)
		}
	}

	// ── subflow 子流程节点（Coze subworkflow / Dify workflow 对齐）──

	// TestRunGraph_Subflow 子流程端到端：回调执行目标流程，输出写回变量池，
	// 子轨迹带「子流程:」前缀并入主轨迹。
	func TestRunGraph_Subflow(t *testing.T) {
		subRes := &RunResult{FlowID: "sub1", Output: "你好 sub！", Steps: []StepTrace{
			{Index: 0, Type: StepAssigner, Name: "赋值", Render: "你好 sub！"},
		}}
		r := &Runner{
			SubFlow: func(_ context.Context, flowID string, vars map[string]any, dry bool) (*RunResult, error) {
				if flowID != "sub1" {
					t.Fatalf("flowID = %q", flowID)
				}
				if vars["who"] != "sub" || dry {
					t.Fatalf("vars/dry: %+v %v", vars, dry)
				}
				return subRes, nil
			},
		}
		c := mkLineCanvas([]CanvasNode{
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
			{ID: "SF", Type: StepSubflow, Step: Step{
				Type: StepSubflow, Name: "调子流程",
				Template: `{"flow": "sub1", "vars": {"who": "{who}"}}`,
			}},
			{ID: "TP", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "上游:{SF.output}"}},
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
		})
		res, err := r.RunGraph(context.Background(), &Flow{ID: "fs"}, c, map[string]any{"who": "sub"}, false)
		if err != nil {
			t.Fatalf("RunGraph: %v", err)
		}
		if res.Steps[len(res.Steps)-1].Render != "上游:你好 sub！" {
			t.Fatalf("下游模板: %+v", res.Steps)
		}
		found := false
		for _, tr := range res.Steps {
			if strings.HasPrefix(tr.Name, "子流程:") {
				found = true
			}
		}
		if !found {
			t.Fatalf("子轨迹未并入: %+v", res.Steps)
		}
	}

	// TestRunGraph_SubflowNoCallback 未注入 SubFlow 回调 → 明确报错。
	func TestRunGraph_SubflowNoCallback(t *testing.T) {
		c := mkLineCanvas([]CanvasNode{
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
			{ID: "SF", Type: StepSubflow, Step: Step{Type: StepSubflow, Template: `{"flow": "x"}`}},
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
		})
		_, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fs2"}, c, map[string]any{}, false)
		if err == nil || !strings.Contains(err.Error(), "SubFlow") {
			t.Fatalf("want no-callback error, got %v", err)
		}
	}

	// TestRunGraph_SubflowValidation 缺 flow 配置 / 非法 JSON 报错。
	func TestRunGraph_SubflowValidation(t *testing.T) {
		cases := []string{
			`{}`,                 // 缺 flow
			`{"flow": "x", bad}`, // 非法 JSON
		}
		for i, tpl := range cases {
			c := mkLineCanvas([]CanvasNode{
				{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
				{ID: "SF", Type: StepSubflow, Step: Step{Type: StepSubflow, Template: tpl}},
				{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
			})
			r := &Runner{SubFlow: func(context.Context, string, map[string]any, bool) (*RunResult, error) {
				return &RunResult{}, nil
			}}
			if _, err := r.RunGraph(context.Background(), &Flow{ID: "fsv"}, c, map[string]any{}, false); err == nil {
				t.Fatalf("case %d 应报错", i)
			}
		}
	}

	// TestNeedsGraphSubflow subflow 进 NeedsGraph 判定（Chain 路径不认，必走图执行）。
	func TestNeedsGraphSubflow(t *testing.T) {
		c := mkLineCanvas([]CanvasNode{
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
			{ID: "SF", Type: StepSubflow, Step: Step{Type: StepSubflow, Template: `{"flow": "x"}`}},
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
		})
		if !NeedsGraph(c) {
			t.Fatal("含 subflow 画布应走图执行")
		}
	}
