// Package tools 写冲突防护（2026-10-05 会话复盘）：多会话（含 roadmap 自动
// 迭代、定时复盘、并行子任务）共用同一工作区时，A 会话 fs_read 后 B 会话
// 改写了同一文件，A 再发 SEARCH 必然落空——复盘样本中 SEARCH 失效 40+ 条
// 居首位即此根因。本模块提供两层防护：
//
//  1. 进程内写互斥（writeLocks）：同文件写操作串行化，消除「同 tick 双写
//     交错」的丢更新窗口；
//  2. 写前新鲜度校验（lastReadMtime）：fs_read 记录 (工作区,相对路径) 的
//     读取时刻，fs_write/fs_apply_diff 写入时若目标 mtime 晚于最近一次读取
//     （且本会话未在读取后写过它），返回明确冲突错误——引导模型重读再改，
//     而非带着陈旧 SEARCH 盲试。
//
// 防误伤边界：
//   - 首次接触的文件（从未 fs_read）不拦截：新文件创建/模型凭自身知识改写
//     是合法场景，只给出未读警告性提示信息（不阻断）；
//   - 本会话写入后 mtime 必然新于读取时刻，用 lastWriteMtime 豁免连环编辑；
//   - 校验仅比较 mtime，毫秒级误差容忍（mtime 相等视为未变更）；
//   - 文件被外部进程（非会话）修改无法区分，mtime 变化即拦截——宁可多一次
//     重读（成本 = 一次分页读），不可漏检（成本 = 40+ 条 SEARCH 失败）。
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// writeLocks 进程内文件写互斥：key = 规范化绝对路径。
// 同一文件的 fs_write / fs_apply_diff 串行执行（多会话并发改同一文件时
// 后写者等待前写者完成再校验 mtime，冲突检测因此无竞态）。
var writeLocks sync.Map

// lastReadMtime 模型最近一次经 fs_read 触达文件的时刻（含读到的 mtime）。
// key = 规范化绝对路径；多会话共享（同工作区语义即共享文件系统状态）。
var lastReadMtime sync.Map

// readGuardRecord fs_read 成功后记录文件当前 mtime（供写前新鲜度比对）。
func readGuardRecord(abs string, modTime time.Time) {
	if abs == "" {
		return
	}
	key := filepath.Clean(abs)
	lastReadMtime.Store(key, recordEntry{mtime: modTime, at: time.Now()})
	// fs_read 触达即清零该文件的 SEARCH 连续失败计数（2026-10-30 复盘：
	// publish-client.ps1 误报——STOP 引导的语义是「先 fs_read 再改」，模型照做
	// 后应视为全新一轮：旧计数若不清零，重读后凭最新内容构造的合法重试仍会被
	// 旧计数 STOP，形成死锁式误报。同参盲发的防护不减弱：清零后重新计数，
	// 连续 2 次仍会 STOP）。
	diffFailCount.Delete(key)
}

// recordEntry 读取记录：文件在读取时刻的 mtime + 记录本身的时间戳。
type recordEntry struct {
	mtime time.Time
	at    time.Time
}

// fsTimeEqual mtime 等价判定（容忍 2s 粒度差异：FAT 等文件系统时间戳
// 粒度粗，相等比较会误报「已被修改」）。
func fsTimeEqual(a, b time.Time) bool {
	if a.Equal(b) {
		return true
	}
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= 2*time.Second
}

