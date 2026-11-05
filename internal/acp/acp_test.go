// acp_test.go —— ACP 客户端端到端测试：node 子进程模拟 ACP agent，
// 验证 initialize 握手 / session/new / session/prompt / session/update
// 流式映射 / fs 反向请求 / 权限审批 / stopReason 全链路。
package acp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kaelvoris/pkg/message"
)

// fakeAgentScript 假 ACP agent（node）：完整实现 client 侧会触发的协议面。
//   - initialize → agentInfo
//   - session/new → sessionId
//   - session/prompt → 思考/正文 chunk ×2 → tool_call → tool_call_update →
//     fs/write_text_file 反向请求 → session/request_permission 反向请求 → end_turn
const fakeAgentScript = `const readline = require("readline");
const rl = readline.createInterface({ input: process.stdin });
let nextId = 1;
function send(o) { process.stdout.write(JSON.stringify(o) + "\n"); }
function reply(id, result) { send({ jsonrpc: "2.0", id, result }); }
function notifyUpdate(sid, update) { send({ jsonrpc: "2.0", method: "session/update", params: { sessionId: sid, update } }); }
rl.on("line", (line) => {
  line = line.trim();
  if (!line) return;
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  if (msg.method === "initialize") {
    reply(msg.id, { protocolVersion: 1, agentInfo: { name: "fake-agent", version: "0.1" } });
  } else if (msg.method === "session/new") {
    reply(msg.id, { sessionId: "sess-test-1" });
  } else if (msg.method === "session/prompt") {
    const sid = msg.params.sessionId;
    notifyUpdate(sid, { sessionUpdate: "agent_thought_chunk", content: { type: "text", text: "思考中" } });
    notifyUpdate(sid, { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "你好" } });
    notifyUpdate(sid, { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "，世界" } });
    notifyUpdate(sid, { sessionUpdate: "tool_call", toolCallId: "tc1", title: "读取 main.go", kind: "read", status: "pending" });
    notifyUpdate(sid, { sessionUpdate: "tool_call_update", toolCallId: "tc1", title: "读取 main.go", kind: "read", status: "completed", content: [{ type: "text", text: "文件内容摘要" }] });
    send({ jsonrpc: "2.0", id: nextId++, method: "fs/write_text_file", params: { sessionId: sid, path: "hello.txt", content: "ACP 你好" } });
    send({ jsonrpc: "2.0", id: nextId++, method: "session/request_permission",
      params: { sessionId: sid, toolCall: { title: "执行 git push", kind: "execute" },
        options: [{ kind: "allow_once", optionId: "allow-1" }, { kind: "reject_once", optionId: "reject-1" }] } });
    setTimeout(() => reply(msg.id, { stopReason: "end_turn" }), 250);
  }
});
`

