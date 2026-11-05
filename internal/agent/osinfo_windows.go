//go:build windows

package agent

import (
	"os/exec"
	"regexp"
	"runtime"
	"sync"

	"kaelvoris/internal/sysproc"
)

// osInfoCached 缓存 OS 描述（对齐 system-info.ts 的 os-name()）。
var osInfoCached = sync.OnceValue(func() string {
	// `cmd /c ver` 输出形如 "Microsoft Windows [Version 10.0.19045.5011]"；
	// 中文系统输出 GBK 编码的 "Microsoft Windows [版本 10.0.19045.5011]"。
	// 直接提取版本号数字序列，规避区域语言与编码差异（GBK 双字节不影响 ASCII 数字）。
	verCmd := exec.Command("cmd", "/c", "ver")
	sysproc.Hide(verCmd)
	out, err := verCmd.Output()
	if err == nil {
		if v := windowsVersionRe.FindString(string(out)); v != "" {
			return "Windows " + v
		}
	}
	return runtime.GOOS
})

// windowsVersionRe 从 ver 输出中提取形如 10.0.19045.5011 的版本号。
var windowsVersionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// osInfo 返回操作系统描述。
func osInfo() string { return osInfoCached() }
