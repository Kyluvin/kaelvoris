package remote

// e2e_remote_test.go 端到端验证：手机端发消息 → 服务端派发 → PC 节点执行 →
// 事件/结果回传手机端（用户验收标准「手机端发消息，PC 端执行则为成功」）。
//
// 覆盖链路（全部真实协议，无 mock 短路）：
//	1. kaelvoris-server（Hub+Server，内存态）启动；
//	2. PC 节点：真实 remote.Client（与桌面端 remote_panel.go 同款接线）注册 /ws/agent；
//	3. 手机端：原生 websocket 拨 /ws/client → auth（统一账号 user/pass）→
//	   create.task 发消息 → 收 task.event 流式事件 → 收 task.result 终态；
//	4. 续聊：chat.send 运行中入队 → 节点继续执行 → 第二个结果回传；
//	5. 取消：task.cancel → 节点停止 → 终态回传；
//	6. 账号：错密码 auth.err（PC 端 RemoteLogin 同协议同账号表）。

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// e2eSandbox 端到端测试环境：真实 Server + 真实 Client 回调接线（模拟桌面端）。
type e2eSandbox struct {
	t      *testing.T
	wsBase string
	client *Client // PC 节点连接器（真实协议栈）
	node   *e2eNode
	hub    *Hub
}

// e2eNode 模拟桌面端节点执行面：记录收到的任务，产出事件与结果。
// hold=true 时模拟长任务（执行中不回终态，等取消信号），用于取消链路验证；
// taskOf 仿真桌面端 remoteTaskOf 映射（续聊沿用首轮任务 ID，与 remote_panel.go 同语义）。
type e2eNode struct {
	mu        sync.Mutex
	tasks     []TaskDispatch // 收到的派发（含续聊）
	events    []string       // SendTaskEvent 调用序列 kind:text
	cancelled []string       // 收到的取消 sessionID\x00taskID
	hold      atomic.Bool
	released  chan struct{}            // hold 解除信号（Cancel 回调关闭）
	of        sync.Map                 // sessionID → taskID（仿真 remoteTaskOf）
	modes     []ModeInfo               // 节点可选模式（仿真 modes.Manager.All）
	current   string                   // 当前执行模式（SetMode/派发指定切换）
	sessions  map[string][]interface{} // sessionID → 模拟历史（SessionMessages 回调返回）
	skills    map[string]string        // 技能名 → SKILL.md 全文（仿真 skills.Manager）
	nodeCfg   NodeConfig               // 节点配置投影（GetConfig 回调返回）
	cfgErr    bool                     // 保存失败开关（SetConfig 回调返回错误）
	deleted   []string                 // 收到的会话删除（server.delete_session 联动）
	analyses  []string                 // 收到的分析请求 sessionID（server.analyze_session）
	analyzeFn func(sessionID string)   // 可注入的分析执行（测试注入回传 analyze.done）
	rmItems   []RoadmapItem            // 仿真规划板条目（roadmap.Store 内存仿体）
	rmPlans   []string                 // 收到的规划请求 guidance 序列
	rmDevs    [][]string               // 收到的执行请求 IDs 序列（含非法空调用记录）
	rmDeletes []string                 // 收到的删除请求 itemID 序列
	// A2A 群聊（仿真 remote_group.go groupStore：内存态，不落盘；
	// 群状态权威在本节点，服务端仅路由/广播/投影缓存）。
	employees []EmployeeMeta            // 数字员工元数据（建群选成员数据源）
	groups    map[string]*Group         // groupID → 群元数据
	msgHist   map[string][]GroupMessage // groupID → 群消息历史
	client    *Client                   // 回传通道（回调装配后注入；nil 时安全跳过）
}

// ---------- 自主开发规划板（roadmap 仿真；对齐 remote_panel.go 装配语义） ----------

// roadmapSnapshot 当前规划板快照（RoadmapData 回调返回；对齐 remoteRoadmapData）。
func (n *e2eNode) roadmapSnapshot() RoadmapDataPayload {
	n.mu.Lock()
	defer n.mu.Unlock()
	stats := RoadmapStats{Total: len(n.rmItems)}
	for _, it := range n.rmItems {
		switch it.Status {
		case "proposed":
			stats.Proposed++
		case "approved":
			stats.Approved++
		case "developing":
			stats.Developing++
		case "done":
			stats.Done++
		case "failed":
			stats.Failed++
		case "rejected":
			stats.Rejected++
		}
	}
	items := append([]RoadmapItem{}, n.rmItems...)
	return RoadmapDataPayload{Items: items, Stats: stats, Auto: RoadmapAutoCfg{DevMode: "parallel", Parallel: 2}}
}

// roadmapPlan 仿真 AI 规划（同步落两条 proposed 提案，guidance 留痕）。
func (n *e2eNode) roadmapPlan(guidance string) error {
	n.mu.Lock()
	n.rmPlans = append(n.rmPlans, guidance)
	n.rmItems = append(n.rmItems,
		RoadmapItem{ID: "rm_plan1", Title: "移动端体验优化（仿真提案）", Status: "proposed", Priority: "high", Category: "feature"},
		RoadmapItem{ID: "rm_plan2", Title: "会话同步修复（仿真提案）", Status: "proposed", Priority: "medium", Category: "bug"})
	n.mu.Unlock()
	return nil
}

// roadmapDev 仿真执行：IDs 空 = 拒绝（执行前必须选择，服务端/节点双重校验）。
func (n *e2eNode) roadmapDev(ids []string, mode string, parallel int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(ids) == 0 {
		return fmt.Errorf("请先选择要开发的条目（未选择不能执行）")
	}
	n.rmDevs = append(n.rmDevs, ids)
	for i := range n.rmItems {
		for _, id := range ids {
			if n.rmItems[i].ID == id {
				n.rmItems[i].Status = "approved"
			}
		}
	}
	return nil
}

// roadmapDelete 仿真删除。
func (n *e2eNode) roadmapDelete(id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.rmDeletes = append(n.rmDeletes, id)
	for i := range n.rmItems {
		if n.rmItems[i].ID == id {
			n.rmItems = append(n.rmItems[:i], n.rmItems[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("条目不存在: %s", id)
}

// roadmapDevIDs 收到的执行请求 IDs 序列（并发安全快照）。
func (n *e2eNode) roadmapDevIDs() [][]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([][]string, len(n.rmDevs))
	copy(out, n.rmDevs)
	return out
}

// roadmapPlans 收到的规划请求 guidance 序列（透传断言用）。
func (n *e2eNode) roadmapPlans() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.rmPlans))
	copy(out, n.rmPlans)
	return out
}

// roadmapDeletes 收到的删除请求 itemID 序列（删除到达节点断言用）。
func (n *e2eNode) roadmapDeletes() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.rmDeletes))
	copy(out, n.rmDeletes)
	return out
}

// ---------- A2A 群聊 + 数字员工（仿真 remote_group.go groupStore：内存态） ----------

// employeeMetas 数字员工元数据投影（EmployeeList 回调返回；空 = 无员工）。
func (n *e2eNode) employeeMetas() []EmployeeMeta {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]EmployeeMeta{}, n.employees...)
}

