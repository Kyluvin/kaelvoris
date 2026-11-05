package tools

import (
	"reflect"
	"testing"
)

// TestIsPurePowerShellCommand —— PowerShell $ 吞失修复（2026-09-27 复盘）判定单测。
// 命中「纯 PowerShell 调用」返回 argv 以便参数化直跑，让 -Command 的脚本串
// （含 $ 变量、$env:、引号）原样到达 PowerShell，不被 cmd /C 中间层吞失。
func TestIsPurePowerShellCommand(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    []string
	}{
		{"双引号 -Command 含 $ 变量", `powershell -Command "Write-Output $PID"`,
			[]string{"powershell", "-Command", "Write-Output $PID"}},
		{"exe 后缀与大小写混合", `PowerShell.EXE -Command "$env:PATH"`,
			[]string{"PowerShell.EXE", "-Command", "$env:PATH"}},
		{"pwsh 变体且引号内管道", `pwsh -NoProfile -Command "Get-Process | Select-Object -First 1"`,
			[]string{"pwsh", "-NoProfile", "-Command", "Get-Process | Select-Object -First 1"}},
		{"File 参数形态", `powershell -NoProfile -File setup/_probe.ps1`,
			[]string{"powershell", "-NoProfile", "-File", "setup/_probe.ps1"}},
		{"powershell 前缀不再误判为非 git 场景的遗漏", `powershell -Command "git st"`,
			[]string{"powershell", "-Command", "git st"}},
		{"单引号脚本串", `powershell -Command 'Write-Output $PID'`,
			[]string{"powershell", "-Command", "Write-Output $PID"}},
	}
	for _, c := range cases {
		if got := isPurePowerShellCommand(c.command); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: isPurePowerShellCommand(%q) = %v, want %v", c.name, c.command, got, c.want)
		}
	}
}

// TestIsPurePowerShellCommandFallback —— 异常形态必须返回 nil，
// 维持原 cmd /C 路径，绝不扩大失败面（与 git argv 化修复同一契约）。
func TestIsPurePowerShellCommandFallback(t *testing.T) {
	nonPS := []string{
		`powershell`, // 光杆调用会进入交互模式挂住
		`powershell -Command "echo hi" | findstr hi`,      // 引号外管道
		`powershell -Command "unclosed`,                   // 未闭合引号
		"echo hi && powershell -Command \"x\"",            // 引号外 && 拼接
		"powershell -Command \"Write-Output $PID\"\nmore", // 引号外换行
		`powershell_ise -Command "x"`,                     // 首词并非 powershell/pwsh
		`Get-Process`,                                     // 裸 PS 命令，不是 powershell 调用
		`powershell -Command "echo 123" & type b`,         // 引号外 &
		`cmd /C powershell -Command "x"`,                  // 已带 cmd 前缀，交还原路径
	}
	for _, cmd := range nonPS {
		if got := isPurePowerShellCommand(cmd); got != nil {
			t.Errorf("isPurePowerShellCommand(%q) = %v, want nil（应走原 cmd /C 路径）", cmd, got)
		}
	}
}
