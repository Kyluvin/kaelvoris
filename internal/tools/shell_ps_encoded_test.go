package tools

import (
	"encoding/base64"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestEncodePSCommand —— -EncodedCommand 编码函数单测（UTF-16LE → Base64）。
func TestEncodePSCommand(t *testing.T) {
	script := `Write-Output ('A=' + $env:USERNAME)`
	got := encodePSCommand(script)
	// 与 PowerShell 官方编码一致：UTF-16LE 字节 → Base64。
	units := utf16.Encode([]rune(script))
	var raw []byte
	for _, u := range units {
		raw = append(raw, byte(u&0xff), byte(u>>8))
	}
	want := base64.StdEncoding.EncodeToString(raw)
	if got != want {
		t.Fatalf("encodePSCommand = %q, want %q", got, want)
	}
	// BMP 外字符（emoji）必须经代理对正确编码，不产生截断。
	emoji := `Write-Output "😀"`
	got2 := encodePSCommand(emoji)
	units2 := utf16.Encode([]rune(emoji))
	var raw2 []byte
	for _, u := range units2 {
		raw2 = append(raw2, byte(u&0xff), byte(u>>8))
	}
	if got2 != base64.StdEncoding.EncodeToString(raw2) {
		t.Fatalf("encodePSCommand(emoji) mismatched")
	}
}

// TestPSEncodedInvocation —— -Command → -EncodedCommand 转换单测。
func TestPSEncodedInvocation(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want []string
		ok   bool
	}{
		{
			"标准 -Command 转换",
			[]string{"powershell", "-NoProfile", "-Command", `Get-ChildItem $env:TEMP`},
			[]string{"powershell", "-NoProfile", "-EncodedCommand", encodePSCommand(`Get-ChildItem $env:TEMP`)},
			true,
		},
		{
			"带其它参数",
			[]string{"pwsh", "-NoProfile", "-NonInteractive", "-Command", `Write-Output $PID`},
			[]string{"pwsh", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePSCommand(`Write-Output $PID`)},
			true,
		},
		{
			"无 -Command（-File 场景）不转换",
			[]string{"powershell", "-File", "setup/probe.ps1"},
			nil,
			false,
		},
		{
			"-Command -（stdin 读取）不转换",
			[]string{"powershell", "-Command", "-"},
			nil,
			false,
		},
		{
			"已 -EncodedCommand 不重复转换",
			[]string{"powershell", "-EncodedCommand", "QUJD"},
			nil,
			false,
		},
		{
			"参数过少不转换",
			[]string{"powershell", "-Command"},
			nil,
			false,
		},
	}
	for _, c := range cases {
		got, ok := psEncodedInvocation(c.argv)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if !c.ok {
			if got != nil {
				t.Errorf("%s: got = %v, want nil", c.name, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got = %#v, want %#v", c.name, got, c.want)
		}
	}
}

// TestPSSafeCommand —— 投递前命令行保真形态单测（cmd /K 与 bg_exec 共用）。
func TestPSSafeCommand(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
	}{
		{
			"纯 PowerShell 转 EncodedCommand",
			`powershell -NoProfile -Command "Write-Output ('A=' + $env:USERNAME)"`,
			`powershell -NoProfile -EncodedCommand ` + encodePSCommand(`Write-Output ('A=' + $env:USERNAME)`),
		},
		{
			"普通命令维持原样",
			`dir /b`,
			`dir /b`,
		},
		{
			"复合命令（引号外管道）维持原样",
			`powershell -Command "x" | findstr x`,
			`powershell -Command "x" | findstr x`,
		},
		{
			"光杆 powershell 维持原样",
			`powershell`,
			`powershell`,
		},
		{
			"pwsh 也转换",
			`pwsh -NoProfile -Command '$env:PATH'`,
			`pwsh -NoProfile -EncodedCommand ` + encodePSCommand(`$env:PATH`),
		},
	}
	for _, c := range cases {
		got := psSafeCommand(c.command)
		if got != c.want {
			t.Errorf("%s: psSafeCommand(%q) = %q, want %q", c.name, c.command, got, c.want)
		}
	}
}

// TestPSSafeCommandExec —— 端到端冒烟（Windows only）：转换后的形态经 cmd /C
// 中转后 PowerShell 端输出与直跑一致（$ 变量未被吞）。
func TestPSSafeCommandExec(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	// 环境守卫：部分机器 SRP/杀软（如 360 主动防御）拦截 powershell.exe 执行
	//（Access is denied），e2e 冒烟无法进行——跳过而非误报代码缺陷。探针必须
	// 与实际执行同路径（cmd /C 中转）：本机实测（2026-10-31）直接启动
	// powershell.exe 可行，但 cmd 子进程链被拦（"拒绝访问"），探针不同路径
	// 会漏检导致 FAIL 而非 SKIP。
	if probe := exec.Command("cmd", "/C", "powershell -NoProfile -Command exit"); probe.Run() != nil {
		t.Skip("本机 cmd /C 启动 powershell 被环境策略拦截（SRP/杀软），跳过 e2e 冒烟")
	}

	cmd := psSafeCommand(`powershell -NoProfile -Command "Write-Output ('A=' + $env:USERNAME)"`)
	if strings.Contains(cmd, "$env") {
		t.Fatalf("safe command 仍含 $ 变量: %q", cmd)
	}

	// 端到端：转换后的命令经 cmd /C 执行，输出应含正确结果。
	// 期望值动态取本机用户名（原硬编码 "PC" 在用户名非 PC 的机器上必然失败，
	// 且无法证明 $ 变量确实被 PowerShell 展开）。
	want := "A=" + os.Getenv("USERNAME")
	out, err := exec.Command("cmd", "/C", cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("exec cmd /C: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), want) {
		t.Fatalf("输出不含 %s（$ 变量仍被吞？）: %s", want, out)
	}
}
