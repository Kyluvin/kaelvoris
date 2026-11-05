// Package desktop MCP 惰性加载桥（上下文超限修复）。
//
// 根因：buildMCPTools 任务启动即连接全部 MCP 服务器+插件并注册全部工具
// （插件 207 个工具全量进模型上下文）→「问了一句就提示上下文长度超了」。
//
// 对齐 Claude Code / codex 的 MCP 懒加载经验：
//   - Claude Code 14.4+：MCP 工具默认不进入系统提示词，采用 Tool Search
//     按需检索调用（defer_loading），"load more tools as needed"；
//   - codex：MCP 服务器列表仅以轻量目录透出，模型显式启用/调用时才连接。
//
// 落地三层：
//  1. mcpWanted：任务文本 + 工作区提示词（AGENTS.md/README 等可见文本）命中
//     服务器名 → 启动时自动激活（零交互直达）；
//  2. mcpCandidates + MCPLazyHint 提示词段：未激活服务器以「一行一服务器」
//     目录透出，模型要用时先 mcp_activate 再调用；
//  3. mcpActivateTool：运行中热加载——连接 server → 工具注册进 Registry
//     → 下轮请求即可调用（toolDefs 每轮现算，无需重启任务）；conn 并入
//     mcpClients，任务结束统一关闭（与启动期连接同生命周期）。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kaelvoris/internal/employee"
	"kaelvoris/internal/mcp"
	"kaelvoris/internal/plugin"
	"kaelvoris/internal/tools"
)

// mcpCandidateEntry MCP 惰性目录条目（一行透出给模型）。
type mcpCandidateEntry struct {
	name string // 服务器/插件 ID（mcp_activate 的 server 参数值）
	desc string // 显示描述（插件 description 或传输摘要）
}

// mcpWantedResult 激活判定结果。
type mcpWantedResult struct {
	servers    []string            // 命中（激活）的服务器名
	candidates []mcpCandidateEntry // 未命中的候选目录（提示词透出）
}

// mcpWanted 判定本任务应激活哪些 MCP 服务器：任务文本或工作区提示词命中
// 服务器名（大小写不敏感的词边界匹配）→ 激活；其余进候选目录。
// 判定源（懒经验对齐 Claude Code deferred tools 的「上下文按需披露」）：
//   - 任务文本（用户当前指令——最高优先级信号，App.mcpProbeText）；
//   - 工作区提示词可见文本（AGENTS.md / CLAUDE.md / .cursorrules / README.md，
//     项目约定里写了用哪个 MCP，模型照约定走时零交互直达）。
func (a *App) mcpWanted() mcpWantedResult {
	// 候选全集：手动配置（enabled）+ 插件（启用且有 MCP 端点），按名去重。
	type cand struct {
		name, desc string
	}
	var all []cand
	seen := map[string]bool{}
	for _, s := range a.loadMCPServers() {
		if !s.Enabled || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		desc := s.Transport
		if desc == "" {
			desc = "stdio"
		}
		all = append(all, cand{name: s.Name, desc: desc})
	}
	for _, p := range plugin.List() {
		if !plugin.IsEnabled(p.ID) || seen[p.ID] {
			continue
		}
		if _, _, _, _, _, _, _, ok := p.MCPServerOf(); !ok {
			continue // 纯技能插件：无 MCP 端点，不进目录
		}
		seen[p.ID] = true
		all = append(all, cand{name: p.ID, desc: p.Description})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })

	// 激活信号文本：任务文本由宿主注入（a.mcpProbeText），叠加工作区提示词。
	signal := a.mcpProbeText + "\n" + workspacePromptText()

	var out mcpWantedResult
	for _, c := range all {
		if mcpSignalHit(signal, c.name) {
			out.servers = append(out.servers, c.name)
		} else {
			out.candidates = append(out.candidates, mcpCandidateEntry{name: c.name, desc: c.desc})
		}
	}
	return out
}

