// 边管理器（逐字对齐 edges/edges.py EdgeManage）：source/target 双向索引 +
// 分支路径遍历。蓝本字段名（sourceHandle 等）保持原样以兼容前端画布数据。
package bisheng

import "strings"

// EdgeBase 单条连线（蓝本 EdgeBase，字段名逐字对齐）。
type EdgeBase struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	SourceHandle string `json:"sourceHandle"`
	SourceType  string `json:"sourceType,omitempty"`
	Target      string `json:"target"`
	TargetHandle string `json:"targetHandle,omitempty"`
	TargetType  string `json:"targetType,omitempty"`
}

// EdgeManage 边集合（蓝本 EdgeManage）。
type EdgeManage struct {
	edges     []EdgeBase
	sourceMap map[string][]EdgeBase // source → edges
	targetMap map[string][]EdgeBase // target → edges
}

// NewEdgeManage 构建双向索引（蓝本 __init__）。
func NewEdgeManage(raw []EdgeBase) *EdgeManage {
	m := &EdgeManage{
		edges:     raw,
		sourceMap: map[string][]EdgeBase{},
		targetMap: map[string][]EdgeBase{},
	}
	for _, e := range raw {
		m.sourceMap[e.Source] = append(m.sourceMap[e.Source], e)
		m.targetMap[e.Target] = append(m.targetMap[e.Target], e)
	}
	return m
}

// GetTargetNode 按源节点查目标节点 ID 列表。
func (m *EdgeManage) GetTargetNode(source string) []string {
	var out []string
	for _, e := range m.sourceMap[source] {
		out = append(out, e.Target)
	}
	return out
}

// GetSourceNode 按目标节点查前驱节点 ID 列表。
func (m *EdgeManage) GetSourceNode(target string) []string {
	var out []string
	for _, e := range m.targetMap[target] {
		out = append(out, e.Source)
	}
	return out
}

// GetTargetEdges 按源节点查全部出边（节点路由用）。
func (m *EdgeManage) GetTargetEdges(source string) []EdgeBase { return m.sourceMap[source] }

// GetNextNodes 递归收集下游全部节点（蓝本 get_next_nodes：直连 + 传递闭包，
// exclude 防环）。
func (m *EdgeManage) GetNextNodes(nodeID string) []string {
	direct := m.GetTargetNode(nodeID)
	if len(direct) == 0 {
		return nil
	}
	seen := map[string]bool{nodeID: true}
	var out []string
	queue := direct
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		out = append(out, cur)
		queue = append(queue, m.GetTargetNode(cur)...)
	}
	return out
}

// GetAllEdgesNodes start → end 的全部分支路径（蓝本 get_all_edges_nodes：
// DFS 展开到终点/叶子，返回节点 ID 序列）。
func (m *EdgeManage) GetAllEdgesNodes(startNodeID, endNodeID string) [][]string {
	var branches [][]string
	var walk func(nodeID string, branch []string, nodeMap map[string]bool)
	walk = func(nodeID string, branch []string, nodeMap map[string]bool) {
		if nodeMap[nodeID] || nodeID == endNodeID {
			branch = append(branch, nodeID)
			branches = append(branches, branch)
			return
		}
		branch = append(branch, nodeID)
		nodeMap[nodeID] = true
		next := m.GetTargetNode(nodeID)
		if len(next) == 0 {
			branches = append(branches, branch)
			return
		}
		for _, one := range next {
			// 蓝本逐分支复制 branch/node_map（分支间隔离）
			b := make([]string, len(branch))
			copy(b, branch)
			nm := map[string]bool{}
			for k := range nodeMap {
				nm[k] = true
			}
			walk(one, b, nm)
		}
	}
	walk(startNodeID, nil, map[string]bool{})
	return branches
}

// isConditionPrefix 判断节点 ID 是否互斥类前缀（蓝本 startswith(("output_","condition_"))；
// bisheng 建图时互斥节点 ID 固定带类型前缀）。
func isConditionPrefix(nodeID string) bool {
	return strings.HasPrefix(nodeID, "output_") || strings.HasPrefix(nodeID, "condition_")
}
