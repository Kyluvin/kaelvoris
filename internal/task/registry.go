// Package task 任务注册表与调度器：移植自 Zoo-Code
//   - TaskRegistry   ← src/core/task/TaskRegistry.ts（LIFO 栈 + Map 适配器）
//   - TaskScheduler  ← src/core/task/TaskScheduler.ts + utils/TaskSemaphore.ts（并发门控）
//
// 语义对齐说明：
//   - push = move-to-top（已存在先移除，保持栈无重复且聚焦新任务）
//   - setCurrent 仅切换焦点，不改栈序
//   - getRunning/hasRunning 过滤 abort/abandoned（用户取消/废弃的任务不算运行中）
package task

import (
	"fmt"
	"sync"
)

// Task 运行单元最小接口（对齐 Task.ts 的 taskId / abort / abandoned）。
type Task interface {
	// TaskID 唯一标识（kaelvoris 中即会话 ID）。
	TaskID() string
	// IsAborted 用户主动取消。
	IsAborted() bool
	// IsAbandoned 被废弃（如父任务取消后孤儿任务）。
	IsAbandoned() bool
}

// Registry Task 注册表：Map + LIFO 栈 + 焦点指针的 Expand-Contract 适配器。
// 不变量：len(tasks) == len(stack)。
type Registry struct {
	mu      sync.Mutex
	tasks   map[string]Task
	stack   []string
	current string
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{tasks: map[string]Task{}}
}

// Push 添加任务并聚焦。已存在同 ID 先移除（move-to-top，不产生重复栈条目）。
func (r *Registry) Push(t Task) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := t.TaskID()
	if _, ok := r.tasks[id]; ok {
		r.removeLocked(id)
	}
	r.tasks[id] = t
	r.stack = append(r.stack, id)
	r.current = id
}

// Pop 弹出栈顶任务并返回；焦点回落到新栈顶。
func (r *Registry) Pop() Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stack) == 0 {
		return nil
	}
	id := r.stack[len(r.stack)-1]
	r.stack = r.stack[:len(r.stack)-1]
	t := r.tasks[id]
	delete(r.tasks, id)
	if r.current == id {
		r.current = ""
		if n := len(r.stack); n > 0 {
			r.current = r.stack[n-1]
		}
	}
	return t
}

// Current 当前焦点任务。
func (r *Registry) Current() Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == "" {
		return nil
	}
	return r.tasks[r.current]
}

// SetCurrent 切换 UI 焦点（不改栈序）；未知 id 返回错误（对齐 TaskRegistry.setCurrent）。
func (r *Registry) SetCurrent(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[id]; !ok {
		return fmt.Errorf("[TaskRegistry] setCurrent: unknown taskId %s", id)
	}
	r.current = id
	return nil
}

// GetByID 按 ID 查找。
func (r *Registry) GetByID(id string) Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasks[id]
}

// All 栈序返回全部任务。
func (r *Registry) All() []Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Task, 0, len(r.stack))
	for _, id := range r.stack {
		out = append(out, r.tasks[id])
	}
	return out
}

// Running 全部未取消/未废弃任务（对齐 getRunning）。
func (r *Registry) Running() []Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Task, 0, len(r.stack))
	for _, id := range r.stack {
		t := r.tasks[id]
		if t != nil && !t.IsAborted() && !t.IsAbandoned() {
			out = append(out, t)
		}
	}
	return out
}

// HasRunning 仅当任务在注册表中且未取消/未废弃时为 true。
func (r *Registry) HasRunning(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	return ok && t != nil && !t.IsAborted() && !t.IsAbandoned()
}

// Remove 按 ID 移除（无论栈中位置），返回被移除任务。
func (r *Registry) Remove(id string) Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.removeLocked(id)
}

func (r *Registry) removeLocked(id string) Task {
	t, ok := r.tasks[id]
	if !ok {
		return nil
	}
	delete(r.tasks, id)
	for i, s := range r.stack {
		if s == id {
			r.stack = append(r.stack[:i], r.stack[i+1:]...)
			break
		}
	}
	if r.current == id {
		r.current = ""
		if n := len(r.stack); n > 0 {
			r.current = r.stack[n-1]
		}
	}
	return t
}

// Replace 原位替换任务（保持栈位与焦点；对齐 rehydration 场景）。未知 ID 返回错误。
func (r *Registry) Replace(id string, replacement Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := -1
	for i, s := range r.stack {
		if s == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("[TaskRegistry] replace: unknown taskId %s", id)
	}
	nid := replacement.TaskID()
	if nid != id {
		if _, dup := r.tasks[nid]; dup {
			return fmt.Errorf("[TaskRegistry] replace: duplicate taskId %s", nid)
		}
	}
	delete(r.tasks, id)
	r.tasks[nid] = replacement
	r.stack[idx] = nid
	if r.current == id {
		r.current = nid
	}
	return nil
}

// Len 栈中任务数。
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stack)
}

// TaskIDs 栈序 ID 列表。
func (r *Registry) TaskIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.stack))
	copy(out, r.stack)
	return out
}
