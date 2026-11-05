package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	ws := t.TempDir()
	s := NewStore(base, "sess1", 0)
	s.SetWorkspaceRoot(ws)
	return s, ws
}

func TestRecordAndList(t *testing.T) {
	s, ws := newTestStore(t)
	p := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(p, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	e1, err := s.Record("fs_write", "a.txt", "v1", false)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if e1.Seq != 1 || e1.Path != "a.txt" || e1.Blob == "" {
		t.Fatalf("bad entry: %+v", e1)
	}
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("want 1 entry, got %d", len(list))
	}
	// 修改文件后按检查点恢复到 v1。
	if err := os.WriteFile(p, []byte("v2-changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(e1.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "v1" {
		t.Fatalf("restore got %q, want v1", b)
	}
	// 恢复本身产生一个 restore 检查点。
	if len(s.List()) != 2 {
		t.Fatalf("restore should add entry, got %d", len(s.List()))
	}
}

func TestRestoreNewFileDeletes(t *testing.T) {
	s, ws := newTestStore(t)
	p := filepath.Join(ws, "new.txt")
	// 新建前的快照：isNew=true。
	e, err := s.Record("fs_write", "new.txt", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("created"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(e.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("file should be deleted after restore, err=%v", err)
	}
	// 幂等：再恢复一次仍成功。
	if _, err := s.Restore(e.ID); err != nil {
		t.Fatalf("idempotent restore: %v", err)
	}
}

func TestTooBigSkipped(t *testing.T) {
	s, _ := newTestStore(t)
	big := strings.Repeat("x", MaxFileBytes+1)
	e, err := s.Record("fs_write", "big.bin", big, false)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if !e.Toobig || e.Blob != "" {
		t.Fatalf("expected tooBig entry without blob: %+v", e)
	}
	if _, err := s.Restore(e.ID); err == nil {
		t.Fatal("restore of too-big entry should fail")
	}
}

func TestQuotaEvictsOldest(t *testing.T) {
	base := t.TempDir()
	s := NewStore(base, "q", 3000) // 3KB 限额
	s.SetWorkspaceRoot(base)
	payload := strings.Repeat("y", 2000) // 每条 2KB
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		e, err := s.Record("fs_write", "f.txt", payload, false)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		ids = append(ids, e.ID)
	}
	entries := s.List()
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	// 最旧两条应被清空 blob（总 6KB > 3KB，删到 ≤3KB 即删 2 条）。
	if entries[0].Blob != "" || entries[1].Blob != "" {
		t.Fatalf("oldest blobs should be evicted: %+v %+v", entries[0], entries[1])
	}
	if entries[2].Blob == "" {
		t.Fatalf("newest blob should survive: %+v", entries[2])
	}
}

func TestCorruptLineSkipped(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Record("fs_write", "x.txt", "data", false); err != nil {
		t.Fatal(err)
	}
	// 手工追加损坏行。
	f, _ := os.OpenFile(filepath.Join(s.dir, "index.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("{broken json\n")
	f.Close()
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("corrupt line should be skipped, got %d entries", len(list))
	}
}

func TestSummary(t *testing.T) {
	s, _ := newTestStore(t)
	if s.Summary() != "" {
		t.Fatal("empty store summary should be empty")
	}
	_, _ = s.Record("fs_write", "a.txt", "old", false)
	_, _ = s.Record("fs_write", "b.txt", "", true)
	sum := s.Summary()
	if sum == "" {
		t.Fatal("summary should not be empty after records")
	}
}
