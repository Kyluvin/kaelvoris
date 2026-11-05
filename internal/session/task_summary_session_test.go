// task_summary_session_test.go —— 会话总结持久化（SessionData.TaskSummary）测试。
package session

import (
	"testing"
	"time"
)

// TestTaskSummaryRoundTrip 会话总结落盘/加载往返（随会话 JSON 持久化）。
func TestTaskSummaryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	defer st.Close()

	s := &Session{ID: "s-ts", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	ts := &TaskSummary{
		Goal:        "重构登录模块",
		Status:      "done",
		Outcome:     "完成 5 处改动，产出 internal/auth/*.go",
		Next:        "无",
		ElapsedSecs: 95,
		GeneratedAt: time.Now(),
	}
	s.SetTaskSummary(ts)
	st.Save(s)
	st.FlushAll()

	// 从盘重新加载（清缓存后读盘验证 JSON 往返）。
	st2 := NewStore(dir)
	defer st2.Close()
	got, err := st2.Get("s-ts")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got.Snapshot().TaskSummary == nil {
		t.Fatal("TaskSummary 未持久化")
	}
	rt := got.Snapshot().TaskSummary
	if rt.Goal != "重构登录模块" || rt.Status != "done" || rt.Next != "无" {
		t.Fatalf("往返不一致: %+v", rt)
	}
	if rt.ElapsedSecs != 95 {
		t.Fatalf("ElapsedSecs = %d, want 95", rt.ElapsedSecs)
	}

	// Summary() 应标记 HasSummary=true（会话卡角标）。
	if sm := got.Summary(); !sm.HasSummary {
		t.Fatal("HasSummary 应为 true")
	}
}

// TestTaskSummaryClear 清除总结（nil）后 HasSummary 归位。
func TestTaskSummaryClear(t *testing.T) {
	s := &Session{ID: "s-ts2", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	s.SetTaskSummary(&TaskSummary{Goal: "g", Status: "done", Outcome: "o"})
	if !s.Summary().HasSummary {
		t.Fatal("设置后 HasSummary 应为 true")
	}
	s.SetTaskSummary(nil)
	if s.Summary().HasSummary {
		t.Fatal("清除后 HasSummary 应为 false")
	}
}
