// 编排工具测试：TodoList、new_task、switch_mode、skill 特殊标记。
package tools

import (
	"context"
	"strings"
	"testing"
)

func TestUpdateTodoListTool(t *testing.T) {
	store := NewTodoStore()
	tool := &UpdateTodoListTool{Store: store}

	res, err := tool.Execute(context.Background(),
		`{"todos":[{"content":"step one","status":"completed"},{"content":"step two","status":"in_progress"}]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res, "2 items") {
		t.Errorf("result = %q", res)
	}
	items := store.Items()
	if len(items) != 2 || items[0].Status != "completed" || items[1].Status != "in_progress" {
		t.Errorf("store items = %+v", items)
	}

	// markdown 渲染。
	md := store.Markdown()
	if !strings.Contains(md, "x step one") || !strings.Contains(md, "- step two") {
		t.Errorf("markdown = %q", md)
	}

	// 非法 status 拒绝。
	if _, err := tool.Execute(context.Background(),
		`{"todos":[{"content":"a","status":"bogus"}]}`); err == nil {
		t.Error("expected error for invalid status")
	}
	// 空清单拒绝。
	if _, err := tool.Execute(context.Background(), `{"todos":[]}`); err == nil {
		t.Error("expected error for empty todos")
	}
}

// TestUpdateTodoListStringForms todos 误传字符串时宽容解析（2026-10-04 会话复盘：
// 历史样本 "cannot unmarshal string into Go struct" 类型错，严格解析零容错）。
func TestUpdateTodoListStringForms(t *testing.T) {
	store := NewTodoStore()
	tool := &UpdateTodoListTool{Store: store}

	// 1) 字符串化 JSON 数组。
	res, err := tool.Execute(context.Background(),
		`{"todos":"[{\"content\":\"a\",\"status\":\"pending\"}]"}`)
	if err != nil {
		t.Fatalf("stringified array should parse: %v", err)
	}
	if !strings.Contains(res, "1 items") {
		t.Errorf("result = %q", res)
	}

	// 2) markdown checklist 文本。
	res, err = tool.Execute(context.Background(),
		`{"todos":"- [x] done step\n- [ ] pending step\n- in progress plain"}`)
	if err != nil {
		t.Fatalf("markdown checklist should parse: %v", err)
	}
	items := store.Items()
	if len(items) != 3 || items[0].Status != "completed" || items[1].Status != "pending" || items[2].Status != "pending" {
		t.Errorf("markdown items = %+v", items)
	}
	if !strings.Contains(res, "3 items") {
		t.Errorf("result = %q", res)
	}

	// 3) 完全无法解析仍报错。
	if _, err := tool.Execute(context.Background(), `{"todos":"random text no list"}`); err == nil {
		t.Error("unparseable todos should error")
	}
}

func TestNewTaskTool(t *testing.T) {
	tool := &NewTaskTool{}
	_, err := tool.Execute(context.Background(),
		`{"mode":"code","message":"do the thing","todos":[{"content":"a","status":"pending"}]}`)
	var sub *SubtaskResult
	if err == nil {
		t.Fatal("expected SubtaskResult error marker")
	}
	sub, ok := err.(*SubtaskResult)
	if !ok {
		t.Fatalf("err type = %T, want *SubtaskResult", err)
	}
	if sub.Mode != "code" || sub.Message != "do the thing" || len(sub.Todos) != 1 {
		t.Errorf("subtask = %+v", sub)
	}

	// 缺参数拒绝。
	if _, err := tool.Execute(context.Background(), `{"mode":"code"}`); err == nil {
		t.Error("expected error for missing message")
	}
}

// TestNewTaskToolDescription 并行/串行描述分支（P2.8 fan-out 能力透出）：
// Parallel=true 明确告知可同轮多 new_task 并行；false 保持 mustBeLastTool 语义。
func TestNewTaskToolDescription(t *testing.T) {
	serial := (&NewTaskTool{}).Description()
	if !strings.Contains(serial, "MUST be called alone") {
		t.Error("serial description should keep must-be-alone semantics")
	}
	if strings.Contains(serial, "PARALLEL EXECUTION") {
		t.Error("serial description should not advertise parallel execution")
	}

	par := (&NewTaskTool{Parallel: true}).Description()
	for _, want := range []string{"PARALLEL EXECUTION", "MULTIPLE new_task tool calls", "wait_agent"} {
		if !strings.Contains(par, want) {
			t.Errorf("parallel description missing %q", want)
		}
	}
	if strings.Contains(par, "MUST be called alone") {
		t.Error("parallel description should not keep must-be-alone semantics")
	}
}

func TestSwitchModeTool(t *testing.T) {
	tool := &SwitchModeTool{}
	_, err := tool.Execute(context.Background(), `{"mode_slug":"architect","reason":"need a plan"}`)
	var sw *SwitchModeResult
	if err == nil {
		t.Fatal("expected SwitchModeResult error marker")
	}
	sw, ok := err.(*SwitchModeResult)
	if !ok {
		t.Fatalf("err type = %T, want *SwitchModeResult", err)
	}
	if sw.ModeSlug != "architect" || sw.Reason != "need a plan" {
		t.Errorf("switch = %+v", sw)
	}
}

func TestSkillTool(t *testing.T) {
	loaded := ""
	tool := &SkillTool{
		Loader: func(name string) (string, error) {
			if name == "commit-helper" {
				loaded = name
				return "SKILL BODY", nil
			}
			return "", errNotFound
		},
		Names: func() []string { return []string{"commit-helper"} },
	}

	res, err := tool.Execute(context.Background(), `{"skill":"commit-helper","args":"x"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res != "SKILL BODY" || loaded != "commit-helper" {
		t.Errorf("res=%q loaded=%q", res, loaded)
	}

	// 未知技能 → 错误列出可用技能。
	_, err = tool.Execute(context.Background(), `{"skill":"missing"}`)
	if err == nil || !strings.Contains(err.Error(), "commit-helper") {
		t.Errorf("error should list available skills, got %v", err)
	}
}

// errNotFound 技能不存在标记（由 skills.Manager 适配层返回）。
var errNotFound = errSkillNotFound{}

type errSkillNotFound struct{}

func (errSkillNotFound) Error() string { return "skill not found" }

// ---- D3：$ARGUMENTS 参数化 + 当轮授权挂点 ----

func TestSkillToolArgumentsPlaceholder(t *testing.T) {
	tool := &SkillTool{
		Loader: func(name string) (string, error) {
			if name == "param-skill" {
				return "Deploy to $ARGUMENTS now. Fallback: $ARGUMENTS.", nil
			}
			return "", errNotFound
		},
	}
	// 占位符替换：全部出现均替换，且不再尾部拼接参数（避免双份）
	res, err := tool.Execute(context.Background(), `{"skill":"param-skill","args":"prod"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res != "Deploy to prod now. Fallback: prod." || strings.Contains(res, "ARGUMENTS:") {
		t.Errorf("placeholder replace: %q", res)
	}
	// 正文无占位符：行为不变
	tool2 := &SkillTool{Loader: func(string) (string, error) { return "PLAIN BODY", nil }}
	res, err = tool2.Execute(context.Background(), `{"skill":"x","args":"prod"}`)
	if err != nil || res != "PLAIN BODY" {
		t.Errorf("no placeholder: res=%q err=%v", res, err)
	}
	// 无 args：行为不变（占位符保留原样）
	res, err = tool.Execute(context.Background(), `{"skill":"param-skill"}`)
	if err != nil || !strings.Contains(res, "$ARGUMENTS") {
		t.Errorf("no args: res=%q err=%v", res, err)
	}
}

func TestSkillToolAuthorize(t *testing.T) {
	var gotSkill string
	var gotPats []string
	tool := &SkillTool{
		Loader: func(string) (string, error) { return "BODY", nil },
		ToolsFor: func(skill string) []string {
			return []string{"shell", "Bash(git add *)"}
		},
		OnAuthorize: func(skill string, patterns []string) {
			gotSkill, gotPats = skill, patterns
		},
	}
	if _, err := tool.Execute(context.Background(), `{"skill":"deploy"}`); err != nil {
		t.Fatal(err)
	}
	if gotSkill != "deploy" || len(gotPats) != 2 || gotPats[1] != "Bash(git add *)" {
		t.Errorf("authorize: skill=%q pats=%v", gotSkill, gotPats)
	}
	// 未配置 ToolsFor：不触发回调
	tool2 := &SkillTool{Loader: func(string) (string, error) { return "B", nil }}
	called := false
	tool2.OnAuthorize = func(string, []string) { called = true }
	if _, err := tool2.Execute(context.Background(), `{"skill":"x"}`); err != nil || called {
		t.Errorf("no ToolsFor: called=%v err=%v", called, err)
	}
}
