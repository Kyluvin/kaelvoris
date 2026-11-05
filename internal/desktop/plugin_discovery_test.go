package desktop

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"kaelvoris/internal/mcp"
	"kaelvoris/internal/plugin"
	"kaelvoris/internal/tools"
)

// TestPluginDiscoveryConnect 集成：发现已安装插件（~/.kaelvoris/plugins/office）
// → 按 manifest 推导 MCP 配置（命令相对路径解析 + 工作区注入）→ 连接 → 列工具。
// 等价于 buildMCPTools 的插件分支（复用 plugin 包与 mcp 包真实代码）。
func TestPluginDiscoveryConnect(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".kaelvoris", "plugins")
	if _, err := os.Stat(filepath.Join(root, "office", "plugin.json")); err != nil {
		t.Skip("office plugin not installed")
	}
	// 1) 发现与启用检查。
	plugs := plugin.List()
	if len(plugs) == 0 {
		t.Fatal("no plugins found")
	}
	var p *plugin.Manifest
	for _, x := range plugs {
		if x.ID == "office" {
			p = x
		}
	}
	if p == nil {
		t.Fatal("office plugin not found")
	}
	if !plugin.IsEnabled(p.ID) {
		t.Fatal("office plugin should be enabled by default")
	}
	// 2) 推导 MCP 配置（同 buildMCPTools 插件分支）。
	name, transport, command, url, args, env, headers, ok := p.MCPServerOf()
	if !ok || name != "office" || transport != "stdio" {
		t.Fatalf("mcp export: %+v ok=%v", p.MCP, ok)
	}
	if command != "" && !filepath.IsAbs(command) {
		command = filepath.Join(p.Dir, command)
	}
	if env == nil {
		env = map[string]string{}
	}
	env["GOFFICE_WORKSPACE"] = "."
	cfg := mcp.ServerConfig{Name: name, Command: command, Args: args, Env: env, URL: url, Headers: headers}
	// 3) 连接 + 列工具。
	ctx := context.Background()
	cli, n, err := mcp.RegisterAll(ctx, cfg, func(name string, t *mcp.RemoteTool) error { return nil })
	if err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if cli != nil {
		defer cli.Close()
	}
	if n < 8 {
		t.Fatalf("expected 8 office tools, got %d", n)
	}
	// 4) 工具直接注册进 Registry（buildMCPTools 的 register 语义）。
	reg := tools.NewRegistry()
	cli2, n2, err := mcp.RegisterAll(ctx, cfg, func(name string, t *mcp.RemoteTool) error {
		return reg.Register(t)
	})
	if err != nil {
		t.Fatalf("RegisterAll(reg): %v", err)
	}
	if cli2 != nil {
		defer cli2.Close()
	}
	if n2 != 8 {
		t.Fatalf("registered tool count = %d, want 8", n2)
	}
	if _, ok := reg.Get("mcp_office_office_new"); !ok {
		names := reg.Names()
		t.Fatalf("registry missing mcp_office_office_new; got %v", names)
	}
}