// groupCreate 创建群聊（GroupCreate 回调；对齐 remoteGroupCreate：
// 用户 user:remote + 初始员工成员 + 群助手机器人，落内存并回传 agent.group_state）。
func (n *e2eNode) groupCreate(req GroupServerReq) {
	n.mu.Lock()
	if n.groups == nil {
		n.groups = map[string]*Group{}
	}
	if n.msgHist == nil {
		n.msgHist = map[string][]GroupMessage{}
	}
	gid := fmt.Sprintf("g%d", time.Now().UnixNano())
	grp := &Group{
		ID: gid, Name: req.Name, Owner: "user:remote", NodeID: "node-e2e",
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if grp.Name == "" {
		grp.Name = "新群聊"
	}
	grp.Members = append(grp.Members, AgentProfile{ID: "user:remote", Name: "用户", Kind: "user"})
	for _, empID := range req.Members {
		for _, e := range n.employees {
			if e.ID == empID {
				grp.Members = append(grp.Members, AgentProfile{ID: e.ID, Name: e.Name, Kind: "employee", Avatar: e.Avatar, Description: e.Description})
				break
			}
		}
	}
	grp.Members = append(grp.Members, AgentProfile{ID: "bot:group", Name: "群助手", Kind: "bot", Description: "协调群内写任务"})
	n.groups[gid] = grp
	sysMsg := GroupMessage{GroupID: gid, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot", Text: "群聊已创建，欢迎加入！", TS: time.Now().UnixMilli()}
	n.msgHist[gid] = append(n.msgHist[gid], sysMsg)
	n.mu.Unlock()
	n.client.SendGroups(n.groupList())
}

// groupMsg 群消息（GroupMsg 回调；对齐 remoteGroupMsg：入历史 + 回传广播）。
func (n *e2eNode) groupMsg(req GroupServerReq) {
	n.mu.Lock()
	if n.groups == nil {
		n.groups = map[string]*Group{}
		n.msgHist = map[string][]GroupMessage{}
	}
	grp, ok := n.groups[req.GroupID]
	if !ok {
		n.mu.Unlock()
		return
	}
	msg := GroupMessage{GroupID: grp.ID, Author: "user:remote", AuthorName: "用户", AuthorKind: "user", Text: req.Text, TS: time.Now().UnixMilli()}
	n.msgHist[grp.ID] = append(n.msgHist[grp.ID], msg)
	n.mu.Unlock()
	n.client.SendGroupMessage(msg)
}

// groupTask 群写任务（GroupTask 回调；对齐 remoteGroupTask 接力版：
// AssignedTo 逗号分隔员工列表按序接力执行，空 = 第一个 employee 成员；
// 每个员工产出回传 + 最终 task_done 回传）。
func (n *e2eNode) groupTask(req GroupServerReq) {
	n.mu.Lock()
	grp, ok := n.groups[req.GroupID]
	if !ok {
		n.mu.Unlock()
		return
	}
	assigned := SplitAssignedIDs(req.AssignedTo)
	if len(assigned) == 0 {
		for _, m := range grp.Members {
			if m.Kind == "employee" {
				assigned = []string{m.ID}
				break
			}
		}
	}
	task := GroupTask{ID: fmt.Sprintf("gt%d", time.Now().UnixNano()), GroupID: grp.ID, Instruction: req.Instruction, AssignedTo: strings.Join(assigned, ","), CreatedBy: "user:remote", Status: "running", TS: time.Now().UnixMilli()}
	if len(assigned) == 0 {
		task.Status = "failed"
		task.Result = "群内没有可执行的员工成员"
		n.msgHist[grp.ID] = append(n.msgHist[grp.ID], GroupMessage{GroupID: grp.ID, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot", Text: "任务失败：" + task.Result, TS: time.Now().UnixMilli()})
		n.mu.Unlock()
		n.client.SendGroupTaskDone(task)
		return
	}
	dispatchMsg := GroupMessage{GroupID: grp.ID, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot", Text: "任务「" + req.Instruction + "」已派发给「" + strings.Join(assigned, "、") + "」接力执行", TS: time.Now().UnixMilli()}
	n.msgHist[grp.ID] = append(n.msgHist[grp.ID], dispatchMsg)
	n.mu.Unlock()
	n.client.SendGroupMessage(dispatchMsg)
	// 异步：仿真员工接力执行（毫秒级）——每位员工产出回传（含前序产出），
	// 最后 task_done 回传（Results 顺序记录）。
	go func(t GroupTask, ids []string) {
		results := make([]GroupStepResult, 0, len(ids))
		for _, id := range ids {
			time.Sleep(50 * time.Millisecond)
			out := GroupMessage{GroupID: t.GroupID, Author: id, AuthorName: "员工" + id, AuthorKind: "employee", Text: "产出：" + t.Instruction + "（" + id + "）", TS: time.Now().UnixMilli()}
			n.client.SendGroupMessage(out)
			results = append(results, GroupStepResult{EmployeeID: id, Name: "员工" + id, Result: "产出：" + t.Instruction + "（" + id + "）"})
		}
		t.Status = "done"
		t.Result = "产出：" + t.Instruction
		t.Results = results
		n.client.SendGroupTaskDone(t)
	}(task, assigned)
}

// groupJoin 添加成员（GroupJoin 回调；对齐 remoteGroupJoin：员工存在才加）。
func (n *e2eNode) groupJoin(req GroupServerReq) {
	n.mu.Lock()
	grp, ok := n.groups[req.GroupID]
	if !ok {
		n.mu.Unlock()
		return
	}
	for _, m := range grp.Members {
		if m.ID == req.AgentID {
			n.mu.Unlock()
			return
		}
	}
	for _, e := range n.employees {
		if e.ID == req.AgentID {
			grp.Members = append(grp.Members, AgentProfile{ID: e.ID, Name: e.Name, Kind: "employee", Avatar: e.Avatar, Description: e.Description})
			n.msgHist[grp.ID] = append(n.msgHist[grp.ID], GroupMessage{GroupID: grp.ID, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot", Text: "「" + e.Name + "」已加入群聊", TS: time.Now().UnixMilli()})
			break
		}
	}
	n.mu.Unlock()
	n.client.SendGroups(n.groupList())
}

// groupLeave 移出成员（GroupLeave 回调；对齐 remoteGroupLeave）。
func (n *e2eNode) groupLeave(req GroupServerReq) {
	n.mu.Lock()
	grp, ok := n.groups[req.GroupID]
	if !ok {
		n.mu.Unlock()
		return
	}
	out := grp.Members[:0]
	name := req.AgentID
	for _, m := range grp.Members {
		if m.ID == req.AgentID {
			name = m.Name
			continue
		}
		out = append(out, m)
	}
	grp.Members = out
	n.msgHist[grp.ID] = append(n.msgHist[grp.ID], GroupMessage{GroupID: grp.ID, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot", Text: "「" + name + "」已移出群聊", TS: time.Now().UnixMilli()})
	n.mu.Unlock()
	n.client.SendGroups(n.groupList())
}

// groupSync 拉群状态（GroupSync 回调；对齐 remoteGroupSync：回传群 + 消息历史）。
func (n *e2eNode) groupSync(req GroupServerReq) {
	n.mu.Lock()
	groups := make([]Group, 0, len(n.groups))
	msgs := []GroupMessage{}
	if req.GroupID != "" {
		grp, ok := n.groups[req.GroupID]
		if ok {
			groups = append(groups, *grp)
			msgs = append(msgs, n.msgHist[req.GroupID]...)
		}
	} else {
		for _, grp := range n.groups {
			groups = append(groups, *grp)
			msgs = append(msgs, n.msgHist[grp.ID]...)
		}
	}
	n.mu.Unlock()
	n.client.SendGroupState(GroupStatePayload{Groups: groups, Messages: msgs})
}

// groupList 群列表快照（并发安全）。
func (n *e2eNode) groupList() []Group {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Group, 0, len(n.groups))
	for _, grp := range n.groups {
		out = append(out, *grp)
	}
	return out
}

// setMessages 预置节点侧会话历史（拉历史链路验证用，仿真桌面端会话快照）。
func (n *e2eNode) setMessages(sessionID string, msgs []interface{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sessions[sessionID] = msgs
}

// allSessions 全部本地会话快照（上线补传用，仿真桌面端 store.List 遍历）。
func (n *e2eNode) allSessions() map[string][]interface{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string][]interface{}, len(n.sessions))
	for k, v := range n.sessions {
		out[k] = append([]interface{}{}, v...)
	}
	return out
}

// ---------- 节点配置状态（仿真桌面端 GetConfig/SetConfig 回调，对齐 remote_panel.go） ----------

// cfg 当前节点配置（仿真 desktop.json Config 投影）。
func (n *e2eNode) cfg() NodeConfig {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nodeCfg
}

// setCfg 直改配置（合并保存链路验证：仅写非 nil 字段）。
func (n *e2eNode) setCfg(sc SetConfig) {
	n.mu.Lock()
	if sc.BaseURL != nil {
		n.nodeCfg.BaseURL = *sc.BaseURL
	}
	if sc.Model != nil {
		n.nodeCfg.Model = *sc.Model
	}
	if sc.APIKey != "" && !strings.Contains(sc.APIKey, "****") {
		n.nodeCfg.APIKey = sc.APIKey // 掩码形态 = 未修改（对齐 remote_panel.go SetConfig 回调）
	}
	if sc.AutoApprove != nil {
		n.nodeCfg.AutoApprove = *sc.AutoApprove
	}
	if sc.ContextWindow != nil {
		n.nodeCfg.ContextWindow = *sc.ContextWindow
	}
	if sc.AutoCondensePercent != nil {
		n.nodeCfg.AutoCondensePercent = *sc.AutoCondensePercent
	}
	n.mu.Unlock()
}

// setCfgErr 切换保存失败模式（仿真 SaveConfig 失败，验证 server.error 回报链路）。
func (n *e2eNode) setCfgErr(v bool) {
	n.mu.Lock()
	n.cfgErr = v
	n.mu.Unlock()
}

// cfgErrFlag 保存失败开关读取（回调侧并发安全）。
func (n *e2eNode) cfgErrFlag() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfgErr
}

// messagesOf 会话历史查询（仿真桌面端 remoteSessionMessages：无记录返回空数组）。
func (n *e2eNode) messagesOf(sessionID string) []interface{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if m, ok := n.sessions[sessionID]; ok {
		return m
	}
	return []interface{}{}
}

// recordDelete / deleted 记录节点侧收到的会话删除（联动链路断言）。
func (n *e2eNode) recordDelete(sessionID string) {
	n.mu.Lock()
	n.deleted = append(n.deleted, sessionID)
	n.mu.Unlock()
}

func (n *e2eNode) deletedList() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.deleted))
	copy(out, n.deleted)
	return out
}

// recordAnalyze / analyzeHook 分析请求记录与注入钩子（测试用）。
func (n *e2eNode) recordAnalyze(sessionID string) { n.analyses = append(n.analyses, sessionID) }

func (n *e2eNode) setAnalyzeHook(fn func(sessionID string)) {
	n.mu.Lock()
	n.analyzeFn = fn
	n.mu.Unlock()
}

func (n *e2eNode) analyzeHook() func(sessionID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.analyzeFn
}

func (n *e2eNode) analysesList() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.analyses))
	copy(out, n.analyses)
	return out
}

// ---------- 技能库状态（仿真 skills.Manager：内存 map，对齐 remote_panel.go 回调） ----------

// skillMetas 技能元数据投影（SkillList 回调返回；仿真 Discover 后的元数据全量）。
func (n *e2eNode) skillMetas() []SkillMeta {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]SkillMeta, 0, len(n.skills))
	for name := range n.skills {
		out = append(out, SkillMeta{Name: name, Description: "e2e skill", Source: "project"})
	}
	return out
}

// skillRaw 读技能全文（无记录报错，仿真 LoadRaw not found）。
func (n *e2eNode) skillRaw(name string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.skills[name]; ok {
		return c, nil
	}
	return "", fmt.Errorf("skill %q not found", name)
}

// saveSkill 写技能（仿真 Manager.Save：落内存即 Discover）。
func (n *e2eNode) saveSkill(name, content string) error {
	n.mu.Lock()
	n.skills[name] = content
	n.mu.Unlock()
	return nil
}

// deleteSkill 删技能（builtin 禁删由前端/Manager 层负责；此处只仿真磁盘删除）。
func (n *e2eNode) deleteSkill(name string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.skills[name]; !ok {
		return fmt.Errorf("skill %q not found", name)
	}
	delete(n.skills, name)
	return nil
}

// modeList / currentMode / setMode 模式状态访问器（并发安全，仿真桌面端 GetMode/SetMode）。
func (n *e2eNode) modeList() []ModeInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]ModeInfo, len(n.modes))
	copy(out, n.modes)
	return out
}

func (n *e2eNode) currentMode() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.current
}

func (n *e2eNode) setMode(slug string) {
	n.mu.Lock()
	n.current = slug
	n.mu.Unlock()
}

// setHold 切换长任务模式（每次 RunTask 重建 released）。
func (n *e2eNode) setHold(v bool) {
	n.mu.Lock()
	n.hold.Store(v)
	if v {
		n.released = make(chan struct{})
	}
	n.mu.Unlock()
}

// releasedChanClose 释放全部 hold 中长任务（测试收尾归还，兑底超时前触发取消终态）。
func (n *e2eNode) releasedChanClose() {
	n.mu.Lock()
	released := n.released
	n.hold.Store(false)
	n.mu.Unlock()
	if released != nil {
		select {
		case <-released:
		default:
			close(released)
		}
	}
}

func (n *e2eNode) dispatches() []TaskDispatch {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]TaskDispatch, len(n.tasks))
	copy(out, n.tasks)
	return out
}

func (n *e2eNode) waitDispatch(timeout time.Duration) TaskDispatch {
	return n.waitDispatchN(1, timeout)
}

