package task

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTask 测试任务。
type fakeTask struct {
	id        string
	aborted   bool
	abandoned bool
}

func (f *fakeTask) TaskID() string    { return f.id }
func (f *fakeTask) IsAborted() bool   { return f.aborted }
func (f *fakeTask) IsAbandoned() bool { return f.abandoned }

func TestRegistry_PushMoveToTop(t *testing.T) {
	r := NewRegistry()
	r.Push(&fakeTask{id: "a"})
	r.Push(&fakeTask{id: "b"})
	r.Push(&fakeTask{id: "c"})
	// 栈序 = 数组序（最旧在前），栈顶 = 末尾（对齐 JS Array.push/pop）。
	if got := r.TaskIDs(); got[0] != "a" || got[2] != "c" {
		t.Fatalf("stack order = %v, want [a b c]", got)
	}
	// 已存在再 push = move-to-top（移除旧条目后追加到栈顶）。
	r.Push(&fakeTask{id: "a"})
	if got := r.TaskIDs(); got[2] != "a" || got[0] != "b" || r.Len() != 3 {
		t.Fatalf("after re-push: %v len=%d, want [b c a] len=3", got, r.Len())
	}
	if r.Current().TaskID() != "a" {
		t.Fatalf("current = %s, want a", r.Current().TaskID())
	}
}

func TestRegistry_PopFocusFallback(t *testing.T) {
	r := NewRegistry()
	r.Push(&fakeTask{id: "a"})
	r.Push(&fakeTask{id: "b"})
	// setCurrent 切焦点不改栈序。
	if err := r.SetCurrent("a"); err != nil {
		t.Fatal(err)
	}
	if r.Current().TaskID() != "a" {
		t.Fatal("SetCurrent failed")
	}
	if err := r.SetCurrent("nope"); err == nil {
		t.Fatal("SetCurrent unknown id should error")
	}
	r.Pop() // 弹出 b，焦点回到 a
	if r.Current().TaskID() != "a" {
		t.Fatalf("current after pop = %s, want a", r.Current().TaskID())
	}
	if r.Len() != 1 {
		t.Fatalf("len = %d, want 1", r.Len())
	}
}

func TestRegistry_RemoveAndHasRunning(t *testing.T) {
	r := NewRegistry()
	r.Push(&fakeTask{id: "a", aborted: true})
	r.Push(&fakeTask{id: "b"})
	if r.HasRunning("a") {
		t.Fatal("aborted task should not be running")
	}
	if !r.HasRunning("b") {
		t.Fatal("b should be running")
	}
	if r.Remove("b") == nil {
		t.Fatal("remove should return task")
	}
	if r.Len() != 1 || r.Current().TaskID() != "a" {
		t.Fatalf("after remove: len=%d current=%s", r.Len(), r.Current().TaskID())
	}
	if r.Remove("zzz") != nil {
		t.Fatal("remove unknown should return nil")
	}
}

func TestRegistry_ReplaceInPlace(t *testing.T) {
	r := NewRegistry()
	r.Push(&fakeTask{id: "a"})
	r.Push(&fakeTask{id: "b"})
	r.SetCurrent("a")
	if err := r.Replace("a", &fakeTask{id: "a2"}); err != nil {
		t.Fatal(err)
	}
	if r.Current().TaskID() != "a2" {
		t.Fatal("focus should follow replacement")
	}
	if ids := r.TaskIDs(); ids[0] != "a2" || ids[1] != "b" {
		t.Fatalf("stack = %v, want [a2 b]", ids)
	}
	if err := r.Replace("nope", &fakeTask{id: "x"}); err == nil {
		t.Fatal("replace unknown should error")
	}
}

func TestScheduler_SerialDefault(t *testing.T) {
	s := NewScheduler(1)
	var active, maxActive int32
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		ft := &fakeTask{id: "t"}
		go func() {
			defer wg.Done()
			s.Schedule(ft, func() {
				cur := atomic.AddInt32(&active, 1)
				if cur > maxActive {
					atomic.StoreInt32(&maxActive, cur)
				}
				time.Sleep(10 * time.Millisecond)
				atomic.AddInt32(&active, -1)
			})
		}()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("max concurrency = %d, want 1", maxActive)
	}
}

