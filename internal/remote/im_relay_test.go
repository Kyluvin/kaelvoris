package remote

// im_relay_test.go IM 渠道公网中继端到端测试（im_relay.go）：
//
//	节点注册中继渠道（agent.im_state）→ 假 IM 平台 POST 公网回调 → 服务端
//	按指纹路由 → server.im_inbound 下发节点（断言载荷完整性）；
//	覆盖：注册/替换/注销、未注册 404、节点离线 503、群消息 hookID 白名单、
//	GET 探活、指纹一致性（节点/服务端双端计算同值）。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// imChatbotAckResp chatbot 文档应答断言结构（服务端 writeIMChatbotAck 输出形态；
// remote 包不 import 宿主包，此处本地声明同构结构）。
type imChatbotAckResp struct {
	Success bool `json:"success"`
	Data    struct {
		Type    int    `json:"type"`
		Content string `json:"content"`
	} `json:"data"`
}

// dialTestAgent 建立节点连接并注册（返回连接；msgs 可空 = 不建读泵）。
// token 参数保留占位（去令牌化后注册匿名即可：NodeID 自报生效，不占用其他测试账号）。
func dialTestAgent(t *testing.T, wsBase, nodeID, token string, msgs chan Envelope) *websocket.Conn {
	_ = token
	t.Helper()
	ws, resp, err := websocket.DefaultDialer.Dial(wsBase+"/ws/agent", nil)
	if err != nil {
		if resp != nil {
			b := make([]byte, 300)
			n, _ := resp.Body.Read(b)
			t.Fatalf("agent dial: %v (http status %d, server: %v, body: %s)", err, resp.StatusCode, resp.Header.Get("Server"), string(b[:n]))
		}
		t.Fatalf("agent dial: %v", err)
	}
	if err := ws.WriteJSON(NewEnvelope("", TypeAgentRegister, AgentRegister{
		NodeID: nodeID, Name: "IM中继节点", Platform: "windows",
	})); err != nil {
		t.Fatalf("agent register: %v", err)
	}
	var regOK Envelope
	if err := ws.ReadJSON(&regOK); err != nil || regOK.Type != TypeServerRegisterOK {
		t.Fatalf("register: %+v err=%v", regOK, err)
	}
	if msgs != nil {
		go func() {
			for {
				var e Envelope
				if err := ws.ReadJSON(&e); err != nil {
					close(msgs)
					return
				}
				msgs <- e
			}
		}()
	}
	return ws
}

// waitEnvelope 等待指定类型帧（超时失败）。
func waitEnvelope(t *testing.T, msgs chan Envelope, typ string, timeout time.Duration) Envelope {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e, ok := <-msgs:
			if !ok {
				t.Fatalf("连接关闭，未等到 %s", typ)
			}
			if e.Type == typ {
				return e
			}
		case <-deadline:
			t.Fatalf("等待 %s 超时", typ)
		}
	}
}

