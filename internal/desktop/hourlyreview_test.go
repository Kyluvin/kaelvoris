// hourlyreview_test.go 每小时复盘单测：哨兵判定、输出解析、聚合转录四类信号、
// 报告渲染、提示词红线。不依赖 Wails 上下文（App 仅构造 store/evolveStore）。
package desktop

import (
	"strings"
	"testing"
	"time"

	"kaelvoris/internal/cron"
	"kaelvoris/internal/evolve"
	"kaelvoris/internal/session"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// newHourlyTestApp 构造无 Wails 上下文的测试 App（store/evolveStore 指临时目录）。
func newHourlyTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	st := session.NewStore(dir)
	ev, err := evolve.NewStore(dir)
	if err != nil {
		t.Fatalf("evolve store: %v", err)
	}
	return &App{store: st, evolveStore: ev}
}

// newHourlyTestSession 构造带基础字段的测试会话。
func newHourlyTestSession(id, workspace string, at time.Time) *session.Session {
	return &session.Session{
		ID:        id,
		ChannelID: "desktop",
		Title:     "测试会话",
		Workspace: workspace,
		CreatedAt: at,
		UpdatedAt: at,
	}
}

// TestIsHourlyReviewTask 哨兵判定：哨兵前缀命中；普通任务不误判。
func TestIsHourlyReviewTask(t *testing.T) {
	if !isHourlyReviewTask(hourlyReviewSentinel) {
		t.Fatalf("哨兵文本应命中 isHourlyReviewTask")
	}
	if !isHourlyReviewTask("  " + hourlyReviewSentinel + " \n") {
		t.Fatalf("带空白哨兵文本应命中")
	}
	if isHourlyReviewTask("复盘一下这段代码") {
		t.Fatalf("普通任务不应命中")
	}
}

// TestParseHourlyReviewOutput JSON 协议解析（剥离围栏）+ 非法输入容错。
func TestParseHourlyReviewOutput(t *testing.T) {
	in := "```json\n{\"struggles\": [\"终端 $ 符号被吞\"], " +
		"\"improvements\": [{\"kind\": \"toolFix\", \"title\": \"shell 提示词强调 PS 转义\"}], " +
		"\"cards\": [{\"title\": \"PS 命令写 .ps1 文件执行\", \"body\": \"现象+根因+方案\", \"keywords\": [\"powershell\"]}]}\n```"
	out := parseHourlyReviewOutput(in)
	if len(out.Struggles) != 1 || !strings.Contains(out.Struggles[0], "$ 符号") {
		t.Fatalf("struggles = %v", out.Struggles)
	}
	if len(out.Improvements) != 1 || out.Improvements[0].Kind != "toolFix" {
		t.Fatalf("improvements = %+v", out.Improvements)
	}
	if len(out.Cards) != 1 || out.Cards[0].Keywords[0] != "powershell" {
		t.Fatalf("cards = %+v", out.Cards)
	}
	// 非法输出返回零值不 panic。
	if out := parseHourlyReviewOutput("模型没按协议输出"); len(out.Struggles) != 0 {
		t.Fatalf("非法输出应返回零值")
	}
	if out := parseHourlyReviewOutput(""); len(out.Cards) != 0 {
		t.Fatalf("空输出应返回零值")
	}
}

// TestBuildHourlyDigest_Signals 聚合转录四类信号齐全；cron_ 会话不入素材
// （防自我复盘滚雪球）；partial 帧不进素材。
func TestBuildHourlyDigest_Signals(t *testing.T) {
	a := newHourlyTestApp(t)
	now := time.Now()
	// 会话 1：任务 + 错误 + 未完成待办 + 工具调用 + partial 噪声帧。
	s1 := newHourlyTestSession("sess-a", "D:/proj", now)
	s1.Append(message.NewSay(message.SayUserFeedback, "修复构建失败"))
	s1.Append(message.NewSay(message.SayError, "go build: undefined: foo"))
	s1.Append(message.NewSay(message.SayTool, `{"tool":"fs_read","path":"a.go"}`))
	s1.Append(message.ClineMessage{Type: message.TypeSay, Say: message.SayText, Text: "partial 噪声", Partial: true})
	s1.Todos = []tools.TodoItem{{Content: "补单测", Status: "pending"}}
	a.store.Save(s1)
	// cron 哨兵会话：不应入素材。
	s2 := newHourlyTestSession("cron_x", "", now)
	s2.Append(message.NewSay(message.SayUserFeedback, "哨兵会话任务"))
	a.store.Save(s2)

	d := a.buildHourlyDigest()
	if strings.Contains(d, "哨兵会话任务") {
		t.Fatalf("cron_ 会话不应入聚合:\n%s", d)
	}
	if strings.Contains(d, "partial 噪声") {
		t.Fatalf("partial 帧不应入聚合:\n%s", d)
	}
	for _, want := range []string{"修复构建失败", "go build: undefined: foo", "补单测", "fs_read×1"} {
		if !strings.Contains(d, want) {
			t.Fatalf("聚合缺少信号 %q:\n%s", want, d)
		}
	}
}

