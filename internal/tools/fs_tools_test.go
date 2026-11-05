// 文件工具测试：覆盖 fs_read / fs_write / fs_apply_diff / fs_list / fs_search。
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestWorkspace(t *testing.T) string {
	t.Helper()
	writeGuardTestSetup(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello world\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "code.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeGuardTestSetup 清空全局写防护记录表（进程级共享状态，避免测试间串扰）。
func writeGuardTestSetup(t *testing.T) {
	t.Helper()
	lastReadMtime = sync.Map{}
	writeLocks = sync.Map{}
}

// TestWriteGuard_ConcurrentModifyBlock 写前新鲜度校验端到端（2026-10-05 复盘
// 修复回归）：fs_read 后文件被外部改写（模拟另一会话写入）→ fs_write 与
// fs_apply_diff 均拒绝盲写并提示重读；重读后恢复正常编辑。
func TestWriteGuard_ConcurrentModifyBlock(t *testing.T) {
	ws := newTestWorkspace(t)
	path := filepath.Join(ws, "hello.txt")
	read := &ReadFileTool{Workspace: ws}
	write := &WriteFileTool{Workspace: ws}
	diff := &ApplyDiffTool{Workspace: ws}

	// 1) fs_read 建立读取基线。
	if _, err := read.Execute(context.Background(), `{"path":"hello.txt"}`); err != nil {
		t.Fatalf("fs_read: %v", err)
	}

	// 2) 外部（另一会话）改写同一文件：内容与 mtime 均变化。
	future := time.Now().Add(5 * time.Second)
	if err := os.WriteFile(path, []byte("changed by another session\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Skipf("Chtimes unsupported: %v", err)
	}

	// 3) fs_write 拒绝盲写（冲突在 SEARCH 匹配之前即被拦截）。
	if _, err := write.Execute(context.Background(), `{"path":"hello.txt","content":"blind overwrite"}`); err == nil || !strings.Contains(err.Error(), "write conflict") {
		t.Errorf("fs_write should reject stale write, got: %v", err)
	}

	// 4) fs_apply_diff 同样拒绝（SEARCH 本可命中，但文件已并发变更）。
	if _, err := diff.Execute(context.Background(), `{"path":"hello.txt","edits":[{"search":"hello world","replace":"x"}]}`); err == nil || !strings.Contains(err.Error(), "write conflict") {
		t.Errorf("fs_apply_diff should reject stale edit, got: %v", err)
	}

	// 5) 重读后放行（新鲜基线建立）。
	if _, err := read.Execute(context.Background(), `{"path":"hello.txt"}`); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if _, err := diff.Execute(context.Background(), `{"path":"hello.txt","edits":[{"search":"changed by","replace":"merged"}]}`); err != nil {
		t.Fatalf("edit after re-read should pass: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "merged") {
		t.Errorf("edit after re-read not applied: %q", b)
	}
}

// TestApplyDiff_AutoRereadRetry 基线后文件被并发改写但 mtime 落在 2s 容差内
// （未触发 write conflict）时，旧 SEARCH 落空 → 工具写锁内自动重读磁盘最新内容
// 并重跑全部编辑一次（2026-10-05 复盘：多会话同工作区编辑，mtime 粒度粗时
// writeGuard 放行但模型 SEARCH 已过期，旧实现只会报 SEARCH block not found）。
func TestApplyDiff_AutoRereadRetry(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "code.txt")
	// 初始内容（作为 fs_read 基线）。
	if err := os.WriteFile(path, []byte("base line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := &ReadFileTool{Workspace: ws}
	diff := &ApplyDiffTool{Workspace: ws}

	// 1) fs_read 建立基线（记录当前 mtime）。
	if _, err := read.Execute(context.Background(), `{"path":"code.txt"}`); err != nil {
		t.Fatalf("fs_read: %v", err)
	}

	// 2) 外部改写：内容换成新版本（新增 v1 old），mtime 设为基线 -1s（2s 容差内，绕过 write conflict）。
	if err := os.WriteFile(path, []byte("v1 old\nbase line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err == nil {
		if err := os.Chtimes(path, fi.ModTime().Add(-1*time.Second), fi.ModTime().Add(-1*time.Second)); err != nil {
			t.Skipf("Chtimes unsupported: %v", err)
		}
	}

	// 3) 旧 SEARCH（v1 old 不在基线内容中）→ 首次失败 → 自动重读新内容并重试 → 应成功。
	//    若自动重读未生效，旧实现会报 SEARCH block not found（测试会失败）。
	if _, err := diff.Execute(context.Background(), `{"path":"code.txt","edits":[{"search":"v1 old","replace":"v3 merged"}]}`); err != nil {
		t.Fatalf("auto re-read retry should succeed: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "v3 merged") {
		t.Errorf("retried edit not applied: %q", b)
	}
	if !strings.Contains(string(b), "base line") {
		t.Errorf("should keep concurrent update: %q", b)
	}
}

// TestWriteGuard_NeverReadPass 首次接触（从未 fs_read）不拦截：新建文件与
// 凭自身知识改写是合法场景（防误伤边界）。
func TestWriteGuard_NeverReadPass(t *testing.T) {
	ws := newTestWorkspace(t)
	write := &WriteFileTool{Workspace: ws}
	diff := &ApplyDiffTool{Workspace: ws}

	if _, err := write.Execute(context.Background(), `{"path":"fresh.txt","content":"x"}`); err != nil {
		t.Fatalf("new file write must pass: %v", err)
	}
	// 已存在但从未读过的文件（工作区预置）→ 不拦截。
	if _, err := diff.Execute(context.Background(), `{"path":"hello.txt","edits":[{"search":"hello world","replace":"y"}]}`); err != nil {
		t.Fatalf("never-read file edit must pass: %v", err)
	}
}

// TestWriteGuard_SelfWriteChain 连续编辑不误伤：本会话写过后再发编辑
// 不应被自身写入误判为并发冲突（fs_apply_diff 逐段追加的合法工作流）。
func TestWriteGuard_SelfWriteChain(t *testing.T) {
	ws := newTestWorkspace(t)
	read := &ReadFileTool{Workspace: ws}
	write := &WriteFileTool{Workspace: ws}
	diff := &ApplyDiffTool{Workspace: ws}

	if _, err := read.Execute(context.Background(), `{"path":"hello.txt"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Execute(context.Background(), `{"path":"hello.txt","content":"v1\n"}`); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// 同会话紧接着的第二笔编辑（未重读）必须放行。
	if _, err := diff.Execute(context.Background(), `{"path":"hello.txt","edits":[{"search":"v1","replace":"v2"}]}`); err != nil {
		t.Fatalf("chained edit after own write must pass: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(ws, "hello.txt"))
	if !strings.Contains(string(b), "v2") {
		t.Errorf("chained edit not applied: %q", b)
	}
}

func TestReadFileTool(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &ReadFileTool{Workspace: ws}

	out, err := tool.Execute(context.Background(), `{"path":"hello.txt"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("output missing content: %q", out)
	}

	// 越界路径拒绝。
	if _, err := tool.Execute(context.Background(), `{"path":"../secret.txt"}`); err == nil {
		t.Error("expected error for path escape")
	}

	// limit/offset 类型宽容（修复③回归）：模型把 integer 参数传成字符串
	//（历史 schema 曾声明 string）不得报「类型解析异常」。
	out, err = tool.Execute(context.Background(), `{"path":"hello.txt","offset":"1","limit":"1"}`)
	if err != nil {
		t.Fatalf("string-typed offset/limit rejected: %v", err)
	}
	if !strings.Contains(out, "hello world") || strings.Contains(out, "line2") {
		t.Errorf("string offset/limit semantics wrong: %q", out)
	}

	// number 形态照常工作。
	out, err = tool.Execute(context.Background(), `{"path":"hello.txt","offset":2,"limit":1}`)
	if err != nil {
		t.Fatalf("number offset/limit rejected: %v", err)
	}
	if !strings.Contains(out, "line2") {
		t.Errorf("number offset/limit semantics wrong: %q", out)
	}

	// 非法 limit 报清晰错误。
	if _, err := tool.Execute(context.Background(), `{"path":"hello.txt","limit":"abc"}`); err == nil {
		t.Error("expected error for non-numeric limit")
	}
}

func TestWriteFileTool(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &WriteFileTool{Workspace: ws}

	if _, err := tool.Execute(context.Background(), `{"path":"new.txt","content":"abc"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(ws, "new.txt"))
	if err != nil || string(b) != "abc" {
		t.Errorf("written file mismatch: %q err=%v", b, err)
	}
}

func TestApplyDiffTool(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &ApplyDiffTool{Workspace: ws}

	args := `{"path":"hello.txt","edits":[{"search":"hello world","replace":"hi kaelvoris"}]}`
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(ws, "hello.txt"))
	if !strings.Contains(string(b), "hi kaelvoris") {
		t.Errorf("diff not applied: %q", b)
	}
}

func TestListFilesTool(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &ListFilesTool{Workspace: ws}

	out, err := tool.Execute(context.Background(), `{"path":"."}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "hello.txt") || !strings.Contains(out, "sub") {
		t.Errorf("listing incomplete: %q", out)
	}
}

func TestSearchFilesTool(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &SearchFilesTool{Workspace: ws}

	out, err := tool.Execute(context.Background(), `{"regex":"line2"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "hello.txt") {
		t.Errorf("search missed match: %q", out)
	}
}

// 优化1 回归：fs_apply_diff 容错匹配（对齐 Zoo-Code MultiSearchReplace）。

// TestApplyDiffToolTrimmedMatch 行级松匹配：模型给出的 SEARCH 带多余缩进仍能命中。
func TestApplyDiffToolTrimmedMatch(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &ApplyDiffTool{Workspace: ws}

	args := `{"path":"hello.txt","edits":[{"search":"    hello world\n    line2","replace":"one\ntwo"}]}`
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(ws, "hello.txt"))
	if string(b) != "one\ntwo\n" {
		t.Errorf("trimmed match result = %q, want %q", b, "one\ntwo\n")
	}
}

// TestApplyDiffToolLineNumberAnchor :start_line:N 行号锚定回退。
func TestApplyDiffToolLineNumberAnchor(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &ApplyDiffTool{Workspace: ws}

	args := `{"path":"hello.txt","edits":[{"search":":start_line:2\nline2\n","replace":"replaced"}]}`
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(ws, "hello.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(b) != "hello world\nreplaced\n" {
		t.Errorf("line-anchor result = %q, want %q", b, "hello world\nreplaced\n")
	}
}

// TestApplyDiffToolFuzzyBlockMatch 首尾行块锚 + 行相似度确认（阈值 0.9）。
func TestApplyDiffToolFuzzyBlockMatch(t *testing.T) {
	ws := t.TempDir()
	src := "function calc() {\n  return value + 1\n}\n"
	if err := os.WriteFile(filepath.Join(ws, "code.js"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &ApplyDiffTool{Workspace: ws}

	// SEARCH 中间行有一处小差异（编辑距离 1），块级模糊匹配应命中。
	args := `{"path":"code.js","edits":[{"search":"function calc() {\n  return value + 2\n}","replace":"function calc() {\n  return value + 3\n}"}]}`
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(ws, "code.js"))
	if err != nil || !strings.Contains(string(b), "value + 3") {
		t.Errorf("fuzzy replace not applied: %q err=%v", b, err)
	}
}

// TestApplyDiffToolNotFound 四种策略全部失败时报错并提示已尝试的策略链。
func TestApplyDiffToolNotFound(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &ApplyDiffTool{Workspace: ws}

	args := `{"path":"hello.txt","edits":[{"search":"totally different content","replace":"x"}]}`
	_, err := tool.Execute(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "tried exact") {
		t.Errorf("expected not-found error, got: %v", err)
	}
}

// TestApplyDiffToolNearestHint 未命中时报错附带「最相似候选块」定位提示
// （2026-10-03 第三轮复盘：旧报错无定位线索，模型盲改重试最多 11 连失败）。
func TestApplyDiffToolNearestHint(t *testing.T) {
	t.Helper()
	writeGuardTestSetup(t)
	ws := t.TempDir()
	src := "package main\n\nfunc oldName() {\n\treturn 1\n}\n\nfunc helper() {}\n"
	if err := os.WriteFile(filepath.Join(ws, "code.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &ApplyDiffTool{Workspace: ws}

	// SEARCH 首行命中但中间行差异过大（超过模糊阈值）→ 报错应带行号提示。
	args := `{"path":"code.go","edits":[{"search":"func oldName() {\n\treturn 999\n\textra lines here\n}\nfunc other() {\n\treturn 2\n}","replace":"x"}]}`
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "nearest similar block") && !strings.Contains(err.Error(), "re-read the file") {
		t.Errorf("error should carry nearest-block hint, got: %v", err)
	}

	// 完全无关的 SEARCH（首行无线索）→ 不硬猜，提示重读。
	args2 := `{"path":"code.go","edits":[{"search":"zzz no such line anywhere","replace":"x"}]}`
	_, err = tool.Execute(context.Background(), args2)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got: %v", err)
	}
}

// TestApplyDiffPlainDiffString edits 误传字符串（整段 SEARCH/REPLACE 标记文本）时
// 自动拆为编辑对（2026-10-03 复盘：会话 1791004025305 实证 invalid args 报错）。
func TestApplyDiffPlainDiffString(t *testing.T) {
	ws := t.TempDir()
	src := "hello world\nline2\n"
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &ApplyDiffTool{Workspace: ws}

	args := `{"path":"hello.txt","edits":"<<<<<<< SEARCH\nhello world\n=======\nhi world\n>>>>>>> REPLACE"}`
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("plain diff string should parse: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(ws, "hello.txt"))
	if string(b) != "hi world\nline2\n" {
		t.Errorf("plain diff result = %q", b)
	}
}

// TestApplyDiffMalformedEdits 畸形 edits 参数容错（2026-10-04 全量会话复盘：
// fs_apply_diff 失败率 10.2%，TOP 失败均为模型参数形态而非匹配问题）。
// 覆盖三种历史失败形态：嵌套 [{path,edits}]、字符串化 JSON 数组、同义字段名。
func TestApplyDiffMalformedEdits(t *testing.T) {
	ws := t.TempDir()
	src := "hello world\nline2\n"
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &ApplyDiffTool{Workspace: ws}

	// 1) 嵌套形态：path/edits 被误包进 edits[0]（历史样本实证）。
	nested := `{"edits":[{"path":"hello.txt","edits":[{"search":"hello world","replace":"hi world"}]}]}`
	if _, err := tool.Execute(context.Background(), nested); err != nil {
		t.Fatalf("nested path+edits should be unwrapped: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "hello.txt")); string(b) != "hi world\nline2\n" {
		t.Errorf("nested result = %q", b)
	}

	// 2) 字符串化 JSON 数组（模型把整个数组二次编码）。
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	strArr := `{"path":"hello.txt","edits":"[{\"search\":\"hello world\",\"replace\":\"hi again\"}]"}`
	if _, err := tool.Execute(context.Background(), strArr); err != nil {
		t.Fatalf("stringified array should parse: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "hello.txt")); string(b) != "hi again\nline2\n" {
		t.Errorf("stringified result = %q", b)
	}

	// 3) 同义字段名（old search / new search，历史样本实证）。
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	synonym := `{"path":"hello.txt","edits":[{"old search":"hello world","new search":"hi third"}]}`
	if _, err := tool.Execute(context.Background(), synonym); err != nil {
		t.Fatalf("synonym field names should be normalized: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "hello.txt")); string(b) != "hi third\nline2\n" {
		t.Errorf("synonym result = %q", b)
	}

	// 4) 嵌套+同义变体混合：内层 edits 是字符串化数组。
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	mixed := `{"edits":[{"path":"hello.txt","edits":"[{\"search\":\"line2\",\"replace\":\"line two\"}]"}]}`
	if _, err := tool.Execute(context.Background(), mixed); err != nil {
		t.Fatalf("nested stringified inner edits should parse: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "hello.txt")); string(b) != "hello world\nline two\n" {
		t.Errorf("mixed result = %q", b)
	}

	// 5) 真正无法解析时仍报清晰错误（不误报）。
	bad := `{"edits":[{"unknown_field":"x"}]}`
	if _, err := tool.Execute(context.Background(), bad); err == nil {
		t.Error("unknown-only edits should still error")
	}
}

// TestIsProtectedRelPath 敏感路径清单（对齐 RooProtectedController）。
func TestIsProtectedRelPath(t *testing.T) {
	protected := []string{
		".env", ".env.local", ".env.production", "config/.env",
		"server.pem", "server.key", "cert.p12", "app.keystore",
		"id_rsa", ".ssh/id_ed25519", ".netrc", ".aws/credentials",
		".git/config", ".git/hooks/pre-commit", ".rooignore",
	}
	for _, p := range protected {
		if !IsProtectedRelPath(p) {
			t.Errorf("%q should be protected", p)
		}
	}
	plain := []string{
		"main.go", "src/app.ts", "environment.go", "keys.txt",
		"docs/env.md", "vendor.keynote", "sub/readme.txt",
	}
	for _, p := range plain {
		if IsProtectedRelPath(p) {
			t.Errorf("%q should NOT be protected", p)
		}
	}
}

// 优化2 回归：.rooignore/.gitignore 过滤（fs_list / fs_search 接入）。
func TestIgnoreMatcher(t *testing.T) {
	ws := t.TempDir()
	roo := "secret*\n!secret-keep.txt\nlogs/\n"
	if err := os.WriteFile(filepath.Join(ws, ".rooignore"), []byte(roo), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"secret.txt", "secret-keep.txt", "normal.txt", "logs/a.txt", "logs/deep/b.txt"} {
		p := filepath.Join(ws, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m := loadIgnoreMatcher(ws)
	if m == nil {
		t.Fatal("expected ignore matcher from .rooignore")
	}
	if !m.ignore("secret.txt", false) {
		t.Error("secret.txt should be ignored")
	}
	if m.ignore("secret-keep.txt", false) {
		t.Error("secret-keep.txt should be negated back to visible")
	}
	if !m.ignore("logs/a.txt", false) || !m.ignore("logs/deep/b.txt", false) {
		t.Error("logs/ subtree should be ignored via ancestor dir")
	}
	if m.ignore("normal.txt", false) {
		t.Error("normal.txt should not be ignored")
	}

	// fs_list 递归接入：被忽略项不出现在清单中。
	list := &ListFilesTool{Workspace: ws}
	out, err := list.Execute(context.Background(), `{"recursive":true}`)
	if err != nil {
		t.Fatalf("fs_list: %v", err)
	}
	if strings.Contains(out, "secret.txt") || strings.Contains(out, "logs/") {
		t.Errorf("ignored entries leaked into listing: %q", out)
	}
	if !strings.Contains(out, "secret-keep.txt") || !strings.Contains(out, "normal.txt") {
		t.Errorf("visible entries missing from listing: %q", out)
	}

	// fs_search 纯 Go 回退路径接入（rg 原生遵守 .gitignore，.rooignore 走否定 glob）。
	out, err = walkSearch(ws, ws, "x", "", nil)
	if err != nil {
		t.Fatalf("walkSearch: %v", err)
	}
	if strings.Contains(out, "logs/") {
		t.Errorf("ignored files leaked into search: %q", out)
	}
}

// fs_search path 定向搜索（性能优化回归）：子目录/单文件范围、白名单目录、
// 越界拒绝、单文件命中上限、大文件跳过。
func TestSearchFilesTool_ScopedPath(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &SearchFilesTool{Workspace: ws}

	// ① path 定向子目录：命中 sub/code.go，不污染工作区根。
	out, err := tool.Execute(context.Background(), `{"regex":"package","path":"sub"}`)
	if err != nil {
		t.Fatalf("scoped search: %v", err)
	}
	if !strings.Contains(out, "sub/code.go") {
		t.Errorf("expected hit in sub/code.go, got: %q", out)
	}

	// ② path 指向单文件：仅在该文件内匹配。
	out, err = tool.Execute(context.Background(), `{"regex":"hello","path":"hello.txt"}`)
	if err != nil {
		t.Fatalf("single-file search: %v", err)
	}
	if !strings.Contains(out, "hello.txt") {
		t.Errorf("expected hit in hello.txt, got: %q", out)
	}

	// ③ path 越界拒绝（复用 resolveInWorkspace 安全语义）。
	if _, err := tool.Execute(context.Background(), `{"regex":"x","path":"../outside"}`); err == nil {
		t.Error("expected error for path escape")
	}

	// ④ path 不存在时明确报错。
	if _, err := tool.Execute(context.Background(), `{"regex":"x","path":"no/such/dir"}`); err == nil {
		t.Error("expected error for missing path")
	}

	// ⑤ 单文件命中上限：同一文件命中 30 次，最多输出 maxHitsPerFile 行，
	// 避免生成产物/锁文件整文件刷屏。
	dir := t.TempDir()
	w2 := &SearchFilesTool{Workspace: dir}
	var rep strings.Builder
	for i := 0; i < 30; i++ {
		rep.WriteString("needle line\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "repetitive.log"), []byte(rep.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = w2.Execute(context.Background(), `{"regex":"needle"}`)
	if err != nil {
		t.Fatalf("repetitive search: %v", err)
	}
	if n := strings.Count(out, "needle line"); n != maxHitsPerFile {
		t.Errorf("expected %d hits per file cap, got %d: %q", maxHitsPerFile, n, out)
	}

	// ⑥ 大文件跳过：超过 maxSearchFileSize 的文件不整读进内存。
	big := filepath.Join(dir, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// 截断至 maxSearchFileSize+1 字节（稀疏写头部 + Seek 到位即可，不占盘）。
	if _, err := f.WriteString("needle\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(int64(maxSearchFileSize), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("tail"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err = w2.Execute(context.Background(), `{"regex":"needle"}`)
	if err != nil {
		t.Fatalf("big-file search: %v", err)
	}
	if strings.Contains(out, "big.bin") {
		t.Errorf("oversized file should be skipped, got: %q", out)
	}
}

// ignore_dirs 参数与默认忽略目录回归（纯 Go 路径，不依赖 rg 是否可用）：
// ① 默认忽略 node_modules/dist 等目录（缺 .gitignore 也不漏扫）；
// ② 用户 ignore_dirs 名字形态（任意层级命中）与路径形态（仅锚定路径命中）；
// ③ path 显式定向到忽略目录内部时豁免（根回调不 SkipDir）；
// ④ parseIgnoreDirs 参数解析边界。
func TestSearchFilesTool_IgnoreDirs(t *testing.T) {
	ws := t.TempDir()
	mk := func(rel, content string) {
		t.Helper()
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("src/app.go", "needle here\n")
	mk("node_modules/pkg/index.js", "needle in node_modules\n") // 默认忽略：无 .gitignore 也跳过
	mk("dist/bundle.js", "needle in dist\n")                    // 默认忽略
	mk("logs/a.log", "needle in logs\n")                        // 默认不忽略
	mk("sub/logs/b.log", "needle in sub/logs\n")                // 默认不忽略

	// ① 全库搜索：默认忽略目录（node_modules/dist）不命中，正常目录命中。
	out, err := walkSearch(ws, ws, "needle", "", nil)
	if err != nil {
		t.Fatalf("walkSearch: %v", err)
	}
	if strings.Contains(out, "node_modules") || strings.Contains(out, "dist/") {
		t.Errorf("default-ignored dirs leaked into search: %q", out)
	}
	if !strings.Contains(out, "src/app.go") {
		t.Errorf("expected hit in src/app.go, got: %q", out)
	}

	// ② 用户 ignore_dirs：名字形态忽略任意层级 logs；路径形态仅锚定 sub/tmp。
	out, err = walkSearch(ws, ws, "needle", "", []string{"logs", "sub/tmp"})
	if err != nil {
		t.Fatalf("walkSearch with ignore_dirs: %v", err)
	}
	if strings.Contains(out, "logs/") {
		t.Errorf("user-ignored dir (name form) leaked into search: %q", out)
	}
	if !strings.Contains(out, "src/app.go") {
		t.Errorf("expected hit in src/app.go, got: %q", out)
	}
	mk("sub/tmp/c.go", "needle in sub/tmp\n")
	mk("other/tmp/c.log", "needle in other/tmp\n") // 路径形态只锚 sub/tmp，other/tmp 应命中
	out, err = walkSearch(ws, ws, "needle", "", []string{"sub/tmp"})
	if err != nil {
		t.Fatalf("walkSearch path-form ignore: %v", err)
	}
	if strings.Contains(out, "sub/tmp") {
		t.Errorf("user-ignored dir (path form) leaked into search: %q", out)
	}
	if !strings.Contains(out, "other/tmp/c.log") {
		t.Errorf("unrelated other/tmp should not be ignored, got: %q", out)
	}

	// ③ path 显式定向到默认忽略目录内部：豁免忽略，能搜到其中内容。
	out, err = walkSearch(ws, filepath.Join(ws, "node_modules"), "needle", "", nil)
	if err != nil {
		t.Fatalf("targeted search inside ignored dir: %v", err)
	}
	if !strings.Contains(out, "node_modules/pkg/index.js") {
		t.Errorf("targeted search inside default-ignored dir should hit, got: %q", out)
	}

	// ④ parseIgnoreDirs 解析：空白/尾斜杠/./ 前缀/空段。
	got := parseIgnoreDirs(" logs , tmp/, ./a/b , , . ")
	want := []string{"logs", "tmp", "a/b"}
	if len(got) != len(want) {
		t.Fatalf("parseIgnoreDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseIgnoreDirs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// rg 缺失回退路径回归（无 rg 环境下 fs_search 仍可用且行为一致）。
func TestSearchFilesTool_WalkFallback(t *testing.T) {
	ws := newTestWorkspace(t)
	out, err := walkSearch(ws, ws, "hello", "", nil)
	if err != nil {
		t.Fatalf("walkSearch: %v", err)
	}
	if !strings.Contains(out, "hello.txt:1:") {
		t.Errorf("expected hello.txt hit at line 1, got: %q", out)
	}
}

// fs_read 分页——对齐 Zoo-Code DEFAULT_LINE_LIMIT=2000：默认每次 2000 行
// （250 行小文件一次读完，不再 20 次翻页）、limit 超限压回 2000、
// 越界 offset 提示、分页 header 含续读偏移（仅超出 2000 行的大文件出现）。
func TestReadFileTool_Pagination(t *testing.T) {
	ws := t.TempDir()
	// 250 行小文件：默认参数应一次读完（旧实现 100 行/次要翻 3 次）。
	var b strings.Builder
	for i := 1; i <= 250; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	if err := os.WriteFile(filepath.Join(ws, "small.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// 2500 行大文件：默认参数读前 2000 行，header 提示续读 offset=2001。
	var big strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&big, "line%d\n", i)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &ReadFileTool{Workspace: ws}

	// 默认参数 + 小文件：整读，无续读提示。
	out, err := tool.Execute(context.Background(), `{"path":"small.txt"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "showing 1-250") {
		t.Errorf("small file must read whole in one call: %q", headerOf(out))
	}
	if strings.Contains(out, "more lines remain") {
		t.Error("small file must not claim more lines remain")
	}

	// 默认参数 + 大文件：读前 2000 行，header 提示续读 offset=2001。
	out, err = tool.Execute(context.Background(), `{"path":"big.txt"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "showing 1-2000") || !strings.Contains(out, "offset=2001") {
		t.Errorf("default paging header wrong: %q", headerOf(out))
	}
	if strings.Contains(out, "line2001") || strings.Contains(out, "line2500") {
		t.Error("default read must not exceed 2000 lines")
	}
	if !strings.Contains(out, "line2000") {
		t.Error("line 2000 missing from default read")
	}

	// offset 续读：第二页读到文件末尾（500 行），无续读提示。
	out, err = tool.Execute(context.Background(), `{"path":"big.txt","offset":2001}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "showing 2001-2500") {
		t.Errorf("second page wrong: %q", headerOf(out))
	}
	if strings.Contains(out, "more lines remain") {
		t.Error("last page must not claim more lines remain")
	}

	// limit 上限 2000：limit=5000 被压到 2000，从 offset 501 读到 2500 行末尾。
	out, err = tool.Execute(context.Background(), `{"path":"big.txt","offset":501,"limit":5000}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "showing 501-2500") {
		t.Errorf("clamped limit page wrong: %q", headerOf(out))
	}
	if strings.Contains(out, "more lines remain") {
		t.Error("last page must not claim more lines remain")
	}
	if !strings.Contains(out, "line2500") {
		t.Error("line 2500 missing from last page")
	}

	// 越界 offset。
	out, err = tool.Execute(context.Background(), `{"path":"big.txt","offset":9999}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "offset 9999 beyond end of file") {
		t.Errorf("expected beyond-end message, got %q", out)
	}

	// 空文件。
	if err := os.WriteFile(filepath.Join(ws, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err = tool.Execute(context.Background(), `{"path":"empty.txt"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "(empty file)" {
		t.Errorf("empty file output = %q, want \"(empty file)\"", out)
	}
}

// TestReadFileTool_WholeReadGuard G3 整读护栏（CCGS validate-read 对标）：
// >40KB 文件「从头整读」返回索引而非全文；带 offset 分页不受影响；
// 小文件完全不受影响。
func TestReadFileTool_WholeReadGuard(t *testing.T) {
	ws := t.TempDir()
	tool := &ReadFileTool{Workspace: ws}

	// 超阈值 markdown：标题索引。
	var md strings.Builder
	md.WriteString("# Big Design Doc\n\nintro paragraph\n\n")
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&md, "## Section %d\n\nfiller paragraph %d with enough words to take space.\n\n", i, i)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.md"), []byte(md.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), `{"path":"big.md"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "whole-read guard") || !strings.Contains(out, "Heading index") {
		t.Errorf("guard header/index missing: %.200q", out)
	}
	if !strings.Contains(out, "## Section 599") {
		t.Error("heading index should list headings (including tail ones)")
	}
	if strings.Contains(out, "filler paragraph 250") {
		t.Error("guard must not return full body text")
	}
	if !strings.Contains(out, "offset=") {
		t.Error("guard should guide offset-based section reads")
	}

	// 同一文件带 offset 分页：不受护栏影响，返回正文。
	out, err = tool.Execute(context.Background(), `{"path":"big.md","offset":5,"limit":3}`)
	if err != nil {
		t.Fatalf("Execute with offset: %v", err)
	}
	if strings.Contains(out, "whole-read guard") || !strings.Contains(out, "filler paragraph") {
		t.Errorf("offset read must bypass guard and return content: %.200q", out)
	}

	// 超阈值 Go 文件：顶层声明索引。
	var goSrc strings.Builder
	goSrc.WriteString("package big\n\n")
	for i := 0; i < 800; i++ {
		fmt.Fprintf(&goSrc, "func BigFn%03d(x int) int {\n\ty := x * %d\n\treturn y\n}\n\n", i, i)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.go"), []byte(goSrc.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = tool.Execute(context.Background(), `{"path":"big.go"}`)
	if err != nil {
		t.Fatalf("Execute go: %v", err)
	}
	if !strings.Contains(out, "Top-level declaration index") || !strings.Contains(out, "func BigFn299") {
		t.Errorf("go decl index wrong: %.300q", out)
	}
	if strings.Contains(out, "\ty := x") {
		t.Error("go guard must not return function bodies")
	}

	// 小文件：不受影响（无护栏，正常整读）。
	if err := os.WriteFile(filepath.Join(ws, "small.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = tool.Execute(context.Background(), `{"path":"small.txt"}`)
	if err != nil {
		t.Fatalf("Execute small: %v", err)
	}
	if strings.Contains(out, "whole-read guard") || !strings.Contains(out, "hello world") {
		t.Errorf("small file must read normally: %q", out)
	}
}

// headerOf 提取分页输出的 header 行（测试断言用）。
func headerOf(out string) string {
	if i := strings.Index(out, "\n"); i >= 0 {
		return out[:i]
	}
	return out
}

// ---------- 只读白名单（2026-10-01 复盘改造回归） ----------

// TestReadOnlyWhitelist 只读白名单：fs_read/fs_list 可读 ~/.kaelvoris/sessions
// 等白名单目录（绝对路径），写类工具仍拒绝；工作区外普通路径双向拒绝。
func TestReadOnlyWhitelist(t *testing.T) {
	ws := newTestWorkspace(t)

	// 临时把白名单指向测试目录（保存/恢复，避免污染其他用例）。
	saved := ReadOnlyWhitelist
	defer func() { ReadOnlyWhitelist = saved }()
	sessDir := t.TempDir()
	ReadOnlyWhitelist = []string{sessDir}

	// 白名单目录内准备会话存档模拟文件。
	sessFile := filepath.Join(sessDir, "s-1.json")
	sessContent := "{\"id\":\"s-1\"}\n"
	if err := os.WriteFile(sessFile, []byte(sessContent), 0o644); err != nil {
		t.Fatal(err)
	}
	sessPath := filepath.ToSlash(sessFile)

	read := &ReadFileTool{Workspace: ws}
	list := &ListFilesTool{Workspace: ws}
	write := &WriteFileTool{Workspace: ws}
	diff := &ApplyDiffTool{Workspace: ws}
	ctx := context.Background()

	// 1) fs_read 白名单绝对路径可读。
	out, err := read.Execute(ctx, `{"path":"`+sessPath+`"}`)
	if err != nil {
		t.Fatalf("fs_read whitelist path rejected: %v", err)
	}
	if !strings.Contains(out, `"id":"s-1"`) {
		t.Errorf("fs_read whitelist content wrong: %q", out)
	}

	// 2) fs_list 白名单绝对路径可列（且不受工作区 ignore 过滤影响）。
	out, err = list.Execute(ctx, `{"path":"`+filepath.ToSlash(sessDir)+`","recursive":true}`)
	if err != nil {
		t.Fatalf("fs_list whitelist path rejected: %v", err)
	}
	if !strings.Contains(out, "s-1.json") {
		t.Errorf("fs_list whitelist missing file: %q", out)
	}

	// 3) fs_write / fs_apply_diff 对白名单路径仍拒绝（只读语义）。
	if _, err := write.Execute(ctx, `{"path":"`+sessPath+`","content":"x"}`); err == nil {
		t.Error("fs_write must reject whitelist path")
	}
	if _, err := diff.Execute(ctx, `{"path":"`+sessPath+`","edits":[{"search":"a","replace":"b"}]}`); err == nil {
		t.Error("fs_apply_diff must reject whitelist path")
	}

	// 4) 白名单外的工作区外路径：读/写都仍拒绝。
	other := filepath.ToSlash(filepath.Join(filepath.Dir(ws), "outside.txt"))
	if _, err := read.Execute(ctx, `{"path":"`+other+`"}`); err == nil {
		t.Error("fs_read must reject non-whitelisted outside path")
	}
	if _, err := write.Execute(ctx, `{"path":"`+other+`","content":"x"}`); err == nil {
		t.Error("fs_write must reject non-whitelisted outside path")
	}

	// 5) 白名单目录下子目录逃逸（../）仍拒绝。
	escaped := filepath.ToSlash(filepath.Join(sessDir, "..", "kaelvoris-config.json"))
	if _, err := read.Execute(ctx, `{"path":"`+escaped+`"}`); err == nil {
		t.Error("fs_read must reject whitelist-sibling escape via ..")
	}
}

// TestWhitelistMemoryDocWrite 白名单记忆文档写入豁免（2026-10-04 会话复盘）：
// 提示词要求模型用 fs_write 维护 ~/.kaelvoris/sessions/<id>.memory.md，但旧实现
// 写类工具一律拒绝白名单路径 → 自相矛盾（历史样本 2 例 escapes workspace 失败）。
// 仅 .memory.md 后缀放行，会话 JSON 本体仍严格只读。
func TestWhitelistMemoryDocWrite(t *testing.T) {
	ws := newTestWorkspace(t)
	saved := ReadOnlyWhitelist
	defer func() { ReadOnlyWhitelist = saved }()
	sessDir := t.TempDir()
	ReadOnlyWhitelist = []string{sessDir}

	write := &WriteFileTool{Workspace: ws}
	diff := &ApplyDiffTool{Workspace: ws}
	ctx := context.Background()

	// 1) .memory.md 记忆文档：写类工具放行。
	memPath := filepath.ToSlash(filepath.Join(sessDir, "1791076789016.memory.md"))
	if _, err := write.Execute(ctx, `{"path":"`+memPath+`","content":"# memory"}`); err != nil {
		t.Fatalf("fs_write memory doc rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessDir, "1791076789016.memory.md")); err != nil {
		t.Fatalf("memory doc not written: %v", err)
	}

	// 2) 会话 JSON 本体：仍严格拒绝（防会话存档被改写）。
	sessPath := filepath.ToSlash(filepath.Join(sessDir, "1791076789016.json"))
	if _, err := write.Execute(ctx, `{"path":"`+sessPath+`","content":"{}"}`); err == nil {
		t.Error("fs_write must still reject session JSON")
	}
	if _, err := diff.Execute(ctx, `{"path":"`+sessPath+`","edits":[{"search":"a","replace":"b"}]}`); err == nil {
		t.Error("fs_apply_diff must still reject session JSON")
	}

	// 3) .corrupt 隔离文件：仍拒绝。
	corrupt := filepath.ToSlash(filepath.Join(sessDir, "x.json.corrupt"))
	if _, err := write.Execute(ctx, `{"path":"`+corrupt+`","content":"x"}`); err == nil {
		t.Error("fs_write must reject .corrupt file")
	}

	// 4) 白名单外的 .memory.md 同名文件：不豁免（必须同时命中白名单）。
	outsideMem := filepath.ToSlash(filepath.Join(filepath.Dir(ws), "fake.memory.md"))
	if _, err := write.Execute(ctx, `{"path":"`+outsideMem+`","content":"x"}`); err == nil {
		t.Error("fs_write must reject non-whitelist memory doc")
	}
}

// TestResolvePathInUnits resolvePathIn 单元断言（相对/绝对/白名单分支）。
func TestResolvePathInUnits(t *testing.T) {
	ws := t.TempDir()
	wl := t.TempDir()
	old := ReadOnlyWhitelist
	ReadOnlyWhitelist = []string{wl}
	defer func() { ReadOnlyWhitelist = old }()

	// 相对路径仍解析到工作区。
	got, err := resolveInWorkspace(ws, "a/b.txt")
	if err != nil {
		t.Fatalf("relative resolve: %v", err)
	}
	if want, _ := filepath.Abs(filepath.Join(ws, "a", "b.txt")); got != want {
		t.Errorf("relative resolve = %q, want %q", got, want)
	}

	// 写路径拒绝白名单。
	if _, err := resolveWritePath(ws, wl); err == nil {
		t.Error("write path must not accept whitelist dir")
	}

	// 读路径接受白名单。
	if got, err = resolveInWorkspace(ws, wl); err != nil || filepath.Clean(got) != filepath.Clean(wl) {
		t.Errorf("read whitelist resolve = %q err=%v", got, err)
	}
}

// ---------- 工作区内绝对路径放行 + fs_read/fs_list 错误分类（2026-10-04 复盘） ----------

// TestResolveInWorkspaceAbsoluteInside 工作区内绝对路径（读+写）放行：
// 正斜杠/反斜杠形态、含 .. 折叠后仍在工作区的路径均须命中；
// 工作区外非白名单绝对路径仍双向拒绝（读+写）。
// 背景：旧实现绝对路径分支只查只读白名单，从不检查"本就在工作区内"，
// 跨 5+ 会话反复误报 escapes workspace，模型起步探索即被阻断。
func TestResolveInWorkspaceAbsoluteInside(t *testing.T) {
	ws := newTestWorkspace(t)
	// 正斜杠形态（模型常用输出形态）。
	wsSlash := filepath.ToSlash(ws)

	got, err := resolveInWorkspace(ws, wsSlash+"/a.txt")
	if err != nil {
		t.Fatalf("read: forward-slash abs inside workspace rejected: %v", err)
	}
	if want, _ := filepath.Abs(filepath.Join(ws, "a.txt")); got != want {
		t.Errorf("read: got %q, want %q", got, want)
	}

	got, err = resolveWritePath(ws, wsSlash+"/a.txt")
	if err != nil {
		t.Fatalf("write: forward-slash abs inside workspace rejected: %v", err)
	}
	if got != filepath.Clean(filepath.Join(ws, "a.txt")) {
		t.Errorf("write: got %q", got)
	}

	// 反斜杠原生形态（Windows）。
	got, err = resolveInWorkspace(ws, filepath.Join(ws, "sub", "code.go"))
	if err != nil {
		t.Fatalf("read: native abs inside workspace rejected: %v", err)
	}
	if want, _ := filepath.Abs(filepath.Join(ws, "sub", "code.go")); got != want {
		t.Errorf("read: got %q, want %q", got, want)
	}

	// 含 .. 折叠后仍在工作区：ws/sub/../hello.txt → ws/hello.txt，合法。
	folded := filepath.ToSlash(filepath.Join(ws, "sub", "..", "hello.txt"))
	got, err = resolveWritePath(ws, folded)
	if err != nil {
		t.Fatalf("write: folded .. abs inside workspace rejected: %v", err)
	}
	if want, _ := filepath.Abs(filepath.Join(ws, "hello.txt")); got != want {
		t.Errorf("write: folded got %q, want %q", got, want)
	}

	// .. 逃逸出工作区：折叠后越界，读+写均拒。
	escaped := filepath.ToSlash(filepath.Join(ws, "sub", "..", "..", "evil.txt"))
	if _, err := resolveInWorkspace(ws, escaped); err == nil {
		t.Error("read: .. escape must be rejected")
	}
	if _, err := resolveWritePath(ws, escaped); err == nil {
		t.Error("write: .. escape must be rejected")
	}

	// 工作区外非白名单绝对路径：读+写均拒（错误文案须带 escapes workspace）。
	outside := filepath.ToSlash(filepath.Join(filepath.Dir(ws), "outside.txt"))
	_, err = resolveInWorkspace(ws, outside)
	if err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Errorf("read: outside abs err = %v, want escapes workspace", err)
	}
	_, err = resolveWritePath(ws, outside)
	if err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Errorf("write: outside abs err = %v, want escapes workspace", err)
	}

	// 工作区内绝对路径经工具层端到端（fs_read/fs_write）可达。
	read := &ReadFileTool{Workspace: ws}
	if _, err := read.Execute(context.Background(), `{"path":"`+wsSlash+`/hello.txt"}`); err != nil {
		t.Fatalf("fs_read abs inside workspace rejected: %v", err)
	}
	write := &WriteFileTool{Workspace: ws}
	if _, err := write.Execute(context.Background(), `{"path":"`+wsSlash+`/out.txt","content":"x"}`); err != nil {
		t.Fatalf("fs_write abs inside workspace rejected: %v", err)
	}
}

// TestReadFileToolErrorFeedback fs_read 错误分类：目录路径 → is a directory 提示
// （不再透传 Windows "Incorrect function"）；不存在路径 → file not found 提示。
func TestReadFileToolErrorFeedback(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &ReadFileTool{Workspace: ws}
	ctx := context.Background()

	// 目录路径：报 is a directory + 指引 fs_list。
	if _, err := tool.Execute(ctx, `{"path":"sub"}`); err == nil ||
		!strings.Contains(err.Error(), "is a directory") {
		t.Errorf("directory path err = %v, want is-a-directory hint", err)
	}

	// 不存在路径：报 file not found + 指引 fs_list 父目录。
	if _, err := tool.Execute(ctx, `{"path":"no_such_file.txt"}`); err == nil ||
		!strings.Contains(err.Error(), "file not found") {
		t.Errorf("missing path err = %v, want file-not-found hint", err)
	}
}

// TestListFilesToolMissingPath fs_list 对不存在路径必须报错，
// 不得静默返回 (empty directory)（旧实现 WalkDir 吞根错误致模型瞎猜路径）。
func TestListFilesToolMissingPath(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := &ListFilesTool{Workspace: ws}
	ctx := context.Background()

	if _, err := tool.Execute(ctx, `{"path":"no_such_dir"}`); err == nil ||
		!strings.Contains(err.Error(), "path not found") {
		t.Errorf("missing dir err = %v, want path-not-found", err)
	}
	// 递归模式同样预检。
	if _, err := tool.Execute(ctx, `{"path":"no_such_dir","recursive":true}`); err == nil ||
		!strings.Contains(err.Error(), "path not found") {
		t.Errorf("missing dir (recursive) err = %v, want path-not-found", err)
	}

	// 真实存在的空目录：仍返回 (empty directory)，语义保留。
	if err := os.Mkdir(filepath.Join(ws, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(ctx, `{"path":"emptydir"}`)
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	if out != "(empty directory)" {
		t.Errorf("empty dir output = %q, want \"(empty directory)\"", out)
	}

	// 传文件路径：提示 is a file + 指引 fs_read（不再返回易误读的清单）。
	out, err = tool.Execute(ctx, `{"path":"hello.txt"}`)
	if err != nil {
		t.Fatalf("file path: %v", err)
	}
	if !strings.Contains(out, "is a file") {
		t.Errorf("file path output = %q, want is-a-file hint", out)
	}
}

// TestApplyDiffFailGuidance 失败引导回归（2026-10-06 复盘：同参重发 3-6 次死循环）：
// ① SEARCH 连续失败 ≥2 次 → 错误带 STOP 引导（fs_read/行锚点/整文件重写）；
// ② search==replace（写反）：报错必须指明交换字段；
// ②b search==replace 且原文未找到（写反/内容已变）：报错指明 identical；
// ③ edits 不是数组 / 缺 search 键 → 参数错误含可操作中文示例。
func TestApplyDiffFailGuidance(t *testing.T) {
	diffFailCount = sync.Map{}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &ApplyDiffTool{Workspace: ws}
	ctx := context.Background()

	// ① 同一 SEARCH 连续失败 2 次 → 第 2 次报 STOP（首报错误本身带 SEARCH not found）。
	args := `{"path":"a.txt","edits":[{"search":"totally missing","replace":"x"}]}`
	err1, err := tool.Execute(ctx, args)
	if err == nil || !strings.Contains(err.Error(), "SEARCH block not found") {
		t.Fatalf("1st fail: %v", err)
	}
	_ = err1
	_, err = tool.Execute(ctx, args)
	if err == nil || !strings.Contains(err.Error(), "STOP") || !strings.Contains(err.Error(), "fs_read") {
		t.Errorf("2nd fail should carry STOP guidance, got: %v", err)
	}

	// ②b search==replace 且原文未找到：报错指明 identical + 未找到（写反/文件已变）。
	if _, err = tool.Execute(ctx, `{"path":"a.txt","edits":[{"search":"not in file","replace":"not in file"}]}`); err == nil ||
		!strings.Contains(err.Error(), "identical") {
		t.Errorf("identical+not-found should say identical, got: %v", err)
	}

	// ③ edits 非数组（裸字符串）→ 参数错误带中文示例。
	if _, err = tool.Execute(ctx, `{"path":"a.txt","edits":"nope"}`); err == nil ||
		!strings.Contains(err.Error(), "edits 必须是数组") {
		t.Errorf("non-array edits should give shape hint, got: %v", err)
	}
	// ④ edits 数组缺 search 键 → 提示缺字段。
	if _, err = tool.Execute(ctx, `{"path":"a.txt","edits":[{"replace":"x"}]}`); err == nil ||
		!strings.Contains(err.Error(), "缺少非空 search") {
		t.Errorf("missing search should give field hint, got: %v", err)
	}

	// ⑤ 成功一次后计数清零：再次失败重新从 1 计（下次才 STOP）。
	if _, err = tool.Execute(ctx, `{"path":"a.txt","edits":[{"search":"hello world","replace":"你好"}]}`); err != nil {
		t.Fatalf("successful edit after failures: %v", err)
	}
	_, err = tool.Execute(ctx, args)
	if err == nil || strings.Contains(err.Error(), "STOP") {
		t.Errorf("after success, 1st fail should not be STOP, got: %v", err)
	}

	// ⑥ replace-exists 检测（对标 Aider）：SEARCH 未命中但多行 REPLACE 文本已在
	// 文件中 → 报错必须携带 replace-exists 提示（防重复编辑/写反复发）。
	if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(ctx, `{"path":"b.txt","edits":[{"search":"missing text here","replace":"beta\ngamma"}]}`)
	if err == nil || !strings.Contains(err.Error(), "REPLACE text is already present") {
		t.Errorf("replace-exists hint missing, got: %v", err)
	}
	// 单行 replace 不触发（误报率高）。
	_, err = tool.Execute(ctx, `{"path":"b.txt","edits":[{"search":"missing text here","replace":"beta"}]}`)
	if err == nil || strings.Contains(err.Error(), "REPLACE text is already present") {
		t.Errorf("single-line replace must not trigger replace-exists hint, got: %v", err)
	}
}

// TestNearestBlockHint_LowSimKeepsLineNo 相似度过低时不再丢弃行号线索
// （2026-10-28 会话复盘：cozeflow_panel.go 1074 行大文件连续失败，旧文案
// "changed significantly" 无任何位置信息，模型只能整文件盲重读浪费多轮）。
func TestNearestBlockHint_LowSimKeepsLineNo(t *testing.T) {
	content := "line one\nline two\nline three\n"
	// 完全无关的 SEARCH：bestSim 必然 < 0.6，但 first 行锚点仍能给出最近似行号。
	hint := nearestBlockHint(content, "completely unrelated text\nmore unrelated words\n")
	if !strings.Contains(hint, "line ") || !strings.Contains(hint, "resembles") {
		t.Errorf("low-similarity hint should keep line number, got: %q", hint)
	}
	if strings.Contains(hint, "changed significantly") {
		t.Errorf("legacy no-clue message should be gone, got: %q", hint)
	}
	// 空文件（无任何行）→ 无线索分支保留。
	empty := nearestBlockHint("", "anything")
	if empty == "" {
		t.Errorf("empty content should return non-empty guidance, got empty")
	}
}

// TestApplyDiffFailCountResetOnRead 回归（2026-10-30 publish-client.ps1 误报复盘）：
// STOP 计数必须被 fs_read 触达清零——STOP 引导的语义是「先 fs_read 再改」，模型照做
// 后凭最新内容构造的合法重试不应被旧计数再次 STOP（死锁式误报）。同参盲发防护不
// 减弱：清零后重新计数，连续 2 次仍 STOP。
func TestApplyDiffFailCountResetOnRead(t *testing.T) {
	diffFailCount = sync.Map{}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := &ApplyDiffTool{Workspace: ws}
	read := &ReadFileTool{Workspace: ws}
	ctx := context.Background()
	args := `{"path":"a.txt","edits":[{"search":"totally missing","replace":"x"}]}`

	// 连续失败 1 次（计数=1，未到 STOP 阈值）。
	if _, err := diff.Execute(ctx, args); err == nil {
		t.Fatal("1st fail should error")
	}
	// fs_read 触达：经 readGuardRecord 清零该文件失败计数。
	if _, err := read.Execute(ctx, `{"path":"a.txt"}`); err != nil {
		t.Fatalf("read: %v", err)
	}
	// 重读后再失败 1 次：不得出现 STOP（旧计数已清零，重新从 1 计起）。
	_, err := diff.Execute(ctx, args)
	if err == nil || strings.Contains(err.Error(), "STOP") {
		t.Errorf("after fs_read, next single failure must not STOP, got: %v", err)
	}
	// 清零后再累计到 2 次 → STOP 恢复生效（同参盲发防护不变）。
	if _, err = diff.Execute(ctx, args); err == nil || !strings.Contains(err.Error(), "STOP") {
		t.Errorf("2nd failure after reset should STOP again, got: %v", err)
	}
}