// workspacePromptText 读取工作区提示词文件可见文本（存在才计入，总量截断
// 32KB——激活判定只需关键词，无需全文）。空 = 工作区无提示词文件。
func workspacePromptText() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", ".cursorrules", "README.md"} {
		raw, err := os.ReadFile(filepath.Join(wd, name))
		if err != nil {
			continue
		}
		b.Write(raw)
		b.WriteByte('\n')
		if b.Len() > 32*1024 {
			break
		}
	}
	return b.String()
}

// mcpSignalHit 激活判定：信号文本中出现服务器名（大小写不敏感）。
// 长名（≥4 字符）直接子串匹配；短名要求词边界（前后非字母数字），防
// "go" 误命中 "category"。
// ponytail: 简版启发式，中文任务里英文名子串命中足够；误激活的代价只是
// 多连一个 server，升级路径是分词+同义词表。
func mcpSignalHit(signal, name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	signal = strings.ToLower(signal)
	if name == "" || signal == "" {
		return false
	}
	if len([]rune(name)) >= 4 {
		return strings.Contains(signal, name)
	}
	// 短名：词边界扫描（rune 级，兼容中文邻接）。
	isAlnum := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
	}
	runes := []rune(signal)
	rname := []rune(name)
	for i := 0; i+len(rname) <= len(runes); i++ {
		if string(runes[i:i+len(rname)]) != name {
			continue
		}
		before := i == 0 || !isAlnum(runes[i-1])
		after := i+len(rname) >= len(runes) || !isAlnum(runes[i+len(rname)])
		if before && after {
			return true
		}
	}
	return false
}

// mcpLazyHint 组装系统提示词的 MCP TOOL CATALOG / LAZY LOADING 段
// （对齐 Claude Code Tool Search「工具默认不进提示词，按需检索加载」经验）：
//   - candidates 为空（全部已激活或未配置 MCP）→ 返回空串，零开销不注入；
//   - 会话禁用 MCP 或员工禁用 MCP → 返回空串（模型完全不可见，双保险语义）；
//   - 非空时注入目录 + mcp_activate 激活语义（工具已注册才有效，宿主同步注册）。
func mcpLazyHint(candidates []mcpCandidateEntry, sessionMCPDisabled bool, emp *employee.Employee) string {
	if sessionMCPDisabled || len(candidates) == 0 || (emp != nil && !emp.MCPEnabled()) {
		return ""
	}
	var b strings.Builder
	b.WriteString(`====

MCP TOOL CATALOG (LAZY LOADING)

MCP servers/plugins below are NOT connected yet (lazy loading, Claude Code style). Their tools are absent from your tool list to keep the context lean:

`)
	for _, c := range candidates {
		desc := strings.TrimSpace(c.desc)
		if desc == "" {
			desc = "MCP server/plugin"
		}
		desc = strings.ReplaceAll(desc, "\n", " ")
		if len([]rune(desc)) > 120 {
			desc = string([]rune(desc)[:120]) + "..."
		}
		b.WriteString("- " + c.name + " — " + desc + "\n")
	}
	b.WriteString(`
WHEN you need capabilities from one of these servers (or the user asks for one by name):
1. Call the mcp_activate tool with {"server": "<name from the catalog above>"}.
2. On success, its tools (mcp_<server>_<tool>) become available on your NEXT tool-calling round — then call them directly.
Do NOT activate servers you do not need. Do NOT guess unavailable tool names — activate first.

If a task mentions a server name, it is usually auto-activated at start; only the catalog above requires explicit activation.`)
	return b.String()
}

// ---------- mcp_activate 运行中热加载 ----------

// mcpActivateTool mcp_activate 工具：模型显式激活未加载的 MCP 服务器/插件。
// 连接 → 工具注册进任务 Registry → 下轮请求即可调用（toolDefs 每轮现算）。
type mcpActivateTool struct {
	app       *App
	workspace string
	registry  *tools.Registry
	// addConn 注册连接生命周期钩子（并入任务 mcpClients，任务结束统一 Close）。
	addConn func(c mcp.Conn)
}

func (t *mcpActivateTool) Name() string { return "mcp_activate" }

