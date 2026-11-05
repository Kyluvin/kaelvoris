package remote

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsPair 本地测试双端（内存管道，模拟 websocket.Conn 接口子集）。
// 直接测 Hub 层逻辑（不走真实网络），WS 路由层由端到端冒烟覆盖。

// TestHubSubtaskProjection 子会话投影：agent.subtask 上报 → ListSessionSummaries
// 输出带 parentId 的子会话条目（与主任务会话并列，含缩进标记字段）；
// start/end 两帧幂等覆盖同一槽位；DeleteSession 联动清理。
func TestHubSubtaskProjection(t *testing.T) {
	hub := NewHub("", nil, "", nil, nil, nil)
	// start 帧（running）。
	hub.AddSubtask(SubtaskEvent{SessionID: "sess-1-s1", ParentID: "sess-1", Title: "↳ 子任务", Status: TaskStatusRunning, TS: 100})
	summaries := hub.ListSessionSummaries()
	if len(summaries) != 1 {
		t.Fatalf("subtask summary count: %d", len(summaries))
	}
	s := summaries[0]
	if s["parentId"] != "sess-1" || s["status"] != TaskStatusRunning || s["subtask"] != true {
		t.Fatalf("subtask summary wrong: %+v", s)
	}
	// end 帧（done）覆盖同一槽位。
	hub.AddSubtask(SubtaskEvent{SessionID: "sess-1-s1", ParentID: "sess-1", Title: "↳ 子任务", Status: TaskStatusDone, Result: "ok", TS: 200})
	summaries = hub.ListSessionSummaries()
	if len(summaries) != 1 || summaries[0]["status"] != TaskStatusDone {
		t.Fatalf("subtask end overwrite failed: %+v", summaries)
	}
	// DeleteSession 父会话 → 子会话投影联动清除。
	hub.DeleteSession("sess-1")
	if len(hub.ListSessionSummaries()) != 0 {
		t.Fatalf("subtask projection should be cleared: %+v", hub.ListSessionSummaries())
	}
}

// TestHubAddTaskEventPartialMirror 服务端会话镜像 partial 过滤：流式 partial 帧
// 不落镜像（定型帧到达后覆盖），get.messages 数据源不含截断文本（复盘缺陷 1
// 「remote 会话 partial 未落盘完整文本」的服务端镜像侧兑底）。
func TestHubAddTaskEventPartialMirror(t *testing.T) {
	hub := NewHub("", nil, "", nil, nil, nil)
	// 流式 partial 帧：不得写入镜像。
	hub.AddTaskEvent(TaskEvent{SessionID: "remote-s-x", Kind: "text", Text: "流式片段...", Partial: true})
	if n := len(hub.SessionMessages("remote-s-x")); n != 0 {
		t.Fatalf("partial 帧不应落镜像: %d", n)
	}
	// 定型帧：写入镜像。
	hub.AddTaskEvent(TaskEvent{SessionID: "remote-s-x", Kind: "text", Text: "完整文本", Partial: false})
	msgs := hub.SessionMessages("remote-s-x")
	if len(msgs) != 1 {
		t.Fatalf("定型帧应落镜像: %d", len(msgs))
	}
	var m map[string]interface{}
	if err := json.Unmarshal(msgs[0], &m); err != nil || m["text"] != "完整文本" {
		t.Fatalf("镜像内容: %v %v", err, m)
	}
	// 镜像不含 partial 痕迹（前端回放不会读到截断文本）。
	if _, has := m["partial"]; has {
		t.Fatalf("镜像不应带 partial 标记: %+v", m)
	}
}

func TestHubTaskLifecycle(t *testing.T) {
	hub := NewHub("", nil, "", nil, nil,
		[]Account{{User: "node-user", Pass: "pw-1", Role: "user", DisplayName: "NU"}})

	// 1. 账号认证（去令牌化：注册鉴权 = 统一账号表）。
	if _, ok := hub.AuthAgentForRegister("node-user", "pw-1"); !ok {
		t.Fatal("agent auth with valid account should pass")
	}
	if _, ok := hub.AuthAgentForRegister("node-user", "bad"); ok {
		t.Fatal("bad password should fail")
	}
	if _, ok := hub.AuthAgentForRegister("", ""); !ok {
		t.Fatal("anonymous register should pass (read-only online)")
	}

	// 2. 无节点时创建任务报错。
	if _, err := hub.CreateTask("", "任务A", "", "mobile:user"); err == nil {
		t.Fatal("create task without nodes should fail")
	}

	// 3. 注册节点（注入模拟连接）。
	conn := &agentConn{nodeID: "node-a", ws: nil}
	hub.RegisterAgent(conn, AgentRegister{Name: "测试机", Platform: "windows", Version: "0.6.0"})
	nodes := hub.ListNodes()
	if len(nodes) != 1 || !nodes[0].Online || nodes[0].Name != "测试机" {
		t.Fatalf("nodes after register: %+v", nodes)
	}

	// 4. 创建任务（dispatch 会因 ws=nil 写失败 → 回退 pending，属可接受分支）。
	task, err := hub.CreateTask("", "任务B", "architect", "admin:admin")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.SessionID == "" || task.Status == "" || task.Mode != "architect" {
		t.Fatalf("task fields: %+v", task)
	}

	// 5. 事件与终态。
	hub.AddTaskEvent(TaskEvent{TaskID: task.ID, SessionID: task.SessionID, Kind: "text", Text: "进行中"})
	if evs := hub.TaskEvents(task.ID); len(evs) != 1 || evs[0].Kind != "text" {
		t.Fatalf("events: %+v", evs)
	}

	// 5b. 事件广播携带归属节点（PC 端执行端过滤他机任务的依据）。
	if evs := hub.TaskEvents(task.ID); evs[0].NodeID != task.NodeID || task.NodeID == "" {
		t.Fatalf("event nodeId not filled: %+v (task node %q)", evs[0], task.NodeID)
	}
	hub.TaskDone(TaskDone{TaskID: task.ID, OK: true, Result: "搞定"})
	got, ok := hub.GetTask(task.ID)
	if !ok || got.Status != TaskStatusDone || got.Result != "搞定" {
		t.Fatalf("task after done: %+v", got)
	}

	// 6. 会话反查。
	sid, ok := hub.SessionTaskID(task.SessionID)
	if !ok || sid != task.ID {
		t.Fatalf("session task: %s %v", sid, ok)
	}

	// 7. 节点下线。
	hub.UnregisterAgent("node-a", conn)
	if hub.NodeOnline("node-a") {
		t.Fatal("node should be offline")
	}
}

