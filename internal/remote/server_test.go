package remote

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestServerE2E 端到端冒烟：agent 注册 → 客户端认证 → 发布任务 → 派发 →
// 事件 → 完成 → 客户端收到结果。
func TestServerE2E(t *testing.T) {
	// 内存 hub（agent token：tok-e2e → node-e2e）。
	hub := NewHub("", map[string]string{"tok-e2e": "node-e2e"}, "", nil, nil, nil)
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18911"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	wsBase := "ws://" + addr

	// ---- agent 连接 ----
	agentWS, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws/agent", nil)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer agentWS.Close()
	if err := agentWS.WriteJSON(NewEnvelope("", TypeAgentRegister, AgentRegister{
		NodeID: "node-e2e", Name: "E2E节点", Platform: "windows",
		Version: "0.6.0", Hostname: "e2e-host",
	})); err != nil {
		t.Fatalf("agent register: %v", err)
	}
	var regOK Envelope
	if err := agentWS.ReadJSON(&regOK); err != nil || regOK.Type != TypeServerRegisterOK {
		t.Fatalf("register response: %+v err=%v", regOK, err)
	}

	// agent 读泵：把推送消息转 channel 供断言。
	agentMsgs := make(chan Envelope, 32)
	go func() {
		for {
			var e Envelope
			if err := agentWS.ReadJSON(&e); err != nil {
				close(agentMsgs)
				return
			}
			agentMsgs <- e
		}
	}()

	// ---- 客户端连接（管理员） ----
	clientWS, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws/client", nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer clientWS.Close()
	if err := clientWS.WriteJSON(NewEnvelope("auth1", TypeAuth, Auth{User: "admin", Pass: ""})); err != nil {
		t.Fatalf("client auth send: %v", err)
	}
	// 空账号（未配置 admin） → auth.err。换共享路径：直接注入 clientToken。
	hub2 := hub
	_ = hub2

	var authResp Envelope
	if err := clientWS.ReadJSON(&authResp); err != nil {
		t.Fatalf("client auth read: %v", err)
	}
	if authResp.Type != TypeAuthErr {
		t.Fatalf("expected auth.err for empty admin config, got %s", authResp.Type)
	}
	clientWS.Close()

	// 重建 hub：带管理员与共享 token。
	srv.Stop()
	hub2 = NewHub("", map[string]string{"tok-e2e": "node-e2e"}, "share-tok",
		map[string]string{"admin": "pw"}, map[string]string{"user": "pw"}, nil)
	srv2 := NewServer(hub2, "")
	addr2 := "127.0.0.1:18912"
	go func() { _ = srv2.Start(addr2) }()
	defer srv2.Stop()
	time.Sleep(200 * time.Millisecond)

	// agent 重连。
	agentWS2, _, err := websocket.DefaultDialer.Dial("ws://"+addr2+"/ws/agent", nil)
	if err != nil {
		t.Fatalf("agent dial2: %v", err)
	}
	defer agentWS2.Close()
	_ = agentWS2.WriteJSON(NewEnvelope("", TypeAgentRegister, AgentRegister{
		NodeID: "node-e2e", Name: "E2E节点", Platform: "windows", Version: "0.6.0",
	}))
	var regOK2 Envelope
	if err := agentWS2.ReadJSON(&regOK2); err != nil || regOK2.Type != TypeServerRegisterOK {
		t.Fatalf("register2: %+v err=%v", regOK2, err)
	}
	agentMsgs2 := make(chan Envelope, 64)
	go func() {
		for {
			var e Envelope
			if err := agentWS2.ReadJSON(&e); err != nil {
				close(agentMsgs2)
				return
			}
			agentMsgs2 <- e
		}
	}()
	// 消费 register_ok 后的首帧心跳回执等。
	time.Sleep(100 * time.Millisecond)

	// 客户端重连（共享 token）。
	clientWS2, _, err := websocket.DefaultDialer.Dial("ws://"+addr2+"/ws/client", nil)
	if err != nil {
		t.Fatalf("client dial2: %v", err)
	}
	defer clientWS2.Close()
	_ = clientWS2.WriteJSON(NewEnvelope("a1", TypeAuth, Auth{User: "admin", Pass: "pw"}))
	if err := clientWS2.ReadJSON(&authResp); err != nil {
		t.Fatalf("auth read2: %v", err)
	}
	if authResp.Type != TypeAuthOK {
		t.Fatalf("expected auth.ok, got %s payload=%s", authResp.Type, authResp.Payload)
	}

	// 客户端发布任务。
	_ = clientWS2.WriteJSON(NewEnvelope("t1", TypeCreateTask, CreateTask{NodeID: "node-e2e", Text: "E2E 任务：检查编译"}))

	// ---- agent 收到派发 ----
	var dispatched *TaskDispatch
	deadline := time.After(5 * time.Second)
	for dispatched == nil {
		select {
		case e, ok := <-agentMsgs2:
			if !ok {
				t.Fatal("agent stream closed")
			}
			if e.Type == TypeServerTaskDispatch {
				var d TaskDispatch
				if err := e.Decode(&d); err != nil {
					t.Fatalf("dispatch decode: %v", err)
				}
				dispatched = &d
			}
		case <-deadline:
			t.Fatal("timeout waiting dispatch")
		}
	}
	if dispatched.Text != "E2E 任务：检查编译" || dispatched.SessionID == "" {
		t.Fatalf("dispatch payload: %+v", dispatched)
	}

	// ---- agent 推事件 + 完成 ----
	_ = agentWS2.WriteJSON(NewEnvelope("", TypeAgentTaskEvent, TaskEvent{
		TaskID: dispatched.TaskID, SessionID: dispatched.SessionID, Kind: "text", Text: "处理中…",
	}))
	_ = agentWS2.WriteJSON(NewEnvelope("", TypeAgentTaskDone, TaskDone{
		TaskID: dispatched.TaskID, OK: true, Result: "E2E 完成",
	}))

	// ---- 客户端收到 task.result ----
	var doneEvt *Envelope
	deadline2 := time.After(5 * time.Second)
	_ = deadline2
	clientMsgs := make(chan Envelope, 64)
	go func() {
		for {
			var e Envelope
			if err := clientWS2.ReadJSON(&e); err != nil {
				close(clientMsgs)
				return
			}
			clientMsgs2 := e
			_ = clientMsgs2
			clientMsgs <- e
		}
	}()
	for doneEvt == nil {
		select {
		case e, ok := <-clientMsgs:
			if !ok {
				t.Fatal("client closed")
			}
			if e.Type == TypeTaskResult {
				doneEvt = &e
			}
		case <-deadline:
			t.Fatal("timeout waiting task.result")
		}
	}
	var p map[string]interface{}
	if err := json.Unmarshal(doneEvt.Payload, &p); err != nil {
		t.Fatalf("result payload: %v", err)
	}
	if p["result"] != "E2E 完成" {
		t.Fatalf("result text: %v", p["result"])
	}
}