// waitDispatchN 等待至少第 n 条派发（返回最新一条；续聊等第二条时用）。
func (n *e2eNode) waitDispatchN(nth int, timeout time.Duration) TaskDispatch {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ds := n.dispatches(); len(ds) >= nth {
			return ds[len(ds)-1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	return TaskDispatch{}
}

// newE2E 起服务端 + 节点（对齐 remote_panel.go startRemoteClient 接线）。
func newE2E(t *testing.T) *e2eSandbox {
	t.Helper()
	hub := NewHub("", map[string]string{"tok-e2e": "node-e2e"}, "",
		map[string]string{}, map[string]string{},
		[]Account{
			{User: "admin", Pass: "admin123", Role: "admin", DisplayName: "管理员"},
			{User: "user", Pass: "user123", Role: "user", DisplayName: "手机用户"},
		})
	srv := NewServer(hub, "")
	addr := startTestServer(t, srv)

	node := &e2eNode{
		modes: []ModeInfo{
			{Slug: "code", Name: "Code"},
			{Slug: "debug", Name: "Debug"},
			{Slug: "architect", Name: "Architect"},
		},
		current:  "code",
		sessions: map[string][]interface{}{},
		skills: map[string]string{
			"deploy": "---\nname: deploy\ndescription: e2e deploy skill\n---\n\nDeploy steps.\n",
		},
		// 数字员工（仿真 employee.Store.List；移动端群聊建群选成员数据源）。
		employees: []EmployeeMeta{
			{ID: "emp-writer", Name: "写作专员", Role: "writer", Description: "负责文案写作", Status: "enabled", Avatar: "✍️"},
			{ID: "emp-analyst", Name: "数据分析师", Role: "analyst", Description: "负责数据分析", Status: "enabled", Avatar: "📊"},
		},
		nodeCfg: NodeConfig{ // 仿真桌面端本地配置（明文存储，同 desktop.json；出站掩码在 GetConfig 回调）
			BaseURL:             "https://api.desktop.local/v1",
			Model:               "claude-sonnet-4-5",
			APIKey:              "sk-e2e-secret-9999",
			AutoApprove:         true,
			ContextWindow:       200000,
			AutoCondensePercent: 55,
		},
	}
	var c *Client
	c = NewClient("ws://"+addr+"/ws/agent", "tok-e2e", "node-e2e", "E2E-PC",
		"test-1.0", "e2e-host", "windows", "amd64",
		ClientCallbacks{
			RunTask: func(sessionID, text string, images []string, mode string, taskID string) bool {
				node.mu.Lock()
				node.tasks = append(node.tasks, TaskDispatch{TaskID: taskID, SessionID: sessionID, Text: text, Mode: mode})
				hold := node.hold.Load()
				released := node.released
				node.mu.Unlock()
				// 仿真桌面端 remoteTaskOf：新任务登记映射，续聊沿用首轮任务 ID。
				if taskID != "" {
					node.of.Store(sessionID, taskID)
				}
				tid, _ := node.of.Load(sessionID)
				tidStr, _ := tid.(string)
				// 异步产出事件流 + 终态（模拟引擎运行；hold = 长任务等取消）。
				go func() {
					c.SendTaskEvent(tidStr, sessionID, "started", text, false)
					c.SendTaskEvent(tidStr, sessionID, "text", "PC 端正在处理: "+text, false)
					if hold {
						select {
						case <-released: // 已取消：回取消终态
							c.SendTaskDone(tidStr, sessionID, false, "", "任务已取消")
						case <-time.After(15 * time.Second): // 兑底超时
							c.SendTaskDone(tidStr, sessionID, true, "结果: "+text+" 完成(超时)", "")
						}
						return
					}
					c.SendTaskEvent(tidStr, sessionID, "tool", "fs_read: main.go", false)
					c.SendTaskEvent(tidStr, sessionID, "text", "执行完成", false)
					c.SendTaskDone(tidStr, sessionID, true, "结果: "+text+" 完成", "")
				}()
				return true
			},
			Cancel: func(sessionID string, taskID string) {
				node.mu.Lock()
				node.cancelled = append(node.cancelled, sessionID+"\x00"+taskID)
				released := node.released
				node.mu.Unlock()
				if released != nil {
					select {
					case <-released:
					default:
						close(released)
					}
				}
			},
			IsRunning:       func(sessionID string) bool { return false },
			SessionMessages: func(sessionID string) []interface{} { return node.messagesOf(sessionID) },
			SessionTodos:    func(sessionID string) []interface{} { return []interface{}{} },
			DeleteSession:   func(sessionID string) { node.recordDelete(sessionID) },
			Analyze: func(sessionID string) {
				node.recordAnalyze(sessionID)
				if fn := node.analyzeHook(); fn != nil {
					fn(sessionID)
				}
			},
			Modes:       func() []ModeInfo { return node.modeList() },
			CurrentMode: func() string { return node.currentMode() },
			// P-Account 指挥权隔离（对齐 remote_panel.go 装配）：仿真 PC 端登录
			// user 账号（与 dialMobile("user") 同一账号），注册帧携带 → 服务端
			// 绑定节点归属；同账号手机端可指挥（e2e 主链路），异账号被拒（新守卫测试）。
			Account: func() string { return "user" },
			Pass:    func() string { return "user123" },
			SetMode: func(slug string) error { node.setMode(slug); return nil },
			// 节点配置回调（对齐 remote_panel.go：读投影 / 合并保存；
			// APIKey 掩码不出节点，见 protocol.go NodeConfig 注释）。
			GetConfig: func() NodeConfig {
				nc := node.cfg()
				nc.APIKey = MaskAPIKey(nc.APIKey) // 出站掩码（桌面端同款）
				return nc
			},
			SetConfig: func(sc SetConfig) error {
				if node.cfgErrFlag() {
					return fmt.Errorf("磁盘写入失败（仿真）")
				}
				node.setCfg(sc)
				return nil
			},
			// 仿真桌面端上线补传（remote_panel.go remoteBackfillOnOnline 同语义）：
			// 注册成功后把节点本地全部会话历史推到服务端镜像（空历史跳过）。
			OnOnline: func() {
				for sid, msgs := range node.allSessions() {
					c.Send(TypeAgentMessages, MessagesResponse{SessionID: sid, Messages: msgs})
				}
			},
			// 技能库回调（对齐 remote_panel.go 装配：内存 map 仿真 skills.Manager）。
			SkillList:   func() []SkillMeta { return node.skillMetas() },
			SkillGet:    func(name string) (string, error) { return node.skillRaw(name) },
			SkillSave:   func(name, content string) error { return node.saveSkill(name, content) },
			SkillDelete: func(name string) error { return node.deleteSkill(name) },
			// 自主开发规划板回调（对齐 remote_panel.go 装配：内存 map 仿真
			// roadmap.Store + 执行入口；IDs 空拒绝 = 执行前必须选择）。
			// 规划完成后主动回传规划板（对齐真实链路 roadmapPlanOnce → pushRoadmap
			// → remoteSendRoadmap；移动端靠 agent.roadmap_data 广播看到新提案）。
			RoadmapData: func() RoadmapDataPayload { return node.roadmapSnapshot() },
			RoadmapPlan: func(guidance string) error {
				err := node.roadmapPlan(guidance)
				if err == nil {
					c.SendRoadmapData()
				}
				return err
			},
			RoadmapDev: func(ids []string, mode string, parallel int) error {
				return node.roadmapDev(ids, mode, parallel)
			},
			RoadmapDelete: func(id string) error { return node.roadmapDelete(id) },
			// A2A 群聊回调（仿真 remote_group.go remoteGroup*；群状态权威在本节点）。
			// 节点在线即上报群列表（移动端群聊页数据源；对齐 OnOnline 兑底语义）。
			EmployeeList: func() []EmployeeMeta { return node.employeeMetas() },
			GroupCreate:  func(req GroupServerReq) { node.groupCreate(req) },
			GroupMsg:     func(req GroupServerReq) { node.groupMsg(req) },
			GroupTask:    func(req GroupServerReq) { node.groupTask(req) },
			GroupJoin:    func(req GroupServerReq) { node.groupJoin(req) },
			GroupLeave:   func(req GroupServerReq) { node.groupLeave(req) },
			GroupSync:    func(req GroupServerReq) { node.groupSync(req) },
		})
	c.Start()
	t.Cleanup(c.Stop)
	node.client = c // 注入回传通道（群回调经此上报 agent.group_*）

	// 等节点在线（服务端视角）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hub.NodeOnline("node-e2e") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !hub.NodeOnline("node-e2e") {
		t.Fatal("PC 节点未在期限内注册上线")
	}
	return &e2eSandbox{t: t, wsBase: "ws://" + addr, client: c, node: node, hub: hub}
}

// startTestServer 起随机端口 HTTP 服务（Server.Start 阻塞，goroutine 运行；
// 端口就绪以 /api/health HTTP 可达为准）。
func startTestServer(t *testing.T, srv *Server) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close() // 释放端口供 Start（ListenAndServe）使用；竞态窗口极小可接受
	go func() { _ = srv.Start(addr) }()
	t.Cleanup(srv.Stop)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, herr := http.Get("http://" + addr + "/api/health")
		if herr == nil {
			_ = resp.Body.Close()
			return addr
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("服务端未在期限内就绪")
	return addr
}

// dialMobile 模拟手机端：拨 /ws/client + auth 登录（统一账号 user/pass，
// 对齐 web/mobile/index.html connect() 与桌面端 remoteAccountAuth 同协议）。
func (s *e2eSandbox) dialMobile(user, pass string) *websocket.Conn {
	s.t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(s.wsBase+"/ws/client", nil)
	if err != nil {
		s.t.Fatalf("手机端拨号失败: %v", err)
	}
	id := fmt.Sprintf("auth-%d", time.Now().UnixNano())
	if err := ws.WriteJSON(NewEnvelope(id, TypeAuth, Auth{User: user, Pass: pass})); err != nil {
		s.t.Fatalf("手机端 auth 发送失败: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var e Envelope
	if err := ws.ReadJSON(&e); err != nil {
		s.t.Fatalf("手机端 auth 响应读取失败: %v", err)
	}
	if e.Type != TypeAuthOK {
		s.t.Fatalf("手机端登录失败: type=%s payload=%s", e.Type, string(e.Payload))
	}
	var ok AuthOK
	_ = e.Decode(&ok)
	if ok.User != user {
		s.t.Fatalf("登录身份不符: got %q want %q", ok.User, user)
	}
	// 消费认证后的初始推送（nodes / tasks）。
	for i := 0; i < 2; i++ {
		var extra Envelope
		if err := ws.ReadJSON(&extra); err != nil {
			s.t.Fatalf("初始推送读取失败: %v", err)
		}
		if extra.Type != TypeNodes && extra.Type != TypeTasks {
			s.t.Fatalf("期望初始推送 nodes/tasks，得到 %s", extra.Type)
		}
	}
	return ws
}

// readUntil 阻塞读信封直到谓词命中（超时 fatal）。
func readUntil(t *testing.T, ws *websocket.Conn, timeout time.Duration, hit func(Envelope) bool) Envelope {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ws.SetReadDeadline(deadline)
		var e Envelope
		if err := ws.ReadJSON(&e); err != nil {
			t.Fatalf("等待消息超时/断开: %v", err)
		}
		if hit(e) {
			return e
		}
	}
}

// TestE2E_MobileTaskExecutesOnPC 核心验收：手机端发消息 → PC 端执行 → 结果回传手机端。
func TestE2E_MobileTaskExecutesOnPC(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123") // 手机端登录（与 PC 端同一账号表）

	// 手机端发布任务（create.task，nodeId 空自动选在线节点）。
	if err := ws.WriteJSON(NewEnvelope("ct-1", TypeCreateTask, CreateTask{Text: "检查项目编译错误并修复"})); err != nil {
		t.Fatalf("create.task 发送失败: %v", err)
	}

	// 断言 1：手机端收到 task.created（服务端受理）。
	created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
	var tc struct {
		Task Task `json:"task"`
	}
	if err := json.Unmarshal(created.Payload, &tc); err != nil {
		t.Fatalf("task.created 解析失败: %v", err)
	}
	if tc.Task.ID == "" || tc.Task.SessionID == "" {
		t.Fatalf("任务受理缺 taskId/sessionId: %+v", tc.Task)
	}
	if tc.Task.Status != TaskStatusRunning && tc.Task.Status != TaskStatusPending {
		t.Fatalf("任务状态异常: %s", tc.Task.Status)
	}
	if !strings.Contains(tc.Task.CreatedBy, "user") {
		t.Fatalf("发布来源缺失账号身份: %q", tc.Task.CreatedBy)
	}

	// 断言 2：PC 节点收到派发（sessionID = 服务端下发的 remote-xxx）。
	d := s.node.waitDispatch(5 * time.Second)
	if d.SessionID == "" || d.Text != "检查项目编译错误并修复" {
		t.Fatalf("PC 节点收到的派发异常: %+v", d)
	}
	if d.TaskID != tc.Task.ID {
		t.Fatalf("派发 taskID 与受理不符: dispatch=%q created=%q", d.TaskID, tc.Task.ID)
	}
	if !strings.HasPrefix(d.SessionID, "remote-") {
		t.Fatalf("会话 ID 应为 remote- 前缀: %q", d.SessionID)
	}

	// 断言 3：手机端收到 started + text + tool 事件流（流式回传）。
	saw := map[string]string{}
	deadline := time.Now().Add(5 * time.Second)
	for len(saw) < 3 && time.Now().Before(deadline) {
		ws.SetReadDeadline(deadline)
		var e Envelope
		if err := ws.ReadJSON(&e); err != nil {
			t.Fatalf("事件流读取失败: %v", err)
		}
		if e.Type != TypeTaskEvent {
			continue
		}
		var ev TaskEvent
		if err := e.Decode(&ev); err != nil {
			continue
		}
		if ev.SessionID != tc.Task.SessionID {
			t.Fatalf("事件会话不符: ev=%q want %q", ev.SessionID, tc.Task.SessionID)
		}
		if ev.TaskID != tc.Task.ID {
			t.Fatalf("事件 taskID 为空或不符（remoteTaskOf 登记链路断裂）: got %q want %q", ev.TaskID, tc.Task.ID)
		}
		saw[ev.Kind] = ev.Text
	}
	for _, kind := range []string{"started", "text", "tool"} {
		if _, ok := saw[kind]; !ok {
			t.Fatalf("缺少 %s 事件: saw=%v", kind, saw)
		}
	}

	// 断言 4（核心）：手机端收到 task.result 且任务落终态 done。
	res := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskResult })
	var tr struct {
		TaskID    string `json:"taskId"`
		SessionID string `json:"sessionId"`
		OK        bool   `json:"ok"`
		Result    string `json:"result"`
	}
	_ = json.Unmarshal(res.Payload, &tr)
	if !tr.OK || tr.TaskID != tc.Task.ID {
		t.Fatalf("终态异常: %+v (expect taskId=%s)", tr, tc.Task.ID)
	}
	if !strings.Contains(tr.Result, "完成") {
		t.Fatalf("结果内容异常: %q", tr.Result)
	}
	if got, _ := s.client2Hub().GetTask(tc.Task.ID); got == nil || got.Status != TaskStatusDone {
		t.Fatalf("服务端任务未落 done: %+v", got)
	}
}