func TestHubClientAuth(t *testing.T) {
	hub := NewHub("", nil, "share-token",
		map[string]string{"admin": "pw1"}, map[string]string{"user": "pw2"}, nil)

	// 管理端。
	role, user, ok := hub.AuthClient("admin", "pw1", "")
	if !ok || role != "admin" || user != "admin" {
		t.Fatalf("admin auth: %v %s %s", ok, role, user)
	}
	// 移动端。
	role, _, ok = hub.AuthClient("user", "pw2", "")
	if !ok || role != "client" {
		t.Fatalf("mobile auth: %v %s", ok, role)
	}
	// 共享 token。
	role, _, ok = hub.AuthClient("", "", "share-token")
	if !ok || role != "client" {
		t.Fatalf("token auth: %v %s", ok, role)
	}
	// 节点 token 不能用于客户端（未注册到 clientToken）。
	if _, _, ok := hub.AuthClient("", "", "agent-token-x"); ok {
		t.Fatal("random token should fail")
	}
	// 错误密码。
	if _, _, ok := hub.AuthClient("admin", "wrong", ""); ok {
		t.Fatal("wrong password should fail")
	}

	// P-Account 统一账号互通：同一账号在两端登录均认可。
	if role, u, ok := hub.AuthClient("admin", "pw1", ""); !ok || u != "admin" || role != "admin" {
		t.Fatalf("unified admin auth: %v %s %s", ok, role, u)
	}
	// 空密码不允许登录。
	if _, _, ok := hub.AuthClient("admin", "", ""); ok {
		t.Fatal("empty pass should fail")
	}
}

func TestHubUnifiedAccounts(t *testing.T) {
	// users 优先于旧双表；同账号 users 提供完整信息。
	users := []Account{
		{User: "alice", Pass: "p1", Role: "admin", DisplayName: "爱丽丝"},
		{User: "bob", Pass: "p2", Role: "user"},
	}
	hub := NewHub("", nil, "", map[string]string{"legacy-admin": "x"}, map[string]string{"legacy-user": "y"}, users)
	// 新表账号。
	if _, u, ok := hub.AuthClient("alice", "p1", ""); !ok || u != "alice" {
		t.Fatal("alice should auth")
	}
	if _, u, ok := hub.AuthClient("bob", "p2", ""); !ok || u != "bob" {
		t.Fatal("bob should auth")
	}
	// 旧双表并入。
	if role, u, ok := hub.AuthClient("legacy-admin", "x", ""); !ok || role != "admin" || u != "legacy-admin" {
		t.Fatalf("legacy admin: %v %s %s", ok, role, u)
	}
	if role, _, ok := hub.AuthClient("legacy-user", "y", ""); !ok || role != "client" {
		t.Fatalf("legacy user: %v %s", ok, role)
	}
	// 账号列表（脱敏）。
	accs := hub.Accounts()
	found := 0
	for _, a := range accs {
		if a.Pass != "" {
			t.Fatalf("accounts must be sanitized: %+v", a)
		}
		if a.User == "alice" && a.Role == "admin" && a.DisplayName == "爱丽丝" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("alice account missing: %+v", accs)
	}
}

// TestHubChangePassword 修改密码（account.passwd 服务端语义）：
// 自己改密验旧密码 / 旧密码错拒 / admin 可重置他人 / 普通用户不可改他人 /
// 新密码长度校验 / 改后即用新密码登录 / 变更回调触发。
func TestHubChangePassword(t *testing.T) {
	users := []Account{
		{User: "admin", Pass: "pw1", Role: "admin"},
		{User: "alice", Pass: "pa", Role: "user"},
	}
	hub := NewHub("", nil, "", nil, nil, users)
	changed := make(chan string, 4)
	hub.OnUsersChanged(func() { changed <- "users" })

	// 自己改密：旧密码错误拒。
	if _, err := hub.ChangePassword("alice", "client", "", "wrong", "newpass"); err == nil {
		t.Fatal("wrong old pass should fail")
	}
	// 自己改密：旧密码为空拒。
	if _, err := hub.ChangePassword("alice", "client", "", "", "newpass"); err == nil {
		t.Fatal("empty old pass should fail")
	}
	// 新密码过短。
	if _, err := hub.ChangePassword("alice", "client", "", "pa", "abc"); err == nil {
		t.Fatal("too short new pass should fail")
	}
	// 普通用户不可改他人。
	if _, err := hub.ChangePassword("alice", "client", "admin", "", "hacked"); err == nil {
		t.Fatal("non-admin changing others should fail")
	}
	// 目标账号不存在。
	if _, err := hub.ChangePassword("admin", "admin", "ghost", "", "newpass"); err == nil {
		t.Fatal("missing target should fail")
	}
	// 自己改密成功。
	if u, err := hub.ChangePassword("alice", "client", "", "pa", "pa22"); err != nil || u != "alice" {
		t.Fatalf("self change: %v %s", err, u)
	}
	// 改后即用新密码登录，旧密码失效。
	if _, _, ok := hub.AuthClient("alice", "pa", ""); ok {
		t.Fatal("old pass should be rejected after change")
	}
	if _, u, ok := hub.AuthClient("alice", "pa22", ""); !ok || u != "alice" {
		t.Fatal("new pass should auth")
	}
	// admin 重置他人（无需旧密码）。
	if u, err := hub.ChangePassword("admin", "admin", "alice", "", "reset1"); err != nil || u != "alice" {
		t.Fatalf("admin reset: %v %s", err, u)
	}
	if _, _, ok := hub.AuthClient("alice", "reset1", ""); !ok {
		t.Fatal("reset pass should auth")
	}
	// 变更回调已触发（异步，最多等 2s）。
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("OnUsersChanged not fired")
	}
}

func TestHubNodeMode(t *testing.T) {
	hub := NewHub("", map[string]string{"tok": "n1"}, "", nil, nil, nil)
	conn := &agentConn{nodeID: "n1", ws: nil}
	hub.RegisterAgent(conn, AgentRegister{Name: "PC", Mode: "code", Modes: []ModeInfo{{Slug: "code", Name: "Code"}}})
	if got := hub.NodeMode("n1"); got != "code" {
		t.Fatalf("mode: %s", got)
	}
	hub.SetNodeMode("n1", "architect")
	if got := hub.NodeMode("n1"); got != "architect" {
		t.Fatalf("mode after set: %s", got)
	}
	nodes := hub.ListNodes()
	if len(nodes) != 1 || nodes[0].Mode != "architect" || len(nodes[0].Modes) != 1 {
		t.Fatalf("nodes: %+v", nodes)
	}
}

func TestHubSessionMirror(t *testing.T) {
	hub := NewHub("", nil, "", nil, nil, nil)
	msgs := []interface{}{
		map[string]interface{}{"say": "text", "text": "你好"},
		map[string]interface{}{"say": "tool", "text": `{"tool":"fs_read"}`},
	}
	hub.SaveSessionMessages("sess-1", msgs)
	got := hub.SessionMessages("sess-1")
	if len(got) != 2 {
		t.Fatalf("session messages: %d", len(got))
	}
	var m map[string]interface{}
	if err := json.Unmarshal(got[0], &m); err != nil || m["text"] != "你好" {
		t.Fatalf("message roundtrip: %v %v", err, m)
	}

	// 摘要列表。
	summaries := hub.ListSessionSummaries()
	if len(summaries) != 0 {
		// 无任务时无会话摘要。
		t.Fatalf("summaries should be empty without tasks: %+v", summaries)
	}
}

