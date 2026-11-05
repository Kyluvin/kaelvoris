package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMCPDisabledFiltering MCP 开关关闭后不再连接的回归测试（2026-10 修复）：
// 覆盖三条曾经导致「开关关闭仍调用」的路径——
//  1. 禁用且配置不完整的条目保存时不因必填校验失败（否则旧启用配置残留）；
//  2. 数组格式 Claude 习惯 disabled:true 必须覆盖 enabled:true（此前被忽略）；
//  3. TestMCPServer 对禁用服务器返回明确提示而非真实连接。
func TestMCPDisabledFiltering(t *testing.T) {
	tmpHome, err := os.MkdirTemp("", "mcp_home_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpHome)
	old := os.Getenv("USERPROFILE")
	t.Setenv("USERPROFILE", tmpHome)
	defer func() { os.Setenv("USERPROFILE", old) }()

	a := &App{}

	// 场景 1：关闭开关后保存「禁用且配置不完整」条目（command 清空）
	// → SaveMCPServers 不得报错（此前强制校验必填导致保存失败 → 旧启用配置残留，
	// 下一任务仍连接旧配置 → 表现为「开关关闭后还是会调用」）。
	if err := a.SaveMCPServers([]MCPServer{
		{Name: "fs", Transport: "stdio", Command: "", Enabled: false},
	}); err != nil {
		t.Fatalf("SaveMCPServers with disabled+empty config should pass, got: %v", err)
	}

	// 场景 2：enabled:false 读取后 Enabled=false（buildMCPTools 的 !Enabled 过滤生效）。
	os.WriteFile(filepath.Join(tmpHome, ".kaelvoris", "mcp-servers.json"),
		[]byte(`[{"name":"fs","transport":"stdio","command":"x","enabled":false}]`), 0o600)
	got := a.loadMCPServers()
	if len(got) != 1 || got[0].Enabled {
		t.Fatalf("enabled:false should parse to Enabled=false, got %+v", got)
	}

	// 场景 3：数组格式带 Claude/Cline 习惯 disabled:true（enabled 残留 true）
	// → 必须禁用（此前数组分支只认 enabled，disabled 被静默忽略）。
	os.WriteFile(filepath.Join(tmpHome, ".kaelvoris", "mcp-servers.json"),
		[]byte(`[{"name":"fs","transport":"stdio","command":"x","enabled":true,"disabled":true}]`), 0o600)
	got = a.loadMCPServers()
	if len(got) != 1 || got[0].Enabled {
		t.Fatalf("disabled:true should override enabled:true, got %+v", got)
	}
	if !got[0].Disabled {
		t.Fatalf("disabled field should be surfaced, got %+v", got[0])
	}

	// 场景 4：启用条目的必填校验仍保留（防空配置误启用）。
	if err := a.SaveMCPServers([]MCPServer{
		{Name: "fs", Transport: "stdio", Command: "", Enabled: true},
	}); err == nil {
		t.Fatal("enabled stdio without command should be rejected")
	}

	// 场景 5：TestMCPServer 对禁用服务器返回明确提示（不真实连接、不返回工具数）。
	res, err2 := a.TestMCPServer(MCPServer{Name: "fs", Transport: "stdio", Command: "x", Enabled: false})
	if err2 != nil {
		t.Fatalf("TestMCPServer disabled should not error, got %v", err2)
	}
	if res.OK {
		t.Fatal("disabled server must not report OK")
	}
	if res.Error == "" {
		t.Fatal("disabled server should return hint error text")
	}

	// 场景 6（2026-10 二次修复）：禁用且名称留空的条目也能保存——用户关闭开关后
	// 名称可能同时被清空/未填（占位条目），此前 Name 校验无条件执行仍会拦截保存
	// → 保存失败，用户误以为已保存（实际未落盘）；校验语义与 command/url 对齐：
	// 仅约束启用条目。
	if err := a.SaveMCPServers([]MCPServer{
		{Name: "", Transport: "stdio", Command: "", Enabled: false},
	}); err != nil {
		t.Fatalf("SaveMCPServers with disabled+empty name should pass, got: %v", err)
	}

	// 场景 7（2026-11 端到端往返回归）：完整复现用户操作——先保存启用配置，
	// 再关闭开关保存，重新 load 必须返回 Enabled=false；这是「保存无效，下次
	// 设置还是开启状态」症状的直接断言（此前保存被校验拦截或解析覆盖导致
	// 磁盘残留 enabled:true）。
	if err := a.SaveMCPServers([]MCPServer{
		{Name: "fs", Transport: "stdio", Command: "npx -y server", Enabled: true},
	}); err != nil {
		t.Fatalf("save enabled server failed: %v", err)
	}
	if err := a.SaveMCPServers([]MCPServer{
		{Name: "fs", Transport: "stdio", Command: "npx -y server", Enabled: false},
	}); err != nil {
		t.Fatalf("save disabled server failed: %v", err)
	}
	got = a.loadMCPServers()
	if len(got) != 1 {
		t.Fatalf("expected 1 server after roundtrip, got %d", len(got))
	}
	if got[0].Enabled {
		t.Fatalf("roundtrip failed: server should be disabled after save, got %+v", got[0])
	}
	if got[0].Name != "fs" {
		t.Fatalf("roundtrip name mismatch: %+v", got[0])
	}
}
