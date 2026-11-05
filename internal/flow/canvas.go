// 画布编译器（2026-10 UI 升级）：校验 + 拓扑编译 + start/end 补齐 + 分支支持。
//
// 演进：P1 仅单向线性链（compile.go 旧语义）；本次升级：
//   - start/end 结构节点：画布必须「开始 → … → 结束」完整首尾（执行时透传）；
//   - branch 条件分支节点：多出边按条件表达式路由（运行时求值，否则走 else）；
//   - 线性约束放宽：仅 branch 节点允许多出边；其余节点仍单入单出；
//   - 引擎执行从 compose.Chain 升级为 compose.Graph（见 engine.go buildGraph）。
package flow

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// isStructType 结构性节点（start/end 不承载业务语义，执行时透传）。
func isStructType(t string) bool { return t == StepStart || t == StepEnd }

// newStructID 生成结构节点 ID（纳秒时间戳，进程内唯一性足够）。
func newStructID(t string) string {
	return fmt.Sprintf("n_%s_%d", t, time.Now().UnixNano())
}

// stepLabel 结构节点默认显示名。
func stepLabel(t string) string {
	switch t {
	case StepStart:
		return "开始"
	case StepEnd:
		return "结束"
	case StepBranch:
		return "条件分支"
	}
	return t
}

// EnsureStartEnd 画布补齐开始/结束结构节点（幂等，尊重用户已有布局与连线）：
//   - start 缺失：新建并连到链头（入度为 0 的业务节点中 x 最小者）；
//   - end 缺失：新建并从链尾（出度为 0 的业务节点中 x 最大者）连入；
//   - 已存在则完全不动（尊重用户布局；接线完整性由 ValidateCanvas 校验）。
//
// 返回是否发生修改（供 SaveFlow 判断回写）。补不全的场景（断链/环/多起点）
// 交由 ValidateCanvas 给出明确中文错误，不在补齐逻辑里静默猜测。
func EnsureStartEnd(c *Canvas) bool {
	if c == nil {
		return false
	}
	changed := false
	indeg := map[string]int{}
	outdeg := map[string]int{}
	for _, e := range c.Edges {
		outdeg[e.Source]++
		indeg[e.Target]++
	}
	var startID, endID string
	headID, tailID := "", ""
	var headY, tailY, minX, maxX, headMinX, tailMaxX float64
	first := true
	for i := range c.Nodes {
		n := &c.Nodes[i]
		switch n.Type {
		case StepStart:
			startID = n.ID
		case StepEnd:
			endID = n.ID
		default:
			if first {
				minX, maxX, headMinX, tailMaxX = n.Pos.X, n.Pos.X, n.Pos.X, n.Pos.X
				first = false
			}
			if n.Pos.X < minX {
				minX = n.Pos.X
			}
			if n.Pos.X > maxX {
				maxX = n.Pos.X
			}
			if indeg[n.ID] == 0 && (headID == "" || n.Pos.X < headMinX) {
				headID = n.ID
				headY = n.Pos.Y
			}
			if outdeg[n.ID] == 0 && (tailID == "" || n.Pos.X > tailMaxX) {
				tailID = n.ID
				tailY = n.Pos.Y
				maxX = n.Pos.X
			}
		}
	}
	if startID == "" {
		startID = newStructID(StepStart)
		c.Nodes = append(c.Nodes, CanvasNode{
			ID: startID, Type: StepStart, Pos: Pos{X: minX - 220, Y: headY},
			Step: Step{Type: StepStart, Name: stepLabel(StepStart)},
		})
		changed = true
	}
	// start 无出边时补一条到链头
	if hasOut := edgeExists(c.Edges, startID, ""); !hasOut && headID != "" {
		c.Edges = append(c.Edges, CanvasEdge{ID: "e_auto_" + fmt.Sprint(time.Now().UnixNano()), Source: startID, Target: headID})
		changed = true
	}
	if endID == "" {
		endID = newStructID(StepEnd)
		c.Nodes = append(c.Nodes, CanvasNode{
			ID: endID, Type: StepEnd, Pos: Pos{X: maxX + 220, Y: tailY},
			Step: Step{Type: StepEnd, Name: stepLabel(StepEnd)},
		})
		changed = true
	}
	// end 无入边时从链尾补一条
	if tailID != "" && !edgeExists(c.Edges, "", endID) {
		c.Edges = append(c.Edges, CanvasEdge{ID: "e_auto_" + fmt.Sprint(time.Now().UnixNano()), Source: tailID, Target: endID})
		changed = true
	}
	return changed
}

// edgeExists 判断是否存在满足条件的边（source/target 传空 = 不限该端）。
func edgeExists(edges []CanvasEdge, source, target string) bool {
	for _, e := range edges {
		if (source == "" || e.Source == source) && (target == "" || e.Target == target) {
			return true
		}
	}
	return false
}

