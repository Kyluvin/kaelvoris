package desktop

// remote_panel.go 远程服务面板（P-Remote 远程控制面）：
//
//	PC 端作为「节点」连接远程服务中心（kaelvoris-server）：开启后自动注册，
//	手机 H5 / 管理后台发布任务 → 服务端经 WebSocket 派发到本节点 →
//	复用桌面端内核执行（buildKernel / buildRunEngine / makeSayBridge 同款路径，
//	对齐 channel_lark.go 渠道内核复用模式）→ 事件流/结果回传服务端 → 移动端。
//
// 设计要点：
//   - 会话 ChannelID = "remote"，会话 ID = 服务端下发的 sessionId（remote-<id>）。
//   - say 双路桥：桌面端事件桥（历史留痕 + 前端实时旁观）+ 远程任务事件回传
//     （text/tool/error/completion 定型帧；partial 节流后照推供移动端流式渲染）。
//   - ask 审批闭环：引擎 ask 挂起 → task_event(kind=ask) 推服务端 → 移动端
//     审批卡片 → ask.reply → ApproveAsk 应答（run.askCh 通道，与桌面端同源）。
//   - 任务终态：agent.task_done（ok/result/error）→ 服务端置状态并广播。
//   - taskID 映射：服务端派发仅携带 sessionID，任务 ID 经 remoteTaskOf
//     （sessionID → taskID）在会话/任务对应关系建立处登记；事件/终态回传
//     引用它，否则服务端 TaskDone 查无此任务、手机端收不到结果。
//   - 运行中续聊：enqueueSessionMsg 入队，本轮结束自动续发（对齐渠道语义）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/employee"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/modes"
	"kaelvoris/internal/remote"
	"kaelvoris/internal/roadmap"
	"kaelvoris/internal/session"
	"kaelvoris/internal/tools"
	"kaelvoris/internal/version"
	"kaelvoris/pkg/message"
)

// RemoteConfig 远程节点配置（持久化 ~/.kaelvoris/remote.json）。
type RemoteConfig struct {
	Enabled   bool   `json:"enabled"`
	ServerURL string `json:"serverURL"` // 服务端地址（http://host:8900）
	Token     string `json:"token"`     // 节点令牌（服务端 accounts.json 分配）
	NodeID    string `json:"nodeID"`    // 节点 ID（首次自动生成后固定）
	Name      string `json:"name"`      // 显示名（默认主机名）
	// 登录账号（服务中心统一账号，2026-11 表单清空修复）：此前结构体无
	// User/Pass，前端「服务中心连接」面板提交的凭据被反序列化静默丢弃，
	// 登录/重开设置页 loadRemote 拉回空凭据覆盖表单（用户名密码消失根因）。
	// remote.json 权限 0600，与 remote_account.json 同级安全；字段同时作为
	// 表单回显数据源，退出登录时随 RemoteLogout 一并清理。
	User string `json:"user"` // 登录账号（表单回显 + SaveRemoteConfig 同步 remote_account.json）
	Pass string `json:"pass"` // 登录密码
}

// RemoteStatus 远程连接状态（前端侧栏/设置页展示）。
type RemoteStatus struct {
	Enabled bool   `json:"enabled"`
	Online  bool   `json:"online"`
	Error   string `json:"error,omitempty"`
	// 本实例派生节点身份（多开槽位派生，instance_slot.go）：节点页展示，
	// 移动端/管理后台按此 ID 指定本实例派发任务。
	NodeID string `json:"nodeID,omitempty"`
	Name   string `json:"name,omitempty"`
	Slot   int    `json:"slot,omitempty"` // 实例槽位号（0 = 槽位分配失败，降级基础身份）
}

// remoteConfigPath 配置文件路径。
func remoteConfigPath() string { return RemoteConfigPath() }

// RemoteConfigPath remote.json 配置文件路径（导出：无头入口 main_headless.go 命令行覆写用）。
func RemoteConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "remote.json")
}

// JSONReadFileBOM 读取 JSON 并容错 UTF-8 BOM（导出：无头入口读 desktop.json/remote.json 用）。
func JSONReadFileBOM(path string, v interface{}) error { return jsonReadFileBOM(path, v) }

// jsonReadFileBOM 读取 JSON 配置文件并容错 UTF-8 BOM：PowerShell 5.1 写出的
// UTF-8 文件默认带 BOM（EF BB BF），encoding/json 会直接报 invalid character
// 'ï' 而失败；此前调用方 "_ = json.Unmarshal" 静默吞错 → 配置整体回退零值、
// 无任何日志（实际案例：remote.json 带 BOM → 远程连接器静默不启动）。
// 统一收口：剥 BOM 后再解析，失败返回 err 供调用方 slog 告警。
func jsonReadFileBOM(path string, v interface{}) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	return json.Unmarshal(b, v)
}

// LoadRemoteConfig 读取远程配置（导出：无头入口启动横幅读取用）。
func (a *App) LoadRemoteConfig() RemoteConfig { return a.loadRemoteConfig() }

// loadRemoteConfig 读取远程配置（无文件返回零值）。
func (a *App) loadRemoteConfig() RemoteConfig {
	var c RemoteConfig
	if err := jsonReadFileBOM(remoteConfigPath(), &c); err != nil && !os.IsNotExist(err) {
		slog.Warn("remote.json 解析失败（已忽略，按未配置处理）", "err", err)
	}
	return c
}

// GetRemoteConfig 前端绑定：读取远程配置（令牌掩码显示交给前端）。
func (a *App) GetRemoteConfig() RemoteConfig {
	c := a.loadRemoteConfig()
	// 表单回显回填（2026-11 表单清空修复配套）：历史版本凭据只落
	// remote_account.json（remote.json 无 user/pass），升级后首次打开设置页
	// 若不回填则表单空白，用户误以为账号丢失。两字段均空时从已存凭据回填
	//（只读不落盘，退出登录路径会同步清理两处）。
	if c.User == "" && c.Pass == "" {
		if sa := loadStoredAccount(); sa.User != "" {
			c.User, c.Pass = sa.User, sa.Pass
		}
	}
	return c
}

// GetRemoteStatus 前端绑定：远程连接状态（含本实例派生节点身份，远程节点页展示）。
func (a *App) GetRemoteStatus() RemoteStatus {
	nodeID, nodeName, slot := a.effectiveNodeIdentity()
	a.remoteMu.Lock()
	defer a.remoteMu.Unlock()
	st := RemoteStatus{Enabled: a.remoteCfg.Enabled, NodeID: nodeID, Name: nodeName, Slot: slot}
	if a.remoteClient != nil {
		st.Online = a.remoteClient.Online()
	}
	st.Error = a.remoteErr
	return st
}

