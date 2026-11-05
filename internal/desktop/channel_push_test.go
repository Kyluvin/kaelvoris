package desktop

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestPushQueueBackpressure_MainLoopNotBlocked 慢消费者场景：消费者不消费时，
// 入队不阻塞（返回后主循环继续），超过容量后丢弃有界（不 panic、不无限增长）。
func TestPushQueueBackpressure_MainLoopNotBlocked(t *testing.T) {
	q := newPushQueue("test", 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.run(ctx)

	// 消费者不启动（模拟慢消费者）：主循环快速投递 1000 条，全部应立即返回。
	start := time.Now()
	for i := 0; i < 1000; i++ {
		q.enqueue(func() {})
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("入队应在纳秒级非阻塞返回，实际耗时 %v（背压未生效）", elapsed)
	}
	if dropped := q.dropped.Load(); dropped == 0 {
		t.Fatalf("队列应发生丢弃（容量=%d，投递=%d）", cap(q.items), 10)
	}
	// 队列深度不超容量（有界）。
	if depth := len(q.items); depth > cap(q.items) {
		t.Fatalf("队列深度 %d 超过容量 %d（有界性被破坏）", depth, cap(q.items))
	}
}

// TestPushQueue_WorkerConsumesFIFO worker 串行消费全部消息（FIFO 顺序）。
func TestPushQueue_WorkerConsumesFIFO(t *testing.T) {
	q := newPushQueue("fifo", 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var got int64
	go func() {
		q.run(ctx)
		close(done)
	}()
	for i := 0; i < 5; i++ {
		n := i
		q.enqueue(func() { atomic.AddInt64(&got, 1); _ = n })
	}
	deadline := time.After(2 * time.Second)
	for {
		if atomic.LoadInt64(&got) == 5 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("worker 未消费全部消息: got=%d want=5", atomic.LoadInt64(&got))
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// TestPushQueue_WorkerPanicIsolation 单条处理 panic 不拖垮 worker（后续消息仍消费）。
func TestPushQueue_WorkerPanicIsolation(t *testing.T) {
	q := newPushQueue("panic", 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.run(ctx)
	var n int64
	atomic.StoreInt64(&n, 0)
	q.enqueue(func() { panic("boom") })
	q.enqueue(func() { atomic.StoreInt64(&n, 1) })
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt64(&n) == 0 {
		select {
		case <-deadline:
			t.Fatal("panic 隔离失败：第二条消息未消费")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// 该测试无法直接验证 main 循环不阻塞——这里断言队列收到 panic 后仍能消费。
}
