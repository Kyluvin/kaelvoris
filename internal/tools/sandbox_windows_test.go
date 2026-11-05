//go:build windows

// sandbox_windows_test.go —— 沙箱核心用例（Windows 实机）：
//   - job 整树终止：cmd /C 拉起孙子 ping 进程 → Kill() → 全家退出；
//   - Attach 失败优雅降级（Attach 已关闭 job → 报错，不 panic）；
//   - noop 执行器零副作用；
//   - limits 冒烟：MaxProcesses=1 时 cmd /C "ping 与 start ping" 派生失败。
package tools

import (
	"context"
	"bytes"
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// waitExit 用句柄等进程退出（不依赖 cmd.Wait 的管道语义）。
// 超时返回 false；句柄在 OpenProcess 失败（进程已死）时视为已退出。
func waitExit(t *testing.T, pid int, d time.Duration) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true // 打不开 = 已退出
	}
	defer windows.CloseHandle(h)
	event, _ := windows.WaitForSingleObject(h, uint32(d.Milliseconds()))
	return event == uint32(windows.WAIT_OBJECT_0)
}

// jobProcCount 用 QueryInformationJobObject(JobObjectBasicProcessIdList) 数 job 内进程。
func jobProcCount(t *testing.T, sb SandboxExecutor) int {
	t.Helper()
	js, ok := sb.(*jobSandbox)
	if !ok {
		t.Fatalf("want *jobSandbox, got %T", sb)
	}
	js.mu.Lock()
	job := js.job
	js.mu.Unlock()
	if job == 0 {
		return 0
	}
	// 先探长度（4 字节返回所需尺寸），再取全量。
	var dummy [64]byte
	var ret uint32
	_ = windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&dummy[0])), uint32(len(dummy)), &ret)
	if ret == 0 {
		return 0
	}
	buf := make([]byte, ret)
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf)), &ret); err != nil {
		return 0
	}
	// JOBOBJECT_BASIC_PROCESS_ID_LIST { DWORD NumberOfAssignedProcess;
	// DWORD NumberOfProcessIdsInList; ULONG_PTR ProcessIdList[]; }
	num := *(*uint32)(unsafe.Pointer(&buf[4]))
	return int(num)
}

// TestSandboxKillTree 核心：cmd /C 拉起 ping 孙进程 → Kill → 整树退出。
// 修复验证点：既有 Process.Kill() 只杀 cmd.exe，ping 会继续跑满 30 秒。
func TestSandboxKillTree(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	f := NewSandboxFactory(SandboxConfig{Mode: SandboxModeJob})
	sb := f.New()
	defer sb.Close()

	cmd := exec.Command("cmd", "/C", "ping -n 30 127.0.0.1 >nul")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := sb.Attach(cmd); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if n := jobProcCount(t, sb); n < 1 {
		t.Fatalf("job should contain >=1 process, got %d", n)
	}

	// 整树终止。
	if err := sb.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if waitExit(t, cmd.Process.Pid, 200*time.Millisecond) {
			// cmd.exe 已死；再确认 job 内进程清空（ping 也被终止）。
			if n := jobProcCount(t, sb); n != 0 {
				t.Fatalf("job still has %d process(es) after kill", n)
			}
			return
		}
	}
	t.Fatal("cmd.exe still alive 5s after Kill")
}

// TestSandboxAttachAfterClose 优雅降级路径：job 已 Close 后 Attach 报错
//（调用方 warn 一次继续裸跑，绝不 panic）。
func TestSandboxAttachAfterClose(t *testing.T) {
	f := NewSandboxFactory(SandboxConfig{Mode: SandboxModeJob})
	sb := f.New()
	if err := sb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	cmd := exec.Command("cmd", "/C", "echo ok")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cmd.Process.Kill()
	// Attach 必须返回 error（而非 panic / 假成功）。
	if err := sb.Attach(cmd); err == nil {
		t.Fatal("attach on closed job should fail")
	}
	// Close/Kill 幂等。
	if err := sb.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := sb.Kill(); err != nil {
		t.Fatalf("kill after close: %v", err)
	}
}

