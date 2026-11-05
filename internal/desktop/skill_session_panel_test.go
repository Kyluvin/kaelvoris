package desktop

// 会话技能面板单测：frontmatter params 解析 + 运行提示词组装。
// 会话技能挂载的持久化路径（Session.SessionSkills → SessionData）由
// internal/session 包内既有会话持久化测试模式覆盖（字段随 Snapshot/IntoSession
// 往返）；此处聚焦本文件新逻辑的纯函数测试。

import (
	"strings"
	"testing"

	"kaelvoris/internal/skills"
)

func TestParseSkillParams(t *testing.T) {
	raw := `---
name: deploy-check
description: 部署检查
modeSlugs:
  - code
params:
  - name: target
    label: 目标环境
    type: select
    required: true
    default: staging
    options: [staging, production]
    description: 部署目标环境
  - name: notes
    type: text
---

# 部署检查正文
params:
  - name: fake-in-body
`
	fields := parseSkillParams(raw)
	if len(fields) != 2 {
		t.Fatalf("期望 2 个参数声明，实得 %d: %+v", len(fields), fields)
	}
	p0 := fields[0]
	if p0.Name != "target" || p0.Label != "目标环境" || p0.Type != "select" ||
		!p0.Required || p0.Default != "staging" ||
		len(p0.Options) != 2 || p0.Options[1] != "production" {
		t.Fatalf("第一个参数解析不符: %+v", p0)
	}
	if p0.Description != "部署目标环境" {
		t.Fatalf("description 丢失: %+v", p0)
	}
	p1 := fields[1]
	if p1.Name != "notes" || p1.Type != "text" || p1.Label != "notes" {
		t.Fatalf("第二个参数解析不符: %+v", p1)
	}
}

func TestParseSkillParams_NoFrontmatter(t *testing.T) {
	if fields := parseSkillParams("普通正文，无 frontmatter"); len(fields) != 0 {
		t.Fatalf("无 frontmatter 应返回空，实得 %+v", fields)
	}
	if fields := parseSkillParams(""); len(fields) != 0 {
		t.Fatalf("空文本应返回空，实得 %+v", fields)
	}
}

func TestBuildSkillRunPrompt(t *testing.T) {
	fields := []SkillParamField{
		{Name: "target", Label: "目标环境", Default: "staging"},
		{Name: "notes", Label: "备注"},
	}
	// 场景 1：用户配置覆盖默认值。
	prompt := buildSkillRunPrompt(&skills.Content{Metadata: skills.Metadata{Name: "deploy-check", Description: "部署检查"}, Body: "第一步：检查服务健康。\n第二步：输出报告。"},
		map[string]string{"target": "production"}, fields)
	for _, want := range []string{"deploy-check", "目标环境 (target) = production", "备注 (notes) = （未配置）", "第一步：检查服务健康。"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("提示词缺 %q:\n%s", want, prompt)
		}
	}
	// 场景 2：无配置 → 默认值兜底。
	prompt2 := buildSkillRunPrompt(&skills.Content{Metadata: skills.Metadata{Name: "deploy-check", Description: "部署检查"}, Body: "x"}, nil, fields)
	if !strings.Contains(prompt2, "目标环境 (target) = staging") {
		t.Fatalf("默认值未兜底:\n%s", prompt2)
	}
	// 场景 3：无参数声明 → 不出参数段。
	prompt3 := buildSkillRunPrompt(&skills.Content{Metadata: skills.Metadata{Name: "deploy-check"}, Body: "x"}, nil, nil)
	if strings.Contains(prompt3, "[参数]") {
		t.Fatalf("无声明不应出参数段:\n%s", prompt3)
	}
}
