package remote

// agent_heartbeat_test.go 移动端节点状态抖动回归：多会话任务执行期间（服务端
// 无下行帧的静默期），PC 节点心跳必须独立于读循环稳定上行，服务端不会误判
// 断线 → 移动端不出现「掉线↔连接」反复抖动。
//
// 修复前根因：client.go 心跳 ticker 与 ReadJSON 同循环 —— ReadJSON 阻塞等待
// 服务端下行帧期间心跳无法触发；服务端 75s 读空闲超时（agentReadIdleTimeout）
// 误判断线 → UnregisterAgent 广播离线 → 3s 重连 RegisterAgent 又广播上线。
// 修复：心跳独立 goroutine 稳定上行 + 读超时续命（非断线信号）。

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newHBHarness 心跳回归专用最小化沙盒：Hub + Server + Client 全真实协议栈，
// 在 client.Start 前注入计时参数（修复前读循环饥饿在短参数下放大必现）。
func newHBHarness(t *testing.T, hbInterval time.Duration) (*Hub, *Client) {
	t.Helper()
	hub := NewHub("", map[string]string{"tok-hb2": "node-hb2"}, "",
		map[string]string{}, map[string]string{},
		[]Account{
			{User: "admin", Pass: "admin123", Role: "admin", DisplayName: "管理员"},
			{User: "user", Pass: "user123", Role: "user", DisplayName: "手机用户"},
		})
	srv := NewServer(hub, "")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	go func() { _ = srv.Start(addr) }()
	t.Cleanup(srv.Stop)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, herr := http.Get("http://" + addr + "/api/health")
		if herr == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	c := NewClient("ws://"+addr+"/ws/agent", "tok-hb2", "node-hb2", "HB-PC",
		"test-1.0", "hb-host", "windows", "amd64",
		ClientCallbacks{
			RunTask: func(sessionID, text string, images []string, mode string, taskID string) bool {
				return true // 心跳回归不关心任务语义
			},
		})
	// 注入计时参数（Start 前必须生效：heartbeatLoop/读循环启动时读取）。
	c.mu.Lock()
	if hbInterval > 0 {
		c.heartbeatInterval = hbInterval
	}
	c.mu.Unlock()
	c.Start()
	t.Cleanup(c.Stop)

	// 等节点上线。
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !hub.NodeOnline("node-hb2") {
		time.Sleep(20 * time.Millisecond)
	}
	if !hub.NodeOnline("node-hb2") {
		t.Fatal("节点未在期限内注册上线")
	}
	return hub, c
}

// dialHBClient 手机端连接（auth user/pass；消费初始 nodes/tasks 推送）。
func dialHBClient(t *testing.T, addr string) *websocket.Conn {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/client", nil)
	if err != nil {
		t.Fatalf("手机端拨号失败: %v", err)
	}
	if err := ws.WriteJSON(NewEnvelope("auth-hb", TypeAuth, Auth{User: "user", Pass: "user123"})); err != nil {
		t.Fatalf("auth: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var e Envelope
	if err := ws.ReadJSON(&e); err != nil {
		t.Fatalf("auth 响应: %v", err)
	}
	if e.Type != TypeAuthOK {
		t.Fatalf("登录失败: %s", e.Type)
	}
	return ws
}

// dialAddrOf 从 client 拿服务端地址（构造手机端连接用）。
func dialAddrOf(t *testing.T, c *Client) string {
	t.Helper()
	c.mu.Lock()
	url := c.url
	c.mu.Unlock()
	// url 形如 ws://127.0.0.1:PORT/ws/agent → 提取 host。
	host := url[len("ws://"):]
	for i := 0; i < len(host); i++ {
		if host[i] == '/' {
			return host[:i]
		}
	}
	return host
}

// nodeSeenAt 读取节点 LastSeen（测试辅助）。
func nodeSeenAt(hub *Hub, nodeID string) int64 {
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	if ns, ok := hub.nodes[nodeID]; ok {
		return ns.info.LastSeen
	}
	return 0
}

// TestE2E_NodeStaysOnlineDuringTaskSilence 长任务静默期节点不掉线：
// 注入短心跳周期放大竞争窗口，节点注册后服务端静默（无下行帧），
// 期间断言：
//  1. 服务端视角节点持续在线（NodeOnline 恒真）；
//  2. 手机端未收到任何 nodes 离线推送（UnregisterAgent 广播已被触发即失败）。
//
// 修复后应通过；修复前（心跳与读循环耦合）短参数下读循环饥饿必触发离线广播。
func TestE2E_NodeStaysOnlineDuringTaskSilence(t *testing.T) {
	hub, c := newHBHarness(t, 500*time.Millisecond)
	addr := dialAddrOf(t, c)
	ws := dialHBClient(t, addr)
	defer ws.Close()

	// 离线广播监控 goroutine：阻塞读 ws（无超时），捕获 nodes 推送并把
	// “node-hb2 离线”信号送入 channel。
	offline := make(chan bool, 1)
	go func() {
		for {
			var e Envelope
			if err := ws.ReadJSON(&e); err != nil {
				return
			}
			if e.Type == TypeNodes {
				var nodes []Node
				_ = e.Decode(&nodes)
				for _, n := range nodes {
					if n.ID == "node-hb2" && !n.Online {
						select {
						case offline <- true:
						default:
						}
						return
					}
				}
			}
		}
	}()

	// 静默等待期：远超心跳周期余量，模拟 LLM 长时间思考
	//（服务端对节点无任何下行帧）。
	silence := 12 * time.Second
	deadline := time.Now().Add(silence)
	for time.Now().Before(deadline) {
		if !hub.NodeOnline("node-hb2") {
			t.Fatal("静默期节点掉线（服务端误判：心跳未独立上行）")
		}
		select {
		case <-offline:
			t.Fatal("移动端收到节点离线广播（抖动根因未修复）")
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestE2E_HeartbeatIndependentOfRead 读循环阻塞（服务端静默）期间心跳独立上行：
// 直接断言 LastSeen 推进（修复前：心跳与 ReadJSON 同循环，阻塞期间饥饿不推进）。
func TestE2E_HeartbeatIndependentOfRead(t *testing.T) {
	hub, _ := newHBHarness(t, 400*time.Millisecond)
	lastSeen := nodeSeenAt(hub, "node-hb2")
	if lastSeen == 0 {
		t.Fatal("节点注册后 LastSeen 未初始化")
	}
	// 静默期观察 2s（5 个心跳周期）：LastSeen 必须持续推进。
	deadline := time.Now().Add(2 * time.Second)
	last := lastSeen
	for time.Now().Before(deadline) {
		cur := nodeSeenAt(hub, "node-hb2")
		if cur > last {
			last = cur
		}
		time.Sleep(50 * time.Millisecond)
	}
	if last-lastSeen < 1000 { // 400ms 周期 × 5 = 2s，至少推进 1s 才算心跳活跃
		t.Fatalf("心跳未独立上行：LastSeen 静默期推进不足（%dms）", last-lastSeen)
	}
	if !hub.NodeOnline("node-hb2") {
		t.Fatal("心跳合规但节点仍被判离线")
	}
}
