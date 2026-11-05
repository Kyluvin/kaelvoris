package desktop

// roadmap_panel_test.go 自主迭代规划板（roadmap）面板层单测：
// 覆盖移动端「AR 自主开发」反馈修复的两条防线 ——
//  1. parseRoadmapPlan：模型输出带代码围栏/解释前缀时仍可解析（规划收敛依赖）；
//  2. SetSession 部分更新：开发会话 ID 落库不触碰其他字段（移动端点开发卡回看会话）。

import (
	"strings"
	"testing"

	"kaelvoris/internal/roadmap"
	"kaelvoris/pkg/message"
)

func TestParseRoadmapPlan(t *testing.T) {
	// 带代码围栏 + 解释前缀（真实模型输出形态）。
	raw := "好的，以下是规划：\n```json\n{\"items\":[{\"title\":\"修复移动端列表\",\"detail\":\"虚拟列表优化\"," +
		"\"priority\":\"high\",\"category\":\"bug\"}],\"summary\":\"现状良好\"}\n```"
	out := parseRoadmapPlan(raw)
	if len(out.Items) != 1 || out.Items[0].Title != "修复移动端列表" || out.Summary != "现状良好" {
		t.Fatalf("围栏/前缀输出应可解析: %+v", out)
	}
	// 空输入 / 无 JSON / 空数组（空数组 = 规划成功但 0 提案，前端 rmApply 收敛复位依赖此语义）。
	if got := len(parseRoadmapPlan("").Items); got != 0 {
		t.Fatalf("空输入应得 0 条: %d", got)
	}
	if got := len(parseRoadmapPlan("模型拒答，无 JSON").Items); got != 0 {
		t.Fatal("无 JSON 应得 0 条")
	}
	got := parseRoadmapPlan(`{"items":[],"summary":"无需新增"}`)
	if len(got.Items) != 0 || got.Summary != "无需新增" {
		t.Fatalf("空数组应可解析: %+v", got)
	}
}

func TestParseRoadmapReview(t *testing.T) {
	raw := "```json\n{\"reviews\":[{\"id\":\"rm_1\",\"verdict\":\"approved\",\"reason\":\"价值高\"}]," +
		"\"summary\":\"整体可行\"}\n```"
	out := parseRoadmapReview(raw)
	if len(out.Reviews) != 1 || out.Reviews[0].ID != "rm_1" || out.Reviews[0].Verdict != "approved" {
		t.Fatalf("审核输出应可解析: %+v", out)
	}
	if got := len(parseRoadmapReview("没有任何 JSON").Reviews); got != 0 {
		t.Fatal("无 JSON 应得 0 条审核结论")
	}
}

// TestBuildRoadmapDevTask 开发任务提示词自包含（目标 + 要点 + 规范 + 收尾要求）。
func TestBuildRoadmapDevTask(t *testing.T) {
	task := buildRoadmapDevTask(roadmap.Item{Title: "给移动端加虚拟列表", Detail: "复用 renderMD 缓存思路"})
	for _, want := range []string{"给移动端加虚拟列表", "复用 renderMD 缓存思路", "AGENTS.md", "attempt_completion"} {
		if !strings.Contains(task, want) {
			t.Fatalf("开发任务提示词缺少 %q:\n%s", want, task)
		}
	}
}

// TestRoadmapDevelopSessionID 固定会话 ID 规则（移动端 openRmSession 兑底派生同源）。
func TestRoadmapDevelopSessionID(t *testing.T) {
	if got := roadmapDevelopSessionID("rm_abc123"); got != "roadmap_rm_abc123" {
		t.Fatalf("固定会话 ID 规则不符: %q", got)
	}
}

// TestUnattendedAskBridge 无人值守 ask 桥应答规则（roadmap 开发会话队列假死根因
// 修复的防线）：审批/错误上限自动放行；followup 自动答复继续；
// 敏感文件红线（文本含 redlineAskMarker）强制拒绝。
func TestUnattendedAskBridge(t *testing.T) {
	// 普通工具/命令审批：自动放行（无人值守不阻塞）。
	if resp, _ := unattendedAskBridge(message.NewAsk(message.AskTool, "fs_write → main.go")); resp != message.ResponseYes {
		t.Fatal("普通工具审批应自动放行")
	}
	if resp, _ := unattendedAskBridge(message.NewAsk(message.AskCommand, "go build ./...")); resp != message.ResponseYes {
		t.Fatal("命令审批应自动放行")
	}
	// 敏感文件红线：强制拒绝（防凭据被静默改写）。
	redline := message.NewAsk(message.AskTool, "fs_write → .env\n"+redlineAskMarker)
	if resp, _ := unattendedAskBridge(redline); resp == message.ResponseYes {
		t.Fatal("敏感文件红线应拒绝")
	}
	// 流错误/错误上限：重试续跑（不放任卡死）。
	if resp, _ := unattendedAskBridge(message.NewAsk(message.AskAPIReqFailed, "timeout")); resp != message.ResponseYes {
		t.Fatal("流错误应重试续跑")
	}
	if resp, _ := unattendedAskBridge(message.NewAsk(message.AskMistakeLimit, "limit")); resp != message.ResponseYes {
		t.Fatal("错误上限应重试续跑")
	}
	// followup：自动答复继续（防提问卡死队列）。
	resp, text := unattendedAskBridge(message.NewAsk(message.AskFollowup, "需要更多信息？"))
	if resp != message.ResponseMessageResponse || text == "" {
		t.Fatal("followup 应自动答复继续")
	}
	// 完成确认：自动通过（收尾即返回）。
	if resp, _ := unattendedAskBridge(message.NewAsk(message.AskCompletionResult, "done")); resp != message.ResponseYes {
		t.Fatal("完成确认应自动通过")
	}
}

// TestRemoteRoadmapPlanNoAPIKey 未配置 API Key 时远端规划入口直接报错（不进 goroutine）。
func TestRemoteRoadmapPlanNoAPIKey(t *testing.T) {
	a := &App{}
	if err := a.remoteRoadmapPlan(""); err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("未配置 APIKey 应报错: %v", err)
	}
}
