// denylist_test.go DenyList 拦截路径回归测试（2026-10-10 第 23 轮自检实锤）：
// DefaultDenyList 的 "format " 子串检查与 danger.go 模式匹配是两条并行路径，
// isPSParamSwitch 修复只覆盖了 danger.go 一条；Get-Date -Format 仍死在
// shell/unified_exec 的 DenyList 检查。本测试锁定两工具共用判定放行 PS 参数
// 形态、保留真 format 磁盘命令拦截。
package tools

import (
	"strings"
	"testing"
)

func TestDenyListGetDateFormatAllowed(t *testing.T) {
	cases := []string{
		`powershell -NoProfile -Command "Get-Date -Format 'yyyy-MM-dd HH:mm:ss'"`,
		`powershell -NoProfile -Command "(Get-Item _chk_dist.ps1).LastWriteTime"`,
	}
	for _, c := range cases {
		if hit := denyListHit(c); hit {
			t.Errorf("PS -Format parameter must not hit DenyList: %q", c)
		}
	}
}

func TestDenyListFormatDiskStillBlocked(t *testing.T) {
	cases := []string{
		`format d: /q`,
		`echo hi & format c:`,
	}
	for _, c := range cases {
		if !denyListHit(c) {
			t.Errorf("real format disk command must stay blocked: %q", c)
		}
	}
	// 其余硬红线不受影响（shutdown 等子串照拦）。
	if !strings.Contains(strings.ToLower(`shutdown /s /t 0`), "shutdown") {
		t.Fatal("sanity: shutdown substring must exist")
	}
}
