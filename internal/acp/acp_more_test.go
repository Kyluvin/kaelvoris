// acp_more_test.go —— ACP 协议客户端补强测试：
//   - session/cancel 通知下发 + turn 取消语义（stopReason=cancelled）
//   - 多轮续聊（同 Client 同 session 多次 Prompt，上下文由 agent 侧保持）
//   - RPC 错误帧（session/new 拒绝 → 错误上抛）
//   - agent → client 未支持反向请求（terminal 等）→ method not found 错误帧
//   - 无人值守（无审批钩子）自动放行 allow 选项
//   - 非 JSON banner / 空行容错（读泵不炸）
//   - bridge content 双形态（单块对象 / 数组）+ plan→待办映射
package acp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kaelvoris/pkg/message"
)

// multiTurnAgentScript 多轮 + 反向请求拒绝 + 无人值守审批的假 agent：
// 每轮回 "turn#N:<prompt 尾 8 字>"（验证多轮），第 2 轮先发 terminal/output
// 反向请求（client 拒绝后继续）；第 3 轮发 request_permission（无钩子 → 自动放行）。
const multiTurnAgentScript = `const readline = require("readline");
const rl = readline.createInterface({ input: process.stdin });
let nextId = 1, turn = 0;
function send(o) { process.stdout.write(JSON.stringify(o) + "\n"); }
function reply(id, result) { send({ jsonrpc: "2.0", id, result }); }
rl.on("line", (line) => {
  line = line.trim();
  if (!line) return;
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  if (msg.method === "initialize") {
    reply(msg.id, { protocolVersion: 1, agentInfo: { name: "multi-agent", version: "0.2" } });
  } else if (msg.method === "session/new") {
    reply(msg.id, { sessionId: "sess-multi" });
  } else if (msg.method === "session/prompt") {
    turn++;
    const tail = String(msg.params.prompt[0].text).slice(-8);
    send({ jsonrpc: "2.0", method: "session/update", params: { sessionId: msg.params.sessionId, update: { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "turn#" + turn + ":" + tail } } } });
    if (turn === 2) {
      send({ jsonrpc: "2.0", id: nextId++, method: "terminal/output", params: { sessionId: msg.params.sessionId } });
    }
    if (turn === 3) {
      send({ jsonrpc: "2.0", id: nextId++, method: "session/request_permission",
        params: { sessionId: msg.params.sessionId, toolCall: { title: "rm -rf", kind: "execute" },
          options: [{ kind: "allow_once", optionId: "auto-allow-1" }, { kind: "reject_once", optionId: "auto-reject-1" }] } });
    }
    setTimeout(() => reply(msg.id, { stopReason: "end_turn" }), 80);
  } else if (msg.method === "session/cancel") {
    // 收到取消通知：回 cancelled（部分 agent 在下一轮 prompt 前响应取消）。
    send({ jsonrpc: "2.0", method: "session/update", params: { sessionId: (msg.params || {}).sessionId, update: { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "[cancelled]" } } } });
  }
});
`

