package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecDebt_Smoke 验证 ponytail_debt 真实扫描：
//  1. 在临时目录生成含 ponytail: 注释的文件（含 no-trigger 与带升级路径两种）；
//  2. 调用 execDebt 扫描，断言找到标记、no-trigger 计数正确；
//  3. 解释性提及（规则说明）与文档文件（.md）不入台账。
func TestExecDebt_Smoke(t *testing.T) {
	root := t.TempDir()

	// 有 upgrade 的标记（真债务）。
	file1 := filepath.Join(root, "a.go")
	os.WriteFile(file1, []byte("package a\n\n// ponytail: global lock, per-account locks if throughput matters\nvar mu sync.Mutex\n"), 0o644)
	// 无 upgrade 的标记（no-trigger 腐化风险）。
	file2 := filepath.Join(root, "b.go")
	os.WriteFile(file2, []byte("package b\n\n// ponytail: naive heuristic\nvar x = 1\n"), 0o644)
	// 解释性提及（规则说明注释，非债务）→ 跳过。
	file3 := filepath.Join(root, "d.go")
	os.WriteFile(file3, []byte("package d\n\n// ponytail: comment naming the ceiling and upgrade path\nvar y = 2\n"), 0o644)
	// 文档文件（.md）→ 不扫。
	file4 := filepath.Join(root, "c.md")
	os.WriteFile(file4, []byte("# Ponytail rules\n\n// ponytail: naive heuristic in docs\n"), 0o644)

	args, _ := json.Marshal(map[string]string{"root": root})
	out, err := execDebt(args)
	if err != nil {
		t.Fatalf("execDebt error: %v", err)
	}

	if !strings.Contains(out, "2 markers") {
		t.Fatalf("want 2 markers, got:\n%s", out)
	}
	if !strings.Contains(out, "1 with no trigger") {
		t.Fatalf("want 1 no-trigger, got:\n%s", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Fatalf("files missing from ledger:\n%s", out)
	}
	if strings.Contains(out, "c.md") || strings.Contains(out, "d.go") {
		t.Fatalf("explanatory/doc should not be markers:\n%s", out)
	}
	if !strings.Contains(out, "no-trigger") {
		t.Fatalf("no-trigger tag missing:\n%s", out)
	}
}

// TestExecDebt_Empty 空目录无标记 → Clean ledger。
func TestExecDebt_Empty(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"root": t.TempDir()})
	out, err := execDebt(args)
	if err != nil {
		t.Fatalf("execDebt error: %v", err)
	}
	if !strings.Contains(out, "Clean ledger") {
		t.Fatalf("want clean ledger, got: %s", out)
	}
}

// TestHasUpgrade 升级触发条件启发判断。
func TestHasUpgrade(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"global lock, per-account locks if throughput matters", true},
		{"naive heuristic", false}, // 无触发条件 → no-trigger
		{"O(n²) scan, upgrade when data grows", true},
	}
	for _, c := range cases {
		if got := hasUpgrade(c.text); got != c.want {
			t.Errorf("hasUpgrade(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// TestIsExplanatory 解释性提及判定。
func TestIsExplanatory(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"comment naming the ceiling and upgrade path", true},
		{"固定上限 3，需要无限对话再扩展", false},
		{"global lock, per-account locks if throughput matters", false},
	}
	for _, c := range cases {
		if got := isExplanatory(c.text); got != c.want {
			t.Errorf("isExplanatory(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
