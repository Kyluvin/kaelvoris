package promptconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStore_Roundtrip Save→重开→Get 全段往返 + 原子落盘。
func TestStore_Roundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompts.json")
	s := New(path)
	c := Config{System: "sys", Summarize: "sum", TaskSummary: "ts",
		SelfReview: "sr", HourlyReview: "hr", RoadmapReview: "rr", SelfCheckTask: "sct",
		RoadmapSystem: "rs", AutonomousDev: "ad"}
	if err := s.Save(c); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 重开（模拟重启）：从磁盘加载
	s2 := New(path)
	for k, want := range map[string]string{
		"system": "sys", "summarize": "sum", "taskSummary": "ts",
		"selfReview": "sr", "hourlyReview": "hr", "roadmapReview": "rr",
		"selfCheckTask": "sct", "roadmapSystem": "rs", "autonomousDev": "ad",
	} {
		if got := s2.Get(k); got != want {
			t.Errorf("Get(%q) = %q, want %q", k, got, want)
		}
	}
}

// TestStore_CacheHotRead 内存缓存语义：Save 后（即使文件被外部删除）
// Get 仍返回已保存值（读内存不读盘），证明「保存即写缓存、热读生效」。
func TestStore_CacheHotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompts.json")
	s := New(path)
	if err := s.Save(Config{System: "cached"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := s.Get("system"); got != "cached" {
		t.Errorf("Get after external delete = %q, want cached value", got)
	}
}

// TestStore_MissingOrCorrupt 文件缺失/损坏按空配置兜底（= 内置默认）。
func TestStore_MissingOrCorrupt(t *testing.T) {
	if got := New(filepath.Join(t.TempDir(), "nope.json")).Get("system"); got != "" {
		t.Errorf("missing file Get = %q, want empty", got)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o600)
	if got := New(bad).Get("system"); got != "" {
		t.Errorf("corrupt file Get = %q, want empty", got)
	}
}