// TestHubDeleteTask 删除任务记录：终态可删 + 运行中拒绝 + 镜像/事件/Todo 同步清理 +
// 重启不复活（tasks.json 已除名）。
func TestHubDeleteTask(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, map[string]string{"tok-1": "node-a"}, "", nil, nil, nil)
	conn := &agentConn{nodeID: "node-a", ws: nil}
	hub.RegisterAgent(conn, AgentRegister{Name: "测试机"})

	task, err := hub.CreateTask("", "待删除任务", "", "mobile:user")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	hub.AddTaskEvent(TaskEvent{TaskID: task.ID, SessionID: task.SessionID, Kind: "text", Text: "进度"})
	hub.SaveSessionMessages(task.SessionID, []interface{}{map[string]interface{}{"say": "text", "text": "历史"}})
	hub.SaveSessionTodos(task.SessionID, []interface{}{map[string]interface{}{"status": "completed", "content": "步骤一"}})
	hub.TaskDone(TaskDone{TaskID: task.ID, OK: true, Result: "完成"})

	// 运行中/待执行拒绝删除。
	t2, err := hub.CreateTask("", "运行中任务", "", "mobile:user")
	if err != nil {
		t.Fatalf("create running: %v", err)
	}
	if _, _, err := hub.DeleteTask(t2.ID); err == nil {
		t.Fatal("删除运行中任务应被拒绝")
	}
	if _, ok := hub.GetTask(t2.ID); !ok {
		t.Fatal("运行中任务不应被删")
	}

	// 终态删除：任务 + 事件 + 会话镜像 + Todo 镜像全部清除。
	if _, _, err := hub.DeleteTask(task.ID); err != nil {
		t.Fatalf("delete done task: %v", err)
	}
	if _, ok := hub.GetTask(task.ID); ok {
		t.Fatal("任务删除后仍可查到")
	}
	if evs := hub.TaskEvents(task.ID); len(evs) != 0 {
		t.Fatalf("事件未清: %+v", evs)
	}
	if msgs := hub.SessionMessages(task.SessionID); len(msgs) != 0 {
		t.Fatalf("会话镜像未清: %d", len(msgs))
	}
	if todos := hub.SessionTodos(task.SessionID); len(todos) != 0 {
		t.Fatalf("Todo 镜像未清: %d", len(todos))
	}
	for _, s := range hub.ListSessionSummaries() {
		if s["sessionID"] == task.SessionID {
			t.Fatal("会话列表仍包含已删会话")
		}
	}

	// 不存在的任务报错。
	if _, _, err := hub.DeleteTask("t-none"); err == nil {
		t.Fatal("删除不存在任务应报错")
	}

	// 异步持久化：等落盘完成后重启，避免 TempDir 清理竞争（下同）。
	hub.FlushPersistence()
	hub.Stop()

	// 重启（同 dir 新 hub）：tasks.json 已除名，删除不可逆不复活。
	hub2 := NewHub(dir, map[string]string{"tok-1": "node-a"}, "", nil, nil, nil)
	defer hub2.Stop() // 防落盘与 TempDir 清理竞态（Windows unlinkat flake）
	if _, ok := hub2.GetTask(task.ID); ok {
		t.Fatal("重启后已删任务不应复活")
	}
	if msgs := hub2.SessionMessages(task.SessionID); len(msgs) != 0 {
		t.Fatalf("重启后镜像未清: %d", len(msgs))
	}
}

// TestHubSessionTodosMirror 会话 Todo 镜像：保存/拉取/空清单保护/重启恢复。
func TestHubSessionTodosMirror(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, nil, "", nil, nil, nil)

	todos := []interface{}{
		map[string]interface{}{"status": "completed", "content": "安装依赖"},
		map[string]interface{}{"status": "in_progress", "content": "实现功能"},
		map[string]interface{}{"status": "pending", "content": "验证测试"},
	}
	hub.SaveSessionTodos("sess-t1", todos)
	got := hub.SessionTodos("sess-t1")
	if len(got) != 3 {
		t.Fatalf("todos count: %d", len(got))
	}
	var td map[string]interface{}
	if err := json.Unmarshal(got[1], &td); err != nil || td["content"] != "实现功能" {
		t.Fatalf("todo roundtrip: %v %v", err, td)
	}

	// 空清单不覆盖旧值（节点未开启 todo 回传时避免误清已有进度）。
	hub.SaveSessionTodos("sess-t1", nil)
	if got := hub.SessionTodos("sess-t1"); len(got) != 3 {
		t.Fatalf("empty todos must not overwrite: %d", len(got))
	}

	// 异步持久化：先确保落盘完成再模拟重启。
	hub.FlushPersistence()
	hub.Stop()

	// 重启恢复（同 dir 新 hub）。
	hub2 := NewHub(dir, nil, "", nil, nil, nil)
	defer hub2.Stop()
	if got := hub2.SessionTodos("sess-t1"); len(got) != 3 {
		t.Fatalf("todos after restart: %d", len(got))
	}

	// 未保存会话返回空（不 panic）。
	if got := hub2.SessionTodos("sess-none"); len(got) != 0 {
		t.Fatalf("missing session should be empty: %d", len(got))
	}
}

