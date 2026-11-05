package desktop

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"kaelvoris/internal/plugin"
)

// TestPluginPanel 插件面板绑定：安装（本地目录）→ 列表 → 开关 → 卸载。
func TestPluginPanel(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("USERPROFILE", tmpHome)

	// 准备一个插件源目录（plugin.json + 伪二进制）。
	src := filepath.Join(tmpHome, "src_office")
	os.MkdirAll(src, 0o755)
	plugin.WriteManifest(src, &plugin.Manifest{
		ID:          "office",
		Name:        "Office",
		Version:     "1.0.0",
		Description: "test plugin",
		MCP:         &plugin.MCPConfig{Transport: "stdio", Command: "goffice.exe"},
	})
	os.WriteFile(filepath.Join(src, "goffice.exe"), []byte("fake"), 0o755)

	a := &App{}
	// 安装。
	inst, err := a.InstallPluginFromDir(InstallPluginFromDirRequest{Dir: src})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if inst.ID != "office" {
		t.Fatalf("installed id = %q, want office", inst.ID)
	}
	list := a.ListPlugins()
	if len(list) != 1 || list[0].Name != "Office" {
		t.Fatalf("list = %+v", list)
	}
	if !list[0].Enabled {
		t.Fatal("new plugin should default enabled")
	}
	// 开关。
	if err := a.TogglePlugin("office", false); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	if plugin.IsEnabled("office") {
		t.Fatal("should be disabled after toggle")
	}
	// 卸载。
	if err := a.UninstallPlugin("office"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(a.ListPlugins()) != 0 {
		t.Fatal("plugin should be removed")
	}
}

// TestPluginPanel_URLInstall 下载安装（httptest 假服务器 + zip 打包）:
// 下载 zip → 解压 → 校验 plugin.json → 安装 → 列表可见。
func TestPluginPanel_URLInstall(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("USERPROFILE", tmpHome)

	// 构造一个 zip 包（内含 plugin.json + 伪二进制），用 httptest 假服务器提供。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("minimal/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`{"id":"minimal","name":"Minimal","version":"1.0.0","mcp":{"transport":"stdio","command":"minimal.exe"}}`))
	bw, _ := zw.Create("minimal/minimal.exe")
	_, _ = bw.Write([]byte("fake"))
	zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	a := &App{}
	inst, err := a.InstallPluginFromURL(InstallPluginFromURLRequest{URL: srv.URL})
	if err != nil {
		t.Fatalf("install-from-url: %v", err)
	}
	if inst.ID != "minimal" {
		t.Fatalf("installed id = %q, want minimal", inst.ID)
	}
	list := a.ListPlugins()
	if len(list) != 1 || list[0].Name != "Minimal" {
		t.Fatalf("list = %+v", list)
	}
	// 来源应为 download。
	if list[0].Source != "download" {
		t.Fatalf("source = %q, want download", list[0].Source)
	}
}

// TestPluginPanel_BuiltinEmbedInstall 内置插件 embed 安装：installBuiltinFromDir
// 从 go:embed 发行目录解包到 ~/.kaelvoris/plugins/<id>/（不依赖工作目录，
// 回归：部署后报「发行目录不存在」）；清单/entry 落位正确。
func TestPluginPanel_BuiltinEmbedInstall(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("USERPROFILE", tmpHome)

	// 切到非仓库工作目录（模拟桌面端部署后的工作目录）。
	oldWd, _ := os.Getwd()
	tmpWd := filepath.Join(tmpHome, "wd")
	os.MkdirAll(tmpWd, 0o755)
	os.Chdir(tmpWd)
	defer os.Chdir(oldWd)

	a := &App{}
	// 未收录 ID 报错（不落目录）。
	if _, err := a.installBuiltinFromDir("no-such"); err == nil {
		t.Fatal("unknown id should fail")
	}
	// 正常安装（embed 内 office）。
	inst, err := a.installBuiltinFromDir("office")
	if err != nil {
		t.Fatalf("install builtin office: %v", err)
	}
	if inst.ID != "office" || inst.Source != "builtin" {
		t.Fatalf("installed = %+v", inst)
	}
	root := filepath.Join(tmpHome, ".kaelvoris", "plugins")
	for _, f := range []string{"plugin.json", "goffice.exe"} {
		if _, err := os.Stat(filepath.Join(root, "office", f)); err != nil {
			t.Fatalf("%s missing after install: %v", f, err)
		}
	}
	// 重复安装报已安装。
	if _, err := a.installBuiltinFromDir("office"); err == nil {
		t.Fatal("duplicate install should fail")
	}
	// 卸载后可重装（升级路径）。
	if err := a.UninstallPlugin("office"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := a.installBuiltinFromDir("office"); err != nil {
		t.Fatalf("reinstall after uninstall: %v", err)
	}
	// 市场列表内置兑底可见 office 且 installed=true。
	found := false
	for _, it := range a.builtinMarketFallback() {
		if it.ID == "office" && it.Installed {
			found = true
		}
	}
	if !found {
		t.Fatal("office should appear installed in builtin market fallback")
	}
}

// TestPluginPanel_Available 内置可下载插件目录：前 ponytail/office 均收录，
// 且已安装项标记 installed=true。
func TestPluginPanel_Available(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("USERPROFILE", tmpHome)

	a := &App{}
	avail := a.ListAvailablePlugins()
	if len(avail) < 2 {
		t.Fatalf("available = %d, want >=2", len(avail))
	}
	var hasPony bool
	for _, p := range avail {
		if p.ID == "ponytail" {
			hasPony = true
			if p.Installed {
				t.Fatal("ponytail should be uninstalled initially")
			}
		}
	}
	if !hasPony {
		t.Fatal("available list missing ponytail")
	}

	// 安装后 installed 标记翻转。
	src := filepath.Join(tmpHome, "ponytail-src")
	os.MkdirAll(src, 0o755)
	plugin.WriteManifest(src, &plugin.Manifest{ID: "ponytail", Name: "Ponytail"})
	if _, err := a.InstallPluginFromDir(InstallPluginFromDirRequest{Dir: src}); err != nil {
		t.Fatalf("install: %v", err)
	}
	avail2 := a.ListAvailablePlugins()
	for _, p := range avail2 {
		if p.ID == "ponytail" && !p.Installed {
			t.Fatal("ponytail should be installed after install")
		}
	}
}
