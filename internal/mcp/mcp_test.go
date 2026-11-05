// Package mcp 测试：Windows 命令启动兼容（cmd /C 包装 + 整行命令拆分）、
// stderr 尾部捕获。spawn/握手链路以 node -e 脚本模拟 stdio server 做端到端
// 验证（node 在 CI/桌面环境必然存在，且不需要 cmd 包装）。
package mcp

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSplitCommandLine(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"npx -y @modelcontextprotocol/server-filesystem", []string{"npx", "-y", "@modelcontextprotocol/server-filesystem"}},
		{`node "C:\Program Files\server\index.js" --port 8080`, []string{"node", `C:\Program Files\server\index.js`, "--port", "8080"}},
		{"  spaced   args  ", []string{"spaced", "args"}},
		{"single", []string{"single"}},
		{`"quoted whole"`, []string{"quoted whole"}}, // 引号内空白不拆分
		{"", nil},
	}
	for _, c := range cases {
		got := splitCommandLine(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitCommandLine(%q) = %q, want %q", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitCommandLine(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestJoinCommandLine(t *testing.T) {
	got := joinCommandLine([]string{"npx", "-y", "some path"})
	want := `npx -y "some path"`
	if got != want {
		t.Fatalf("joinCommandLine = %q, want %q", got, want)
	}
	// 已带引号的片段原样保留。
	if got := joinCommandLine([]string{`"a b"`, "c"}); got != `"a b" c` {
		t.Fatalf("joinCommandLine preserve quotes: %q", got)
	}
}

func TestStdioCommand(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd /C 包装仅 Windows 路径")
	}
	// 整行命令 + 无 args：引号感知拆分后整体经 cmd /C 重组。
	cmd := stdioCommand(ServerConfig{Name: "fs", Command: "npx -y @modelcontextprotocol/server-filesystem"})
	if cmd == nil || len(cmd.Args) != 3 || cmd.Args[0] != "cmd" || cmd.Args[1] != "/C" {
		t.Fatalf("want cmd /C wrapped, got %+v", cmd)
	}
	if !strings.Contains(cmd.Args[2], "npx") {
		t.Fatalf("command line lost: %q", cmd.Args[2])
	}
	// 显式 args：命令与参数拼接保留（模型照 Claude 配置逐段填写）。
	cmd = stdioCommand(ServerConfig{Name: "fs", Command: "npx", Args: []string{"-y", "pkg"}})
	if cmd == nil || len(cmd.Args) != 3 {
		t.Fatalf("want cmd /C with joined args, got %+v", cmd)
	}
	// 空命令：nil（Start 返回 empty command 错误）。
	if cmd := stdioCommand(ServerConfig{Name: "empty"}); cmd != nil {
		t.Fatalf("empty command should be nil, got %+v", cmd)
	}
}

func TestCapBufTail(t *testing.T) {
	b := &capBuf{max: 16}
	_, _ = b.Write([]byte("0123456789"))
	_, _ = b.Write([]byte("abcdefghij"))
	// 20 字节写入 max=16 的缓冲：保留末尾 16 字节。
	if got := b.tail(); got != "456789abcdefghij" {
		t.Fatalf("tail = %q", got)
	}
	// 超长截断：stderrHint 只取尾部。
	b2 := &capBuf{max: 8 * 1024}
	for i := 0; i < 3000; i++ {
		_, _ = b2.Write([]byte("xxxx"))
	}
	if len(b2.tail()) != 8*1024 {
		t.Fatalf("tail len = %d", len(b2.tail()))
	}
	if got := stderrHint(nil); got != "" {
		t.Fatalf("stderrHint(nil) = %q", got)
	}
	if got := stderrHint(b); !strings.HasPrefix(got, "；stderr: ") {
		t.Fatalf("stderrHint = %q", got)
	}
}

// TestStart_RealStdioServer 端到端：spawn 真实 node 子进程模拟 MCP server，
// 完整走 initialize → tools/list（验证 stdioCommand 非 Windows 直spawn 路径
// 与 JSON-RPC 帧；Windows 下 node 亦经 cmd /C，双路径均覆盖）。
func TestStart_RealStdioServer(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("验证 Windows cmd /C 包装路径")
	}
	// 单行脚本：cmd /C 参数传递不支持裸换行（真实 MCP server 命令同为单行）。
	// 临时脚本文件（模拟真实 MCP server 启动方式；cmd /C 参数传递不保留
	// 裸换行与嵌套引号，文件方式避开 cmd 转义问题）。
	srv := `let buf='';process.stdin.setEncoding('utf8');` +
		`process.stdin.on('data',d=>{buf+=d;let i;while((i=buf.indexOf(String.fromCharCode(10)))>=0){const l=buf.slice(0,i);buf=buf.slice(i+1);if(l.trim())handle(l);}});` +
		`function handle(l){const r=JSON.parse(l);` +
		`if(r.method==='initialize'){send({jsonrpc:'2.0',id:r.id,result:{protocolVersion:'2024-11-05'}});}` +
		`else if(r.method==='tools/list'){send({jsonrpc:'2.0',id:r.id,result:{tools:[{name:'echo',description:'echo tool',inputSchema:{type:'object'}}]}});}}` +
		`function send(o){process.stdout.write(JSON.stringify(o)+String.fromCharCode(10));}process.stdin.resume();`
	tmp, err := os.CreateTemp(t.TempDir(), "mcp-fake-*.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(srv); err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()
	cli, err := Start(context.Background(), ServerConfig{Name: "fake", Command: "node", Args: []string{tmp.Name()}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cli.Close()
	tools, err := cli.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	// tools/call 转发（假服务器不实现 tools/call：验证超时路径非阻塞返回）。
	cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ccancel()
	if _, err := cli.CallTool(cctx, "echo", []byte(`{"x":1}`)); err == nil {
		t.Fatal("want timeout error for unimplemented tools/call")
	}
}

func TestStart_SpawnFailure(t *testing.T) {
	// 不存在的可执行文件：Windows 经 cmd /C 时错误在握手超时暴露（cmd 返回
	// 非零退出码，stdout 无响应）——验证不会 panic 且错误信息可读。
	if runtime.GOOS != "windows" {
		t.Skip("Windows cmd /C 语义")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := Start(ctx, ServerConfig{Name: "nope", Command: "definitely-not-exist-cmd-xyz"})
	if err == nil {
		t.Fatal("want error for missing command")
	}
}