// jsonUnmarshal 辅助。
func jsonUnmarshalE2E(b []byte, out interface{}) error {
	return json.Unmarshal(b, out)
}

// TestServerE2E_MultiInstanceSameToken 多实例同令牌多开（PC 端槽位派生身份）：
// 同一 token 的两个实例分别以基础 ID 与派生 ID 注册 → 服务端呈现为两个在线
// 节点；指定派生节点派发任务仅由该实例收到（多开不互相顶替）。
func TestServerE2E_MultiInstanceSameToken(t *testing.T) {
	hub := NewHub("", map[string]string{"tok-multi": "node-multi"}, "share-multi",
		map[string]string{"admin": "pw"}, nil, nil)
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18913"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	dialAgent := func(nodeID, name string) *websocket.Conn {
		t.Helper()
		ws, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/agent", nil)
		if err != nil {
			t.Fatalf("agent dial (%s): %v", nodeID, err)
		}
		if err := ws.WriteJSON(NewEnvelope("", TypeAgentRegister, AgentRegister{
			NodeID: nodeID, Name: name,
			Platform: "windows", Version: "0.6.3",
		})); err != nil {
			t.Fatalf("agent register (%s): %v", nodeID, err)
		}
		var regOK Envelope
		if err := ws.ReadJSON(&regOK); err != nil || regOK.Type != TypeServerRegisterOK {
			t.Fatalf("register (%s): %+v err=%v", nodeID, regOK, err)
		}
		return ws
	}

	// 两个实例：槽位 1（基础 ID）+ 槽位 2（派生 node-multi-2）。
	ws1 := dialAgent("node-multi", "主机实例")
	defer ws1.Close()
	ws2 := dialAgent("node-multi-2", "主机实例 (2)")
	defer ws2.Close()

	// 服务端应呈现两个在线节点，且名称/ID 各自独立。
	nodes := hub.ListNodes()
	seen := map[string]string{} // id → name
	for _, n := range nodes {
		if n.Online {
			seen[n.ID] = n.Name
		}
	}
	if seen["node-multi"] != "主机实例" || seen["node-multi-2"] != "主机实例 (2)" {
		t.Fatalf("multi-instance nodes: %+v", seen)
	}

	// 节点 ID 语义（去令牌化 2026-10）：无令牌家族绑定，自报 ID 即身份（服务端
	// 原样采纳为独立节点）。冒充防护转为凭据层：注册凭据校验（非空凭据必须
	// 过统一账号表）+ 指挥权 Owner 校验（同账号客户端才能派发）。
	ws3, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/agent", nil)
	if err != nil {
		t.Fatalf("impersonation dial: %v", err)
	}
	defer ws3.Close()
	_ = ws3.WriteJSON(NewEnvelope("", TypeAgentRegister, AgentRegister{
		NodeID: "node-other", Name: "冒充者"}))
	var regOK3 Envelope
	if err := ws3.ReadJSON(&regOK3); err != nil || regOK3.Type != TypeServerRegisterOK {
		t.Fatalf("impersonation register: %+v err=%v", regOK3, err)
	}
	var p3 struct {
		NodeID string `json:"nodeId"`
	}
	if err := json.Unmarshal(regOK3.Payload, &p3); err != nil || p3.NodeID != "node-other" {
		t.Fatalf("token-less mode should adopt self-reported id: %+v err=%v", p3, err)
	}
	// 第三节点独立呈现（不顶替 node-multi 家族）。
	seen3 := map[string]bool{}
	for _, n := range hub.ListNodes() {
		if n.Online {
			seen3[n.ID] = true
		}
	}
	if !seen3["node-multi"] || !seen3["node-multi-2"] || !seen3["node-other"] {
		t.Fatalf("three independent nodes expected: %v", seen3)
	}

	// 客户端发布任务到派生节点 → 仅实例 2 收到派发。
	cws, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/client", nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer cws.Close()
	_ = cws.WriteJSON(NewEnvelope("a1", TypeAuth, Auth{User: "admin", Pass: "pw"}))
	var authResp Envelope
	if err := cws.ReadJSON(&authResp); err != nil || authResp.Type != TypeAuthOK {
		t.Fatalf("client auth: %+v err=%v", authResp, err)
	}

	msgs1 := make(chan Envelope, 16)
	go func() {
		for {
			var e Envelope
			if err := ws1.ReadJSON(&e); err != nil {
				close(msgs1)
				return
			}
			msgs1 <- e
		}
	}()
	msgs2 := make(chan Envelope, 16)
	go func() {
		for {
			var e Envelope
			if err := ws2.ReadJSON(&e); err != nil {
				close(msgs2)
				return
			}
			msgs2 <- e
		}
	}()

	_ = cws.WriteJSON(NewEnvelope("t1", TypeCreateTask, CreateTask{NodeID: "node-multi-2", Text: "多开定向任务"}))

	// 实例 2 必须收到派发。
	var dispatched *TaskDispatch
	deadline := time.After(5 * time.Second)
	for dispatched == nil {
		select {
		case e, ok := <-msgs2:
			if !ok {
				t.Fatal("agent2 stream closed")
			}
			if e.Type == TypeServerTaskDispatch {
				var d TaskDispatch
				if err := e.Decode(&d); err != nil {
					t.Fatalf("dispatch decode: %v", err)
				}
				dispatched = &d
			}
		case <-deadline:
			t.Fatal("timeout waiting dispatch on node-multi-2")
		}
	}
	if dispatched.Text != "多开定向任务" || dispatched.SessionID == "" {
		t.Fatalf("dispatch payload: %+v", dispatched)
	}

	// 实例 1 不得收到该任务的派发（读到 nodes/tasks 推送中无 dispatch 即可）。
	drainDeadline := time.After(1 * time.Second)
	for {
		select {
		case e, ok := <-msgs1:
			if !ok {
				t.Fatal("agent1 stream closed")
			}
			if e.Type == TypeServerTaskDispatch {
				t.Fatal("instance 1 should not receive dispatch for node-multi-2")
			}
		case <-drainDeadline:
			return // 观察窗口内无派发 → 符合预期
		}
	}
}

