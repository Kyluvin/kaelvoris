//go:build windows

package sysproc

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// preserveCmdLine 单测：cmd /C|/K 命中透传、其他 exe 与非 /C /K 不命中、
// 已指定 CmdLine 不覆盖、Args 不足不动（2026-10-10 第 25 轮自检）。
func TestPreserveCmdLine(t *testing.T) {
	cases := []struct {
		name string
		args []string
		path string
		want string // 期望 CmdLine；"" 表示不应设置
	}{
		{
			name: "cmdC compound passes through",
			args: []string{"cmd", "/C", `cd /d D:\ws && powershell -NoProfile -Command "Get-Date -Format 'yyyy-MM-dd'"`},
			want: `C:\Windows\system32\cmd.exe /C cd /d D:\ws && powershell -NoProfile -Command "Get-Date -Format 'yyyy-MM-dd'"`,
		},
		{
			name: "cmdK passes through",
			args: []string{"cmd.exe", "/K", "echo hi"},
			want: `C:\Windows\system32\cmd.exe /K echo hi`,
		},
		{
			name: "git argv direct untouched",
			args: []string{"git", "status", "--short"},
			want: "",
		},
		{
			name: "non-cmd-flag untouched",
			args: []string{"cmd", "/T", "foo"},
			want: "",
		},
		{
			name: "too few args untouched",
			args: []string{"cmd", "/C"},
			want: "",
		},
	}
	for _, tc := range cases {
		c := exec.Command(tc.args[0], tc.args[1:]...)
		c.SysProcAttr = &syscall.SysProcAttr{}
		preserveCmdLine(c)
		got := c.SysProcAttr.CmdLine
		if tc.want == "" {
			if got != "" {
				t.Errorf("%s: CmdLine should stay empty, got %q", tc.name, got)
			}
			continue
		}
		if !strings.HasSuffix(got, tc.want) || !strings.HasPrefix(got, `C:\Windows\system32\`) {
			// cmd.Path 可能是短路径或大小写差异，只校验尾部核心形态
			if !strings.HasSuffix(got, tc.want) {
				t.Errorf("%s: CmdLine = %q, want suffix %q", tc.name, got, tc.want)
			}
		}
	}
}

// TestPreserveCmdLineEndToEnd 端到端：cmd /C 复合命令 + 双引号 -Command，
// 修复前 PowerShell 把 \" 当字面引号回显原文；修复后应执行并输出标记。
func TestPreserveCmdLineEndToEnd(t *testing.T) {
	// 环境守卫：部分机器 SRP/杀软拦截 powershell.exe 执行（Access is denied），
	// 跳过 e2e 冒烟而非误报。探针与实际执行同路径（cmd /C 中转；本机实测直接
	// 启动可行但 cmd 子进程链被拦，探针不同路径会漏检致 FAIL）。
	if probe := exec.Command("cmd", "/C", "powershell -NoProfile -Command exit"); probe.Run() != nil {
		t.Skip("本机 cmd /C 启动 powershell 被环境策略拦截（SRP/杀软），跳过 e2e 冒烟")
	}

	c := exec.Command("cmd", "/C", `powershell -NoProfile -Command "Write-Output PRESERVE_E2E_OK"`)
	Hide(c) // 公共入口（内含 preserveCmdLine，shell 工具复合命令路径依赖它）
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("e2e: %v, out=%q", err, string(out))
	}
	if !strings.Contains(string(out), "PRESERVE_E2E_OK") {
		t.Fatalf("e2e output should contain marker, got %q", string(out))
	}
	// 字面量回显的回归特征：输出等于脚本原文（带引号剥离后的字面量）。
	if strings.Contains(string(out), `Write-Output PRESERVE_E2E_OK`) && !strings.Contains(string(out), "PRESERVE_E2E_OK\n") {
		t.Fatalf("e2e output looks like literal echo, got %q", string(out))
	}
}