// TestHubTaskRecoveryAfterRestart 服务重启后未完成任务自动恢复闭环：
//  1. 落盘 hub 写入 pending/running/done/canceled 混合任务（tasks.json）与节点（nodes.json）；
//  2. 新 hub（同 dir）加载：running/pending → 重置 pending 并重建暂存队列，终态不变；
//  3. 节点上线 RegisterAgent → 暂存任务自动补发（模拟连接写成功即置 running）；
//  4. 节点回终态 TaskDone → done/failed 落盘；重启恢复不再标“任务状态丢失”。
func TestHubTaskRecoveryAfterRestart(t *testing.T) {
	dir := t.TempDir()

	// 第一生命周期：写入持久化数据（模拟服务重启前状态）。
	first := NewHub(dir, map[string]string{"tok-r": "node-r"}, "", nil, nil, nil)
	first.saveNodes() // 无节点也落一份（load 需容忍空文件）
	now := time.Now().UnixMilli()
	tasks := []*Task{
		{ID: "t-done", SessionID: "remote-s1", NodeID: "node-r", Text: "已完成任务", Status: TaskStatusDone, Result: "OK", CreatedAt: now - 4000, FinishedAt: now - 1000},
		{ID: "t-cancel", SessionID: "remote-s2", NodeID: "node-r", Text: "已取消任务", Status: TaskStatusCanceled, CreatedAt: now - 3000, FinishedAt: now - 500},
		{ID: "t-run", SessionID: "remote-s3", NodeID: "node-r", Text: "重启时运行中", Status: TaskStatusRunning, CreatedAt: now - 2000, StartedAt: now - 1500},
		{ID: "t-pend", SessionID: "remote-s4", NodeID: "node-r", Text: "重启时待派发", Status: TaskStatusPending, CreatedAt: now - 1000},
		{ID: "t-pend2", SessionID: "remote-s5", NodeID: "node-r", Text: "待派发任务二", Status: TaskStatusPending, CreatedAt: now - 500},
	}
	// 异步持久化：先停掉落盘者，避免 persistLoop 用内存态覆盖下方手工 fixture。
	first.FlushPersistence()
	first.Stop()
	b, _ := json.MarshalIndent(tasks, "", "  ")
	if err := os.WriteFile(first.tasksFile(), b, 0o644); err != nil {
		t.Fatalf("写 tasks.json: %v", err)
	}
	nodes := persistedNodes{Nodes: map[string]Node{"node-r": {ID: "node-r", Name: "恢复节点", Online: true, CurrentTask: "t-run"}}}
	b, _ = json.MarshalIndent(nodes, "", "  ")
	if err := os.WriteFile(first.nodesFile(), b, 0o644); err != nil {
		t.Fatalf("写 nodes.json: %v", err)
	}

	// 第二生命周期：重启加载。
	hub := NewHub(dir, map[string]string{"tok-r": "node-r"}, "", nil, nil, nil)
	defer hub.Stop() // 停 persistLoop：防测试退出后落盘者与 TempDir 清理竞态（Windows unlinkat flake）
	// 断言 1：终态任务保持不变。
	if got, ok := hub.GetTask("t-done"); !ok || got.Status != TaskStatusDone || got.Result != "OK" {
		t.Fatalf("终态任务应保持 done: %+v", got)
	}
	if got, ok := hub.GetTask("t-cancel"); !ok || got.Status != TaskStatusCanceled {
		t.Fatalf("取消任务应保持 canceled: %+v", got)
	}
	// 断言 2：running/pending 重置为 pending（不再标失败），执行时间戳清零。
	for _, id := range []string{"t-run", "t-pend", "t-pend2"} {
		got, ok := hub.GetTask(id)
		if !ok {
			t.Fatalf("未完成任务 %s 应恢复", id)
		}
		if got.Status != TaskStatusPending {
			t.Fatalf("%s 应重置为 pending（旧行为是 failed）: %+v", id, got)
		}
		if got.Error != "" || got.StartedAt != 0 || got.FinishedAt != 0 {
			t.Fatalf("%s 恢复后应清执行痕衷: %+v", id, got)
		}
	}
	// 断言 3：节点落盘态清理（Online=false / CurrentTask=""）。
	for _, n := range hub.ListNodes() {
		if n.Online || n.CurrentTask != "" {
			t.Fatalf("落盘节点应重置离线: %+v", n)
		}
	}

	// 断言 4：节点上线 → 暂存任务自动补发（写成功 → 置 running）。
	// 真实 writeJSON 需要可用 ws：用环回 WS 对验证补发链路（gorilla 真实协议）。
	srvWS, peerWS := wsLoopbackPair(t)
	defer peerWS.Close()
	conn := &agentConn{nodeID: "node-r", ws: srvWS}
	hub.RegisterAgent(conn, AgentRegister{Name: "恢复节点"})
	// 补发为异步 goroutine，轮询等待任务转为 running。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := hub.GetTask("t-run"); got != nil && got.Status == TaskStatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, _ := hub.GetTask("t-run"); got == nil || got.Status != TaskStatusRunning {
		t.Fatalf("节点上线后 t-run 应补发置 running: %+v", got)
	}
	if got, _ := hub.GetTask("t-pend"); got == nil || got.Status != TaskStatusRunning {
		t.Fatalf("节点上线后 t-pend 应补发置 running: %+v", got)
	}
	if got, _ := hub.GetTask("t-pend2"); got == nil || got.Status != TaskStatusRunning {
		t.Fatalf("节点上线后 t-pend2 应补发置 running: %+v", got)
	}

	// 断言 5：终态回传落盘 + 恢复后不再出现旧错误文案。
	hub.TaskDone(TaskDone{TaskID: "t-run", OK: true, Result: "续跑完成"})
	got, _ := hub.GetTask("t-run")
	if got.Status != TaskStatusDone || got.Result != "续跑完成" {
		t.Fatalf("恢复任务终态: %+v", got)
	}
	raw, _ := os.ReadFile(hub.tasksFile())
	if strings.Contains(string(raw), "服务端重启，任务状态丢失") {
		t.Fatal("tasks.json 不应再出现旧失败文案（恢复语义生效）")
	}
	// 终态任务不被重派：直接调 dispatchTask 验证终态保护
	//（dispatchTask 自带锁管理，无需外部持锁）。
	if hub.dispatchTask(hub.tasks["t-run"]) { // done 任务应被跳过
		t.Fatal("终态任务 dispatchTask 应回 false（跳过）")
	}
	if got := hub.tasks["t-run"]; got.Status != TaskStatusDone {
		t.Fatalf("终态任务不应被重派: %+v", got)
	}
}

// wsLoopbackPair 环回 WS 真实连接对（服务端侧连接 + 可丢弃读的对端）。
// 补发链路测试用：agentConn.ws 需真实 *websocket.Conn（writeJSON 才会成功），
// 对端仅接收派发帧、不回消息（由测试直接调 hub 方法模拟终态）。
func wsLoopbackPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	var up = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	ready := make(chan *websocket.Conn, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/__test_ws", func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ready <- ws
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	ws, _, err := websocket.DefaultDialer.Dial("ws://"+l.Addr().String()+"/__test_ws", nil)
	if err != nil {
		t.Fatalf("dial test ws: %v", err)
	}
	server := <-ready
	return server, ws
}

func TestWSURLFromHTTPHelper(t *testing.T) {
	// wsURLFromHTTP 在 main 包，此处验证 NewNodeID 唯一性已由 protocol_test 覆盖。
	_ = websocket.TextMessage // 保持依赖引用（编译期确认 gorilla 可用）。
}

// seedTasks 注入多状态/多节点任务集（直接操作内存态，不依赖连接）。
func seedTasks(t *testing.T, h *Hub) map[string]*Task {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	mk := func(id, node, status, text, by string, at int64) *Task {
		return &Task{ID: id, SessionID: "s-" + id, NodeID: node, Status: status, Text: text, CreatedBy: by, CreatedAt: at}
	}
	ts := map[string]*Task{
		"t-a": mk("t-a", "node-1", TaskStatusDone, "重构登录模块", "mobile:alice", 1000),
		"t-b": mk("t-b", "node-1", TaskStatusFailed, "修复登录 BUG", "admin:admin", 2000),
		"t-c": mk("t-c", "node-2", TaskStatusRunning, "部署新版本", "mobile:alice", 3000),
		"t-d": mk("t-d", "node-2", TaskStatusCanceled, "文档整理", "admin:admin", 4000),
	}
	for id, task := range ts {
		h.tasks[id] = task
	}
	return ts
}

