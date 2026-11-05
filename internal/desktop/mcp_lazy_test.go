package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kaelvoris/internal/employee"
	"kaelvoris/internal/tools"
)

// TestMCPSignalHit 激活信号命中判定（惰性加载第一层：任务文本/工作区提示词）。
func TestMCPSignalHit(t *testing.T) {
	cases := []struct {
		signal, name string
		want         bool
	}{
		{"请用 office 插件生成周报", "office", true},     // 中文句子内直接命中
		{"Use the OFFICE tools", "office", true}, // 大小写不敏感
		{"connect to context7 for docs", "context7", true},
		{"请用 go 写个脚本", "go", true},      // 短名词边界命中
		{"浏览 category 分类", "go", false}, // 短名不被英文词内命中
		{"随便聊聊", "office", false},       // 无关任务不激活
		{"", "office", false},           // 空信号不激活
		{"用 office 办公", "", false},      // 空名不激活
	}
	for _, c := range cases {
		if got := mcpSignalHit(c.signal, c.name); got != c.want {
			t.Errorf("mcpSignalHit(%q, %q) = %v, want %v", c.signal, c.name, got, c.want)
		}
	}
}

// TestMCPWantedActivation mcpWanted：任务文本命中 → servers；未命中 → candidates 目录。
func TestMCPWantedActivation(t *testing.T) {
	tmpHome, err := os.MkdirTemp("", "mcp_wanted_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpHome)
	old := os.Getenv("USERPROFILE")
	t.Setenv("USERPROFILE", tmpHome)
	defer func() { os.Setenv("USERPROFILE", old) }()

	dir := filepath.Join(tmpHome, ".kaelvoris")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp-servers.json"),
		[]byte(`[{"name":"office","transport":"stdio","command":"x.exe","enabled":true},{"name":"fs-server","transport":"stdio","command":"y.exe","enabled":true},{"name":"dead","transport":"stdio","command":"z.exe","enabled":false}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &App{}
	a.mcpProbeText = "请把会议纪要写入 office 文档"
	w := a.mcpWanted()
	if len(w.servers) != 1 || w.servers[0] != "office" {
		t.Fatalf("office should be activated, got servers=%v", w.servers)
	}
	// 未命中的启用服务器进目录；禁用的不进目录。
	var names []string
	for _, c := range w.candidates {
		names = append(names, c.name)
	}
	if len(names) != 1 || names[0] != "fs-server" {
		t.Fatalf("only enabled+unmatched server should be candidate, got %v", names)
	}

	// 无信号：全部进目录，零激活。
	a.mcpProbeText = ""
	w = a.mcpWanted()
	if len(w.servers) != 0 {
		t.Fatalf("no signal should activate nothing, got %v", w.servers)
	}
	if len(w.candidates) != 2 {
		t.Fatalf("candidates should carry enabled servers only, got %v", w.candidates)
	}
}

// TestMCPLazyHint 提示词目录段：非空目录注入、空目录/禁用零注入。
func TestMCPLazyHint(t *testing.T) {
	cands := []mcpCandidateEntry{{name: "office", desc: "Office 文档生成"}}
	hint := mcpLazyHint(cands, false, nil)
	if !strings.Contains(hint, "MCP TOOL CATALOG") || !strings.Contains(hint, "mcp_activate") {
		t.Fatalf("hint should contain catalog + activation guide: %q", hint)
	}
	if !strings.Contains(hint, "- office — Office 文档生成") {
		t.Fatalf("hint should list candidate: %q", hint)
	}
	if mcpLazyHint(nil, false, nil) != "" {
		t.Fatal("empty candidates should emit no hint")
	}
	if mcpLazyHint(cands, true, nil) != "" {
		t.Fatal("session-disabled should emit no hint")
	}
	emp := &employee.Employee{MCPDisabled: true}
	if mcpLazyHint(cands, false, emp) != "" {
		t.Fatal("employee-disabled should emit no hint")
	}
}

// TestMCPActivateToolUnknown mcp_activate 对未知/禁用服务器的负路径（不连接、错误可读）。
func TestMCPActivateToolUnknown(t *testing.T) {
	tmpHome, err := os.MkdirTemp("", "mcp_act_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpHome)
	old := os.Getenv("USERPROFILE")
	t.Setenv("USERPROFILE", tmpHome)
	defer func() { os.Setenv("USERPROFILE", old) }()

	dir := filepath.Join(tmpHome, ".kaelvoris")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp-servers.json"),
		[]byte(`[{"name":"off","transport":"stdio","command":"x.exe","enabled":false}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	tl := &mcpActivateTool{app: a, registry: tools.NewRegistry()}
	if _, err := tl.Execute(context.Background(), `{"server":"off"}`); err == nil {
		t.Fatal("disabled server should fail activation")
	}
	if _, err := tl.Execute(context.Background(), `{"server":"nope"}`); err == nil {
		t.Fatal("unknown server should fail activation")
	}
	if _, err := tl.Execute(context.Background(), `{}`); err == nil {
		t.Fatal("empty server should fail activation")
	}
}

// TestMCPActivateIdempotent 幂等：同前缀工具已注册 → 不再连接直接返回已激活。
// （用伪造注册项模拟已激活态；不依赖真实子进程。）
func TestMCPActivateIdempotent(t *testing.T) {
	tmpHome, err := os.MkdirTemp("", "mcp_idem_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpHome)
	old := os.Getenv("USERPROFILE")
	t.Setenv("USERPROFILE", tmpHome)
	defer func() { os.Setenv("USERPROFILE", old) }()

	dir := filepath.Join(tmpHome, ".kaelvoris")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 命令不存在：若幂等失效会真实 spawn 并失败；幂等生效则提前返回成功。
	if err := os.WriteFile(filepath.Join(dir, "mcp-servers.json"),
		[]byte(`[{"name":"probe","transport":"stdio","command":"definitely-not-exist-cmd","enabled":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(&fakeProbeTool{name: "mcp_probe_echo"}); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	tl := &mcpActivateTool{app: a, registry: reg}
	out, err := tl.Execute(context.Background(), `{"server":"probe"}`)
	if err != nil {
		t.Fatalf("idempotent activation should succeed without connecting, got err: %v", err)
	}
	if !strings.Contains(out, "已激活") {
		t.Fatalf("unexpected output: %q", out)
	}
}

// fakeProbeTool 幂等测试桩（伪装已注册的 mcp_probe_* 工具）。
type fakeProbeTool struct{ name string }

func (f *fakeProbeTool) Name() string { return f.name }
func (f *fakeProbeTool) Description() string {
	return "probe"
}
func (f *fakeProbeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "required": []string{}}
}
func (f *fakeProbeTool) Execute(ctx context.Context, args string) (string, error) {
	return "ok", nil
}

// TestBuildMCPToolsLazy 端到端：任务未提及 → 不连接不注册（207 工具超限修复的核心断言）；
// 任务提及 → 激活路径生效（命令不存在时降级跳过，不阻断）。
func TestBuildMCPToolsLazy(t *testing.T) {
	tmpHome, err := os.MkdirTemp("", "mcp_lazy_e2e_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpHome)
	old := os.Getenv("USERPROFILE")
	t.Setenv("USERPROFILE", tmpHome)
	defer func() { os.Setenv("USERPROFILE", old) }()

	dir := filepath.Join(tmpHome, ".kaelvoris")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp-servers.json"),
		[]byte(`[{"name":"probe","transport":"stdio","command":"definitely-not-exist-cmd","enabled":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{}

	// 场景 1：任务未提及 → 完全不连接（此前全量连接是上下文超限根因）。
	reg := tools.NewRegistry()
	a.mcpProbeText = "帮我看看这个仓库结构"
	clients := a.buildMCPTools(context.Background(), reg, nil, tmpHome, nil, false)
	if len(clients) != 0 {
		t.Fatalf("unrelated task must connect nothing, got %d clients", len(clients))
	}
	if got := reg.Names(); len(got) > 0 {
		t.Fatalf("no mcp tools should be registered, got %v", got)
	}

	// 场景 2：任务提及 → 尝试连接（命令不存在 → 连接失败降级跳过、不注册）。
	a.mcpProbeText = "请用 probe 服务器查询数据"
	clients = a.buildMCPTools(context.Background(), reg, nil, tmpHome, nil, false)
	if len(clients) != 0 { // 命令不存在：连接失败 → 0 client，但必须走过激活分支（不 panic）
		t.Logf("nonexistent command fail-skip as expected (%d clients)", len(clients))
	}
	if _, ok := reg.Get("mcp_probe_anything"); ok {
		t.Fatal("failed server must not register tools")
	}

	// 场景 3：会话禁用 MCP → 即使命中也不连接。
	a.mcpProbeText = "用 probe 查询"
	clients = a.buildMCPTools(context.Background(), reg, nil, tmpHome, nil, true)
	if len(clients) != 0 {
		t.Fatalf("session-disabled must connect nothing, got %d", len(clients))
	}
}
