package cron

import (
	"path/filepath"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		expr string
		ok   bool
	}{
		{"* * * * *", true},
		{"0 9 * * 1-5", true},
		{"*/15 * * * *", true},
		{"0 12 1 * *", true},
		{"30 8,20 * * *", true},
		{"0 9-18/2 * * *", true},
		{"61 * * * *", false},
		{"* 24 * * *", false},
		{"* * 0 * *", false},
		{"* * * 13 *", false},
		{"* * * * 7", false},
		{"0 9 * *", false},
		{"a * * * *", false},
	}
	for _, c := range cases {
		_, err := Parse(c.expr)
		if c.ok && err != nil {
			t.Errorf("Parse(%q) 应成功，得到 %v", c.expr, err)
		}
		if !c.ok && err == nil {
			t.Errorf("Parse(%q) 应失败", c.expr)
		}
	}
}

func TestMatch(t *testing.T) {
	s, err := Parse("0 9 * * 1-5") // 工作日 09:00
	if err != nil {
		t.Fatal(err)
	}
	if !s.Match(time.Date(2024, 1, 1, 9, 0, 0, 0, time.Local)) { // 周一
		t.Fatal("周一 09:00 应命中")
	}
	if s.Match(time.Date(2024, 1, 1, 9, 1, 0, 0, time.Local)) {
		t.Fatal("09:01 不应命中")
	}
	if s.Match(time.Date(2024, 1, 6, 9, 0, 0, 0, time.Local)) { // 周六
		t.Fatal("周六不应命中")
	}
	// 日/周并集语义：1 号或周一。
	s2, _ := Parse("0 9 1 * 1")
	if !s2.Match(time.Date(2024, 1, 1, 9, 0, 0, 0, time.Local)) { // 1 号也是周一
		t.Fatal("1 号应命中")
	}
	if !s2.Match(time.Date(2024, 1, 15, 9, 0, 0, 0, time.Local)) { // 周一非 1 号
		t.Fatal("周一应命中（并集语义）")
	}
}

func TestNext(t *testing.T) {
	s, _ := Parse("*/15 * * * *")
	from := time.Date(2024, 6, 1, 10, 7, 0, 0, time.Local)
	nx := s.Next(from)
	want := time.Date(2024, 6, 1, 10, 15, 0, 0, time.Local)
	if !nx.Equal(want) {
		t.Fatalf("*/15 Next(%v) = %v，应为 %v", from, nx, want)
	}
}

func TestStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "cron"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := st.Add(Job{Name: "日报", Expr: "0 9 * * *", Task: "生成日报", Enabled: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if j.ID == "" {
		t.Fatal("应生成 ID")
	}
	if _, err := st.Add(Job{Name: "bad", Expr: "bad", Task: "x", Enabled: true}); err == nil {
		t.Fatal("非法表达式应报错")
	}
	if _, err := st.Add(Job{Name: "empty", Expr: "* * * * *", Task: "", Enabled: true}); err == nil {
		t.Fatal("空提示词应报错")
	}

	j.Task = "更新后的任务"
	if _, err := st.Update(j); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := st.Get(j.ID)
	if got.Task != "更新后的任务" {
		t.Fatalf("Update 未生效: %+v", got)
	}

	// 任务级模型：Update 写入 + 重载回读（Model 空串 omitempty 不落盘）。
	j.Model = "deepseek-chat"
	if _, err := st.Update(j); err != nil {
		t.Fatalf("Update Model: %v", err)
	}
	if got, _ := st.Get(j.ID); got.Model != "deepseek-chat" {
		t.Fatalf("Model 未生效: %+v", got)
	}
	j.Model = ""
	if _, err := st.Update(j); err != nil {
		t.Fatalf("清空 Model: %v", err)
	}
	if got, _ := st.Get(j.ID); got.Model != "" {
		t.Fatalf("Model 应已清空: %+v", got)
	}

	if _, err := st.SetEnabled(j.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if g, _ := st.Get(j.ID); g.Enabled {
		t.Fatal("应已禁用")
	}

	// 执行记录。
	if err := st.MarkRun(j.ID, time.Now(), Run{JobID: j.ID, JobName: j.Name, Status: "done", StartedAt: time.Now()}); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	runs := st.RecentRuns(10)
	if len(runs) != 1 || runs[0].Status != "done" {
		t.Fatalf("执行记录不符: %+v", runs)
	}

	if err := st.Delete(j.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := st.Get(j.ID); ok {
		t.Fatal("删除后不应存在")
	}

	// 持久化回读。
	j2, _ := st.Add(Job{Name: "持久", Expr: "*/5 * * * *", Task: "任务", Enabled: true})
	st2, err := NewStore(filepath.Join(dir, "cron"))
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := st2.Get(j2.ID); !ok || g.Name != "持久" {
		t.Fatalf("重载后应读到任务: %+v ok=%v", g, ok)
	}
}

func TestStoreDeleteCleansRuns(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "cron"))
	if err != nil {
		t.Fatal(err)
	}
	del, _ := st.Add(Job{Name: "待删除", Expr: "0 9 * * *", Task: "t", Enabled: true})
	keep, _ := st.Add(Job{Name: "保留", Expr: "0 10 * * *", Task: "t", Enabled: true})
	// 两任务各留一条执行记录，再删除其一。
	if err := st.MarkRun(del.ID, time.Now(), Run{JobID: del.ID, JobName: del.Name, Status: "done", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRun(keep.ID, time.Now(), Run{JobID: keep.ID, JobName: keep.Name, Status: "done", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(del.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 1. 内存态：被删任务的执行记录全部清除，其他任务记录保留。
	runs := st.RecentRuns(50)
	if len(runs) != 1 || runs[0].JobID != keep.ID {
		t.Fatalf("删除后执行记录应只剩保留任务: %+v", runs)
	}
	// 2. 持久化回读：runs.json 同样无残留。
	st2, err := NewStore(filepath.Join(dir, "cron"))
	if err != nil {
		t.Fatal(err)
	}
	if r := st2.RecentRuns(50); len(r) != 1 || r[0].JobID != keep.ID {
		t.Fatalf("重载后执行记录仍有残留: %+v", r)
	}
	// 3. 删除后异步收尾（模拟 runJob 完成写回）不应复活记录。
	if err := st.MarkRun(del.ID, time.Now(), Run{JobID: del.ID, JobName: del.Name, Status: "done", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if r := st.RecentRuns(50); len(r) != 1 || r[0].JobID != keep.ID {
		t.Fatalf("删除后写回应被忽略: %+v", r)
	}
}

func TestSchedulerTriggers(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "cron"))
	if err != nil {
		t.Fatal(err)
	}
	fired := make(chan Job, 4)
	sched := NewScheduler(st, func(j Job) (string, error) {
		fired <- j
		return "sess-1", nil
	})
	// 每分钟任务，NextRun 设为过去 → 第一轮 tick 立即到期。
	if _, err := st.Add(Job{Name: "每分钟", Expr: "* * * * *", Task: "tick", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	sched.tickOnce(time.Now(), false)
	select {
	case j := <-fired:
		if j.Task != "tick" {
			t.Fatalf("触发任务不符: %+v", j)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("到期任务未被触发")
	}
	// 等待异步 runJob 完成记录落盘（runner 回调发出后 MarkRun 仍异步写
	// runs.json；立即返回会与 TempDir 清理竞态——Windows 上偶发
	// "directory is not empty"）。以记录落盘为完成信号，确定性等待。
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
	sched.Stop()
}

func TestSchedulerRunNow(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "cron"))
	called := make(chan Job, 1)
	sched := NewScheduler(st, func(j Job) (string, error) {
		called <- j
		time.Sleep(50 * time.Millisecond) // 模拟运行耗时
		return "s1", nil
	})
	j, _ := st.Add(Job{Name: "手动", Expr: "0 0 1 1 *", Task: "手动", Enabled: true})
	sched.Start()
	defer sched.Stop()
	if _, err := sched.RunNow(j.ID); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	// 运行中再触发应报 busy。
	if _, err := sched.RunNow(j.ID); err != ErrBusy {
		t.Fatalf("防重入应返回 ErrBusy，得到 %v", err)
	}
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("RunNow 未触发")
	}
}
