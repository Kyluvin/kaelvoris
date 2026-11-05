package desktop

// remote_group.go 远程 A2A 群聊节点侧模块（P1：协议 + 服务端路由 + 节点群模块）。
//
// 群状态权威在 PC 节点（员工主场：员工进程/工具/工作空间都在节点），服务端只做
// 路由/广播/投影缓存。本文件实现：
//   - 群元数据存储（~/.kaelvoris/groups/<gid>.json + 内存态）；
//   - ClientCallbacks 群回调（GroupCreate/GroupMsg/GroupTask/GroupJoin/GroupLeave/GroupSync）；
//   - 群助手最小版（GroupBot）：GroupTask 指令 → 分派给成员员工 → 聚合作品回群。
//
// 对齐 A2A 概念映射：AgentProfile（Agent Card）/ GroupMessage（Message/Part）/
// GroupTask（Task）/ GroupStatePayload（Agent Card 目录 + 消息流）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"kaelvoris/internal/remote"
	"kaelvoris/internal/session"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// groupDir 群存储根目录（~/.kaelvoris/groups/）。
func groupDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "kaelvoris-groups")
	}
	return filepath.Join(home, ".kaelvoris", "groups")
}

// groupsPath 群元数据文件路径。
func groupsPath(gid string) string { return filepath.Join(groupDir(), gid+".json") }

// groupStore 节点侧群存储（内存态 + 落盘）。
type groupStore struct {
	mu     sync.RWMutex
	groups map[string]*remote.Group
	// msgHist 群消息历史 {groupID: []GroupMessage}（节点持久化，服务端不落）。
	msgHist map[string][]remote.GroupMessage
	// lastHintAt 群助手提示节流（gid → 上次提示毫秒；shouldHint 用）。
	lastHintAt map[string]int64
}

// newGroupStore 创建群存储。
func newGroupStore() *groupStore {
	return &groupStore{groups: map[string]*remote.Group{}, msgHist: map[string][]remote.GroupMessage{}, lastHintAt: map[string]int64{}}
}

// ensureGroupStore 惰性初始化（App 字段）。
func (a *App) ensureGroupStore() *groupStore {
	a.groupMu.Lock()
	defer a.groupMu.Unlock()
	if a.groupSt == nil {
		a.groupSt = newGroupStore()
		a.groupSt.loadAll()
	}
	return a.groupSt
}

// loadAll 从 ~/.kaelvoris/groups/ 加载全部群（损坏文件跳过不致命）。
func (g *groupStore) loadAll() {
	entries, err := os.ReadDir(groupDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		gid := strings.TrimSuffix(e.Name(), ".json")
		b, err := os.ReadFile(groupsPath(gid))
		if err != nil {
			continue
		}
		var grp remote.Group
		if json.Unmarshal(b, &grp) != nil || grp.ID == "" {
			continue
		}
		g.groups[gid] = &grp
		// 消息历史：同目录 <gid>.messages.json。
		if mb, err := os.ReadFile(filepath.Join(groupDir(), gid+".messages.json")); err == nil {
			var msgs []remote.GroupMessage
			if json.Unmarshal(mb, &msgs) == nil {
				g.msgHist[gid] = msgs
			}
		}
	}
}

// get 读取群（只读快照指针；调用方勿改）。
func (g *groupStore) get(gid string) (*remote.Group, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	grp, ok := g.groups[gid]
	return grp, ok
}

// all 全部群列表。
func (g *groupStore) all() []remote.Group {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]remote.Group, 0, len(g.groups))
	for _, grp := range g.groups {
		out = append(out, *grp)
	}
	return out
}

// saveGroup 落盘群元数据（原子写；失败仅日志）。
func (g *groupStore) saveGroup(grp *remote.Group) {
	b, err := json.MarshalIndent(grp, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(groupDir(), 0o755); err != nil {
		return
	}
	tmp := groupsPath(grp.ID) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, groupsPath(grp.ID))
}