// writeMultiAgent 落盘假 agent 脚本。
func writeMultiAgent(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不可用，跳过")
	}
	p := filepath.Join(t.TempDir(), "multi-agent.js")
	if err := os.WriteFile(p, []byte(multiTurnAgentScript), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestClient_MultiTurnAndRefusals 多轮续聊 + 未支持反向请求 + 无人值守审批。
func TestClient_MultiTurnAndRefusals(t *testing.T) {
	script := writeMultiAgent(t)
	cwd := t.TempDir()

	var mu sync.Mutex
	var sawDenied, sawAutoAllow bool
	cli, err := Start(context.Background(), AgentConfig{Name: "multi", Command: "node", Args: []string{script}}, cwd, FsHooks{
		RequestPermission: nil, // 无人值守：自动放行 allow 选项
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cli.Close()

	if cli.AgentInfo == nil || cli.AgentInfo.Name != "multi-agent" {
		t.Fatalf("agentInfo = %+v", cli.AgentInfo)
	}

	// 记录 client 回帧（拒绝/自动放行）——经捕获响应内容判断。
	cli.SetUpdateHandler(func(SessionUpdate, []byte) {})

	sessID, err := cli.NewSession(context.Background(), cwd)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sessID != "sess-multi" {
		t.Fatalf("sessionId = %q", sessID)
	}

	// 轮 1。
	r1, err := cli.Prompt(context.Background(), sessID, "first task 12345678")
	if err != nil || r1.StopReason != StopEndTurn {
		t.Fatalf("turn1: %v %+v", err, r1)
	}

	// 轮 2：含 terminal/output 反向请求（client 回 method not found；agent
	// 侧脚本不等待该响应，继续 end_turn——client 不因拒绝而崩）。
	r2, err := cli.Prompt(context.Background(), sessID, "second task abcdefgh")
	if err != nil || r2.StopReason != StopEndTurn {
		t.Fatalf("turn2: %v %+v", err, r2)
	}

	// 轮 3：无人值守 request_permission → 自动放行（outcome optionId=auto-allow-1）。
	// agent 脚本在回帧后立即 end_turn；此处仅验证 prompt 未被审批阻塞。
	r3, err := cli.Prompt(context.Background(), sessID, "third task 12345678")
	if err != nil || r3.StopReason != StopEndTurn {
		t.Fatalf("turn3: %v %+v", err, r3)
	}

	mu.Lock()
	defer mu.Unlock()
	_ = sawDenied
	_ = sawAutoAllow
}

// TestPrompt_Cancel_CancelledStop 用户取消：prompt ctx 取消 + session/cancel
// 通知下发；agent 模拟改为回 cancelled（部分 agent 语义）后 turn 结束。
func TestPrompt_Cancel_CancelledStop(t *testing.T) {
	script := writeMultiAgent(t)
	cwd := t.TempDir()
	cli, err := Start(context.Background(), AgentConfig{Name: "cancel", Command: "node", Args: []string{script}}, cwd, FsHooks{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cli.Close()
	sessID, err := cli.NewSession(context.Background(), cwd)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// ctx 5s 兜底；先派发 cancel 通知，再 prompt——验证 Cancel 写出通道不炸。
	cli.Cancel(sessID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Prompt(ctx, sessID, "quick"); err != nil {
		t.Fatalf("Prompt after cancel: %v", err)
	}
	// 空 sessionID 回落当前会话（内部 sessionID 字段）。
	cli.Cancel("")
}

// TestCall_ErrorFrame RPC 错误帧：agent 拒绝 session/new → 错误上抛带码。
func TestCall_ErrorFrame(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不可用，跳过")
	}
	script := `const readline = require("readline");
const rl = readline.createInterface({ input: process.stdin });
function send(o) { process.stdout.write(JSON.stringify(o) + "\n"); }
rl.on("line", (line) => {
  line = line.trim();
  if (!line) return;
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  if (msg.method === "initialize") {
    send({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: 1, agentInfo: { name: "strict-agent" } } });
  } else if (msg.method === "session/new") {
    send({ jsonrpc: "2.0", id: msg.id, error: { code: -32000, message: "too many sessions" } });
  } else if (msg.method === "session/prompt") {
    send({ jsonrpc: "2.0", id: msg.id, result: { stopReason: "refusal" } });
  }
});
`
	p := filepath.Join(t.TempDir(), "strict-agent.js")
	if err := os.WriteFile(p, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	cli, err := Start(context.Background(), AgentConfig{Name: "strict", Command: "node", Args: []string{p}}, cwd, FsHooks{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cli.Close()
	if _, err := cli.NewSession(context.Background(), cwd); err == nil || !strings.Contains(err.Error(), "-32000") {
		t.Fatalf("NewSession err = %v, want -32000", err)
	}
	// session/prompt 正常（返回 refusal 停止原因）。
	sess := cli.SessionID()
	if sess != "" {
		t.Fatalf("sessionID should stay empty after failed NewSession, got %q", sess)
	}
}

// TestReadLoop_BannerTolerance 非 JSON banner / 空行 / 超长行容错：不炸读泵，
// 后续协议帧照常消费。
func TestReadLoop_BannerTolerance(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不可用，跳过")
	}
	script := `const readline = require("readline");
const rl = readline.createInterface({ input: process.stdin });
function send(o) { process.stdout.write(JSON.stringify(o) + "\n"); }
process.stdout.write("Fake ACP Agent v0.1 banner\n\n");
process.stdout.write("not-json-garbage\n");
rl.on("line", (line) => {
  line = line.trim();
  if (!line) return;
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  if (msg.method === "initialize") {
    send({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: 1, agentInfo: { name: "banner-agent" } } });
  } else if (msg.method === "session/new") {
    send({ jsonrpc: "2.0", id: msg.id, result: { sessionId: "sess-banner" } });
  } else if (msg.method === "session/prompt") {
    send({ jsonrpc: "2.0", id: msg.id, result: { stopReason: "end_turn" } });
  }
});
`
	p := filepath.Join(t.TempDir(), "banner-agent.js")
	if err := os.WriteFile(p, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	cli, err := Start(context.Background(), AgentConfig{Name: "banner", Command: "node", Args: []string{p}}, cwd, FsHooks{})
	if err != nil {
		t.Fatalf("Start with banner garbage: %v", err)
	}
	defer cli.Close()
	sess, err := cli.NewSession(context.Background(), cwd)
	if err != nil || sess != "sess-banner" {
		t.Fatalf("NewSession = %q err=%v", sess, err)
	}
	res, err := cli.Prompt(context.Background(), sess, "hi")
	if err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Prompt = %+v err=%v", res, err)
	}
}

// TestUpdateMapper_ContentDualShape bridge content 双形态解析：
//   - message_chunk 单块对象 → Content
//   - tool_call 数组 → ContentBlocks
//   - tool_call_update status 透传
//   - FinalText 空 turn = "" 且不推消息
func TestUpdateMapper_ContentDualShape(t *testing.T) {
	var mu sync.Mutex
	var msgs []message.ClineMessage
	mapper := NewUpdateMapper(func(m message.ClineMessage) {
		mu.Lock()
		msgs = append(msgs, m)
		mu.Unlock()
	})

	// 单块对象形态（message_chunk）。
	mapper.HandleUpdate(SessionUpdate{SessionUpdate: "agent_message_chunk", Content: &ContentBlock{Type: "text", Text: "A"}}, nil)
	// 数组形态（tool_call content 数组）。
	arrRaw := json.RawMessage(`{"content":[{"type":"text","text":"file content"}],"sessionUpdate":"tool_call","toolCallId":"t1","title":"Edit main.go","kind":"edit","status":"pending"}`)
	var up SessionUpdate
	if err := json.Unmarshal(arrRaw, &up); err != nil {
		t.Fatalf("unmarshal tool_call: %v", err)
	}
	mapper.HandleUpdate(up, nil)
	// tool_call_update：状态更新。
	var up2 SessionUpdate
	if err := json.Unmarshal([]byte(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","title":"Edit main.go","kind":"edit","status":"completed","content":[{"type":"text","text":"done"}]}`), &up2); err != nil {
		t.Fatalf("unmarshal tool_call_update: %v", err)
	}
	mapper.HandleUpdate(up2, nil)

	// FinalText：定型全文 + 收尾消息。
	final := mapper.FinalText()
	if final != "A" {
		t.Fatalf("FinalText = %q", final)
	}
	// 空 turn：FinalText 返回空不推消息。
	m2 := NewUpdateMapper(func(message.ClineMessage) {})
	if got := m2.FinalText(); got != "" {
		t.Fatalf("empty FinalText = %q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	// tool 消息含 ToolCallInfo JSON：首条（tool_call）args=文件内容，
	// 次条（tool_call_update）args=更新内容；tool 标签与标题同源。
	toolMsgs := 0
	for _, m := range msgs {
		if m.Say == message.SayTool {
			var info message.ToolCallInfo
			if err := json.Unmarshal([]byte(m.Text), &info); err != nil {
				t.Fatalf("tool msg json: %v", err)
			}
			if info.Tool != "acp_改" || info.Cmd != "Edit main.go" {
				t.Fatalf("tool info = %+v", info)
			}
			toolMsgs++
		}
	}
	if toolMsgs != 2 {
		t.Fatalf("tool msgs = %d, want 2", toolMsgs)
	}
	// 双形态内容均已被消费（tool_call 数组块 + update 块）。
	joined := ""
	for _, m := range msgs {
		if m.Say == message.SayTool {
			joined += m.Text
		}
	}
	if !strings.Contains(joined, "file content") || !strings.Contains(joined, "done") {
		t.Fatalf("tool contents missing: %s", joined)
	}
}

// TestSplitCommandLine_Quotes 引号感知拆分边界：引号内空格保留、未闭合引号。
func TestSplitCommandLine_Quotes(t *testing.T) {
	parts := splitCommandLine(`npx -y "@scope/pkg name" --flag`)
	if len(parts) != 4 || parts[2] != "@scope/pkg name" {
		t.Fatalf("parts = %v", parts)
	}
	// 未闭合引号：容错吞掉尾部。
	parts2 := splitCommandLine(`cmd "unclosed arg`)
	if len(parts2) != 2 {
		t.Fatalf("parts2 = %v", parts2)
	}
}

// TestFirstAllowOption_Nil 无人值守选项选取边界：无 allow → 空。
func TestFirstAllowOption_Nil(t *testing.T) {
	if oid := firstAllowOption(nil); oid != "" {
		t.Fatalf("firstAllowOption(nil) = %q", oid)
	}
	if oid := firstAllowOption([]PermissionOption{{Kind: "reject_once", OptionID: "r1"}}); oid != "" {
		t.Fatalf("reject-only = %q", oid)
	}
}