// SaveRemoteConfig 保存并应用远程配置（enabled=true 启动连接器，false 停止）。
// 去令牌化（2026-10）：Token 不再强制（注册凭据 = 登录账号 User/Pass；
// 旧配置保留 Token 不影响，服务端仅用于上传鉴权兼容路径）。
func (a *App) SaveRemoteConfig(c RemoteConfig) error {
	if c.NodeID == "" {
		c.NodeID = remote.NewNodeID()
	}
	if c.Name == "" {
		host, _ := os.Hostname()
		c.Name = host
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(remoteConfigPath(), b, 0o600); err != nil {
		return err
	}
	// 账号凭据同步（2026-11 表单清空修复）：user/pass 成对非空时同步写
	// remote_account.json（登录态持久化，启动自动重登数据源），「仅点保存」
	// 也能保住凭据；为空时保留已有凭据（仅改服务中心地址不误清登录态）。
	if c.User != "" && c.Pass != "" {
		if ab, err := json.MarshalIndent(storedAccount{User: c.User, Pass: c.Pass}, "", "  "); err == nil {
			if werr := os.WriteFile(remoteAccountPath(), ab, 0o600); werr != nil {
				slog.Warn("remote_account.json 同步保存失败（仅 remote.json 生效）", "err", werr)
			}
		}
	}
	a.remoteMu.Lock()
	a.remoteCfg = c
	a.remoteErr = ""
	a.remoteMu.Unlock()

	// 重启连接器（先停旧）。
	a.stopRemoteClient()
	if c.Enabled {
		a.startRemoteClient()
	}
	return nil
}

// effectiveNodeIdentity 本实例生效的节点身份：按实例槽位派生（instance_slot.go
// deriveNodeIdentity）。槽位未分配（分配失败/未认领）时降级为基础身份，行为与
// 旧版一致。返回 (nodeID, nodeName, slot)。
func (a *App) effectiveNodeIdentity() (string, string, int) {
	a.remoteMu.Lock()
	defer a.remoteMu.Unlock()
	cfg := a.remoteCfg
	slot := a.instanceSlot
	id, name := deriveNodeIdentity(cfg.NodeID, cfg.Name, slot)
	return id, name, slot
}

// startRemoteClient 启动远程连接器（startup / 配置保存时调用）。
// 多开实例：先认领实例槽位，连接器以槽位派生节点身份注册（同令牌多实例在
// 服务端呈现为多个节点，互不顶替；见 instance_slot.go）。
func (a *App) startRemoteClient() {
	a.acquireSlot() // 幂等；失败降级基础身份（日志见 acquireSlot）

	a.remoteMu.Lock()
	cfg := a.remoteCfg
	// 去令牌化（2026-10）：注册凭据改统一账号（User/Pass 回调），令牌不再是
	// 硬门槛 —— 旧配置文件无 Token 时仍可匿名/账号注册（服务端校验账号）。
	if !cfg.Enabled || cfg.ServerURL == "" {
		a.remoteMu.Unlock()
		return
	}
	if a.remoteClient != nil {
		a.remoteMu.Unlock()
		return // 已在运行
	}
	a.remoteMu.Unlock()

	nodeID, nodeName, slot := a.effectiveNodeIdentity()
	remote.SetVersionInfo(version.Number)
	host, _ := os.Hostname()
	// 代际校验快照（存量互顶修复之二，0.7.2 线上复现路径）：连接器启动时
	// 持有的令牌快照。用户登录触发 provisionNodeToken 换发机器专属令牌后，
	// 旧连接器在途注册应答携带旧令牌家族 base 回调 OnNodeIDAligned —— 无校验
	// 会把刚写回的新机器身份覆盖回 node-default（remote.json 反复还原根因）。
	// OnNodeIDAligned 内比对 cfg.Token != startToken 即丢弃对齐写回。
	startToken := cfg.Token
	client := remote.NewClient(
		wsURLFromHTTP(cfg.ServerURL), cfg.Token, nodeID, nodeName,
		version.Number, host, runtime.GOOS, runtime.GOARCH,
		remote.ClientCallbacks{
			RunTask: func(sessionID, text string, images []string, mode string, taskID string) bool {
				return a.remoteRunTask(sessionID, text, images, mode, taskID)
			},
			Cancel: func(sessionID string, taskID string) {
				a.remoteCancel(sessionID, taskID)
			},
			ApproveAsk: func(sessionID string, response string, text string) error {
				return a.ApproveAsk(sessionID, response, text)
			},
			IsRunning: func(sessionID string) bool {
				return a.IsSessionRunning(sessionID)
			},
			SessionMessages: func(sessionID string) []interface{} {
				msgs := a.remoteSessionMessages(sessionID)
				out := make([]interface{}, len(msgs))
				for i, m := range msgs {
					out[i] = m
				}
				return out
			},
			SessionTodos: func(sessionID string) []interface{} {
				todos := a.GetTodos(sessionID)
				out := make([]interface{}, len(todos))
				for i, td := range todos {
					out[i] = td
				}
				return out
			},
			DeleteSession: func(sessionID string) {
				a.remoteDeleteSession(sessionID)
			},
			Analyze: func(sessionID string) {
				a.remoteAnalyzeSession(sessionID)
			},
			CleanupSessions: func(mode string) {
				a.remoteCleanupSessions(mode)
			},
			VerifyTasks: func(taskIDs []string, requestedBy string, verifyAll bool) {
				a.remoteVerifyAll(taskIDs, requestedBy, verifyAll)
			},
			Modes:       a.remoteModesInfo,
			CurrentMode: a.GetMode,
			// P-Account 指挥权隔离（去令牌化 2026-10）：注册帧上报当前登录账号凭据，
			// 服务端经统一账号表校验后绑定节点归属；未登录（空）时匿名注册、节点
			// 仍在线展示但不可被任何客户端指挥（登录后重注册生效）。
			Account: func() string {
				a.accMu.Lock()
				defer a.accMu.Unlock()
				return a.accUser
			},
			Pass: func() string {
				sa := loadStoredAccount()
				return sa.Pass
			},
			SetMode: func(slug string) error {
				// 校验模式存在后切换（PC 端唯一执行面，远端指令同步本地）。
				for _, m := range a.remoteModes() {
					if m.Slug == slug {
						a.SetMode(slug)
						return nil
					}
				}
				return fmt.Errorf("未知模式: %s", slug)
			},
			GetConfig: func() remote.NodeConfig {
				// 配置投影（desktop.json 子集）；APIKey 掩码：明文密钥不出节点。
				cfg := a.GetConfig()
				return remote.NodeConfig{
					BaseURL:             cfg.BaseURL,
					Model:               cfg.Model,
					APIKey:              remote.MaskAPIKey(cfg.APIKey),
					AutoApprove:         cfg.AutoApprove,
					ContextWindow:       cfg.ContextWindow,
					AutoCondensePercent: cfg.AutoCondensePercent,
					ImageGenBaseURL:     cfg.ImageGenBaseURL,
					ImageGenModel:       cfg.ImageGenModel,
					ImageGenAPIKey:      remote.MaskAPIKey(cfg.ImageGenAPIKey),
					VideoGenBaseURL:     cfg.VideoGenBaseURL,
					VideoGenModel:       cfg.VideoGenModel,
					VideoGenAPIKey:      remote.MaskAPIKey(cfg.VideoGenAPIKey),
					SpeechBaseURL:       cfg.SpeechBaseURL,
					SpeechModel:         cfg.SpeechModel,
					SpeechAPIKey:        remote.MaskAPIKey(cfg.SpeechAPIKey),
					SpeechVoice:         cfg.SpeechVoice,
				}
			},
			SetConfig: func(sc remote.SetConfig) error {
				// 合并保存：读当前配置 → 仅覆盖远端显式下发的字段 → SaveConfig
				//（校验/落盘/invalidate 一条龙；Workspace/RecentProjects 等本机字段天然保留）。
				cfg := a.GetConfig()
				// 条目化模型配置：远端下发的平铺字段需同步回写同类型生效条目
				//（profiles 存在时投影以条目为源，不回写则下次投影冲掉远端改动）。
				// 回写目标：该类第一个未停用条目；无条目时隐式建一条（仅当下发
				// 非空字段时；掩码/空值不动既有条目）。
				writeBack := func(kind, baseURL, model, apiKey string) {
					if baseURL == "" && model == "" && apiKey == "" {
						return
					}
					for i := range cfg.ModelProfiles {
						p := &cfg.ModelProfiles[i]
						if p.Kind == kind && !p.Disabled {
							if baseURL != "" {
								p.BaseURL = baseURL
							}
							if model != "" {
								p.Model = model
							}
							if apiKey != "" {
								p.APIKey = apiKey
							}
							return
						}
					}
					cfg.ModelProfiles = append(cfg.ModelProfiles, ModelProfile{Kind: kind, BaseURL: baseURL, Model: model, APIKey: apiKey})
				}
				if sc.BaseURL != nil {
					cfg.BaseURL = *sc.BaseURL
				}
				if sc.Model != nil {
					cfg.Model = *sc.Model
				}
				if sc.APIKey != "" && !strings.Contains(sc.APIKey, "****") {
					cfg.APIKey = sc.APIKey // 掩码形态 = 未修改（远端只见掩码），不回写
				}
				writeBack("chat", cfg.BaseURL, cfg.Model, cfg.APIKey)
				if sc.AutoApprove != nil {
					cfg.AutoApprove = *sc.AutoApprove
				}
				if sc.ContextWindow != nil {
					cfg.ContextWindow = *sc.ContextWindow
				}
				if sc.AutoCondensePercent != nil {
					cfg.AutoCondensePercent = *sc.AutoCondensePercent
				}
				if sc.ImageGenBaseURL != nil {
					cfg.ImageGenBaseURL = *sc.ImageGenBaseURL
				}
				if sc.ImageGenModel != nil {
					cfg.ImageGenModel = *sc.ImageGenModel
				}
				if sc.ImageGenAPIKey != "" && !strings.Contains(sc.ImageGenAPIKey, "****") {
					cfg.ImageGenAPIKey = sc.ImageGenAPIKey // 掩码形态 = 未修改，不回写
				}
				if sc.VideoGenBaseURL != nil {
					cfg.VideoGenBaseURL = *sc.VideoGenBaseURL
				}
				if sc.VideoGenModel != nil {
					cfg.VideoGenModel = *sc.VideoGenModel
				}
				if sc.VideoGenAPIKey != "" && !strings.Contains(sc.VideoGenAPIKey, "****") {
					cfg.VideoGenAPIKey = sc.VideoGenAPIKey // 掩码形态 = 未修改，不回写
				}
				if sc.SpeechBaseURL != nil {
					cfg.SpeechBaseURL = *sc.SpeechBaseURL
				}
				if sc.SpeechModel != nil {
					cfg.SpeechModel = *sc.SpeechModel
				}
				if sc.SpeechAPIKey != "" && !strings.Contains(sc.SpeechAPIKey, "****") {
					cfg.SpeechAPIKey = sc.SpeechAPIKey // 掩码形态 = 未修改，不回写
				}
				if sc.SpeechVoice != nil {
					cfg.SpeechVoice = *sc.SpeechVoice
				}
				writeBack("image", cfg.ImageGenBaseURL, cfg.ImageGenModel, cfg.ImageGenAPIKey)
				writeBack("video", cfg.VideoGenBaseURL, cfg.VideoGenModel, cfg.VideoGenAPIKey)
				writeBack("speech", cfg.SpeechBaseURL, cfg.SpeechModel, cfg.SpeechAPIKey)
				return a.SaveConfig(cfg)
			},
			OnStatus: func(online bool, errMsg string) {
				a.remoteMu.Lock()
				if !online && errMsg != "" {
					a.remoteErr = errMsg
				} else if online {
					a.remoteErr = ""
				}
				a.remoteMu.Unlock()
				a.pushRemoteStatus()
			},
			SkillList:    a.remoteSkillsMeta,
			EmployeeList: a.remoteEmployeeList,
			SkillGet:     a.remoteSkillGet,
			SkillSave:    a.remoteSkillSave,
			SkillDelete:  a.remoteSkillDelete,
			// 自主开发规划板（移动端「AR 自主开发」页数据链路；roadmap_panel.go 同源）。
			RoadmapData:        a.remoteRoadmapData,
			RoadmapPlan:        a.remoteRoadmapPlan,
			RoadmapDev:         a.remoteRoadmapDev,
			RoadmapDelete:      a.remoteRoadmapDelete,
			RoadmapDeleteBatch: a.remoteRoadmapDeleteBatch,
			RoadmapRetry:       a.remoteRoadmapRetry,
			RoadmapApprove:     a.remoteRoadmapApprove,
			RoadmapAuto:        a.remoteRoadmapAuto,
			// A2A 群聊回调（remote_group.go；群状态权威在本节点，服务端路由）。
			GroupCreate: func(req remote.GroupServerReq) { a.remoteGroupCreate(req) },
			// IM 中继回调下发（公网服务端代持 IM 平台回调 → 本节点执行；channel_im.go）。
			IMInbound:  func(msg remote.IMRelayInbound) { a.remoteIMInbound(msg) },
			GroupMsg:   func(req remote.GroupServerReq) { a.remoteGroupMsg(req) },
			GroupTask:  func(req remote.GroupServerReq) { a.remoteGroupTask(req) },
			GroupJoin:  func(req remote.GroupServerReq) { a.remoteGroupJoin(req) },
			GroupLeave: func(req remote.GroupServerReq) { a.remoteGroupLeave(req) },
			GroupSync:  func(req remote.GroupServerReq) { a.remoteGroupSync(req) },
			// 上线补传：每次注册成功后把本地会话历史/清单推到服务端镜像。
			// 修复：节点断线期间手机发布的任务照常执行，但 agent.messages 依赖
			// get.messages 请求触发，断线期无人拉取 → 服务端镜像空白 → 节点回
			// 线后手机点开任务卡仍看不到聊天记录。补传后镜像与服务端列表必然有历史。
			// client 经 remoteBackfillOnOnline 内部快照获取（回调构造期变量未就绪）。
			OnOnline: func() {
				a.remoteBackfillOnOnline()
				// 节点上线即上报技能列表（服务端缓存刷新 → skills 广播；
				// 兑底服务重启/节点换机后移动端技能库空白）。
				a.remoteSendSkillsList()
				// 同款兑底：上报自主开发规划板（移动端 AR 自主开发页数据源；
				// 服务重启/换机后 roadmap 缓存空白）。
				a.remoteSendRoadmap()
				// 同款兑底：上报 A2A 群聊列表（移动端群聊页数据源）。
				a.remoteSendGroups()
				// 同款兑底：上报 IM 中继渠道注册表（公网回调入口路由；渠道配置
				// 保存/服务重启后回调入口不失效）。
				a.remoteSendIMRelayState()
			},
			// 节点 ID 对齐持久化：注册应答学习到令牌家族基础 ID（如 node-default）
			// 时写回 remote.json（幂等：已是该 ID 则跳过）。修复：自生成 NodeID
			// 与令牌不同族 → 服务端每次注册都回落 + WARN 刷屏（66s 一次）。
			// 对齐后槽位派生 "基础ID-N" 与服务端令牌表同族，多开身份也不受影响。
			// 代际校验（2026-10 存量互顶修复）：cfg.Token 已换发（≠ 启动快照
			// startToken）说明旧连接器的在途应答携带的是旧家族 base，写回会把
			// 新机器身份还原 → 丢弃；同代令牌照常写回（正常对齐路径不回归）。
			OnNodeIDAligned: func(baseNodeID string) {
				a.remoteMu.Lock()
				cfg := a.remoteCfg
				a.remoteMu.Unlock()
				if cfg.NodeID == baseNodeID {
					return // 已对齐（并发重连多次触发时幂等跳过）
				}
				if cfg.Token != startToken {
					slog.Info("忽略节点 ID 对齐写回（配置令牌已换发，身份以新令牌开通为准）", "staleBase", baseNodeID)
					return
				}
				cfg.NodeID = baseNodeID
				if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
					if err := os.WriteFile(remoteConfigPath(), b, 0o600); err != nil {
						slog.Warn("节点 ID 对齐后写回 remote.json 失败", "err", err)
					} else {
						a.remoteMu.Lock()
						a.remoteCfg.NodeID = baseNodeID
						a.remoteMu.Unlock()
						slog.Info("节点 ID 已对齐并持久化", "nodeID", baseNodeID)
					}
				}
			},
		},
	)
	// 绘画链路：节点上传生成图到服务端 /api/images（WS 地址反推同源 HTTP 根）。
	client.SetHTTPBase(httpBaseFromWS(wsURLFromHTTP(cfg.ServerURL)))
	a.remoteMu.Lock()
	a.remoteClient = client
	a.remoteMu.Unlock()
	client.Start()
	slog.Info("远程节点连接器已启动", "server", cfg.ServerURL, "node", nodeID, "name", nodeName, "slot", slot)
}

