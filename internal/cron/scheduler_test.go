package cron

import (
	"testing"
	"time"
)

func TestParseDescriptors(t *testing.T) {
	cases := []struct {
		expr  string
		ok    bool
		every time.Duration
	}{
		{"@hourly", true, 0},
		{"@daily", true, 0},
		{"@midnight", true, 0},
		{"@weekly", true, 0},
		{"@monthly", true, 0},
		{"@yearly", true, 0},
		{"@annually", true, 0},
		{"@every 30m", true, 30 * time.Minute},
		{"@every 1h30m", true, 90 * time.Minute},
		{"@EVERY 2H", false, 0}, // 大写前缀不匹配（保守：仅小写/原文）
		{"@every 30s", false, 0},
		{"@bogus", false, 0},
		{"@every", false, 0},
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if c.ok {
			if err != nil {
				t.Errorf("Parse(%q) 应成功，得到 %v", c.expr, err)
				continue
			}
			if s.Every() != c.every {
				t.Errorf("Parse(%q).Every() = %v，应为 %v", c.expr, s.Every(), c.every)
			}
		} else if err == nil {
			t.Errorf("Parse(%q) 应失败", c.expr)
		}
	}
	// @daily 等价 0 0 * * *。
	s, _ := Parse("@daily")
	if !s.Match(time.Date(2024, 6, 1, 0, 0, 0, 0, time.Local)) {
		t.Fatal("@daily 应命中 00:00")
	}
	if s.Match(time.Date(2024, 6, 1, 0, 1, 0, 0, time.Local)) {
		t.Fatal("@daily 不应命中 00:01")
	}
}

func TestEveryNext(t *testing.T) {
	s, err := Parse("@every 1h30m")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2024, 6, 1, 10, 0, 0, 0, time.Local)
	nx := s.Next(from)
	want := time.Date(2024, 6, 1, 11, 30, 0, 0, time.Local)
	if !nx.Equal(want) {
		t.Fatalf("@every 1h30m Next = %v，应为 %v", nx, want)
	}
	// Match 恒 false（由调度器按间隔推进）。
	if s.Match(from) {
		t.Fatal("@every 模式 Match 应恒 false")
	}
}

func TestDescribeDescriptors(t *testing.T) {
	cases := map[string]string{
		"@hourly":     "每小时整点",
		"@daily":      "每天 00:00",
		"@weekly":     "每周日 00:00",
		"@monthly":    "每月 1 日 00:00",
		"@yearly":     "每年 1 月 1 日 00:00",
		"@every 90m":  "每 1 小时 30 分",
		"@every 24h":  "每 1 天",
		"@every 2h":   "每 2 小时",
		"0 9 * * 1-5": "0 9 * * 1-5", // 复杂表达式原样返回
		"0 9 * * 1":   "每周一 9:0",
	}
	for expr, want := range cases {
		if got := Describe(expr); got != want {
			t.Errorf("Describe(%q) = %q，应为 %q", expr, got, want)
		}
	}
}

func TestSchedulerTimeoutRelease(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{}, 1)
	sched := NewScheduler(st, func(j Job) (string, error) {
		<-release // 挂起模拟假死
		return "s", nil
	})
	j, _ := st.Add(Job{Name: "挂起", Expr: "* * * * *", Task: "x", Enabled: true, TimeoutMin: 1})
	// 占用并模拟超时（startedAt 回拨 2 分钟 > 1 分钟超时）。
	busy, _ := sched.tryOccupy(j.ID, DefaultTimeout(j), time.Now().Add(-2*time.Minute))
	if busy {
		t.Fatal("空任务位不应 busy")
	}
	// tryOccupy 已登记 startedAt=过去时间？——登记时传入 now=过去，模拟超时。
	// 再占用：超时应解除旧占用放行。
	busy2, _ := sched.tryOccupy(j.ID, DefaultTimeout(j), time.Now())
	if busy2 {
		t.Fatal("超时占用应被解除放行")
	}
	release <- struct{}{}
	sched.release(j.ID, sched.currentRunState(j.ID))
	if ids := sched.RunningIDs(); len(ids) != 0 {
		t.Fatalf("释放后不应有运行位: %v", ids)
	}
}

func TestSchedulerFailureRetryAndCircuit(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	calls := make(chan int, 16)
	n := 0
	var mu chan struct{} = make(chan struct{}, 1)
	sched := NewScheduler(st, func(j Job) (string, error) {
		mu <- struct{}{}
		n++
		k := n
		<-mu
		calls <- k
		return "s", errFail
	})
	// RetryLimit=2，MaxFailStreak=3：三次失败后熔断，重试至多 2 次（首次+2）。
	j, _ := st.Add(Job{Name: "易败", Expr: "0 0 1 1 *", Task: "x", Enabled: true, RetryLimit: 2})
	sched.runJob(j, time.Now(), 1)
	<-calls
	// 首次失败：连败 1，安排重试。
	g, _ := st.Get(j.ID)
	if g.FailStreak != 1 || g.NextRetry.IsZero() {
		t.Fatalf("首次失败应记连败并安排重试: streak=%d retry=%v", g.FailStreak, g.NextRetry)
	}
	sched.runJob(j, time.Now(), 2)
	<-calls
	sched.runJob(j, time.Now(), 3)
	<-calls
	g, _ = st.Get(j.ID)
	if g.Enabled {
		t.Fatal("连败 3 次应熔断停用")
	}
	if g.DisabledReason == "" {
		t.Fatal("熔断应记录原因")
	}
	// 重新启用清零。
	if _, err := st.SetEnabled(j.ID, true); err != nil {
		t.Fatal(err)
	}
	g, _ = st.Get(j.ID)
	if g.FailStreak != 0 || g.DisabledReason != "" || !g.NextRetry.IsZero() {
		t.Fatalf("重新启用应清零状态: %+v", g)
	}
	sched.Stop()
}

var errFail = &simpleErrT{"boom"}

type simpleErrT struct{ s string }

func (e *simpleErrT) Error() string { return e.s }

func TestSchedulerEveryInterval(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	fired := make(chan time.Time, 4)
	sched := NewScheduler(st, func(j Job) (string, error) {
		fired <- time.Now()
		return "s", nil
	})
	// @every 1h：LastRun 设为 2 小时前 → 立即到期。
	j, _ := st.Add(Job{Name: "间隔", Expr: "@every 1h", Task: "x", Enabled: true})
	past := time.Now().Add(-2 * time.Hour)
	st.SetLastRunForTest(j.ID, past)
	sched.tickOnce(time.Now(), false)
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("@every 到期任务未被触发")
	}
	sched.Stop()
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
}

func TestEvolveStore(t *testing.T) {
	// 见 evolve 包内测试。
}