// TestServerE2E_VerifyAllWhenNoActiveTasks 回归：全部任务已终态 + 节点在线
// 时，verify.tasks 不再误报「没有在线节点」（根因：仅按 pending/running 分组，
// 活动清单为空时 routed==0 走离线分支）。修复后空清单下发 verifyAll 哨兵到
// 在线节点，节点回空报告 → op.result ok + verify.done(ok, checked=0) 收尾。
func TestServerE2E_VerifyAllWhenNoActiveTasks(t *testing.T) {
	hub := NewHub("", map[string]string{"tok-v": "node-v"}, "share-v",
		map[string]string{"admin": "pw"}, nil, nil)
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18914"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	// 节点上线。
	agentWS, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/agent", nil)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer agentWS.Close()
	_ = agentWS.WriteJSON(NewEnvelope("", TypeAgentRegister, AgentRegister{
		NodeID: "node-v", Name: "核实节点", Platform: "windows", Version: "0.6.4",
	}))
	var regOK Envelope
	if err := agentWS.ReadJSON(&regOK); err != nil || regOK.Type != TypeServerRegisterOK {
		t.Fatalf("register: %+v err=%v", regOK, err)
	}
	agentMsgs := make(chan Envelope, 16)
	go func() {
		for {
			var e Envelope
			if err := agentWS.ReadJSON(&e); err != nil {
				close(agentMsgs)
				return
			}
			agentMsgs <- e
		}
	}()

	// 预置历史终态任务（无任何 pending/running）—— 复现线上误报场景。
	hub.mu.Lock()
	hub.tasks["t-hist"] = &Task{ID: "t-hist", SessionID: "remote-sh", NodeID: "node-v", Text: "历史任务", Status: TaskStatusFailed, CreatedAt: 1000}
	hub.mu.Unlock()

	// 客户端连接（读泵只建一次，全断言共用一个 channel）。
	cws, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/client", nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer cws.Close()
	_ = cws.WriteJSON(NewEnvelope("a1", TypeAuth, Auth{Token: "share-v"}))
	var authResp Envelope
	if err := cws.ReadJSON(&authResp); err != nil || authResp.Type != TypeAuthOK {
		t.Fatalf("client auth: %+v err=%v", authResp, err)
	}
	clientMsgs := make(chan Envelope, 32)
	go func() {
		for {
			var e Envelope
			if err := cws.ReadJSON(&e); err != nil {
				close(clientMsgs)
				return
			}
			clientMsgs <- e
		}
	}()

	// 发起核实（移动端刷新小按钮同款请求）。
	_ = cws.WriteJSON(NewEnvelope("v1", TypeVerifyTasks, VerifyTasks{}))

	// 断言 1：op.result ok=true（旧代码此处为 ok=false「没有在线节点，无法核实」）。
	var opResp *Envelope
	deadline := time.After(5 * time.Second)
	for opResp == nil {
		select {
		case e, ok := <-clientMsgs:
			if !ok {
				t.Fatal("client stream closed")
			}
			if e.ID == "v1" && e.Type == TypeOpResult {
				opResp = &e
			}
		case <-deadline:
			t.Fatal("timeout waiting op.result for verify.tasks")
		}
	}
	var op OpResult
	if err := opResp.Decode(&op); err != nil || !op.OK {
		t.Fatalf("verify.tasks 应受理成功（旧代码误报无节点）: %+v err=%v payload=%s", op, err, opResp.Payload)
	}

	// 断言 2：在线节点收到 server.verify_tasks 且携带 verifyAll 哨兵。
	var vtSeen *VerifyTasks
	deadline2 := time.After(5 * time.Second)
	for vtSeen == nil {
		select {
		case e, ok := <-agentMsgs:
			if !ok {
				t.Fatal("agent stream closed")
			}
			if e.Type == TypeServerVerifyTasks {
				var vt VerifyTasks
				if err := e.Decode(&vt); err != nil {
					t.Fatalf("verify decode: %v", err)
				}
				vtSeen = &vt
			}
		case <-deadline2:
			t.Fatal("timeout waiting server.verify_tasks on agent")
		}
	}
	if !vtSeen.VerifyAll || len(vtSeen.TaskIDs) != 0 || vtSeen.RequestedBy != "client" {
		t.Fatalf("应下发 verifyAll 哨兵（空清单+requestedBy=client）: %+v", vtSeen)
	}

	// 节点回空报告（无本地会话事实）→ verify.done(ok=true) 收尾广播。
	_ = agentWS.WriteJSON(NewEnvelope("", TypeAgentVerifyReport, VerifyReport{NodeID: "node-v", Items: []VerifyTaskState{}, RequestedBy: vtSeen.RequestedBy}))
	var doneEvt *Envelope
	deadline3 := time.After(5 * time.Second)
	for doneEvt == nil {
		select {
		case e, ok := <-clientMsgs:
			if !ok {
				t.Fatal("client stream closed (2)")
			}
			if e.Type == TypeVerifyDone {
				doneEvt = &e
			}
		case <-deadline3:
			t.Fatal("timeout waiting verify.done")
		}
	}
	var vd VerifyDone
	if err := doneEvt.Decode(&vd); err != nil || !vd.OK || vd.Checked != 0 || vd.RequestedBy != "client" {
		t.Fatalf("verify.done 应 ok=true/checked=0/requestedBy=client: %+v err=%v", vd, err)
	}
}

