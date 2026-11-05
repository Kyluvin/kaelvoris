package cron

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Runner 定时任务触发回调：调度器到期时调用（实现方送入现有会话执行链）。
// 返回 sessionId 与 error（用于执行记录）。
type Runner func(job Job) (sessionID string, err error)

	// FlowLauncher flow 启动回调（Action=run_flow 任务到期时调用）：由宿主注入
	// （桌面端桥接 internal/flow 启动链路），cron 包不反向依赖 flow（内核零耦合，
	// 对齐 Runner.KB/LLMFor 的回调注入模式）。返回（会话/运行标识, 错误）。
	type FlowLauncher func(job Job) (runID string, err error)

	// ScriptRunner 前置脚本执行回调：执行脚本并返回 (stdout, stderr, err)。
	// 由宿主注入（桌面端用 os/exec 跑脚本）；cron 包不依赖 exec/fs，内核零耦合
	//（对齐 Runner/FlowLauncher 注入模式）。stdout 注入任务上下文，stderr 供排查。
	type ScriptRunner func(job Job, ctx context.Context) (stdout, stderr string, err error)

// Scheduler 应用内 cron 调度器：分钟级 tick（30s 轮询），到期触发回调。
// 可靠性语义（借鉴 GitHub Actions schedule/timeout、chroniker 熔断、
// robfig/cron 描述符）：
//   - 防重入：上次触发仍运行中则跳过本次（记录 skipped）；
//   - 运行时间窗口：TimeWindowEnabled 开启后仅允许 StartHHMM～EndHHMM
//     （支持跨零点，默认 18:00～06:00）时段内触发，窗口外记 skipped
//     （RunNow 手动触发不受窗口限制）；
//   - 超时：单次运行超过 Job.TimeoutMin（默认 DefaultTimeoutMin）强制解除
//     占用（记 timeout），下次到期可再触发——修复"任务假死占用"问题；
//   - 失败重试：Job.RetryLimit > 0 时按 RetryBackoff 自动重试（记 retry；
//     对齐 GitHub Actions retry 语义，重试不叠加触发新会话之外的副作用）；
//   - 熔断：连续失败达 MaxFailStreak 自动停用任务（防失败风暴），
//     DisabledReason 记录原因，用户排查后手动重新启用即清零。
//   - @every：固定间隔模式，按"上次触发时间 + 间隔"推进（自上次触发起算，
//     对齐 robfig/cron 定时器语义而非固定整点网格）。
type Scheduler struct {
	store  *Store
	runner Runner
	// flowLauncher run_flow 动作任务的启动回调（可空：nil 时此类任务触发
	// 即报错，不静默丢弃——执行记录 failed 可见）。
	flowLauncher FlowLauncher
	// scriptRunner 前置脚本执行回调（可空；F1/F3 使用。nil 时 Script/NoAgent 任务
	// 触发即报错，不静默丢弃——执行记录 failed 可见）。
	scriptRunner ScriptRunner
	tick        time.Duration

	stop   chan struct{}
	stopMu sync.Mutex
	once   sync.Once

	mu      sync.Mutex
	running map[string]*runState // jobID → 运行态（开始时间，用于超时解除）
	done    chan struct{}        // 非零表示已 Start
}

// runState 一次运行的占用状态（超时解除依据）。
type runState struct {
	startedAt time.Time
}

// NewScheduler 创建调度器（不启动；Start 后台运行）。
func NewScheduler(store *Store, runner Runner) *Scheduler {
	return &Scheduler{
		store:   store,
		runner:  runner,
		tick:    30 * time.Second,
		stop:    make(chan struct{}),
		running: map[string]*runState{},
	}
}

	// SetFlowLauncher 注入 flow 启动回调（Action=run_flow 任务分发用；须在
	// Start 之前调用，现有装配点 NewScheduler 后立即满足）。
	func (s *Scheduler) SetFlowLauncher(fl FlowLauncher) { s.flowLauncher = fl }

	// SetScriptRunner 注入前置脚本执行回调（F1 前置脚本 / F3 no-agent 纯脚本；
	// 须在 Start 之前调用）；不注入时 Script/NoAgent 任务触发即报错（执行记录可见）。
	func (s *Scheduler) SetScriptRunner(sr ScriptRunner) { s.scriptRunner = sr }

	// IsSilentResult 判断运行结果是否为静默标记 [SILENT]（对标 Hermes [SILENT]）：
	// 去除首尾空白后等值 "[SILENT]"（不区分大小写）即视为静默——成功运行不打扰，
	// 仅在有内容时通知。只对成功结果生效（失败仍按 failed 通知）。
	func IsSilentResult(text string) bool {
		return strings.EqualFold(strings.TrimSpace(text), "[SILENT]")
	}