// stopRemoteClient 停止连接器。
// 用 StopGraceful 而非 Stop：发送 agent.offline 通知服务端把节点标记为
// disabled=true（从移动端/管理端列表移除 + 拒绝后续派发/续聊），覆盖
// 关远程节点开关 / 退出账号 / 重启连接器 / 实例槽位切换 等主动下线场景。
func (a *App) stopRemoteClient() {
	a.remoteMu.Lock()
	c := a.remoteClient
	a.remoteClient = nil
	a.remoteMu.Unlock()
	if c != nil {
		c.StopGraceful()
	}
}

// pushRemoteStatus 推送远程状态到前端。
func (a *App) pushRemoteStatus() {
	if a.ctx == nil {
		return
	}
	uiEmit(a.ctx, "kaelvoris:remote_status", a.GetRemoteStatus())
}

// wsURLFromHTTP http(s)://host:port → ws(s)://host:port/ws/agent。
func wsURLFromHTTP(serverURL string) string {
	u := serverURL
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	// scheme 替换。
	switch {
	case len(u) >= 8 && u[:8] == "https://":
		u = "wss://" + u[8:]
	case len(u) >= 7 && u[:7] == "http://":
		u = "ws://" + u[7:]
	}
	// 已带 ws:// 或 wss:// 前缀时直接复用（修复：u[:4]!="ws" 对 "ws://" 恒为真——
	// 前 4 字符是 "ws:/"，导致二次拼接 ws://ws://...，gorilla 解析 host 变成字面量
	// "ws"，报 lookup ws: no such host；与 wsURLFromClient 同步修复）。
	if strings.HasPrefix(u, "ws://") || strings.HasPrefix(u, "wss://") {
		return u + "/ws/agent"
	}
	if u != "" {
		u = "ws://" + u
	}
	return u + "/ws/agent"
}

// httpBaseFromWS ws(s)://host:port/ws/agent → http(s)://host:port（上传图片的
// HTTP 根地址；与 wsURLFromHTTP 互逆。无 scheme 前缀时默认 http://）。
func httpBaseFromWS(wsURL string) string {
	u := strings.TrimSuffix(strings.TrimSuffix(wsURL, "/ws/agent"), "/")
	switch {
	case strings.HasPrefix(u, "wss://"):
		return "https://" + u[len("wss://"):]
	case strings.HasPrefix(u, "ws://"):
		return "http://" + u[len("ws://"):]
	}
	if u != "" && !strings.Contains(u, "://") {
		return "http://" + u
	}
	return u
}

// ---------- 远程任务执行（内核复用，对齐 runLarkTask） ----------

// remoteRunTask 远程任务入口（派发/续聊共用）：运行中入队，空闲新轮次。
// mode 非空时先切换执行模式（PC 端唯一执行面；远端指定 → 本地同步）。
// taskID 非空（server.task_dispatch 新任务）时登记 remoteTaskOf，事件/终态
// 回传需引用；续聊（server.chat）不携带新任务 ID，保留既有映射。
// 返回是否受理。
func (a *App) remoteRunTask(sessionID, task string, images []string, mode, taskID string) bool {
	if sessionID == "" || task == "" {
		return false
	}
	if taskID != "" {
		remoteTaskOf.Store(sessionID, taskID)
	}
	if mode != "" && mode != a.GetMode() {
		for _, m := range a.remoteModes() {
			if m.Slug == mode {
				a.SetMode(mode)
				slog.Info("远程指定执行模式", "session", sessionID, "mode", mode)
				break
			}
		}
	}
	// 运行中：消息入队（FIFO），本轮结束后自动继续（对齐渠道语义）。
	if a.IsSessionRunning(sessionID) {
		return a.enqueueSessionMsg(sessionID, task, images)
	}
	go a.runRemoteTaskSafe(sessionID, task, images)
	return true
}

// remoteModes 当前全部可选执行模式（远程注册/远端模式选择器数据源）。
func (a *App) remoteModes() []modes.ModeConfig {
	mgr := modes.NewManager()
	_ = mgr.LoadProject(a.cfgWorkspace())
	return mgr.All()
}

// remoteModesInfo 模式摘要（ModeConfig → remote.ModeInfo）。
func (a *App) remoteModesInfo() []remote.ModeInfo {
	all := a.remoteModes()
	out := make([]remote.ModeInfo, 0, len(all))
	for _, m := range all {
		out = append(out, remote.ModeInfo{Slug: m.Slug, Name: m.Name, Description: m.Description})
	}
	return out
}

// remoteTaskOf 会话 → 任务 ID 映射（远程任务事件回传需引用 taskID）。
// 服务端派发（server.task_dispatch）同时携带 taskID 与 sessionID，但节点侧
// handleDispatch 只向回调透传 sessionID；此映射是事件/终态回传 taskID 的唯一来源：
// 不登记则 SendTaskEvent/SendTaskDone 带空 taskID → 服务端 TaskDone 查无任务
// → 手机端任务永远停在「运行中」且收不到执行结果（历史缺陷，端到端验证发现）。
var remoteTaskOf sync.Map // sessionID → taskID

// remoteCancel 远程取消：停止本地执行（会话维度），并清映射。
// sessionID 为空（旧服务端不下发）时按 taskID 反查兑底。
func (a *App) remoteCancel(sessionID, taskID string) {
	if sessionID == "" {
		remoteTaskOf.Range(func(k, v interface{}) bool {
			if v == taskID {
				sid, _ := k.(string)
				if sid != "" {
					sessionID = sid
				}
				return false // 找到即停
			}
			return true
		})
	}
	if sessionID == "" {
		slog.Warn("远程取消：无法定位会话", "task", taskID)
		return
	}
	slog.Info("远程取消任务", "session", sessionID, "task", taskID)
	a.Stop(sessionID)
}

// runRemoteTaskSafe 任务 goroutine 的 panic 防护包装（对齐 runLarkTaskSafe）。
func (a *App) runRemoteTaskSafe(sessionID, task string, images []string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("远程任务 panic（已拦截，进程保持运行）",
				"session", sessionID, "panic", r)
		}
	}()
	a.runRemoteTask(sessionID, task, images)
}

// remoteSessionMessages 远程历史回放：运行中回运行期事件，否则回退到持久化会话
// （服务重启/进程重开后，移动端与管理端仍能看到完整历史，对齐桌面端 OpenSession）。
func (a *App) remoteSessionMessages(sessionID string) []message.ClineMessage {
	if a.IsSessionRunning(sessionID) {
		return a.GetMessages(sessionID)
	}
	if s, err := a.store.Get(sessionID); err == nil && s != nil {
		return s.Snapshot().Messages
	}
	return nil
}

