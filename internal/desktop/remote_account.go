package desktop

// remote_account.go 桌面端账号登录（P-Account 统一账号体系 · PC 端入口）：
//
//	服务端已有统一账号表（accounts.json users，PC/移动/管理端互通），桌面端此前
//	仅以「节点令牌」接入远程服务中心，无账号一说。本文件为桌面端补齐账号登录：
//	  - 登录协议：拨号服务端 /ws/client → 首条 auth（user+pass）→ auth.ok / auth.err
//	    （对齐 web/admin、web/mobile 登录路径，服务端 Hub.AuthClient 统一校验）。
//	  - 登录态持久化 ~/.kaelvoris/remote_account.json（0600），启动自动重登；
//	    仅在远程功能开启（remote.json enabled）时展示入口（前端控制）。
//	  - 状态经 kaelvoris:remote_account 推送（顶栏账号按钮渲染数据源）。
//
//	边界：登录仅为「账号身份打通」（远程任务发布/审批以账号身份进行）；
//	节点令牌仍走 remote.json（Agent 通道与账号通道互不干扰）。

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"kaelvoris/internal/remote"
)

// RemoteAccountState 账号状态（前端顶栏/登录弹窗渲染数据源）。
type RemoteAccountState struct {
	Enabled     bool   `json:"enabled"` // 远程功能是否开启（false = 隐藏入口）
	ServerURL   string `json:"serverURL,omitempty"`
	LoggedIn    bool   `json:"loggedIn"`
	User        string `json:"user,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Role        string `json:"role,omitempty"` // admin | client
	Error       string `json:"error,omitempty"`
}

// storedAccount 登录凭据持久化结构（~/.kaelvoris/remote_account.json）。
type storedAccount struct {
	User string `json:"user"`
	Pass string `json:"pass"`
}

// remoteAccountPath 凭据文件路径。
func remoteAccountPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "remote_account.json")
}

// loadStoredAccount 读取已保存凭据（无文件/解析失败返回零值，不致命）。
func loadStoredAccount() storedAccount {
	var s storedAccount
	if err := jsonReadFileBOM(remoteAccountPath(), &s); err != nil && !os.IsNotExist(err) {
		slog.Warn("remote_account.json 解析失败（已忽略）", "err", err)
	}
	return s
}

// GetRemoteAccount 前端绑定：当前账号状态。
func (a *App) GetRemoteAccount() RemoteAccountState {
	a.accMu.Lock()
	defer a.accMu.Unlock()
	cfg := a.remoteCfg
	return RemoteAccountState{
		Enabled:     cfg.Enabled,
		ServerURL:   cfg.ServerURL,
		LoggedIn:    a.accLoggedIn,
		User:        a.accUser,
		DisplayName: a.accDisplayName,
		Role:        a.accRole,
		Error:       a.accError,
	}
}

// RemoteLogin 前端绑定：账号登录（拨号服务端 /ws/client 走 auth 协议）。
// 成功后凭据持久化（启动自动重登），状态推送前端。
func (a *App) RemoteLogin(user, pass string) (RemoteAccountState, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return a.GetRemoteAccount(), fmt.Errorf("请输入用户名")
	}
	if pass == "" {
		return a.GetRemoteAccount(), fmt.Errorf("请输入密码")
	}
	cfg := a.loadRemoteConfig()
	if !cfg.Enabled || cfg.ServerURL == "" {
		return a.GetRemoteAccount(), fmt.Errorf("请先在 设置 → 远程节点 开启远程并填写服务中心地址")
	}
	st, err := a.remoteAccountAuth(cfg.ServerURL, user, pass)
	if err != nil {
		// 失败不落盘；状态带 error 推送（弹窗内直接展示）。
		a.accMu.Lock()
		a.accError = err.Error()
		a.accMu.Unlock()
		a.pushRemoteAccount()
		return st, err
	}
	// 成功：持久化凭据（启动自动重登）+ 内存态。
	sa := storedAccount{User: user, Pass: pass}
	if b, err := json.MarshalIndent(sa, "", "  "); err == nil {
		if werr := os.WriteFile(remoteAccountPath(), b, 0o600); werr != nil {
			slog.Warn("remote_account.json 保存失败（仅本次会话生效）", "err", werr)
		}
	}
	a.accMu.Lock()
	a.accLoggedIn = true
	a.accUser = st.User
	a.accDisplayName = st.DisplayName
	a.accRole = st.Role
	a.accError = ""
	a.accMu.Unlock()
	a.pushRemoteAccount()
	// P-Remote-Nodes：登录成功 → 启动远程会话通道（/ws/client）。
	a.rnNotifyStatusChanged(true)
	// 指挥权隔离（P-Account，去令牌化 2026-10）：连接器已在运行（启动早于登录
	// 的时序）时，其注册帧 User/Pass 为空 → 匿名注册、节点不可被指挥。登录后
	// 重发注册（携带账号凭据），服务端校验后立即绑定 Owner（同账号客户端立即可
	// 指挥）；连接器未运行（退出登录后重登）时由 rnNotifyStatusChanged 启动
	// 的连接器直接带凭据注册。
	go func() {
		a.remoteMu.Lock()
		rc := a.remoteClient
		a.remoteMu.Unlock()
		if rc != nil {
			rc.SendModeUpdate()
		} else {
			a.startRemoteClient()
		}
	}()
	slog.Info("远程账号登录成功", "user", st.User, "role", st.Role, "server", cfg.ServerURL)
	return st, nil
}

// RemoteLogout 前端绑定：退出登录（清凭据与内存态）。
func (a *App) RemoteLogout() RemoteAccountState {
	_ = os.Remove(remoteAccountPath())
	// 同步清空 remote.json 内的 User/Pass（2026-11 表单回显修复）：凭据现也
	// 落 remote.json（设置面板表单回显数据源），退出登录需一并清除，否则
	// 重开设置页仍回显已退出的账号密码。
	if cfg := a.loadRemoteConfig(); cfg.User != "" || cfg.Pass != "" {
		cfg.User, cfg.Pass = "", ""
		if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			if werr := os.WriteFile(remoteConfigPath(), b, 0o600); werr != nil {
				slog.Warn("退出登录清理 remote.json 凭据失败", "err", werr)
			}
		}
		a.remoteMu.Lock()
		a.remoteCfg.User, a.remoteCfg.Pass = "", ""
		a.remoteMu.Unlock()
	}
	a.accMu.Lock()
	a.accLoggedIn = false
	a.accUser = ""
	a.accDisplayName = ""
	a.accRole = ""
	a.accError = ""
	a.accMu.Unlock()
	// 退出账号即主动下线节点：发送 agent.offline 让服务端把节点标记为 disabled
	//（从移动端/管理端列表移除并拒绝后续任务派发/续聊；重登账号重连自动恢复）。
	// 修复：此前退出仅清凭据，远程连接器未断 → 服务端仍显示该节点在线、
	// 仍可接收任务派发，导致"账号已退出但节点仍在服务"的不一致。
	a.stopRemoteClient()
	a.pushRemoteAccount()
	// P-Remote-Nodes：退出登录 → 断开远程会话通道并清空会话映射。
	a.rnNotifyStatusChanged(false)
	return a.GetRemoteAccount()
}

// remoteAccountAuth 走服务端 auth 协议校验账号（一次性短连接，对齐
// web/admin 登录路径：/ws/client → auth → auth.ok/auth.err）。
func (a *App) remoteAccountAuth(serverURL, user, pass string) (RemoteAccountState, error) {
	st := RemoteAccountState{ServerURL: serverURL}
	dialer := websocket.Dialer{HandshakeTimeout: 8 * time.Second}
	ws, _, err := dialer.Dial(wsURLFromClient(serverURL), nil)
	if err != nil {
		return st, fmt.Errorf("无法连接服务中心：%w", err)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(8 * time.Second))
	_ = ws.SetWriteDeadline(time.Now().Add(8 * time.Second))
	if err := ws.WriteJSON(remote.NewEnvelope("", remote.TypeAuth, remote.Auth{User: user, Pass: pass})); err != nil {
		return st, fmt.Errorf("发送认证失败：%w", err)
	}
	var e remote.Envelope
	if err := ws.ReadJSON(&e); err != nil {
		return st, fmt.Errorf("认证响应超时：%w", err)
	}
	switch e.Type {
	case remote.TypeAuthOK:
		var ok remote.AuthOK
		_ = e.Decode(&ok)
		st.LoggedIn = true
		st.User = user
		st.DisplayName = ok.DisplayName
		st.Role = ok.Role
		return st, nil
	case remote.TypeAuthErr:
		var ep remote.ErrorPayload
		_ = e.Decode(&ep)
		if ep.Message != "" {
			return st, fmt.Errorf("%s", ep.Message)
		}
		return st, fmt.Errorf("用户名或密码错误")
	default:
		return st, fmt.Errorf("服务端响应异常（%s）", e.Type)
	}
}

// autoReloginAccount 启动自动重登：有已保存凭据且远程开启时静默重登，
// 成败均推送状态（前端渲染顶栏入口；失败不弹窗）。
func (a *App) autoReloginAccount() {
	sa := loadStoredAccount()
	if sa.User == "" || sa.Pass == "" {
		return
	}
	cfg := a.remoteCfg
	if !cfg.Enabled || cfg.ServerURL == "" {
		return
	}
	st, err := a.remoteAccountAuth(cfg.ServerURL, sa.User, sa.Pass)
	if err != nil {
		a.accMu.Lock()
		a.accError = err.Error()
		a.accMu.Unlock()
		a.pushRemoteAccount()
		slog.Info("远程账号自动重登失败", "user", sa.User, "err", err)
		return
	}
	a.accMu.Lock()
	a.accLoggedIn = true
	a.accUser = st.User
	a.accDisplayName = st.DisplayName
	a.accRole = st.Role
	a.accError = ""
	a.accMu.Unlock()
	a.pushRemoteAccount()
	// P-Remote-Nodes：自动重登成功 → 启动远程会话通道。
	a.rnNotifyStatusChanged(true)
	// 指挥权隔离（P-Account）：同 RemoteLogin —— 连接器已在运行时重发注册绑定归属。
	go func() {
		a.remoteMu.Lock()
		rc := a.remoteClient
		a.remoteMu.Unlock()
		if rc != nil {
			rc.SendModeUpdate()
		}
	}()
	slog.Info("远程账号自动重登成功", "user", st.User, "role", st.Role)
}

// pushRemoteAccount 推送账号状态到前端。
func (a *App) pushRemoteAccount() {
	if a.ctx == nil {
		return
	}
	uiEmit(a.ctx, "kaelvoris:remote_account", a.GetRemoteAccount())
}

// wsURLFromClient http(s)://host:port → ws(s)://host:port/ws/client
// （与 wsURLFromHTTP 同构：agent 通道走 /ws/agent，账号/客户端通道走 /ws/client）。
func wsURLFromClient(serverURL string) string {
	u := strings.TrimRight(serverURL, "/")
	switch {
	case len(u) >= 8 && u[:8] == "https://":
		u = "wss://" + u[8:]
	case len(u) >= 7 && u[:7] == "http://":
		u = "ws://" + u[7:]
	}
	// 已带 ws:// 或 wss:// 前缀时直接复用（修复：u[:4]!="ws" 对 "ws://" 恒为真，
	// 二次拼接 ws://ws://... 后 host 变成字面量 "ws"，报 lookup ws: no such host）。
	if strings.HasPrefix(u, "ws://") || strings.HasPrefix(u, "wss://") {
		return u + "/ws/client"
	}
	if u != "" {
		u = "ws://" + u
	}
	return u + "/ws/client"
}
