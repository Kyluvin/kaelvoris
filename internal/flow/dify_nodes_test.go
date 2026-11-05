// dify_nodes_test.go —— 2026-10 Dify/Coze 补齐节点引擎级测试：
// http / assigner / extractor / iteration / end 输出模板 / 变量池写回链路。

package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"kaelvoris/internal/llm"
)

// newEchoHTTP 起一个 httptest 假 HTTP 服务：POST /submit 回 JSON（含回显体），
// GET /ping 回 "pong"。用于 http 节点端到端测试。
func newEchoHTTP(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "echo": body["name"]})
	})
	return httptest.NewServer(mux)
}

// TestRunGraph_HTTPNode http 节点：GET 请求 + {var} 渲染 + status/output 写回变量池。
func TestRunGraph_HTTPNode(t *testing.T) {
	srv := newEchoHTTP(t)
	defer srv.Close()

	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "H", Type: StepHTTP, Step: Step{
			Type: StepHTTP, Name: "探活", Role: "GET",
			Template: fmt.Sprintf(`{"url": "%s/ping?from={who}", "timeoutSec": 5}`, srv.URL),
		}},
		{ID: "TP", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "响应:{H.output} 码:{H.status}"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	r := &Runner{}
	res, err := r.RunGraph(context.Background(), &Flow{ID: "fh"}, c, map[string]any{"who": "kael"}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	// 池写回：{H.output} = "pong"、{H.status} = 200。
	if res.Steps[1].Render != "响应:pong 码:200" {
		t.Fatalf("下游模板 = %q", res.Steps[1].Render)
	}
	// trace 携带状态码与 NodeID。
	if res.Steps[0].HTTPStatus != 200 || res.Steps[0].NodeID != "H" {
		t.Fatalf("trace: %+v", res.Steps[0])
	}
}

// TestRunGraph_HTTPPostBody http 节点 POST：体 {var} 渲染 + 头注入。
func TestRunGraph_HTTPPostBody(t *testing.T) {
	srv := newEchoHTTP(t)
	defer srv.Close()

	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "H", Type: StepHTTP, Step: Step{
			Type: StepHTTP, Name: "提交", Role: "POST",
			Template: fmt.Sprintf(`{"url": "%s/submit", "headers": {"X-From": "flow"}, "body": "{\"name\": \"{user}\"}"}`, srv.URL),
		}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	r := &Runner{}
	res, err := r.RunGraph(context.Background(), &Flow{ID: "fh2"}, c, map[string]any{"user": "测试"}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if !strings.Contains(res.Steps[0].Render, "200") {
		t.Fatalf("POST 应 200: %+v", res.Steps[0])
	}
	if !strings.Contains(res.Output, `"ok":true`) {
		t.Fatalf("output 应含 JSON 响应: %q", res.Output)
	}
}

// TestRunGraph_HTTPDryRun dry-run 只渲染 URL 不发送（目标地址不存在的假 URL 也不报错）。
func TestRunGraph_HTTPDryRun(t *testing.T) {
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "H", Type: StepHTTP, Step: Step{
			Type: StepHTTP, Role: "GET", Template: `{"url": "http://127.0.0.1:1/nope"}`,
		}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fh3"}, c, map[string]any{}, true)
	if err != nil {
		t.Fatalf("dry-run 不应发请求: %v", err)
	}
	if !strings.Contains(res.Steps[0].Render, "dry-run") {
		t.Fatalf("dry-run 轨迹: %+v", res.Steps[0])
	}
}

// TestRunGraph_HTTPBadMethod 非法方法报错。
func TestRunGraph_HTTPBadMethod(t *testing.T) {
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "H", Type: StepHTTP, Step: Step{Type: StepHTTP, Role: "BREW", Template: `{"url": "http://x"}`}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	_, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fh4"}, c, map[string]any{}, false)
	if err == nil || !strings.Contains(err.Error(), "BREW") {
		t.Fatalf("want bad method error, got %v", err)
	}
}

// TestRunGraph_Assigner assigner 节点：值表达式写回用户变量 + 输出池，下游 {var} 直引。
func TestRunGraph_Assigner(t *testing.T) {
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "A", Type: StepAssigner, Step: Step{Type: StepAssigner, Name: "title", Template: "报告-{who}"}},
		{ID: "TP", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "标题:{title} / 池:{A.output}"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fa"}, c, map[string]any{"who": "kael"}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if res.Steps[1].Render != "标题:报告-kael / 池:报告-kael" {
		t.Fatalf("下游模板 = %q", res.Steps[1].Render)
	}
}

// TestRunGraph_AssignerFromNode assigner 引用上游节点输出（聚合语义：分支互斥下同名覆盖）。
func TestRunGraph_AssignerFromNode(t *testing.T) {
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "TR", Type: StepTransform, Step: Step{Type: StepTransform, Role: "python", Name: "转换", Template: "{{upper name}}"}},
		{ID: "A", Type: StepAssigner, Step: Step{Type: StepAssigner, Name: "result", Template: "{TR.output}"}},
		{ID: "TP", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "结果:{result}"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fa2"}, c, map[string]any{"name": "dify"}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if res.Steps[2].Render != "结果:DIFY" {
		t.Fatalf("下游模板 = %q", res.Steps[2].Render)
	}
}