// TestTaskFilterMatches 过滤条件 AND 语义与白名单校验。
func TestTaskFilterMatches(t *testing.T) {
	mk := func(node, status, text, by string, at int64) *Task {
		return &Task{ID: "x", NodeID: node, Status: status, Text: text, CreatedBy: by, CreatedAt: at}
	}
	cases := []struct {
		name   string
		filter TaskFilter
		task   *Task
		want   bool
	}{
		{"空过滤命中", TaskFilter{}, mk("n1", "done", "任意", "u", 1), true},
		{"节点不匹配", TaskFilter{NodeID: "n1"}, mk("n2", "done", "任意", "u", 1), false},
		{"状态不匹配", TaskFilter{Status: "done"}, mk("n1", "failed", "任意", "u", 1), false},
		{"search 命中文本", TaskFilter{Search: "登录"}, mk("n1", "done", "重构登录模块", "u", 1), true},
		{"search 命中发布者（大小写不敏感）", TaskFilter{Search: "ALICE"}, mk("n1", "done", "x", "mobile:alice", 1), true},
		{"search 不命中", TaskFilter{Search: "不存在"}, mk("n1", "done", "任意", "u", 1), false},
		{"since 边界（含等值）", TaskFilter{Since: 1000}, mk("n1", "done", "任意", "u", 1000), true},
		{"since 早于创建时间", TaskFilter{Since: 2000}, mk("n1", "done", "任意", "u", 1000), false},
		{"组合条件全命中", TaskFilter{NodeID: "n1", Status: "done", Search: "登录"}, mk("n1", "done", "重构登录模块", "u", 1), true},
		{"组合条件一票否决", TaskFilter{NodeID: "n1", Status: "done", Search: "登录"}, mk("n1", "running", "重构登录模块", "u", 1), false},
	}
	for _, c := range cases {
		if got := c.filter.Matches(c.task); got != c.want {
			t.Errorf("%s: Matches=%v want %v", c.name, got, c.want)
		}
	}
	// nil filter = 恒命中（防御式）。
	var nilFilter *TaskFilter
	if !nilFilter.Matches(mk("n1", "done", "x", "u", 1)) {
		t.Fatal("nil filter should match all")
	}
	// Validate：非法状态报错，合法状态通过；Limit 夹取。
	if err := (&TaskFilter{Status: "bogus"}).Validate(); err == nil {
		t.Fatal("bogus status should fail validation")
	}
	ok := &TaskFilter{Status: TaskStatusRunning}
	if err := ok.Validate(); err != nil || ok.Limit != 100 {
		t.Fatalf("default limit: %v %d", err, ok.Limit)
	}
	big := &TaskFilter{Limit: 9999}
	_ = big.Validate()
	if big.Limit != 1000 {
		t.Fatalf("limit clamp: %d", big.Limit)
	}
}

// TestHubQueryTasks 服务端过滤查询：条件组合、limit 截断、非法状态返回空。
func TestHubQueryTasks(t *testing.T) {
	hub := NewHub("", nil, "", nil, nil, nil)
	seedTasks(t, hub)

	// 全量（nil filter = 旧行为）。
	if got := hub.QueryTasks(nil); len(got) != 4 {
		t.Fatalf("nil filter count: %d", len(got))
	}
	// 按状态。
	if got := hub.QueryTasks(&TaskFilter{Status: TaskStatusDone}); len(got) != 1 || got[0].ID != "t-a" {
		t.Fatalf("status=done: %+v", got)
	}
	// 按节点。
	if got := hub.QueryTasks(&TaskFilter{NodeID: "node-1"}); len(got) != 2 {
		t.Fatalf("nodeId=node-1: %d", len(got))
	}
	// 组合 + limit（node-1 共 2 条，limit=1 取最新）。
	got := hub.QueryTasks(&TaskFilter{NodeID: "node-1", Limit: 1})
	if len(got) != 1 || got[0].ID != "t-b" { // t-b CreatedAt=2000 较新
		t.Fatalf("node+limit: %+v", got)
	}
	// search + since。
	if got := hub.QueryTasks(&TaskFilter{Search: "alice", Since: 2500}); len(got) != 1 || got[0].ID != "t-c" {
		t.Fatalf("search+since: %+v", got)
	}
	// 非法状态：空结果（REST 路径已提前 400；Hub 层防御为空集）。
	if got := hub.QueryTasks(&TaskFilter{Status: "bogus"}); len(got) != 0 {
		t.Fatalf("bogus status should be empty: %d", len(got))
	}
	// 结果排序仍为最新在前（QueryTasks 不改变 taskList 顺序语义）。
	all := hub.QueryTasks(&TaskFilter{})
	for i := 1; i < len(all); i++ {
		if all[i-1].CreatedAt < all[i].CreatedAt {
			t.Fatalf("order not desc: %+v", all)
		}
	}
}

// TestHubTaskDoneTerminalProtection 终态保护：canceled 任务不被迟到的正常终态覆盖
// （修复：手机端 ABORT 后 PC 端迟到 task_done 把卡片又改回 done/failed，观感
// 即「取消/状态切换不管用」）；done 任务也不被后续失败回报回退。
func TestHubTaskDoneTerminalProtection(t *testing.T) {
	hub := NewHub("", map[string]string{"tok": "node-a"}, "", nil, nil, nil)
	conn := &agentConn{nodeID: "node-a", ws: nil}
	hub.RegisterAgent(conn, AgentRegister{Name: "终端保护机"})
	task, err := hub.CreateTask("", "取消保护任务", "", "mobile:user")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 取消 → canceled；迟到 done 回报不得覆盖。
	hub.CancelTask(task.ID, task.SessionID)
	if got, _ := hub.GetTask(task.ID); got.Status != TaskStatusCanceled {
		t.Fatalf("取消后应为 canceled: %+v", got)
	}
	hub.TaskDone(TaskDone{TaskID: task.ID, OK: true, Result: "迟到的成功"})
	got, _ := hub.GetTask(task.ID)
	if got.Status != TaskStatusCanceled {
		t.Fatalf("迟到 done 不应覆盖 canceled: %+v", got)
	}
	hub.TaskDone(TaskDone{TaskID: task.ID, OK: false, Error: "迟到的失败"})
	if got, _ := hub.GetTask(task.ID); got.Status != TaskStatusCanceled {
		t.Fatalf("迟到 failed 也不应覆盖 canceled: %+v", got)
	}

	// 取消后 CurrentTask 应被清空（节点页「● EXECUTING」徽标收敛）。
	ns := hub.ListNodes()
	for _, n := range ns {
		if n.ID == "node-a" && n.CurrentTask != "" {
			t.Fatalf("取消后节点 CurrentTask 应清空: %+v", n)
		}
	}

	// 正常链路回归：running → done 仍生效（保护不误伤）。
	t2, err := hub.CreateTask("", "正常完成", "", "mobile:user")
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	hub.TaskDone(TaskDone{TaskID: t2.ID, OK: true, Result: "OK"})
	if got, _ := hub.GetTask(t2.ID); got.Status != TaskStatusDone {
		t.Fatalf("running→done 应正常: %+v", got)
	}
}

