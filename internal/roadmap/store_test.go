package roadmap

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreAddListDelete(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 空标题应报错。
	if _, err := s.Add(Item{Title: " "}); err == nil {
		t.Fatal("空标题应报错")
	}
	it, err := s.Add(Item{Title: "给设置页加模型测速", Detail: "ping /models 计时", Priority: "high", Source: "manual"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if it.ID == "" || it.Status != StatusProposed {
		t.Fatalf("新条目应为 proposed 且有 ID: %+v", it)
	}
	if it.Priority != PriorityHigh {
		t.Fatalf("优先级应保留 high: %q", it.Priority)
	}
	// 超长标题截断。
	long, _ := s.Add(Item{Title: strings.Repeat("标", 200)})
	if len([]rune(long.Title)) != maxTitleRunes {
		t.Fatalf("标题应截断到 %d rune", maxTitleRunes)
	}
	if got := len(s.List()); got != 2 {
		t.Fatalf("应有 2 条，得到 %d", got)
	}
	// 更新。
	it.Title = "改名后的标题"
	up, err := s.Update(it)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if up.Title != "改名后的标题" || up.CreatedAt.IsZero() {
		t.Fatalf("更新应保留不可变字段: %+v", up)
	}
	if _, err := s.Update(Item{ID: "rm_none"}); err == nil {
		t.Fatal("更新不存在条目应报错")
	}
	if err := s.Delete(it.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get(it.ID); ok {
		t.Fatal("删除后应不存在")
	}
}

func TestSetSession(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	it, _ := s.Add(Item{Title: "待开发条目", Detail: "要点"})
	if _, err := s.SetStatus(it.ID, StatusApproved, "", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// 写入开发会话 ID：其他字段不得被触碰（SetSession 是部分更新）。
	if err := s.SetSession(it.ID, "roadmap_"+it.ID); err != nil {
		t.Fatalf("SetSession: %v", err)
	}
	got, _ := s.Get(it.ID)
	if got.SessionID != "roadmap_"+it.ID {
		t.Fatalf("SessionID 应写入: %+v", got)
	}
	if got.Title != "待开发条目" || got.Status != StatusApproved || got.CreatedAt.IsZero() {
		t.Fatalf("SetSession 不得触碰其他字段: %+v", got)
	}
	// 不存在条目报错。
	if err := s.SetSession("rm_none", "x"); err == nil {
		t.Fatal("SetSession 不存在条目应报错")
	}
	// 持久化：重开 Store 仍可读到。
	s2, _ := NewStore(t.TempDir())
	if err := s2.SetSession("rm_none", "x"); err == nil {
		t.Fatal("空库 SetSession 应报错")
	}
	_ = s2
}

func TestSetStatusFlow(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	it, _ := s.Add(Item{Title: "修复 cron 熔断不清零", Source: "manual"})
	if _, err := s.SetStatus(it.ID, StatusApproved, "", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, _ := s.Get(it.ID)
	if got.Status != StatusApproved {
		t.Fatalf("状态应为 approved: %s", got.Status)
	}
	if _, err := s.SetStatus(it.ID, StatusDeveloping, "", ""); err != nil {
		t.Fatalf("developing: %v", err)
	}
	got, _ = s.Get(it.ID)
	if got.StartedAt.IsZero() {
		t.Fatal("进入开发应记录 StartedAt")
	}
	if _, err := s.SetStatus(it.ID, StatusDone, "已完成：修复熔断计数", ""); err != nil {
		t.Fatalf("done: %v", err)
	}
	got, _ = s.Get(it.ID)
	if got.Result != "已完成：修复熔断计数" && got.Result != "已完成：修复熔断不清零" && got.Result == "" {
		t.Fatalf("完成应记录结果: %+v", got)
	}
	if got.FinishedAt.IsZero() {
		t.Fatal("完成应记录 FinishedAt")
	}
	// failed 记录错误信息（走合法路径 proposed→approved→developing→failed）。
	it2, _ := s.Add(Item{Title: "会失败的条目"})
	if _, err := s.SetStatus(it2.ID, StatusApproved, "", ""); err != nil {
		t.Fatalf("approve for failed: %v", err)
	}
	if _, err := s.SetStatus(it2.ID, StatusDeveloping, "", ""); err != nil {
		t.Fatalf("developing for failed: %v", err)
	}
	if _, err := s.SetStatus(it2.ID, StatusFailed, "", "编译失败: undefined: Foo"); err != nil {
		t.Fatalf("failed: %v", err)
	}
	got2, _ := s.Get(it2.ID)
	if got2.Error != "编译失败: undefined: Foo" {
		t.Fatalf("失败应记录错误: %q", got2.Error)
	}
	// 非法状态直接拒（保持原状态）。
	if _, err := s.SetStatus(it2.ID, "bogus", "", ""); err == nil {
		t.Fatal("非法状态应报错")
	}
}

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to string
		ok       bool
	}{
		{StatusProposed, StatusApproved, true},
		{StatusProposed, StatusRejected, true},
		{StatusProposed, StatusDeveloping, false}, // 必须先审批
		{StatusApproved, StatusDeveloping, true},
		{StatusApproved, StatusProposed, true},
		{StatusDeveloping, StatusDone, true},
		{StatusDeveloping, StatusFailed, true},
		{StatusDeveloping, StatusBlocked, true}, // 自动重试超限封存
		{StatusDeveloping, StatusApproved, false},
		{StatusFailed, StatusApproved, true},
		{StatusFailed, StatusProposed, true},
		{StatusBlocked, StatusApproved, true},    // 人工重跑回队
		{StatusBlocked, StatusDeveloping, false}, // blocked 不再自动开发
		{StatusDone, StatusApproved, false},
		{StatusRejected, StatusApproved, false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.ok {
			t.Errorf("CanTransition(%s→%s) = %v, want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestEvictAndCount(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	for i := 0; i < maxItems+10; i++ {
		added, err := s.Add(Item{Title: "条目" + string(rune('a'+i%26)) + string(rune('0'+i%10))})
		if err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
		// 标记终态让它可被淘汰（走合法路径到 done）。
		_, _ = s.SetStatus(added.ID, StatusApproved, "", "")
		_, _ = s.SetStatus(added.ID, StatusDeveloping, "", "")
		_, _ = s.SetStatus(added.ID, StatusDone, "ok", "")
	}
	if got := len(s.List()); got > maxItems {
		t.Fatalf("条目应淘汰到 %d 以内，得到 %d", maxItems, got)
	}
	// 活跃条目不被淘汰：加满终态后再加一条活跃，应保留。
	s2, _ := NewStore(t.TempDir())
	for i := 0; i < maxItems; i++ {
		_, _ = s2.Add(Item{Title: "活跃" + string(rune('a'+i%26)) + string(rune('0'+i%10))})
	}
	kept, _ := s2.Add(Item{Title: "不许被淘汰"})
	found := false
	for _, it := range s2.List() {
		if it.ID == kept.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("活跃条目不应被淘汰")
	}
	// Count 统计。
	c := s2.Count()
	if c["total"] != maxItems+1 || c[StatusProposed] != maxItems+1 {
		t.Fatalf("Count 不正确: %+v", c)
	}
}

func TestAutoDevDailyLimit(t *testing.T) {
	st := &AutoDevState{}
	for i := 0; i < 3; i++ {
		if !st.AllowAutoDev(3) {
			t.Fatalf("第 %d 次应放行", i+1)
		}
		st.RecordDev()
	}
	if st.AllowAutoDev(3) {
		t.Fatal("达到每日上限应拒绝")
	}
	if st.TodayCount != 3 {
		t.Fatalf("计数应为 3: %d", st.TodayCount)
	}
	// 无上限（0 = 不限）。
	st2 := &AutoDevState{}
	for i := 0; i < 100; i++ {
		st2.RecordDev()
	}
	if !st2.AllowAutoDev(0) {
		t.Fatal("dailyLimit=0 应不限流")
	}
}

func TestZeroRoundBreaker(t *testing.T) {
	st := &AutoDevState{}
	// 连续全驳回 < 阈值：不熔断。
	for i := 0; i < AutoDevMaxZeroRounds-1; i++ {
		if st.PlanBlockedByZero() {
			t.Fatalf("第 %d 轮全驳回不应熔断", i+1)
		}
		st.RecordZeroRound()
	}
	// 有通过：计数清零（审核通过即有效，见 roadmapReviewOnce）。
	st.ResetZeroRounds()
	if st.PlanBlockedByZero() {
		t.Fatal("ResetZeroRounds 后不应熔断")
	}
	// 达到阈值：熔断（当日暂停自动规划）。
	for i := 0; i < AutoDevMaxZeroRounds; i++ {
		st.RecordZeroRound()
	}
	if !st.PlanBlockedByZero() {
		t.Fatal("连续全驳回达阈值应熔断")
	}
}

func TestSetReviewAIAudit(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	it, _ := s.Add(Item{Title: "待 AI 审核的提案"})
	// 通过：写审核意见 + 置 approved + 记批准来源。
	got, err := s.SetReview(it.ID, "✅ 通过：解决真实痛点，粒度合适", StatusApproved, "ai_review")
	if err != nil {
		t.Fatalf("SetReview approve: %v", err)
	}
	if got.Status != StatusApproved || got.ApprovedBy != "ai_review" || got.Review == "" {
		t.Fatalf("审核通过应记录意见/状态/来源: %+v", got)
	}
	// 驳回：留痕 + 状态置 rejected。
	it2, _ := s.Add(Item{Title: "应被驳回的提案"})
	got2, err := s.SetReview(it2.ID, "❌ 驳回：与既有条目重复", StatusRejected, "")
	if err != nil {
		t.Fatalf("SetReview reject: %v", err)
	}
	if got2.Status != StatusRejected || got2.Review == "" {
		t.Fatalf("驳回应记录理由: %+v", got2)
	}
	// 终态保护：rejected 是终态，不能直接回队（保证审核结论不被无声覆写）。
	if _, err := s.SetStatus(it2.ID, StatusApproved, "", ""); err == nil {
		t.Fatal("rejected→approved 应被状态机拒绝")
	}
	// 审核意见不被后续状态变更覆写。
	got3, _ := s.Get(it2.ID)
	if got3.Review == "" {
		t.Fatal("审核意见应持久保留")
	}
}

func TestRejectionRingLog(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	// 空日志：无反馈。
	if got := s.RejectionFeedback(6); len(got) != 0 {
		t.Fatalf("空日志应无反馈: %v", got)
	}
	s.AppendRejection("提案A", "与既有条目重复")
	s.AppendRejection("提案B", "粒度过大需拆分")
	got := s.RejectionFeedback(6)
	if len(got) != 2 || !strings.Contains(got[0], "提案B") || !strings.Contains(got[0], "粒度过大") {
		t.Fatalf("最新在前应返回提案B: %v", got)
	}
	// 环形上限：超过 maxRejections 淘汰最旧。
	for i := 0; i < maxRejections+3; i++ {
		s.AppendRejection(fmt.Sprintf("提案%d", i), "理由")
	}
	if got := s.RejectionFeedback(maxRejections + 5); len(got) != maxRejections {
		t.Fatalf("环形上限应为 %d 条: %d", maxRejections, len(got))
	}
	// 持久化：重新打开 store 反馈仍在（进程重启不丢失）。共追加 maxRejections+3
	// 条，淘汰后最新一条是提案(maxRejections+2)。
	s2, _ := NewStore(s.dir)
	if got := s2.RejectionFeedback(1); len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("提案%d", maxRejections+2)) {
		t.Fatalf("重启后应保留最新反馈: %v", got)
	}
}

func TestAttachBatch(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	it, _ := s.Add(Item{Title: "并行开发条目 A"})
	it2, _ := s.Add(Item{Title: "并行开发条目 B"})
	// 归批次：BatchID/ParentSessionID 写入，其余字段不受影响。
	if err := s.AttachBatch(it.ID, "roadmap_batch_1001", "roadmap_batch_1001"); err != nil {
		t.Fatalf("AttachBatch: %v", err)
	}
	if err := s.AttachBatch(it2.ID, "roadmap_batch_1001", "roadmap_batch_1001"); err != nil {
		t.Fatalf("AttachBatch it2: %v", err)
	}
	got, _ := s.Get(it.ID)
	if got.BatchID != "roadmap_batch_1001" || got.ParentSessionID != "roadmap_batch_1001" {
		t.Fatalf("批次应落库: %+v", got)
	}
	if got.Title != "并行开发条目 A" || got.Status != StatusProposed {
		t.Fatalf("部分更新不应触碰其他字段: %+v", got)
	}
	// 持久化：重建后批次字段仍在。
	s2, _ := NewStore(s.dir)
	got2, _ := s2.Get(it2.ID)
	if got2.BatchID != "roadmap_batch_1001" {
		t.Fatalf("批次应持久化: %+v", got2)
	}
	// 不存在的条目报错。
	if err := s.AttachBatch("rm_nope", "b", "b"); err == nil {
		t.Fatal("不存在的条目应报错")
	}
}

// TestRunLogAndBlocked 执行轨迹与 blocked 封存（用户需求 2026-10：
// 尝试次数/失败原因/改动文件/耗时全留痕；失败条目原因可追溯）。
func TestRunLogAndBlocked(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	it, _ := s.Add(Item{Title: "会失败的条目"})
	_, _ = s.SetStatus(it.ID, StatusApproved, "", "")
	// 第一次尝试：developing 计数 +1 → failed 记日志。
	if _, err := s.SetStatus(it.ID, StatusDeveloping, "", ""); err != nil {
		t.Fatalf("developing: %v", err)
	}
	got, _ := s.Get(it.ID)
	if got.Attempts != 1 {
		t.Fatalf("首次认领应计尝试次数 1: %d", got.Attempts)
	}
	if _, err := s.SetStatus(it.ID, StatusFailed, "", "编译失败: undefined: Foo"); err != nil {
		t.Fatalf("failed: %v", err)
	}
	// 补写改动文件（异步收口路径：先 SetStatus 后 AppendRunFiles）。
	if err := s.AppendRunFiles(it.ID, []string{"app.go", "internal/agent/engine.go"}); err != nil {
		t.Fatalf("AppendRunFiles: %v", err)
	}
	got, _ = s.Get(it.ID)
	run := got.LatestRun()
	if run == nil || run.Outcome != "failed" || run.Summary == "" {
		t.Fatalf("失败应记执行日志: %+v", run)
	}
	if len(run.Files) != 2 || run.Files[0] != "app.go" {
		t.Fatalf("改动文件应补写到最新日志: %+v", run.Files)
	}
	if run.DurationMS < 0 {
		t.Fatalf("耗时应非负: %d", run.DurationMS)
	}
	// 第二次尝试（自动重试路径）：计数累计。
	_, _ = s.SetStatus(it.ID, StatusApproved, "", "")
	_, _ = s.SetStatus(it.ID, StatusDeveloping, "", "")
	got, _ = s.Get(it.ID)
	if got.Attempts != 2 {
		t.Fatalf("重试应累计尝试次数 2: %d", got.Attempts)
	}
	if len(got.RunLog) != 1 {
		t.Fatalf("尚无终局，应只有 1 条日志: %d", len(got.RunLog))
	}
	// 尝试超限 → blocked 封存（developing → blocked 合法路径）。
	if _, err := s.SetStatus(it.ID, StatusBlocked, "", "重试超限（已尝试 2 次）: 编译失败"); err != nil {
		t.Fatalf("developing→blocked: %v", err)
	}
	got, _ = s.Get(it.ID)
	if got.Status != StatusBlocked || got.BlockedReason == "" {
		t.Fatalf("blocked 应记录封存原因: %+v", got)
	}
	if got.RunLog[0].Outcome != "blocked" {
		t.Fatalf("blocked 应记执行日志: %+v", got.RunLog[0])
	}
	// blocked 不再自动回队（非 developing → blocked 非法）。
	if _, err := s.SetStatus(got.ID, StatusBlocked, "", "x"); err == nil {
		t.Fatal("blocked→blocked 应被拒绝")
	}
	// 一键重跑：blocked → approved，错误/封存原因清除，日志与尝试次数保留。
	mr, err := s.MarkRetry(got.ID)
	if err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}
	if mr.Status != StatusApproved || mr.Error != "" || mr.BlockedReason != "" {
		t.Fatalf("重跑应回队并清错误: %+v", mr)
	}
	if mr.Attempts != 2 || len(mr.RunLog) != 2 {
		t.Fatalf("重跑应保留尝试次数与日志: attempts=%d logs=%d", mr.Attempts, len(mr.RunLog))
	}
	// 成功后日志记 done 且带结果摘要。
	_, _ = s.SetStatus(mr.ID, StatusDeveloping, "", "")
	_, _ = s.SetStatus(mr.ID, StatusDone, "完成：新增执行日志与重试机制", "")
	final, _ := s.Get(mr.ID)
	if final.RunLog[0].Outcome != "done" || final.RunLog[0].Summary == "" {
		t.Fatalf("done 应记执行日志与摘要: %+v", final.RunLog[0])
	}
	// 日志上限裁剪 + 最新在前。
	for i := 0; i < 60; i++ {
		_, _ = s.SetStatus(final.ID, StatusFailed, "", "失败循环")
	}
	final, _ = s.Get(final.ID)
	if len(final.RunLog) != maxRunLogEntries {
		t.Fatalf("日志应裁剪到上限: %d", len(final.RunLog))
	}
	// 持久化：重开 Store 日志仍在。
	s2, _ := NewStore(s.dir)
	final2, _ := s2.Get(final.ID)
	if len(final2.RunLog) != maxRunLogEntries || final2.BlockedReason != "" {
		t.Fatalf("执行日志应持久化: logs=%d blocked=%q", len(final2.RunLog), final2.BlockedReason)
	}
}

// TestAppendRunFilesDedup 改动文件清单清洗：去重 + 去空 + 上限。
func TestAppendRunFilesDedup(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	it, _ := s.Add(Item{Title: "文件清洗条目"})
	_, _ = s.SetStatus(it.ID, StatusApproved, "", "")
	_, _ = s.SetStatus(it.ID, StatusDeveloping, "", "")
	_, _ = s.SetStatus(it.ID, StatusDone, "完成", "")
	if err := s.AppendRunFiles(it.ID, []string{"a.go", " a.go ", "", "b.go"}); err != nil {
		t.Fatalf("AppendRunFiles: %v", err)
	}
	got, _ := s.Get(it.ID)
	if len(got.RunLog[0].Files) != 2 || got.RunLog[0].Files[0] != "a.go" {
		t.Fatalf("文件应去重去空: %v", got.RunLog[0].Files)
	}
}

// TestClaimConcurrency 认领防重：N 个调度方并发把同一条目 approved →
// developing，仅一个成功（同一条目不被并发重复开发的存储层保证）。
func TestClaimConcurrency(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	it, _ := s.Add(Item{Title: "并发认领条目"})
	_, _ = s.SetStatus(it.ID, StatusApproved, "", "")
	const n = 16
	success := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.SetStatus(it.ID, StatusDeveloping, "", ""); err == nil {
				success <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(success)
	if len(success) != 1 {
		t.Fatalf("并发认领应恰好成功 1 次，实际 %d", len(success))
	}
	got, _ := s.Get(it.ID)
	if got.Status != StatusDeveloping || got.Attempts != 1 {
		t.Fatalf("终态应为 developing 且尝试 1 次: %+v", got)
	}
}

// TestNormalizeRunFilesLimit 文件清单条数与路径长度上限。
func TestNormalizeRunFilesLimit(t *testing.T) {
	files := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		files = append(files, fmt.Sprintf("dir/file_%02d.go", i))
	}
	out := normalizeRunFiles(files)
	if len(out) != maxRunFiles {
		t.Fatalf("应截断到 %d 条: %d", maxRunFiles, len(out))
	}
	long := normalizeRunFiles([]string{strings.Repeat("长", 300)})
	if len(long) != 1 || len([]rune(long[0])) != maxRunFileRunes {
		t.Fatalf("路径应截断到 %d rune: %+v", maxRunFileRunes, long)
	}
	if normalizeRunFiles(nil) != nil || normalizeRunFiles([]string{"", " "}) != nil {
		t.Fatal("空清单应返回 nil")
	}
}

func TestNextRunAt(t *testing.T) {
	if !NextRunAt(time.Time{}, 0).After(time.Now().Add(-time.Second)) {
		t.Fatal("未运行过应立即触发")
	}
	last := time.Now().Add(-time.Hour)
	next := NextRunAt(last, 2*time.Hour)
	if diff := next.Sub(last); diff < 119*time.Minute || diff > 121*time.Minute {
		t.Fatalf("间隔应为约 2h: %v", diff)
	}
}