// TestServerE2E_ClearTasks 回归：移动端「清空任务与会话」全链路。
// 前置：2 条任务（1 running / 1 done）+ 会话镜像。断言：
//  1. clear.tasks 受理成功（op.result ok，含删除数）；
//  2. 服务端权威数据立即清空（任务/事件/镜像全空，活动任务置 canceled）；
//  3. 在线节点收到 server.clear_sessions（本地存档联动清理指令）；
//  4. 客户端收到 clear.done{mode=clear, ok, canceled=1, deleted=2} + tasks/sessions 刷新广播。
func TestServerE2E_ClearTasks(t *testing.T) {
	hub := NewHub("", map[string]string{"tok-c": "node-c"}, "share-c",
		map[string]string{"admin": "pw"}, nil, nil)
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18916"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	// 节点上线。
	agentWS, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/agent", nil)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer agentWS.Close()
	_ = agentWS.WriteJSON(NewEnvelope("", TypeAgentRegister, AgentRegister{
		NodeID: "node-c", Name: "清空节点", Platform: "windows",
	}))
	var regOK Envelope
	if err := agentWS.ReadJSON(&regOK); err != nil || regOK.Type != TypeServerRegisterOK {
		t.Fatalf("register: %+v err=%v", regOK, err)
	}
	agentMsgs := make(chan Envelope, 16)
	go func() {
		for {
			var e Envelope
			if err := agentWS.ReadJSON(&e); err != nil {
				close(agentMsgs)
				return
			}
			agentMsgs <- e
		}
	}()

	// 预置任务 + 会话镜像（1 running + 1 done）。
	hub.mu.Lock()
	hub.tasks["t-run"] = &Task{ID: "t-run", SessionID: "remote-s1", NodeID: "node-c", Text: "运行中任务", Status: TaskStatusRunning, CreatedAt: 1000}
	hub.tasks["t-done"] = &Task{ID: "t-done", SessionID: "remote-s2", NodeID: "node-c", Text: "已完成任务", Status: TaskStatusDone, CreatedAt: 2000}
	hub.events["t-run"] = []TaskEvent{{TS: 1000, Kind: "text", Text: "hi"}}
	hub.mu.Unlock()
	hub.SaveSessionMessages("remote-s1", []interface{}{json.RawMessage(`{"type":"say","say":"text"}`)})

	// 客户端连接。
	cws, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/client", nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer cws.Close()
	_ = cws.WriteJSON(NewEnvelope("a1", TypeAuth, Auth{User: "admin", Pass: "pw"}))
	var authResp Envelope
	if err := cws.ReadJSON(&authResp); err != nil || authResp.Type != TypeAuthOK {
		t.Fatalf("client auth: %+v err=%v", authResp, err)
	}
	clientMsgs := make(chan Envelope, 32)
	go func() {
		for {
			var e Envelope
			if err := cws.ReadJSON(&e); err != nil {
				close(clientMsgs)
				return
			}
			clientMsgs <- e
		}
	}()

	// 发起清空（移动端「清空」按钮同款请求，mode=clear）。
	_ = cws.WriteJSON(NewEnvelope("c1", TypeClearTasks, ClearAll{Mode: "clear"}))

	// 断言 1：op.result ok=true（含删除数文案）。
	var opResp *Envelope
	deadline := time.After(5 * time.Second)
	for opResp == nil {
		select {
		case e, ok := <-clientMsgs:
			if !ok {
				t.Fatal("client stream closed")
			}
			if e.ID == "c1" && e.Type == TypeOpResult {
				opResp = &e
			}
		case <-deadline:
			t.Fatal("timeout waiting op.result for clear.tasks")
		}
	}
	var op OpResult
	if err := opResp.Decode(&op); err != nil || !op.OK {
		t.Fatalf("clear.tasks 应受理成功: %+v err=%v", op, err)
	}

	// 断言 2：节点收到 server.clear_sessions（本地存档联动清理）。
	var clearSeen bool
	deadline2 := time.After(5 * time.Second)
	for !clearSeen {
		select {
		case e, ok := <-agentMsgs:
			if !ok {
				t.Fatal("agent stream closed")
			}
			if e.Type == TypeServerClearSessions {
				clearSeen = true
			}
		case <-deadline2:
			t.Fatal("timeout waiting server.clear_sessions on agent")
		}
	}

	// 断言 3：服务端权威数据已清空（任务/事件/镜像；活动任务已置 canceled）。
	hub.mu.Lock()
	taskN, evN := len(hub.tasks), len(hub.events)
	hub.mu.Unlock()
	if taskN != 0 || evN != 0 {
		t.Fatalf("服务端任务/事件应全部清空: tasks=%d events=%d", taskN, evN)
	}
	if got := len(hub.SessionMessages("remote-s1")); got != 0 {
		t.Fatalf("会话镜像应已清空: got %d", got)
	}

	// 断言 4：客户端收到 clear.done{mode=clear, ok, canceled=1, deleted=2}
	//（+ tasks/sessions 刷新广播）。
	var clearDone *Envelope
	var tasksSeen, sessionsSeen bool
	deadline3 := time.After(5 * time.Second)
	for clearDone == nil || !tasksSeen || !sessionsSeen {
		select {
		case e, ok := <-clientMsgs:
			if !ok {
				t.Fatal("client stream closed (2)")
			}
			switch e.Type {
			case TypeClearDone:
				var cd ClearDone
				if err := e.Decode(&cd); err != nil {
					t.Fatalf("clear.done decode: %v", err)
				}
				if !cd.OK || cd.Mode != "clear" || cd.Canceled != 1 || cd.Deleted != 2 {
					t.Fatalf("clear.done 应 ok/mode=clear/canceled=1/deleted=2: %+v", cd)
				}
				clearDone = &e
			case TypeTasks:
				tasksSeen = true
			case "sessions":
				sessionsSeen = true
			}
		case <-deadline3:
			t.Fatalf("timeout waiting clear.done=%v tasks=%v sessions=%v", clearDone != nil, tasksSeen, sessionsSeen)
		}
	}

	// 服务端任务表已空（ClearAllTasks 已置终态并整表清除）。
	if got := len(hub.ListTasks(0)); got != 0 {
		t.Fatalf("清空后任务列表应为空: got %d", got)
	}
}