// TestHubReapStaleRunningOnReconnect 节点重连时收敛断连期间遗留的 running 僵尸任务
// （修复：节点断连期间终态回传丢失，任务永久卡「运行中」，startedAt 全部同值即此场景）。
func TestHubReapStaleRunningOnReconnect(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, map[string]string{"tok": "node-a"}, "", nil, nil, nil)
	now := time.Now().UnixMilli()
	// 预置：一条 running 僵尸 + 一条 pending 暂存（不应被收敛）。
	hub.mu.Lock()
	hub.tasks["t-zombie"] = &Task{ID: "t-zombie", SessionID: "remote-sz", NodeID: "node-a", Text: "僵尸", Status: TaskStatusRunning, CreatedAt: now - 1000, StartedAt: now - 900}
	hub.tasks["t-pend"] = &Task{ID: "t-pend", SessionID: "remote-sp", NodeID: "node-a", Text: "待派发", Status: TaskStatusPending, CreatedAt: now}
	ns := &nodeState{info: Node{ID: "node-a", Name: "断连节点"}}
	ns.pending = append(ns.pending, hub.tasks["t-pend"])
	hub.nodes["node-a"] = ns
	hub.mu.Unlock()

	// 节点重连：僵尸收敛，pending 保留待补发。
	conn := &agentConn{nodeID: "node-a", ws: nil}
	hub.RegisterAgent(conn, AgentRegister{Name: "断连节点"})
	got, ok := hub.GetTask("t-zombie")
	if !ok || got.Status != TaskStatusFailed {
		t.Fatalf("僵尸 running 应收敛为 failed: %+v ok=%v", got, ok)
	}
	if got.Error == "" || got.FinishedAt == 0 {
		t.Fatalf("收敛任务应带说明文案与终态时间: %+v", got)
	}
	if got, _ := hub.GetTask("t-pend"); got.Status != TaskStatusPending {
		t.Fatalf("暂存任务不应被收敛（应保持 pending 待补发）: %+v", got)
	}
	// 终态落盘可查（重启后不再复活）。
	hub.FlushPersistence()
	raw, _ := os.ReadFile(hub.tasksFile())
	if !strings.Contains(string(raw), "状态暂标失败") {
		t.Fatal("收敛结果应落盘 tasks.json")
	}
	hub.Stop()
}

// TestHubWatchdogSweepDisconnected 节点失联巡检：断连超宽限期的节点，
// 其 running 任务自动收敛 failed（修复：节点失联后任务卡永久滞留「运行中」，
// 此前收敛仅挂在节点重连时机，节点不回来就永远卡死）。
func TestHubWatchdogSweepDisconnected(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, map[string]string{"tok": "node-a"}, "", nil, nil, nil)
	now := time.Now().UnixMilli()
	// 预置：失联节点（断连且 LastSeen 超 10 分钟）+ 在线节点（宽限期内）
	// + 空卡节点（从未上线 LastSeen=0，不应误伤）。
	hub.mu.Lock()
	hub.tasks["t-lost"] = &Task{ID: "t-lost", SessionID: "remote-sl", NodeID: "node-lost", Text: "失联任务", Status: TaskStatusRunning, CreatedAt: now - 600000, StartedAt: now - 590000}
	hub.tasks["t-done"] = &Task{ID: "t-done", SessionID: "remote-sd", NodeID: "node-lost", Text: "已完成", Status: TaskStatusDone, CreatedAt: now - 2000, FinishedAt: now - 1000}
	hub.tasks["t-fresh"] = &Task{ID: "t-fresh", SessionID: "remote-sf", NodeID: "node-fresh", Text: "宽限期内", Status: TaskStatusRunning, CreatedAt: now - 1000, StartedAt: now - 900}
	nsLost := &nodeState{info: Node{ID: "node-lost", LastSeen: now - 10*60*1000}} // 断连 10 分钟
	nsFresh := &nodeState{info: Node{ID: "node-fresh", LastSeen: now - 10*1000}}
	nsFresh.conn = &agentConn{nodeID: "node-fresh", ws: nil} // 在线
	hub.nodes["node-lost"] = nsLost
	hub.nodes["node-fresh"] = nsFresh
	hub.nodes["node-ghost"] = &nodeState{info: Node{ID: "node-ghost"}}
	hub.mu.Unlock()

	if n := hub.sweepDisconnected(150 * time.Second); n != 1 {
		t.Fatalf("应恰好收敛 1 个失联节点，got %d", n)
	}
	if got, _ := hub.GetTask("t-lost"); got.Status != TaskStatusFailed {
		t.Fatalf("失联节点 running 应收敛 failed: %+v", got)
	}
	if got, _ := hub.GetTask("t-fresh"); got.Status != TaskStatusRunning {
		t.Fatalf("在线节点 running 不应被误伤: %+v", got)
	}
	if got, _ := hub.GetTask("t-done"); got.Status != TaskStatusDone {
		t.Fatalf("终态任务不应被触碰: %+v", got)
	}
	ns := hub.ListNodes()
	for _, n := range ns {
		if n.ID == "node-lost" && n.CurrentTask != "" {
			t.Fatalf("巡检收敛后节点 CurrentTask 应清空: %+v", n)
		}
	}
	// 巡检结果落盘可查（服务重启后不复活）。
	hub.FlushPersistence()
	raw, _ := os.ReadFile(hub.tasksFile())
	if !strings.Contains(string(raw), "状态暂标失败") {
		t.Fatal("巡检收敛结果应落盘 tasks.json")
	}
	hub.Stop()
	// 幂等：再扫一轮无事发生（不再重复广播）。
	if n := hub.sweepDisconnected(150 * time.Second); n != 0 {
		t.Fatalf("收敛后重复巡检应无事发生，got %d", n)
	}
}

// TestHubWatchdogStop 启停生命周期：Stop 同步等巡检 goroutine 退出
// （不泄漏、不尾焰）；重复 Stop 幂等不 panic。
func TestHubWatchdogStop(t *testing.T) {
	hub := NewHub(t.TempDir(), nil, "", nil, nil, nil)
	defer hub.Stop()
	hub.StartWatchdog(150 * time.Second)
	time.Sleep(50 * time.Millisecond)
	done := hub.watchdogDone // Stop 内部会置空字段，先留引用
	hub.StopWatchdog()
	hub.StopWatchdog() // 幂等：重复 Stop 不 panic
	select {
	case <-done:
	default:
		t.Fatal("StopWatchdog 后巡检 goroutine 应退出")
	}
	// 重启巡检：再次 Start 应正常工作（无残留双巡检）。
	hub.StartWatchdog(150 * time.Second)
	done2 := hub.watchdogDone
	hub.StopWatchdog()
	<-done
	<-done2
}

