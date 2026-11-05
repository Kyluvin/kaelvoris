package desktop

import (
	"net"
	"testing"
)

// TestLocalOutboundIP 出口 IP 探测自检：开发机有网络时应返回合法非环回 IP；
// 无网络（CI 隔离环境）允许空串，但只要返回非空就必须是合法 IP 格式。
func TestLocalOutboundIP(t *testing.T) {
	ip := localOutboundIP()
	if ip == "" {
		t.Skip("无出口 IP（离线环境），跳过")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		t.Fatalf("返回非法 IP: %q", ip)
	}
	if parsed.IsLoopback() {
		t.Fatalf("不应返回环回地址: %q", ip)
	}
	t.Logf("出口 IP: %s", ip)
}
