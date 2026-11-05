// Package sysproc 子进程系统属性：GUI 进程下隐藏控制台窗口。
//
// 背景：kaelvoris 为 Wails GUI 应用，在 Windows 上 exec.Command 启动
// 控制台程序（cmd / git / rg 等）时系统会为新进程分配可见控制台
// 窗口（黑框）。Zoo-Code（Node 扩展宿主）等价语义是 spawn 时
// windowsHide + 无终端窗口；Go 对应做法为 SysProcAttr{HideWindow,
// CreationFlags: CREATE_NO_WINDOW}。
//
// 跨平台入口 Hide：Windows 实现见 hide_windows.go（构建标签 windows），
// 非 Windows 空操作见 hide_other.go（构建标签 !windows）。
package sysproc
