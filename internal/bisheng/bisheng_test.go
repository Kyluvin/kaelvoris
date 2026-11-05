// bisheng 包端到端测试：模拟蓝本 bisheng 官方测试场景（线性流、条件路由、
// input 中断回填、output choose 交互、fan-in 汇聚、循环熔断）。
package bisheng

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ---------- 测试基建 ----------

// mockLLM 脚本化 LLMInvoker（按 user 内容返回）。
type mockLLM struct {
	lastMsgs []LLMMessage
	reply    string
	err      error
}

func (m *mockLLM) Invoke(msgs []LLMMessage) (string, error) {
	m.lastMsgs = msgs
	if m.err != nil {
		return "", m.err
	}
	return m.reply, nil
}

// mockTools 脚本化 ToolExecutor。
type mockTools struct{ called []string }

func (m *mockTools) Has(name string) bool { return name != "" }
func (m *mockTools) Execute(name, args string) (string, error) {
	m.called = append(m.called, name+":"+args)
	return "tool-out:" + args, nil
}

// recorder 记录事件的 Callback。
type recorder struct {
	NoopCallback
	events []string
}

func (r *recorder) log(format string, a ...any) { r.events = append(r.events, fmt.Sprintf(format, a...)) }
func (r *recorder) OnNodeStart(d NodeStartData) { r.log("start:%s", d.NodeID) }
func (r *recorder) OnNodeEnd(d NodeEndData)     { r.log("end:%s(%s)", d.NodeID, d.Reason) }
func (r *recorder) OnUserInput(d UserInputData) { r.log("input_req:%s", d.NodeID) }
func (r *recorder) OnOutputMsg(d OutputMsgData) { r.log("msg:%s", d.Msg) }
func (r *recorder) OnOutputChoose(d OutputMsgChooseData) {
	r.log("choose_req:%s opts=%d", d.NodeID, len(d.Options))
}

// node 快捷构造节点配置。
func node(id, typ string, params map[string]any) BaseNodeData {
	g := NodeGroupParams{Params: nil}
	for k, v := range params {
		g.Params = append(g.Params, NodeParams{Key: k, Value: v})
	}
	return BaseNodeData{ID: id, Type: typ, Name: id, GroupParams: []NodeGroupParams{g}}
}

// edge 快捷构造边（sourceHandle 默认 right_handle）。
func edge(id, source, target string) EdgeBase {
	return EdgeBase{ID: id, Source: source, Target: target, SourceHandle: "right_handle"}
}

// mustRun 断言执行到 SUCCESS。
func mustRun(t *testing.T, w *Workflow) {
	t.Helper()
	st, reason := w.Run()
	if st != StatusSuccess {
		t.Fatalf("期望 SUCCESS，实得 %s（reason=%s）", st, reason)
	}
}

// ---------- 1. 线性流：start → llm → output(message) → end ----------