// TestE2E_MobileChatContinuesOnPC 续聊：手机端 chat.send → PC 端继续执行 → 新事件回传。
func TestE2E_MobileChatContinuesOnPC(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 发布第一轮。
	if err := ws.WriteJSON(NewEnvelope("", TypeCreateTask, CreateTask{Text: "第一轮任务"})); err != nil {
		t.Fatalf("create.task: %v", err)
	}
	created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
	var tc struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(created.Payload, &tc)
	s.node.waitDispatch(5 * time.Second)
	time.Sleep(300 * time.Millisecond) // 等第一轮终态广播完成

	// 续聊（chat.send；sessionID 引用第一轮会话，运行中已结束 → 新轮次执行）。
	if err := ws.WriteJSON(NewEnvelope("", TypeChatSend, ChatSend{SessionID: tc.Task.SessionID, TaskID: tc.Task.ID, Text: "第二轮续聊"})); err != nil {
		t.Fatalf("chat.send: %v", err)
	}

	// PC 节点收到续聊（同一会话，taskID 为空 = 不新建任务）。
	d := s.node.waitDispatchN(2, 5*time.Second) // 等第二条（续聊）派发
	if d.SessionID != tc.Task.SessionID || d.Text != "第二轮续聊" {
		t.Fatalf("续聊派发异常: %+v", d)
	}

	// 手机端收到第二轮事件与结果（跳过第一轮终态帧，等含续聊内容的新终态）。
	var tr struct {
		TaskID    string `json:"taskId"`
		SessionID string `json:"sessionId"`
		OK        bool
		Result    string
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		res := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeTaskResult })
		_ = json.Unmarshal(res.Payload, &tr)
		if strings.Contains(tr.Result, "第二轮续聊") {
			break // 第二轮终态已到
		}
		if time.Now().After(deadline) {
			t.Fatalf("未等到第二轮终态，最后收到: %+v", tr)
		}
	}
	if !tr.OK {
		t.Fatalf("续聊终态异常: %+v", tr)
	}
	if tr.TaskID != tc.Task.ID {
		t.Fatalf("续聊事件 taskID 断链: got %q want %q", tr.TaskID, tc.Task.ID)
	}
}

// TestE2E_MobileCancelStopsPC 取消：手机端 task.cancel → 节点收到停止 → 终态回传。
func TestE2E_MobileCancelStopsPC(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 切长任务模式：模拟节点执行中不回终态（真实 PC 执行期间才有取消窗口）。
	s.node.setHold(true)
	if err := ws.WriteJSON(NewEnvelope("", TypeCreateTask, CreateTask{Text: "长任务"})); err != nil {
		t.Fatalf("create.task: %v", err)
	}
	created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
	var tc struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(created.Payload, &tc)
	d := s.node.waitDispatch(5 * time.Second)
	if d.TaskID != tc.Task.ID {
		t.Fatalf("派发任务 ID 不符: %+v vs %s", d, tc.Task.ID)
	}

	// 取消（只带 taskID，模拟手机端 web/mobile task.cancel 用法）。
	if err := ws.WriteJSON(NewEnvelope("", TypeTaskCancel, TaskCancel{TaskID: tc.Task.ID})); err != nil {
		t.Fatalf("task.cancel: %v", err)
	}

	// PC 节点收到取消：sessionID 经服务端下发（新协议）。
	deadline := time.Now().Add(5 * time.Second)
	var gotSession, gotTask string
	for time.Now().Before(deadline) {
		s.node.mu.Lock()
		done := len(s.node.cancelled) > 0
		if done {
			parts := strings.SplitN(s.node.cancelled[0], "\x00", 2)
			gotSession, gotTask = parts[0], parts[1]
		}
		s.node.mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gotSession == "" {
		t.Fatal("PC 节点未收到取消或 sessionID 为空（取消链路断裂）")
	}
	if gotSession != tc.Task.SessionID {
		t.Fatalf("取消 sessionID 不符: got %q want %q", gotSession, tc.Task.SessionID)
	}
	if gotTask != tc.Task.ID {
		t.Fatalf("取消 taskID 不符: got %q want %q", gotTask, tc.Task.ID)
	}

	// 手机端收到取消终态。
	res := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskResult })
	var tr struct {
		TaskID string `json:"taskId"`
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(res.Payload, &tr)
	if tr.OK {
		t.Fatalf("取消终态不应为 ok: %+v", tr)
	}
	if tr.TaskID != tc.Task.ID {
		t.Fatalf("取消终态 taskID 断链: got %q want %q", tr.TaskID, tc.Task.ID)
	}
}

// TestE2E_AuthUnifiedAccount 账号互通：错误密码 auth.err（PC 端 RemoteLogin 走同一协议）。
func TestE2E_AuthUnifiedAccount(t *testing.T) {
	s := newE2E(t)
	ws, _, err := websocket.DefaultDialer.Dial(s.wsBase+"/ws/client", nil)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(NewEnvelope("a1", TypeAuth, Auth{User: "user", Pass: "wrong"})); err != nil {
		t.Fatalf("auth 发送: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var e Envelope
	if err := ws.ReadJSON(&e); err != nil {
		t.Fatalf("auth 响应: %v", err)
	}
	if e.Type != TypeAuthErr {
		t.Fatalf("错误密码应返回 auth.err，got %s", e.Type)
	}
	var ep ErrorPayload
	_ = e.Decode(&ep)
	if ep.Message == "" {
		t.Fatal("auth.err 应带失败原因")
	}
}

// client2Hub 测试内访问 hub（验证服务端任务终态落盘）。
func (s *e2eSandbox) client2Hub() *Hub { return s.hub }

// TestE2E_SetModeRemoteSwitch 远端切换模式：手机端 set.mode → 节点 SetMode 回调
// 切换 → SendModeUpdate 重发 register → 服务端节点 Mode 回写广播。
func TestE2E_SetModeRemoteSwitch(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 手机端切换节点模式到 debug。
	if err := ws.WriteJSON(NewEnvelope("sm-1", TypeSetMode, SetMode{Mode: "debug"})); err != nil {
		t.Fatalf("set.mode 发送失败: %v", err)
	}

	// 断言 1：手机端收到 op.result 成功回执。
	op := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult })
	var o OpResult
	_ = json.Unmarshal(op.Payload, &o)
	if !o.OK {
		t.Fatalf("set.mode 应成功: %+v", o)
	}

	// 断言 2：节点本地模式已切换（SetMode 回调生效）。
	deadline := time.Now().Add(5 * time.Second)
	for s.node.currentMode() != "debug" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.node.currentMode(); got != "debug" {
		t.Fatalf("节点模式未切换: %q", got)
	}

	// 断言 3：服务端节点状态回写为新模式（SendModeUpdate → RegisterAgent 幂等刷新）。
	for time.Now().Before(deadline) {
		for _, n := range s.hub.ListNodes() {
			if n.ID == "node-e2e" && n.Mode == "debug" {
				if len(n.Modes) != 3 {
					t.Fatalf("节点可选模式清单应保持 3 项: %v", n.Modes)
				}
				return // 全链路生效
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("服务端节点模式未回写为 debug: %+v", s.hub.ListNodes())
}

// TestE2E_MobileOpensChatHistory 拉历史闭环：手机端点开聊天卡（get.messages）→
// 服务端先回本地镜像（请求-响应配对），再路由节点 server.get_messages → 节点回传
// agent.messages → 服务端落镜像 + Broadcast messages → 手机端收到非空历史。
// 修复前：全链路无 server.get_messages 发送方，镜像恒空 → 聊天卡点开永远空白。
func TestE2E_MobileOpensChatHistory(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 发布任务并等节点收到派发（会话 ID 由服务端生成 remote-xxx）。
	if err := ws.WriteJSON(NewEnvelope("", TypeCreateTask, CreateTask{Text: "写首诗"})); err != nil {
		t.Fatalf("create.task: %v", err)
	}
	created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
	var tc struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(created.Payload, &tc)
	s.node.waitDispatch(5 * time.Second)

	// 节点侧预置会话历史（仿真桌面端会话快照：用户消息 + 回复）。
	s.node.setMessages(tc.Task.SessionID, []interface{}{
		map[string]interface{}{"ts": 1, "type": "say", "say": "user_feedback", "text": "写首诗"},
		map[string]interface{}{"ts": 2, "type": "say", "say": "text", "text": "好的，诗如下……"},
	})

	// 手机端点开聊天卡（get.messages，与 web/mobile openSessionChat 同协议）。
	if err := ws.WriteJSON(NewEnvelope("gm-1", TypeGetMessages, GetMessages{SessionID: tc.Task.SessionID})); err != nil {
		t.Fatalf("get.messages: %v", err)
	}

	// 断言 1：立即应答（镜像或节点回传）与节点回传广播均到达，且后者含预置历史。
	var gotText string
	deadline := time.Now().Add(5 * time.Second)
	sawResponse := false
	for time.Now().Before(deadline) {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeMessages })
		var mr MessagesResponse
		_ = json.Unmarshal(e.Payload, &mr)
		if mr.SessionID != tc.Task.SessionID {
			continue
		}
		sawResponse = true
		for _, raw := range mr.Messages {
			b, _ := json.Marshal(raw)
			var m struct {
				Say  string `json:"say"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b, &m)
			if strings.Contains(m.Text, "诗如下") {
				gotText = m.Text
			}
		}
		if gotText != "" {
			break
		}
	}
	if !sawResponse {
		t.Fatal("手机端未收到 messages 应答（镜像 + 节点回传均缺席）")
	}
	if gotText == "" {
		t.Fatal("节点回传的历史未到达手机端（server.get_messages 链路断裂）")
	}

	// 断言 2：服务端镜像已落盘（服务重启后 sessions.json 仍可回放）。
	if mirror := s.hub.SessionMessages(tc.Task.SessionID); len(mirror) == 0 {
		t.Fatal("服务端会话镜像未落盘（agent.messages → SaveSessionMessages 断裂）")
	}
}

// TestE2E_EventMirrorFallbackWithoutNode 任务事件镜像兑底：手机端 create.task 后
// 不依赖节点回传（仿真节点直接下线），服务端从 task.event 定型帧同步落镜像 ——
// 手机端 get.messages 立即应答即有历史（修复：断线期间执行的任务点开空白）。
func TestE2E_EventMirrorFallbackWithoutNode(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	if err := ws.WriteJSON(NewEnvelope("", TypeCreateTask, CreateTask{Text: "断线任务"})); err != nil {
		t.Fatalf("create.task: %v", err)
	}
	created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
	var tc struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(created.Payload, &tc)

	// 等事件流落镜像（started/text/tool/text 定型帧均会同步追加）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(s.hub.SessionMessages(tc.Task.SessionID)) < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(s.hub.SessionMessages(tc.Task.SessionID)); n == 0 {
		t.Fatal("task.event 未同步会话镜像（AddTaskEvent mirror 兑底断裂）")
	}

	// 手机端拉历史：镜像应非空（节点在不在线都不影响）。
	if err := ws.WriteJSON(NewEnvelope("gm-mirror", TypeGetMessages, GetMessages{SessionID: tc.Task.SessionID})); err != nil {
		t.Fatalf("get.messages: %v", err)
	}
	e := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeMessages })
	var mr MessagesResponse
	_ = json.Unmarshal(e.Payload, &mr)
	if len(mr.Messages) == 0 {
		t.Fatal("get.messages 应答空历史（镜像兑底未生效）")
	}
}

// TestE2E_ReconnectBackfillMirror 节点断线重连补传：断线期间节点本地产生/持有
// 的会话历史，重连注册成功后经 OnOnline 回调补传 agent.messages，服务端镜像
// 填充 → 手机端 get.messages 拿到历史（修复核心场景）。
func TestE2E_ReconnectBackfillMirror(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 节点本地预置会话历史（断线期间任务执行产生的记录）。
	s.node.setMessages("remote-s-backfill", []interface{}{
		map[string]interface{}{"ts": 1, "type": "say", "say": "user_feedback", "text": "断线任务"},
		map[string]interface{}{"ts": 2, "type": "say", "say": "text", "text": "断线期间完成的结果"},
	})

	// 断开节点连接（停止连接器 → 服务端视角节点下线）。
	s.client.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.hub.NodeOnline("node-e2e") {
		time.Sleep(20 * time.Millisecond)
	}
	if s.hub.NodeOnline("node-e2e") {
		t.Fatal("节点未按预期下线")
	}

	// 重连：起一个同身份的新连接器（真实 OnOnline 补传链路）。
	var c2 *Client
	c2 = NewClient(s.wsBase+"/ws/agent", "tok-e2e", "node-e2e", "E2E-PC",
		"test-1.0", "e2e-host", "windows", "amd64",
		ClientCallbacks{
			OnOnline: func() {
				for sid, msgs := range s.node.allSessions() {
					c2.Send(TypeAgentMessages, MessagesResponse{SessionID: sid, Messages: msgs})
				}
			},
		})
	c2.Start()
	t.Cleanup(c2.Stop)

	// 手机端拉历史：补传后镜像非空（OnOnline → agent.messages → SaveSessionMessages）。
	if err := ws.WriteJSON(NewEnvelope("gm-bf", TypeGetMessages, GetMessages{SessionID: "remote-s-backfill"})); err != nil {
		t.Fatalf("get.messages: %v", err)
	}
	var gotText string
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && gotText == "" {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeMessages })
		var mr MessagesResponse
		_ = json.Unmarshal(e.Payload, &mr)
		if mr.SessionID != "remote-s-backfill" {
			continue
		}
		for _, raw := range mr.Messages {
			b, _ := json.Marshal(raw)
			var m struct {
				Say  string `json:"say"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b, &m)
			if strings.Contains(m.Text, "断线期间完成") {
				gotText = m.Text
			}
		}
	}
	if gotText == "" {
		t.Fatal("重连补传后镜像仍空（OnOnline → agent.messages 链路断裂）")
	}
	if mirror := s.hub.SessionMessages("remote-s-backfill"); len(mirror) == 0 {
		t.Fatal("服务端镜像未落盘")
	}
}

// TestE2E_DispatchWithMode 指定模式派发：create.task 携带 mode → 派发透传 mode →
// 节点先切模式再执行 → 服务端任务记录该模式。
func TestE2E_DispatchWithMode(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 手机端发布任务并指定 architect 模式。
	if err := ws.WriteJSON(NewEnvelope("ct-m", TypeCreateTask, CreateTask{Text: "设计远程文件传输方案", Mode: "architect"})); err != nil {
		t.Fatalf("create.task 发送失败: %v", err)
	}
	created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
	var tc struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(created.Payload, &tc)
	if tc.Task.Mode != "architect" {
		t.Fatalf("任务模式未记录: %q", tc.Task.Mode)
	}

	// 断言：派发携带 mode，且节点本地模式已随之切换（PC 端唯一执行面）。
	d := s.node.waitDispatch(5 * time.Second)
	if d.Mode != "architect" {
		t.Fatalf("派发未透传 mode: %+v", d)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.node.currentMode() != "architect" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.node.currentMode(); got != "architect" {
		t.Fatalf("指定模式派发后节点模式未切换: %q", got)
	}

	// 断言：任务正常执行落终态（模式切换不影响既有链路）。
	readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskResult })
	if got, _ := s.hub.GetTask(tc.Task.ID); got == nil || got.Status != TaskStatusDone {
		t.Fatalf("任务未落 done: %+v", got)
	}
}

