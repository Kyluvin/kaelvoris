package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultRuleMatchesGo 内置默认规则命中 .go；未命中其他扩展名。
func TestDefaultRuleMatchesGo(t *testing.T) {
	r := NewRunner(t.TempDir(), nil)
	if got := r.match("a/b.go"); got == nil || got.Command != "go vet ./..." {
		t.Fatalf(".go 应命中默认规则, got %+v", got)
	}
	if got := r.match("a/b.ts"); got != nil {
		t.Fatalf(".ts 不应内置规则, got %+v", got)
	}
}

// TestAfterEditPassReturnsEmpty 检查通过返回空（无反馈）。
func TestAfterEditPassReturnsEmpty(t *testing.T) {
	ws := t.TempDir()
	write(t, filepath.Join(ws, "ok.go"), "package main\n")
	// 规则：对 .go 跑「exit 0」命令（跨平台安全：cmd /C exit 0 与 sh -c exit 0 等价）。
	r := &Runner{Workspace: ws, Timeout: 10, Rules: []Rule{{Ext: []string{".go"}, Command: "exit 0"}}}
	if fb := r.AfterEdit("ok.go"); fb != "" {
		t.Fatalf("通过检查应返回空, got %q", fb)
	}
}

// TestAfterEditFailFeedsBack 检查失败返回回灌反馈（含路径/命令/错误输出）。
func TestAfterEditFailFeedsBack(t *testing.T) {
	ws := t.TempDir()
	r := &Runner{Workspace: ws, Timeout: 10, Rules: []Rule{{Ext: []string{".txt"}, Command: "exit 3"}}}
	fb := r.AfterEdit("note.txt")
	if fb == "" {
		t.Fatal("失败检查应返回反馈")
	}
	for _, want := range []string{"[verify]", "note.txt", "exit 3", "请立即修复"} {
		if !strings.Contains(fb, want) {
			t.Fatalf("反馈缺 %q: %q", want, fb)
		}
	}
}

// TestAfterEditCooldown 同路径 30s 冷却：第二次不再执行检查。
// TestAfterEditLaunchFailureSilent 命令启动失败（如 PATH 缺命令，exit -1）= 静默不回灌：
// 命令不存在不是代码问题，回灌只会产生无法修复的噪音反馈。
func TestAfterEditLaunchFailureSilent(t *testing.T) {
	r := &Runner{Workspace: t.TempDir(), Timeout: 10, Rules: []Rule{{Ext: []string{".txt"}, Command: "definitely-not-exist-cmd-xyz-1234"}}}
	if fb := r.AfterEdit("a.txt"); fb != "" {
		t.Fatalf("启动失败应静默, got %q", fb)
	}
}

func TestAfterEditCooldown(t *testing.T) {
	ws := t.TempDir()
	r := &Runner{Workspace: ws, Timeout: 10, Rules: []Rule{{Ext: []string{".txt"}, Command: "exit 3"}}}
	if fb := r.AfterEdit("a.txt"); fb == "" {
		t.Fatal("首次应返回反馈")
	}
	if fb := r.AfterEdit("a.txt"); fb != "" {
		t.Fatalf("冷却期内应跳过, got %q", fb)
	}
	// 不同路径不受影响。
	if fb := r.AfterEdit("b.txt"); fb == "" {
		t.Fatal("不同路径应正常检查")
	}
}

// TestFilePlaceholder {file} 占位符被替换为正斜杠相对路径。
func TestFilePlaceholder(t *testing.T) {
	ws := t.TempDir()
	// Windows 下 cmd /C echo 会回显命令；用输出文件验证占位符替换。
	cmd := "echo {file} > _ph.txt"
	r := &Runner{Workspace: ws, Timeout: 10, Rules: []Rule{{Ext: []string{".py"}, Command: cmd}}}
	r.AfterEdit(filepath.Join("sub", "x.py")) // 无 exit 0 → 失败但命令已执行
	b, err := os.ReadFile(filepath.Join(ws, "_ph.txt"))
	if err != nil {
		t.Fatalf("命令未执行: %v", err)
	}
	if !strings.Contains(string(b), "sub/x.py") {
		t.Fatalf("占位符未替换为正斜杠路径: %q", string(b))
	}
}

// TestDisabledConfig disabled 配置 = 不检查。
func TestDisabledConfig(t *testing.T) {
	r := NewRunner(t.TempDir(), &Config{Disabled: true})
	if r.Rules != nil {
		t.Fatal("disabled 应清空规则")
	}
	if fb := r.AfterEdit("a.go"); fb != "" {
		t.Fatalf("disabled 不应检查, got %q", fb)
	}
}

// TestLoadMissingFile 配置不存在 = nil（零值行为）。
func TestLoadMissingFile(t *testing.T) {
	if cfg := Load(filepath.Join(t.TempDir(), "nope.json")); cfg != nil {
		t.Fatalf("缺失配置应返回 nil, got %+v", cfg)
	}
}

// TestLoadBrokenFile 配置损坏 = nil（跳过不致命）。
func TestLoadBrokenFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "verify.json")
	if err := os.WriteFile(p, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg := Load(p); cfg != nil {
		t.Fatalf("损坏配置应返回 nil, got %+v", cfg)
	}
}

// TestLoadValidWithBOM 合法配置（带 BOM）正常解析。
func TestLoadValidWithBOM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "verify.json")
	content := "\xEF\xBB\xBF{\"disabled\":false,\"timeout\":5,\"rules\":[{\"ext\":[\".py\"],\"command\":\"ruff check {file}\"}]}"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Load(p)
	if cfg == nil || cfg.Timeout != 5 || len(cfg.Rules) != 1 || cfg.Rules[0].Command != "ruff check {file}" {
		t.Fatalf("配置解析不符: %+v", cfg)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
