// mcp_import_test.go —— MCP 快速添加（粘贴导入）与工具清单查看测试：
//   - parseMCPConfig 三格式：kaelvoris 数组 / mcpServers 映射 / 顶层单服务器映射
//     （用户从 Claude/Cline 等工具直贴的 fragment，含 streamable-http 归一）；
//   - ImportMCPServers 合并落盘（同名覆盖、异名保留）；
//   - MCPServerTools 工具清单枚举（httptest 假 streamable 服务器端到端）。
package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseMCPConfigFormats 三格式解析 + 非法输入拒绝。
func TestParseMCPConfigFormats(t *testing.T) {
	// 1) 用户直贴顶层单服务器映射（真实案例：b1-pro streamable-http + 认证头）。
	frag := `{"b1-pro":{"type":"streamable-http","url":"http://10.0.0.1:8091/mcp/stream",` +
		`"headers":{"Authorization":"Bearer tok123"},"disabled":false,"alwaysAllow":[]}}`
	servers, n := parseMCPConfig([]byte(frag))
	if n != 1 || len(servers) != 1 {
		t.Fatalf("fragment: n=%d servers=%+v", n, servers)
	}
	s := servers[0]
	if s.Name != "b1-pro" || s.Transport != "http" || !s.Enabled {
		t.Fatalf("fragment parse: %+v", s)
	}
	if s.URL != "http://10.0.0.1:8091/mcp/stream" || s.Headers["Authorization"] != "Bearer tok123" {
		t.Fatalf("fragment fields: %+v", s)
	}

	// 2) mcpServers 包裹映射（Claude Desktop / Cline 格式），disabled:true 生效
	//（映射条目归一为 Enabled=false；Disabled 字段仅数组格式回写，历史行为一致）。
	wrapped := `{"mcpServers":{"fs":{"command":"npx -y @modelcontextprotocol/server-filesystem","disabled":true}}}`
	servers, n = parseMCPConfig([]byte(wrapped))
	if n != 1 || servers[0].Enabled {
		t.Fatalf("wrapped parse: n=%d %+v", n, servers)
	}
	if servers[0].Transport != "stdio" || servers[0].Command == "" {
		t.Fatalf("wrapped fields: %+v", servers[0])
	}

	// 3) kaelvoris 原生数组格式。
	arr := `[{"name":"x","transport":"http","url":"http://h/mcp","enabled":true}]`
	servers, n = parseMCPConfig([]byte(arr))
	if n != 1 || servers[0].Name != "x" || !servers[0].Enabled {
		t.Fatalf("array parse: n=%d %+v", n, servers)
	}

	// 4) 非法/无关 JSON 必须拒绝（返回 0，不产出占位条目）。
	for _, bad := range []string{`{"a":1}`, `not json`, `[{"nope":1}]`, `{"mcpServers":null}`} {
		if _, n := parseMCPConfig([]byte(bad)); n != 0 {
			t.Fatalf("bad input %q should be rejected, got %d", bad, n)
		}
	}
	// 空对象：不失败也不产出（粘贴空配置不应报错）。
	if _, n := parseMCPConfig([]byte(`{}`)); n != 0 {
		t.Fatalf("empty object should yield 0, got %d", n)
	}
}

