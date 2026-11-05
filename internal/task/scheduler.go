package task

import "sync"

// semaphore 计数信号量（移植自 Zoo-Code utils/TaskSemaphore.ts）。
// acquire 返回 release 闭包（幂等）；等待期间发生 cancel 则该次获取失败。
type semaphore struct {
	mu      sync.Mutex
	cond    *sync.Cond
	permits int
	waiters int    // 等待许可的任务数（对应 TaskSemaphore.waiting）
	gen     uint64 // cancel 代次：等待者据此感知取消
	max     int    // 当前并发上限（随 SetMax 动态调整）
}

func newSemaphore(n int) *semaphore {
	s := &semaphore{permits: n, max: n}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// acquire 获取一个许可；返回 release 与是否成功（false = 等待期间被取消）。
func (s *semaphore) acquire() (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gen
	s.waiters++
	for s.permits == 0 && g == s.gen {
		s.cond.Wait()
	}
	s.waiters--
	if g != s.gen {
		// 等待期间被 cancel：该等待者从未扣减许可，直接失败返回。
		// （修复崩溃级缺陷：原实现误将 permits++ 当作"归还"——实际是凭空
		//  膨胀总许可数。每次取消排队任务若恰逢空闲许可，并发上限永久 +1，
		//  长期运行下超额并发不受控，直至资源耗尽/进程崩溃。）
		return nil, false
	}
	s.permits--
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.permits++
			s.cond.Broadcast()
			s.mu.Unlock()
		})
	}, true
}

// cancel 唤醒全部等待者（其 acquire 以失败返回）。
func (s *semaphore) cancel() {
	s.mu.Lock()
	s.gen++
	s.cond.Broadcast()
	s.mu.Unlock()
}

// setMax 动态调整并发上限（设置界面调节用；立即生效）。
// 升高：立即增加可用许可并唤醒等待者；降低：仅停止新分配，运行中的任务
// 不中断（活跃数可能短暂超过新上限，任务释放后收敛），等待者继续排队。
func (s *semaphore) setMax(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 活跃数 = 上限 - 剩余许可；以此为基准换算新上限下的剩余许可。
	active := s.max - s.permits
	if active < 0 {
		active = 0
	}
	s.max = n
	s.permits = n - active
	if s.permits < 0 {
		s.permits = 0
	}
	s.cond.Broadcast()
}

// Scheduler 任务并发门控（移植自 Zoo-Code TaskScheduler.ts）。
// maxConcurrency=1 时与 Zoo-Code 出厂串行行为一致；调高即启用 fan-out 并行。
type Scheduler struct {
	sem *semaphore
}

// NewScheduler 创建调度器（maxConcurrency <= 0 视为 1）。
func NewScheduler(maxConcurrency int) *Scheduler {
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	return &Scheduler{sem: newSemaphore(maxConcurrency)}
}

// Waiting 等待许可的任务数（对齐 TaskScheduler.waiting）。
func (s *Scheduler) Waiting() int {
	s.sem.mu.Lock()
	defer s.sem.mu.Unlock()
	return s.sem.waiters
}

// Schedule 为 task 获取许可后运行 run()，结束保证释放许可（try/finally 语义）。
// 等待许可期间任务被取消/废弃（abort|abandoned）时，直接释放许可且不运行——
// 对齐 Zoo-Code schedule 中 acquire 后的 abort 检查。
// 排队期间被 CancelQueued 的任务不运行（acquire 失败）。
func (s *Scheduler) Schedule(t Task, run func()) {
	release, ok := s.sem.acquire()
	if !ok {
		return
	}
	if t != nil && (t.IsAborted() || t.IsAbandoned()) {
		release()
		return
	}
	defer release()
	run()
}

// CancelQueued 取消全部排队任务；已持有许可的任务不受影响，继续运行至结束。
func (s *Scheduler) CancelQueued() {
	s.sem.cancel()
}

// SetMax 动态调整最大并行数（设置界面调节用；立即生效）。
// 升高：立即唤醒排队等待者；降低：不中断运行中任务，释放后收敛到新上限。
func (s *Scheduler) SetMax(n int) {
	s.sem.setMax(n)
}