// writeGuardCheck 写类工具（fs_write/fs_apply_diff）写前校验：
//
//	fileMtime 目标文件当前 mtime（os.Stat 取得）；文件不存在传零值与 false。
//
// 返回 (提示信息, 是否放行)：
//   - 从未读取过 → 放行（返回空串）；
//   - 文件 mtime 与读取时记录一致 → 放行；
//   - 文件 mtime 晚于读取记录（且晚于本进程任何写记录）→ 拦截并返回冲突
//     错误提示（由调用方包装为 error 返回模型）。
func writeGuardCheck(abs string, fileMtime time.Time, fileExists bool) (string, bool) {
	key := filepath.Clean(abs)
	v, ok := lastReadMtime.Load(key)
	if !ok {
		return "", true // 首次接触（未 fs_read 过）：不拦截
	}
	rec := v.(recordEntry)
	if !fileExists {
		// 读过但现在不存在：被外部删除（其他会话清理/重构），拦截。
		// 2026-10-06 复盘：模型收到该冲突后反复重试同名文件（连续 3 次）不会换名，
		// 追加「换文件名 / 重读最新内容」两步可操作指引。
		return "the file was deleted after you last read it (concurrent modification); " +
			"re-check with fs_list before recreating it - " +
			"该文件可能已被并发修改/删除，建议换文件名写入，或重新 fs_read 最新内容后重试", false
	}
	if fsTimeEqual(rec.mtime, fileMtime) {
		return "", true // 与读取时一致：安全
	}
	if fileMtime.After(rec.at.Add(-2 * time.Second)) {
		// mtime 晚于读取动作本身：读取后文件被改写（并发会话/外部进程）。
		return fmt.Sprintf("the file changed on disk after your last fs_read (mtime %s, read at %s); "+
			"another session or process modified it - fs_read it again and redo the edit from fresh content",
			fileMtime.Format("15:04:05"), rec.at.Format("15:04:05")), false
	}
	// mtime 早于读取动作：时钟回拨等异常场景，放行（不因异常时间误伤）。
	return "", true
}

// writeGuardAfterWrite 写成功后更新记录：本会话已知文件最新 mtime，
// 后续连续编辑（fs_apply_diff 逐段追加）不再被自身写入误判为并发冲突。
func writeGuardAfterWrite(abs string) {
	if fi, err := os.Stat(abs); err == nil {
		lastReadMtime.Store(filepath.Clean(abs), recordEntry{mtime: fi.ModTime(), at: time.Now()})
	}
}

// withFileWriteLock 以文件为粒度的写互斥执行 fn（同文件串行，不同文件并行）。
func withFileWriteLock(abs string, fn func() (string, error)) (string, error) {
	key := filepath.Clean(abs)
	mu, _ := writeLocks.LoadOrStore(key, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	return fn()
}

// workspaceRelPath 求 abs 相对 workspace 的展示路径（失败时回退 abs）。
// 错误信息展示用：模型对相对路径更敏感（与其传参一致）。
func workspaceRelPath(workspace, abs string) string {
	if ws, err := filepath.Abs(workspace); err == nil {
		if rel, err := filepath.Rel(filepath.Clean(ws), filepath.Clean(abs)); err == nil &&
			!strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(abs)
}

// ---------- fs_apply_diff SEARCH 连续失败计数（2026-10-06 复盘） ----------
//
// 背景：SEARCH block not found 后模型常在同会话内反复重发相同 SEARCH（3-6 次），
// 提示文本已含 "do NOT re-send" 仍不改。此处按文件记录连续失败次数：第 2 次起
// 返回更强调的 STOP 引导（内容可能已变，先 fs_read 最新内容再用行锚点，或整文件
// 重写；若 search==replace 则指明写反）。计数用 sync.Map 保证并发安全（多会话
// 并行改同一文件），无新增依赖。
var diffFailCount sync.Map // key = 规范化绝对路径, value = int

// diffFailIncr 每次 SEARCH 未命中返回前调用，返回该文件当前连续失败次数（≥1）。
func diffFailIncr(abs string) int {
	key := filepath.Clean(abs)
	v, _ := diffFailCount.LoadOrStore(key, 0)
	n := v.(int) + 1
	diffFailCount.Store(key, n)
	return n
}

// diffFailReset 编辑成功后清零失败计数（下次失败重新从 1 计起）。
func diffFailReset(abs string) {
	diffFailCount.Delete(filepath.Clean(abs))
}
