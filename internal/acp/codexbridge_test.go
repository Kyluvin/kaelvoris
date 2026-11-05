// codexbridge_test.go —— Codex app-server 适配器端到端测试：
// node 脚本模拟 codex app-server（无 jsonrpc 字段帧），验证
// initialize 握手 / thread/start / turn/start 异步 / agent_message.delta
// 流式 / item 工具卡 / turn/completed 完成信号 / turn/interrupt 中断。
package acp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// fakeCodexServerScript 假 codex app-server（node）：
// 无 jsonrpc 字段；initialize → userAgent；thread/start → {thread:{id}}；
// turn/start → {turn:{id,status:inProgress}} + 通知流（delta/工具卡）+
// 最终 turn/completed；turn/interrupt → 回 turn/completed(interrupted)。
// item 结构按 v2 schema：type=agentMessage/commandExecution。
const fakeCodexServerScript = `const readline = require("readline");
const rl = readline.createInterface({ input: process.stdin });
let nextId = 1;
function send(o) { process.stdout.write(JSON.stringify(o) + "\n"); }
function reply(id, result) { send({ id, result }); }
function notify(method, params) { send({ method, params }); }
rl.on("line", (line) => {
  line = line.trim();
  if (!line) return;
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  if (msg.method === "initialize") {
    reply(msg.id, { userAgent: "codex/0.66.0" });
  } else if (msg.method === "thread/start") {
    reply(msg.id, { thread: { id: "thread-abc-1" }, cwd: msg.params.cwd });
  } else if (msg.method === "turn/start") {
    const threadId = msg.params.threadId;
    const turnId = "turn-xyz-1";
    notify("turn/started", { threadId, turn: { id: turnId, status: "inProgress" } });
    notify("item/agentMessage/delta", { threadId, turnId, itemId: "item-1", delta: "你好" });
    notify("item/agentMessage/delta", { threadId, turnId, itemId: "item-1", delta: "，Codex" });
    notify("item/reasoning/textDelta", { threadId, turnId, itemId: "item-r1", delta: "思考" });
    notify("item/started", { threadId, turnId, item: { type: "commandExecution", id: "ce-1", command: "git status", status: "inProgress" } });
    setTimeout(() => {
      notify("item/completed", { threadId, turnId, item: { type: "commandExecution", id: "ce-1", command: "git status", status: "completed", exitCode: 0, aggregatedOutput: "ok" } });
      notify("turn/completed", { threadId, turn: { id: turnId, status: "completed" } });
    }, 250);
    reply(msg.id, { turn: { id: turnId, status: "inProgress" } });
  } else if (msg.method === "turn/interrupt") {
    const turnId = msg.params.turnId;
    notify("turn/completed", { threadId: msg.params.threadId, turn: { id: turnId, status: "interrupted" } });
  }
});
`

// TestCodexStartThreadTurn 握手 + 会话 + turn 全链路。
func TestCodexStartThreadTurn(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不可用，跳过")
	}
	script := filepath.Join(t.TempDir(), "fake-codex-server.js")
	if err := os.WriteFile(script, []byte(fakeCodexServerScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cli, err := StartCodex(ctx, AgentConfig{Name: "codex", Command: "node", Args: []string{script}}, cwd, FsHooks{})
	if err != nil {
		t.Fatalf("StartCodex: %v", err)
	}
	defer cli.Close()

	if cli.AgentInfo == nil || cli.AgentInfo.Name == "" {
		t.Fatalf("AgentInfo = %+v, want 非空", cli.AgentInfo)
	}

	threadID, err := cli.NewThread(ctx, cwd)
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	if threadID != "thread-abc-1" {
		t.Fatalf("threadID = %q, want thread-abc-1", threadID)
	}

	// 流式收集（通知回调）。
	var deltas []string
	var tools []string
	cli.SetUpdateHandler(func(n CodexNotification) {
		switch n.Method {
		case "item/agentMessage/delta":
			var p struct {
				Delta string `json:"delta"`
			}
			if jsonUnmarshal(n.Params, &p) == nil {
				deltas = append(deltas, p.Delta)
			}
		case "item/completed":
			var p struct {
				Item codexThreadItem `json:"item"`
			}
			if jsonUnmarshal(n.Params, &p) == nil {
				tools = append(tools, p.Item.Command)
			}
		}
	})

	turnID, err := cli.StartTurn(ctx, threadID, "你好")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if turnID != "turn-xyz-1" {
		t.Fatalf("turnID = %q, want turn-xyz-1", turnID)
	}

	turn, err := cli.WaitTurn(ctx)
	if err != nil {
		t.Fatalf("WaitTurn: %v", err)
	}
	if turn.Status != codexTurnCompleted {
		t.Fatalf("turn status = %q, want completed", turn.Status)
	}

	if len(deltas) != 2 || deltas[0] != "你好" || deltas[1] != "，Codex" {
		t.Fatalf("deltas = %v, want [你好 ，Codex]", deltas)
	}
	if len(tools) != 1 || tools[0] != "git status" {
		t.Fatalf("tools = %v, want [git status]", tools)
	}
}

// TestCodexInterrupt 中断：turn/interrupt → turn/completed(interrupted)。
func TestCodexInterrupt(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不可用，跳过")
	}
	script := filepath.Join(t.TempDir(), "fake-codex-server.js")
	if err := os.WriteFile(script, []byte(fakeCodexServerScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cli, err := StartCodex(ctx, AgentConfig{Name: "codex", Command: "node", Args: []string{script}}, cwd, FsHooks{})
	if err != nil {
		t.Fatalf("StartCodex: %v", err)
	}
	defer cli.Close()

	threadID, err := cli.NewThread(ctx, cwd)
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	turnID, err := cli.StartTurn(ctx, threadID, "跑个测试")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	cli.Interrupt(threadID, turnID)
	turn, err := cli.WaitTurn(ctx)
	if err != nil {
		t.Fatalf("WaitTurn: %v", err)
	}
	if turn.Status != codexTurnInterrupted {
		t.Fatalf("turn status = %q, want interrupted", turn.Status)
	}
}

// jsonUnmarshal 测试辅助（避免与包内 json 名冲突）。
func jsonUnmarshal(b []byte, v interface{}) error { return json.Unmarshal(b, v) }
