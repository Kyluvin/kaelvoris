package remote

// lockorder_deadlock_test.go 锁序死锁与认证链路加固回归测试（2026-10 线上事故）：
//
//  1. TestClientAuth_BadPayload_ReturnsAuthErr：真实 WS 端，首帧 auth 载荷非法
//     （Decode 失败），断言收到 auth.err（此前静默 return 无回执）。
//  2. TestClientAuth_LockStuck_ReturnsAuthErr：h.mu 被异常持有（模拟锁泄漏）时，
//     认证完成路径 5s 超时兜底回 auth.err + 关闭连接，不永久挂起。
//  3. TestHealth_MuStuck_Returns503：h.mu 被占用时 /api/health 2s 超时回 503
//     （探活永不挂死）。
//
// 注：TestAssignNodeToken_* 系列已随 node.token 去令牌化移除（2026-10），
// node_tokens.json / nodeTokensMu / AssignNodeToken 不复存在。

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestClientAuth_BadPayload_ReturnsAuthErr 认证链路静默吞错修复回归：
// 首帧 auth 载荷非法（payload 非对象 → e.Decode(&a) 失败），断言服务端回
// auth.err（修复前：静默 return，无任何回执，前端永久等待）。
func TestClientAuth_BadPayload_ReturnsAuthErr(t *testing.T) {
	hub := NewHub("", nil, "", nil, nil, nil)
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18925"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	ws, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/client", nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer ws.Close()
	// 非法载荷：payload 为数组（Auth 结构体解码失败）。
	if err := ws.WriteJSON(map[string]interface{}{
		"id": "bad1", "type": TypeAuth, "payload": []int{1, 2, 3},
	}); err != nil {
		t.Fatalf("write bad auth: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var resp Envelope
	if err := ws.ReadJSON(&resp); err != nil {
		t.Fatalf("read auth resp: %v", err)
	}
	if resp.Type != TypeAuthErr {
		t.Fatalf("bad auth payload 应回 auth.err，实际=%s payload=%s", resp.Type, resp.Payload)
	}
}

// TestClientAuth_LockStuck_ReturnsAuthErr 认证完成路径加有界超时兜底回归：
// 模拟 h.mu 被异常持续持有（锁泄漏/死锁的现场形态），认证凭据本身有效，
// AddClientTimeout 5s 拿不到锁 → 回 auth.err（前端不再永久挂起）。
func TestClientAuth_LockStuck_ReturnsAuthErr(t *testing.T) {
	hub := NewHub("", nil, "",
		map[string]string{"admin": "pw"}, nil, nil)
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18926"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	// 持有 h.mu 写锁并保持（模拟 10-09 事故：锁永久持有）。
	hub.mu.Lock()
	defer hub.mu.Unlock()

	ws, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/client", nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(NewEnvelope("a1", TypeAuth, Auth{User: "admin", Pass: "pw"})); err != nil {
		t.Fatalf("auth write: %v", err)
	}
	// AddClientTimeout 5s：在 5s + 3s 余量内必须拿到 auth.err（修复前此处永久阻塞）。
	ws.SetReadDeadline(time.Now().Add(8 * time.Second))
	var resp Envelope
	if err := ws.ReadJSON(&resp); err != nil {
		t.Fatalf("auth read（应回 auth.err，实际读错误=%v）", err)
	}
	if resp.Type != TypeAuthErr {
		t.Fatalf("锁被占用时认证应回 auth.err，实际=%s", resp.Type)
	}
}

// TestHealth_MuStuck_Returns503 健康探活带超时回归：h.mu 被占用时 /api/health
// 2s 内回 503（修复前 ListNodes 内 RLock 无限阻塞 → 监控判定服务死）。
func TestHealth_MuStuck_Returns503(t *testing.T) {
	hub := NewHub("", nil, "", nil, nil, nil)
	srv := NewServer(hub, "")

	// 持有 h.mu 写锁（模拟锁泄漏现场）。
	hub.mu.Lock()
	defer hub.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	srv.handleHealth(rec, req)
	elapsed := time.Since(start)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("h.mu 被占用时 /api/health 应回 503，实际=%d body=%s", rec.Code, rec.Body.String())
	}
	if elapsed > 4*time.Second {
		t.Fatalf("/api/health 应带 ~2s 超时返回，实际耗时 %v", elapsed)
	}
}

