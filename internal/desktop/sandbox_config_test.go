// sandbox_config_test.go —— 沙箱配置兜底测试（internal/desktop 同包白盒）：
//   - loadConfig：旧配置无 sandbox 字段 → 兜底 job 模式（limits 全 0）；
//   - 未知 mode → 兜底 job；mode=off 保留；
//   - defaultConfig：Sandbox 默认 job。
package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"kaelvoris/internal/tools"
)

// writeTestConfig 写临时 desktop.json 并把 UserHomeDir 重定向到临时目录
// （同 model_profiles_test.go 惯例：绝不碰用户真实 ~/.kaelvoris）。
func writeTestConfig(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir) // Windows UserHomeDir 来源
	t.Setenv("HOME", dir)        // Unix 兜底
	if err := os.MkdirAll(filepath.Join(dir, ".kaelvoris"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".kaelvoris", "desktop.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestLoadConfigSandboxFallback 旧配置无 sandbox 字段 → 兜底 job（行为升级）。
func TestLoadConfigSandboxFallback(t *testing.T) {
	writeTestConfig(t, `{"baseURL":"http://x","apiKey":"k","model":"m"}`)
	a := &App{}
	c := a.loadConfig()
	if c.Sandbox == nil {
		t.Fatal("sandbox must be defaulted for legacy config")
	}
	if c.Sandbox.Mode != tools.SandboxModeJob {
		t.Fatalf("sandbox mode must default to job, got %q", c.Sandbox.Mode)
	}
	if c.Sandbox.MemoryLimitMB != 0 || c.Sandbox.CPUPercent != 0 || c.Sandbox.MaxProcesses != 0 {
		t.Fatalf("limits must default to 0 (unlimited), got %+v", c.Sandbox)
	}
}

// TestLoadConfigSandboxPreserve 用户显式配置保留；未知 mode 兜底 job。
func TestLoadConfigSandboxPreserve(t *testing.T) {
	writeTestConfig(t, `{"baseURL":"http://x","sandbox":{"mode":"off","memoryLimitMB":512}}`)
	a := &App{}
	c := a.loadConfig()
	if c.Sandbox == nil || c.Sandbox.Mode != tools.SandboxModeOff || c.Sandbox.MemoryLimitMB != 512 {
		t.Fatalf("explicit sandbox config must be preserved, got %+v", c.Sandbox)
	}

	writeTestConfig(t, `{"baseURL":"http://x","sandbox":{"mode":"bogus"}}`)
	a2 := &App{}
	c2 := a2.loadConfig()
	if c2.Sandbox == nil || c2.Sandbox.Mode != tools.SandboxModeJob {
		t.Fatalf("unknown mode must fall back to job, got %+v", c2.Sandbox)
	}
}

// TestDefaultConfigSandbox 缺省配置：Sandbox 默认 job 纯杀树。
func TestDefaultConfigSandbox(t *testing.T) {
	c := DefaultConfig()
	if c.Sandbox == nil || c.Sandbox.Mode != tools.SandboxModeJob {
		t.Fatalf("default config sandbox must be job, got %+v", c.Sandbox)
	}
	// JSON 往返不丢字段（前端 SaveConfig 链路）。
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Sandbox == nil || back.Sandbox.Mode != tools.SandboxModeJob {
		t.Fatalf("sandbox lost in JSON round-trip: %+v", back.Sandbox)
	}
}

// TestBuildToolsSandboxWired buildTools 注入：Sandbox 工厂已接到 CommandRunner /
// PersistentShellManager / BackgroundManager 三路径（非 nil 且 job 模式）。
func TestBuildToolsSandboxWired(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
	a := &App{}
	a.cfg = DefaultConfig()
	reg := buildTools(dir, tools.NewTodoStore(), testSkillMgr(t), a)
	if reg == nil {
		t.Fatal("buildTools returned nil")
	}
	sh, ok := reg.Get("shell")
	if !ok {
		t.Fatal("shell tool missing")
	}
	st, ok := sh.(*tools.ShellTool)
	if !ok {
		t.Fatalf("shell tool type mismatch: %T", sh)
	}
	if st.Runner == nil || st.Runner.Sandbox == nil {
		t.Fatal("CommandRunner.Sandbox not wired")
	}
}