// TestBuildHourlyDigest_Empty 窗口外/无会话 = 空素材（复盘跳过依据）。
func TestBuildHourlyDigest_Empty(t *testing.T) {
	a := newHourlyTestApp(t)
	// 仅一个 48h 前的过期会话（窗口 26h 外）。
	old := newHourlyTestSession("old", "", time.Now().Add(-48*time.Hour))
	a.store.Save(old)
	if d := a.buildHourlyDigest(); d != "" {
		t.Fatalf("窗口外会话不应入聚合，got:\n%s", d)
	}
	// 空仓库 = 空。
	a2 := newHourlyTestApp(t)
	if d := a2.buildHourlyDigest(); d != "" {
		t.Fatalf("空仓库应返回空聚合，got:\n%s", d)
	}
}

// TestHourlyReviewPrompt_Redlines 提示词覆盖四类建议 + 协议 + 既有卡去重引导。
func TestHourlyReviewPrompt_Redlines(t *testing.T) {
	p := hourlyReviewPrompt([]string{"既有卡A"}, "")
	for _, want := range []string{"promptFix", "toolFix", "newTool", "config", `"struggles"`, "既有经验卡标题", "既有卡A", "不要任何解释"} {
		if !strings.Contains(p, want) {
			t.Fatalf("提示词缺少 %q", want)
		}
	}
}

// TestRenderHourlyReviewReport 报告渲染：困难/建议/沉淀计数；空产出兜底文案。
func TestRenderHourlyReviewReport(t *testing.T) {
	out := hourlyReviewOutput{
		Struggles: []string{"终端 $ 符号被吞"},
		Improvements: []hourlyReviewImprovement{
			{Kind: "promptFix", Title: "shell 提示词强调 PS 转义"},
		},
		Cards: []hourlyReviewCard{{Title: "t", Body: "b"}},
	}
	rep := renderHourlyReviewReport(out, 1)
	for _, want := range []string{"遇到的困难", "$ 符号被吞", "[promptFix]", "已沉淀 1 条"} {
		if !strings.Contains(rep, want) {
			t.Fatalf("报告缺少 %q:\n%s", want, rep)
		}
	}
	empty := renderHourlyReviewReport(hourlyReviewOutput{}, 0)
	if !strings.Contains(empty, "未发现明显困难") {
		t.Fatalf("空产出报告缺兜底文案:\n%s", empty)
	}
}

// TestHourlyReviewSentinelExpr 一键创建用的默认表达式合法（5 段式与 @hourly）。
func TestHourlyReviewSentinelExpr(t *testing.T) {
	for _, expr := range []string{"0 * * * *", "15 * * * *", "@hourly"} {
		if _, err := cron.Parse(expr); err != nil {
			t.Fatalf("expr %q 应合法: %v", expr, err)
		}
	}
}

// TestFirstLine 首行截取（toast 单行展示）。
func TestFirstLine(t *testing.T) {
	if got := firstLine("第一行\n第二行"); got != "第一行" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine("单行"); got != "单行" {
		t.Fatalf("firstLine 单行 = %q", got)
	}
}