// appendMsg 追加群消息并落盘（历史上限 500 条，防膨胀）。
func (g *groupStore) appendMsg(m remote.GroupMessage) {
	g.mu.Lock()
	defer g.mu.Unlock()
	hist := g.msgHist[m.GroupID]
	hist = append(hist, m)
	if len(hist) > 500 {
		hist = hist[len(hist)-500:]
	}
	g.msgHist[m.GroupID] = hist
	if b, err := json.Marshal(hist); err == nil {
		_ = os.MkdirAll(groupDir(), 0o755)
		_ = os.WriteFile(filepath.Join(groupDir(), m.GroupID+".messages.json"), b, 0o600)
	}
}

// msgs 群消息历史。
func (g *groupStore) msgs(gid string) []remote.GroupMessage {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]remote.GroupMessage{}, g.msgHist[gid]...)
}

// shouldHint 群助手提示节流（同群 60s 最多一条；lastHintAt 惰性初始化）。
func (g *groupStore) shouldHint(gid string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lastHintAt == nil {
		g.lastHintAt = map[string]int64{}
	}
	now := time.Now().UnixMilli()
	if g.lastHintAt[gid] > 0 && now-g.lastHintAt[gid] < 60_000 {
		return false
	}
	g.lastHintAt[gid] = now
	return true
}

// ---------- App 接入 ----------

