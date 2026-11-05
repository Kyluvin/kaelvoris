// acp_cancel_test.go —— 取消/超时后子进程树终止与资源回收验证：
//   - ctx 取消（用户停止）→ Start 后 Prompt 前的挂起 agent 被整树终止（Windows
//     taskkill /T：cmd.exe → node 孙进程全灭；POSIX 进程组 SIGKILL）
//   - Close 幂等（defer 与 ctx 取消并发触发 = sync.Once 不双杀）
//   - 无 goroutine 泄漏（Close 后读泵/消费循环退出）
package acp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hungAgentScript 挂起 agent：initialize/session/new 正常应答，session/prompt
// 永不回复（模拟 agent 僵死/长任务中用户点停止）。
const hungAgentScript = `const readline = require("readline");
const rl = readline.createInterface({ input: process.stdin });
function send(o) { process.stdout.write(JSON.stringify(o) + "\n"); }
rl.on("line", (line) => {
  line = line.trim();
  if (!line) return;
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  if (msg.method === "initialize") send({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: 1, agentInfo: { name: "hung-agent" } } });
  else if (msg.method === "session/new") send({ jsonrpc: "2.0", id: msg.id, result: { sessionId: "sess-hung" } });
  // session/prompt：永不回复，保持进程存活
});
setInterval(() => {}, 1000);
`

// pidAlive 进程是否存活：
//   - Windows: tasklist /FO CSV（PID 列精确匹配；"错误: 没有运行的任务"空输出）
//   - POSIX: signal 0
func pidAlive(pid int) bool {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/FO", "CSV", "/NH").Output()
		if err != nil {
			return false
		}
		// CSV 行形如 "node.exe","1234","Console","1","20,000 K"
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Split(line, `","`)
			if len(fields) >= 2 && strings.Trim(fields[1], `"`) == strconv.Itoa(pid) {
				return true
			}
		}
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(nil) == nil
}

// waitPidGone 进程在窗口期内退出（定时轮询；Process.Kill 后 OS 回收需时间）。
func waitPidGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !pidAlive(pid)
}

// countHungProcs 统计仍存活的 hung-agent node 进程数。
func countHungProcs() int {
	if runtime.GOOS != "windows" {
		out, err := exec.Command("pgrep", "-f", "hung-agent.js").Output()
		if err != nil {
			return 0
		}
		return len(strings.Fields(string(out)))
	}
	// Windows: 通过 wmic 查命令行（Go exec argv 传递无引号问题）。
	out, err := exec.Command("wmic", "process", "where", "name='node.exe'", "get", "ProcessId,CommandLine", "/format:csv").Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "hung-agent.js") {
			count++
		}
	}
	return count
}

// writeHung 落盘挂起 agent 脚本。
func writeHung(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不可用，跳过")
	}
	p := filepath.Join(t.TempDir(), "hung-agent.js")
	if err := os.WriteFile(p, []byte(hungAgentScript), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCancel_KillsProcessTree 核心验收：取消后 agent 子进程（含 cmd/node 孙进程）
// 无残留、无 goroutine 泄漏。
func TestCancel_KillsProcessTree(t *testing.T) {
	script := writeHung(t)
	cwd := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runtime.NumGoroutine()
	cli, err := Start(ctx, AgentConfig{Name: "hung", Command: "node", Args: []string{script}}, cwd, FsHooks{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cli.cmd.Process.Pid // Windows 直接子进程 = cmd.exe
	if _, err := cli.NewSession(ctx, cwd); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// 取消传播：ctx 取消 → CommandContext.cmd.Cancel（= killTree）→ 整树终止。
	cancel()
	cli.Close()
	if !waitPidGone(pid, 5*time.Second) {
		t.Fatalf("agent 直接子进程仍然存活: pid=%d", pid)
	}
	// 孙进程（Windows: node.exe）也不得残留。
	if count := countHungProcs(); count > 0 {
		t.Fatalf("agent 孙进程残留 %d 个", count)
	}
	// 读泵/消费 goroutine 随 doneCh 退出（允许短暂调度窗口）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > base+2 {
		time.Sleep(50 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+2 {
		t.Fatalf("goroutine 泄漏: base=%d now=%d", base, n)
	}
}

// TestClose_Idempotent ctx 取消与显式 Close 并发：不 panic、不双杀（sync.Once）。
func TestClose_Idempotent(t *testing.T) {
	script := writeHung(t)
	cwd := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cli, err := Start(ctx, AgentConfig{Name: "hung2", Command: "node", Args: []string{script}}, cwd, FsHooks{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cli.cmd.Process.Pid
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cli.Close()
		}()
	}
	cancel() // 与 Close 并发；Close 内部 sync.Once 幂等
	wg.Wait()
	if !waitPidGone(pid, 5*time.Second) {
		t.Fatalf("agent 子进程仍然存活: pid=%d", pid)
	}
}