// TestE2E_SkillsFullChain 技能库全链路：skills.list（缓存立即应答 + 节点刷新广播）
// → skills.get（skills.item 回传全文）→ skills.save（节点落盘后 agent.skills_list
// 广播刷新）→ skills.delete（同上）；builtin 删除被节点拒绝（server.error 转发）。
func TestE2E_SkillsFullChain(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 1) skills.list：缓存立即应答（空）+ 节点回传后广播非空列表。
	if err := ws.WriteJSON(NewEnvelope("sk-l1", TypeSkillsList, SkillsListReq{NodeID: "node-e2e"})); err != nil {
		t.Fatalf("skills.list: %v", err)
	}
	var listPayload struct {
		NodeID string      `json:"nodeId"`
		Skills []SkillMeta `json:"skills"`
	}
	deadline := time.Now().Add(5 * time.Second)
	sawCache := false
	for time.Now().Before(deadline) {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeSkills })
		_ = json.Unmarshal(e.Payload, &listPayload)
		if listPayload.NodeID != "node-e2e" {
			continue
		}
		if len(listPayload.Skills) == 0 {
			sawCache = true // 第一帧：服务端缓存立即应答（此时可能仍为空）
			continue
		}
		break // 第二帧：节点回传广播（非空）
	}
	if len(listPayload.Skills) == 0 {
		t.Fatalf("skills.list 广播未回传节点技能: %+v", listPayload)
	}
	_ = sawCache

	// 2) skills.get：skills.item 回传全文（ID 关联）。
	if err := ws.WriteJSON(NewEnvelope("sk-g1", TypeSkillsGet, SkillsGetReq{NodeID: "node-e2e", Name: "deploy"})); err != nil {
		t.Fatalf("skills.get: %v", err)
	}
	item := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeSkillsItem })
	var sd SkillsData
	_ = json.Unmarshal(item.Payload, &sd)
	if !sd.OK || sd.Name != "deploy" || !strings.Contains(sd.Content, "Deploy steps") {
		t.Fatalf("skills.item 内容不符: %+v", sd)
	}

	// 3) skills.save：受理回执 → 节点落盘 → agent.skills_list 广播含新技能。
	if err := ws.WriteJSON(NewEnvelope("sk-s1", TypeSkillsSave, SkillsSaveReq{NodeID: "node-e2e", Name: "my-new", Content: "---\nname: my-new\ndescription: new\n---\n\nBody.\n"})); err != nil {
		t.Fatalf("skills.save: %v", err)
	}
	op := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult })
	var o OpResult
	_ = json.Unmarshal(op.Payload, &o)
	if !o.OK {
		t.Fatalf("skills.save 应受理: %+v", o)
	}
	deadline = time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeSkills })
		_ = json.Unmarshal(e.Payload, &listPayload)
		for _, m := range listPayload.Skills {
			if m.Name == "my-new" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("保存后 skills 广播未含 my-new: %+v", listPayload.Skills)
	}
	// 节点侧落盘校验（仿真 Manager.Save）。
	if raw, err := s.node.skillRaw("my-new"); err != nil || !strings.Contains(raw, "Body.") {
		t.Fatalf("节点侧技能未落盘: %v %q", err, raw)
	}

	// 4) skills.delete：受理回执 → 广播列表不含已删技能。
	if err := ws.WriteJSON(NewEnvelope("sk-d1", TypeSkillsDelete, SkillsGetReq{NodeID: "node-e2e", Name: "my-new"})); err != nil {
		t.Fatalf("skills.delete: %v", err)
	}
	op2 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult })
	var o2 OpResult
	_ = json.Unmarshal(op2.Payload, &o2)
	if !o2.OK {
		t.Fatalf("skills.delete 应受理: %+v", o2)
	}
	deadline = time.Now().Add(5 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeSkills })
		_ = json.Unmarshal(e.Payload, &listPayload)
		still := false
		for _, m := range listPayload.Skills {
			if m.Name == "my-new" {
				still = true
			}
		}
		if !still {
			gone = true
			break
		}
	}
	if !gone {
		t.Fatalf("删除后 skills 广播仍含 my-new: %+v", listPayload.Skills)
	}
}

// TestE2E_RoadmapFullChain 自主开发规划板全链路：roadmap.list（缓存立即应答 +
// 节点回传广播）→ roadmap.dev 未选择被拒（执行前必须选择，移动端与服务端双重门禁）
// → roadmap.plan（节点落提案）→ roadmap.dev 勾选后执行（提案置 approved）
// → roadmap.delete（条目消失；agent.roadmap_data 广播同步全端）。
func TestE2E_RoadmapFullChain(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 预置一条既有提案（节点侧仿真规划板）。
	s.node.mu.Lock()
	s.node.rmItems = []RoadmapItem{{ID: "rm_seed", Title: "种子提案", Status: "proposed"}}
	s.node.mu.Unlock()

	// 1) roadmap.list：缓存立即应答（空）→ 节点回传广播（含种子提案）。
	if err := ws.WriteJSON(NewEnvelope("rm-l1", TypeRoadmapList, RoadmapListReq{NodeID: "node-e2e"})); err != nil {
		t.Fatalf("roadmap.list: %v", err)
	}
	var rd RoadmapDataPayload
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeRoadmap })
		_ = json.Unmarshal(e.Payload, &rd)
		if rd.NodeID == "node-e2e" && len(rd.Items) > 0 {
			break
		}
	}
	if len(rd.Items) == 0 || rd.Items[0].ID != "rm_seed" {
		t.Fatalf("roadmap 广播未回传节点规划板: %+v", rd)
	}

	// 2) 执行门禁：未选择（IDs 空）直接拒绝 —— 服务端不下发节点。
	if err := ws.WriteJSON(NewEnvelope("rm-d0", TypeRoadmapDev, RoadmapDevReq{NodeID: "node-e2e", IDs: []string{}})); err != nil {
		t.Fatalf("roadmap.dev(空): %v", err)
	}
	op0 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult })
	var o0 OpResult
	_ = json.Unmarshal(op0.Payload, &o0)
	if o0.OK || !strings.Contains(o0.Message, "请先选择") {
		t.Fatalf("未选择应被拒绝: %+v", o0)
	}
	if devs := s.node.roadmapDevIDs(); len(devs) != 0 {
		t.Fatalf("空 IDs 不应到达节点: %+v", devs)
	}

	// 3) roadmap.plan：受理 → 节点落提案 → agent.roadmap_data 广播刷新。
	// guidance 透传断言（2026-10 移动端引导意见链路回归）。
	if err := ws.WriteJSON(NewEnvelope("rm-p1", TypeRoadmapPlan, RoadmapPlanReq{NodeID: "node-e2e", Guidance: "优先移动端体验"})); err != nil {
		t.Fatalf("roadmap.plan: %v", err)
	}
	op1 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool {
		return e.Type == TypeOpResult && strings.Contains(string(e.Payload), "规划已受理")
	})
	var o1 OpResult
	_ = json.Unmarshal(op1.Payload, &o1)
	if !o1.OK {
		t.Fatalf("roadmap.plan 应受理: %+v", o1)
	}
	deadline = time.Now().Add(5 * time.Second)
	planSeen := false
	for time.Now().Before(deadline) && !planSeen {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeRoadmap })
		_ = json.Unmarshal(e.Payload, &rd)
		for _, it := range rd.Items {
			if it.ID == "rm_plan1" && it.Status == "proposed" {
				planSeen = true
			}
		}
	}
	if !planSeen {
		t.Fatalf("规划提案未随广播回传: %+v", rd.Items)
	}
	if gs := s.node.roadmapPlans(); len(gs) != 1 || gs[0] != "优先移动端体验" {
		t.Fatalf("guidance 未透传到节点: %+v", gs)
	}

	// 4) roadmap.dev 勾选执行：提案置 approved，节点执行入口收到精确 IDs。
	if err := ws.WriteJSON(NewEnvelope("rm-d1", TypeRoadmapDev, RoadmapDevReq{NodeID: "node-e2e", IDs: []string{"rm_seed", "rm_plan1"}, Mode: "parallel", Parallel: 2})); err != nil {
		t.Fatalf("roadmap.dev: %v", err)
	}
	op2 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool {
		return e.Type == TypeOpResult && strings.Contains(string(e.Payload), "已受理")
	})
	var o2 OpResult
	_ = json.Unmarshal(op2.Payload, &o2)
	if !o2.OK {
		t.Fatalf("勾选执行应受理: %+v", o2)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeRoadmap })
		_ = json.Unmarshal(e.Payload, &rd)
		n := 0
		for _, it := range rd.Items {
			if it.Status == "approved" {
				n++
			}
		}
		if n >= 2 {
			break
		}
	}
	if rd.Stats.Approved < 2 {
		t.Fatalf("勾选执行后 approved 应≥2: %+v", rd.Stats)
	}
	if devs := s.node.roadmapDevIDs(); len(devs) != 1 || len(devs[0]) != 2 {
		t.Fatalf("节点执行入口未收到勾选 IDs: %+v", devs)
	}

	// 5) roadmap.delete：删除后广播不再含该条目。
	if err := ws.WriteJSON(NewEnvelope("rm-x1", TypeRoadmapDelete, RoadmapDeleteReq{NodeID: "node-e2e", ID: "rm_plan2"})); err != nil {
		t.Fatalf("roadmap.delete: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeRoadmap })
		_ = json.Unmarshal(e.Payload, &rd)
		still := false
		for _, it := range rd.Items {
			if it.ID == "rm_plan2" {
				still = true
			}
		}
		if !still {
			gone = true
			break
		}
	}
	if !gone {
		t.Fatalf("删除后规划板广播仍含 rm_plan2: %+v", rd.Items)
	}
	if dels := s.node.roadmapDeletes(); len(dels) != 1 || dels[0] != "rm_plan2" {
		t.Fatalf("删除请求未到达节点: %+v", dels)
	}
}