// runRemoteTask 以远程会话（ChannelID="remote"）执行一条指令；事件双路：
// 桌面端前端（实时旁观）+ 服务端（转发移动端）。
func (a *App) runRemoteTask(sessionID, task string, images []string) {
	client := a.remoteClientSnapshot()
	if client == nil {
		return // 连接器已停止
	}
	taskID, _ := remoteTaskOf.Load(sessionID)
	taskIDStr, _ := taskID.(string)

	// 内核组件惰性构建。
	a.kmu.Lock()
	needBuild := a.modeMgr == nil || a.skillMgr == nil
	a.kmu.Unlock()
	if needBuild {
		if err := a.buildKernel(); err != nil {
			client.SendTaskDone(taskIDStr, sessionID, false, "", err.Error())
			return
		}
	}
	a.kmu.Lock()
	modeMgr, skillMgr := a.modeMgr, a.skillMgr
	a.kmu.Unlock()

	// 会话创建/复用（含恢复摘要，与桌面端一致）。
	// Phase 2-④（doc/09-设计方案 §4.3 远程行）：远程会话创建时接入
	// ResolveAssignment 解析器——存量远程会话若绑定员工/岗位（经
	// SetSessionEmployee 或历史快照），则按四分支解析：固定/降级/岗位池选取；
	// 解析结果固化进会话（EmployeeID + SetPositionSnapshot 岗位快照，§2 固化
	// 语义会话内不重选），降级事件日志留痕（§10）；未绑定 = 存量裸跑语义。
	now := time.Now()
	var sess *session.Session
	var resumeDigest string
	if old, err := a.store.Get(sessionID); err == nil && old != nil {
		sess = old
		sess.SetTitle(truncateTitle(task, 40))
		if snap := sess.Snapshot(); len(snap.Messages) > 0 || len(snap.Todos) > 0 {
			resumeDigest = ResumeDigestFor(snap, a.memory.Load(sessionID), task)
		}
	} else {
		sess = &session.Session{
			ID:        sessionID,
			ChannelID: "remote",
			Title:     truncateTitle(task, 40),
			Mode:      a.GetMode(),
			Workspace: a.cfgWorkspace(),
			CreatedAt: now,
			UpdatedAt: now,
		}
	}
	// 远程绑定解析（doc/09-设计方案 Phase 2-④ / §2 四分支 / §4.3 远程行）：
	// 源 = 会话既有绑定快照（EmployeeID/PositionID），解析结果固化回会话。
	// 解析失败（员工不存在/岗位池空）→ 回传失败并终止本轮（不静默裸跑）。
	bindEmpID := strings.TrimSpace(sess.Snapshot().EmployeeID)
	bindPosID := strings.TrimSpace(sess.Snapshot().PositionID)
	if bindEmpID != "" || bindPosID != "" {
		r, _, rErr := employee.ResolveAssignment(a.empStore.List(), employee.Assignment{
			EmployeeID: bindEmpID,
			PositionID: bindPosID,
			AssignRule: strings.TrimSpace(sess.Snapshot().AssignRule), // 快照规则随会话（降级/岗位池重选时沿用，与渠道/cron 同语义）
			Load:       a.employeeLoad,                                // least_loaded 运行计数（与渠道侧同源）
		})
		if rErr != nil {
			errMsg := "远程任务员工解析失败: " + rErr.Error()
			slog.Warn("runRemoteTask 绑定解析失败", "session", sessionID, "err", rErr)
			sess.SetError(true)
			a.store.Save(sess)
			client.SendTaskDone(taskIDStr, sessionID, false, "", errMsg)
			return
		}
		// 解析成功：员工回写会话（含降级换人）+ 岗位快照固化（§2 固化语义：
		// 会话生命周期内不重选）。降级事件留痕（§10）。
		if r.EmployeeID != "" && r.EmployeeID != sess.Snapshot().EmployeeID {
			slog.Info("远程任务员工分配（降级/岗位池）", "session", sessionID,
				"from", sess.Snapshot().EmployeeID, "to", r.EmployeeID, "degraded", r.Degraded)
		}
		sess.SetEmployeeID(r.EmployeeID)
		sess.SetPositionSnapshot(r.Employee.PositionID, strings.TrimSpace(sess.Snapshot().AssignRule))
	} else {
		// 裸跑分支（§2-④ 存量语义）：bindEmpID/bindPosID 全空时清残留岗位快照，
		// 保持普通远程会话行为（不强制迁移，§10 兼容）。
		sess.SetPositionSnapshot("", "")
	}
	if sess.Snapshot().Workspace == "" {
		sess.SetWorkspace(a.cfgWorkspace())
	}
	// 错误/完成态清除（与桌面端 runTask 一致）：新任务开始重置上一轮标记。
	sess.SetError(false)
	sess.SetDone(false)
	a.store.Save(sess)
	uiEmit(a.ctx, "kaelvoris:sessions", a.store.List())

	run := &taskRun{id: sessionID, workspace: sess.Snapshot().Workspace, employeeID: sess.Snapshot().EmployeeID, todo: tools.NewTodoStore(), askCh: make(chan askReply, 1)}
	run.mcpProbe = task // MCP 惰性加载：远程任务文本作为激活信号
	if snap := sess.Snapshot(); len(snap.Todos) > 0 {
		run.todo.Set(snap.Todos)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	defer func() {
		if run.cleanup != nil {
			run.cleanup()
		}
		cancel()
		a.runsMu.Lock()
		delete(a.runs, sessionID)
		a.runsMu.Unlock()
		a.registry.Remove(sessionID)
		uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": false, "sessionId": sessionID})
	}()
	a.runsMu.Lock()
	a.runs[sessionID] = run
	a.runsMu.Unlock()
	a.registry.Push(run)
	uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": true, "sessionId": sessionID})

	client.SendTaskEvent(taskIDStr, sessionID, "started", task, false)

	// ACP 会话分支：会话绑定外部 agent（Claude Code / Gemini CLI 等）→
	// 任务经 ACP 派发，事件双路复用（桌面端旁观 + 服务端转发移动端）。
	if agt := a.acpAgentOf(sessionID); agt != nil {
		say := func(m message.ClineMessage) {
			desktopSay := a.makeSayBridge(sess, run, sessionID)
			desktopSay(m)
			a.pushRemoteEvent(client, taskIDStr, sessionID, m)
		}
		result, acpErr := a.acpTurn(ctx, run, say, *agt, task)
		done := map[string]any{"result": result, "sessionId": sessionID}
		if acpErr != nil {
			done["error"] = acpErr.Error()
			sess.SetError(true) // 错误态落置（列表卡红点）
		} else {
			sess.SetDone(true) // 成功完成态落置（列表卡绿标题）
		}
		client.SendTaskEvent(taskIDStr, sessionID, "assistant", result, false)
		if acpErr != nil {
			client.SendTaskDone(taskIDStr, sessionID, false, "", acpErr.Error())
		} else {
			client.SendTaskDone(taskIDStr, sessionID, true, result, "")
		}
		a.store.FlushAll()
		// 轮末收尾对齐主路径：取消/队列语义一致。
		if run.IsAborted() || ctx.Err() != nil {
			return
		}
		return // 远程 ACP 单轮即止（续聊经新任务派发）
	}

	// 轮次循环（运行中不停止，对齐渠道与桌面端 runTask）。
	curText := task
	for {
		engine, mcpClients := a.buildRunEngine(ctx, modeMgr, skillMgr, run, images)
		flushMCP := func() {
			for _, c := range mcpClients {
				if c != nil {
					c.Close()
				}
			}
		}
		engine.ResumeDigest = resumeDigest
		engine.MemoryDoc = a.memory.Load(sessionID)
		engine.MemoryPath = a.memory.Path(sessionID)
		engine.EvolveSnippet = a.evolveStore.ContextSnippet(curText)

		// say 双路桥：桌面端事件桥 + 远程任务事件回传。
		desktopSay := a.makeSayBridge(sess, run, sessionID)
		say := func(m message.ClineMessage) {
			desktopSay(m)
			a.pushRemoteEvent(client, taskIDStr, sessionID, m)
		}

		result, runErr := engine.RunInMode(ctx, a.GetMode(), curText, false, 0, say, a.makeRemoteAskBridge(client, run, ctx, taskIDStr, sessionID))
		flushMCP()
		if runErr != nil {
			client.SendTaskDone(taskIDStr, sessionID, false, "", runErr.Error())
			sess.SetError(true) // 错误态落置（与桌面端 runTask 一致；列表卡红点）
		} else {
			client.SendTaskDone(taskIDStr, sessionID, true, result, "")
			sess.SetDone(true) // 成功完成态落置（列表卡绿标题；中途已恢复报错一并覆盖）
		}
		a.store.FlushAll()

		if run.IsAborted() || ctx.Err() != nil {
			return
		}
		next, ok := a.dequeueSessionMsg(sessionID)
		if !ok {
			return
		}
		if next.task == "" && len(next.images) == 0 {
			continue
		}
		curText = next.task
		images = next.images
		resumeDigest = ResumeDigestFor(sess.Snapshot(), a.memory.Load(sessionID), curText)
		uiEmit(a.ctx, "kaelvoris:queued-continue", map[string]any{
			"sessionId": sessionID, "task": curText,
		})
	}
}

// pushRemoteEvent 定型 say 事件转远程任务事件（partial 节流后照推，供移动端流式）。
// 绘画链路（SayImage）：generate_image 产出的图片先上传服务端 /api/images 换取
// /img/<file> URL，再以 task.event(kind=image) 广播（服务端同步回填 Task.Images，
// 移动端聊天流/绘画 tab 直接展示）。上传失败不阻塞事件流（仅日志，移动端从
// 会话历史 get.messages 补看路径提示）。
func (a *App) pushRemoteEvent(client *remote.Client, taskID, sessionID string, m message.ClineMessage) {
	switch {
	case m.Partial && m.Say != message.SayText && m.Say != message.SayReasoning:
		return // 非文本流式帧不推（tool partial 等噪音）
	case m.Say == message.SayAPIReqStarted || m.Say == message.SayAPIReqFinished || m.Say == message.SayAIContext:
		return
	case m.Say == message.SayImage:
		a.pushRemoteImages(client, taskID, sessionID, m)
		return
	case m.Say == message.SayVideo || m.Say == message.SayAudio:
		a.pushRemoteMedia(client, taskID, sessionID, m)
		return
	}
	kind := "text"
	switch {
	case m.Type == message.TypeAsk:
		kind = "ask" // ask 推送由 ask 桥处理（此处防重复）
		return
	case m.Say == message.SayTool:
		kind = "tool"
		// 工具事件带摘要（工具名 + 路径/命令）。
		var info message.ToolCallInfo
		if json.Unmarshal([]byte(m.Text), &info) == nil {
			label := info.Tool
			if info.Path != "" {
				label += ": " + info.Path
			} else if info.Cmd != "" {
				label += ": " + truncateTitle(info.Cmd, 120)
			}
			client.SendTaskEvent(taskID, sessionID, kind, label, m.Partial)
			return
		}
	case m.Say == message.SayError:
		kind = "error"
	case m.Say == message.SayTurnDiff:
		// 任务完成文件变更摘要：kind=turn_diff 原样透传（移动端折叠卡片渲染）。
		kind = "turn_diff"
	case m.Say == message.SayProgress:
		// 长任务心跳（R2 内核化）：kind=progress 透传，移动端/管理端渲染进度条。
		kind = "progress"
	case m.Say == message.SayFallback:
		// 降级续跑通知（备用模型切换 / 第三级兑底降级指引）：透传给移动端展示。
		kind = "fallback"
	default:
		if m.Say == message.SayText || m.Say == message.SayReasoning {
			kind = "text"
		} else {
			return // 其余类型（reasoning/api_req 等内部帧）不推
		}
	}
	if t := trimRemoteText(m.Text); t != "" {
		client.SendTaskEvent(taskID, sessionID, kind, t, m.Partial)
	}
}

// remoteImagePayload SayImage 消息 Images（data URL）转上传载荷：data URL 解码为字节。
func remoteImageDataURLs(m message.ClineMessage) []string {
	if len(m.Images) == 0 {
		return nil
	}
	out := make([]string, 0, len(m.Images))
	for _, d := range m.Images {
		if strings.HasPrefix(d, "data:") {
			out = append(out, d)
		}
	}
	return out
}

// pushRemoteImages 生成图片事件处理：逐图上传服务端（磁盘路径优先，data URL 兑底），
// 全部成功后发一条 kind=image 事件（载荷含全部 URL）。上传失败的图跳过
// （服务端无文件，移动端不展示），不中断剩余图片上传。
func (a *App) pushRemoteImages(client *remote.Client, taskID, sessionID string, m message.ClineMessage) {
	urls := make([]string, 0, len(m.Images))
	// text 载荷携带磁盘路径列表（MarshalImageMsgText；与 m.Images data URL 顺序一一对应）。
	var paths []struct {
		Path     string `json:"path"`
		FileName string `json:"fileName,omitempty"`
	}
	_ = json.Unmarshal([]byte(m.Text), &paths)
	for i, dataURL := range m.Images {
		var (
			data []byte
			name string
		)
		// 优先磁盘路径重读（比解码 base64 data URL 更轻）。
		if i < len(paths) && paths[i].Path != "" {
			if raw, err := os.ReadFile(paths[i].Path); err == nil {
				data = raw
			}
			if fn := strings.TrimSpace(paths[i].FileName); fn != "" {
				name = fn
			} else {
				name = filepath.Base(paths[i].Path)
			}
		}
		if data == nil {
			// 兑底：data URL 解码（path 不可读/未携带时）。
			if j := strings.Index(dataURL, ","); j > 0 {
				if raw, err := base64.StdEncoding.DecodeString(dataURL[j+1:]); err == nil {
					data = raw
					name = "generated-" + fmt.Sprintf("%d", i+1) + ".png"
				}
			}
		}
		if len(data) == 0 {
			slog.Warn("生成图片跳过上传（无可用数据源）", "session", sessionID, "idx", i)
			continue
		}
		u, err := client.UploadImageBytes(data, name, taskID, sessionID)
		if err != nil {
			slog.Warn("生成图片上传服务端失败（移动端将看不到本图）", "session", sessionID, "err", err)
			continue
		}
		urls = append(urls, u)
	}
	if len(urls) > 0 {
		payload, _ := json.Marshal(remote.ImageEventPayload{Images: urls})
		client.SendTaskImageEvent(taskID, sessionID, string(payload), urls)
	}
}

// pushRemoteMedia 生成视频/音频事件处理：磁盘文件上传服务端 /api/media 换取
// /img/<file> URL，再发一条 kind=video/audio 事件（载荷含 URL + 文件名 + MIME；
// 服务端同步回填 Task.Images，移动端按扩展名分发 <video>/<audio> 渲染）。
// 上传失败不阻塞事件流（仅日志，移动端从会话历史补看文件名卡）。
func (a *App) pushRemoteMedia(client *remote.Client, taskID, sessionID string, m message.ClineMessage) {
	// text 载荷携带 {kind,path,fileName,mime} 列表（MarshalMediaMsgText）。
	var paths []struct {
		Kind     string `json:"kind,omitempty"`
		Path     string `json:"path"`
		FileName string `json:"fileName,omitempty"`
		MIME     string `json:"mime,omitempty"`
	}
	_ = json.Unmarshal([]byte(m.Text), &paths)
	if len(paths) == 0 {
		return
	}
	kind := "video"
	if paths[0].Kind == "audio" {
		kind = "audio"
	}
	for _, p := range paths {
		if p.Path == "" {
			continue
		}
		name := p.FileName
		if name == "" {
			name = filepath.Base(p.Path)
		}
		u, err := client.UploadMedia(p.Path, taskID, sessionID)
		if err != nil {
			slog.Warn("生成媒体上传服务端失败（移动端将只看到文件名卡）", "session", sessionID, "file", name, "err", err)
			// 上传失败仍推文件名卡（移动端展示占位，历史回放不丢提示）。
			payload, _ := json.Marshal(map[string]string{"kind": kind, "fileName": name, "mime": p.MIME})
			client.SendTaskEvent(taskID, sessionID, kind, string(payload), false)
			continue
		}
		payload, _ := json.Marshal(map[string]string{"kind": kind, "fileName": name, "mime": p.MIME, "url": u})
		client.SendTaskEvent(taskID, sessionID, kind, string(payload), false)
	}
}

// makeRemoteAskBridge 远程会话 ask 桥：completion_result 自动通过；其余推服务端等审批。
func (a *App) makeRemoteAskBridge(client *remote.Client, run *taskRun, ctx context.Context, taskID, sessionID string) func(message.ClineMessage) (message.AskResponse, string) {
	return func(m message.ClineMessage) (message.AskResponse, string) {
		// 任务完成确认：自动通过（结果回执由 runRemoteTask 发送）。
		if m.Ask == message.AskCompletionResult {
			return message.ResponseYes, ""
		}
		// 清理残留应答（防上一轮超时/取消串扰）。
		select {
		case <-run.askCh:
		default:
		}
		if !m.Partial {
			client.SendTaskEvent(taskID, sessionID, "ask", firstLine(m.Text), false)
		}
		select {
		case r := <-run.askCh:
			return r.response, r.text
		case <-ctx.Done():
			return message.ResponseNo, ""
		}
	}
}

// remoteBackfillOnOnline 节点上线补传：本地全部会话的快照推送到服务端镜像
// （agent.messages 覆盖式落盘 + Broadcast）。覆盖场景：
//   - 断线期间手机发布并执行完的任务：历史从未回传，镜像空白；
//   - 服务端重启/换机：镜像丢失后重新填充；
//   - 正在运行的会话：推送当前快照，手机端立即能看到已产生的消息。
//
// 会话按 UpdatedAt 倒序、逐个发送（服务端逐条落盘广播；两约 10s 写超时）。
func (a *App) remoteBackfillOnOnline() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("上线会话补传 panic（已拦截）", "panic", r)
		}
	}()
	sessions := a.store.List()
	if len(sessions) == 0 {
		return
	}
	count := 0
	for _, sum := range sessions {
		s, err := a.store.Get(sum.ID)
		if err != nil || s == nil {
			continue
		}
		snap := s.Snapshot()
		if len(snap.Messages) == 0 {
			continue
		}
		msgs := make([]interface{}, len(snap.Messages))
		for i, m := range snap.Messages {
			msgs[i] = m
		}
		resp := remote.MessagesResponse{SessionID: sum.ID, Messages: msgs}
		if snapTodos := snap.Todos; len(snapTodos) > 0 {
			resp.Todos = make([]interface{}, len(snapTodos))
			for i, td := range snapTodos {
				resp.Todos[i] = td
			}
		}
		client := a.remoteClientSnapshot()
		if client == nil {
			return // 连接器已停止
		}
		client.Send(remote.TypeAgentMessages, resp)
		count++
	}
	if count > 0 {
		slog.Info("节点上线：本地会话历史已补传服务端镜像", "sessions", count)
	}
}

