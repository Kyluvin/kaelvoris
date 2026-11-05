// flow_action_test.go —— cron run_flow 动作（规划板：定时触发指定 flow）：
//   - validateJob：run_flow 动作 FlowID 必填 / Task 放行空 / 未知动作拒绝 / 旧数据零值兼容；
//   - 调度器分派：run_flow 任务到期触发时 FlowLauncher 被调用（mock 断言），
//     未注入回调时明确报错（执行记录 failed 可见），提示词任务仍走 Runner。

package cron

import (
	"strings"
	"testing"
	"time"
)

// TestValidateJobRunFlow run_flow 动作校验：FlowID 必填、Task 不再强制、未知动作拒绝。
func TestValidateJobRunFlow(t *testing.T) {
	// 正路径：run_flow + FlowID，无 Task 合法（存量校验要求 Task 非空）。
	if err := validateJob(Job{Name: "日更图", Expr: "@daily", Action: ActionRunFlow, FlowID: "flow_1", Enabled: true}); err != nil {
		t.Fatalf("run_flow 合法定义应通过校验: %v", err)
	}
	// 负路径：缺 FlowID 拒绝。
	err := validateJob(Job{Name: "缺ID", Expr: "@daily", Action: ActionRunFlow})
	if err == nil || !strings.Contains(err.Error(), "flowId") {
		t.Fatalf("run_flow 缺 flowId 应拒绝，得到 %v", err)
	}
	// 负路径：未知动作拒绝。
	if err := validateJob(Job{Name: "坏", Expr: "@daily", Task: "t", Action: "explode"}); err == nil {
		t.Fatal("未知动作应拒绝")
	}
	// 存量兼容：无动作提示词任务照旧（Task 必填）。
	if err := validateJob(Job{Name: "旧", Expr: "@daily", Task: "跑"}); err != nil {
		t.Fatalf("存量任务应通过校验: %v", err)
	}
	if err := validateJob(Job{Name: "旧空", Expr: "@daily"}); err == nil {
		t.Fatal("存量任务缺 Task 仍应拒绝")
	}
}

// TestStoreJobActionJSONRoundTrip Action/FlowID 字段落盘往返 + 旧 JSON 兼容。
func TestStoreJobActionJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add(Job{Name: "流程任务", Expr: "@daily", Action: ActionRunFlow, FlowID: "flow_9", Enabled: true}); err != nil {
		t.Fatalf("run_flow 任务应保存成功: %v", err)
	}
	st2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	jobs := st2.List()
	if len(jobs) != 1 {
		t.Fatalf("重载后应有 1 条任务，得到 %d", len(jobs))
	}
	got := jobs[0]
	if got.Action != ActionRunFlow || got.FlowID != "flow_9" {
		t.Fatalf("往返不一致: %+v", got)
	}
	// 旧 JSON（无 action/flowId 键）加载零值语义不变。
	st3, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st3.Add(Job{Name: "旧", Expr: "@daily", Task: "跑", Enabled: true}); err != nil {
		t.Fatalf("旧语义任务应保存成功: %v", err)
	}
	if j := st3.List()[0]; j.Action != "" || j.FlowID != "" {
		t.Fatalf("旧任务新字段应为零值: %+v", j)
	}
}

// TestSchedulerRunFlowDispatch 验收核心：run_flow 任务到点触发时 FlowLauncher
// 被调用（mock 断言入参为该任务），且不再走 Runner 会话执行链。
func TestSchedulerRunFlowDispatch(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	fired := make(chan Job, 4)
	sched := NewScheduler(st, func(j Job) (string, error) {
		t.Error("run_flow 任务不应走 Runner 会话执行链")
		return "s", nil
	})
	sched.SetFlowLauncher(func(j Job) (string, error) {
		fired <- j
		return "cron_flow_x", nil
	})
	// 每分钟任务，第一轮 tick 立即到期（对齐 TestSchedulerTriggers 触发方式）。
	if _, err := st.Add(Job{Name: "定时流程", Expr: "* * * * *", Action: ActionRunFlow, FlowID: "flow_9", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	sched.tickOnce(time.Now(), false)
	select {
	case j := <-fired:
		if j.Action != ActionRunFlow || j.FlowID != "flow_9" {
			t.Fatalf("FlowLauncher 收到任务不符: %+v", j)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run_flow 任务未被触发（FlowLauncher 未被调用）")
	}
	// 等待异步 runJob 落盘完成（防 Windows TempDir 清理竞态，对齐既有测试）。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if runs := st.RecentRuns(10); len(runs) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("执行记录未落盘")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := st.RecentRuns(1)[0]; r.Status != "done" || r.SessionID != "cron_flow_x" {
		t.Fatalf("执行记录不符: %+v", r)
	}
	sched.Stop()
}

// TestSchedulerRunFlowNoLauncher 未注入 FlowLauncher：触发即报错（执行记录
// failed 可见，不静默丢弃），且不重试不熔断一次性的配置类失败语义由上层判断
// （此处仅断言错误上抛路径）。
func TestSchedulerRunFlowNoLauncher(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sched := NewScheduler(st, func(j Job) (string, error) { return "s", nil })
	if _, err := st.Add(Job{Name: "无回调", Expr: "* * * * *", Action: ActionRunFlow, FlowID: "flow_1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	sched.tickOnce(time.Now(), false)
	deadline := time.Now().Add(3 * time.Second)
	for {
		runs := st.RecentRuns(10)
		if len(runs) >= 1 {
			if runs[0].Status != "failed" || !strings.Contains(runs[0].Error, "flow 启动回调") {
				t.Fatalf("应记 failed 且错误指向未注入回调: %+v", runs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("执行记录未落盘")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sched.Stop()
}