// TestDetectUserRepeat 用户重复提交卡点检测（2026-10 复盘：宁可漏报不可误报）：
// ① 3 分钟内完全相同/规范化后相同 ≥2 次命中；② 超窗不命中；③ 仅一次不命中；
// ④ 规范化规则（去空格/标点/大小写、全角标点）对不上不误报；⑤ 无时间戳跳过。
func TestDetectUserRepeat(t *testing.T) {
	const w = 3 * time.Minute
	now := time.Now().UnixNano()
	min := func(m int64) time.Duration { return time.Duration(m) * time.Minute }
	mk := func(ts int64, text string) message.ClineMessage {
		return message.ClineMessage{TS: ts, Type: message.TypeSay, Say: message.SayUserFeedback, Text: text}
	}
	// ① 3 分钟内同文本 ×3（中间隔 1 分钟）→ 命中 3 次。
	m1 := mk(now, "构建失败了，帮我修一下")
	m2 := mk(now+int64(min(1)), "构建失败了，帮我修一下")
	m3 := mk(now+int64(min(2)), "构建失败了，帮我修一下")
	if r := detectUserRepeat([]message.ClineMessage{m1, m2, m3}, w, 10); r.Count != 3 || r.Text != m3.Text {
		t.Fatalf("① 3 分钟同文本应命中 3 次，got %+v", r)
	}
	// ② 间隔 5 分钟（超窗）→ 不命中。
	m4 := mk(now, "修复构建失败")
	m5 := mk(now+int64(min(5)), "修复构建失败")
	if r := detectUserRepeat([]message.ClineMessage{m4, m5}, w, 10); r.Count != 0 {
		t.Fatalf("② 超窗不应命中，got %+v", r)
	}
	// ③ 仅一条 → 不命中。
	if r := detectUserRepeat([]message.ClineMessage{m1}, w, 10); r.Count != 0 {
		t.Fatalf("③ 单条不应命中，got %+v", r)
	}
	// ④ 规范化：空格/标点/大小写/全角标点不同 → 仍命中（去标点后相同）。
	n1 := mk(now, "Go 构建失败了，帮我修一下！")
	n2 := mk(now+int64(min(1)), "go构建失败了帮我修一下")
	if r := detectUserRepeat([]message.ClineMessage{n1, n2}, w, 10); r.Count < 2 {
		t.Fatalf("④ 去标点/空格/大小写后相同应命中，got %+v", r)
	}
	// ⑤ 无时间戳 → 跳过上一条，不误报（宁漏报）。
	z1 := message.ClineMessage{TS: 0, Type: message.TypeSay, Say: message.SayUserFeedback, Text: "没时间戳"}
	z2 := mk(now, "没时间戳")
	if r := detectUserRepeat([]message.ClineMessage{z1, z2}, w, 10); r.Count != 0 {
		t.Fatalf("⑤ 无时间戳应跳过，got %+v", r)
	}
	// ⑥ 非用户反馈/partial 帧不参与；空文本不参与。
	e1 := mk(now, "修复构建")
	e2 := mk(now+int64(min(1)), "   ") // 全空白 → 规范化后空，跳过
	e3 := mk(now+int64(min(2)), "修复构建")
	e4 := message.ClineMessage{TS: now + int64(min(3)), Type: message.TypeSay, Say: message.SayText, Text: "修复构建"}
	e5 := message.ClineMessage{TS: now + int64(min(3)), Type: message.TypeSay, Say: message.SayUserFeedback, Text: "修复构建", Partial: true}
	if r := detectUserRepeat([]message.ClineMessage{e1, e2, e3, e4, e5}, w, 10); r.Count != 2 {
		t.Fatalf("⑥ 过滤参与项后应命中 2 次，got %+v", r)
	}
}

// TestBuildHourlyDigest_RepeatSignal 聚合输出应包含「疑似卡点」信号行。
func TestBuildHourlyDigest_RepeatSignal(t *testing.T) {
	a := newHourlyTestApp(t)
	now := time.Now()
	s := newHourlyTestSession("sess-rep", "D:/proj", now)
	// 三条 3 分钟内相同用户消息（NewSay 无 TS 时用带 TS 的构造）。
	for i := 0; i < 3; i++ {
		s.Append(message.ClineMessage{TS: now.Add(time.Duration(i) * time.Minute).UnixNano(),
			Type: message.TypeSay, Say: message.SayUserFeedback, Text: "构建失败了，帮我修一下"})
	}
	// 一条无关用户消息去重：
	s.Append(message.ClineMessage{TS: now.Add(4 * time.Minute).UnixNano(),
		Type: message.TypeSay, Say: message.SayUserFeedback, Text: "再补个单测"})
	a.store.Save(s)
	d := a.buildHourlyDigest()
	if !strings.Contains(d, "疑似卡点：用户重复提交 3 次") {
		t.Fatalf("聚合缺少重复提交卡点信号:\n%s", d)
	}
}
