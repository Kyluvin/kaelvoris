package desktop

// 本机局域网出口 IP 探测（IM 群接收回调地址展示用）：
// 设置抽屉「群消息接收模式」需展示可配到 IM 平台的回调 URL
// （http://<IP>:端口/im/group/<hookID>），此前前端展示 <本机IP> 占位符，
// 用户需手工查询本机 IP 后拼接；改为后端 UDP 出口路由探测直接返回真实 IP。
//
// 探测语义（UDP dial 不实际发包，仅让 OS 按默认路由选择源地址）：
//   - dial udp 8.8.8.8:80 → 本机对外出口 IP（多网卡取默认路由网卡，与 IM
//     平台实际可达路径一致，比遍历 net.Interfaces 首个地址更准）；
//   - 失败回退 hostname 解析首个非环回 IPv4；
//   - 全部失败返回空串（前端回退展示 <本机IP> 占位符）。

import "net"

// localOutboundIP 本机局域网出口 IP（IPv4 优先；失败返回空，不 panic）。
func localOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			return addr.IP.String()
		}
	}
	// 回退：hostname 解析（离线/无默认路由时仍可能有局域网地址）
	hosts, err := net.LookupHost(hostnameSafe())
	if err != nil {
		return ""
	}
	for _, h := range hosts {
		ip := net.ParseIP(h)
		if ip != nil && !ip.IsLoopback() && ip.To4() != nil {
			return ip.String()
		}
	}
	return ""
}

// GetLocalIP 前端绑定：本机局域网出口 IP（IM 回调地址拼接用）。
func (a *App) GetLocalIP() string {
	return localOutboundIP()
}