// ValidateCanvas 校验画布拓扑（2026-10 升级版）：
//  1. 节点 ID 非空且不重复；类型 ∈ StepTypes；
//  2. start 恰 1 个、end 恰 1 个（结构性节点强制成对）；
//  3. start+end 之外至少一个业务节点（空流程拦截）；
//  4. 边引用节点存在、禁止自环；
//  5. 度约束：非 branch 节点单入；出度单出（branch 除外）；branch 单入、出度 ≥ 2、else（空条件出边）至多一条；
//  6. 从 start 可达 end 且无环（BFS 重访即环；不可达 end 即断链）。
func ValidateCanvas(c *Canvas) error {
	if c == nil || len(c.Nodes) == 0 {
		return fmt.Errorf("画布为空：至少需要一个节点")
	}
	byID := make(map[string]*CanvasNode, len(c.Nodes))
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.ID == "" {
			return fmt.Errorf("节点 %d 缺少 ID", i+1)
		}
		if _, dup := byID[n.ID]; dup {
			return fmt.Errorf("节点 ID 重复: %s", n.ID)
		}
		if !StepTypes[n.Type] {
			return fmt.Errorf("节点 %s 类型无效: %s", n.ID, n.Type)
		}
		byID[n.ID] = n
	}
	indeg := make(map[string]int, len(c.Nodes))
	outdeg := make(map[string]int, len(c.Nodes))
	next := map[string][]string{}
	for _, e := range c.Edges {
		if _, ok := byID[e.Source]; !ok {
			return fmt.Errorf("连线 %s 源节点不存在: %s", e.ID, e.Source)
		}
		if _, ok := byID[e.Target]; !ok {
			return fmt.Errorf("连线 %s 目标节点不存在: %s", e.ID, e.Target)
		}
		if e.Source == e.Target {
			return fmt.Errorf("节点 %s 不能连接自身", e.Source)
		}
		outdeg[e.Source]++
		indeg[e.Target]++
		next[e.Source] = append(next[e.Source], e.Target)
	}
	startID, endID := "", ""
	bizCount := 0
	for _, n := range c.Nodes {
		switch n.Type {
		case StepStart:
			if startID != "" {
				return fmt.Errorf("画布只能有一个「开始」节点")
			}
			startID = n.ID
		case StepEnd:
			if endID != "" {
				return fmt.Errorf("画布只能有一个「结束」节点")
			}
			endID = n.ID
		default:
			bizCount++
		}
	}
	if startID == "" {
		return fmt.Errorf("画布缺少「开始」节点")
	}
	if endID == "" {
		return fmt.Errorf("画布缺少「结束」节点")
	}
	if bizCount == 0 {
		return fmt.Errorf("画布至少需要一个业务节点（开始/结束之间）")
	}
	for _, n := range c.Nodes {
		d, o := indeg[n.ID], outdeg[n.ID]
		switch n.Type {
		case StepStart:
			if d != 0 {
				return fmt.Errorf("「开始」节点不能有入边")
			}
			if o != 1 {
				return fmt.Errorf("「开始」节点必须连出一条边")
			}
		case StepEnd:
			if o != 0 {
				return fmt.Errorf("「结束」节点不能有出边")
			}
			if d < 1 {
				return fmt.Errorf("「结束」节点必须有入边（分支汇合）")
			}
		case StepBranch:
			if d != 1 {
				return fmt.Errorf("分支节点 %s 必须恰有一条入边（当前 %d 条）", n.ID, d)
			}
			if o < 2 {
				return fmt.Errorf("分支节点 %s 至少两条出边（按条件路由各分支）", n.ID)
			}
			elseCnt := 0
			for _, e := range c.Edges {
				if e.Source == n.ID && strings.TrimSpace(e.Expr) == "" {
					elseCnt++
				}
			}
			if elseCnt > 1 {
				return fmt.Errorf("分支节点 %s 只能有一条空条件（else）出边", n.ID)
			}
		default:
			if o > 1 {
				return fmt.Errorf("节点 %s 存在多条出线（仅条件分支节点支持多出边）", n.ID)
			}
			if d > 1 {
				return fmt.Errorf("节点 %s 存在多条入线（仅支持单入单出 + 条件分支）", n.ID)
			}
		}
	}
	// BFS 可达性 + 有向无环判定（Kahn 入度削减：入度归零计数器重访即环）
	visited := make(map[string]bool, len(c.Nodes))
	indeg2 := make(map[string]int, len(c.Nodes))
	for id := range indeg {
		indeg2[id] = indeg[id]
	}
	queue := []string{startID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			return fmt.Errorf("画布存在环")
		}
		visited[cur] = true
		for _, nx := range next[cur] {
			indeg2[nx]--
			if indeg2[nx] == 0 {
				queue = append(queue, nx)
			}
		}
	}
	if len(visited) < len(c.Nodes) {
		return fmt.Errorf("画布存在环或未连通节点（孤岛/断链）：仅 %d/%d 个节点可达", len(visited), len(c.Nodes))
	}
	if !visited[endID] {
		return fmt.Errorf("「结束」节点不可达（存在断链）")
	}
	return nil
}