// TestE2E_DeleteTaskRecord 删除任务记录闭环：手机端 delete.task（终态）→
// op.result ok → tasks/sessions 广播同步移除 → 会话镜像清空；
// 运行中任务删除被拒（op.result ok=false）。
func TestE2E_DeleteTaskRecord(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 发布任务并等终态。
	if err := ws.WriteJSON(NewEnvelope("", TypeCreateTask, CreateTask{Text: "待删除的任务"})); err != nil {
		t.Fatalf("create.task: %v", err)
	}
	created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
	var tc struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(created.Payload, &tc)
	s.node.waitDispatch(5 * time.Second)

	// 第二个任务保持运行中（hold 长任务模式锁住，验证运行中删除被拒）。
	s.node.setHold(true)
	if err := ws.WriteJSON(NewEnvelope("ct-2", TypeCreateTask, CreateTask{Text: "运行中任务"})); err != nil {
		t.Fatalf("create.task#2: %v", err)
	}
	created2 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated && e.ID == "ct-2" })
	var tc2 struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(created2.Payload, &tc2)
	s.node.waitDispatchN(2, 5*time.Second)

	// 运行中任务删除被拒（回归：两任务同 ID 覆盖曾致此处误删成功，见 newID 修复注释）。
	if tc.Task.ID == tc2.Task.ID {
		t.Fatalf("两任务 ID 冲突（newID 唯一性破坏）: %s", tc.Task.ID)
	}
	if err := ws.WriteJSON(NewEnvelope("dt-run", TypeDeleteTask, DeleteTask{TaskID: tc2.Task.ID})); err != nil {
		t.Fatalf("delete.task(running): %v", err)
	}
	op2 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "dt-run" })
	var o2 OpResult
	_ = json.Unmarshal(op2.Payload, &o2)
	if o2.OK {
		t.Fatal("运行中任务删除应被拒")
	}

	// 解除长任务：task.cancel（节点 Cancel 回调关 released → 回取消终态）→ 任务落终态。
	if err := ws.WriteJSON(NewEnvelope("", TypeTaskCancel, TaskCancel{TaskID: tc2.Task.ID})); err != nil {
		t.Fatalf("task.cancel#2: %v", err)
	}
	readUntil(t, ws, 8*time.Second, func(e Envelope) bool { return e.Type == TypeTaskResult })

	// 终态保护落库轮询：task.result 广播与 DeleteTask 的 running 检查之间有时序窗
	//（task1 终态经节点上行链路异步到达，可能晚于 task2 的取消广播），直接删会偶现
	//「任务仍在 执行中」。此处以服务端任务表为权威事实，轮询等两任务都落终态再删。
	waitTerminal := func(id string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if tsk, ok := s.hub.GetTask(id); ok && tsk.Status != TaskStatusPending && tsk.Status != TaskStatusRunning {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		tsk, _ := s.hub.GetTask(id)
		status := "<不存在>"
		if tsk != nil {
			status = tsk.Status
		}
		t.Fatalf("任务未在限时内落终态: %s status=%s", id, status)
	}
	waitTerminal(tc.Task.ID)
	waitTerminal(tc2.Task.ID)

	// 终态任务删除：两个任务（done + canceled）均可删，op.result ok。
	for i, id := range []string{tc.Task.ID, tc2.Task.ID} {
		delID := fmt.Sprintf("dt-%d", i+1)
		if err := ws.WriteJSON(NewEnvelope(delID, TypeDeleteTask, DeleteTask{TaskID: id})); err != nil {
			t.Fatalf("delete.task(%s): %v", id, err)
		}
		op := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == delID })
		var o OpResult
		_ = json.Unmarshal(op.Payload, &o)
		if !o.OK {
			t.Fatalf("终态任务删除应成功（%s）: %+v", id, o)
		}
	}

	// tasks/sessions 广播同步移除（服务端删除后主动广播）。
	deadline := time.Now().Add(5 * time.Second)
	tasksGone, sessGone := false, false
	for time.Now().Before(deadline) && (!tasksGone || !sessGone) {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(en Envelope) bool {
			return en.Type == TypeTasks || en.Type == "sessions"
		})
		if e.Type == TypeTasks {
			var list []*Task
			_ = json.Unmarshal(e.Payload, &list)
			tasksGone = true
			for _, tt := range list {
				if tt != nil && (tt.ID == tc.Task.ID || tt.ID == tc2.Task.ID) {
					t.Fatalf("tasks 广播仍含已删任务: %s", tt.ID)
				}
			}
		} else {
			var p struct {
				Sessions []map[string]interface{} `json:"sessions"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			sessGone = true
			for _, ss := range p.Sessions {
				if ss["sessionID"] == tc.Task.SessionID {
					t.Fatalf("sessions 广播仍含已删会话: %v", ss["sessionID"])
				}
			}
		}
	}
	if !tasksGone || !sessGone {
		t.Fatalf("删除后未收到 tasks/sessions 广播: tasks=%v sessions=%v", tasksGone, sessGone)
	}

	// 服务端任务与会话镜像同步消失。
	if _, ok := s.hub.GetTask(tc.Task.ID); ok {
		t.Fatal("hub 任务未删")
	}
	if msgs := s.hub.SessionMessages(tc.Task.SessionID); len(msgs) != 0 {
		t.Fatalf("hub 会话镜像未清: %d", len(msgs))
	}
}

// TestE2E_NodeConfigRemote 銳点链路：远端读取/修改 PC 节点配置（get.config / set.config）。
// 覆盖：
//  1. get.config → 服务端缓存立即应答（首次为空）+ 节点实时回传 agent.config →
//     hub 缓存 + node.config 广播（掩码 APIKey，明文不出节点）；
//  2. set.config 部分字段更新（指针 = 显式下发）→ 节点合并保存 → agent.config 回报 →
//     node.config 广播；掩码形态密钥不回写（保持明文不变）；
//  3. 再拉 get.config → 应答即更新后缓存快照；
//  4. 节点保存失败 → server.error 广播（远端感知失败）。
func TestE2E_NodeConfigRemote(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// ---- 1. get.config：缓存立即应答（首次无缓存 → 空 config）+ 实时快照广播覆盖 ----
	if err := ws.WriteJSON(NewEnvelope("gc-1", TypeGetConfig, GetConfig{NodeID: "node-e2e"})); err != nil {
		t.Fatalf("get.config 发送失败: %v", err)
	}
	resp := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeNodeConfig && e.ID == "gc-1" })
	var ncResp struct {
		NodeID string     `json:"nodeId"`
		Config NodeConfig `json:"config"`
	}
	if err := json.Unmarshal(resp.Payload, &ncResp); err != nil {
		t.Fatalf("node.config 应答解析失败: %v", err)
	}
	if ncResp.NodeID != "node-e2e" {
		t.Fatalf("应答 nodeId 不符: %q", ncResp.NodeID)
	}

	// 实时链路：server.get_config → 节点回传 agent.config → hub 广播 node.config。
	bc := readUntil(t, ws, 5*time.Second, func(e Envelope) bool {
		return e.Type == TypeNodeConfig && e.ID == ""
	})
	var bcP struct {
		NodeID string     `json:"nodeId"`
		Config NodeConfig `json:"config"`
	}
	if err := json.Unmarshal(bc.Payload, &bcP); err != nil {
		t.Fatalf("node.config 广播解析失败: %v", err)
	}
	if bcP.NodeID != "node-e2e" {
		t.Fatalf("广播 nodeId 不符: %q", bcP.NodeID)
	}
	if bcP.Config.BaseURL != "https://api.desktop.local/v1" || bcP.Config.Model != "claude-sonnet-4-5" {
		t.Fatalf("实时快照与节点配置不符: %+v", bcP.Config)
	}
	if !strings.Contains(bcP.Config.APIKey, "****") {
		t.Fatalf("APIKey 出站应掩码: %q", bcP.Config.APIKey)
	}

	// hub 缓存同步落地（get.config 二拉应答即实时快照）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.hub.NodeConfigOf("node-e2e"); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := s.hub.NodeConfigOf("node-e2e"); !ok {
		t.Fatal("节点配置未落 hub 缓存")
	}

	// ---- 2. set.config：改模型 + 上下文窗口；掩码密钥不下发（保持不变）----
	newModel := "gpt-5.2"
	newCW := 128000
	sentKey := "sk-****9999" // 远端只见掩码：原样回传 = 未修改
	if err := ws.WriteJSON(NewEnvelope("sc-1", TypeSetConfig, SetConfig{
		NodeID: "node-e2e", Model: &newModel, ContextWindow: &newCW, APIKey: sentKey,
	})); err != nil {
		t.Fatalf("set.config 发送失败: %v", err)
	}

	// 节点侧收到合并保存指令（真实确认来自节点 agent.config 回报，非立即 op.result）。
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c := s.node.cfg(); c.Model == newModel && c.ContextWindow == newCW {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	merged := s.node.cfg()
	if merged.Model != newModel || merged.ContextWindow != newCW {
		t.Fatalf("节点未合并保存配置: %+v", merged)
	}
	// 掩码形态密钥未被回写（仍是节点本地明文）。
	if merged.APIKey != "sk-e2e-secret-9999" {
		t.Fatalf("掩码密钥被回写覆盖: %q", merged.APIKey)
	}

	// 成功回报：node.config 广播（客户端刷新）。
	done := readUntil(t, ws, 5*time.Second, func(e Envelope) bool {
		return e.Type == TypeNodeConfig && e.ID == ""
	})
	var doneP struct {
		NodeID string     `json:"nodeId"`
		Config NodeConfig `json:"config"`
	}
	_ = json.Unmarshal(done.Payload, &doneP)
	if doneP.NodeID != "node-e2e" || doneP.Config.Model != newModel {
		t.Fatalf("保存成功回报异常: %+v", doneP)
	}
	if !strings.Contains(doneP.Config.APIKey, "****") {
		t.Fatalf("回报 APIKey 应掩码: %q", doneP.Config.APIKey)
	}

	// ---- 3. 再拉 get.config：应答即保存后快照 ----
	if err := ws.WriteJSON(NewEnvelope("gc-2", TypeGetConfig, GetConfig{NodeID: "node-e2e"})); err != nil {
		t.Fatalf("get.config#2 发送失败: %v", err)
	}
	resp2 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeNodeConfig && e.ID == "gc-2" })
	var nc2 struct {
		Config NodeConfig `json:"config"`
	}
	_ = json.Unmarshal(resp2.Payload, &nc2)
	if nc2.Config.Model != newModel || nc2.Config.ContextWindow != newCW {
		t.Fatalf("缓存快照未更新: %+v", nc2.Config)
	}

	// ---- 4. 节点保存失败 → server.error 广播 ----
	s.node.setCfgErr(true)
	badModel := "broken-model"
	if err := ws.WriteJSON(NewEnvelope("sc-2", TypeSetConfig, SetConfig{
		NodeID: "node-e2e", Model: &badModel,
	})); err != nil {
		t.Fatalf("set.config#2 发送失败: %v", err)
	}
	errEnv := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeServerError })
	var ep ErrorPayload
	_ = json.Unmarshal(errEnv.Payload, &ep)
	if !strings.Contains(ep.Message, "保存配置失败") {
		t.Fatalf("server.error 未透传节点失败原因: %+v", ep)
	}
	// 失败后节点配置保持原值（未被半写）。
	if got := s.node.cfg().Model; got != newModel {
		t.Fatalf("失败保存不应改动配置: %q", got)
	}
}

// TestE2E_DeleteSessionSyncNode 会话删除联动 PC 端闭环：delete.session →
// 服务端记录+镜像全清 + 广播 tasks/sessions + 路由 server.delete_session →
// 节点回调收到 sessionID（仿真桌面端本地存档删除）；运行中会话删除被拒。
func TestE2E_DeleteSessionSyncNode(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 预置节点侧会话历史（验证镜像与节点存档双源）。
	if err := ws.WriteJSON(NewEnvelope("", TypeCreateTask, CreateTask{Text: "待删会话任务"})); err != nil {
		t.Fatalf("create.task: %v", err)
	}
	created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
	var tc struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(created.Payload, &tc)
	s.node.setMessages(tc.Task.SessionID, []interface{}{map[string]interface{}{"say": "text", "text": "历史内容"}})
	s.node.waitDispatch(5 * time.Second)
	readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskResult })

	// delete.session：服务端删记录+镜像 + 路由节点侧删除。
	if err := ws.WriteJSON(NewEnvelope("ds-1", TypeDeleteSession, DeleteSession{SessionID: tc.Task.SessionID})); err != nil {
		t.Fatalf("delete.session: %v", err)
	}
	op := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "ds-1" })
	var o OpResult
	_ = json.Unmarshal(op.Payload, &o)
	if !o.OK {
		t.Fatalf("delete.session 应成功: %+v", o)
	}
	// 服务端：任务+镜像同步消失。
	if _, ok := s.hub.GetTask(tc.Task.ID); ok {
		t.Fatal("delete.session 未删任务记录")
	}
	if msgs := s.hub.SessionMessages(tc.Task.SessionID); len(msgs) != 0 {
		t.Fatalf("delete.session 未清镜像: %d", len(msgs))
	}
	// 节点联动：回调收到 sessionID（PC 端本地存档同步删除）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, sid := range s.node.deletedList() {
			if sid == tc.Task.SessionID {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("节点侧未收到会话删除联动: %v", s.node.deletedList())
}

// TestE2E_RenameSessionSyncAll 会话重命名闭环：rename.session → 服务端写任务
// Title + 落盘 + 广播 tasks/sessions；会话摘要 title 优先手动名（移动端/管理端
// 同步显示）；空名/空会话被拒。
func TestE2E_RenameSessionSyncAll(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 预置一个已完成会话。
	if err := ws.WriteJSON(NewEnvelope("", TypeCreateTask, CreateTask{Text: "原始任务标题文本"})); err != nil {
		t.Fatalf("create.task: %v", err)
	}
	created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
	var tc struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(created.Payload, &tc)
	s.node.waitDispatch(5 * time.Second)
	readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskResult })

	// rename.session：手动命名 → op.result ok + tasks/sessions 广播。
	if err := ws.WriteJSON(NewEnvelope("rn-1", TypeRenameSession, RenameSession{SessionID: tc.Task.SessionID, Title: "我的交货会话"})); err != nil {
		t.Fatalf("rename.session: %v", err)
	}
	op := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "rn-1" })
	var o OpResult
	_ = json.Unmarshal(op.Payload, &o)
	if !o.OK {
		t.Fatalf("rename.session 应成功: %+v", o)
	}
	// 服务端任务记录：Title 已写入手动名（tasks.json 持久化同源）。
	t2, ok := s.hub.GetTask(tc.Task.ID)
	if !ok || t2.Title != "我的交货会话" {
		t.Fatalf("rename.session 未写入 Task.Title: %+v", t2)
	}
	// 广播链路：sessions 摘要 title 优先手动名（移动端列表/聊天页数据源）。
	sessions := s.hub.ListSessionSummaries()
	found := false
	for _, sm := range sessions {
		if sm["sessionID"] == tc.Task.SessionID {
			found = true
			if sm["title"] != "我的交货会话" {
				t.Fatalf("会话摘要应优先手动名: %v", sm["title"])
			}
		}
	}
	if !found {
		t.Fatal("会话摘要未找到目标会话")
	}
	// 广播帧：tasks + sessions 都应到达手机端。
	readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTasks })
	readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == "sessions" })

	// 空名被拒。
	if err := ws.WriteJSON(NewEnvelope("rn-2", TypeRenameSession, RenameSession{SessionID: tc.Task.SessionID, Title: "  "})); err != nil {
		t.Fatalf("rename.session 空名: %v", err)
	}
	op2 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "rn-2" })
	var o2 OpResult
	_ = json.Unmarshal(op2.Payload, &o2)
	if o2.OK {
		t.Fatal("空名 rename.session 应被拒")
	}
}

// TestE2E_PinSessionSyncAll 会话置顶闭环：pin.session → 服务端写任务 Pinned
// + 落盘 + 广播 tasks/sessions；会话摘要 pinned 投影生效且置顶会话排序最前；
// 再次 pin(false) 取消置顶恢复原序；空会话被拒。
func TestE2E_PinSessionSyncAll(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 预置两个已完成会话（p2 创建更晚；不置顶时 p2 应排最前）。
	createDone := func(text string) Task {
		if err := ws.WriteJSON(NewEnvelope("", TypeCreateTask, CreateTask{Text: text})); err != nil {
			t.Fatalf("create.task: %v", err)
		}
		created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
		var tc struct {
			Task Task `json:"task"`
		}
		_ = json.Unmarshal(created.Payload, &tc)
		s.node.waitDispatch(5 * time.Second)
		readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskResult })
		return tc.Task
	}
	p1 := createDone("置顶测试旧会话")
	p2 := createDone("置顶测试新会话")

	// pin.session 置顶旧会话 p1 → op.result ok + tasks/sessions 广播。
	if err := ws.WriteJSON(NewEnvelope("pin-1", TypePinSession, PinSession{SessionID: p1.SessionID, Pinned: true})); err != nil {
		t.Fatalf("pin.session: %v", err)
	}
	op := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "pin-1" })
	var o OpResult
	_ = json.Unmarshal(op.Payload, &o)
	if !o.OK {
		t.Fatalf("pin.session 应成功: %+v", o)
	}
	// 服务端任务记录：Pinned 已写毫秒时间戳（tasks.json 持久化同源）。
	t1, ok := s.hub.GetTask(p1.ID)
	if !ok || t1.Pinned == 0 {
		t.Fatalf("pin.session 未写入 Task.Pinned: %+v", t1)
	}
	// 广播帧：tasks + sessions 都应到达手机端。
	readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTasks })
	readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == "sessions" })
	// 会话摘要：pinned 投影生效 + 置顶会话排最前（原排序应是新会话 p2 在前）。
	sessions := s.hub.ListSessionSummaries()
	if len(sessions) == 0 || sessions[0]["sessionID"] != p1.SessionID {
		t.Fatalf("置顶会话应排最前: %+v", sessions)
	}
	found := false
	for _, sm := range sessions {
		if sm["sessionID"] == p1.SessionID && sm["pinned"] != true {
			t.Fatalf("置顶会话摘要 pinned 投影应非空: %v", sm["pinned"])
		}
		if sm["sessionID"] == p2.SessionID && sm["pinned"] == true {
			t.Fatal("未置顶会话 pinned 投影应为空")
		}
		if sm["sessionID"] == p1.SessionID {
			found = true
		}
	}
	if !found {
		t.Fatal("会话摘要未找到置顶会话")
	}

	// 取消置顶：pinned 投影消失，排序恢复创建时间倒序（p2 回到最前）。
	if err := ws.WriteJSON(NewEnvelope("pin-2", TypePinSession, PinSession{SessionID: p1.SessionID, Pinned: false})); err != nil {
		t.Fatalf("pin.session 取消: %v", err)
	}
	op2 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "pin-2" })
	var o2 OpResult
	_ = json.Unmarshal(op2.Payload, &o2)
	if !o2.OK {
		t.Fatalf("取消置顶应成功: %+v", o2)
	}
	sessions = s.hub.ListSessionSummaries()
	if sessions[0]["sessionID"] != p2.SessionID {
		t.Fatalf("取消置顶后新会话应回最前: %v", sessions[0])
	}
	// 空会话被拒。
	if err := ws.WriteJSON(NewEnvelope("pin-3", TypePinSession, PinSession{SessionID: "  "})); err != nil {
		t.Fatalf("pin.session 空会话: %v", err)
	}
	op3 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "pin-3" })
	var o3 OpResult
	_ = json.Unmarshal(op3.Payload, &o3)
	if o3.OK {
		t.Fatal("空会话 pin.session 应被拒")
	}
}

// TestE2E_ConcurrentDispatchNoSerial 并发派发验收：手机端连发 3 个任务，
// hold 模式下节点同步派发不互斥（修复：CreateTask 锁内「按会话空闲才派发」
// 与 CurrentTask 单徽标导致的隐式串行）。同时验证心跳回传 RunningCount 与
// 服务端节点卡并发计数一致（负载感知数据链路）。
func TestE2E_ConcurrentDispatchNoSerial(t *testing.T) {
	s := newE2E(t)
	s.node.setHold(true) // 长任务模式：不回终态，保持并发窗口
	ws := s.dialMobile("user", "user123")

	// 连发 3 个任务（同一会话串行语义仅限同会话；不同会话并发派发）。
	ids := make([]string, 3)
	for i := 0; i < 3; i++ {
		if err := ws.WriteJSON(NewEnvelope("", TypeCreateTask, CreateTask{Text: fmt.Sprintf("并发任务%d", i)})); err != nil {
			t.Fatalf("create.task %d: %v", i, err)
		}
		created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
		var c struct {
			Task Task `json:"task"`
		}
		_ = json.Unmarshal(created.Payload, &c)
		ids[i] = c.Task.ID
	}
	if got := s.node.waitDispatchN(3, 5*time.Second); got.TaskID == "" {
		t.Fatal("3 个任务未全部派发到节点")
	}

	// 节点侧真实并发（Client.RunningCount）：3 个会话各自登记，互不阻塞。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.client.RunningCount() == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.client.RunningCount(); got != 3 {
		t.Fatalf("节点应并发 3 任务，got %d", got)
	}

	// 服务端负载感知：心跳间隔内 dispatchLocked 已增量计数（节点卡并发展示数据源）。
	nodeRunning := func() int {
		for _, n := range s.hub.ListNodes() {
			if n.ID == "node-e2e" {
				return n.RunningTasks
			}
		}
		return -1
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if nodeRunning() >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := nodeRunning(); got < 3 {
		t.Fatalf("服务端节点并发计数应 ≥3（dispatch 增量）: %d", got)
	}

	// 心跳回传校正（首跳 15s 周期外，手动模拟权威同步）：节点真实计数覆盖服务端。
	s.hub.TouchAgent("node-e2e", s.client.RunningCount())
	if got := nodeRunning(); got != 3 {
		t.Fatalf("心跳校正后服务端计数应与节点一致 =3: %d", got)
	}

	// 取消其中 1 个：并发 3→2（cancel 链路顺带回归计数递减）。
	if err := ws.WriteJSON(NewEnvelope("", TypeTaskCancel, TaskCancel{TaskID: ids[0]})); err != nil {
		t.Fatalf("task.cancel: %v", err)
	}
	readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskResult })
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if nodeRunning() <= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := nodeRunning(); got > 2 {
		t.Fatalf("取消后并发计数应递减至 ≤2: %d", got)
	}
	// 释放剩余长任务（清理现场：兑底超时前归还）。
	s.node.releasedChanClose()
}

// TestE2E_AnalyzeSession 会话 AI 分析闭环：analyze.session → 服务端路由 →
// 节点 Analyze 回调 → agent.analyze_done → 服务端广播 → 手机端收到分析结果。
func TestE2E_AnalyzeSession(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 发布任务并等完成（会话有历史后才可分析）。
	if err := ws.WriteJSON(NewEnvelope("", TypeCreateTask, CreateTask{Text: "待分析任务"})); err != nil {
		t.Fatalf("create.task: %v", err)
	}
	created := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskCreated })
	var tc struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(created.Payload, &tc)
	s.node.waitDispatch(5 * time.Second)
	readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeTaskResult })

	// 注入分析执行：仿真桌面端 LLM 分析路径（真实协议回传 analyze.done）。
	s.node.setAnalyzeHook(func(sessionID string) {
		s.client.SendAnalyzeDone(AnalyzeDone{SessionID: sessionID, OK: true, Analysis: "## 用户问题理解\n测试分析内容"})
	})

	// 手机端发起分析：受理 op.result ok → analyze.done 广播（分析文本回传）。
	if err := ws.WriteJSON(NewEnvelope("as-1", TypeAnalyzeSession, AnalyzeSession{SessionID: tc.Task.SessionID})); err != nil {
		t.Fatalf("analyze.session: %v", err)
	}
	op := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "as-1" })
	var o OpResult
	_ = json.Unmarshal(op.Payload, &o)
	if !o.OK {
		t.Fatalf("analyze.session 受理应成功: %+v", o)
	}
	done := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeAnalyzeDone })
	var ad AnalyzeDone
	_ = json.Unmarshal(done.Payload, &ad)
	if !ad.OK || ad.SessionID != tc.Task.SessionID || !strings.Contains(ad.Analysis, "用户问题理解") {
		t.Fatalf("analyze.done 内容异常: %+v", ad)
	}
	// 节点侧确实收到了分析指令。
	if len(s.node.analysesList()) == 0 {
		t.Fatal("节点侧未收到 analyze.session")
	}
}

// TestE2E_GroupFullChain A2A 群聊全链路（P1 验收）：employee.list（建群选成员
// 数据源）→ group.create（受理 → 节点建群 → agent.group_state 广播 groups）→
// group.list / group.sync（拉权威快照含消息历史）→ group.msg（节点入历史 +
// group.message 广播）→ group.task（群助手派发员工 → 员工产出 + group.task_done
// 广播）→ group.join / group.leave（成员增删 → groups 广播刷新）。
func TestE2E_GroupFullChain(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// ---- 0. employee.list：缓存立即应答 + 节点回传广播（建群选成员数据源） ----
	if err := ws.WriteJSON(NewEnvelope("emp-l1", TypeEmployeeList, EmployeeListReq{NodeID: "node-e2e"})); err != nil {
		t.Fatalf("employee.list: %v", err)
	}
	var empPayload struct {
		Employees []EmployeeMeta `json:"employees"`
		NodeID    string         `json:"nodeId"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == "employees" })
		_ = json.Unmarshal(e.Payload, &empPayload)
		if empPayload.NodeID != "node-e2e" {
			continue
		}
		if len(empPayload.Employees) >= 2 {
			break // 节点回传广播（非空）
		}
	}
	if len(empPayload.Employees) < 2 {
		t.Fatalf("employee.list 未回传节点员工: %+v", empPayload)
	}

	// ---- 1. group.create：受理 op.result ok → 节点建群 → agent.group_state → groups 广播 ----
	if err := ws.WriteJSON(NewEnvelope("gc-1", TypeGroupCreate, GroupCreateReq{Name: "写作协作组", Members: []string{"emp-writer", "emp-analyst"}})); err != nil {
		t.Fatalf("group.create: %v", err)
	}
	op := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "gc-1" })
	var o OpResult
	_ = json.Unmarshal(op.Payload, &o)
	if !o.OK {
		t.Fatalf("group.create 应受理: %+v", o)
	}

	// 节点侧建群完成 + 服务端投影：group.state 广播（agent.group_state → groups）。
	var gid string
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeGroups })
		var gp struct {
			Groups []Group `json:"groups"`
		}
		_ = json.Unmarshal(e.Payload, &gp)
		for _, g := range gp.Groups {
			if g.Name == "写作协作组" {
				gid = g.ID
				break
			}
		}
		if gid != "" {
			break
		}
	}
	if gid == "" {
		t.Fatal("groups 广播未含新群（节点建群→agent.group_state→服务端投影链路断裂）")
	}

	// 群元数据校验：owner + 成员（user + 2 员工 + bot）。
	grp := s.node.groupList()
	var found *Group
	for i := range grp {
		if grp[i].ID == gid {
			found = &grp[i]
		}
	}
	if found == nil {
		t.Fatal("节点侧未找到新建群")
	}
	if found.Name != "写作协作组" || found.Owner != "user:remote" || found.NodeID != "node-e2e" {
		t.Fatalf("群元数据异常: %+v", *found)
	}
	memberIDs := map[string]bool{}
	for _, m := range found.Members {
		memberIDs[m.ID] = true
	}
	for _, want := range []string{"user:remote", "emp-writer", "emp-analyst", "bot:group"} {
		if !memberIDs[want] {
			t.Fatalf("群成员缺失 %s: %+v", want, found.Members)
		}
	}

	// ---- 2. group.sync：拉到权威快照（含欢迎消息历史） ----
	if err := ws.WriteJSON(NewEnvelope("gs-1", TypeGroupSync, GroupListReq{GroupID: gid})); err != nil {
		t.Fatalf("group.sync: %v", err)
	}
	var syncSeen bool
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !syncSeen {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeGroupState })
		var gsp GroupStatePayload
		_ = json.Unmarshal(e.Payload, &gsp)
		for _, g := range gsp.Groups {
			if g.ID == gid {
				for _, m := range gsp.Messages {
					if m.GroupID == gid && strings.Contains(m.Text, "欢迎加入") {
						syncSeen = true
					}
				}
			}
		}
	}
	if !syncSeen {
		t.Fatal("group.sync 未回传群状态快照（含欢迎消息）")
	}

	// ---- 3. group.msg：节点入历史 + group.message 广播 ----
	if err := ws.WriteJSON(NewEnvelope("gm-1", TypeGroupMsg, GroupMsgReq{GroupID: gid, Text: "大家好，开始写作吧"})); err != nil {
		t.Fatalf("group.msg: %v", err)
	}
	op2 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "gm-1" })
	var o2 OpResult
	_ = json.Unmarshal(op2.Payload, &o2)
	if !o2.OK {
		t.Fatalf("group.msg 应答异常: %+v", o2)
	}
	msgSeen := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !msgSeen {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeGroupMessage })
		var gm GroupMessage
		_ = json.Unmarshal(e.Payload, &gm)
		if gm.GroupID == gid && gm.Text == "大家好，开始写作吧" {
			msgSeen = true
		}
	}
	if !msgSeen {
		t.Fatal("group.message 广播未回传用户消息")
	}

	// ---- 4. group.task：群助手派发给员工 → 员工产出 + group.task_done ----
	if err := ws.WriteJSON(NewEnvelope("gt-1", TypeGroupTask, GroupTaskReq{GroupID: gid, Instruction: "写一篇产品介绍", AssignedTo: "emp-writer"})); err != nil {
		t.Fatalf("group.task: %v", err)
	}
	op3 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "gt-1" })
	var o3 OpResult
	_ = json.Unmarshal(op3.Payload, &o3)
	if !o3.OK {
		t.Fatalf("group.task 应受理: %+v", o3)
	}

	// 群消息广播 + 派发消息 + 员工产出（异步执行毫秒级）。
	var sawTaskDone bool
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sawTaskDone {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool {
			return e.Type == TypeGroupTaskDone || e.Type == TypeGroupMessage
		})
		if e.Type == TypeGroupTaskDone {
			var gt GroupTask
			_ = json.Unmarshal(e.Payload, &gt)
			if gt.GroupID == gid && gt.AssignedTo == "emp-writer" && gt.Status == "done" && strings.Contains(gt.Result, "写一篇产品介绍") {
				sawTaskDone = true
			}
		} else {
			var gm GroupMessage
			_ = json.Unmarshal(e.Payload, &gm)
			if gm.GroupID == gid && gm.AuthorKind == "employee" && strings.Contains(gm.Text, "产出：") {
				// 员工产出消息已广播
			}
		}
	}
	if !sawTaskDone {
		t.Fatal("group.task_done 未回传（群任务链路断裂）")
	}

	// ---- 5. group.join / group.leave：成员增删 → groups 广播刷新 ----
	if err := ws.WriteJSON(NewEnvelope("gj-1", TypeGroupJoin, GroupMemberReq{GroupID: gid, AgentID: "emp-analyst"})); err != nil {
		t.Fatalf("group.join: %v", err)
	}
	op4 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "gj-1" })
	var o4 OpResult
	_ = json.Unmarshal(op4.Payload, &o4)
	if !o4.OK {
		t.Fatalf("group.join 应受理: %+v", o4)
	}

	if err := ws.WriteJSON(NewEnvelope("gl-1", TypeGroupLeave, GroupMemberReq{GroupID: gid, AgentID: "emp-writer"})); err != nil {
		t.Fatalf("group.leave: %v", err)
	}
	op5 := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "gl-1" })
	var o5 OpResult
	_ = json.Unmarshal(op5.Payload, &o5)
	if !o5.OK {
		t.Fatalf("group.leave 应受理: %+v", o5)
	}

	// 广播刷新后群成员应为：user + emp-analyst + bot（emp-writer 已移出）。
	deadline = time.Now().Add(5 * time.Second)
	memberOK := false
	for time.Now().Before(deadline) && !memberOK {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeGroups })
		var gp struct {
			Groups []Group `json:"groups"`
		}
		_ = json.Unmarshal(e.Payload, &gp)
		for _, g := range gp.Groups {
			if g.ID != gid {
				continue
			}
			ids := map[string]bool{}
			for _, m := range g.Members {
				ids[m.ID] = true
			}
			if ids["user:remote"] && ids["emp-analyst"] && ids["bot:group"] && !ids["emp-writer"] {
				memberOK = true
			}
		}
	}
	if !memberOK {
		t.Fatal("join/leave 后群成员广播未生效（期望 user+emp-analyst+bot，无 emp-writer）")
	}
}

