package desktop

// roadmap_verify_test.go 验收方式与机器验证声明核验单测（2026-10 harness 增强，
// 对齐 learn-harness-engineering 五子系统框架的 Verification/Instructions 缺口）：
//  1. 规划 JSON 协议 verification 字段解析（验收标准从规划→开发→收口全链路贯通）；
//  2. 开发任务提示词注入验收方式（生成者知道完成判据才可能满足判据）；
//  3. hasVerifyEvidence：验证证据检测（只认 shell/unified_exec 工具卡的真实命令，
//     不扫助手文本帧——防「我会跑测试」的说明文字被误判为已验证）；
//  4. 审核准则含可验收条目（gate 前移：规划侧按同尺自查）。

import (
	"encoding/json"
	"strings"
	"testing"

	"kaelvoris/internal/roadmap"
	"kaelvoris/pkg/message"
)

// TestParseRoadmapPlanVerification 规划产出的验收方式应完整落库。
func TestParseRoadmapPlanVerification(t *testing.T) {
	raw := `{"items":[{"title":"修复列表闪烁","detail":"虚拟列表优化","verification":"go test ./... 通过；面板列表滚动无闪烁","priority":"high","category":"bug"}],"summary":"ok"}`
	out := parseRoadmapPlan(raw)
	if len(out.Items) != 1 {
		t.Fatalf("应解析出 1 条: %+v", out)
	}
	if out.Items[0].Verification == "" {
		t.Fatal("verification 字段应被解析")
	}
	if !strings.Contains(out.Items[0].Verification, "go test") {
		t.Fatalf("验收方式内容不符: %q", out.Items[0].Verification)
	}
}

// TestBuildRoadmapDevTaskVerification 开发任务提示词包含验收方式与验证纪律。
func TestBuildRoadmapDevTaskVerification(t *testing.T) {
	task := buildRoadmapDevTask(roadmap.Item{Title: "给移动端加虚拟列表", Detail: "复用 renderMD 缓存思路", Verification: "go test ./... 通过"})
	for _, want := range []string{"验收方式", "go test ./... 通过", "机器核验", "降级为失败"} {
		if !strings.Contains(task, want) {
			t.Fatalf("开发任务提示词缺少 %q:\n%s", want, task)
		}
	}
}

// TestHasVerifyEvidence 验证证据检测：真实 shell 工具卡有证据；
// 纯文本宣称「已跑测试」不算证据；partial 帧不算；非验证命令不算。
func TestHasVerifyEvidence(t *testing.T) {
	toolCard := func(tool, cmd string, partial bool) message.ClineMessage {
		b, _ := json.Marshal(message.ToolCallInfo{Tool: tool, Cmd: cmd})
		m := message.NewSay(message.SayTool, string(b))
		m.Partial = partial
		return m
	}
	// shell 工具卡真实运行 go test：有证据。
	if !hasVerifyEvidence([]message.ClineMessage{toolCard("shell", "go build ./... && go test ./...", false)}) {
		t.Fatal("真实验证命令应检出证据")
	}
	// unified_exec 运行 npm run build：有证据。
	if !hasVerifyEvidence([]message.ClineMessage{toolCard("unified_exec", "npm run build", false)}) {
		t.Fatal("unified_exec 构建命令应检出证据")
	}
	// 助手文本帧宣称完成：不算证据（生成者不给自己打分）。
	text := message.NewSay(message.SayText, "我已运行 go test ./... 全部通过")
	if hasVerifyEvidence([]message.ClineMessage{text}) {
		t.Fatal("纯文本宣称不应算验证证据")
	}
	// partial 流式帧不算（最终帧才可信）。
	if hasVerifyEvidence([]message.ClineMessage{toolCard("shell", "go test ./...", true)}) {
		t.Fatal("partial 帧不应算证据")
	}
	// 只跑了无关命令（如 ls）：不算。
	if hasVerifyEvidence([]message.ClineMessage{toolCard("shell", "ls -la", false)}) {
		t.Fatal("无关命令不应算证据")
	}
	// 工具卡但命令为空：不算。
	if hasVerifyEvidence([]message.ClineMessage{toolCard("shell", "", false)}) {
		t.Fatal("空命令不应算证据")
	}
	// 空会话：无证据。
	if hasVerifyEvidence(nil) {
		t.Fatal("空会话不应有证据")
	}
}

// TestRoadmapReviewCriteriaVerifiable 审核准则包含可验收维度（规划预筛与 AI
// 审核共用同一把尺，gate 前移依赖准则同步）。
func TestRoadmapReviewCriteriaVerifiable(t *testing.T) {
	if !strings.Contains(roadmapReviewCriteria, "可验收") {
		t.Fatalf("审核准则应含可验收维度:\n%s", roadmapReviewCriteria)
	}
}

