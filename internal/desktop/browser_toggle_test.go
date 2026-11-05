// browser_toggle_test.go 内置浏览器开关（Config.BrowserDisabled，用户需求：
// 设置面板可隐藏/显示 browser_* 工具组）：
//   - 默认（false）：browser_* 六件套全部注册，模型可见；
//   - BrowserDisabled=true：不注册任何 browser_* 工具（定义侧不可见语义，
//     对齐 SkillsDisabled），desktop_* 不受影响（Windows 上仍注册）；
//   - BrowserPath 接线：配置的路径透传给 BrowserTool（修复：设置页
//     「浏览器路径」此前从未接线，findBrowser 永远走自动探测）。
//
// App 依赖最小化：&App{cfg: ...} 直构（GetConfig 读结构体，无 Wails 依赖）；
// buildTools 需要 skills.Manager，传临时目录 Manager（无技能不影响本测）。
package desktop

import (
	"testing"

	"kaelvoris/internal/skills"
	"kaelvoris/internal/tools"
)

// hasTool 判断注册中心是否已注册指定工具。
func hasTool(r *tools.Registry, name string) bool {
	for _, n := range r.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// browserTools browser_* 六件套名单。
var browserTools = []string{
	"browser_open", "browser_eval", "browser_screenshot",
	"browser_act", "browser_snapshot", "browser_tabs",
}

func TestBuildToolsBrowserEnabledByDefault(t *testing.T) {
	a := &App{cfg: Config{BrowserPath: `C:\fake\edge.exe`}}
	reg := buildTools(t.TempDir(), tools.NewTodoStore(), testSkillMgr(t), a)
	for _, n := range browserTools {
		if !hasTool(reg, n) {
			t.Errorf("default config should register %s", n)
		}
	}
}

func TestBuildToolsBrowserDisabledHidesGroup(t *testing.T) {
	a := &App{cfg: Config{BrowserDisabled: true}}
	reg := buildTools(t.TempDir(), tools.NewTodoStore(), testSkillMgr(t), a)
	for _, n := range browserTools {
		if hasTool(reg, n) {
			t.Errorf("BrowserDisabled=true should NOT register %s", n)
		}
	}
	// desktop_* 不受浏览器开关影响：Windows 上仍应注册（非 Windows 跳过）。
	if tools.DesktopSupported() && !hasTool(reg, "desktop_windows") {
		t.Error("BrowserDisabled must not affect desktop_* registration on Windows")
	}
}

func TestBuildToolsBrowserPathWired(t *testing.T) {
	const p = `D:\browsers\my-chrome.exe`
	a := &App{cfg: Config{BrowserPath: p}}
	reg := buildTools(t.TempDir(), tools.NewTodoStore(), testSkillMgr(t), a)
	v, ok := reg.Get("browser_open")
	if !ok {
		t.Fatal("browser_open not registered")
	}
	bt, ok := v.(*tools.BrowserTool)
	if !ok {
		t.Fatalf("browser_open is %T, want *tools.BrowserTool", v)
	}
	if bt.BrowserPath != p {
		t.Fatalf("BrowserPath not wired: got %q want %q", bt.BrowserPath, p)
	}
}

// testSkillMgr 测试用技能管理器（临时目录，避免读真实 ~/.kaelvoris）。
func testSkillMgr(t *testing.T) *skills.Manager {
	t.Helper()
	return skills.NewManager(t.TempDir(), t.TempDir())
}