// TestRunTurn_E2E 端到端全链路。
func TestRunTurn_E2E(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不可用，跳过")
	}
	script := filepath.Join(t.TempDir(), "fake-acp-agent.js")
	if err := os.WriteFile(script, []byte(fakeAgentScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()

	var (
		mu         sync.Mutex
		toolTitles []string
		wrote      string
		askedPerm  string
		thoughts   int
	)

	// 流式映射：chunk → ClineMessage partial 累计（onSay 收集断言）。
	var says []string
	var sayMu sync.Mutex
	mapper := NewUpdateMapper(func(m message.ClineMessage) {
		sayMu.Lock()
		says = append(says, fmt.Sprintf("%v|%v|partial=%v", m.Say, m.Text, m.Partial))
		sayMu.Unlock()
	})
	cfg := AgentConfig{
		Name:    "fake",
		Command: "node",
		Args:    []string{script},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, err := Start(ctx, cfg, cwd, FsHooks{
		ReadTextFile: func(path string, line, limit int) (string, error) {
			return "file-content", nil
		},
		WriteTextFile: func(path, content string) error {
			mu.Lock()
			wrote = filepath.Base(path) + "|" + content
			mu.Unlock()
			return nil
		},
		RequestPermission: func(sessionID string, p *PermissionRequestPayload) (string, error) {
			mu.Lock()
			askedPerm = toolCallTitle(p.ToolCall)
			mu.Unlock()
			if strings.Contains(askedPerm, "git push") {
				return "allow-1", nil
			}
			return "reject-1", nil
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cli.Close()

	if cli.AgentInfo == nil || cli.AgentInfo.Name != "fake-agent" {
		t.Fatalf("initialize agentInfo = %+v, want fake-agent", cli.AgentInfo)
	}

	// session/update 流式消费验证。
	var streamMu sync.Mutex
	cli.SetUpdateHandler(func(up SessionUpdate, raw []byte) {
		streamMu.Lock()
		defer streamMu.Unlock()
		switch up.SessionUpdate {
		case "agent_thought_chunk":
			thoughts++
		case "tool_call", "tool_call_update":
			toolTitles = append(toolTitles, up.Title)
		}
		mapper.HandleUpdate(up, raw)
	})

	sessID, err := cli.NewSession(ctx, cwd)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sessID != "sess-test-1" {
		t.Fatalf("sessionId = %q", sessID)
	}

	res, err := cli.Prompt(ctx, sessID, "测试任务")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.StopReason != StopEndTurn {
		t.Fatalf("stopReason = %q, want end_turn", res.StopReason)
	}

	// 断言：流式 chunk 累计全文。
	final := mapper.FinalText()
	if final != "你好，世界" {
		t.Fatalf("FinalText = %q, want 你好，世界", final)
	}
	// say 流含 partial 正文与定型全文（AppendMerged 消费侧语义）。
	sayMu.Lock()
	finalSays := 0
	for _, s := range says {
		if strings.HasSuffix(s, "partial=false") && strings.Contains(s, "你好，世界") {
			finalSays++
		}
	}
	sayMu.Unlock()
	if finalSays != 1 {
		t.Fatalf("final says = %d, want 1 (says=%v)", finalSays, says)
	}
	// 工具卡 ×2（tool_call + tool_call_update 同标题）。
	if len(toolTitles) != 2 || toolTitles[0] != "读取 main.go" {
		t.Fatalf("toolTitles = %v", toolTitles)
	}
	// 反向 fs/write 请求已执行。
	mu.Lock()
	w := wrote
	p := askedPerm
	mu.Unlock()
	if w != "hello.txt|ACP 你好" {
		t.Fatalf("write hook = %q", w)
	}
	if !strings.Contains(p, "git push") {
		t.Fatalf("permission request title = %q", p)
	}
	// 思考 chunk 已消费。
	if thoughts < 1 {
		t.Fatalf("thoughts = %d, want >=1", thoughts)
	}
}

// TestStart_BadCommand spawn 失败路径：命令不存在 → 握手超时/启动错误。
func TestStart_BadCommand(t *testing.T) {
	cfg := AgentConfig{Name: "nope", Command: "definitely-not-exist-cmd-xyz"}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if _, err := Start(ctx, cfg, "", FsHooks{}); err == nil {
		t.Fatal("expect error for bad command")
	}
}

// TestAgentCommand_整行拆分 命令解析：整行 npx 命令拆分 + Windows cmd /C 包装。
func TestAgentCommand_SplitLine(t *testing.T) {
	cfg := AgentConfig{Name: "cc", Command: `npx -y @zed-industries/claude-code-acp`}
	cmd := agentCommand(cfg)
	if cmd == nil {
		t.Fatal("cmd nil")
	}
	if len(cmd.Args) < 2 {
		t.Fatalf("args = %v", cmd.Args)
	}
	// Windows: cmd /C "npx -y ..."; 其他: npx 直接作 argv[0]。
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "claude-code-acp") {
		t.Fatalf("args lost command: %v", cmd.Args)
	}
}
