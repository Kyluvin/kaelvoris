package desktop

// remote_im_relay.go IM 渠道公网中继的节点侧装配（P-Remote 通道复用）：
//
//	公网服务端（kaelvoris-server）代持 IM 平台回调入口（/im/c/<fp>、
//	/im/g/<fp>/<hookID>，见 internal/remote/im_relay.go），经 /ws/agent
//	既有长连接以 server.im_inbound 帧下发到本节点 → 路由到目标 IMChannel
//	实例 → 复用既有内核路径执行（验签/去重/白名单/会话路由/ask 审批）。
//	回复侧由节点出站直连 IM 平台（pubsendV2/Webhook；本地 PC 出站网络可达）。
//
//	渠道定位：服务端回调载荷携带渠道五元组（Name/PublicID/EID/WebhookToken/
//	GroupHookIDs），本节点按五元组指纹（imRelayChannelFP，与服务端
//	im_relay.go imRelayChannelFP 逐字节一致 —— 跨进程契约）匹配本机 IMChannel
//	实例（同节点多渠道互不串扰）。

import (
	"log/slog"
	"strings"

	"kaelvoris/internal/remote"
)

// remoteIMInbound 中继回调入口：按载荷渠道指纹路由到本节点对应 IMChannel。
func (a *App) remoteIMInbound(msg remote.IMRelayInbound) {
	if ch := a.imChannelByFP(imRelayChannelFP(
		msg.Name, msg.PublicID, msg.EID, msg.WebhookToken, msg.GroupHooks,
	)); ch != nil {
		ch.handleRelayed(msg)
		return
	}
	slog.Warn("IM 中继回调未命中本节点渠道（指纹不匹配，渠道配置可能已变更）",
		"name", msg.Name, "publicId", msg.PublicID, "eid", msg.EID,
		"webhookToken", msg.WebhookToken, "groupHookId", msg.GroupHookID, "path", msg.Path)
}

// imChannelByFP 按渠道指纹找本节点启用的 IM 通道实例（chanMu 锁内快照）。
func (a *App) imChannelByFP(fp string) *IMChannel {
	if fp == "" {
		return nil
	}
	a.chanMu.Lock()
	defer a.chanMu.Unlock()
	for _, ch := range a.imChannels {
		if ch.relayFP() == fp {
			return ch
		}
	}
	return nil
}

// imRelayChannels 汇总本节点可中继的 IM 渠道（启用 + 有接收模式凭据）：
// 公共号模式（PublicID+PubSecret+EID）或群消息接收模式（GroupHookIDs）。
// 纯 Webhook 单向推送渠道无接收能力，不参与中继。
func (a *App) imRelayChannels() []remote.IMRelayChannel {
	out := []remote.IMRelayChannel{}
	for _, cfg := range a.ListChannels() {
		if !cfg.Enabled || cfg.Type != "im" {
			continue
		}
		hasPub := cfg.PublicID != "" && cfg.PubSecret != "" && cfg.EID != ""
		hooks := strings.TrimSpace(cfg.GroupHookIDs)
		if !hasPub && hooks == "" {
			continue
		}
		out = append(out, remote.IMRelayChannel{
			Name:         cfg.Name,
			PublicID:     cfg.PublicID,
			EID:          cfg.EID,
			WebhookToken: cfg.WebhookToken,
			GroupHookIDs: cfg.GroupHookIDs,
		})
	}
	return out
}

// remoteSendIMRelayState 上报 IM 中继渠道注册表（agent.im_state）：
// 渠道配置保存/通道启停/节点上线时调用（服务端落路由投影 + im.state 广播）。
func (a *App) remoteSendIMRelayState() {
	client := a.remoteClientSnapshot()
	if client == nil || !client.Online() {
		return
	}
	chs := a.imRelayChannels()
	client.SendIMRelayState(remote.IMRelayState{Enabled: len(chs) > 0, Channels: chs})
}
