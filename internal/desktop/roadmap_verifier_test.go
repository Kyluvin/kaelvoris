package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kaelvoris/internal/roadmap"
)

// TestVerifierAutoDetect 验证通道自动推断：go.mod → go build+test；
// package.json → npm run build；均无 → nil（跳过复核）。
func TestVerifierAutoDetect(t *testing.T) {
	// go.mod 工作区
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := verifierAutoDetect(ws)
	if len(got) != 2 || got[0] != "go build ./..." || got[1] != "go test ./..." {
		t.Errorf("go.mod detect = %v, want [go build ./... go test ./...]", got)
	}
	// package.json 工作区（无 go.mod）
	ws2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws2, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := verifierAutoDetect(ws2); len(got) != 1 || got[0] != "npm run build" {
		t.Errorf("package.json detect = %v, want [npm run build]", got)
	}
	// 空目录：无公认验证手段 → nil
	if got := verifierAutoDetect(t.TempDir()); got != nil {
		t.Errorf("empty ws detect = %v, want nil", got)
	}
}

// TestNormalizeVerifyCommands 用户配置归一化：trim/去空/去重；"-" 关闭语义
// 归一为 ["-"]；空输入 → nil。
func TestNormalizeVerifyCommands(t *testing.T) {
	got := normalizeVerifyCommands([]string{" go build ./... ", "", "go test ./...", "go build ./..."})
	want := []string{"go build ./...", "go test ./..."}
	if len(got) != len(want) {
		t.Fatalf("normalize = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalize = %v, want %v", got, want)
		}
	}
	if got := normalizeVerifyCommands([]string{"a", "-"}); len(got) != 1 || got[0] != "-" {
		t.Errorf("dash normalize = %v, want [-]", got)
	}
	if got := normalizeVerifyCommands(nil); got != nil {
		t.Errorf("nil normalize = %v, want nil", got)
	}
	if got := normalizeVerifyCommands([]string{"  "}); got != nil {
		t.Errorf("blank normalize = %v, want nil", got)
	}
}

// TestVerifierCommandsOverride 配置优先于自动推断；["-"] 显式关闭（→ nil）。
// autoDevCfg 链路经 App.GetAutoDevConfig → GetConfig（内存态 a.cfg，零值锁安全）。
func TestVerifierCommandsOverride(t *testing.T) {
	ws := t.TempDir()
	// 工作区有 go.mod（自动推断存在），但用户显式配置优先。
	a := &App{}
	a.cfg = Config{Workspace: ws, AutoDev: &AutoDevConfig{
		VerifyCommands: []string{"echo custom-verify"},
	}}
	got := a.verifierCommands(ws)
	if len(got) != 1 || got[0] != "echo custom-verify" {
		t.Errorf("config override = %v, want [echo custom-verify]", got)
	}
	// ["-"] 显式关闭：即便工作区可自动推断也不复核。
	a.cfg.AutoDev.VerifyCommands = []string{"-"}
	if got := a.verifierCommands(ws); got != nil {
		t.Errorf("dash off = %v, want nil", got)
	}
	// 配置为空：回落自动推断（go.mod 工作区）。
	_ = os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n"), 0o600)
	a.cfg.AutoDev.VerifyCommands = nil
	if got := a.verifierCommands(ws); len(got) != 2 {
		t.Errorf("auto fallback = %v, want go build+test", got)
	}
}

// TestRunVerifierPassFail 独立验证通道端到端：全部通过 → (true, 含 ✓ 报告)；
// 任一失败 → (false, 含 ✗ 与失败命令)；无通道 → 直接通过（跳过复核）。
// 失败命令用必然不存在的可执行名，跨平台稳定。
func TestRunVerifierPassFail(t *testing.T) {
	ws := t.TempDir()
	// 通过路径：echo 命令跨平台均可经 shell 执行。
	a := &App{}
	a.cfg = Config{Workspace: ws, AutoDev: &AutoDevConfig{
		VerifyCommands: []string{"echo verifier-ok"},
	}}
	ok, report := a.runVerifier(ws)
	if !ok || !strings.Contains(report, "✓ echo verifier-ok") {
		t.Errorf("pass case: ok=%v report=%q", ok, report)
	}
	// 失败路径：不存在的命令 → 非零退出 → false + ✗ 报告。
	a.cfg.AutoDev.VerifyCommands = []string{"echo step1", "kz-definitely-not-a-command-xyz"}
	ok, report = a.runVerifier(ws)
	if ok {
		t.Errorf("fail case: want false")
	}
	if !strings.Contains(report, "✗ kz-definitely-not-a-command-xyz") {
		t.Errorf("fail report missing ✗ line: %q", report)
	}
	// 关闭路径（["-"]）：直接通过且无报告。
	a.cfg.AutoDev.VerifyCommands = []string{"-"}
	if ok, report := a.runVerifier(ws); !ok || report != "" {
		t.Errorf("off case: ok=%v report=%q, want true/empty", ok, report)
	}
}

// TestBuildRoadmapDevTask_AcceptanceFirst 验收测试先行条款注入回归：
// 任务文本包含验收先行要求与独立复核告知；重试条目回注上次失败原因。
func TestBuildRoadmapDevTask_AcceptanceFirst(t *testing.T) {
	it := roadmap.Item{ID: "rm_t1", Title: "测试条目", Status: "developing", Attempts: 1}
	task := buildRoadmapDevTask(it)
	for _, want := range []string{
		"验收测试先行", "先有验收标尺", "独立复核",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("dev task missing %q", want)
		}
	}
	// 首次尝试：无失败原因回注。
	it.Attempts = 1
	it.Error = "上次复核未过：go test 失败"
	if strings.Contains(buildRoadmapDevTask(it), "上次尝试失败原因") {
		t.Errorf("first attempt should not inject last error")
	}
	// 重试条目（Attempts>1 且有 Error）：回注失败原因。
	it.Attempts = 2
	if task := buildRoadmapDevTask(it); !strings.Contains(task, "go test 失败") {
		t.Errorf("retry task should inject last error")
	}
}

// TestRoadmapSystemPromptDefault_AcceptanceFirst 开发守则默认文本回归：
// 五条守则含验收先行与独立复核告知。
func TestRoadmapSystemPromptDefault_AcceptanceFirst(t *testing.T) {
	for _, want := range []string{"5. 验收测试先行", "独立复核", "先有验收标尺"} {
		if !strings.Contains(roadmapSystemPromptDefault, want) {
			t.Errorf("system prompt default missing %q", want)
		}
	}
}