// ---------- 技能库（remote 装配） ----------

// remoteSkillsMeta 技能元数据投影（skills.Metadata → remote.SkillMeta；
// 服务端 skills.list / save/delete 后刷新的数据源）。
func (a *App) remoteSkillsMeta() []remote.SkillMeta {
	all := a.ListSkills()
	out := make([]remote.SkillMeta, 0, len(all))
	for _, m := range all {
		out = append(out, remote.SkillMeta{
			Name:        m.Name,
			Description: m.Description,
			Path:        m.Path,
			Source:      m.Source,
			ModeSlugs:   m.ModeSlugs,
		})
	}
	return out
}

// remoteEmployeeList 数字员工元数据投影（employee.Store.List → remote.EmployeeMeta；
// 服务端 employee.list 拉取的数据源，移动端群聊建群选成员）。
func (a *App) remoteEmployeeList() []remote.EmployeeMeta {
	all := a.empStore.List()
	out := make([]remote.EmployeeMeta, 0, len(all))
	for _, e := range all {
		out = append(out, remote.EmployeeMeta{
			ID:          e.ID,
			Name:        e.Name,
			Role:        e.RoleSection(a.empStore.Dir()),
			Description: e.Description,
			Status:      e.Status,
			Avatar:      e.Avatar,
		})
	}
	return out
}

// remoteSkillGet 读取技能全文（SkillRaw 同源；server.skills_get → agent.skills_data）。
func (a *App) remoteSkillGet(name string) (string, error) {
	return a.SkillRaw(name)
}

// remoteSkillSave 保存/新建技能（远端下发）：SaveSkill 复用（含事件广播与
// 列表上报；此处事件本地 emit 对远程发起无副作用）。
func (a *App) remoteSkillSave(name, content string) error {
	return a.SaveSkill(name, content)
}

// remoteSkillDelete 删除技能（远端下发；builtin 禁删由 Manager 拒绝）。
func (a *App) remoteSkillDelete(name string) error {
	return a.DeleteSkill(name)
}

// remoteClientSnapshot 获取连接器快照（nil 安全）。
func (a *App) remoteClientSnapshot() *remote.Client {
	a.remoteMu.Lock()
	defer a.remoteMu.Unlock()
	return a.remoteClient
}

// ---------- 自主开发规划板（remote 装配；移动端「AR 自主开发」页数据链路） ----------