// TestImportMCPServers 导入合并语义：同名覆盖、异名保留、直接落盘。
func TestImportMCPServers(t *testing.T) {
	tmpHome, err := os.MkdirTemp("", "mcp_import_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpHome)
	old := os.Getenv("USERPROFILE")
	t.Setenv("USERPROFILE", tmpHome)
	defer func() { os.Setenv("USERPROFILE", old) }()

	a := &App{}
	// 预置磁盘上已有另一台服务器（导入不得清空它）。
	if err := a.SaveMCPServers([]MCPServer{
		{Name: "existing", Transport: "stdio", Command: "npx -y srv", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	// 导入同名旧配置（应覆盖）+ 新服务器（应新增）。
	imported, err := a.ImportMCPServers(`{
		"b1-pro": {"type":"streamable-http","url":"http://h/mcp","headers":{"Authorization":"Bearer x"}},
		"existing": {"command":"npx -y srv-v2"}
	}`)
	if err != nil {
		t.Fatalf("ImportMCPServers: %v", err)
	}
	if len(imported) != 2 {
		t.Fatalf("expected 2 servers after merge, got %d: %+v", len(imported), imported)
	}
	byName := map[string]MCPServer{}
	for _, s := range imported {
		byName[s.Name] = s
	}
	b1 := byName["b1-pro"]
	if b1.Name == "" || b1.Transport != "http" || !b1.Enabled || b1.Headers["Authorization"] != "Bearer x" {
		t.Fatalf("imported server wrong: %+v", b1)
	}
	if byName["existing"].Command != "npx -y srv-v2" {
		t.Fatalf("same-name override failed: %+v", byName["existing"])
	}

	// 直接落盘验证（重读磁盘 = ListMCPServers 一致）。
	reloaded := a.loadMCPServers()
	if len(reloaded) != 2 {
		t.Fatalf("disk roundtrip: %+v", reloaded)
	}

	// 非法格式导入必须报错且不落盘（磁盘内容保持 2 台）。
	if _, err := a.ImportMCPServers(`garbage`); err == nil {
		t.Fatal("garbage import should fail")
	}
	if got := len(a.loadMCPServers()); got != 2 {
		t.Fatalf("failed import must not touch disk, got %d", got)
	}
}

// fakeMCPStreamable 最小 streamable HTTP 假服务器（JSON 单响应模式），
// 供 MCPServerTools 端到端枚举测试。
func fakeMCPStreamable(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2024-11-05"}`
		case "tools/list":
			result = `{"tools":[
				{"name":"echo","description":"回显输入文本","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}},
				{"name":"ping","description":"连通性探测"}
			]}`
		default:
			result = `{}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(req.ID) + `,"result":` + result + `}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestMCPServerTools 工具清单查看：真实握手 → tools/list → 条目含描述与参数 schema。
func TestMCPServerTools(t *testing.T) {
	srv := fakeMCPStreamable(t)
	a := &App{}

	res, err := a.MCPServerTools(MCPServer{
		Name: "demo", Transport: "http", URL: srv.URL + "/mcp", Enabled: true,
	})
	if err != nil {
		t.Fatalf("MCPServerTools: %v", err)
	}
	if !res.OK || len(res.Tools) != 2 {
		t.Fatalf("result: %+v", res)
	}
	echo := res.Tools[0]
	if echo.Name != "echo" || echo.FullName != "mcp_demo_echo" || echo.Desc != "回显输入文本" {
		t.Fatalf("echo tool: %+v", echo)
	}
	if len(echo.InputSchema) == 0 || !strings.Contains(string(echo.InputSchema), `"text"`) {
		t.Fatalf("echo inputSchema missing: %s", string(echo.InputSchema))
	}
	if res.Tools[1].Name != "ping" || res.Tools[1].Desc != "连通性探测" {
		t.Fatalf("ping tool: %+v", res.Tools[1])
	}

	// 禁用服务器：返回提示不连接。
	res, err = a.MCPServerTools(MCPServer{Name: "off", Transport: "http", URL: srv.URL + "/mcp", Enabled: false})
	if err != nil || res.OK || res.Error == "" {
		t.Fatalf("disabled server: res=%+v err=%v", res, err)
	}

	// 连接失败：错误经数据返回（不 panic、不 error）。
	res, err = a.MCPServerTools(MCPServer{Name: "dead", Transport: "http", URL: "http://127.0.0.1:1/mcp", Enabled: true})
	if err != nil {
		t.Fatalf("dead server should not error: %v", err)
	}
	if res.OK || res.Error == "" {
		t.Fatalf("dead server: %+v", res)
	}
}

// TestLoadMCPServersFragmentFile 磁盘文件为顶层单服务器映射格式时也能读取
// （用户手动把 fragment 粘进 mcp-servers.json 的场景）。
func TestLoadMCPServersFragmentFile(t *testing.T) {
	tmpHome, err := os.MkdirTemp("", "mcp_frag_*")
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
	frag := `{"b1-pro":{"type":"streamable-http","url":"http://h/mcp/stream","headers":{"Authorization":"Bearer t"}}}`
	if err := os.WriteFile(filepath.Join(dir, "mcp-servers.json"), []byte(frag), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	got := a.loadMCPServers()
	if len(got) != 1 || got[0].Name != "b1-pro" || got[0].Transport != "http" || !got[0].Enabled {
		t.Fatalf("fragment file load: %+v", got)
	}
	_ = context.Background // 保持 import 对齐（未来扩展用）
}