// TestSandboxNoopZeroSideEffect noop 执行器：全零副作用，命令照常跑完。
func TestSandboxNoopZeroSideEffect(t *testing.T) {
	f := NewSandboxFactory(SandboxConfig{Mode: SandboxModeOff})
	sb := f.New()
	cmd := exec.Command("cmd", "/C", "echo noop-ok")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := sb.Attach(cmd); err != nil {
		t.Fatalf("noop attach should always succeed, got %v", err)
	}
	if err := sb.Kill(); err != nil {
		t.Fatalf("noop kill: %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("noop close: %v", err)
	}
	if string(out) != "noop-ok\r\n" {
		t.Fatalf("unexpected output %q", out)
	}
}

// TestSandboxMaxProcessLimit limits 冒烟：MaxProcesses=1 时第二个进程无法入 job。
// cmd /C "ping & start ping"：start 派生第二个 ping → 入 job 失败 → start 报错。
func TestSandboxMaxProcessLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	f := NewSandboxFactory(SandboxConfig{Mode: SandboxModeJob, MaxProcesses: 1})
	sb := f.New()
	defer sb.Close()

	// 初始 shell 进程占掉唯一的名额；start 再拉进程会因 ActiveProcessLimit 拒绝。
	cmd := exec.Command("cmd", "/C", "start /B ping -n 30 127.0.0.1 >nul & echo DONE")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := sb.Attach(cmd); err != nil {
		t.Fatalf("attach: %v", err)
	}
	_ = cmd.Wait()
	// start 失败时 cmd 会打印错误但整体退出码仍为 0；断言输出不含 ping 窗口
	// 成功信息不可靠，改为行为断言：job 内进程数最终回落到 0（第二个进程
	// 未被允许常驻）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if jobProcCount(t, sb) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("job still holds processes after MaxProcesses=1 run: %d", jobProcCount(t, sb))
}

// TestShellToolSandboxTimeout shell 工具端到端：timeout_sec 生效 + 沙箱整树终止。
// ping -n 30 挂 30 秒，timeout_sec=2 应在 ~5 秒内返回。
func TestShellToolSandboxTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	tool := &ShellTool{Runner: &CommandRunner{
		Workspace:  ".",
		TimeoutSec: 60,
		DenyList:   DefaultDenyList,
		Sandbox:    NewSandboxFactory(SandboxConfig{Mode: SandboxModeJob}),
	}}
	start := time.Now()
	res, err := tool.Execute(context.Background(), `{"command":"ping -n 30 127.0.0.1","timeout_sec":2}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	el := time.Since(start)
	if el > 10*time.Second {
		t.Fatalf("timeout_sec=2 took %v, sandbox kill tree failed", el)
	}
	if res == "" {
		t.Fatal("empty result")
	}
}

// TestShellToolGBKDecoding GBK 兑底：cmd 输出 GBK 字节（中文系统代码页），
// ensureUTF8 应解码为合法中文。
func TestShellToolGBKDecoding(t *testing.T) {
	// "中文" 的 GBK 编码：D6 D0 CE C4
	gbk := string([]byte{0xD6, 0xD0, 0xCE, 0xC4})
	got := ensureUTF8(gbk)
	if got != "中文" {
		t.Fatalf("GBK decode mismatch: %q", got)
	}
	// 合法 UTF-8 原样通过。
	if ensureUTF8("already-utf8-中文") != "already-utf8-中文" {
		t.Fatal("valid utf8 must pass through unchanged")
	}
	// 二进制垃圾（GBK 也解不出）原样保留。
	junk := string([]byte{0xFF, 0xFE, 0x00, 0x81})
	if ensureUTF8(junk) != junk {
		t.Fatal("undecodable bytes must pass through unchanged")
	}
}

// TestShellToolTimeoutParam schema/接线冒烟：timeout_sec 走 Runner 兑底路径
//（Runner.TimeoutSec=1 + 命令挂 5 秒 → ~1 秒返回）。
func TestShellToolTimeoutParamFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	tool := &ShellTool{Runner: &CommandRunner{Workspace: ".", TimeoutSec: 1}}
	start := time.Now()
	if _, err := tool.Execute(context.Background(), `{"command":"ping -n 10 127.0.0.1"}`); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if el := time.Since(start); el > 8*time.Second {
		t.Fatalf("runner timeout=1 took %v", el)
	}
}