// Start 启动调度循环（goroutine；重复调用无害）。
func (s *Scheduler) Start() {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if s.done != nil {
		return
	}
	s.done = make(chan struct{})
	done := s.done
	go func() {
		defer close(done)
		// panic 防护（崩溃修复）：调度循环是常驻 goroutine，tickOnce（含
		// store/runner 调用链）内 panic 会杀死整个进程；拦截后重建循环，
		// 调度能力不随单次异常丢失。
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("cron 调度循环 panic（已拦截，重建循环）", "panic", r, "stack", string(debug.Stack()))
					}
				}()
				// 启动即补算 NextRun 并检查错过窗口（上次运行期间到期的任务立即补跑一次）。
				s.tickOnce(time.Now(), true)
				t := time.NewTicker(s.tick)
				defer t.Stop()
				for {
					select {
					case <-t.C:
						s.tickOnce(time.Now(), false)
					case <-s.stop:
						return
					}
				}
			}()
			// panic 后走到这里：stop 已关闭则退出，否则重建循环。
			select {
			case <-s.stop:
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

// Stop 停止调度循环（等待退出；重复调用无害）。
func (s *Scheduler) Stop() {
	s.stopMu.Lock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	done := s.done
	s.stopMu.Unlock()
	if done != nil {
		<-done
	}
}

// isBusy 任务是否占用中（超时的运行视为已解除）。
func (s *Scheduler) isBusy(jobID string, timeout time.Duration, now time.Time) (*runState, bool) {
	st, ok := s.running[jobID]
	if !ok {
		return nil, false
	}
	if timeout > 0 && now.Sub(st.startedAt) >= timeout {
		// 运行超过上限：视为假死，解除占用（记 timeout 日志；原 goroutine
		// 结束后 delete 是幂等的，不会误伤新运行态）。
		slog.Warn("cron 任务运行超时，强制解除占用", "job", jobID,
			"startedAt", st.startedAt.Format("15:04:05"), "timeout", timeout)
		return nil, false
	}
	return st, true
}

// nextFire 计算任务下次触发时间：
//   - @every：NextRetry 优先（重试等待），否则上次触发 + 间隔；
//   - 标准式：Parse 后 Next(now)。
func (s *Scheduler) nextFire(j Job, sched *Schedule, now time.Time) time.Time {
	if sched.every > 0 {
		if !j.NextRetry.IsZero() {
			return j.NextRetry
		}
		if !j.LastRun.IsZero() {
			return sched.Next(j.LastRun)
		}
		return sched.Next(now)
	}
	return sched.Next(now)
}

// tickOnce 单轮调度：扫描启用任务，命中即触发。
// catchup=true 时允许触发"上次 tick 后过期"的窗口（<=2 分钟），避免错过整点任务。
func (s *Scheduler) tickOnce(now time.Time, catchup bool) {
	jobs := s.store.List()
	window := now
	next := map[string]time.Time{}
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		sched, err := Parse(j.Expr)
		if err != nil {
			slog.Warn("cron 表达式非法，跳过", "job", j.ID, "expr", j.Expr, "err", err)
			continue
		}
		nf := sched.Next(now)
		if sched.every > 0 {
			// @every：按“上次触发 + 间隔”或重试时间推进。
			nf = s.nextFire(j, sched, now)
		}
		next[j.ID] = nf
		due := false
		if sched.every > 0 {
			due = !nf.After(now)
		} else if j.NextRun.IsZero() {
			// 从未排程：直接计算本分钟是否命中。
			due = sched.Match(now)
		} else {
			due = !j.NextRun.After(window)
		}
		if !due {
			continue
		}
		// 运行时间窗口门控：开启后仅允许窗口内触发，窗口外记 skipped
		//（任务本身仍“启用”，只是本次不在允许时段内）。
		if !j.InTimeWindow(now) {
			_ = s.store.MarkRun(j.ID, now, Run{
				JobID: j.ID, JobName: j.Name, StartedAt: now, Status: "skipped",
				Error: "当前时间不在指定运行时间窗口内，本次跳过",
			})
			continue
		}
		if busy, _ := s.tryOccupy(j.ID, DefaultTimeout(j), now); busy {
			_ = s.store.MarkRun(j.ID, now, Run{
				JobID: j.ID, JobName: j.Name, StartedAt: now, Status: "skipped",
				Error: "上次运行尚未结束，本次跳过",
			})
			continue
		}
		_ = catchup
		go s.runJob(j, now, 1)
	}
	if len(next) > 0 {
		s.store.UpdateNextRun(next)
	}
}