// TestE2E_GroupRelayChain 多员工接力协议级验证（P2 验收）：group.task 传
// assignedTo:"emp-writer,emp-analyst" → 节点按序接力执行 → 两员工产出消息
// 依次广播 + 最终 group.task_done 携带 Results（按序两条）。
func TestE2E_GroupRelayChain(t *testing.T) {
	s := newE2E(t)
	ws := s.dialMobile("user", "user123")

	// 建群（复用员工数据源；群状态权威在节点）。
	if err := ws.WriteJSON(NewEnvelope("rc-gc", TypeGroupCreate, GroupCreateReq{Name: "接力协作组", Members: []string{"emp-writer", "emp-analyst"}})); err != nil {
		t.Fatalf("group.create: %v", err)
	}
	readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "rc-gc" })
	var gid string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && gid == "" {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool { return e.Type == TypeGroups })
		var gp struct {
			Groups []Group `json:"groups"`
		}
		_ = json.Unmarshal(e.Payload, &gp)
		for _, g := range gp.Groups {
			if g.Name == "接力协作组" {
				gid = g.ID
				break
			}
		}
	}
	if gid == "" {
		t.Fatal("接力群建群未回传（groups 广播缺失）")
	}

	// 派发接力任务：assignedTo 逗号分隔两员工。
	if err := ws.WriteJSON(NewEnvelope("rc-gt", TypeGroupTask, GroupTaskReq{GroupID: gid, Instruction: "写产品介绍并分析竞品", AssignedTo: "emp-writer,emp-analyst"})); err != nil {
		t.Fatalf("group.task: %v", err)
	}
	op := readUntil(t, ws, 5*time.Second, func(e Envelope) bool { return e.Type == TypeOpResult && e.ID == "rc-gt" })
	var o OpResult
	_ = json.Unmarshal(op.Payload, &o)
	if !o.OK {
		t.Fatalf("group.task 接力应受理: %+v", o)
	}

	// 断言：依次收到两员工产出消息（emp-writer → emp-analyst）+ 最终 task_done
	//（Results 按序两条）。
	var order []string
	var final GroupTask
	haveDone := false
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !haveDone {
		e := readUntil(t, ws, deadline.Sub(time.Now()), func(e Envelope) bool {
			return e.Type == TypeGroupMessage || e.Type == TypeGroupTaskDone
		})
		if e.Type == TypeGroupMessage {
			var gm GroupMessage
			_ = json.Unmarshal(e.Payload, &gm)
			if gm.GroupID == gid && gm.AuthorKind == "employee" && gm.Author != "" {
				order = append(order, gm.Author)
			}
		} else {
			var gt GroupTask
			_ = json.Unmarshal(e.Payload, &gt)
			if gt.GroupID == gid {
				final = gt
				haveDone = true
			}
		}
	}
	if !haveDone {
		t.Fatal("接力任务 task_done 未回传")
	}
	// 顺序断言：emp-writer 先产出，emp-analyst 后产出。
	if len(order) != 2 || order[0] != "emp-writer" || order[1] != "emp-analyst" {
		t.Fatalf("接力产出顺序异常（期望 emp-writer→emp-analyst）: %v", order)
	}
	// task_done 断言：AssignedTo 逗号语义 + Results 按序两条。
	if final.AssignedTo != "emp-writer,emp-analyst" {
		t.Fatalf("task_done AssignedTo 应为逗号列表: %q", final.AssignedTo)
	}
	if final.Status != "done" || len(final.Results) != 2 {
		t.Fatalf("task_done Results 应两条: %+v", final)
	}
	if final.Results[0].EmployeeID != "emp-writer" || final.Results[1].EmployeeID != "emp-analyst" {
		t.Fatalf("Results 顺序异常: %+v", final.Results)
	}
}