// TestApplyVerifyReport 任务状态核实回报落地：
//   - 节点判定 done/failed：服务端 pending/running 任务被校正为节点事实，
//     Result/Error/FinishedAt 同步落盘，节点 CurrentTask 徽标清零；
//   - 节点判定 running：pending 任务推进为 running（补 StartedAt）；
//   - 终态保护：已终态任务不被迟到核实回报覆盖（与 TaskDone 同语义）。
func TestApplyVerifyReport(t *testing.T) {
	hub := NewHub(t.TempDir(), nil, "", nil, nil, nil)
	defer hub.Stop() // 异步持久化：先停落盘者再让 TempDir 清理（下同）
	hub.tasks["t-done"] = &Task{ID: "t-done", SessionID: "remote-sd", NodeID: "node-v", Text: "完成校验", Status: TaskStatusRunning, CreatedAt: 1000}
	hub.tasks["t-fail"] = &Task{ID: "t-fail", SessionID: "remote-sf", NodeID: "node-v", Text: "失败校验", Status: TaskStatusRunning, CreatedAt: 1000}
	hub.tasks["t-run"] = &Task{ID: "t-run", SessionID: "remote-sr", NodeID: "node-v", Text: "运行校验", Status: TaskStatusPending, CreatedAt: 1000}
	hub.tasks["t-final"] = &Task{ID: "t-final", SessionID: "remote-so", NodeID: "node-v", Text: "终态保护", Status: TaskStatusDone, Result: "早已完成", FinishedAt: 2000}
	ns := &nodeState{}
	ns.info.ID = "node-v"
	ns.info.CurrentTask = "t-done"
	hub.nodes["node-v"] = ns

	hub.ApplyVerifyReport(VerifyReport{
		NodeID: "node-v",
		Items: []VerifyTaskState{
			{TaskID: "t-done", Status: TaskStatusDone, Result: "PC 端实际已完成"},
			{TaskID: "t-fail", Status: TaskStatusFailed, Error: "PC 端实际已失败"},
			{TaskID: "t-run", Status: TaskStatusRunning},
			{TaskID: "t-final", Status: TaskStatusFailed, Error: "迟到回报不得覆盖终态"},
		},
	})

	if got, _ := hub.GetTask("t-done"); got == nil || got.Status != TaskStatusDone || got.Result != "PC 端实际已完成" || got.FinishedAt == 0 {
		t.Fatalf("running→done 校正: %+v", got)
	}
	if got, _ := hub.GetTask("t-fail"); got == nil || got.Status != TaskStatusFailed || got.Error != "PC 端实际已失败" {
		t.Fatalf("running→failed 校正: %+v", got)
	}
	if got, _ := hub.GetTask("t-run"); got == nil || got.Status != TaskStatusRunning || got.StartedAt == 0 {
		t.Fatalf("pending→running 校正: %+v", got)
	}
	if got, _ := hub.GetTask("t-final"); got == nil || got.Status != TaskStatusDone || got.Result != "早已完成" || got.Error != "" {
		t.Fatalf("终态任务不应被核实回报覆盖: %+v", got)
	}
	if got := hub.nodes["node-v"].info.CurrentTask; got != "" {
		t.Fatalf("核实修正后节点 CurrentTask 应清零: %q", got)
	}
}

// TestHubPruneFinishedSessions 会话任务自动清理（上限 20）：
//   - 超限后删最旧终态会话（任务+镜像+Todo 一并清，重启不复活）；
//   - 运行中/待执行会话不被强删；
//   - 未超限时不误删；
//   - 启动时孤儿镜像清理（无任务关联的 sessions.json 残留）。
func TestHubPruneFinishedSessions(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, nil, "", nil, nil, nil)
	now := time.Now().UnixMilli()
	// 造 22 个终态会话 + 1 个运行中（最旧）。终态会话按时间递增编号：s0 最旧。
	for i := 0; i < 22; i++ {
		hub.tasks[fmt.Sprintf("t-%02d", i)] = &Task{
			ID: fmt.Sprintf("t-%02d", i), SessionID: fmt.Sprintf("remote-s%02d", i),
			NodeID: "node-p", Text: fmt.Sprintf("任务%d", i),
			Status: TaskStatusDone, CreatedAt: now + int64(i)*1000, FinishedAt: now + int64(i)*1000 + 500,
		}
		hub.SaveSessionMessages(fmt.Sprintf("remote-s%02d", i), []interface{}{map[string]interface{}{"say": "text", "text": fmt.Sprintf("历史%d", i)}})
	}
	hub.tasks["t-run"] = &Task{
		ID: "t-run", SessionID: "remote-s-run", NodeID: "node-p", Text: "运行中",
		Status: TaskStatusRunning, CreatedAt: now - 999999, // 最旧
	}

	removed := hub.pruneFinishedSessions()
	// 23 个会话超上限 3 个；运行中最旧但不可删 → 删最旧终态的 3 个（s00/s01/s02）。
	if removed != 3 {
		t.Fatalf("应清理 3 个最旧终态会话（23 超 20 上限 3 个，运行中不可删）: got %d", removed)
	}
	if _, ok := hub.GetTask("t-00"); ok {
		t.Fatal("最旧终态任务 t-00 应被自动清理")
	}
	if _, ok := hub.GetTask("t-01"); ok {
		t.Fatal("次旧终态任务 t-01 应被自动清理")
	}
	if _, ok := hub.GetTask("t-02"); ok {
		t.Fatal("第三旧终态任务 t-02 应被自动清理")
	}
	if got, ok := hub.GetTask("t-run"); !ok || got.Status != TaskStatusRunning {
		t.Fatal("运行中会话不得被自动清理")
	}
	if _, ok := hub.GetTask("t-03"); !ok {
		t.Fatal("未到清理名额的会话应保留")
	}
	// 镜像联动清理（s00~s02 的 sessions.json 条目应消失）。
	for _, sid := range []string{"remote-s00", "remote-s01", "remote-s02"} {
		if msgs := hub.SessionMessages(sid); len(msgs) != 0 {
			t.Fatalf("被清理会话镜像应同步删除 %s: %d", sid, len(msgs))
		}
	}
	if msgs := hub.SessionMessages("remote-s03"); len(msgs) == 0 {
		t.Fatal("保留会话镜像不应被误删")
	}
	// 落盘复核：重启后不再复活（tasks.json 已除名）。
	hub.FlushPersistence()
	hub.Stop()
	hub2 := NewHub(dir, nil, "", nil, nil, nil)
	defer hub2.Stop()
	if _, ok := hub2.GetTask("t-00"); ok {
		t.Fatal("自动清理的任务不应在重启后复活")
	}
	if got, ok := hub2.GetTask("t-03"); !ok || got.Status != TaskStatusDone {
		t.Fatal("保留任务重启后应完整恢复")
	}

	// 未超限（20 个以内）时不清理。
	hub3 := NewHub("", nil, "", nil, nil, nil)
	for i := 0; i < 20; i++ {
		hub3.tasks[fmt.Sprintf("x-%02d", i)] = &Task{ID: fmt.Sprintf("x-%02d", i), SessionID: fmt.Sprintf("xs-%02d", i), Status: TaskStatusDone, CreatedAt: int64(i)}
	}
	if r := hub3.pruneFinishedSessions(); r != 0 {
		t.Fatalf("未超限不应清理: %d", r)
	}
}

// TestHubPruneOrphanMirrors 启动孤儿镜像清理：无任务关联的镜像条目被清。
func TestHubPruneOrphanMirrors(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, nil, "", nil, nil, nil)
	defer hub.Stop()
	hub.SaveSessionMessages("orphan-s", []interface{}{map[string]interface{}{"say": "text", "text": "孤儿"}})
	hub.tasks["t-live"] = &Task{ID: "t-live", SessionID: "live-s", Status: TaskStatusDone, CreatedAt: 1}
	hub.SaveSessionMessages("live-s", []interface{}{map[string]interface{}{"say": "text", "text": "在用"}})

	if n := hub.pruneOrphanMirrors(); n != 1 {
		t.Fatalf("应清理 1 个孤儿镜像: %d", n)
	}
	if msgs := hub.SessionMessages("orphan-s"); len(msgs) != 0 {
		t.Fatal("孤儿镜像应被清理")
	}
	if msgs := hub.SessionMessages("live-s"); len(msgs) == 0 {
		t.Fatal("在用镜像不应被误删")
	}
}