// remoteRoadmapData 规划板全量投影（roadmap.Item → remote.RoadmapItem；
// roadmap.list 拉取 / 变更后回传的数据源；时间戳转毫秒对齐移动端 fmtTime）。
// 2026-10 执行轨迹：attempts/blockedReason/runLog 一并投影（移动端 AR 页
// 与管理端展示最近执行日志；Old 客户端未知字段自动忽略，协议向后兼容）。
func (a *App) remoteRoadmapData() remote.RoadmapDataPayload {
	items := a.ListRoadmapItems()
	out := make([]remote.RoadmapItem, 0, len(items))
	for _, it := range items {
		ri := remote.RoadmapItem{
			ID: it.ID, Title: it.Title, Detail: it.Detail,
			Priority: it.Priority, Category: it.Category, Status: it.Status,
			Source: it.Source, Result: it.Result, Error: it.Error, Review: it.Review,
			ApprovedBy: it.ApprovedBy,
			Attempts:   it.Attempts, BlockedReason: it.BlockedReason,
			CreatedAt: timeToMillis(it.CreatedAt),
			UpdatedAt: timeToMillis(it.UpdatedAt),
		}
		for i := range it.RunLog {
			ri.RunLog = append(ri.RunLog, remote.RoadmapRunEntry{
				Outcome:    it.RunLog[i].Outcome,
				Summary:    it.RunLog[i].Summary,
				Files:      it.RunLog[i].Files,
				DurationMS: it.RunLog[i].DurationMS,
				FinishedAt: timeToMillis(it.RunLog[i].FinishedAt),
			})
		}
		out = append(out, ri)
	}
	c := a.roadmapStore.Count()
	auto := a.GetAutoDevConfig()
	return remote.RoadmapDataPayload{
		Items: out,
		Stats: remote.RoadmapStats{
			Total: c["total"], Proposed: c[roadmap.StatusProposed],
			Approved: c[roadmap.StatusApproved], Developing: c[roadmap.StatusDeveloping],
			Done: c[roadmap.StatusDone], Failed: c[roadmap.StatusFailed],
			Blocked: c[roadmap.StatusBlocked], Rejected: c[roadmap.StatusRejected],
		},
		Auto: remote.RoadmapAutoCfg{Enabled: auto.Enabled, DevMode: auto.DevMode, Parallel: auto.Parallel,
			MinDeveloping: auto.MinDeveloping, AutoReview: auto.AutoReview, RejectDelete: auto.RejectDelete,
			MaxPlanItems: auto.MaxPlanItems},
	}
}

// timeToMillis 时间转毫秒（零值返回 0；移动端时间展示用）。
func timeToMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// remoteRoadmapPlan 远端触发 AI 规划（PlanRoadmapWithGuidance 复用；异步 LLM 调用）。
// 与本地面板路径的差异：失败必须回报 server.error（客户端移动端在等规划收敛，
// 吞错会导致按钮干等到 10 分钟兑底才复位 —— 2026-10 移动端反馈修复）。
// 成功路径由 roadmapPlanOnce → pushRoadmap → remoteSendRoadmap 推送规划板。
func (a *App) remoteRoadmapPlan(guidance string) error {
	if strings.TrimSpace(a.GetConfig().APIKey) == "" {
		return fmt.Errorf("请先在 PC 端设置中填写 API Key")
	}
	go func() {
		if err := a.roadmapPlanOnce("remote", a.autoDevMaxPlanItems(), guidance); err != nil {
			slog.Warn("远端触发 roadmap 规划失败", "err", err)
			if c := a.remoteClientSnapshot(); c != nil && c.Online() {
				c.Send(remote.TypeServerError, remote.ErrorPayload{Message: "AI 规划失败: " + trimRemoteText(err.Error())})
			}
			return
		}
		// 自动审核开启：规划产出直接交 AI 审核（与本地 PlanRoadmap 同语义）。
		if a.autoDevCfg().AutoReview {
			a.roadmapAutoAIReview()
		}
	}()
	return nil
}

// remoteRoadmapDev 远端执行自主开发（StartRoadmapDev 复用：批准勾选项后启动
// 开发循环；IDs 已由服务端与移动端双重校验非空 —— 执行前必须选择）。
func (a *App) remoteRoadmapDev(ids []string, mode string, parallel int) error {
	return a.StartRoadmapDev(ids, mode, parallel)
}

// remoteRoadmapDelete 远端删除规划条目（DeleteRoadmapItem 复用）。
func (a *App) remoteRoadmapDelete(id string) error {
	return a.DeleteRoadmapItem(id)
}

// remoteRoadmapDeleteBatch 远端批量删除规划条目（已完成/失败清理）：
// 开发中条目先停止会话再删（与单删同语义）；返回错误由 server.error 回报。
func (a *App) remoteRoadmapDeleteBatch(ids []string) error {
	return a.DeleteRoadmapItems(ids)
}

// remoteRoadmapRetry 远端一键重跑失败/暂停条目（RetryRoadmapItem 复用：
// failed/blocked → approved 回队即拉起开发调度）。
func (a *App) remoteRoadmapRetry(id string) error {
	return a.RetryRoadmapItem(id)
}

// remoteRoadmapApprove 远端批量审核通过（ApproveRoadmapItems 复用：
// proposed → approved 入排期，不立即开发）。
func (a *App) remoteRoadmapApprove(ids []string) error {
	return a.ApproveRoadmapItems(ids)
}

// remoteRoadmapAuto 远端开启完全自主开发：提示词（可空）+ 并发线程数 +
// 最低并行开发数落盘 autoDev 配置并开启自动循环（自动规划 → AI 审核 →
// 并行开发；开发中不足最低并行数时持续规划补货）。
func (a *App) remoteRoadmapAuto(guidance string, parallel int, minDeveloping int) error {
	if strings.TrimSpace(a.GetConfig().APIKey) == "" {
		return fmt.Errorf("请先在 PC 端设置中填写 API Key")
	}
	cfg := a.GetConfig()
	dev := AutoDevConfig{}
	if cfg.AutoDev != nil {
		dev = *cfg.AutoDev
	}
	dev.Enabled = true
	dev.DevMode = roadmapDevParallel
	if parallel > 0 {
		if parallel > autoDevMaxParallel {
			parallel = autoDevMaxParallel
		}
		dev.Parallel = parallel
	}
	if minDeveloping > 0 {
		if minDeveloping > autoDevMaxParallel {
			minDeveloping = autoDevMaxParallel
		}
		dev.MinDeveloping = minDeveloping
	}
	c := a.GetConfig()
	c.AutoDev = &dev
	if err := a.SaveConfig(c); err != nil {
		return err
	}
	a.roadmapAutoDevStart()
	// guidance 提示词非空：立即按用户意图触发一轮自主规划（直接链路：
	// remote.roadmap_auto → 规划 → AI 审核 → 开发，不等 2h 心跳；
	// 规划任务数同源取配置 maxPlanItems）。
	if g := strings.TrimSpace(guidance); g != "" {
		go func() {
			if err := a.roadmapPlanOnce("autodev", a.autoDevMaxPlanItems(), g); err != nil {
				slog.Warn("完全自主规划失败", "err", err)
			} else {
				a.roadmapAutoAIReview()
			}
		}()
	} else {
		// 无提示词：唤醒自动循环立即评估（存量提案 AI 审核 + 队列消费）。
		a.wakeRoadmapAutoDev()
	}
	a.pushRoadmap()
	return nil
}

// remoteSendRoadmap 主动上报规划板（nil 安全；上线兑底 + 变更后推送共用）。
func (a *App) remoteSendRoadmap() {
	client := a.remoteClientSnapshot()
	if client != nil && client.Online() {
		client.SendRoadmapData()
	}
}

// trimRemoteText 远程事件文本清洗（限长 2000 rune）。
func trimRemoteText(s string) string {
	r := []rune(s)
	if len(r) > 2000 {
		return string(r[:2000]) + "…"
	}
	return s
}

// ---------- 远程会话删除联动 / AI 会话分析（P-Remote 新增能力） ----------

// remoteDeleteSession 删除节点本地会话存档（服务端 delete.session/delete.task 联动路由）：
//   - 运行中会话先停止执行（删除语义 = 彻底终止，避免删除后任务仍在跑）；
//   - 消息队列中该会话的未发送续聊一并丢弃（防删除后残句复活）；
//   - Store.Delete 清内存缓存 + 磁盘 JSON + 记忆文档（与桌面端 DeleteSession 同一路径）。
//
// 失败仅日志（服务端侧记录已删，此处残留不影响远端）。
func (a *App) remoteDeleteSession(sessionID string) {
	if sessionID == "" {
		return
	}
	slog.Info("远程删除节点本地会话存档", "session", sessionID)
	a.Stop(sessionID)
	a.dropSessionQueue(sessionID)
	remoteTaskOf.Delete(sessionID)
	if err := a.store.Delete(sessionID); err != nil {
		slog.Warn("节点本地会话存档删除失败", "session", sessionID, "err", err)
		return
	}
	uiEmit(a.ctx, "kaelvoris:sessions", a.store.List())
}