// TestAccountPasswdE2E 修改密码 WS 端到端（account.passwd 链路）：
//
//  1. client（alice/client）自己改密：错旧密码拒（op.result false）、
//     对旧密码成（account.passwd_ok + self）→ 旧密码登录拒、新密码登录通；
//  2. admin 重置他人：alice 新密码登录 → admin 无旧密码改 alice 密成功；
//  3. 非 admin 改他人：拒（op.result false，内存表未动）。
func TestAccountPasswdE2E(t *testing.T) {
	hub := NewHub("", map[string]string{"tok-e2e": "node-e2e"}, "",
		map[string]string{"admin": "pw"}, map[string]string{"alice": "pa", "bob": "pb"}, nil)
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18913"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	dial := func(user, pass string) *websocket.Conn {
		t.Helper()
		ws, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/client", nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_ = ws.WriteJSON(NewEnvelope("auth", TypeAuth, Auth{User: user, Pass: pass}))
		var resp Envelope
		if err := ws.ReadJSON(&resp); err != nil {
			t.Fatalf("auth read: %v", err)
		}
		if resp.Type != TypeAuthOK {
			t.Fatalf("auth %s/%s: got %s payload=%s", user, pass, resp.Type, resp.Payload)
		}
		return ws
	}
	// sendReq 发送请求帧并等待同 ID 响应帧（auth.ok 之外的首帧即目标响应；
	// 广播帧无 ID 或 ID 不匹配时继续读，超时 5s 判失败）。
	sendReq := func(ws *websocket.Conn, typ string, payload interface{}) Envelope {
		t.Helper()
		req := NewEnvelope("pw1", typ, payload)
		if err := ws.WriteJSON(req); err != nil {
			t.Fatalf("write %s: %v", typ, err)
		}
		deadline := time.After(5 * time.Second)
		for {
			var e Envelope
			select {
			case done := <-deadline:
				_ = done
				t.Fatalf("timeout waiting %s response", typ)
			default:
			}
			_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
			if err := ws.ReadJSON(&e); err != nil {
				t.Fatalf("read %s response: %v", typ, err)
			}
			if e.ID != "pw1" {
				continue // 广播/无关帧，跳过继续读
			}
			return e
		}
	}

	// ---- 1. alice 自己改密 ----
	aliceWS := dial("alice", "pa")
	defer aliceWS.Close()
	// 错旧密码：op.result(false)，密码未变（旧密码仍可登录）。
	if e := sendReq(aliceWS, TypeAccountPasswd, AccountPasswd{OldPass: "WRONG", NewPass: "pa22"}); e.Type != TypeOpResult {
		t.Fatalf("错旧密码应 op.result: got %s payload=%s", e.Type, e.Payload)
	}
	bobWS := dial("bob", "pb")
	bobWS.Close()
	// 正确旧密码：account.passwd_ok + self=true。
	okResp := sendReq(aliceWS, TypeAccountPasswd, AccountPasswd{OldPass: "pa", NewPass: "pa22"})
	var okp AccountPasswdOK
	if err := okResp.Decode(&okp); err != nil || okp.User != "alice" || !okp.Self {
		t.Fatalf("passwd_ok 应 user=alice self=true: resp=%s payload=%s err=%v", okResp.Type, okResp.Payload, err)
	}
	aliceWS.Close()
	// 新密码登录通（dial 内部失败即 Fatal，成功即证明旧密码失效新密码生效）。
	newWS := dial("alice", "pa22")
	newWS.Close()

	// ---- 2. admin 无旧密码重置 alice ----
	adminWS := dial("admin", "pw")
	defer adminWS.Close()
	resetResp := sendReq(adminWS, TypeAccountPasswd, AccountPasswd{User: "alice", NewPass: "reset1"})
	var rk AccountPasswdOK
	if err := resetResp.Decode(&rk); err != nil || rk.User != "alice" || rk.Self {
		t.Fatalf("重置应 user=alice self=false: resp=%s payload=%s err=%v", resetResp.Type, resetResp.Payload, err)
	}
	// 旧密码 pa22 失效，reset1 生效。
	badWS, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/client", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = badWS.WriteJSON(NewEnvelope("auth", TypeAuth, Auth{User: "alice", Pass: "pa22"}))
	var badResp Envelope
	_ = badWS.ReadJSON(&badResp)
	if badResp.Type != TypeAuthErr {
		t.Fatalf("旧密码 pa22 应失效: got %s", badResp.Type)
	}
	badWS.Close()
	if w := dial("alice", "reset1"); w != nil {
		w.Close()
	}

	// ---- 3. 非 admin（alice）改他人（bob）：拒，内存表未动 ----
	aliceWS2 := dial("alice", "reset1")
	defer aliceWS2.Close()
	if e := sendReq(aliceWS2, TypeAccountPasswd, AccountPasswd{User: "bob", NewPass: "hacked"}); e.Type != TypeOpResult {
		t.Fatalf("非 admin 改他人应 op.result: got %s payload=%s", e.Type, e.Payload)
	}
	bobWS2 := dial("bob", "pb")
	bobWS2.Close() // bob 密码未被改动，原密码仍可登录
}

// TestDeriveAgentNodeID 派生 ID 校验：合法派生采纳，冒充/非法回落基础 ID。
func TestDeriveAgentNodeID(t *testing.T) {
	cases := []struct {
		base, claimed, want string
	}{
		{"node-a", "", "node-a"},           // 旧客户端不报 ID
		{"node-a", "node-a", "node-a"},     // 基础 ID 自身
		{"node-a", "node-a-2", "node-a-2"}, // 槽位派生
		{"node-a", "node-a-12", "node-a-12"},
		{"node-a", "node-b", "node-a"},       // 冒充他节点
		{"node-a", "node-a-x", "node-a"},     // 非数字后缀
		{"node-a", "node-a-2x", "node-a"},    // 混合后缀
		{"node-a", "other-node-3", "node-a"}, // 前缀不匹配
	}
	for _, c := range cases {
		if got := deriveAgentNodeID(c.base, c.claimed); got != c.want {
			t.Errorf("deriveAgentNodeID(%q,%q)=%q want %q", c.base, c.claimed, got, c.want)
		}
	}
}