// DefaultTimeout 任务超时上限（Job.TimeoutMin；0 = 默认值）。
func DefaultTimeout(j Job) time.Duration {
	if j.TimeoutMin > 0 {
		return time.Duration(j.TimeoutMin) * time.Minute
	}
	return DefaultTimeoutMin * time.Minute
}

// tryOccupy 尝试占用任务运行位；返回是否已占用（busy=true 表示被占，跳过）。
func (s *Scheduler) tryOccupy(jobID string, timeout time.Duration, now time.Time) (busy bool, st *runState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.running[jobID]; ok {
		if timeout <= 0 || now.Sub(cur.startedAt) < timeout {
			return true, cur
		}
		// 超时假死：解除旧占用，放行本次。
		slog.Warn("cron 任务超时解除占用", "job", jobID, "since", cur.startedAt.Format("15:04:05"))
	}
	s.running[jobID] = &runState{startedAt: now}
	return false, nil
}

// release 释放任务运行位（仅当仍是本次运行登记的状态时才删除，防误删新运行态）。
func (s *Scheduler) release(jobID string, st *runState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.running[jobID]; ok && cur == st {
		delete(s.running, jobID)
	}
}

	// dispatchResult 分派结果：sessionID（agent 会话 / run_flow 运行标识）、
	// silent（no-agent 脚本输出为 [SILENT] 时）、resultText（no-agent 脚本 stdout verbatim）。
	type dispatchResult struct {
		sessionID  string
		silent     bool
		resultText string
	}

	// runJob 执行一次任务（异步；结束回写记录并解除重入标记）。
	// attempt 从 1 起（1 = 首次；>1 = 失败重试）。
	func (s *Scheduler) runJob(j Job, at time.Time, attempt int) {
		// panic 防护：runner 调用链 panic 不杀死进程，按 failed 记录并走重试/熔断。
		var out dispatchResult
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("cron 任务 panic", "job", j.ID, "attempt", attempt, "recover", r, "stack", string(debug.Stack()))
					err = &runPanicError{r}
				}
			}()
				out, err = s.dispatch(j)
			}()

		// 运行位释放：成功/失败都释放（超时假死由 tryOccupy 兜底）。
		if st := s.currentRunState(j.ID); st != nil {
			s.release(j.ID, st)
		}

		status := "done"
		errMsg := ""
		if err != nil {
			status = "failed"
			errMsg = err.Error()
		} else if out.silent {
			// F2 [SILENT] 静默：成功但仅输出 [SILENT]（no-agent 场景在调度器判定），
			// 记 silent 状态，不推送完成/失败通知（对标 Hermes）。
			status = StatusSilent
		}
		_ = s.store.MarkRun(j.ID, at, Run{
			JobID: j.ID, JobName: j.Name, SessionID: out.sessionID, StartedAt: at,
			Status: status, Error: errMsg, Attempt: attempt,
		})

		// 失败处理：重试（退避）或熔断。
		if err != nil {
			s.handleFailure(j, at, attempt, errMsg)
			return
		}
		s.store.ClearFailure(j.ID)
	}

// runPanicError runner panic 转错误（保留 recover 值）。
type runPanicError struct{ v any }

func (e *runPanicError) Error() string { return "任务执行 panic: " + safeString(e.v) }