// remoteVerifyAll 任务状态核实（server.verify_tasks 路由到达节点侧）：
// 以节点本地事实为权威源逐任务判定真实状态（口径）：
//   - 会话运行中（内存 run 活跃）→ running；
//   - 会话历史末尾有定型 completion_result（AskCompletionResult）→ done；
//   - 会话历史末尾定型 error（SayError）→ failed；
//   - 本地无会话档案 → 不回该条（节点无事实，不路盲改）。
//
// verifyAll 哨兵（TaskIDs 为空时服务端下发）：核实 remoteTaskOf 中全部
// 会话 —— 覆盖服务端被 watchdog「自动收敛」误标 failed 的任务（节点本地
// 会话档案实际已完成 → 回报校正回 done）。
//
// 结果 SendVerifyReport 回传（服务端 ApplyVerifyReport 校正 + 广播），异步执行。
func (a *App) remoteVerifyAll(taskIDs []string, requestedBy string, verifyAll bool) {
	client := a.remoteClientSnapshot()
	if client == nil {
		return
	}
	rep := remote.VerifyReport{Items: []remote.VerifyTaskState{}}
	if verifyAll && len(taskIDs) == 0 {
		// 全量核实：服务端活动任务清单为空，用节点本地映射反查全部会话事实。
		sessionIDs := make([]string, 0, 32)
		remoteTaskOf.Range(func(k, v interface{}) bool {
			if sid, ok := k.(string); ok {
				sessionIDs = append(sessionIDs, sid)
			}
			return true
		})
		for _, sid := range sessionIDs {
			taskID, _ := remoteTaskOf.Load(sid)
			tid, _ := taskID.(string)
			if tid == "" {
				continue
			}
			a.verifyOne(&rep, sid, tid)
		}
		client.SendVerifyReport(rep, requestedBy)
		slog.Info("任务状态核实完成（全量）", "count", len(rep.Items))
		return
	}
	for _, taskID := range taskIDs {
		if taskID == "" {
			continue
		}
		// taskID → sessionID：遍历 remoteTaskOf 映射（核实请求不含 sessionID）。
		sessionID := ""
		remoteTaskOf.Range(func(k, v interface{}) bool {
			if v == taskID {
				sessionID, _ = k.(string)
				return false
			}
			return true
		})
		if sessionID == "" {
			continue // 本节点不持有该任务的会话（多节点场景），跳过
		}
		a.verifyOne(&rep, sessionID, taskID)
	}
	// 无论有无可回报事实都回传报告：服务端 ApplyVerifyReport 校正后统一
	// 广播 verify.done 收尾（空报告 = checked 0，提示「无待核实任务」）。
	// 修复：此前空报告直接 return 不回传，空核实请求永不收尾，
	// 移动端核实小按钮 busy 态只能等 15s 超时复位。
	if len(rep.Items) == 0 {
		slog.Info("任务状态核实完成（无可核实任务，空回报）")
	} else {
		slog.Info("任务状态核实完成", "count", len(rep.Items))
	}
	client.SendVerifyReport(rep, requestedBy)
}

// verifyOne 单会话事实核实（remoteVerifyAll 逐会话复用）：本地有事实
// （运行中/历史定型帧）才追加到报告；无档案不回报（节点无事实，不盲改）。
func (a *App) verifyOne(rep *remote.VerifyReport, sessionID, taskID string) {
	st, result, errMsg := a.remoteSessionFacts(sessionID)
	if st == "" {
		return // 无本地会话事实（档案不存在且未运行）
	}
	rep.Items = append(rep.Items, remote.VerifyTaskState{
		TaskID: taskID, Status: st, Result: result, Error: errMsg,
	})
}

// remoteSessionFacts 按节点本地事实判定会话对应任务的真实状态：
// 运行中 → running；历史末尾定型 completion_result → done；定型 error → failed。
// 返回状态与结果/错误摘要（截断；无事实返回空状态）。
func (a *App) remoteSessionFacts(sessionID string) (status, result, errMsg string) {
	// 运行中：内存运行态为最高优先事实（无论历史末尾是什么）。
	if a.IsSessionRunning(sessionID) {
		return remote.TaskStatusRunning, "", ""
	}
	var msgs []message.ClineMessage
	if s, err := a.store.Get(sessionID); err == nil && s != nil {
		msgs = s.Snapshot().Messages
	}
	if len(msgs) == 0 {
		return "", "", "" // 无本地会话档案：无事实可报
	}
	// 自后向前找第一条定型结论帧（忽略 partial 流式帧）。
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Partial {
			continue
		}
		switch {
		case m.Type == message.TypeAsk && m.Ask == message.AskCompletionResult:
			return remote.TaskStatusDone, truncateTitle(m.Text, 200), ""
		case m.Say == message.SayError:
			return remote.TaskStatusFailed, "", truncateTitle(m.Text, 200)
		}
	}
	// 历史存在但无终态标志：保守回报 running（会话存在且未走到终态，
	// 可能是上轮中断后的中间态，等待用户续聊；不误标 failed）。
	return remote.TaskStatusRunning, "", ""
}

// remoteAnalyzeSession 会话 AI 分析（analyze.session）：拉取整个会话历史（PC 端
// 权威源，含运行期与持久化两层）+ 待办清单 + 记忆文档，组装分析提示词调 LLM，
// 生成「用户问题理解 → 执行过程 → 结果评价 → 后续建议」四段式报告。
// 结果 SendAnalyzeDone 回传（服务端广播 → 移动端渲染），同时落本地会话历史
// （下次拉历史仍可见）。
func (a *App) remoteAnalyzeSession(sessionID string) {
	client := a.remoteClientSnapshot()
	if client == nil {
		return // 连接器已停止，无法回传
	}
	taskID, _ := remoteTaskOf.Load(sessionID)
	taskIDStr, _ := taskID.(string)

	// 会话历史：运行中取运行期事件，否则取持久化快照（与 remoteSessionMessages 同源语义，
	// 保证服务重启/节点重开后分析仍基于完整历史）。
	var msgs []message.ClineMessage
	if a.IsSessionRunning(sessionID) {
		msgs = a.GetMessages(sessionID)
	} else if s, err := a.store.Get(sessionID); err == nil && s != nil {
		msgs = s.Snapshot().Messages
	}
	if len(msgs) == 0 {
		client.SendAnalyzeDone(remote.AnalyzeDone{SessionID: sessionID, TaskID: taskIDStr, OK: false, Error: "会话暂无历史内容，无法分析"})
		return
	}

	// 组装分析材料（复用恢复摘要的消息摘要逻辑，扩量到全部定型消息）。
	var b strings.Builder
	b.WriteString("[会话背景]\n")
	if s, err := a.store.Get(sessionID); err == nil && s != nil {
		snap := s.Snapshot()
		if snap.Title != "" {
			b.WriteString("会话标题: " + snap.Title + "\n")
		}
		if snap.Mode != "" {
			b.WriteString("执行模式: " + snap.Mode + "\n")
		}
		if len(snap.Todos) > 0 {
			b.WriteString("[任务清单]\n")
			for _, td := range snap.Todos {
				mark := "[ ]"
				switch td.Status {
				case "completed":
					mark = "[x]"
				case "in_progress":
					mark = "[-]"
				}
				b.WriteString(mark + " " + td.Content + "\n")
			}
		}
	}
	if mem := a.memory.Load(sessionID); mem != "" {
		b.WriteString("[会话记忆文档]\n" + mem + "\n")
	}
	b.WriteString("[完整对话记录（旧→新）]\n")
	for _, m := range msgs {
		if m.Partial || m.Say == message.SayAPIReqStarted || m.Say == message.SayAPIReqFinished ||
			m.Say == message.SayAIContext || m.Say == message.SayReasoning {
			continue // 流式中间帧/内部帧不进分析材料
		}
		kind, text := describeMessage(m)
		if text == "" {
			continue
		}
		b.WriteString("- " + kind + ": " + text + "\n")
	}

	prompt := "你是 AI Agent 执行过程的复盘分析助手。请基于以下远程会话的完整对话记录，" +
		"面向任务发布者生成一份简明扼要的分析报告（用简体中文，Markdown 格式，800 字以内）。\n\n" +
		"报告必须包含以下四个部分（用 ## 标题）：\n" +
		"1. ## 用户问题理解 —— 用一两句话概括用户提出的问题/任务目标；\n" +
		"2. ## 执行过程回顾 —— Agent 经历了哪些关键步骤（工具调用/文件修改/命令执行/遇到的错误与纠偏）；\n" +
		"3. ## 结果评价 —— 最终回复是否解决了用户的问题，还存在哪些风险或未覆盖项；\n" +
		"4. ## 后续建议 —— 如果用户想继续推进，建议的下一步（如验证方式/可优化点/注意事项）。\n\n" +
		"要求：基于记录客观陈述，不要臆造记录中不存在的内容。\n\n" + b.String()

	cfg := a.GetConfig()
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.APIKey) == "" {
		client.SendAnalyzeDone(remote.AnalyzeDone{SessionID: sessionID, OK: false,
			Error: "节点未配置 LLM（BaseURL/APIKey），无法分析"})
		return
	}
	llmClient := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	resp, err := llmClient.Generate(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "你是 AI Agent 执行过程的复盘分析助手，输出简洁、结构化、可操作的中文分析报告。"},
			{Role: llm.RoleUser, Content: truncateTitle(prompt, 60000)},
		},
	})
	if err != nil {
		slog.Warn("会话 AI 分析失败", "session", sessionID, "err", err)
		client.SendAnalyzeDone(remote.AnalyzeDone{SessionID: sessionID, OK: false, Error: "分析失败: " + err.Error()})
		return
	}
	analysis := strings.TrimSpace(resp.Content)
	if analysis == "" {
		client.SendAnalyzeDone(remote.AnalyzeDone{SessionID: sessionID, OK: false, Error: "模型返回空分析"})
		return
	}
	// 分析报告落节点本地会话历史（say: text 定型帧；移动端重拉历史仍可见），
	// 并同步服务端镜像（下次 get.messages 即含分析结论）。
	if s, err := a.store.Get(sessionID); err == nil && s != nil {
		s.Append(message.NewSay(message.SayText, "[AI 会话分析]\n\n"+analysis))
		a.store.Save(s)
		a.store.FlushAll()
	}
	client.SendAnalyzeDone(remote.AnalyzeDone{SessionID: sessionID, TaskID: taskIDStr, OK: true, Analysis: analysis})
}

// remoteCleanupSessions 批量清理会话（cleanup.sessions / clear.tasks 路由到达节点侧，
// mode 分流）：
//   - archive（空）：逐会话「完成校验 → AI 分析（含可优化冗余步骤）→ 分析文档落盘
//     → 归档删除」，复用 session_cleanup.go 的 cleanupOne；结果 SendCleanupDone 回传。
//   - clear：直接清空本地会话存档（不分析不保留；配合移动端「清空任务与会话」，
//     服务端侧记录已先行清空）。回 CleanupDone{Mode:"clear"} 收尾。
//   - summarize：先汇总未完成任务（运行中/失败/无终态 + 未勾选 todos）为一条
//     「续作任务指令」（LLM），回传 CleanupDone{Mode:"summarize", Summary} ——
//     服务端据此创建新会话任务并派发运行，再统一清空两端记录。
//
// 全程异步（由 client.go 读循环 goroutine 派发），不阻塞连接器。
func (a *App) remoteCleanupSessions(mode string) {
	client := a.remoteClientSnapshot()
	if client == nil {
		return
	}
	switch mode {
	case "clear":
		a.remoteClearAllSessions()
		return
	case "summarize":
		a.remoteSummarizeAndClear()
		return
	}
	all := a.store.List()
	cd := remote.CleanupDone{Total: len(all), Reports: []remote.CleanupSessionReport{}}
	for _, s := range all {
		rep := a.cleanupOne(s.ID)
		rr := remote.CleanupSessionReport{
			SessionID: rep.SessionID,
			Title:     rep.Title,
			OK:        rep.OK,
			Skipped:   rep.Skipped,
			Reason:    rep.Reason,
			DocPath:   rep.DocPath,
			Analysis:  truncateTitle(rep.Analysis, 800),
		}
		cd.Reports = append(cd.Reports, rr)
		if rep.OK {
			cd.Deleted++
		}
	}
	cd.Skipped = cd.Total - cd.Deleted
	client.SendCleanupDone(cd)
	a.pushToast(fmt.Sprintf("🧹 远程清理完成：%d/%d 已分析归档删除", cd.Deleted, cd.Total))
}