func TestScheduler_ParallelFanOut(t *testing.T) {
	s := NewScheduler(3)
	var active, maxActive int32
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		ft := &fakeTask{id: "t"}
		go func() {
			defer wg.Done()
			s.Schedule(ft, func() {
				cur := atomic.AddInt32(&active, 1)
				if cur > maxActive {
					atomic.StoreInt32(&maxActive, cur)
				}
				time.Sleep(80 * time.Millisecond)
				atomic.AddInt32(&active, -1)
			})
		}()
	}
	wg.Wait()
	if maxActive != 3 {
		t.Fatalf("max concurrency = %d, want 3", maxActive)
	}
}

func TestScheduler_CancelQueuedSkipsRun(t *testing.T) {
	s := NewScheduler(1)
	releaseFirst := make(chan struct{})
	var ran int32
	// 占满唯一许可。
	go s.Schedule(&fakeTask{id: "first"}, func() {
		<-releaseFirst
	})
	time.Sleep(50 * time.Millisecond)
	// 排队任务：随后被 cancelQueued 跳过。
	done := make(chan struct{})
	go func() {
		s.Schedule(&fakeTask{id: "queued"}, func() { atomic.AddInt32(&ran, 1) })
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	s.CancelQueued()
	close(releaseFirst)
	<-done
	if n := atomic.LoadInt32(&ran); n != 0 {
		t.Fatalf("cancelled queued task ran %d times", n)
	}
	if s.Waiting() != 0 {
		t.Fatalf("waiters = %d, want 0", s.Waiting())
	}
}

func TestScheduler_AbortedWhileWaiting(t *testing.T) {
	s := NewScheduler(1)
	gate := make(chan struct{})
	go s.Schedule(&fakeTask{id: "first"}, func() { <-gate })
	time.Sleep(30 * time.Millisecond)
	// 任务在等待许可期间被取消。
	ft := &fakeTask{id: "late", aborted: true}
	var ran int32
	done := make(chan struct{})
	go func() {
		s.Schedule(ft, func() { atomic.AddInt32(&ran, 1) })
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	<-done
	if n := atomic.LoadInt32(&ran); n != 0 {
		t.Fatalf("aborted task ran %d times", n)
	}
}

func TestScheduler_SetMaxRaiseWakesWaiting(t *testing.T) {
	s := NewScheduler(1)
	gate := make(chan struct{})
	go s.Schedule(&fakeTask{id: "first"}, func() { <-gate })
	time.Sleep(30 * time.Millisecond)
	// 第二个任务在许可不足时排队等待。
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.Schedule(&fakeTask{id: "second"}, func() { close(started); time.Sleep(10 * time.Millisecond) })
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	// 升高上限 → 排队任务被唤醒执行（许可充足）。
	s.SetMax(2)
	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("queued task not woken after SetMax raise")
	}
	close(gate)
	<-done
}

func TestScheduler_SetMaxLowerDoesNotInterrupt(t *testing.T) {
	s := NewScheduler(2)
	gate1 := make(chan struct{})
	gate2 := make(chan struct{})
	go s.Schedule(&fakeTask{id: "a"}, func() { <-gate1 })
	go s.Schedule(&fakeTask{id: "b"}, func() { <-gate2 })
	time.Sleep(50 * time.Millisecond)
	// 降低上限：正在运行的两个任务不受影响。
	s.SetMax(1)
	if got := s.Waiting(); got != 0 {
		t.Fatalf("waiting = %d, want 0", got)
	}
	// 再提交第三个任务：上限 1 已被 2 个任务占用 → 排队。
	ran := make(chan struct{})
	go s.Schedule(&fakeTask{id: "c"}, func() { close(ran) })
	time.Sleep(50 * time.Millisecond)
	select {
	case <-ran:
		t.Fatal("task should be queued after lowering limit")
	default:
	}
	close(gate1)
	close(gate2)
	<-ran
}
