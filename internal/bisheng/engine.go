// GraphEngine 图执行器（对齐 graph/graph_engine.py；langgraph StateGraph +
// MemorySaver + interrupt_before 的零依赖等价实现）。
//
// 执行模型（与蓝本语义逐条对应）：
//   - 蓝本经 langgraph 拓扑边顺序流式推进，interrupt_before 在 INPUT /
//     output_fake 节点前挂起；本实现用「单步推进 + 激活集」等价建模：
//     active 集合 = 下一批可执行节点；每轮取队首节点，交互节点未回填时
//     保留在队首挂起（等价 interrupt_before 不执行该节点）；
//   - 条件互斥（condition / choose 型 output）经 RouteNode() 决定唯一后继
//     （蓝本 add_conditional_edges + route_node）；output 交互节点先流经
//     fake 影子节点再路由（蓝本 add_edge(output, fake) + conditional(fake)）；
//   - fan-in 汇聚：多前驱节点按蓝本 parse_fan_in_node 判定——互斥分支汇合
//     放行（分支剥离条件/目标节点后不相交），否则等待全部前驱完成；
//   - 循环：变量池跨轮保留 + MaxSteps 熔断（蓝本 recursion_limit 等价）；
//   - 中断：input / output(input|choose 型) 挂起等待回填，Continue(input)
//     后从断点续跑（蓝本 continue_run + handle_input）。
package bisheng

import (
	"fmt"
	"sync"
)

// Engine 图执行器（蓝本 GraphEngine）。
type Engine struct {
	WorkflowID   string
	WorkflowName string

	edges *EdgeManage
	state *State
	deps  Deps
	cb    Callback
	steps int // max_steps（蓝本 max_steps）

	nodes     map[string]Node
	fanIn     map[string][]string // node_id → 前驱列表（蓝本 nodes_fan_in）
	levels    map[string]int      // node_id → start 最长路径层级（蓝本 node_level）
	condNodes []string            // 互斥节点列表（蓝本 condition_nodes）
	fakeOf    map[string]string   // output 节点 ID → fake 影子节点 ID

	// 运行态
	mu       sync.Mutex
	active   []string // 待执行节点集（蓝本 langgraph 的 next 游标）
	status   string   // WorkflowStatus
	reason   string   // 失败/等待原因
	suspend  bool     // 是否挂起等待输入
	filled   map[string]bool // 中断节点已回填标记（Continue 时置位）
}

// WorkflowData 工作流定义（蓝本 workflow_data：nodes+edges 数组，前端画布直传）。
type WorkflowData struct {
	Nodes []BaseNodeData `json:"nodes"`
	Edges []EdgeBase     `json:"edges"`
}

