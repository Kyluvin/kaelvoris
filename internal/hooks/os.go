package hooks

import (
	"path/filepath"
	"runtime"
)

// goosWindows 平台判定（钩子 shell 包装选择：cmd /C vs sh -c）。
func goosWindows() bool { return runtime.GOOS == "windows" }

// DefaultPath 默认配置路径：~/.kaelvoris/hooks.json。
func DefaultPath(home string) string {
	return filepath.Join(home, ".kaelvoris", "hooks.json")
}