// clearLocalSessions 清空节点本地全部会话存档（清空/总结并清空的公共核心）：
// 运行中会话先停止（含未发送队列丢弃 + remoteTaskOf 清理），再逐个
// Store.Delete（内存+磁盘+记忆文档）；无需 LLM，秒级完成。不回帧不广播
// —— 由调用方按模式收尾（本地链路 pushSessionsChanged / 远程 SendCleanupDone）。
// 返回（实际删除数, 会话总数）。
func (a *App) clearLocalSessions() (deleted, total int) {
	all := a.store.List()
	total = len(all)
	for _, s := range all {
		sid := s.ID
		a.Stop(sid)             // 运行中先停（幂等；空闲会话为 no-op）
		a.dropSessionQueue(sid) // 丢弃未发送续聊（防删除后残句复活）
		remoteTaskOf.Delete(sid)
		if err := a.store.Delete(sid); err != nil {
			slog.Warn("清空模式：节点本地会话存档删除失败", "session", sid, "err", err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		uiEmit(a.ctx, "kaelvoris:sessions", a.store.List())
	}
	slog.Info("清空节点本地会话存档完成", "deleted", deleted, "total", total)
	return deleted, total
}

// remoteClearAllSessions 直接清空节点本地全部会话存档（clear.tasks Mode=clear
// 联动；summarize 汇总后复用）：运行中会话先停止（含未发送队列丢弃），再
// Store.Delete（内存+磁盘+记忆文档）；无需 LLM，秒级完成。不回帧 —— 服务端
// 在受理时已直接清空记录并广播（summarize 模式下由收尾链路统一清空）。
// 返回实际删除的会话数（CleanupDone.Deleted 回传数据源）。
func (a *App) remoteClearAllSessions() int {
	n, total := a.clearLocalSessions()
	if total > 0 {
		a.pushToast(fmt.Sprintf("🧹 已清空本地会话 %d/%d", n, total))
	}
	return n
}

// summarizeAnalyzeTimeout 未完成任务汇总 LLM 调用超时（与清理分析同 5min）。
const summarizeAnalyzeTimeout = 5 * time.Minute

// remoteSummarizeAndClear 总结并清理（clear.tasks Mode=summarize 节点侧）：
//  1. 收集未完成事实：运行中会话 + 无终态结论的历史会话 + 未完成 todos；
//  2. LLM 把这些事实汇总为一份可执行的「续作任务指令」（新会话直接可跑）；
//  3. 清空节点本地全部会话存档（含运行中停止）。
//  4. CleanupDone{Mode:"summarize", Summary} 回传 → 服务端创建新会话任务并
//     统一清空两端记录 → 广播 clear.done{newTaskId}。
//
// summarizeClearCore 总结并清空的核心（remote 链路与 PC 本地链路共用）：
//  1. 收集未完成事实（运行中/失败/无终态会话 + 未完成 todos，复用 remoteSessionFacts 口径）；
//  2. LLM 汇总为一条可执行的「续作任务指令」（未配置 LLM/调用失败时无汇总文本，清空照常）；
//  3. 清空节点本地全部会话存档（含运行中停止、队列丢弃）。
//
// 返回（汇总文本, 删除数, 会话总数, 未完成事实数）。不回帧不广播（由调用方按模式收尾）。
func (a *App) summarizeClearCore() (summary string, deleted, total, unfinished int) {
	sessions := a.store.List()
	total = len(sessions)
	// 运行中会话集合（运行态 = 最高优先事实）。
	running := map[string]bool{}
	a.runsMu.Lock()
	for id := range a.runs {
		running[id] = true
	}
	a.runsMu.Unlock()
	var mat strings.Builder
	mat.WriteString("[历史会话与任务清单（旧→新）]\n")
	for _, sum := range sessions {
		sid := sum.ID
		isRunning := running[sid] || a.IsSessionRunning(sid)
		// 判定该会话是否已有终态结论（复用 remoteSessionFacts 口径）。
		st, _, _ := a.remoteSessionFacts(sid)
		unfinishedSession := isRunning || st == remote.TaskStatusFailed || st == "" || st == remote.TaskStatusRunning
		// 未完成 todos：当前会话运行态清单或持久化快照，状态非 completed 视为未完成。
		a.runsMu.Lock()
		run := a.runs[sid]
		a.runsMu.Unlock()
		var todos []tools.TodoItem
		if run != nil && run.todo != nil {
			todos = run.todo.Items()
		}
		snap := session.SessionData{}
		if sess, err := a.store.Get(sid); err == nil && sess != nil {
			snap = sess.Snapshot()
			if len(todos) == 0 {
				todos = snap.Todos
			}
		}
		for _, td := range todos {
			if td.Status != "completed" {
				mat.WriteString("  · 待办[" + td.Status + "] " + td.Content + "\n")
				unfinished++
			}
		}
		if isRunning {
			mat.WriteString("- [运行中] " + truncateTitle(sum.Title, 80) + "：执行被清理操作中断，未产出结果\n")
			unfinished++
		} else if st == remote.TaskStatusFailed {
			mat.WriteString("- [失败] " + sid + "：" + truncateTitle(sum.Title, 80) + "\n")
			unfinished++
		} else if st == "" || st == remote.TaskStatusRunning {
			// 无终态结论（中断后的中间态）也视为未完成。
			mat.WriteString("- [未完成] " + sid + "：" + truncateTitle(sum.Title, 80) + "\n")
			unfinished++
		}
		if !unfinishedSession {
			// 已完成会话：仅附最简上下文（标题），无未完成事项不再铺开对话材料。
			continue
		}
		// 会话最近对话摘要材料（截断，供 LLM 理解上下文；含未完成 todos 标注）。
		mat.WriteString(buildCleanupMaterial(sum.Title, sum.Mode, todos, "", recentMessages(snap.Messages, 12)))
		mat.WriteString("\n")
	}
	deleted, _ = a.clearLocalSessions()
	if unfinished == 0 {
		// 无未完成事实：直接清空本地（无需汇总），回空 Summary 由服务端照常清空。
		return "", deleted, total, 0
	}
	// LLM 汇总（复用 cleanupAnalyze 的客户端装配；超时同 5min）。
	cfg := a.GetConfig()
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.APIKey) == "" {
		slog.Warn("总结并清空：节点未配置 LLM，无法汇总（直接清空）")
		return "", deleted, total, unfinished
	}
	s, err := a.summarizeUnfinished(mat.String())
	if err != nil {
		slog.Warn("总结并清空：汇总失败（直接清空）", "err", err)
		return "", deleted, total, unfinished
	}
	return s, deleted, total, unfinished
}

// remoteSummarizeAndClear 总结并清理（clear.tasks Mode=summarize 节点侧）：
// 复用 summarizeClearCore 完成「收集 → LLM 汇总 → 清空本地」，回传
// CleanupDone{Mode:"summarize", Summary} → 服务端创建新会话任务并统一清空两端记录。
func (a *App) remoteSummarizeAndClear() {
	client := a.remoteClientSnapshot()
	if client == nil {
		return
	}
	summary, deleted, _, unfinished := a.summarizeClearCore()
	cd := remote.CleanupDone{Mode: "summarize", Summary: summary, Deleted: deleted}
	client.SendCleanupDone(cd)
	if unfinished > 0 {
		a.pushToast(fmt.Sprintf("🧹 已汇总 %d 项未完成任务并清空本地会话，续接任务已回传", unfinished))
	} else {
		a.pushToast(fmt.Sprintf("🧹 无未完成任务，本地会话已清空 %d 个", deleted))
	}
}

// recentMessages 取会话末尾 n 条定型消息（跳过 partial 流式帧与高频过程帧，
// 与 buildCleanupMaterial 过滤口径一致；供汇总材料控制体积）。
func recentMessages(msgs []message.ClineMessage, n int) []message.ClineMessage {
	var out []message.ClineMessage
	for _, m := range msgs {
		if m.Partial || m.Say == message.SayAPIReqStarted || m.Say == message.SayAPIReqFinished ||
			m.Say == message.SayAIContext || m.Say == message.SayReasoning {
			continue
		}
		out = append(out, m)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// summarizeUnfinished 调 LLM 把未完成任务/会话事实汇总为一条可执行的续作任务指令。
// 输出格式约束为「目标 → 已完成进展 → 待续步骤」三段（移动端新会话首条任务直接可跑）。
func (a *App) summarizeUnfinished(material string) (string, error) {
	cfg := a.GetConfig()
	llmClient := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	ctx, cancel := context.WithTimeout(context.Background(), summarizeAnalyzeTimeout)
	defer cancel()
	prompt := "以下是多个 AI Agent 会话中未完成的任务与待办清单。请把它们汇总为一条" +
		"可直接派发给 Agent 继续执行的「续作任务指令」（简体中文，600 字以内）。要求：\n" +
		"1. 开头一句话总括剩余工作目标；\n" +
		"2. 按「已完成进展 / 待完成事项」分节列出（仅保留与未完成部分相关的关键上下文）；\n" +
		"3. 待完成事项写成可执行的具体指令（不要照搬内部日志细节）；\n" +
		"4. 不要泛化臆造记录中不存在的需求。\n\n" + material
	resp, err := llmClient.Generate(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "你是任务汇总助手：把多个未完成任务的上下文收敛为一条可直接执行的续作任务指令，输出简洁结构化的中文指令。"},
			{Role: llm.RoleUser, Content: truncateTitle(prompt, 60000)},
		},
	})
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" {
		return "", fmt.Errorf("模型返回空汇总")
	}
	return out, nil
}