// remoteGroupCreate 创建群聊（server.group_create 回调）：
// 生成群 ID、加入创建者（user:<name>）与初始成员（员工），落盘并回传 agent.group_state。
func (a *App) remoteGroupCreate(req remote.GroupServerReq) {
	st := a.ensureGroupStore()
	gid := "g" + fmt.Sprintf("%d", time.Now().UnixMilli())
	grp := &remote.Group{
		ID:        gid,
		Name:      strings.TrimSpace(req.Name),
		Owner:     "user:remote",
		NodeID:    a.remoteNodeID(),
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	if grp.Name == "" {
		grp.Name = "新群聊"
	}
	// 初始成员：用户（始终） + 员工（Members 列表）。
	userProfile := remote.AgentProfile{ID: "user:remote", Name: "用户", Kind: "user"}
	grp.Members = append(grp.Members, userProfile)
	for _, empID := range req.Members {
		if empID == "" {
			continue
		}
		if e, ok := a.empStore.Get(empID); ok {
			grp.Members = append(grp.Members, remote.AgentProfile{
				ID: empID, Name: e.Name, Kind: "employee", Avatar: e.Avatar, Description: e.Description,
			})
		}
	}
	// 群助手机器人（协调者）。
	grp.Members = append(grp.Members, remote.AgentProfile{ID: "bot:group", Name: "群助手", Kind: "bot", Description: "协调群内写任务"})
	st.mu.Lock()
	st.groups[gid] = grp
	st.mu.Unlock()
	st.saveGroup(grp)
	// 欢迎消息 + 回传状态。
	sysMsg := remote.GroupMessage{GroupID: gid, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot", Text: "群聊已创建，欢迎加入！", TS: time.Now().UnixMilli()}
	st.appendMsg(sysMsg)
	a.remoteSendGroups()
	// 本地事件桥：欢迎消息实时渲染（节点写历史后主动推前端）。
	a.pushGroupMsg(gid)
	slog.Info("远程群聊已创建", "gid", gid, "name", grp.Name, "members", len(grp.Members))
}

// remoteGroupMsg 群消息（server.group_msg 回调）：追加历史 + 广播成员员工 + 回传。
// @提及自动应答（2026-12 修复「发消息没人回」）：@员工名 → 转群任务接力执行
// （复用 remoteGroupTask 链路，产出渐进回群）；未提及时群助手节流提示用法
// （60s 一次，防每条消息刷屏），群内无员工时提示先添加成员。
func (a *App) remoteGroupMsg(req remote.GroupServerReq) {
	st := a.ensureGroupStore()
	grp, ok := st.get(req.GroupID)
	if !ok {
		slog.Warn("群消息：群不存在", "gid", req.GroupID)
		return
	}
	msg := remote.GroupMessage{
		GroupID: grp.ID, Author: "user:remote", AuthorName: "用户", AuthorKind: "user",
		Text: strings.TrimSpace(req.Text), TS: time.Now().UnixMilli(),
	}
	st.appendMsg(msg)
	a.remoteSendGroupMessage(msg)
	// 本地事件桥：用户消息回显（GroupMsg 绑定已单独推；桌面端未登录远程账号时
	// 节点服务端广播不可达，须在写历史后主动推前端，保证本地群聊实时渲染）。
	a.pushGroupMsg(grp.ID)
	// @提及：被点名员工自动应答（复用群任务接力链，产出回群）。
	if ids := a.groupMentionedEmployees(grp, req.Text); len(ids) > 0 {
		a.remoteGroupTask(remote.GroupServerReq{GroupID: grp.ID, Instruction: strings.TrimSpace(req.Text), AssignedTo: strings.Join(ids, ",")})
		return
	}
	// 未提及任何员工：群助手节流提示（防「发消息石沉大海」的静默体验）。
	a.groupHintNoReply(st, grp)
}

// groupMentionedEmployees 解析群消息中的 @提及（@员工名，支持多个）。匹配带
// 名字边界校验：@名字 后不能紧跟汉字（防「@张三丰」被子串误命中短名「张三」）；
// 后接空白/标点/末尾才算命中。无提及 = nil（不自动派任务，避免每条消息都烧 LLM）。
func (a *App) groupMentionedEmployees(grp *remote.Group, text string) []string {
	text = strings.TrimSpace(text)
	if grp == nil || text == "" {
		return nil
	}
	type cand struct{ id, name string }
	var cands []cand
	for _, m := range grp.Members {
		if m.Kind == "employee" && strings.TrimSpace(m.Name) != "" {
			cands = append(cands, cand{id: m.ID, name: m.Name})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return len(cands[i].name) > len(cands[j].name) })
	seen := map[string]bool{}
	var ids []string
	for _, c := range cands {
		if groupMentionHit(text, c.name) && !seen[c.id] {
			seen[c.id] = true
			ids = append(ids, c.id)
		}
	}
	return ids
}

// groupMentionHit @名字 边界匹配：命中 @name 且名字后一个 rune 非汉字
// （「@张三丰」不再被子串误命中「张三」；后接空格/标点/文本末尾 = 完整名字）。
func groupMentionHit(text, name string) bool {
	needle := "@" + name
	for i := 0; ; {
		idx := strings.Index(text[i:], needle)
		if idx < 0 {
			return false
		}
		end := i + idx + len(needle)
		if end >= len(text) {
			return true // 名字到文本末尾
		}
		r, _ := utf8.DecodeRuneInString(text[end:])
		if !unicode.Is(unicode.Han, r) {
			return true // 后接非汉字（空格/标点/英文等）= 完整名字
		}
		i = end // 名字后是汉字：可能是更长名字的前缀，继续找下一处
	}
}

// groupHintNoReply 未 @ 提及时群助手节流提示（同群 60s 最多一条）。
func (a *App) groupHintNoReply(st *groupStore, grp *remote.Group) {
	if !st.shouldHint(grp.ID) {
		return
	}
	var empNames []string
	for _, m := range grp.Members {
		if m.Kind == "employee" && strings.TrimSpace(m.Name) != "" {
			empNames = append(empNames, m.Name)
		}
	}
	text := "提示：@员工名 即可获得回应；点「派任务」可让多位员工接力协作。"
	if len(empNames) > 0 {
		text = "提示：@员工名 即可获得回应（如 @" + empNames[0] + " 你好）；点「派任务」可让多位员工接力协作。"
	} else {
		text = "群内还没有数字员工：点标题栏「成员」添加员工后，@员工名 即可获得回应，或用「派任务」下发协作任务。"
	}
	msg := remote.GroupMessage{GroupID: grp.ID, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot",
		Text: text, TS: time.Now().UnixMilli()}
	st.appendMsg(msg)
	a.remoteSendGroupMessage(msg)
	a.pushGroupMsg(grp.ID)
}

// remoteGroupTask 群写任务（server.group_task 回调）：群助手协调 → 成员员工执行。
// 协作模式语义（2026-10，调研 AutoGen 后嵌入三模式，见 internal/remote/groupchain.go）：
//   - chain（默认/空）：接力——AssignedTo 为逗号分隔的员工 ID 列表 → 按序接力执行，
//     后员工输入 = 原始指令 + 前序员工产出；空 = 取第一个 employee 成员（旧行为）；
//   - parallel：并行分工——所选员工独立执行同一指令，群助手汇总；
//   - review：评审打磨——第 1 位产出、第 2 位评审修订（AutoGen reflection）。
func (a *App) remoteGroupTask(req remote.GroupServerReq) {
	st := a.ensureGroupStore()
	grp, ok := st.get(req.GroupID)
	if !ok {
		slog.Warn("群任务：群不存在", "gid", req.GroupID)
		return
	}
	mode := remote.NormalizeGroupMode(req.Mode)
	task := remote.GroupTask{
		ID:      "gt" + fmt.Sprintf("%d", time.Now().UnixMilli()),
		GroupID: grp.ID, Instruction: strings.TrimSpace(req.Instruction),
		AssignedTo: req.AssignedTo, CreatedBy: "user:remote",
		Mode:   mode,
		Status: "running", TS: time.Now().UnixMilli(),
	}
	// 派发目标：AssignedTo 逗号列表（接力）；否则第一个 employee 成员（旧行为）。
	assigned := remote.SplitAssignedIDs(req.AssignedTo)
	if len(assigned) == 0 {
		for _, m := range grp.Members {
			if m.Kind == "employee" {
				assigned = []string{m.ID}
				break
			}
		}
	}
	if len(assigned) == 0 {
		task.Status = "failed"
		task.Result = "群内没有可执行的员工成员"
		st.appendMsg(remote.GroupMessage{GroupID: grp.ID, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot", Text: "任务失败：" + task.Result, TS: time.Now().UnixMilli()})
		a.remoteSendGroupTaskDone(task)
		return
	}
	task.AssignedTo = strings.Join(assigned, ",")
	// 派发消息 + 异步执行（员工引擎在独立 goroutine 运行；接力串行/并行并发）。
	// 派发文案随模式区分（用户在群内可辨识协作方式）。
	dispatchText := "任务「" + task.Instruction + "」已派发给「" + strings.Join(assigned, "、") + "」"
	switch mode {
	case remote.GroupModeParallel:
		dispatchText += "并行分工执行（各员工独立完成后汇总）"
	case remote.GroupModeReview:
		dispatchText += "评审打磨执行（第 1 位产出、第 2 位评审修订）"
	default:
		dispatchText += "接力执行"
	}
	dispatchMsg := remote.GroupMessage{GroupID: grp.ID, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot",
		Text: dispatchText, TS: time.Now().UnixMilli()}
	st.appendMsg(dispatchMsg)
	a.remoteSendGroupMessage(dispatchMsg)
	a.pushGroupMsg(grp.ID) // 本地事件桥：派发消息实时渲染
	go func() {
		// panic 防护（对齐 runIMTaskSafe / runLarkTaskSafe）：裸 goroutine 中的 panic
		// 会击穿整个进程（「一@员工就闪退」），此处拦截转失败消息落群。
		defer func() {
			if r := recover(); r != nil {
				slog.Error("群聊任务链 panic（已拦截）", "groupId", grp.ID, "panic", r, "stack", string(debug.Stack()))
				task.Status = "failed"
				task.Result = fmt.Sprintf("任务内部异常终止（已恢复）：%v", r)
				a.finishGroupTask(st, grp.ID, task)
			}
		}()
		a.runGroupChain(st, grp.ID, task, assigned, mode)
	}()
}

// runGroupChain 多员工协作执行（群助手协调，按 mode 分流三编排）：每个员工
// 完成后即回传该员工产出（群内渐进对话），全部完成后群助手发 ✅ 汇总（含 Results），
// 任一失败发 ❌ 并注明失败员工（parallel 为部分失败聚合语义）。
func (a *App) runGroupChain(st *groupStore, gid string, task remote.GroupTask, assigned []string, mode string) {
	// 员工 ID → 展示名（接力输入「【员工名】<产出>」标注作者）。
	nameOf := func(empID string) string {
		if grp, ok := st.get(gid); ok {
			for _, m := range grp.Members {
				if m.ID == empID {
					return m.Name
				}
			}
		}
		return empID
	}
	// 单步适配：员工校验/独立会话/引擎执行；产出渐进回传群（chain/review 串行
	// 调用；parallel 由 RunParallelChain 并发调用，群消息并发写经 groupStore.mu
	// 串行落盘，无冲突）。
	step := func(ctx context.Context, empID, instruction string) (string, error) {
		t2 := task
		t2.AssignedTo = empID
		t2.Instruction = instruction
		out, err := a.runGroupEmployeeTask(gid, t2)
		if err != nil {
			return "", fmt.Errorf("员工 %s 执行失败: %w", empID, err)
		}
		// 每员工完成后即时回传该员工产出（群内渐进对话；已截断防超长）。
		prodMsg := remote.GroupMessage{GroupID: gid, Author: empID, AuthorName: nameOf(empID), AuthorKind: "employee",
			Text: "【" + nameOf(empID) + "产出】" + clipTaskResult(out, 2000), TS: time.Now().UnixMilli()}
		st.appendMsg(prodMsg)
		a.remoteSendGroupMessage(prodMsg)
		a.pushGroupMsg(gid)
		return out, nil
	}
	// 按协作模式分流编排核心（见 internal/remote/groupchain.go 三模式注释）：
	//   chain → RunEmployeeChain（按序接力）；parallel → RunParallelChain（并行分工，
	//   失败不中断他人）；review → RunReviewChain（第 1 位产出第 2 位评审）。
	runner := groupRunner(step)
	var results []remote.StepResult
	var err error
	switch mode {
	case remote.GroupModeParallel:
		results, err = remote.RunParallelChain(context.Background(), runner, assigned, task.Instruction, nameOf)
	case remote.GroupModeReview:
		results, err = remote.RunReviewChain(context.Background(), runner, assigned, task.Instruction, nameOf)
	default:
		results, err = remote.RunEmployeeChain(context.Background(), runner, assigned, task.Instruction, remote.GroupChainMaxRounds, nameOf)
	}
	task.Results = make([]remote.GroupStepResult, 0, len(results))
	for _, r := range results {
		task.Results = append(task.Results, remote.GroupStepResult{EmployeeID: r.EmployeeID, Name: r.Name, Result: r.Result})
	}
	if err != nil {
		task.Status = "failed"
		task.Result = "员工执行失败：" + err.Error()
	} else {
		task.Status = "done"
		task.Result = buildChainSummary(task, results)
	}
	a.finishGroupTask(st, gid, task)
}

// buildChainSummary 汇总多员工接力产出（群助手 ✅ 消息 + GroupTask.Result）。
func buildChainSummary(task remote.GroupTask, results []remote.StepResult) string {
	var b strings.Builder
	b.WriteString("任务「" + clipTaskResult(task.Instruction, 200) + "」完成，共 " + fmt.Sprintf("%d", len(results)) + " 位员工参与：")
	for _, r := range results {
		if r.Name == "" {
			b.WriteString("\n【" + r.EmployeeID + "】" + clipTaskResult(r.Result, 800))
		} else {
			b.WriteString("\n【" + r.Name + "】" + clipTaskResult(r.Result, 800))
		}
	}
	return b.String()
}

// groupRunner 把单步闭包适配为 remote.Runner（RunEmployeeChain 注入接口）。
type groupRunner func(ctx context.Context, employeeID, instruction string) (string, error)

func (f groupRunner) Run(ctx context.Context, employeeID, instruction string) (string, error) {
	return f(ctx, employeeID, instruction)
}

// runGroupEmployeeTask 员工执行群写任务（复用远程执行链的独立 taskRun）：
// 会话 ID = group-<gid>-<taskID>-<empID>（独立会话，员工角色注入 + 双区工作区）。
// 返回 (产出文本, error)：接力链 runGroupChain 逐员工调用并统一收尾（含 ✅/❌ 汇总）。
//
// 群聊双区语义（doc/09-设计方案 §4.3 群聊行 + §7.1 缺口①）：
//   - 会话区（sessionWS）= 员工工作空间 ws：群任务当前以员工工作空间为会话
//     工作区（工具 cwd，复用 runRemoteTask 布局）；
//   - 员工区（empWS）= 同一目录：RoleSection 内部同目录收敛为单区旧文案，
//     与改造前行为零差异（兼容存量）；员工自定义 WorkspaceRoot 后两区不同，
//     网关 dirAllowed OR 语义（workspace ∪ cwd ∪ emp.Dirs）双区同时可达；
//   - 员工直选：AssignedTo/@员工名 直接指定员工，不强制走岗位池（§4.3）；
//     岗位快照随员工当前岗位固化进会话（§2 固化语义，调岗不漂移）。
func (a *App) runGroupEmployeeTask(gid string, task remote.GroupTask) (string, error) {
	st := a.ensureGroupStore()
	// 员工校验。
	emp, ok := a.empStore.Get(task.AssignedTo)
	if !ok || !emp.Runnable(false) {
		return "", fmt.Errorf("员工不可用: %s", task.AssignedTo)
	}
	// 会话 ID = group-<gid>-<taskID>-<empID>：多位员工接力时各员工独立会话，
	// 互不覆盖（同一任务接力 n 员工 = n 个独立会话记录）。
	sessionID := "group-" + gid + "-" + task.ID + "-" + task.AssignedTo
	// 员工工作空间作为会话工作区（复用 runRemoteTask 布局）。
	ws := emp.Workspace(a.empStore.Dir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	run := &taskRun{id: sessionID, workspace: ws, employeeID: emp.ID, todo: tools.NewTodoStore(), askCh: make(chan askReply, 1)}
	run.mcpProbe = task.Instruction // MCP 惰性加载：员工任务指令作为激活信号
	a.runsMu.Lock()
	a.runs[sessionID] = run
	a.runsMu.Unlock()
	defer func() {
		a.runsMu.Lock()
		delete(a.runs, sessionID)
		a.runsMu.Unlock()
		a.registry.Remove(sessionID)
		if run.cleanup != nil {
			run.cleanup() // 惰性加载热连接统一关闭（后进先出）
		}
	}()

	a.kmu.Lock()
	needBuild := a.modeMgr == nil || a.skillMgr == nil
	a.kmu.Unlock()
	if needBuild {
		if err := a.buildKernel(); err != nil {
			return "", err
		}
	}
	// 崩溃修复 2026-10：快照必须在 buildKernel 之后取——原实现先取快照再 build，
	// 首次触发群任务时快照仍为 nil，nil skillMgr 传入 buildRunEngine →
	// availableSkillsXML → ForMode 上 sync.RWMutex 解引用 panic，且 panic 发生在
	// 裸 goroutine（go runGroupChain）中直接击穿进程（症状：一@员工就闪退）。
	// 对齐 remote_panel.go / channel_im.go 的「needBuild → buildKernel → 快照」顺序。
	a.kmu.Lock()
	modeMgr, skillMgr := a.modeMgr, a.skillMgr
	a.kmu.Unlock()
	if modeMgr == nil || skillMgr == nil {
		return "", fmt.Errorf("内核初始化失败：modeMgr/skillMgr 仍为空")
	}

	engine, mcpClients := a.buildRunEngine(ctx, modeMgr, skillMgr, run, nil)
	defer func() {
		for _, c := range mcpClients {
			if c != nil {
				c.Close()
			}
		}
	}()
	// doc/09-设计方案 §4.3 群聊行（Phase 2-③）：双区 RoleSection(empWS, sessionWS)
	// ——员工区（员工专属工作空间 ws）+ 会话区（群会话工作区 run.workspace，工具 cwd）
	// 双区声明，与渠道主路径 app.go:2316 同语义；RoleSection 内部同目录收敛单区
	//（当前两者同目录 = 存量行为零差异，员工自定义 WorkspaceRoot 后双区生效）。
	// 员工直选不走岗位池（§4.3 表注）；岗位快照随员工当前岗位固化（§2 固化语义）。
	engine.EmployeeRole = emp.RoleSection(ws, run.workspace)

	// 群会话快照（doc/09-设计方案 §2 固化语义 + §4.3）：解析出员工后固化
	// Session.EmployeeID + 岗位快照（@员工 直选语义不强制走岗位池，positionId
	// 取员工档案当前岗位；分配规则空 = random 默认 §2.1）。群会话惰性建：
	// store 无记录时仅快照字段构轻量记录落盘（消息由 say 桥留痕，回放可见）。
	if sessRef, err := a.store.Get(sessionID); err == nil && sessRef != nil {
		sessRef.SetEmployeeID(emp.ID)
		sessRef.SetPositionSnapshot(emp.PositionID, "")
		a.store.Save(sessRef)
	} else {
		now := time.Now()
		a.store.Save(&session.Session{
			ID:         sessionID,
			ChannelID:  "group",
			Title:      truncateTitle("群任务："+task.Instruction, 40),
			Mode:       "code",
			Workspace:  ws,
			EmployeeID: emp.ID,
			PositionID: emp.PositionID,
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	}

	// say 桥：群内广播（员工产出文本/工具事件 → 群消息回传 + 本地事件桥刷新）。
	say := func(m message.ClineMessage) {
		// 仅定型文本入群：Partial=true 为流式累计帧（每帧全文），入群会
		// 重复刷屏（定型帧随后照常入群）；工具/错误事件留日志。
		if m.Say != message.SayText || m.Partial || strings.TrimSpace(m.Text) == "" {
			return
		}
		msg := remote.GroupMessage{GroupID: gid, Author: emp.ID, AuthorName: emp.Name, AuthorKind: "employee",
			Text: strings.TrimSpace(m.Text), TS: time.Now().UnixMilli()}
		st.appendMsg(msg)
		a.remoteSendGroupMessage(msg)
		a.pushGroupMsg(gid) // 本地事件桥：节点自身不回收广播，需主动推前端
	}

	result, runErr := engine.RunInMode(ctx, "code", task.Instruction, false, 0, say, nil)
	if runErr != nil {
		return "", runErr
	}
	return result, nil
}

// finishGroupTask 任务收尾：完成消息入群 + agent.group_task_done 回传。
func (a *App) finishGroupTask(st *groupStore, gid string, task remote.GroupTask) {
	status := "完成"
	icon := "✅"
	if task.Status != "done" {
		status = "失败"
		icon = "❌"
	}
	st.appendMsg(remote.GroupMessage{GroupID: gid, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot",
		Text: fmt.Sprintf("%s 任务%s：「%s」", icon, status, clipTaskResult(task.Result, 800)), TS: time.Now().UnixMilli()})
	a.remoteSendGroupTaskDone(task)
	a.pushGroupMsg(gid)       // 本地事件桥：完成消息渲染（节点不回收自身广播）
	a.pushGroupTaskDone(task) // 本地事件桥：任务卡收敛
}

// clipTaskResult 截断任务结果（群消息防超长）。
func clipTaskResult(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// remoteGroupJoin 添加成员（server.group_join 回调）。
func (a *App) remoteGroupJoin(req remote.GroupServerReq) {
	st := a.ensureGroupStore()
	grp, ok := st.get(req.GroupID)
	if !ok {
		return
	}
	for _, m := range grp.Members {
		if m.ID == req.AgentID {
			return // 已存在
		}
	}
	if e, ok := a.empStore.Get(req.AgentID); ok {
		grp.Members = append(grp.Members, remote.AgentProfile{ID: req.AgentID, Name: e.Name, Kind: "employee", Avatar: e.Avatar, Description: e.Description})
		st.saveGroup(grp)
		st.appendMsg(remote.GroupMessage{GroupID: grp.ID, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot",
			Text: "「" + e.Name + "」已加入群聊", TS: time.Now().UnixMilli()})
		a.remoteSendGroups()
		a.pushGroupMsg(grp.ID) // 本地事件桥：成员加入消息实时渲染
	}
}

// remoteGroupLeave 移出成员（server.group_leave 回调）。
func (a *App) remoteGroupLeave(req remote.GroupServerReq) {
	st := a.ensureGroupStore()
	grp, ok := st.get(req.GroupID)
	if !ok {
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
	st.saveGroup(grp)
	st.appendMsg(remote.GroupMessage{GroupID: grp.ID, Author: "bot:group", AuthorName: "群助手", AuthorKind: "bot",
		Text: "「" + name + "」已移出群聊", TS: time.Now().UnixMilli()})
	a.remoteSendGroups()
	a.pushGroupMsg(grp.ID) // 本地事件桥：成员移出消息实时渲染
}

// remoteGroupSync 拉群状态（server.group_sync 回调）：回传 agent.group_state（含消息历史）。
func (a *App) remoteGroupSync(req remote.GroupServerReq) {
	st := a.ensureGroupStore()
	groups := st.all()
	var msgs []remote.GroupMessage
	if req.GroupID != "" {
		msgs = st.msgs(req.GroupID)
	} else {
		// 全量：合并全部群历史（移动端按 groupId 分流）。
		for _, grp := range groups {
			msgs = append(msgs, st.msgs(grp.ID)...)
		}
	}
	a.remoteSendGroupState(remote.GroupStatePayload{Groups: groups, Messages: msgs})
}

// remoteSendGroups 上报节点群列表全量（agent.group_state → 服务端 projection 广播）。
func (a *App) remoteSendGroups() {
	client := a.remoteClientSnapshot()
	if client != nil && client.Online() {
		client.SendGroups(a.ensureGroupStore().all())
	}
}

// remoteSendGroupState 上报群状态 + 消息历史（sync 应答）。
func (a *App) remoteSendGroupState(gsp remote.GroupStatePayload) {
	client := a.remoteClientSnapshot()
	if client != nil && client.Online() {
		client.SendGroupState(gsp)
	}
}

// remoteSendGroupMessage 上报群消息（员工产出 → 服务端广播 group.message）。
func (a *App) remoteSendGroupMessage(msg remote.GroupMessage) {
	client := a.remoteClientSnapshot()
	if client != nil && client.Online() {
		client.SendGroupMessage(msg)
	}
}

// remoteSendGroupTaskDone 上报群任务完成（agent.group_task_done）。
func (a *App) remoteSendGroupTaskDone(task remote.GroupTask) {
	client := a.remoteClientSnapshot()
	if client != nil && client.Online() {
		client.SendGroupTaskDone(task)
	}
}

// remoteNodeID 当前节点 ID（快照）。
func (a *App) remoteNodeID() string {
	a.remoteMu.Lock()
	defer a.remoteMu.Unlock()
	return a.remoteCfg.NodeID
}
