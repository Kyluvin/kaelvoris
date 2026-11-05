// Package session 会话记忆文档（P3.1-C）。
//
// 每个会话一份 MEMORY.md（~/.kaelvoris/sessions/<id>.memory.md）：
//   - 指针式记录：主体（任务目标/进度状态）+ 详细内容仅记录文件位置指针，
//     需要时引擎自行 fs_read 相应文档——记忆文档保持精简不膨胀；
//   - 里程碑总结：每完成一个待办项，通道层（say 桥钩子）让引擎追加一条
//     「已完成 + 产出物位置」的总结；
//   - 恢复注入：任务恢复（停止后继续）时自动把记忆文档 + 待办清单注入
//     系统提示词，无需回放全部聊天记录即可续接上下文。
package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// memorySuffix 记忆文档文件后缀（<sessionID>.memory.md）。
const memorySuffix = ".memory.md"

// memoryPath 记忆文档路径。
func memoryPath(dir, id string) string { return filepath.Join(dir, id+memorySuffix) }

// MemoryStore 会话记忆文档存储（直接落盘，低频写入无需缓存）。
type MemoryStore struct {
	dir string
	mu  sync.Mutex
}

// NewMemoryStore 创建记忆存储。
func NewMemoryStore(dir string) *MemoryStore { return &MemoryStore{dir: dir} }

// Path 会话记忆文档路径。
func (m *MemoryStore) Path(id string) string { return memoryPath(m.dir, id) }

// Load 读取记忆文档（不存在返回空串）。
func (m *MemoryStore) Load(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := os.ReadFile(memoryPath(m.dir, id))
	if err != nil {
		return ""
	}
	return string(b)
}

// Save 覆盖写入记忆文档（原子替换；调用方保证内容精简——主体+指针）。
func (m *MemoryStore) Save(id, content string) error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tmp := memoryPath(m.dir, id) + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, memoryPath(m.dir, id))
}

// Append 追加一条记忆（自动补标题与分隔；每次完成后由引擎总结调用）。
func (m *MemoryStore) Append(id, title, content string) error {
	old := m.Load(id)
	var b strings.Builder
	b.WriteString(old)
	if old != "" && !strings.HasSuffix(old, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("## " + title + "\n\n" + content + "\n\n")
	return m.Save(id, b.String())
}

// wrapUpHeading 「任务收尾」节标题（自学习复盘维护；每次复盘覆盖重写）。
const wrapUpHeading = "## 任务收尾"

// AppendWrapUp 覆盖式更新「任务收尾」节（自学习复盘调用）：
//   - 已有该节：整节替换为最新内容（同一会话多轮任务只保留最新进度快照，
//     防记忆文档随任务数线性膨胀）；
//   - 无该节：追加到文档末尾。
//
// 与 Append（里程碑累积式）互补：里程碑只增不删，收尾节始终反映当前进度。
func (m *MemoryStore) AppendWrapUp(id, content string) error {
	old := m.Load(id)
	var b strings.Builder
	if i := strings.Index(old, wrapUpHeading); i >= 0 {
		// 替换既有收尾节：保留节前内容（里程碑等累积节）；收尾节标题及其后
		// 的全部内容视为旧收尾整体替换——该节始终是文档最后一节，节体与
		// "节后文本"无法区分，多轮复盘只保留最新进度快照（防膨胀）。
		b.WriteString(old[:i])
		b.WriteString(wrapUpHeading + "\n\n" + strings.TrimSpace(content) + "\n")
	} else {
		b.WriteString(old)
		if old != "" && !strings.HasSuffix(old, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n" + wrapUpHeading + "\n\n" + strings.TrimSpace(content) + "\n")
	}
	return m.Save(id, strings.TrimRight(b.String(), "\n")+"\n")
}

// lastActiveHeading 复用 workspace_memory.go 同名常量（"## 最近交互"，同包同语义：
// 每次交互自动更新；单节覆盖式）。

// Touch 每次交互自动更新「最近交互」节（宿主 say 桥拦截 api_req_finished 调用，
// 2026-10-31 双层记忆：用户要求每次交互都更新会话记忆）：
//   - 无该节：插入「任务收尾」节之前（无收尾节则追加文档末尾——收尾节始终是
//     文档最后一节，维持 AppendWrapUp 的节序约定）；
//   - 已有该节：整节替换（同会话多轮只保留最新时间，防膨胀）。
//
// 与 Append（里程碑）/AppendWrapUp（收尾）/Save（用户编辑）互不干扰：
// Touch 只动「最近交互」节，其余节原样保留。
func (m *MemoryStore) Touch(id, line string) error {
	old := m.Load(id)
	var b strings.Builder
	if old == "" {
		b.WriteString("# 会话记忆\n\n任务进行中，以下按里程碑记录（指针式：详情见指向的文件）。\n")
	} else {
		b.WriteString(old)
		if !strings.HasSuffix(old, "\n") {
			b.WriteString("\n")
		}
	}
	body := b.String()
	section := lastActiveHeading + "\n\n- " + line + "\n"
	// 移除既有「最近交互」节（从节标题到下一 "## " 标题或文档末尾）。
	if i := strings.Index(body, lastActiveHeading); i >= 0 {
		end := len(body)
		if j := strings.Index(body[i+len(lastActiveHeading):], "\n## "); j >= 0 {
			end = i + len(lastActiveHeading) + j
		}
		body = strings.TrimRight(body[:i], "\n") + "\n\n" + strings.TrimLeft(body[end:], "\n")
	}
	// 插入：「任务收尾」节之前；无收尾节则文档末尾。
	if k := strings.Index(body, wrapUpHeading); k >= 0 {
		return m.Save(id, body[:k]+section+"\n"+body[k:])
	}
	return m.Save(id, strings.TrimRight(body, "\n")+"\n\n"+section)
}
