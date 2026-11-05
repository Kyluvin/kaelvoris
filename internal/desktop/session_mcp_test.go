package desktop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaelvoris/internal/session"
	"kaelvoris/internal/tools"
)

// TestSessionMCPDisabledPersistence 会话级 MCP 禁用字段持久化回归：
// SetMCPDisabled → Snapshot/Summary/JSON 落盘 → 回读，全链路不丢
// （右侧会话设置面板的开关状态以会话记录为唯一权威源，重启后不丢）。
func TestSessionMCPDisabledPersistence(t *testing.T) {
	tmp := t.TempDir()
	s := &session.Session{ID: "sess-mcp", ChannelID: "desktop", CreatedAt: time.Now()}
	if s.Snapshot().MCPDisabled {
		t.Fatal("new session should default MCPDisabled=false")
	}

	// 1) 开启禁用 → Snapshot/Summary 透传。
	s.SetMCPDisabled(true)
	if !s.Snapshot().MCPDisabled {
		t.Fatal("Snapshot should carry MCPDisabled=true after SetMCPDisabled(true)")
	}
	if !s.Summary().MCPDisabled {
		t.Fatal("Summary should carry MCPDisabled=true")
	}

	// 2) JSON 序列化含 mcpDisabled，反序列化回读不丢（磁盘持久化语义）。
	d := s.Snapshot()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"mcpDisabled":true`) {
		t.Fatalf("SessionData JSON should contain mcpDisabled:true, got: %s", b)
	}
	var back session.SessionData
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.MCPDisabled {
		t.Fatal("SessionData roundtrip should keep MCPDisabled=true")
	}
	if got := back.IntoSession(); !got.MCPDisabled {
		t.Fatal("IntoSession should keep MCPDisabled=true")
	}

	// 3) 真实持久化路径：Store 落盘（Save 异步刷盘无返回值）→ FlushAll 后从盘加载。
	store := session.NewStore(filepath.Join(tmp, "sessions"))
	store.Save(s)
	store.FlushAll()
	reloaded, err := store.Get("sess-mcp")
	if err != nil || reloaded == nil {
		t.Fatalf("store get after save: %v", err)
	}
	if !reloaded.Snapshot().MCPDisabled {
		t.Fatal("reloaded session from disk should keep MCPDisabled=true")
	}

	// 4) 关闭禁用 → 恢复启用。
	s.SetMCPDisabled(false)
	if s.Snapshot().MCPDisabled || s.Summary().MCPDisabled {
		t.Fatal("SetMCPDisabled(false) should clear the flag")
	}
}

// TestSessionMCPDisabledBuildPath 会话禁用 MCP 的双保险负路径行为断言：
//  1. 定义侧 buildMCPTools 在 sessionMCPDisabled=true 时返回空（不连接不注册，
//     模型完全不可见，优先级高于员工白名单——员工允许也连不上）；
//  2. 常规路径（未禁用）未被破坏：失败降级跳过仍成立；
//  3. 执行侧：会话禁用时的 mcpDenyHook 拒绝 mcp_* 调用、原因可读；
//     非 MCP 工具不受影响（组合钩子不越界拦截）。
func TestSessionMCPDisabledBuildPath(t *testing.T) {
	tmpHome, err := os.MkdirTemp("", "sess_mcp_home_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpHome)
	old := os.Getenv("USERPROFILE")
	t.Setenv("USERPROFILE", tmpHome)
	defer func() { os.Setenv("USERPROFILE", old) }()

	// 配置一个已启用的 stdio 服务器（命令不存在：若禁用失效将被真实连接，
	// 注册失败即暴露；若正常路径退化也会在本用例暴露）。
	dir := filepath.Join(tmpHome, ".kaelvoris")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp-servers.json"),
		[]byte(`[{"name":"probe","transport":"stdio","command":"definitely-not-exist-cmd","enabled":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &App{}

	// 场景 1：sessionMCPDisabled=true → 不连接不注册（mcp_ 前缀工具不可见）。
	reg := tools.NewRegistry()
	clients := a.buildMCPTools(context.Background(), reg, nil, tmpHome, nil, true)
	if len(clients) != 0 {
		t.Fatalf("session MCP disabled should connect nothing, got %d clients", len(clients))
	}
	if _, ok := reg.Get("mcp_probe_anything"); ok {
		t.Fatal("mcp_* tools must not be registered when session MCP disabled")
	}

	// 场景 2：未禁用（false）→ 走正常连接路径（命令不存在 → 连接失败降级跳过，
	// 不 panic、不注册），验证加参后常规路径未被破坏。
	clients = a.buildMCPTools(context.Background(), reg, nil, tmpHome, nil, false)
	if len(clients) != 0 {
		t.Fatalf("nonexistent command should fail-skip, got %d clients", len(clients))
	}
	if _, ok := reg.Get("mcp_probe_anything"); ok {
		t.Fatal("failed server must not register tools")
	}

	// 场景 3（执行侧兜底钩子）：会话禁用时 mcp_* 调用被拒绝、原因可读；
	// 非 MCP 工具不受影响（组合钩子不越界拦截）。
	hook := mcpDenyHook(nil, true)
	if allow, _, _ := hook("fs_read", `{"path":"x.txt"}`); !allow {
		t.Fatal("non-mcp tool should pass through the session-disable hook")
	}
	allow, _, blockMsg := hook("mcp_probe_tool", "{}")
	if allow {
		t.Fatal("mcp_* call must be denied when session MCP disabled")
	}
	if !strings.Contains(blockMsg, "禁用 MCP") {
		t.Fatalf("deny reason should mention MCP disabled, got: %s", blockMsg)
	}

	// 场景 4：会话未禁用 → 兜底钩子不改变行为（无已有钩子时保持 nil，
	// 引擎侧 HookPreToolUse==nil 即无钩子语义）。
	if hookOff := mcpDenyHook(nil, false); hookOff != nil {
		if allow, _, _ := hookOff("mcp_probe_tool", "{}"); !allow {
			t.Fatal("session without disable flag must not block mcp tools")
		}
	}
}