func TestLinearFlow(t *testing.T) {
	llm := &mockLLM{reply: "模型回答"}
	w, err := New("wf1", "线性流", WorkflowData{
		Nodes: []BaseNodeData{
			node("st", NodeStart, map[string]any{"preset_question": []any{}}),
			node("l1", NodeLLM, map[string]any{
				"system_prompt": "你是助手",
				"user_prompt":   "回答 {st.preset_question}",
				"output_user":   true,
			}),
			node("o1", NodeOutput, map[string]any{
				"output_result": map[string]any{"type": "message", "value": ""},
				"output_msg":    map[string]any{"msg": "结果: {l1.output}", "files": []any{}},
			}),
			node("e1", NodeEnd, nil),
		},
		Edges: []EdgeBase{
			edge("e11", "st", "l1"), edge("e12", "l1", "o1"), edge("e13", "o1", "e1"),
		},
	}, 20, 0, &recorder{}, Deps{LLM: llm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustRun(t, w)

	// llm 收到 system+user，preset_question 空渲染为空
	if len(llm.lastMsgs) != 2 || llm.lastMsgs[0].Role != "system" {
		t.Fatalf("LLM 消息异常: %+v", llm.lastMsgs)
	}
	// output 渲染了变量
	if v := w.Engine().State().GetVariable("o1", "message", 0); v == nil || !strings.Contains(renderValue(v), "模型回答") {
		t.Fatalf("output 未引用 llm 输出: %v", v)
	}
	// 会话历史：output_user 写入 AI 消息
	if len(w.Engine().State().HistoryList(10)) != 1 {
		t.Fatalf("会话历史应为 1 条（AI），实得 %d", len(w.Engine().State().HistoryList(10)))
	}
}

// ---------- 2. 条件路由：start → condition → (分支A | 分支B) → end ----------

func TestConditionRouting(t *testing.T) {
	// 走 tool 产出 score，条件 score >= 90 走 A 分支，否则 else
	tools := &mockTools{}
	for _, tc := range []struct {
		score    string
		expected string
	}{
		{"95", "a1"}, // equals 95 → A
		{"80", "b1"}, // else → B
	} {
		// 工具输出形如 tool-out:{"arg":"<score>"}，用 contains 匹配分支：
		// score=95 时左值含 "95" 命中 A；score=80 时条件（contains 95）恒假 → else B。
		condParams := map[string]any{
			"condition": []any{
				map[string]any{
					"id":       "left_handle",
					"operator": "and",
					"conditions": []any{
						map[string]any{
							"left_var":            "t1.output",
							"comparison_operation": CmpContains,
							"right_value_type":    "const",
							"right_value":         "95",
						},
					},
				},
				map[string]any{"id": "right_handle"}, // else
			},
		}
		w, err := New("wf2", "条件流", WorkflowData{
			Nodes: []BaseNodeData{
				node("st", NodeStart, nil),
				node("t1", NodeTool, map[string]any{"arg": tc.score}),
				node("c1", NodeCondition, condParams),
				node("a1", NodeOutput, map[string]any{
					"output_result": map[string]any{"type": "message", "value": ""},
					"output_msg":    map[string]any{"msg": "A分支", "files": []any{}},
				}),
				node("b1", NodeOutput, map[string]any{
					"output_result": map[string]any{"type": "message", "value": ""},
					"output_msg":    map[string]any{"msg": "B分支", "files": []any{}},
				}),
				node("e1", NodeEnd, nil),
			},
			Edges: []EdgeBase{
				edge("e1", "st", "t1"), edge("e2", "t1", "c1"),
				{ID: "e3", Source: "c1", Target: "a1", SourceHandle: "left_handle"},
				{ID: "e4", Source: "c1", Target: "b1", SourceHandle: "right_handle"},
				edge("e5", "a1", "e1"), edge("e6", "b1", "e1"),
			},
		}, 20, 0, &recorder{}, Deps{Tool: tools})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mustRun(t, w)
		if v := w.Engine().State().GetVariable(tc.expected, "message", 0); v == nil {
			t.Fatalf("score=%s 应路由到 %s", tc.score, tc.expected)
		}
		// 互斥分支汇合 end：只有一支被激活（message 变量仅一份产出）
		all := w.Engine().State().AllVariables()
		count := 0
		for k := range all {
			if strings.HasSuffix(k, ".message") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("互斥分支应只走一支，实得 %d 产出: %v", count, all)
		}
	}
}

// ---------- 3. input 中断回填：start → input → llm → end ----------

func TestInputInterrupt(t *testing.T) {
	llm := &mockLLM{reply: "已收到"}
	w, err := New("wf3", "输入流", WorkflowData{
		Nodes: []BaseNodeData{
			node("st", NodeStart, nil),
			node("in1", NodeInput, nil), // dialog 型：无表单字段
			node("l1", NodeLLM, map[string]any{
				"user_prompt": "用户说: {in1.user_input}",
			}),
			node("e1", NodeEnd, nil),
		},
		Edges: []EdgeBase{edge("e1", "st", "in1"), edge("e2", "in1", "l1"), edge("e3", "l1", "e1")},
	}, 20, 0, &recorder{}, Deps{LLM: llm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st, _ := w.Run()
	if st != StatusInput {
		t.Fatalf("期望中断 INPUT，实得 %s", st)
	}
	// 回填后续跑
	st, _ = w.Continue(map[string]map[string]any{"in1": {"user_input": "你好"}})
	if st != StatusSuccess {
		t.Fatalf("回填后期望 SUCCESS，实得 %s", st)
	}
	// llm prompt 已替换用户输入
	found := false
	for _, m := range llm.lastMsgs {
		if strings.Contains(m.Content, "用户说: 你好") {
			found = true
		}
	}
	if !found {
		t.Fatalf("LLM 未收到用户输入: %+v", llm.lastMsgs)
	}
	// 会话历史含 human 输入
	if h := w.Engine().State().HistoryList(10); len(h) == 0 || h[0].Role != "human" {
		t.Fatalf("会话历史应含 human 消息: %+v", h)
	}
}

// ---------- 4. output choose 交互：start → output(choose) → (x|y) ----------

func TestOutputChoose(t *testing.T) {
	w, err := New("wf4", "选择流", WorkflowData{
		Nodes: []BaseNodeData{
			node("st", NodeStart, nil),
			node("o1", NodeOutput, map[string]any{
				"output_result": map[string]any{"type": "choose", "value": ""},
				"output_msg":    map[string]any{"msg": "请选择", "files": []any{}},
			}),
			node("x1", NodeOutput, map[string]any{
				"output_result": map[string]any{"type": "message", "value": ""},
				"output_msg":    map[string]any{"msg": "X", "files": []any{}},
			}),
			node("y1", NodeOutput, map[string]any{
				"output_result": map[string]any{"type": "message", "value": ""},
				"output_msg":    map[string]any{"msg": "Y", "files": []any{}},
			}),
			node("e1", NodeEnd, nil),
		},
		Edges: []EdgeBase{
			edge("e1", "st", "o1"),
			{ID: "e2", Source: "o1", Target: "x1", SourceHandle: "opt_x"},
			{ID: "e3", Source: "o1", Target: "y1", SourceHandle: "opt_y"},
			edge("e4", "x1", "e1"), edge("e5", "y1", "e1"),
		},
	}, 20, 0, &recorder{}, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st, _ := w.Run()
	if st != StatusInput {
		t.Fatalf("期望 choose 中断，实得 %s", st)
	}
	// 回填选项值 = sourceHandle → 路由到 y1
	st, _ = w.Continue(map[string]map[string]any{"o1": {"output_result": "opt_y"}})
	if st != StatusSuccess {
		t.Fatalf("回填后期望 SUCCESS，实得 %s", st)
	}
	if v := w.Engine().State().GetVariable("y1", "message", 0); v != "Y" {
		t.Fatalf("应走 y 分支，实得 %v", v)
	}
}

// ---------- 5. 循环 + MaxSteps 熔断（蓝本 recursion_limit 语义）----------

func TestLoopMaxSteps(t *testing.T) {
	// 构造环：start → c1(condition) → 回 start ... 用 condition 恒 else 自环
	// 简化：start → c1 → c1（condition 节点 else 指向自己，MaxSteps=3 熔断）
	w, err := New("wf5", "循环流", WorkflowData{
		Nodes: []BaseNodeData{
			node("st", NodeStart, nil),
			node("c1", NodeCondition, map[string]any{
				"condition": []any{
					map[string]any{"id": "left_handle", "operator": "and", "conditions": []any{
						map[string]any{"left_var": "st.current_time", "comparison_operation": CmpEqual, "right_value_type": "const", "right_value": "never"},
					}},
					map[string]any{"id": "right_handle"}, // else 恒走
				},
			}),
			node("e1", NodeEnd, nil),
		},
		Edges: []EdgeBase{
			edge("e1", "st", "c1"),
			{ID: "e2", Source: "c1", Target: "c1", SourceHandle: "right_handle"}, // 自环
			{ID: "e3", Source: "c1", Target: "e1", SourceHandle: "left_handle"},
		},
	}, 3, 0, &recorder{}, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st, reason := w.Run()
	if st != StatusFailed || !strings.Contains(reason, "maximum number") {
		t.Fatalf("期望 MaxSteps 熔断 FAILED，实得 %s / %s", st, reason)
	}
}

// ---------- 6. fan-in 汇聚：start 分两支（tool×2）→ 汇合 llm ----------

func TestFanIn(t *testing.T) {
	llm := &mockLLM{reply: "合流"}
	tools := &mockTools{}
	w, err := New("wf6", "汇聚流", WorkflowData{
		Nodes: []BaseNodeData{
			node("st", NodeStart, nil),
			node("t1", NodeTool, map[string]any{"arg": "left"}),
			node("t2", NodeTool, map[string]any{"arg": "right"}),
			node("l1", NodeLLM, map[string]any{"user_prompt": "{t1.output} + {t2.output}"}),
			node("e1", NodeEnd, nil),
		},
		Edges: []EdgeBase{
			edge("e1", "st", "t1"), edge("e2", "st", "t2"),
			edge("e3", "t1", "l1"), edge("e4", "t2", "l1"),
			edge("e5", "l1", "e1"),
		},
	}, 20, 0, &recorder{}, Deps{LLM: llm, Tool: tools})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustRun(t, w)
	// llm 同时拿到两支输出（汇聚等待生效）
	prompt := llm.lastMsgs[len(llm.lastMsgs)-1].Content
	if !strings.Contains(prompt, "tool-out") || !strings.Contains(prompt, "left") || !strings.Contains(prompt, "right") {
		t.Fatalf("汇聚节点未拿到全部前驱输出: %s", prompt)
	}
}

// ---------- 7. 十三比较符语义（蓝本 compare_two_value 全覆盖）----------

func TestCompareValues(t *testing.T) {
	cases := []struct {
		op    string
		l, r  string
		want  bool
	}{
		{CmpEqual, "abc", "abc", true},
		{CmpNotEqual, "abc", "xyz", true},
		{CmpContains, "hello world", "lo w", true},
		{CmpNotContains, "abc", "zzz", true},
		{CmpIsEmpty, "", "", true},
		{CmpIsNotEmpty, "x", "", true},
		{CmpStartsWith, "prefix_x", "prefix", true},
		{CmpEndsWith, "x_suffix", "suffix", true},
		{CmpGreaterThan, "10", "9", true},
		{CmpGreaterThanOrEqual, "9", "9", true},
		{CmpLessThan, "3", "4", true},
		{CmpLessThanOrEqual, "4", "4", true},
		{CmpRegex, "user-123", `user-\d+`, true},
		{CmpRegex, "abc", `^\d+$`, false},
		{CmpGreaterThan, "abc", "9", false}, // 非数字比较 → false
	}
	for _, c := range cases {
		if got := compareValues(c.op, c.l, c.r); got != c.want {
			t.Errorf("compareValues(%s, %q, %q) = %v, want %v", c.op, c.l, c.r, got, c.want)
		}
	}
}

// ---------- 8. 模板变量（PromptTemplateParser 语义）----------

func TestTemplateVars(t *testing.T) {
	tpl := "你好 {user.name}，得分 {user.score#0}，常量 {{ignore}}"
	vars := ExtractTemplateVars(tpl)
	if len(vars) != 2 || vars[0] != "user.name" || vars[1] != "user.score#0" {
		t.Fatalf("变量提取异常: %v", vars)
	}
	out := FormatTemplate(tpl, map[string]any{
		"user.name":     "张三",
		"user.score#0":  "98",
		"user.missing":  "",
	})
	if !strings.Contains(out, "张三") || !strings.Contains(out, "98") || strings.Contains(out, "{user.name}") {
		t.Fatalf("替换异常: %s", out)
	}
}

// ---------- 9. 变量池点分/下标路径（GraphState 语义）----------

func TestStateVariablePaths(t *testing.T) {
	s := NewState()
	s.SetVariable("n1", "list", []any{"a", "b", "c"})
	s.SetVariable("n1", "preset_question", map[string]any{"q1": "答案"})
	if v := s.GetVariableByStr("n1.list#1", 0); v != "b" {
		t.Fatalf("下标取值异常: %v", v)
	}
	if v := s.GetVariableByStr("n1.list#9", 0); v != nil {
		t.Fatalf("越界应返回 nil: %v", v)
	}
	// preset_question 子键展开
	all := s.AllVariables()
	if all["n1.preset_question#q1"] != "答案" {
		t.Fatalf("子键展开缺失: %v", all)
	}
	// chat_history 窗口
	s.SaveContext("问", "human")
	s.SaveContext("答", "AI")
	if got := s.GetVariable("n1", "chat_history", 0); !strings.Contains(got.(string), "Human: 问") || !strings.Contains(got.(string), "AI: 答") {
		t.Fatalf("chat_history 异常: %q", got)
	}
}

// ---------- 10. 工具节点 JSON 参数协议 ----------

func TestToolNodeJSONProtocol(t *testing.T) {
	tools := &mockTools{}
	w, err := New("wf7", "工具流", WorkflowData{
		Nodes: []BaseNodeData{
			node("st", NodeStart, map[string]any{"preset_question": []any{map[string]any{"key": "k", "value": "v"}}}),
			node("t1", NodeTool, map[string]any{"query": "{st.preset_question}", "empty": ""}),
			node("e1", NodeEnd, nil),
		},
		Edges: []EdgeBase{edge("e1", "st", "t1"), edge("e2", "t1", "e1")},
	}, 20, 0, &recorder{}, Deps{Tool: tools})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustRun(t, w)
	if len(tools.called) != 1 {
		t.Fatalf("工具应被调用 1 次: %v", tools.called)
	}
		var args map[string]any
		body := tools.called[0]
		if idx := strings.Index(body, ":"); idx >= 0 {
			body = body[idx+1:]
		}
		if err := json.Unmarshal([]byte(body), &args); err != nil {
			t.Fatalf("参数非 JSON: %s", tools.called[0])
		}
		// preset_question 渲染为 JSON 串（等价蓝本 str(dict)），内嵌转义解析
		q, _ := args["query"].(string)
		var inner map[string]any
		if err := json.Unmarshal([]byte(q), &inner); err != nil {
			t.Fatalf("变量值非 JSON: %q", q)
		}
		if inner["k"] != "v" {
			t.Fatalf("变量值异常: %v", inner)
		}
	if _, has := args["empty"]; has {
		t.Fatalf("空参数应剔除: %v", args)
	}
}
