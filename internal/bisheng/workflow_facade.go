// Workflow 门面（对齐 graph/workflow.py Workflow）：组装引擎 + 驱动状态机
// 到终态。kaelvoris 宿主（桌面端/CLI/IM 通道）经此入口执行 bisheng 画布。
package bisheng

// Workflow 工作流门面（蓝本 Workflow）。
type Workflow struct {
	engine *Engine
	// TimeoutMin 等待用户输入超时（分钟；蓝本 timeout 字段，超时置 FAILED。
	// 蓝本由 Celery beat 轮询实现，kaelvoris 宿主自行定时调用 CheckTimeout）。
	TimeoutMin    int
	startedAtUnix int64
}

// New 构建工作流（蓝本 Workflow.__init__）。
func New(wfID, wfName string, data WorkflowData, maxSteps, timeoutMin int, cb Callback, deps Deps) (*Workflow, error) {
	if wfName == "" {
		wfName = wfID
	}
	if maxSteps <= 0 {
		maxSteps = 20 // 蓝本默认值由调用方传入，此处收敛非零下限
	}
	eng, err := NewEngine(wfID, wfName, data, maxSteps, cb, deps)
	if err != nil {
		return nil, err
	}
	return &Workflow{engine: eng, TimeoutMin: timeoutMin}, nil
}

// Run 执行到终态（蓝本 run：首轮 + 循环消化 RUNNING；本实现中断/失败即时
// 返回 —— 蓝本 while 循环消化的是异步任务排队，同步实现无此需求）。
// 返回 (status, reason)。
func (w *Workflow) Run() (string, string) {
	w.engine.Run()
	return w.engine.Status(), w.engine.Reason()
}

// Continue 用户回填后续跑到下一终态（蓝本 continue_run + while 循环）。
// input: {node_id: {key: value}}。
func (w *Workflow) Continue(input map[string]map[string]any) (string, string) {
	w.engine.Continue(input)
	return w.engine.Status(), w.engine.Reason()
}

// Engine 暴露引擎（宿主查状态/变量/历史）。
func (w *Workflow) Engine() *Engine { return w.engine }

// Status 当前状态。
func (w *Workflow) Status() string { return w.engine.Status() }

// Reason 当前原因。
func (w *Workflow) Reason() string { return w.engine.Reason() }

// Stop 用户停止。
func (w *Workflow) Stop() { w.engine.Stop() }