// TestEnqueueChat_Offline_NoSelfDeadlock 锁内再取锁自死锁回归（2026-10 线上锁冻结：
// h.mu 被永久持有 → 移动端 auth/health/节点注册全挂、nodes.json 停止更新、TCP accept
// 仍工作）：EnqueueChat 离线分支此前在持 h.mu 写锁时调用 SessionTaskID（内部
// h.mu.RLock），sync.RWMutex 不可重入 → 同 goroutine 写锁内再取读锁 = 永久自死锁。
// 修复：整个 SessionTaskID 调用移除（其结果仅被丢弃，零副作用）。
// 验证：离线暂存 + 随后的 ListNodesTimeout（/api/health 同路径）都应在短超时内完成。
func TestEnqueueChat_Offline_NoSelfDeadlock(t *testing.T) {
	hub := NewHub("", nil, "", nil, nil, nil)

	// 1) 离线暂存路径必须在 3s 内完成（修复前：持 h.mu 写锁内 RLock 自死锁，永久 hang）。
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = hub.EnqueueChat("node-offline", ChatSend{SessionID: "s-repro-1", TaskID: "", Text: "hi"})
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("EnqueueChat 离线分支 3s 未返回（持 h.mu 写锁内调用 SessionTaskID → RLock 自死锁）")
	}

	// 2) h.mu 必须可复用（模拟 /api/health 的 ListNodesTimeout 路径，2s 超时）。
	if _, ok := hub.ListNodesTimeout(2 * time.Second); !ok {
		t.Fatal("h.mu 仍不可得（离线暂存后锁未被回收，疑似残留死锁）")
	}
}

// TestAgentRegister_AccountAuth 注册鉴权（去令牌化）回归：
//   - 有效账号凭据注册 → register_ok；
//   - 错误密码 → server.error 拒绝；
//   - 匿名（空凭据）注册 → register_ok（只读在线，不绑定 Owner）。
func TestAgentRegister_AccountAuth(t *testing.T) {
	hub := NewHub("", nil, "", nil, nil,
		[]Account{{User: "pc1", Pass: "pw1", Role: "user", DisplayName: "PC1"}})
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18927"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	register := func(nodeID, user, pass string) (string, error) {
		ws, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/agent", nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer ws.Close()
		if err := ws.WriteJSON(NewEnvelope("", TypeAgentRegister, AgentRegister{
			NodeID: nodeID, User: user, Pass: pass, Name: "T", Platform: "windows",
		})); err != nil {
			t.Fatalf("register write: %v", err)
		}
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		var e Envelope
		if err := ws.ReadJSON(&e); err != nil {
			return "", err
		}
		if e.Type != TypeServerRegisterOK {
			return e.Type, nil
		}
		return e.Type, nil
	}

	// 有效凭据 → ok。
	if typ, _ := register("node-acc-1", "pc1", "pw1"); typ != TypeServerRegisterOK {
		t.Fatalf("有效账号注册应 ok，got %s", typ)
	}
	// 错误密码 → server.error。
	if typ, _ := register("node-acc-2", "pc1", "WRONG"); typ != TypeServerError {
		t.Fatalf("错误密码注册应被拒，got %s", typ)
	}
	// 匿名注册 → ok（只读在线）。
	if typ, _ := register("node-acc-3", "", ""); typ != TypeServerRegisterOK {
		t.Fatalf("匿名注册应 ok，got %s", typ)
	}
}
