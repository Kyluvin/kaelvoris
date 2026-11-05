package desktop

// 回归测试：设置 → 服务中心连接「保存后用户名密码消失」（2026-11 用户报告）。
//
// 根因：RemoteConfig 结构体无 User/Pass 字段，前端提交的凭据被 Go 反序列化
// 静默丢弃；登录成功/重开设置页 loadRemote 拉回不含凭据的配置覆盖表单。
//
// 修复后行为：
//  1. SaveRemoteConfig 持久化 user/pass 到 remote.json（表单回显数据源）；
//  2. user/pass 成对非空时同步写 remote_account.json（启动自动重登数据源）；
//  3. RemoteLogout 清除两处凭据（remote.json + remote_account.json）。
//
// 全部隔离 HOME（t.TempDir），不污染真实 ~/.kaelvoris。

import (
	"os"
	"path/filepath"
	"testing"
)

// mustKaelvorisDir 创建隔离 HOME 下的 ~/.kaelvoris 目录
// （SaveRemoteConfig/凭据文件均落在该目录，真实应用启动时已存在；
// 测试环境需自建，否则 os.WriteFile 报路径不存在）。
func mustKaelvorisDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, ".kaelvoris"), 0o700); err != nil {
		t.Fatalf("创建 ~/.kaelvoris: %v", err)
	}
}

// TestSaveRemoteConfigKeepsCredentials 保存后配置须保留凭据且回显。
func TestSaveRemoteConfigKeepsCredentials(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("USERPROFILE", tmpHome)
	t.Setenv("HOME", tmpHome)
	mustKaelvorisDir(t)

	a := &App{}
	in := RemoteConfig{
		Enabled:   false,
		ServerURL: "http://127.0.0.1:18900",
		User:      "alice",
		Pass:      "secret123",
		Name:      "PC-Test",
	}
	if err := a.SaveRemoteConfig(in); err != nil {
		t.Fatalf("SaveRemoteConfig: %v", err)
	}

	// 1) GetRemoteConfig 回显凭据（表单不再被清空的核心断言）。
	got := a.GetRemoteConfig()
	if got.User != "alice" || got.Pass != "secret123" {
		t.Fatalf("GetRemoteConfig 凭据丢失: got user=%q pass=%q, want alice/secret123", got.User, got.Pass)
	}
	if got.ServerURL != "http://127.0.0.1:18900" {
		t.Fatalf("ServerURL = %q", got.ServerURL)
	}

	// 2) remote.json 落盘含凭据。
	var disk RemoteConfig
	if err := jsonReadFileBOM(remoteConfigPath(), &disk); err != nil {
		t.Fatalf("读取 remote.json: %v", err)
	}
	if disk.User != "alice" || disk.Pass != "secret123" {
		t.Fatalf("remote.json 凭据未落盘: user=%q pass=%q", disk.User, disk.Pass)
	}
}

// TestSaveRemoteConfigSyncsAccountFile 凭据成对非空时同步写 remote_account.json
// （仅点保存也保住凭据，启动自动重登可用）。
func TestSaveRemoteConfigSyncsAccountFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("USERPROFILE", tmpHome)
	t.Setenv("HOME", tmpHome)
	mustKaelvorisDir(t)

	a := &App{}
	if err := a.SaveRemoteConfig(RemoteConfig{
		ServerURL: "http://127.0.0.1:18900",
		User:      "bob",
		Pass:      "pw-bob",
	}); err != nil {
		t.Fatalf("SaveRemoteConfig: %v", err)
	}
	sa := loadStoredAccount()
	if sa.User != "bob" || sa.Pass != "pw-bob" {
		t.Fatalf("remote_account.json 未同步: user=%q pass=%q, want bob/pw-bob", sa.User, sa.Pass)
	}

	// 仅改服务中心地址（凭据为空）不误清已有凭据。
	if err := a.SaveRemoteConfig(RemoteConfig{
		ServerURL: "http://127.0.0.1:28900",
		User:      "",
		Pass:      "",
		NodeID:    "node-keep",
	}); err != nil {
		t.Fatalf("SaveRemoteConfig(空凭据): %v", err)
	}
	sa = loadStoredAccount()
	if sa.User != "bob" || sa.Pass != "pw-bob" {
		t.Fatalf("空凭据保存误清 remote_account.json: user=%q pass=%q", sa.User, sa.Pass)
	}
}

// TestRemoteLogoutClearsStoredCredentials 退出登录清两处凭据。
func TestRemoteLogoutClearsStoredCredentials(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("USERPROFILE", tmpHome)
	t.Setenv("HOME", tmpHome)
	mustKaelvorisDir(t)

	a := &App{}
	if err := a.SaveRemoteConfig(RemoteConfig{
		ServerURL: "http://127.0.0.1:18900",
		User:      "carol",
		Pass:      "pw-carol",
	}); err != nil {
		t.Fatalf("SaveRemoteConfig: %v", err)
	}

	a.RemoteLogout() // ctx 为 nil 不推送事件，零值安全

	if st := a.GetRemoteAccount(); st.LoggedIn {
		t.Fatal("退出后 loggedIn 应为 false")
	}
	cfg := a.GetRemoteConfig()
	if cfg.User != "" || cfg.Pass != "" {
		t.Fatalf("退出后 remote.json 凭据应清空: user=%q pass=%q", cfg.User, cfg.Pass)
	}
	sa := loadStoredAccount()
	if sa.User != "" || sa.Pass != "" {
		t.Fatalf("退出后 remote_account.json 应不存在/为空: user=%q pass=%q", sa.User, sa.Pass)
	}
	if _, err := os.Stat(remoteAccountPath()); !os.IsNotExist(err) {
		t.Fatalf("remote_account.json 应已删除, stat err = %v", err)
	}
}
