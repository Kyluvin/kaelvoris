package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

// 存量互顶修复回归（0.7.2 线上复现路径，App 客户端层）：
//
// 时序：应用启动 → startRemoteClient 用 remote.json 旧令牌（账号级对）拉起连接器
// → 用户登录触发 provisionNodeToken → 服务端按机器指纹换发机器专属令牌并写回
// remote.json（NodeID/Token 都换了）→ **旧连接器在途注册应答**携带旧令牌家族
// base（如 node-default）回调 OnNodeIDAligned。
//
// 修复前：无条件把 base 写回 remote.json 并覆盖 a.remoteCfg.NodeID —— 刚写回的
// 新机器身份被还原成 node-default，重启后继续以错误身份注册（线上 remote.json
// 反复还原、双机互顶的根因之二）。
//
// 修复后：OnNodeIDAligned 以连接器启动令牌快照（startToken）做代际校验，
// 配置令牌已换发 → 丢弃对齐写回。本测试直接构造回调闭包等价场景断言。

// TestOnNodeIDAlignedTokenGenerationGuard 代际校验：令牌换代后旧家族对齐写回被丢弃。
func TestOnNodeIDAlignedTokenGenerationGuard(t *testing.T) {
	// 隔离 HOME，避免污染真实 ~/.kaelvoris/remote.json。
	tmpHome := t.TempDir()
	t.Setenv("USERPROFILE", tmpHome)
	t.Setenv("HOME", tmpHome)

	newToken := "ghnode-machine-aaaaaaaaaaaaaaaa"
	oldToken := "ghnode-legacy-bbbbbbbbbbbbbbbb"
	legacyBase := "node-default"

	a := &App{}
	a.remoteMu.Lock()
	a.remoteCfg = RemoteConfig{
		Enabled:   true,
		ServerURL: "http://127.0.0.1:1",
		NodeID:    "node-machine-111111111111", // provision 刚写回的新机器身份
		Token:     newToken,                    // 配置令牌已换代
		Name:      "PC-A",
	}
	a.remoteMu.Unlock()

	startToken := oldToken // 连接器仍持旧令牌（幂等未重启的旧连接器）
	var alignedWriteback func(baseNodeID string)
	// 构造与 remote_panel.go startRemoteClient 内 OnNodeIDAligned 相同的闭包逻辑
	//（复制实现有漂移风险，但回调闭包无法从外部构造 —— 这里以行为等价断言兜底，
	// 真正的实现一致性由 TestOnNodeIDAlignedSourceMatchesGuard 源码检查兜底）。
	alignedWriteback = func(baseNodeID string) {
		a.remoteMu.Lock()
		cfg := a.remoteCfg
		a.remoteMu.Unlock()
		if cfg.NodeID == baseNodeID {
			return
		}
		if cfg.Token != startToken {
			t.Logf("忽略节点 ID 对齐写回（配置令牌已换发，身份以新令牌开通为准）staleBase=%s", baseNodeID)
			return
		}
		cfg.NodeID = baseNodeID
		a.remoteMu.Lock()
		a.remoteCfg = cfg
		a.remoteMu.Unlock()
	}

	alignedWriteback(legacyBase) // 旧连接器对齐写回（修复前会覆盖 NodeID）

	a.remoteMu.Lock()
	nodeID := a.remoteCfg.NodeID
	a.remoteMu.Unlock()
	if nodeID != "node-machine-111111111111" {
		t.Fatalf("对齐写回必须被代际校验丢弃：NodeID 被污染为 %q", nodeID)
	}

	// 同代令牌（startToken == cfg.Token）时对齐写回仍生效（正常对齐路径不回归）。
	startToken = newToken
	alignedWriteback(legacyBase)
	a.remoteMu.Lock()
	nodeID = a.remoteCfg.NodeID
	a.remoteMu.Unlock()
	if nodeID != legacyBase {
		t.Fatalf("同代令牌对齐写回应生效：NodeID=%q want %q", nodeID, legacyBase)
	}
}

// TestOnNodeIDAlignedSourceMatchesGuard 源码一致性兜底：startRemoteClient 内
// OnNodeIDAligned 必须包含 startToken 代际校验（防止后续重构丢失修复）。
func TestOnNodeIDAlignedSourceMatchesGuard(t *testing.T) {
	b, err := os.ReadFile("remote_panel.go")
	if err != nil {
		t.Fatalf("读取 remote_panel.go: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"startToken := cfg.Token",
		"if cfg.Token != startToken",
	} {
		if !contains(src, want) {
			t.Fatalf("remote_panel.go 缺少代际校验代码 %q（OnNodeIDAligned 修复被移除？）", want)
		}
	}
	_ = filepath.Join // 保持 import（Windows 路径构造示例，避免未使用告警）
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
