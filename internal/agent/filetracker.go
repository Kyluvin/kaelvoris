// filetracker.go 文件上下文追踪（简版）：移植自 Zoo-Code
// src/core/context/context-tracking/FileContextTracker 的 stale 检测核心语义。
//
// 记录本任务内 fs_read 读取文件时的 mtime；模型随后用 fs_write / fs_apply_diff
// 写入同一文件时，若磁盘 mtime 已变化（被 shell 命令 / 用户 / 其他工具在外部
// 修改），判定"基于过期内容写文件"，say 警告 + 结果附加提示，让模型重读后再改。
// 纯 mtime 对比，零解析依赖（apply-patch/搜索替换形态的解析全部省略）。
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fileContextTracker 单任务文件读写状态（RunInMode 懒初始化，跨模式/子任务
// 共享同一实例——整个任务生命周期内统一追踪，对齐 diffTracker 模式）。
type fileContextTracker struct {
	reads  map[string]time.Time // 规范化相对路径 → 读取时 mtime
	writes map[string]time.Time // 规范化相对路径 → 最近写入时 mtime
}

func newFileContextTracker() *fileContextTracker {
	return &fileContextTracker{reads: map[string]time.Time{}, writes: map[string]time.Time{}}
}

// normPath 把工具参数中的相对路径规范化（正斜杠 + Clean；空安全）。
func normPath(p string) string {
	return filepath.ToSlash(filepath.Clean(strings.TrimSpace(p)))
}

// statWorkspace 解析工作区内相对路径并返回 mtime（不存在/越界返回 zero）。
func statWorkspace(workspace, rel string) (time.Time, bool) {
	if rel == "" || normPath(rel) == "." {
		return time.Time{}, false
	}
	ws, err := filepath.Abs(workspace)
	if err != nil {
		return time.Time{}, false
	}
	abs := filepath.Clean(filepath.Join(ws, filepath.FromSlash(normPath(rel))))
	if abs != ws && !strings.HasPrefix(abs, ws+string(os.PathSeparator)) {
		return time.Time{}, false
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// checkStale 写入前检查：目标文件在读取后 mtime 是否已变化。返回警告文本
// （空串 = 无 stale）。
func (f *fileContextTracker) checkStale(workspace, rel string) string {
	if f == nil {
		return ""
	}
	rel = normPath(rel)
	readAt, ok := f.reads[rel]
	if !ok {
		return "" // 本任务内从未读过 → 不判定（外部基线无法推断）
	}
	cur, ok := statWorkspace(workspace, rel)
	if !ok || cur.Equal(readAt) {
		return ""
	}
	return fmt.Sprintf(
		"[file-context warning] %s was modified after your last read (mtime changed). "+
			"Your edit may be based on stale content — re-read the file before writing if unsure.",
		rel)
}

// markRead 记录读取（fs_read 成功后调用）。
func (f *fileContextTracker) markRead(workspace, rel string) {
	if f == nil {
		return
	}
	rel = normPath(rel)
	if mt, ok := statWorkspace(workspace, rel); ok {
		f.reads[rel] = mt
	}
}

// markWritten 记录写入（fs_write / fs_apply_diff 成功后调用）：刷新 mtime 基线，
// 使后续写入不误报（自己刚写过的文件不算外部修改）。
func (f *fileContextTracker) markWritten(workspace, rel string) {
	if f == nil {
		return
	}
	rel = normPath(rel)
	if mt, ok := statWorkspace(workspace, rel); ok {
		f.writes[rel] = mt
		// 写入即已知当前内容：读取基线同步刷新（自己刚写过的文件
		// 不算外部修改，后续写入不误报 stale）。
		f.reads[rel] = mt
	}
}

// staleNotice 组装附加到工具结果尾部的提示块（空警告返回原样）。
func staleNotice(warning, result string) string {
	if warning == "" {
		return result
	}
	return result + "\n\n" + warning
}