// NewEngine 构建引擎（蓝本 __init__：build_edges → build_nodes）。
func NewEngine(wfID, wfName string, data WorkflowData, maxSteps int, cb Callback, deps Deps) (*Engine, error) {
	if len(data.Nodes) == 0 {
		return nil, fmt.Errorf("workflow must have at least one node")
	}
	e := &Engine{
		WorkflowID:   wfID,
		WorkflowName: wfName,
		state:        NewState(),
		deps:         deps,
		cb:           cb,
		steps:        maxSteps,
		nodes:        map[string]Node{},
		fanIn:        map[string][]string{},
		levels:       map[string]int{},
		fakeOf:       map[string]string{},
		filled:       map[string]bool{},
		status:       StatusRunning,
	}
	e.edges = NewEdgeManage(data.Edges)

	// init_nodes（蓝本）：note 跳过、逐节点实例化、识别 start/end/input/output
	startNode := ""
	var endNodes []string
	for _, nd := range data.Nodes {
		if nd.Type == NodeNote {
			continue
		}
		inst, err := InstanceNode(nd, e.state, e.edges, maxSteps, cb, deps)
		if err != nil {
			return nil, err
		}
		if inst.IsConditionNode() {
			e.condNodes = append(e.condNodes, inst.ID())
		}
		e.nodes[inst.ID()] = inst
		e.fanIn[inst.ID()] = e.edges.GetSourceNode(inst.ID())
		switch inst.Type() {
		case NodeStart:
			startNode = inst.ID()
		case NodeEnd:
			endNodes = append(endNodes, inst.ID())
		case NodeInput:
			// 交互中断点（蓝本 interrupt_nodes.append）
		case NodeOutput:
			// output 交互型 → fake 影子节点（蓝本 OutputFakeNode）：
			// output 执行后流经 fake，中断发生在 fake 上，回填后由 fake 路由放行。
			if out, ok := inst.(*OutputNode); ok && out.GetInputSchema() != nil {
				fake := &OutputFakeNode{output: out}
				InitBaseNode(&fake.BaseNode, BaseNodeData{
					ID:   inst.ID() + "_fake",
					Type: "fake_output",
					Name: inst.Name(),
				}, e.state, nil, maxSteps, cb)
				e.nodes[fake.ID()] = fake
				e.fakeOf[inst.ID()] = fake.ID()
			}
		}
	}
	_ = endNodes
	if startNode == "" {
		return nil, fmt.Errorf("workflow must have start node")
	}
	// 拓扑层级（蓝本 build_node_level：DFS 最长路径，环由 nodeMap 阻断）
	e.buildLevel(startNode, map[string]bool{}, 0)
	// 孤立节点校验（蓝本 add_node_edge：无任何连线报错）
	for _, n := range e.nodes {
		if n.Type() == NodeEnd || n.Type() == "fake_output" {
			continue
		}
		if len(e.edges.GetTargetNode(n.ID())) == 0 && len(e.edges.GetSourceNode(n.ID())) == 0 {
			return nil, fmt.Errorf("node %s(%s) must have at least one edge", n.Name(), n.ID())
		}
	}
	e.active = []string{startNode}
	return e, nil
}

// buildLevel 节点层级（蓝本 build_node_level.mark_node_level：DFS，已访问即回溯
// —— 环不无限展开；多路径取最大层级）。
func (e *Engine) buildLevel(nodeID string, nodeMap map[string]bool, level int) {
	if nodeMap[nodeID] {
		return
	}
	if level > e.levels[nodeID] {
		e.levels[nodeID] = level
	}
	nodeMap[nodeID] = true
	for _, next := range e.edges.GetTargetNode(nodeID) {
		nm := map[string]bool{}
		for k := range nodeMap {
			nm[k] = true
		}
		e.buildLevel(next, nm, level+1)
	}
}

// State 全局状态（宿主查变量/历史用）。
func (e *Engine) State() *State { return e.state }

// Status 当前状态（蓝本 status）。
func (e *Engine) Status() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// Reason 失败/等待原因（蓝本 reason）。
func (e *Engine) Reason() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reason
}

// stopAll 全节点停止（蓝本 GraphEngine.stop）。
func (e *Engine) stopAll() {
	for _, n := range e.nodes {
		n.Stop()
	}
}

// Run 首次执行（蓝本 run → graph.stream({"flag": True})）。
func (e *Engine) Run() { e.step() }