func safeString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "runtime panic"
}

	// dispatch 按任务动作分派：
	//   - run_flow：走 flow 启动回调（未注入即报错，执行记录可见）；
	//   - no-agent 纯脚本（F3，对齐 Hermes no_agent）：只运行 Script，stdout verbatim
	//     作为结果，不构造 agent 会话、零 LLM；脚本失败按 failed 记录；
	//     无脚本回调（scriptRunner==nil）即报错，不静默丢弃（执行记录 failed 可见）；
	//   - 其余：送入现有会话执行链（存量语义）。
	func (s *Scheduler) dispatch(j Job) (dispatchResult, error) {
		if j.Action == ActionRunFlow {
			if s.flowLauncher == nil {
				return dispatchResult{}, fmt.Errorf("任务 %s 为 run_flow 动作，但调度器未注入 flow 启动回调", j.ID)
			}
			runID, err := s.flowLauncher(j)
			return dispatchResult{sessionID: runID}, err
		}
		// F3 no-agent 纯脚本：零 LLM，不构造会话。
		if j.NoAgent {
			if s.scriptRunner == nil {
				return dispatchResult{}, fmt.Errorf("任务 %s 为 no-agent 纯脚本任务，但调度器未注入脚本执行回调", j.ID)
			}
			stdout, _, err := s.scriptRunner(j, context.Background())
			if err != nil {
				return dispatchResult{}, err
			}
			// stdout verbatim 作为结果；支持 [SILENT] 静默判定（F2 复用）。
			return dispatchResult{silent: IsSilentResult(stdout), resultText: stdout}, nil
		}
		// F1 前置脚本：agent 任务执行前先跑脚本，stdout/stderr 注入任务上下文。
		if s.scriptRunner != nil && strings.TrimSpace(j.Script) != "" {
			stdout, stderr, serr := s.scriptRunner(j, context.Background())
			if serr != nil {
				// 脚本失败不阻断：注入错误信息，模型可基于错误推理（F1-R2）。
				j.Task = "[script error] " + serr.Error() + "\n\n" + j.Task
			} else {
				j.Task = s.injectScriptOutput(j.Task, stdout, stderr)
			}
		}
		sessID, err := s.runner(j)
		return dispatchResult{sessionID: sessID}, err
	}

	// injectScriptOutput 把脚本 stdout/stderr 注入任务文本前缀（[script output]/[script stderr] 段）。
	// fresh session 首帧可见；非空才注入，空输出不占上下文。
	func (s *Scheduler) injectScriptOutput(task, stdout, stderr string) string {
		var b strings.Builder
		if strings.TrimSpace(stdout) != "" {
			b.WriteString("[script output]\n" + stdout)
			if strings.TrimSpace(stderr) != "" {
				b.WriteString("\n\n")
			}
		}
		if strings.TrimSpace(stderr) != "" {
			b.WriteString("[script stderr]\n" + stderr)
		}
		if b.Len() == 0 {
			return task
		}
		return b.String() + "\n\n" + task
	}

// handleFailure 失败后的重试/熔断决策：
//   - attempt < RetryLimit+1：安排 NextRetry = now + RetryBackoff（@every 与
//     标准式通用——标准式重试到期按 NextRetry 触发一次）；
//   - 连败达 MaxFailStreak：熔断停用（Enabled=false + DisabledReason）。
func (s *Scheduler) handleFailure(j Job, at time.Time, attempt int, errMsg string) {
	j2, circuit := s.store.RecordFailure(j.ID)
	if circuit {
		slog.Warn("cron 任务连续失败熔断", "job", j.ID, "name", j.Name, "streak", j2.FailStreak, "lastErr", errMsg)
		return
	}
	limit := j.RetryLimit
	if limit < 0 || limit > MaxRetryLimit {
		limit = MaxRetryLimit
	}
	if attempt <= limit {
		retryAt := at.Add(RetryBackoff)
		s.store.SetNextRetry(j.ID, retryAt)
		slog.Info("cron 任务失败，安排重试", "job", j.ID, "attempt", attempt, "retryAt", retryAt.Format("15:04"), "err", errMsg)
	}
}

// currentRunState 读取当前运行态（无则 nil）。
func (s *Scheduler) currentRunState(jobID string) *runState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[jobID]
}

// RunNow 立即触发一次（不经过到期判断；仍受防重入约束）。
func (s *Scheduler) RunNow(id string) (string, error) {
	j, ok := s.store.Get(id)
	if !ok {
		return "", ErrJobNotFound
	}
	busy, _ := s.tryOccupy(j.ID, DefaultTimeout(j), time.Now())
	if busy {
		return "", ErrBusy
	}
	go s.runJob(j, time.Now(), 1)
	return "", nil
}

// RunningIDs 当前运行中（未超时）的任务 ID 集合（前端运行徽标）。
func (s *Scheduler) RunningIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.running))
	for id := range s.running {
		out = append(out, id)
	}
	return out
}