// CompileCanvas 校验并按 BFS 拓扑序输出业务 Steps（剥掉 start/end/branch）。
// 线性流程（无 branch）语义与 P1 完全一致；分支流程交由 buildGraph 图执行，
// Steps 仅用于保存校验与变量提取（branch 不进线性 Steps）。
func CompileCanvas(c *Canvas) ([]Step, error) {
	if err := ValidateCanvas(c); err != nil {
		return nil, err
	}
	byID := make(map[string]*CanvasNode, len(c.Nodes))
	for i := range c.Nodes {
		byID[c.Nodes[i].ID] = &c.Nodes[i]
	}
	next := map[string][]string{}
	for _, e := range c.Edges {
		next[e.Source] = append(next[e.Source], e.Target)
	}
	startID := ""
	for _, n := range c.Nodes {
		if n.Type == StepStart {
			startID = n.ID
		}
	}
	steps := make([]Step, 0, len(c.Nodes))
	seen := map[string]bool{}
	queue := []string{startID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		n := byID[cur]
		if !isStructType(n.Type) && n.Type != StepBranch {
			n.Step.Type = n.Type // 归一化：节点类型为准
			steps = append(steps, n.Step)
		}
		queue = append(queue, next[cur]...)
	}
	return steps, nil
}

// HasBranch 画布是否含条件分支节点（RunFlow 执行路径分派：
// true → RunGraph 图执行；false → 原 Eino Chain 单链执行，语义保真）。
func HasBranch(c *Canvas) bool {
	for _, n := range c.Nodes {
		if n.Type == StepBranch {
			return true
		}
	}
	return false
}

// HasTransform 画布是否含代码转换节点（transform 需要变量池回写，
// RunFlow 分派：含 transform 同样强制走 RunGraph 图执行路径）。
func HasTransform(c *Canvas) bool {
	for _, n := range c.Nodes {
		if n.Type == StepTransform {
			return true
		}
	}
	return false
}

// HasKnowledge 画布是否含知识库检索节点（knowledge 引用上游 {nodeID.output}
// 需变量池，RunFlow 分派：含 knowledge 同样强制走 RunGraph 图执行路径）。
func HasKnowledge(c *Canvas) bool {
	for _, n := range c.Nodes {
		if n.Type == StepKnowledge {
			return true
		}
	}
	return false
}

// NeedsGraph 画布是否必须走图执行路径（RunGraph）——2026-10-27 Dify/Coze
// 对齐查缺：Chain 路径（Run 的 switch）只认识 9 类官方组件节点，Dify 融入
// 的 6 类画布节点（transform/http/assigner/extractor/iteration/knowledge）
// 依赖变量池回写语义，Chain 路径会报「类型未知」。判定规则：含任一 Dify
// 融入节点或 branch 节点 → 图执行；纯官方 9 类线性画布维持 Chain（语义保真）。
func NeedsGraph(c *Canvas) bool {
	if c == nil {
		return false
	}
	for _, n := range c.Nodes {
		switch n.Type {
		case StepBranch, StepTransform, StepHTTP, StepAssigner,
			StepExtractor, StepIteration, StepKnowledge, StepSubflow:
			return true
		}
	}
	return false
}

// Start 统一启动入口（2026-10 cron 联动补齐）：按画布内容分派执行路径——
// 含 branch 与 Dify 融入节点的画布 → RunGraph 图执行（条件路由 +
	// 变量池回写，轨迹携 NodeID）；纯官方组件线性画布 → Run 的 Eino Chain
	// 链式执行（单链语义不变）。与桌面端 RunFlow 绑定的既有分派语义一致，
	// cron 触发与 UI 运行共用同一入口，后续调用方不各自重复分派逻辑。
func (r *Runner) Start(ctx context.Context, f *Flow, vars map[string]any, dryRun bool) (*RunResult, error) {
	if NeedsGraph(f.Canvas) {
		return r.RunGraph(ctx, f, f.Canvas, vars, dryRun)
	}
	return r.Run(ctx, f, vars, dryRun)
}

// topologicalOrder 从 start 起按 BFS 输出全部节点 ID（ValidateCanvas 已保证无环可达）。
func topologicalOrder(c *Canvas) []string {
	next := map[string][]string{}
	for _, e := range c.Edges {
		next[e.Source] = append(next[e.Source], e.Target)
	}
	startID := ""
	for _, n := range c.Nodes {
		if n.Type == StepStart {
			startID = n.ID
		}
	}
	order := make([]string, 0, len(c.Nodes))
	seen := map[string]bool{}
	queue := []string{startID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		order = append(order, cur)
		queue = append(queue, next[cur]...)
	}
	return order
}