// Continue 用户回填后续跑（蓝本 continue_run：data = {node_id: {key: value}}，
// 逐节点 handle_input 后恢复推进）。
func (e *Engine) Continue(input map[string]map[string]any) {
	e.mu.Lock()
	if !e.suspend {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	for nodeID, params := range input {
		inst, ok := e.nodes[nodeID]
		if !ok {
			continue
		}
		inst.HandleInput(params)
		// 蓝本：output 型交互回填到 output 节点（fake 的宿主），fake 节点
		// 断点同样视为已回填。
		e.mu.Lock()
		e.filled[nodeID] = true
		if fakeID, ok := e.fakeOf[nodeID]; ok {
			e.filled[fakeID] = true
		}
		e.suspend = false
		e.status = StatusRunning
		e.mu.Unlock()
	}
	e.step()
}

// Stop 用户停止（蓝本 stop）。
func (e *Engine) Stop() { e.stopAll() }

// step 单步推进主循环（蓝本 graph.stream + judge_status 的合并等价）：
// 逐个消化 active 队首；交互节点未回填 → 挂起（节点保留在队首 = langgraph
// interrupt_before 断点）；耗尽即按状态判定收尾。
func (e *Engine) step() {
	e.mu.Lock()
	if e.status == StatusFailed || e.suspend {
		e.mu.Unlock()
		return
	}
	e.status = StatusRunning
	e.mu.Unlock()

	for {
		e.mu.Lock()
		if len(e.active) == 0 {
			// 执行完成（蓝本 judge_status：next 为空 → SUCCESS）；
			// FAILED 已发生时不得覆盖（MaxSteps 熔断后 active 耗尽的场景）。
			if e.status == StatusRunning {
				e.status = StatusSuccess
			}
			e.mu.Unlock()
			return
		}
		nodeID := e.active[0]
		inst, ok := e.nodes[nodeID]
		if !ok {
			e.status = StatusFailed
			e.reason = fmt.Sprintf("node %s not found", nodeID)
			e.mu.Unlock()
			return
		}
		// 交互节点中断判定（蓝本 interrupt_before + judge_status）：
		// input / fake_output 节点未回填 → 状态置 INPUT 挂起（节点保留队首）。
		if (inst.Type() == NodeInput || inst.Type() == "fake_output") && !e.filled[nodeID] {
			if schema := inst.GetInputSchema(); schema != nil {
				e.status = StatusInput
				e.suspend = true
				e.mu.Unlock()
				if fake, ok2 := inst.(*OutputFakeNode); ok2 {
					e.cb.OnUserInput(UserInputData{NodeID: fake.output.ID(), Name: fake.output.Name(), InputSchema: schema})
				} else {
					e.cb.OnUserInput(UserInputData{NodeID: inst.ID(), Name: inst.Name(), InputSchema: schema})
				}
				return
			}
		}
		e.active = e.active[1:]
		e.execute(nodeID, inst)
		if e.status == StatusFailed {
			e.mu.Unlock()
			return
		}
		e.mu.Unlock()
	}
}

// execute 执行单节点并推进后继（蓝本 add_node_edge 拓扑 + route_node 合并）。
func (e *Engine) execute(nodeID string, inst Node) {
	defer func() {
		if r := recover(); r != nil {
			switch err := r.(type) {
			case *ignoreError:
				// 蓝本 IgnoreException：FAILED + reason（日志 warning）
				e.status = StatusFailed
				e.reason = err.msg
			case stopError:
				e.status = StatusFailed
				e.reason = "stop by user"
			default:
				e.status = StatusFailed
				e.reason = fmt.Sprintf("%v", r)
			}
		}
	}()

	// END 节点：无后继（蓝本 end → END 收尾）
	if inst.Type() == NodeEnd {
		inst.Run(newExecID())
		return
	}

	inst.Run(newExecID())

	// 后继推进：output 交互节点 → fake 影子节点（蓝本 add_edge(output, fake)）；
	// 互斥节点走 RouteNode（蓝本 add_conditional_edges）；其余全部目标。
	var nexts []string
	switch {
	case e.fakeOf[nodeID] != "":
		nexts = []string{e.fakeOf[nodeID]}
	case inst.IsConditionNode():
		if rid := inst.RouteNode(); rid != "" {
			nexts = []string{rid}
		}
	default:
		nexts = e.edges.GetTargetNode(nodeID)
	}
	// fake_output 放行：按真实 output 节点的路由决定后继
	// （蓝本 add_conditional_edges(fake, output.route_node)）。
	if fake, ok := inst.(*OutputFakeNode); ok {
		if fake.output.IsConditionNode() {
			if rid := fake.output.RouteNode(); rid != "" {
				nexts = []string{rid}
			} else {
				nexts = nil
			}
		} else {
			nexts = e.edges.GetTargetNode(fake.output.ID())
		}
	}

	// fan-in 门禁（蓝本 build_more_fan_in_node：非互斥汇合需全部前驱完成）。
	for _, nid := range nexts {
		if e.readyToEnter(nid, nodeID) {
			e.active = append(e.active, nid)
		}
	}
}

// readyToEnter fan-in 门禁（蓝本 parse_fan_in_node）：
//
//   - 单前驱 / 无前驱：直接放行；
//   - 循环回边（from 层级 > 目标层级）：放行（蓝本 all_source_node_prev 反例）；
//   - 互斥分支汇合：从各互斥节点到此节点的分支路径，剥离【目标节点与互斥
//     节点自身】后两两不相交 → 放行（蓝本 branch.remove(node_id)/remove(one)
//     + judge_not_same_branch）；
//   - 普通汇聚：要求其余前驱已产出（变量池有输出/已执行），未就绪丢弃本次
//     激活 —— 后续前驱完成时会再次触发激活。
func (e *Engine) readyToEnter(nodeID, from string) bool {
	sources := e.fanIn[nodeID]
	if len(sources) <= 1 {
		return true
	}
	// 循环回边：from 在目标下游 → 回边放行
	if e.levels[from] > e.levels[nodeID] {
		return true
	}
	// 互斥分支汇合判定（蓝本 parse_fan_in_node 分支相交检查）
	for _, cond := range e.condNodes {
		if cond == nodeID {
			continue
		}
		branches := e.edges.GetAllEdgesNodes(cond, nodeID)
		var valid [][]string
		for _, b := range branches {
			for _, id := range b {
				if id == nodeID {
					// 剥离目标节点与互斥节点自身（蓝本 branch.remove×2）
					trimmed := make([]string, 0, len(b))
					for _, id2 := range b {
						if id2 != nodeID && id2 != cond {
							trimmed = append(trimmed, id2)
						}
					}
					valid = append(valid, trimmed)
					break
				}
			}
		}
		for i := 0; i < len(valid); i++ {
			for j := i + 1; j < len(valid); j++ {
				if !branchIntersect(valid[i], valid[j]) {
					return true // 互斥两支 → 无需等待
				}
			}
		}
	}
	// 普通汇聚：要求其余前驱已执行（有变量输出 / end / fake 类型）。
	// 自环前驱（src == nodeID，循环回边）豁免 —— 它就是当前节点，未执行
	// 不能阻塞自己（蓝本 langgraph 多入边 OR 触发语义：首次进入靠其它前驱）。
	for _, src := range sources {
		if src == from || src == nodeID {
			continue
		}
		if !e.sourceDone(src) {
			return false
		}
	}
	return true
}

// sourceDone 前驱是否已执行（有变量输出 / end / fake_output 类型）。
func (e *Engine) sourceDone(src string) bool {
	inst, ok := e.nodes[src]
	if !ok {
		return true
	}
	if inst.Type() == NodeEnd || inst.Type() == "fake_output" {
		return true
	}
	if bn, ok := inst.(interface{ RunCount() int }); ok && bn.RunCount() > 0 {
		return true
	}
	return false
}

// branchIntersect 两分支是否相交（蓝本 judge_not_same_branch 的集合交）。
func branchIntersect(a, b []string) bool {
	set := map[string]bool{}
	for _, id := range a {
		set[id] = true
	}
	for _, id := range b {
		if set[id] {
			return true
		}
	}
	return false
}

// newExecID 执行唯一 ID（蓝本 uuid.uuid4().hex）。
func newExecID() string {
	return fmt.Sprintf("%08x%08x%08x%08x", randUint32(), randUint32(), randUint32(), randUint32())
}
