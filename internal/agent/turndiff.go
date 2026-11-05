// Package agent turn diff 追踪（codex 对齐补全 ③）。
//
// 对齐 codex-rs core/src/turn_diff_tracker.rs 的核心思想：本轮任务内记录
// 文件写入类工具（fs_write / fs_apply_diff）触达的文件内容基线快照，
// attempt_completion（或任意时刻）输出全部被改文件的 net diff 摘要——
// 无需依赖 git 仓库即可工作（内存基线 vs 当前磁盘内容的行级对比）。
//
// 与 codex 实现差异（Go 简版）：
//   - codex 以 apply_patch delta 维护 per-path TrackedContent 并渲染
//     unified diff；kaelvoris 直接读磁盘：写入前快照旧内容（不存在视为
//     新建），输出时做内置行级 diff（旧/新内容 LCS 简版：以行集合差异
//     呈现删除行与新增行，不求最小编辑序列）；
//   - 零外部进程依赖（无 git 调用），行为确定、跨平台一致。
package agent

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// turnDiffTracker 本轮文件变更追踪器（对齐 TurnDiffTracker 简版）。
// 记录 fs_write / fs_apply_diff 触达的文件 → 写入前内容基线（nil = 新建）。
type turnDiffTracker struct {
	mu        sync.Mutex
	baselines map[string][]byte // 相对路径 → 写入前内容（nil = 文件原先不存在）
}

// newTurnDiffTracker 创建追踪器。
func newTurnDiffTracker() *turnDiffTracker {
	return &turnDiffTracker{baselines: map[string][]byte{}}
}

// track 写入前快照一个相对路径；重复触达保留最早基线（net diff 语义：
// 任务起点 vs 最终状态，中间态不参与）。
func (t *turnDiffTracker) track(workspace, relPath string) {
	if t == nil || strings.TrimSpace(relPath) == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.baselines[relPath]; ok {
		return
	}
	abs := resolveWorkspacePath(workspace, relPath)
	data, err := os.ReadFile(abs)
	if err != nil {
		t.baselines[relPath] = nil // 原文件不存在 → 新建
		return
	}
	t.baselines[relPath] = data
}

// resolveWorkspacePath 相对路径 → 工作区绝对路径（已绝对则原样返回）。
func resolveWorkspacePath(workspace, relPath string) string {
	if filepath.IsAbs(relPath) || workspace == "" {
		return relPath
	}
	return filepath.Join(workspace, relPath)
}

// summary 输出本轮 net diff 摘要：变更文件统计 + 逐文件行级 diff。
// 无变更返回空串（调用方零分支处理，对齐 codex TurnDiffTracker.diff 语义）。
func (t *turnDiffTracker) summary(workspace string) string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	paths := make([]string, 0, len(t.baselines))
	snap := make(map[string][]byte, len(t.baselines))
	for p, base := range t.baselines {
		paths = append(paths, p)
		snap[p] = base
	}
	t.mu.Unlock()
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)

	created, modified, deleted := 0, 0, 0
	var entries []string
	var diffParts []string
	for _, p := range paths {
		cur, err := os.ReadFile(resolveWorkspacePath(workspace, p))
		if err != nil {
			cur = nil
		}
		base := snap[p]
		var status string
		switch {
		case base == nil && err == nil:
			status, created = "新建", created+1
		case base == nil && err != nil:
			continue // 基线不存在且现在也不存在：净效果为零，不列入
		case err != nil:
			status, deleted = "已删除", deleted+1
		case bytes.Equal(base, cur):
			status = "无净变化"
		default:
			status, modified = "修改", modified+1
		}
		entry := fmt.Sprintf("- %s（%s）", p, status)
		if err == nil {
			entry += fmt.Sprintf("，%d 字节", len(cur))
		}
		entries = append(entries, entry)

		if !bytes.Equal(base, cur) {
			if d := renderLineDiff(p, base, cur); d != "" {
				diffParts = append(diffParts, d)
			}
		}
	}
	if created+modified+deleted == 0 && len(diffParts) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "本轮文件变更净摘要：新建 %d / 修改 %d / 删除 %d", created, modified, deleted)
	b.WriteString("\n" + strings.Join(entries, "\n"))
	if len(diffParts) > 0 {
		b.WriteString("\n\n" + strings.Join(diffParts, "\n"))
	}
	return b.String()
}

// diffLinesCap 单文件 diff 渲染行上限（防爆屏，对齐 codex diff 渲染的截断策略）。
const diffLinesCap = 80

// renderLineDiff 渲染单文件 diff（统一 diff 风格的行级对比，内置实现零依赖）。
// path 为相对路径（diff 头展示用）。
func renderLineDiff(path string, base, cur []byte) string {
	oldLines := splitLines(base)
	newLines := splitLines(cur)

	// 行级 LCS（DP，小文件足够；大文件退化为集合差异避免 O(n²) 爆内存）。
	var lines []string
	if len(oldLines) <= 400 && len(newLines) <= 400 {
		lines = lcsDiff(oldLines, newLines)
	} else {
		lines = setDiff(oldLines, newLines)
	}
	if len(lines) > diffLinesCap {
		lines = append(lines[:diffLinesCap], fmt.Sprintf("... (%d more lines)", len(lines)-diffLinesCap))
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "diff --kaelvoris a/%s\n", filepath.ToSlash(path))
	for _, ln := range lines {
		b.WriteString(ln + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// splitLines 按行切分（保留行尾 \r 剥离；末尾空行不产生空元素）。
func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// lcsDiff 行级 LCS diff（经典 DP；输出统一 +/-/空格 三类行）。
func lcsDiff(old, new []string) []string {
	n, m := len(old), len(new)
	// dp[i][j] = old[i:] 与 new[j:] 的 LCS 长度。
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if old[i] == new[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case old[i] == new[j]:
			out = append(out, "  "+old[i])
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, "- "+old[i])
			i++
		default:
			out = append(out, "+ "+new[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "- "+old[i])
	}
	for ; j < m; j++ {
		out = append(out, "+ "+new[j])
	}
	return out
}

// setDiff 大文件退化的行集合差异（丢弃顺序信息，仅保证 +/- 集合正确）。
func setDiff(old, new []string) []string {
	oldSet := map[string]int{}
	for _, l := range old {
		oldSet[l]++
	}
	newSet := map[string]int{}
	for _, l := range new {
		newSet[l]++
	}
	var out []string
	for _, l := range old {
		if oldSet[l] > newSet[l] {
			out = append(out, "- "+l)
			oldSet[l]--
		}
	}
	for _, l := range new {
		if newSet[l] > oldSet[l] {
			out = append(out, "+ "+l)
			newSet[l]--
		}
	}
	return out
}

// DiffFiles 返回本任务触达的全部文件相对路径（排序去重；C3 收尾提交的
// 文件清单源：autocommit.Run 仅 add 这些文件，绝不 add -A）。
// 无变更返回空 slice（nil）。只报任务起点 vs 最终状态有净变化的文件
// （新建/修改/删除），与 summary 同一判定。
func (e *Engine) DiffFiles() []string {
	if e == nil || e.diffTracker == nil {
		return nil
	}
	e.diffTracker.mu.Lock()
	defer e.diffTracker.mu.Unlock()
	out := make([]string, 0, len(e.diffTracker.baselines))
	for p := range e.diffTracker.baselines {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// fileSha1 内容指纹（对齐 codex git_blob_sha1_hex_bytes 的去重用途，简版）。
func fileSha1(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}
