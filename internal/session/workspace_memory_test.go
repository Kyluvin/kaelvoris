// workspace_memory_test.go 工作空间记忆存储单测：按工作区隔离、标注维护、
// 跨会话共享语义。
package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestWSMemoryStore(t *testing.T) *WorkspaceMemoryStore {
	t.Helper()
	return NewWorkspaceMemoryStore(t.TempDir())
}

// TestWorkspaceMemory_IsolatedByWorkspace 不同工作区互不可见（按项目隔离核心语义）。
func TestWorkspaceMemory_IsolatedByWorkspace(t *testing.T) {
	w := newTestWSMemoryStore(t)
	if err := w.Save(`F:\proj\a`, "A 项目记忆"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := w.Load(`F:\proj\b`); got != "" {
		t.Fatalf("不同工作区应不可见，得到: %q", got)
	}
	if got := w.Load(`F:\proj\a`); !strings.Contains(got, "A 项目记忆") {
		t.Fatalf("同工作区应可读回: %q", got)
	}
}

// TestWorkspaceMemory_CaseInsensitive Windows 盘符/路径大小写差异归一（不产生两份记忆）。
func TestWorkspaceMemory_CaseInsensitive(t *testing.T) {
	w := newTestWSMemoryStore(t)
	_ = w.Save(`f:\Proj\A`, "内容")
	if w.Path(`F:\proj\a`) != w.Path(`f:\Proj\A`) {
		t.Fatalf("大小写不同的同一路径应映射到同一文件")
	}
	if got := w.Load(`F:/PROJ/a`); !strings.Contains(got, "内容") {
		t.Fatalf("斜杠/大小写变体应命中同一记忆: %q", got)
	}
}

// TestWorkspaceMemory_Marker 首行工作区路径标注：Save 自动补、Load→Save 回写不重复。
func TestWorkspaceMemory_Marker(t *testing.T) {
	w := newTestWSMemoryStore(t)
	_ = w.Save(`F:\proj\a`, "# 记忆\n\n- 条目")
	doc := w.Load(`F:\proj\a`)
	if !strings.HasPrefix(doc, "<!-- workspace:") {
		t.Fatalf("缺少首行路径标注:\n%s", doc)
	}
	// 回写已带标注的文档：标注不重复（替换而非追加）。
	if err := w.Save(`F:\proj\a`, doc); err != nil {
		t.Fatalf("回写: %v", err)
	}
	doc2 := w.Load(`F:\proj\a`)
	if strings.Count(doc2, "<!-- workspace:") != 1 {
		t.Fatalf("标注应恰好一份:\n%s", doc2)
	}
	if !strings.Contains(doc2, "# 记忆") {
		t.Fatalf("正文应保留:\n%s", doc2)
	}
}

// TestWorkspaceMemory_EmptyWorkspace 空工作区：Load 空串、Save 报错（不落野文件）。
func TestWorkspaceMemory_EmptyWorkspace(t *testing.T) {
	w := newTestWSMemoryStore(t)
	if got := w.Load(""); got != "" {
		t.Fatalf("空工作区 Load 应为空: %q", got)
	}
	if w.Path("") != "" {
		t.Fatalf("空工作区 Path 应为空")
	}
	if err := w.Save("", "x"); err == nil {
		t.Fatalf("空工作区 Save 应报错")
	}
}

// TestWorkspaceMemory_Touch 「最近交互」节：空文档建骨架、已有文档追加、
// 重复 Touch 覆盖不膨胀（单节语义）、正文保留。
func TestWorkspaceMemory_Touch(t *testing.T) {
	w := newTestWSMemoryStore(t)
	if err := w.Touch(`F:\proj\a`, "2026-10-31 10:00 · 会话 s1 有交互"); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	doc := w.Load(`F:\proj\a`)
	if !strings.Contains(doc, "# 工作空间记忆") || !strings.Contains(doc, lastActiveHeading) {
		t.Fatalf("空文档 Touch 应建骨架:\n%s", doc)
	}
	// 用户正文写入后 Touch：正文保留、交互节刷新为单条。
	_ = w.Save(`F:\proj\a`, "# 工作空间记忆\n\n- 构建命令: go build ./...\n\n## 最近交互\n\n- 旧时间\n")
	if err := w.Touch(`F:\proj\a`, "2026-10-31 11:00 · 会话 s2 有交互"); err != nil {
		t.Fatalf("Touch2: %v", err)
	}
	doc2 := w.Load(`F:\proj\a`)
	if !strings.Contains(doc2, "go build ./...") {
		t.Fatalf("正文应保留:\n%s", doc2)
	}
	if strings.Contains(doc2, "旧时间") || strings.Count(doc2, "有交互") != 1 {
		t.Fatalf("交互节应覆盖不膨胀:\n%s", doc2)
	}
	if strings.Count(doc2, lastActiveHeading) != 1 {
		t.Fatalf("交互节标题应恰好一份:\n%s", doc2)
	}
}

// TestWorkspaceMemory_FileLayout 落盘位置：root/<sha前16hex>.memory.md。
func TestWorkspaceMemory_FileLayout(t *testing.T) {
	w := newTestWSMemoryStore(t)
	_ = w.Save(`F:\proj\a`, "内容")
	name := filepath.Base(w.Path(`F:\proj\a`))
	if !strings.HasSuffix(name, memorySuffix) || len(strings.TrimSuffix(name, memorySuffix)) != 16 {
		t.Fatalf("文件名应为 16 hex + .memory.md: %s", name)
	}
	if _, err := os.Stat(filepath.Join(w.dir, name)); err != nil {
		t.Fatalf("记忆文件未落盘: %v", err)
	}
}