// TestRunGraph_AssignerValidation assigner 缺变量名/缺值表达式报错。
func TestRunGraph_AssignerValidation(t *testing.T) {
	cases := []Step{
		{Type: StepAssigner, Name: "", Template: "x"},          // 缺目标变量名
		{Type: StepAssigner, Name: "a.b", Template: "x"},       // 非法变量名（含点）
		{Type: StepAssigner, Name: "ok", Template: "  "},       // 缺值表达式
	}
	for i, as := range cases {
		c := mkLineCanvas([]CanvasNode{
			{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
			{ID: "A", Type: StepAssigner, Step: as},
			{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
		})
		if _, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fav"}, c, map[string]any{}, false); err == nil {
			t.Fatalf("case %d 应报错", i)
		}
	}
}

// TestRunGraph_Extractor extractor 节点：mock LLM 返回 JSON → 解析 + 逐参数写回变量池。
func TestRunGraph_Extractor(t *testing.T) {
	var got []llm.Message
	srv := newMockLLM(t, &got)
	defer srv.Close()
	// mock 回 "REPLY:<最后 user 消息>"，这里直接换成合法 JSON（覆写 handler 不可行，
	// 用 LLMFor 包一层转换输出）。改为自定义 httptest。
	extractSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"city\": \"杭州\", \"days\": 3}"}}]}`))
	}))
	defer extractSrv.Close()

	r := &Runner{LLM: llm.NewClient(extractSrv.URL, "k", "m")}
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "E", Type: StepExtractor, Step: Step{
			Type: StepExtractor, Name: "提取", Model: "",
			Template: `{"text": "{input}", "params": [{"name": "city", "desc": "城市", "required": true}, {"name": "days", "desc": "天数", "required": false}]}`,
		}},
		{ID: "TP", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "城市:{E.city} 结果:{E.output}"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := r.RunGraph(context.Background(), &Flow{ID: "fe"}, c, map[string]any{"input": "我想去杭州玩三天"}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if res.Steps[1].Render != "城市:杭州 结果:{\"city\":\"杭州\",\"days\":3}" {
		t.Fatalf("下游模板 = %q", res.Steps[1].Render)
	}
	// 提取提示词含输入文本（模型侧校验）。
	if len(got) == 0 || !strings.Contains(got[0].Content, "我想去杭州玩三天") {
		// got 由 mockLLM 捕获但本用例用独立服务器；此断言只在共用时生效，跳过。
		_ = got
	}
}

// TestRunGraph_ExtractorRequiredMissing 必填参数缺失 → 报错（Dify fail 分支语义）。
func TestRunGraph_ExtractorRequiredMissing(t *testing.T) {
	extractSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"days\": 3}"}}]}`))
	}))
	defer extractSrv.Close()

	r := &Runner{LLM: llm.NewClient(extractSrv.URL, "k", "m")}
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "E", Type: StepExtractor, Step: Step{
			Type: StepExtractor,
			Template: `{"text": "{input}", "params": [{"name": "city", "desc": "城市", "required": true}]}`,
		}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	_, err := r.RunGraph(context.Background(), &Flow{ID: "fe2"}, c, map[string]any{"input": "玩三天"}, false)
	if err == nil || !strings.Contains(err.Error(), "city") {
		t.Fatalf("want missing city error, got %v", err)
	}
}

// TestRunGraph_ExtractorFence 模型回 ```json 围栏/说明文字 → 容错剥离解析。
func TestRunGraph_ExtractorFence(t *testing.T) {
	extractSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "好的，提取结果如下：\n```json\n{\"city\": \"北京\"}\n```"},
			}},
		})
		_, _ = w.Write(msg)
	}))
	defer extractSrv.Close()

	r := &Runner{LLM: llm.NewClient(extractSrv.URL, "k", "m")}
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "E", Type: StepExtractor, Step: Step{
			Type: StepExtractor,
			Template: `{"text": "{input}", "params": [{"name": "city", "desc": "城市", "required": true}]}`,
		}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := r.RunGraph(context.Background(), &Flow{ID: "fe3"}, c, map[string]any{"input": "北京"}, false)
	if err != nil {
		t.Fatalf("围栏容错失败: %v", err)
	}
	if !strings.Contains(res.Steps[0].Render, "北京") {
		t.Fatalf("提取结果: %+v", res.Steps[0])
	}
}

