// cron_notify_test.go 定时任务失败失焦通知（规划板 2026-12 条目）：
//   - cronFailedPayload 事件载荷：任务名 + 错误截断（前端通知文案数据源）；
//   - cronRunner 同步失败路径（池空）经 emitEvent 推送 kaelvoris:cron-failed
//     （ctx nil 静默语义与既有事件桥一致，载荷断言走纯函数）；
//   - cronJobName 兜底：任务已删 → 回退 ID。
//
// 不依赖 Wails 上下文与真实 LLM（App 仅构造 cronStore，同 cron_binding_test.go 基建）。
package desktop

import (
	"errors"
	"strings"
	"testing"

	"kaelvoris/internal/cron"
)

// TestCronFailedPayload 载荷：jobId/jobName/error 三键 + 长错误按 rune 截断。
func TestCronFailedPayload(t *testing.T) {
	long := strings.Repeat("模型不存在，", 100) // 600 rune
	p := cronFailedPayload(cron.Job{ID: "cron_x1", Name: "每日巡检"}, errors.New(long))
	if p["jobId"] != "cron_x1" || p["jobName"] != "每日巡检" {
		t.Fatalf("payload ids = %v / %v", p["jobId"], p["jobName"])
	}
	msg, _ := p["error"].(string)
	if msg == "" || len([]rune(msg)) != 201 { // truncateRunes 截断后补省略号
		t.Fatalf("error 应为 200 rune + 省略号, got %d rune", len([]rune(msg)))
	}
	if !strings.HasSuffix(msg, "…") {
		t.Fatalf("截断错误应以省略号结尾, got %q", msg)
	}
}

// TestCronFailedPayload_NilErr nil 错误 → error 为空串（前端兜底文案）。
func TestCronFailedPayload_NilErr(t *testing.T) {
	p := cronFailedPayload(cron.Job{ID: "cron_x2", Name: "任务B"}, nil)
	if p["error"] != "" {
		t.Fatalf("nil err → error 应为空串, got %v", p["error"])
	}
}

// TestCronRunner_FailureEmitsEvent 同步失败（池空）→ notifyCronFailed 被调用：
// emitEvent ctx==nil 静默不 panic（事件桥既有语义），错误仍上返调度器记 failed。
func TestCronRunner_FailureEmitsEvent(t *testing.T) {
	a := newCronBindingTestApp(t)
	pos := seedPosition(t, a, "空池岗")
	j := addBoundJob(t, a, "", pos.ID, "", 0)

	// ctx 为 nil：emitEvent 静默旁路（不 panic 即通过；真实弹出由前端核验）。
	if _, err := a.cronRunner(j); err == nil {
		t.Fatal("cronRunner pool-empty must fail")
	}
}

// TestCronJobName 兜底：任务存在取名；已删除回退 ID。
func TestCronJobName(t *testing.T) {
	a := newCronBindingTestApp(t)
	j := addBoundJob(t, a, "", "", "", 0)
	if got := cronJobName(a.cronStore, j.ID); got != "巡检" {
		t.Fatalf("name = %q, want 巡检", got)
	}
	if got := cronJobName(a.cronStore, "cron_gone"); got != "cron_gone" {
		t.Fatalf("deleted fallback = %q, want id", got)
	}
}