// TestRoadmapPlanPromptVerificationField 规划提示词 JSON 协议含 verification
// 字段说明（否则规划器不会产出该字段，链路断在源头）。
func TestRoadmapPlanPromptVerificationField(t *testing.T) {
	prompt := roadmapPlanPrompt("digest", nil, nil, 5, false)
	if !strings.Contains(prompt, "verification") {
		t.Fatal("规划提示词应含 verification 字段协议")
	}
}

// TestParseRoadmapReviewPaired 逐条成对 JSON 协议解析（判定对象 + 理由对象
// 相邻成对，按 ID 合并；summary 单独对象）。
func TestParseRoadmapReviewPaired(t *testing.T) {
	raw := `{"id":"rm_a","checks":[{"dim":"有据","ok":true}],"verdict":"approved"}
					 {"id":"rm_a","reason":"证据充分"}
					 {"id":"rm_b","verdict":"rejected"}
					 {"id":"rm_b","reason":"无素材出处"}
					 {"summary":"ok"}`
	out := parseRoadmapReview(raw)
	if len(out.Reviews) != 2 {
		t.Fatalf("应解析出 2 条: %+v", out)
	}
	if out.Reviews[0].ID != "rm_a" || out.Reviews[0].Verdict != "approved" || out.Reviews[0].Reason != "证据充分" {
		t.Fatalf("成对合并不符: %+v", out.Reviews[0])
	}
	if out.Reviews[1].ID != "rm_b" || out.Reviews[1].Verdict != "rejected" || out.Reviews[1].Reason != "无素材出处" {
		t.Fatalf("成对合并不符: %+v", out.Reviews[1])
	}
	if out.Summary != "ok" {
		t.Fatalf("summary 应解析: %q", out.Summary)
	}
}

// TestParseRoadmapReviewLegacy 旧版单对象 {reviews:[...]} 协议兼容（存量
// 模型输出形态不变即可继续工作）。
func TestParseRoadmapReviewLegacy(t *testing.T) {
	raw := `前言略。{"reviews":[{"id":"rm_a","verdict":"approved","reason":"ok"}],"summary":"s"}`
	out := parseRoadmapReview(raw)
	if len(out.Reviews) != 1 || out.Reviews[0].Verdict != "approved" || out.Reviews[0].Reason != "ok" {
		t.Fatalf("旧协议应兼容: %+v", out)
	}
}

// TestParseRoadmapReviewFenced 代码围栏包裹仍可解析（剥围栏后逐对象扫描）。
func TestParseRoadmapReviewFenced(t *testing.T) {
	raw := "```json\n{\"id\":\"rm_a\",\"verdict\":\"approved\"}\n{\"id\":\"rm_a\",\"reason\":\"r\"}\n```"
	out := parseRoadmapReview(raw)
	if len(out.Reviews) != 1 || out.Reviews[0].Verdict != "approved" || out.Reviews[0].Reason != "r" {
		t.Fatalf("围栏内成对对象应可解析: %+v", out)
	}
}

// TestRoadmapReviewMode 归一：空/非法→宽松（默认），standard/strict（历史值）
// →严审。
func TestRoadmapReviewMode(t *testing.T) {
	a := &App{}
	if got := a.roadmapReviewMode(); got != roadmapReviewModeLenient {
		t.Fatalf("空配置应默认宽松: %q", got)
	}
	a.roadmapAutoDev.Running = false
	// 直接构造配置读取路径：autoDevCfg 走 GetConfig（desktop.json），单测中
	// 未配置即宽松；standard 归一逻辑由 SetAutoDevConfig 校验分支覆盖。
}

// TestRoadmapPlanPromptUpgradeMode 升级模式注入：全驳回后规划提示词应含
// 证据锚点硬性要求（治「规划—审核同层空转」）。
func TestRoadmapPlanPromptUpgradeMode(t *testing.T) {
	up := roadmapPlanPrompt("digest", nil, nil, 5, true)
	if !strings.Contains(up, "升级模式") || !strings.Contains(up, "证据") {
		t.Fatal("升级模式应注入证据锚点要求")
	}
}

// TestRoadmapReviewPromptModes 宽松/严审判定规则注入（同一把尺子，两种松紧）。
func TestRoadmapReviewPromptModes(t *testing.T) {
	items := []roadmap.Item{{ID: "rm_a", Title: "t"}}
	lenient := roadmapReviewPrompt(items, "", nil, roadmapReviewModeLenient)
	if !strings.Contains(lenient, "一票否决") {
		t.Fatal("宽松模式应含红线一票否决规则")
	}
	strict := roadmapReviewPrompt(items, "", nil, roadmapReviewModeStandard)
	if !strings.Contains(strict, "全部 7 个维度") {
		t.Fatal("严审模式应含 7 维全过规则")
	}
}

// TestVerifyEvidencePatterns Go/前端/python 主链路验证命令模式必须覆盖。
func TestVerifyEvidencePatterns(t *testing.T) {
	joined := strings.Join(verifyEvidencePatterns, "|")
	for _, want := range []string{"go build", "go test", "npm run build", "pytest"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("验证命令模式缺 %q", want)
		}
	}
}
