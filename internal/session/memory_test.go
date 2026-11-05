// memory_test.go 会话记忆文档存储单测：Append 累积式与 AppendWrapUp
// 「任务收尾」单节覆盖式更新（自学习闭环双路之会话回路）。
package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestMemoryStore(t *testing.T) *MemoryStore {
	t.Helper()
	dir := t.TempDir()
	return NewMemoryStore(dir)
}

// TestAppendWrapUp_NewSection 无既有文档：追加「任务收尾」节。
func TestAppendWrapUp_NewSection(t *testing.T) {
	m := newTestMemoryStore(t)
	if err := m.AppendWrapUp("s1", "任务收尾：重构完成，产出 build/output.exe，下次接着干前端。"); err != nil {
		t.Fatalf("AppendWrapUp: %v", err)
	}
	doc := m.Load("s1")
	if !strings.Contains(doc, wrapUpHeading) {
		t.Fatalf("缺少收尾节标题:\n%s", doc)
	}
	if !strings.Contains(doc, "下次接着干") {
		t.Fatalf("缺少收尾正文:\n%s", doc)
	}
}

// TestAppendWrapUp_Replace 既有收尾节：整节覆盖（不残留旧内容，防膨胀）。
func TestAppendWrapUp_Replace(t *testing.T) {
	m := newTestMemoryStore(t)
	_ = m.Append("s2", "里程碑", "- 已完成：后端地基")
	_ = m.AppendWrapUp("s2", "旧收尾：进行到一半")
	// 第二轮复盘覆盖。
	if err := m.AppendWrapUp("s2", "新收尾：全部完成"); err != nil {
		t.Fatalf("AppendWrapUp: %v", err)
	}
	doc := m.Load("s2")
	if strings.Contains(doc, "旧收尾") {
		t.Fatalf("旧收尾应被覆盖:\n%s", doc)
	}
	if !strings.Contains(doc, "新收尾：全部完成") {
		t.Fatalf("新收尾缺失:\n%s", doc)
	}
	// 里程碑节（收尾节之前）必须保留。
	if !strings.Contains(doc, "已完成：后端地基") {
		t.Fatalf("里程碑被误删:\n%s", doc)
	}
	if strings.Count(doc, wrapUpHeading) != 1 {
		t.Fatalf("收尾节应恰好一份:\n%s", doc)
	}
}

// TestAppendWrapUp_Idempotent 同内容重复写不产生重复节。
func TestAppendWrapUp_Idempotent(t *testing.T) {
	m := newTestMemoryStore(t)
	_ = m.AppendWrapUp("s3", "收尾内容 A")
	_ = m.AppendWrapUp("s3", "收尾内容 A")
	doc := m.Load("s3")
	if strings.Count(doc, "收尾内容 A") != 1 {
		t.Fatalf("应幂等覆盖:\n%s", doc)
	}
}

// TestAppendWrapUp_FileLayout 落盘文件位置：sessions 目录 <id>.memory.md。
func TestAppendWrapUp_FileLayout(t *testing.T) {
	m := newTestMemoryStore(t)
	_ = m.AppendWrapUp("abc123", "内容")
	b, err := os.ReadFile(filepath.Join(m.dir, "abc123"+memorySuffix))
	if err != nil {
		t.Fatalf("记忆文件未落盘: %v", err)
	}
	if !strings.Contains(string(b), "内容") {
		t.Fatalf("文件内容缺失: %s", b)
	}
}
