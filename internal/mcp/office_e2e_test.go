package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestOfficePluginE2E 端到端：以 mcp 客户端方式连接 office 插件（真实子进程），
// 验证 initialize → tools/list → tools/call（office_new）完整链路。
func TestOfficePluginE2E(t *testing.T) {
	exe := `D:\code\gocoder\plugins\office-plugin\bin\goffice.exe`
	if _, err := os.Stat(exe); err != nil {
		t.Skip("goffice.exe not built")
	}
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	os.MkdirAll(ws, 0o755)
	cfg := ServerConfig{
		Name:    "office",
		Command: exe,
		Env:     map[string]string{"GOFFICE_WORKSPACE": ws},
	}
	ctx := context.Background()
	cli, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cli.Close()
	tools, err := cli.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) < 8 {
		t.Fatalf("expected >=8 tools, got %d", len(tools))
	}
	// 调用 office_new 创建 xlsx。
	out, err := cli.CallTool(ctx, "office_new", json.RawMessage(`{"file":"test.xlsx"}`))
	if err != nil {
		t.Fatalf("CallTool office_new: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ws, "test.xlsx")); err != nil {
		t.Fatalf("office_new did not create file: %v", err)
	}
}