func (t *mcpActivateTool) Description() string {
	return "Load an MCP server's tools on demand (lazy loading). " +
		"MCP servers/plugins are NOT connected at task start; only a catalog is listed in the prompt. " +
		"When you need tools from a listed server, call mcp_activate with its server name first — " +
		"after success the tools (mcp_<server>_<tool>) are available on your NEXT tool-calling round. " +
		"Only activate servers you actually need; keep the context lean."
}

func (t *mcpActivateTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"server": map[string]interface{}{
				"type":        "string",
				"description": "MCP server / plugin name as shown in the MCP TOOL CATALOG prompt section",
			},
		},
		"required": []string{"server"},
	}
}

func (t *mcpActivateTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Server string `json:"server"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	name := strings.TrimSpace(p.Server)
	if name == "" {
		return "", fmt.Errorf("server is required")
	}
	cfg, ok := t.app.mcpServerConfigOf(name, t.workspace)
	if !ok {
		return "", fmt.Errorf("MCP 服务器/插件 %q 不存在、已禁用或无 MCP 端点（请对照系统提示词 MCP TOOL CATALOG 段中的名称，或检查 MCP 设置面板）", name)
	}
	// 幂等：该服务器已有工具注册则直接返回（防重复连接占资源）。
	prefix := "mcp_" + sanitizeMCPName(name) + "_"
	for _, n := range t.registry.Names() {
		if strings.HasPrefix(n, prefix) {
			return "MCP 服务器 " + name + " 已激活，工具可直接调用。", nil
		}
	}
	// 20s 总预算（initialize + tools/list）：npx 冷启动需解析/下载包。
	mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cli, n, err := mcp.RegisterAll(mctx, cfg, func(_ string, rt *mcp.RemoteTool) error {
		return t.registry.Register(rt)
	})
	if err != nil {
		if cli != nil {
			cli.Close()
		}
		return "", fmt.Errorf("MCP 服务器 %q 连接失败: %v", name, err)
	}
	if t.addConn != nil {
		t.addConn(cli)
	}
	if n == 0 {
		return "MCP 服务器 " + name + " 已连接，但未提供任何工具。", nil
	}
	return fmt.Sprintf("MCP 服务器 %s 已激活，注册 %d 个工具（mcp_%s_*）。从下一轮开始可直接调用。", name, n, name), nil
}

// mcpServerConfigOf 按名解析 MCP 服务器配置（手动配置优先，插件兜底）；
// 禁用/无端点/未启用的返回 ok=false。
func (a *App) mcpServerConfigOf(name, workspace string) (mcp.ServerConfig, bool) {
	for _, s := range a.loadMCPServers() {
		if s.Name == name && s.Enabled {
			switch s.Transport {
			case "", "stdio":
				if strings.TrimSpace(s.Command) == "" {
					return mcp.ServerConfig{}, false
				}
			case "sse", "http":
				if strings.TrimSpace(s.URL) == "" {
					return mcp.ServerConfig{}, false
				}
			default:
				return mcp.ServerConfig{}, false
			}
			return mcp.ServerConfig{Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Headers: s.Headers}, true
		}
	}
	for _, p := range plugin.List() {
		if p.ID != name || !plugin.IsEnabled(p.ID) {
			continue
		}
		n, transport, command, url, args, env, headers, ok := p.MCPServerOf()
		if !ok || (transport == "stdio" && strings.TrimSpace(command) == "") || (transport != "stdio" && strings.TrimSpace(url) == "") {
			return mcp.ServerConfig{}, false
		}
		if env == nil {
			env = map[string]string{}
		}
		if workspace != "" {
			env["GOFFICE_WORKSPACE"] = workspace
		}
		if (transport == "stdio" || transport == "") && command != "" && !filepath.IsAbs(command) {
			command = filepath.Join(p.Dir, command)
		}
		return mcp.ServerConfig{Name: n, Command: command, Args: args, Env: env, URL: url, Headers: headers}, true
	}
	return mcp.ServerConfig{}, false
}

// sanitizeMCPName 与 internal/mcp sanitizeToolName 的 server 段清洗规则一致
// （非 [a-zA-Z0-9_] 替换下划线）：幂等前缀判定用。
func sanitizeMCPName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
