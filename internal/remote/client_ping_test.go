package remote

// client_ping_test.go — 客户端心跳保活协议回归。
//
// 背景（长期反馈「PC 端执行完了移动端状态不更新」根因之一）：客户端 WS 半开
// （手机休眠/弱网）时 onclose 不触发，客户端假活，服务端广播的 task.result
// 永远发不到死连接。修复：客户端定期发 client.ping，服务端回 client.pong，
// 客户端以 pong/任意来帧判活；假活时客户端主动重建连接。
//
// 本测试覆盖服务端侧：
//  1. client.ping → client.pong（带 id 关联，载荷回传 ts）；
//  2. 心跳共存下常规请求-响应不受影响；
//  3. 连续多次心跳全部即时配对（模拟 25s 周期探测）；
//  4. 协议常量锁定（客户端按字符串匹配，改动即破坏新旧版本互探活）。

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// readEnv 带超时读一帧（超时返回错误，避免测试悬挂）。
func readEnv(t *testing.T, ws *websocket.Conn, timeout time.Duration) (Envelope, error) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(timeout))
	var e Envelope
	err := ws.ReadJSON(&e)
	return e, err
}

// TestServerE2E_ClientPingPong 心跳回环 + id 配对 + 常规请求共存。
func TestServerE2E_ClientPingPong(t *testing.T) {
	hub := NewHub("", map[string]string{"tok-hb": "node-hb"}, "share-hb",
		map[string]string{"admin": "pw"}, nil, nil)
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18915"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	cws, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/client", nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer cws.Close()
	_ = cws.WriteJSON(NewEnvelope("a1", TypeAuth, Auth{Token: "share-hb"}))
	var authResp Envelope
	if err := cws.ReadJSON(&authResp); err != nil || authResp.Type != TypeAuthOK {
		t.Fatalf("client auth: %+v err=%v", authResp, err)
	}

	// 心跳请求（H5 定时器同款：带 id 便于配对）。
	_ = cws.WriteJSON(NewEnvelope("hb1", TypeClientPing, map[string]interface{}{"ts": time.Now().UnixMilli()}))

	// 断言 1：收到 client.pong 且 id 配对、ts 非零。
	var pong *Envelope
	for pong == nil {
		e, err := readEnv(t, cws, 5*time.Second)
		if err != nil {
			t.Fatalf("等待 pong 超时: %v", err)
		}
		if e.ID == "hb1" && e.Type == TypeClientPong {
			pong = &e
		}
	}
	var pp struct {
		Ts int64 `json:"ts"`
	}
	if err := pong.Decode(&pp); err != nil || pp.Ts <= 0 {
		t.Fatalf("pong 载荷应携带 ts: %+v err=%v payload=%s", pong, err, pong.Payload)
	}

	// 断言 2：心跳共存下常规请求-响应不受影响（list.tasks 应答带 id）。
	_ = cws.WriteJSON(NewEnvelope("lt1", TypeListTasks, map[string]interface{}{}))
	var tasksResp *Envelope
	for tasksResp == nil {
		e, err := readEnv(t, cws, 5*time.Second)
		if err != nil {
			t.Fatalf("等待 list.tasks 应答: %v", err)
		}
		if e.ID == "lt1" {
			tasksResp = &e
		}
	}
	if tasksResp.Type != TypeTasks {
		t.Fatalf("list.tasks 应答类型不符: %s", tasksResp.Type)
	}

	// 断言 3：连续多次心跳（模拟客户端 25s 周期）全部即时配对。
	for i := 0; i < 3; i++ {
		id := "hb-loop-" + string(rune('a'+i))
		_ = cws.WriteJSON(NewEnvelope(id, TypeClientPing, nil))
		got := false
		for !got {
			e, err := readEnv(t, cws, 5*time.Second)
			if err != nil {
				t.Fatalf("心跳循环 %d 读超时: %v", i, err)
			}
			if e.ID == id && e.Type == TypeClientPong {
				got = true
			}
		}
	}
}

// TestClientPingProtocolConstants 心跳协议常量锁定 + 载荷形状校验。
func TestClientPingProtocolConstants(t *testing.T) {
	if TypeClientPing != "client.ping" || TypeClientPong != "client.pong" {
		t.Fatalf("心跳协议常量被改动: ping=%q pong=%q", TypeClientPing, TypeClientPong)
	}
	e := NewEnvelope("hb-x", TypeClientPing, map[string]interface{}{"ts": time.Now().UnixMilli()})
	if e.ID != "hb-x" {
		t.Fatalf("envelope id: %+v", e)
	}
	var p map[string]interface{}
	if err := e.Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := p["ts"]; !ok {
		t.Fatal("ping 载荷应含 ts")
	}
}
