// skill_ai_categorize_test.go —— 技能 AI 分类（skill_ai_categorize.go）单测。
package desktop

import (
	"strings"
	"testing"

	"kaelvoris/internal/skills"
)

// TestParseAICatGroups 正常解析（含解释文本与代码围栏容错）。
func TestParseAICatGroups(t *testing.T) {
	in := "```json\n{\"groups\":[{\"name\":\"前端开发\",\"skills\":[\"vue\",\"react\"]},{\"name\":\"文档写作\",\"skills\":[\"doc\"]}]}\n```"
	out := parseAICatGroups(in)
	if len(out.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(out.Groups))
	}
	if out.Groups[0].Name != "前端开发" || len(out.Groups[0].SkillNames) != 2 {
		t.Fatalf("group0 = %+v", out.Groups[0])
	}
	if out.Groups[1].Name != "文档写作" || out.Groups[1].SkillNames[0] != "doc" {
		t.Fatalf("group1 = %+v", out.Groups[1])
	}
}

// TestParseAICatGroups_Invalid 垃圾输出返回零值（Groups 空 → 调用方报错跳过）。
func TestParseAICatGroups_Invalid(t *testing.T) {
	for _, in := range []string{"", "垃圾文本无 JSON", "{ broken"} {
		if out := parseAICatGroups(in); len(out.Groups) != 0 {
			t.Fatalf("输入 %q 应解析出 0 组, got %+v", in, out.Groups)
		}
	}
}

// TestAICatGroupsPrompt 提示词应包含技能清单与既有分类段（技能名/描述/已有分类均需出现）。
func TestAICatGroupsPrompt(t *testing.T) {
	items := []skills.Metadata{
		{Name: "vue-pro", Description: "Vue 组件生成"},
		{Name: "deploy", Description: "部署流水线"},
	}
	p := aiCatGroupsPrompt(items, []string{"开发"})
	for _, want := range []string{"vue-pro", "deploy", "Vue 组件生成", "已有分类", "开发"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt 缺少 %q", want)
		}
	}
}