// TestHubPickNodeLoadAware 负载感知选节点：多节点在线时选 RunningTasks 最少者
// （修复串行根因之一：原实现 map 随机取首个在线节点，任务全部压向单一节点）。
func TestHubPickNodeLoadAware(t *testing.T) {
	hub := NewHub("", map[string]string{"tok1": "n-busy", "tok2": "n-idle"}, "", nil, nil, nil)
	cb := &agentConn{nodeID: "n-busy", ws: nil}
	ci := &agentConn{nodeID: "n-idle", ws: nil}
	hub.RegisterAgent(cb, AgentRegister{Name: "beta"})
	hub.RegisterAgent(ci, AgentRegister{Name: "alpha"})

	hub.mu.Lock()
	hub.nodes["n-busy"].info.RunningTasks = 2
	hub.mu.Unlock()

	// 忙碌节点 vs 空闲节点：空闲胜出。
	if got, err := hub.PickNode(""); err != nil || got != "n-idle" {
		t.Fatalf("应选空闲节点 n-idle: got=%s err=%v", got, err)
	}

	// 负载相同 → 名称序稳定决胜（alpha < beta，消除 map 遍历随机性）。
	hub.mu.Lock()
	hub.nodes["n-idle"].info.RunningTasks = 1
	hub.mu.Unlock()
	if got, _ := hub.PickNode(""); got != "n-idle" {
		t.Fatalf("同负载应按名称序选 alpha: got=%s", got)
	}

	// 指定节点优先（不看负载）。
	if got, err := hub.PickNode("n-busy"); err != nil || got != "n-busy" {
		t.Fatalf("指定节点应优先: got=%s err=%v", got, err)
	}

	// 指定节点离线 → 明确报错（不静默落到其他节点）。
	hub.UnregisterAgent("n-idle", ci)
	if _, err := hub.PickNode("n-idle"); err == nil {
		t.Fatal("指定离线节点应报错")
	}
	// 自动选择：仅剩在线节点照常选中。
	if got, err := hub.PickNode(""); err != nil || got != "n-busy" {
		t.Fatalf("仅剩在线节点应被选中: got=%s err=%v", got, err)
	}
}

// TestHubRunningCountSync 并发计数全同步点回归：终态/取消/核实递减、心跳覆盖
// （含负值容错）、重连与下线清零、落盘值重启归零 —— PickNode 负载感知的数据根基。
func TestHubRunningCountSync(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, map[string]string{"tok": "node-a"}, "", nil, nil, nil)
	conn := &agentConn{nodeID: "node-a", ws: nil}
	hub.RegisterAgent(conn, AgentRegister{Name: "计数机"})
	now := time.Now().UnixMilli()
	hub.mu.Lock()
	mk := func(id, sid string, at int64) *Task {
		tk := &Task{ID: id, SessionID: sid, NodeID: "node-a", Text: id, Status: TaskStatusRunning, CreatedAt: at, StartedAt: at}
		hub.tasks[id] = tk
		return tk
	}
	mk("t-c1", "s-c1", now)
	mk("t-c2", "s-c2", now+1)
	ns := hub.nodes["node-a"]
	ns.info.RunningTasks = 2 // 仿真两任务已派发（dispatchLocked 写成功后的状态）
	ns.info.CurrentTask = "t-c2"
	hub.mu.Unlock()

	nodeRunning := func() int {
		for _, n := range hub.ListNodes() {
			if n.ID == "node-a" {
				return n.RunningTasks
			}
		}
		return -1
	}

	// 1) 终态递减：done 后 2→1；CurrentTask 为 t-c2 不受影响。
	hub.TaskDone(TaskDone{TaskID: "t-c1", OK: true, Result: "ok"})
	if got := nodeRunning(); got != 1 {
		t.Fatalf("终态后并发应为 1: %d", got)
	}
	// 2) 取消递减：1→0。
	hub.CancelTask("t-c2", "s-c2")
	if got := nodeRunning(); got != 0 {
		t.Fatalf("取消后并发应为 0: %d", got)
	}
	// 3) 心跳覆盖（权威源回传）+ 负值容错归零 + CurrentTask 清理。
	hub.TouchAgent("node-a", 3)
	if got := nodeRunning(); got != 3 {
		t.Fatalf("心跳覆盖后并发应为 3: %d", got)
	}
	hub.TouchAgent("node-a", -2)
	if got := nodeRunning(); got != 0 {
		t.Fatalf("负值心跳应容错归零: %d", got)
	}
	hub.mu.RLock()
	ct := ns.info.CurrentTask
	hub.mu.RUnlock()
	if ct != "" {
		t.Fatalf("running=0 心跳应清 CurrentTask: %s", ct)
	}
	// 4) verify 校正递减：running 任务被节点核实为终态 → 1→0。
	hub.mu.Lock()
	mk("t-c4", "s-c4", now+3)
	ns.info.RunningTasks = 1
	ns.info.CurrentTask = "t-c4"
	hub.mu.Unlock()
	hub.ApplyVerifyReport(VerifyReport{NodeID: "node-a", Items: []VerifyTaskState{{TaskID: "t-c4", Status: TaskStatusDone, Result: "节点核实完成"}}})
	if got := nodeRunning(); got != 0 {
		t.Fatalf("verify 校正后并发应为 0: %d", got)
	}
	if got, _ := hub.GetTask("t-c4"); got.Status != TaskStatusDone {
		t.Fatalf("verify 校正应为 done: %+v", got)
	}
	// 5) 重连清零陈旧计数（首跳心跳回传真实值校正）。
	hub.mu.Lock()
	ns.info.RunningTasks = 5
	hub.mu.Unlock()
	hub.RegisterAgent(conn, AgentRegister{Name: "计数机"})
	if got := nodeRunning(); got != 0 {
		t.Fatalf("重连后计数应清零: %d", got)
	}
	// 6) 下线清零。
	hub.TouchAgent("node-a", 2)
	hub.UnregisterAgent("node-a", conn)
	if got := nodeRunning(); got != 0 {
		t.Fatalf("下线后计数应清零: %d", got)
	}
	// 7) 落盘值重启归零（心跳快照不可信，重启后以首跳校正）。
	hub.mu.Lock()
	ns.info.RunningTasks = 7
	hub.saveNodes()
	hub.mu.Unlock()
	hub.FlushPersistence()
	hub.Stop()
	hub2 := NewHub(dir, nil, "", nil, nil, nil)
	defer hub2.Stop()
	for _, n := range hub2.ListNodes() {
		if n.ID == "node-a" && n.RunningTasks != 0 {
			t.Fatalf("重启后落盘计数应归零: %+v", n)
		}
	}
}
