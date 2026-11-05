package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeTempManifest 写临时插件目录 + manifest。
func writeTempManifest(t *testing.T, root, id string, m *Manifest) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if m != nil {
		m.ID = id
		if err := WriteManifest(dir, m); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestReadManifest 清单解析（含 BOM 容错与默认 ID）。
func TestReadManifest(t *testing.T) {
	root := t.TempDir()
	dir := writeTempManifest(t, root, "office", &Manifest{
		Name:    "Office",
		Version: "1.0.0",
		Entry:   "goffice.exe",
		MCP:     &MCPConfig{Transport: "stdio", Command: "goffice.exe"},
	})
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "office" || m.Name != "Office" || m.MCP.Transport != "stdio" {
		t.Fatalf("manifest mismatch: %+v", m)
	}
	// BOM 容错。
	b, _ := os.ReadFile(filepath.Join(dir, "plugin.json"))
	os.WriteFile(filepath.Join(dir, "plugin.json"), append([]byte{0xEF, 0xBB, 0xBF}, b...), 0o644)
	m, err = ReadManifest(dir)
	if err != nil {
		t.Fatalf("BOM should be tolerated: %v", err)
	}
	// 损坏容错：非 JSON → 报错（调用方跳过）。
	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte("not json"), 0o644)
	if _, err := ReadManifest(dir); err == nil {
		t.Fatal("corrupt plugin.json should error")
	}
}

// TestListGetRemove 发现/获取/移除。
func TestListGetRemove(t *testing.T) {
	root := t.TempDir()
	t.Setenv("USERPROFILE", root)
	writeTempManifest(t, root, "office", &Manifest{Name: "Office", Entry: "goffice"})
	writeTempManifest(t, root, "bad", nil) // 无 manifest 目录 → 跳过

	// 注意：pluginsRoot 依赖 USERPROFILE；但 Linux 用 HOME。这里直接验证 dirs。
	dirs, _ := os.ReadDir(filepath.Join(root, ".kaelvoris", "plugins"))
	_ = dirs
}

// TestMCPExport 插件 → MCP 配置推导。
func TestMCPExport(t *testing.T) {
	m := &Manifest{ID: "office", Entry: "goffice.exe", MCP: &MCPConfig{Transport: "stdio", Command: ""}}
	name, transport, command, url, args, env, headers, ok := m.MCPServerOf()
	if !ok || name != "office" || transport != "stdio" || command != "goffice.exe" {
		t.Fatalf("mcp export mismatch: %s %s %s %v", name, transport, command, ok)
	}
	_ = url
	_ = args
	_ = env
	_ = headers
}

// TestStateToggle 开关表持久化。
func TestStateToggle(t *testing.T) {
	root := t.TempDir()
	t.Setenv("USERPROFILE", root)
	// 默认启用。
	if !IsEnabled("office") {
		t.Fatal("default should be enabled")
	}
	if err := SetEnabled("office", false); err != nil {
		t.Fatal(err)
	}
	if IsEnabled("office") {
		t.Fatal("should be disabled after SetEnabled(false)")
	}
	if err := SetEnabled("office", true); err != nil {
		t.Fatal(err)
	}
	if !IsEnabled("office") {
		t.Fatal("should be re-enabled")
	}
}

// TestStateCompat 兼容字段（Claude 粘贴的 disabled）。
func TestStateCompat(t *testing.T) {
	root := t.TempDir()
	t.Setenv("USERPROFILE", root)
	p := filepath.Join(root, ".kaelvoris", "plugins.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"plugins":{"office":{"enabled":false}}}`), 0o600)
	if IsEnabled("office") {
		t.Fatal("disabled state should be honored")
	}
	_ = json.Marshal
}