// TestIMRelayE2E_PubCallback 公共号回调端到端：注册 → 平台 POST → 节点收到
// server.im_inbound（载荷渠道定位五元组 + 原始请求体完整透传）。
func TestIMRelayE2E_PubCallback(t *testing.T) {
	hub := NewHub("", map[string]string{"tok-im": "node-im"}, "", nil, nil, nil)
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18921"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)
	base := "http://" + addr

	agentMsgs := make(chan Envelope, 32)
	agentWS := dialTestAgent(t, "ws://"+addr, "node-im", "tok-im", agentMsgs)
	defer agentWS.Close()

	// 节点注册中继渠道（公共号模式五元组）。
	ch := IMRelayChannel{Name: "测试公众号", PublicID: "XT-1", EID: "E1", WebhookToken: "tk"}
	fp := imRelayChannelFP(ch.Name, ch.PublicID, ch.EID, ch.WebhookToken, ch.GroupHookIDs)
	agentWS.WriteJSON(NewEnvelope("", TypeAgentIMState, IMRelayState{Enabled: true, Channels: []IMRelayChannel{ch}}))

	// 等 im_state 处理完成（worker 串行处理；轮询注册表）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(hub.IMRelayChannelList()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("中继渠道注册未生效: %v", hub.IMRelayChannelList())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 假 IM 平台：POST 公共号回调。
	body := `{"ServiceType":"1","PubAccId":"XT-1","FromUserName":"ou-abc","MsgType":"text","MsgId":"m-1","CreateTime":"1700000000000","PubToken":"sig","Payload":{"Content":"帮我查个东西"}}`
	resp, err := http.Post(base+imRelayPathPub+fp, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("平台回调请求: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("平台回调应答非 200: %d", resp.StatusCode)
	}

	// 节点收到 server.im_inbound：载荷五元组与请求体逐字段断言。
	e := waitEnvelope(t, agentMsgs, TypeServerIMInbound, 3*time.Second)
	var in IMRelayInbound
	if err := e.Decode(&in); err != nil {
		t.Fatalf("载荷解析: %v", err)
	}
	if in.Kind != "pub" || in.Name != ch.Name || in.PublicID != ch.PublicID || in.EID != ch.EID {
		t.Fatalf("渠道定位字段错误: %+v", in)
	}
	if !strings.Contains(in.Body, `"FromUserName":"ou-abc"`) {
		t.Fatalf("原始请求体未透传: %s", in.Body)
	}
}

// TestIMRelayE2E_GroupHookWhitelist 群消息路由：GET 探活、已配置 hookID 转发
// （GroupHooks 完整列表透传，节点指纹契约）、未配置 hookID 404、未注册 404、
// 节点离线 503。
func TestIMRelayE2E_GroupHookWhitelist(t *testing.T) {
	hub := NewHub("", map[string]string{"tok-im": "node-im"}, "", nil, nil, nil)
	srv := NewServer(hub, "")
	addr := "127.0.0.1:18922"
	go func() { _ = srv.Start(addr) }()
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)
	base := "http://" + addr

	agentMsgs := make(chan Envelope, 32)
	agentWS := dialTestAgent(t, "ws://"+addr, "node-im", "tok-im", agentMsgs)
	defer agentWS.Close()

	// 未注册：404。
	if resp, err := http.Post(base+imRelayPathGroup+"nosuchfp/hook-a", "application/json", strings.NewReader(`{}`)); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未注册渠道应 404: %v %d", err, resp.StatusCode)
	}

	ch := IMRelayChannel{Name: "群渠道", GroupHookIDs: "hook-a,hook-b"}
	fp := imRelayChannelFP(ch.Name, ch.PublicID, ch.EID, ch.WebhookToken, ch.GroupHookIDs)
	agentWS.WriteJSON(NewEnvelope("", TypeAgentIMState, IMRelayState{Enabled: true, Channels: []IMRelayChannel{ch}}))
	deadline := time.Now().Add(2 * time.Second)
	for len(hub.IMRelayChannelList()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("中继渠道注册失败")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// GET 探活：200。
	if resp, err := http.Get(base + imRelayPathGroup + fp + "/hook-a"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET 探活应 200: %v %d", err, resp.StatusCode)
	}
	// 未配置 hookID：404（防未分配群组乱投）。
	if resp, err := http.Post(base+imRelayPathGroup+fp+"/hook-c", "application/json", strings.NewReader(`{}`)); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未配置 hookID 应 404: %v %d", err, resp.StatusCode)
	}
	// 已配置 hookID：转发到节点；受理应答 = chatbot 文档格式
	//（success=true + data.type=2 + 非空 content；chatbot 平台 3 秒窗）。
	resp, err := http.Post(base+imRelayPathGroup+fp+"/hook-a", "application/json", strings.NewReader(`{"text":"群消息"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("群消息回调应 200: %v %v", err, resp)
	}
	ackBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var ack imChatbotAckResp
	if err := json.Unmarshal(ackBody, &ack); err != nil || !ack.Success {
		t.Fatalf("群回调受理应答错误: %q err=%v", string(ackBody), err)
	}
	if ack.Data.Type != 2 || ack.Data.Content == "" {
		t.Fatalf("受理应答非 chatbot 文档格式（data.type=2+content 必填）: %q", string(ackBody))
	}
	e := waitEnvelope(t, agentMsgs, TypeServerIMInbound, 3*time.Second)
	var msg IMRelayInbound
	_ = e.Decode(&msg)
	if msg.Kind != "group" || msg.GroupHookID != "hook-a" || msg.GroupHooks != "hook-a,hook-b" {
		t.Fatalf("群消息载荷错误: %+v", msg)
	}

	// chatbot 平台探活测试请求（创建机器人时发起，robotId=test-robotId）：
	// 直接回文档示例文案，不下发节点（无 server.im_inbound 帧）。
	probeBody := `{"type":2,"robotId":"test-robotId","robotName":"test-robotName","operatorOpenid":"test-userId","operatorName":"test-userName","time":1599727083000,"msgId":"test-msgId","content":"你好，你能做什么呢?"}`
	probeResp, err := http.Post(base+imRelayPathGroup+fp+"/hook-a", "application/json", strings.NewReader(probeBody))
	if err != nil || probeResp.StatusCode != http.StatusOK {
		t.Fatalf("探活回调应 200: %v %v", err, probeResp)
	}
	probeAck, _ := io.ReadAll(probeResp.Body)
	probeResp.Body.Close()
	var probeRespAck imChatbotAckResp
	_ = json.Unmarshal(probeAck, &probeRespAck)
	if !probeRespAck.Success || probeRespAck.Data.Type != 2 || probeRespAck.Data.Content != "好风凭借力，送我上青云" {
		t.Fatalf("探活应答非文档示例格式: %q", string(probeAck))
	}
	select {
	case e := <-agentMsgs:
		t.Fatalf("探活请求不应下发节点: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}

	// 节点下线：路由摘除 → 503（平台超时重发语义兜底）。
	agentWS.Close()
	deadline = time.Now().Add(2 * time.Second)
	for {
		if _, _, online := hub.imRelay.Lookup(fp); !online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("节点下线后路由未摘除")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp, err := http.Post(base+imRelayPathGroup+fp+"/hook-a", "application/json", strings.NewReader(`{}`)); err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("节点离线应 503: %v %d", err, resp.StatusCode)
	}
}

// TestIMRelay_ChannelPersistAndReregister 注册表持久化与重连恢复：SetChannels
// 落盘 → 新 registry 加载（entries 恢复、nodeByFP 空）→ AttachNode 恢复路由。
func TestIMRelay_ChannelPersistAndReregister(t *testing.T) {
	dir := t.TempDir()
	r := newIMRelayRegistry(dir)
	ch := IMRelayChannel{Name: "持久化渠道", GroupHookIDs: "h1"}
	fp := imRelayChannelFP(ch.Name, ch.PublicID, ch.EID, ch.WebhookToken, ch.GroupHookIDs)
	r.SetChannels("node-p", []IMRelayChannel{ch})
	if len(r.List()) != 1 {
		t.Fatalf("注册失败: %v", r.List())
	}

	// 模拟服务重启：新 registry 加载 → 节点上线 AttachNode → 路由恢复。
	r2 := newIMRelayRegistry(dir)
	if len(r2.entries) != 1 {
		t.Fatalf("落盘恢复失败: %d", len(r2.entries))
	}
	if _, _, online := r2.Lookup(fp); online {
		t.Fatalf("节点未上线不应有路由")
	}
	r2.AttachNode("node-p")
	nodeID, _, ok := r2.Lookup(fp)
	if !ok || nodeID != "node-p" {
		t.Fatalf("重连路由恢复失败: %s %v", nodeID, ok)
	}

	// 注销：Enabled=false → 空表全量替换。
	r2.SetChannels("node-p", nil)
	if len(r2.entries) != 0 {
		t.Fatalf("注销失败: %d", len(r2.entries))
	}
}

// TestIMRelay_FailoverMultiOwner 多主故障转移（2026-10-11 线上「软件退出重开
// 后回调 node offline」修复）：同机多开读同一 channels.json，同一指纹被两个
// 节点先后注册（后者不顶替前者）；最后注册者下线后同指纹其他在线节点自动
// 接管（Lookup 返回在线节点），全部离线才 503。
func TestIMRelay_FailoverMultiOwner(t *testing.T) {
	r := newIMRelayRegistry("")
	ch := IMRelayChannel{Name: "多主渠道", GroupHookIDs: "h1"}
	fp := imRelayChannelFP(ch.Name, ch.PublicID, ch.EID, ch.WebhookToken, ch.GroupHookIDs)

	// 节点 A 注册 → 指纹路由到 A。
	r.SetChannels("node-a", []IMRelayChannel{ch})
	if nid, _, ok := r.Lookup(fp); !ok || nid != "node-a" {
		t.Fatalf("单节点路由应指向 node-a: %s %v", nid, ok)
	}

	// 节点 B 注册同指纹（同机多开同渠道）：不顶替 A 的 entry，但路由集含 B。
	r.SetChannels("node-b", []IMRelayChannel{ch})
	if nid, _, ok := r.Lookup(fp); !ok || nid != "node-a" {
		t.Fatalf("双节点在线应优先归属节点: %s %v", nid, ok)
	}

	// 归属节点 A 下线 → B 自动接管（核心修复：旧实现此处整体 503）。
	r.DetachNode("node-a")
	nid, _, ok := r.Lookup(fp)
	if !ok || nid != "node-b" {
		t.Fatalf("A 下线后 B 应接管路由: %s %v", nid, ok)
	}

	// B 重连 → 恢复；再全部下线 → 503 语义（无路由）。
	r.AttachNode("node-b")
	if nid, _, ok := r.Lookup(fp); !ok || nid != "node-b" {
		t.Fatalf("B 重连后路由应恢复: %s %v", nid, ok)
	}
	r.DetachNode("node-b")
	if _, _, ok := r.Lookup(fp); ok {
		t.Fatalf("全部离线应无路由（503）")
	}

	// 注销语义不回归：B 全量替换空表仅摘 B，A 的注册不受影响。
	r.SetChannels("node-a", []IMRelayChannel{ch})
	r.SetChannels("node-b", nil)
	if nid, _, ok := r.Lookup(fp); !ok || nid != "node-a" {
		t.Fatalf("B 注销后 A 应仍可达: %s %v", nid, ok)
	}
}

// TestIMRelay_ChannelReplace 幂等全量替换：同节点旧渠道（五元组变化 → 指纹
// 变化）自动失效，仅新指纹可达（渠道改名/改凭据后旧回调 URL 404）。
func TestIMRelay_ChannelReplace(t *testing.T) {
	r := newIMRelayRegistry("")
	old := IMRelayChannel{Name: "渠道A", PublicID: "P1"}
	fpOld := imRelayChannelFP("渠道A", "P1", "", "", "")
	r.SetChannels("node-r", []IMRelayChannel{old})
	if _, _, ok := r.Lookup(fpOld); !ok {
		t.Fatalf("旧指纹应可达")
	}
	// 改名 → 新指纹；旧指纹自动摘除。
	nu := IMRelayChannel{Name: "渠道A-新", PublicID: "P1"}
	fpNew := imRelayChannelFP("渠道A-新", "P1", "", "", "")
	r.SetChannels("node-r", []IMRelayChannel{nu})
	if _, _, ok := r.Lookup(fpOld); ok {
		t.Fatalf("旧指纹应失效（渠道配置已变更）")
	}
	if _, _, ok := r.Lookup(fpNew); !ok {
		t.Fatalf("新指纹应可达")
	}
}