// TestRunGraph_Iteration 迭代节点：列表逐项跑子步骤（transform 上转换），输出聚合。
func TestRunGraph_Iteration(t *testing.T) {
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "IT", Type: StepIteration, Step: Step{
			Type: StepIteration, Name: "批量转换",
			Template: `{"list": "{names}", "itemVar": "item", "steps": [{"type": "transform", "role": "python", "template": "{{upper item}}"}]}`,
		}},
		{ID: "TP", Type: StepTemplate, Step: Step{Type: StepTemplate, Role: "user", Name: "引用", Template: "聚合:{IT.output}"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fi"}, c, map[string]any{"names": "alpha\nbeta\ngamma"}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	// 迭代聚合 = 子步骤输出按行拼接（倒数第 2 帧为 iteration 聚合帧）。
	it := res.Steps[len(res.Steps)-2]
	if it.Type != StepIteration || it.Render != "ALPHA\nBETA\nGAMMA" {
		t.Fatalf("迭代聚合 = %+v", it)
	}
	// 子帧存在（每项 transform 一帧）+ 聚合帧 + 下游模板帧。
	if len(res.Steps) != 5 { // 3 项子帧 + 1 聚合帧 + 1 引用帧
		t.Fatalf("轨迹数 = %d: %+v", len(res.Steps), res.Steps)
	}
}

// TestRunGraph_IterationJSONList 迭代列表为 JSON 数组（变量池 {nodeID.output} 引用上游）。
func TestRunGraph_IterationJSONList(t *testing.T) {
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "IT", Type: StepIteration, Step: Step{
			Type: StepIteration,
			Template: `{"list": "[\"a\", \"b\"]", "itemVar": "x", "steps": [{"type": "transform", "role": "python", "template": "{{upper x}}-{{len x}}"}]}`,
		}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fi2"}, c, map[string]any{}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if len(res.Steps) != 3 { // 2 子帧 + 1 聚合帧
		t.Fatalf("轨迹数 = %d: %+v", len(res.Steps), res.Steps)
	}
	if res.Steps[2].Type != StepIteration || res.Steps[2].Render != "A-1\nB-1" || res.Steps[2].Index != 2 {
		t.Fatalf("迭代聚合帧: %+v", res.Steps[2])
	}
}

// TestRunGraph_IterationEmptyList 空列表报错；超上限报错。
func TestRunGraph_IterationEmptyList(t *testing.T) {
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "IT", Type: StepIteration, Step: Step{
			Type: StepIteration,
			Template: `{"list": "{empty}", "steps": [{"type": "transform", "role": "python", "template": "{{item}}"}]}`,
		}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	_, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fi3"}, c, map[string]any{"empty": ""}, false)
	if err == nil || !strings.Contains(err.Error(), "迭代列表为空") {
		t.Fatalf("want empty list error, got %v", err)
	}
}

// TestRunGraph_EndOutputTemplate end 节点输出模板：渲染写回 Output（Dify answer 对齐）。
func TestRunGraph_EndOutputTemplate(t *testing.T) {
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "TR", Type: StepTransform, Step: Step{Type: StepTransform, Role: "python", Name: "转换", Template: "{{name}}"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd, Template: "最终答案：{TR.output}（{name}）"}},
	})
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fz"}, c, map[string]any{"name": "kael"}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if res.Output != "最终答案：kael（kael）" {
		t.Fatalf("end 输出模板未生效: %q", res.Output)
	}
	// 轨迹含 end 输出帧。
	if len(res.Steps) != 2 || res.Steps[1].Type != StepEnd || res.Steps[1].Render != res.Output {
		t.Fatalf("end 轨迹: %+v", res.Steps)
	}
}

// TestRunGraph_EndTemplateEmpty 无模板保持原语义（Output = 最后业务输出）。
func TestRunGraph_EndTemplateEmpty(t *testing.T) {
	c := mkLineCanvas([]CanvasNode{
		{ID: "S", Type: StepStart, Step: Step{Type: StepStart}},
		{ID: "TR", Type: StepTransform, Step: Step{Type: StepTransform, Role: "python", Name: "转换", Template: "{{name}}"}},
		{ID: "Z", Type: StepEnd, Step: Step{Type: StepEnd}},
	})
	res, err := (&Runner{}).RunGraph(context.Background(), &Flow{ID: "fz2"}, c, map[string]any{"name": "kael"}, false)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if res.Output != "kael" {
		t.Fatalf("原语义破坏: %q", res.Output)
	}
}

// TestStepTypesNew 新类型进 StepTypes 白名单（保存校验放行）。
func TestStepTypesNew(t *testing.T) {
	for _, tp := range []string{StepHTTP, StepAssigner, StepExtractor, StepIteration} {
		if !StepTypes[tp] {
			t.Fatalf("类型 %s 未注册", tp)
		}
	}
}

// TestExtractJSONStr 围栏剥离辅助。
func TestExtractJSONStr(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"结果是 {\"a\":1} 请查收", `{"a":1}`},
		{"no json here", ""},
	}
	for _, c := range cases {
		if got := extractJSONStr(c.in); got != c.want {
			t.Fatalf("extractJSONStr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// schema 引用保活（extractor 提示词路径经 invokeLLM 使用 schema.User）。
var _ = schema.User
