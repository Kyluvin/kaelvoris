//go:build !windows

// Package tools 沙箱非 Windows 实现：noop（job object 是 Windows 专属能力）。
package tools

// newPlatformSandbox 非 Windows：返回 nil 触发工厂降级 noop
//（与 Windows 创建失败同一降级路径）。
func newPlatformSandbox(SandboxConfig) (SandboxExecutor, error) {
	return nil, nil
}
