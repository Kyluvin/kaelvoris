package desktop

// market_panel.go —— 插件市场（PC 端 ⇆ 服务端市场）。
//
// 职责（替换 plugin_panel.go 的硬编码 builtinPlugins 内置目录）：
//   - ListMarketItems：拉取服务端市场列表（GET /api/market，REST 凭据 = remote.json 节点令牌
//     或统一账号——服务端 restAuth 接受 Basic/clientToken，本端统一走节点令牌快速路径，
//     未配置远程时回落内置目录）；
//   - InstallFromMarket：从市场下载 zip → 复用 InstallPluginFromURL 的解压安装链；
//   - PublishSkillToMarket：把本地技能（SKILL.md 目录）打包发布到市场（kind=skill）；
//   - PublishPluginToMarket：把本地插件目录（plugin.json）打包发布到市场（kind=plugin）；
//   - PublishMCPToMarket：把手动 MCP 服务器配置（mcp-servers.json 条目）导出为
//     plugin.json 包发布（kind=mcp，下载安装后即成为本地插件接入 MCP 运行时）。
//
// 鉴权：发布走 X-Node-Token（节点令牌，服务端 imageAuth 接受）；列表/删除走同令牌
// 头（restAuth 的 X-Access-Token 同形）。zip 打包零第三方依赖（archive/zip）。

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"kaelvoris/internal/plugin"
	"kaelvoris/internal/skills"
)

// MarketKind 市场条目类别（与服务端 MarketKind* 对齐）。
const (
	marketKindPlugin = "plugin"
	marketKindSkill  = "skill"
	marketKindMCP    = "mcp"
)

// MarketItemView 市场条目（服务端 MarketItem 的 PC 端投影 + 安装态）。
type MarketItem struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	Size        int64  `json:"size,omitempty"`
	PublishedBy string `json:"publishedBy,omitempty"`
	PublishedAt int64  `json:"publishedAt,omitempty"`
	Installed   bool   `json:"installed"`
}

// marketIDRE 同服务端 marketIDRE（发布前本地预校验）。
var marketIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// marketBase 服务端 HTTP 根（remote.json ServerURL；空 = 未配置远程）。
func (a *App) marketBase() (string, string, bool) {
	cfg := a.loadRemoteConfig()
	if !cfg.Enabled || strings.TrimSpace(cfg.ServerURL) == "" || strings.TrimSpace(cfg.Token) == "" {
		return "", "", false
	}
	return httpBaseFromWS(wsURLFromHTTP(cfg.ServerURL)), strings.TrimSpace(cfg.Token), true
}

// marketHTTP 市场请求客户端（发布大包放宽到 10 分钟）。
var marketHTTP = &http.Client{Timeout: 10 * time.Minute}

// maxMarketDownloadBytes 下载限流上限（与服务端上传上限同量级；服务端常量未导出，此处独立声明）。
const maxMarketDownloadBytes = 256 << 20

// ListMarketItems 拉取服务端市场列表（含本地安装态标记）。
// 远程未配置时返回内置目录兜底（老行为不回归）；远程已配置但服务端市场
// 为空/失败时返回空列表（不再回落内置目录——内置项无真实下载源，展示后
// 点「下载安装」必 404，属误导性 UI）。
func (a *App) ListMarketItems() []MarketItem {
	base, token, ok := a.marketBase()
	if !ok {
		return a.builtinMarketFallback()
	}
	req, err := http.NewRequest(http.MethodGet, base+"/api/market", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-Node-Token", token)
	resp, err := marketHTTP.Do(req)
	if err != nil {
		a.pushToast("⚠️ 市场拉取失败: " + truncateRunes(err.Error(), 120))
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		a.pushToast(fmt.Sprintf("⚠️ 市场拉取失败 HTTP %d", resp.StatusCode))
		return nil
	}
	var out struct {
		OK   bool         `json:"ok"`
		Data []MarketItem `json:"data"`
	}
	if json.Unmarshal(b, &out) != nil || !out.OK {
		a.pushToast("⚠️ 市场响应解析失败")
		return nil
	}
	items := out.Data
	if items == nil {
		items = []MarketItem{}
	}
	for i := range items {
		_, err := plugin.Get(items[i].ID)
		items[i].Installed = err == nil
	}
	return items
}

// builtinMarketItem 内置目录条目（远程未配置时的兜底市场；同 plugin_panel 旧清单）。
type builtinMarketItem struct {
	ID          string
	Name        string
	Version     string
	Description string
	DownloadURL string
}

// builtinMarketFallback 内置目录投影（ListAvailablePlugins 同源数据 + 安装态）。
func (a *App) builtinMarketFallback() []MarketItem {
	avail := a.ListAvailablePlugins()
	out := make([]MarketItem, 0, len(avail))
	for _, p := range avail {
		out = append(out, MarketItem{
			ID:          p.ID,
			Kind:        marketKindPlugin,
			Name:        p.Name,
			Version:     p.Version,
			Description: p.Description,
			Installed:   p.Installed,
		})
	}
	return out
}

// InstallFromMarketRequest 市场安装入参。
type InstallFromMarketRequest struct {
	ID string `json:"id"` // 市场条目 ID
}

// InstallFromMarket 从市场下载并安装（zip → 复用 URL 安装链路）。
// 来源分流：内置目录项（远程不可用时的兜底清单）没有服务端下载源，
// 改走本地内置发行目录安装（office 插件随仓库发布）；远程市场项走
// 服务端 /api/market/download 下载安装链。
func (a *App) InstallFromMarket(req InstallFromMarketRequest) (*PluginView, error) {
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return nil, errors.New("市场条目 ID 不能为空")
	}
	base, _, ok := a.marketBase()
	if !ok {
		// 内置目录项：本地发行目录安装（zip 下载源不存在，避免 404）。
		return a.installBuiltinFromDir(id)
	}
	dl := base + "/api/market/download?id=" + id
	// 按包类型分流：技能包落技能库，插件/MCP 包走插件安装链路。
	if kind, _ := a.marketItemKind(id); kind == marketKindSkill {
		if err := a.installSkillFromMarket(id); err != nil {
			return nil, err
		}
		return &PluginView{ID: id, Name: id, Source: "market"}, nil
	}
	// 经 InstallPluginFromURL 复用下载/解压/校验/安装全链路（zip-slip 防护同源；download 公开无鉴权）。
	return a.InstallPluginFromURL(InstallPluginFromURLRequest{URL: dl})
}

// installBuiltinFromDir 内置目录项安装：实现迁至 builtin_plugins.go（embed 发行目录）。

// installSkillFromMarket 下载技能包并安装到技能库（<workspace>/.kaelvoris/skills/<name>/SKILL.md）。
func (a *App) installSkillFromMarket(id string) error {
	base, _, ok := a.marketBase()
	if !ok {
		return errors.New("未配置远程服务（设置 → 远程服务：填服务端地址并登录）")
	}
	resp, err := marketHTTP.Get(base + "/api/market/download?id=" + id)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败 HTTP %d", resp.StatusCode)
	}
	// ponytail: 256MB 上限与服务端上传常量同量级，正常技能包远小于此。
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxMarketDownloadBytes))
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return fmt.Errorf("技能包解析失败: %w", err)
	}
	// 提取 <name>/SKILL.md（取第一个命中）。
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || filepath.Base(f.Name) != "SKILL.md" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		content, err := io.ReadAll(io.LimitReader(rc, 8<<20))
		rc.Close()
		if err != nil {
			return err
		}
		name := filepath.Base(filepath.Dir(f.Name))
		if name == "." || name == "/" || !validSkillDirName(name) {
			name = id
		}
		return a.SaveSkill(name, string(content))
	}
	return errors.New("技能包内未找到 SKILL.md")
}

// validSkillDirName 校验技能目录名（对齐 skills.validName：小写字母/数字/连字符，≤64）。
func validSkillDirName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// marketItemKind 查询市场条目类型（失败兜底 plugin，兼容旧包）。
func (a *App) marketItemKind(id string) (string, error) {
	base, _, ok := a.marketBase()
	if !ok {
		return marketKindPlugin, errors.New("未配置远程服务")
	}
	req, err := http.NewRequest(http.MethodGet, base+"/api/market", nil)
	if err != nil {
		return marketKindPlugin, err
	}
	req.Header.Set("X-Node-Token", a.marketToken())
	resp, err := marketHTTP.Do(req)
	if err != nil {
		return marketKindPlugin, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return marketKindPlugin, fmt.Errorf("市场查询失败 HTTP %d", resp.StatusCode)
	}
	var out struct {
		OK   bool         `json:"ok"`
		Data []MarketItem `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil || !out.OK {
		return marketKindPlugin, errors.New("市场响应解析失败")
	}
	for _, it := range out.Data {
		if it.ID == id {
			if it.Kind == "" {
				return marketKindPlugin, nil
			}
			return it.Kind, nil
		}
	}
	return marketKindPlugin, nil
}

// ---------- 发布 ----------

// PublishSkillToMarket 发布本地技能到市场（kind=skill）：打包 <name>/SKILL.md。
func (a *App) PublishSkillToMarket(name string) (*MarketItem, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("技能名不能为空")
	}
	// 技能元数据（skills.Manager 无 Get；从 All() 线性查找同名技能）。
	var md *skills.Metadata
	for _, s := range a.currentSkillMgr().All() {
		if s.Name == name {
			s2 := s
			md = &s2
			break
		}
	}
	if md == nil {
		return nil, fmt.Errorf("技能不存在: %s", name)
	}
	if md.Source == "builtin" {
		return nil, errors.New("内置技能不可发布（请先另存为自定义技能）")
	}
	content, err := a.currentSkillMgr().LoadRaw(name)
	if err != nil {
		return nil, err
	}
	return a.marketPublish(marketKindSkill, name, []byte(content), name, "", md.Description, "")
}

// PublishPluginToMarket 发布本地插件目录到市场（kind=plugin）：整目录打包
// （含 plugin.json、entry 二进制、skills/），下载安装后即完整可用。
func (a *App) PublishPluginToMarket(id string) (*MarketItem, error) {
	id = strings.TrimSpace(id)
	m, err := plugin.Get(id)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	n, err := zipDir(zw, m.Dir, "")
	zw.Close()
	if err != nil {
		return nil, fmt.Errorf("打包插件失败: %w", err)
	}
	if n == 0 {
		return nil, errors.New("插件目录为空")
	}
	// 插件整目录已由 zipDir 打包，直接经预打包 zip 通道发布。
	return a.marketPublishBytes(marketKindPlugin, id, buf.Bytes(), m.Name, m.Version, m.Description, m.Author)
}

// marketPublishBytes 直接以预打包 zip 发布（插件/MCP 整包场景；
// 併入服务端 POST /api/market multipart 上传）。
func (a *App) marketPublishBytes(kind, id string, zipBytes []byte, name, version, description, author string) (*MarketItem, error) {
	base, _, ok := a.marketBase()
	if !ok {
		return nil, errors.New("未配置远程服务（设置 → 远程服务：填服务端地址并登录）")
	}
	if !marketIDRE.MatchString(id) {
		return nil, fmt.Errorf("非法市场 ID: %s（小写字母/数字/连字符，1~64 位）", id)
	}
	// multipart 构造（zip 主文件 + kind/name/version/description/author 字段）。
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("kind", kind)
	_ = mw.WriteField("name", name)
	_ = mw.WriteField("version", version)
	_ = mw.WriteField("description", description)
	_ = mw.WriteField("author", author)
	_ = mw.WriteField("id", id)
	fw, err := mw.CreateFormFile("file", id+".zip")
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(zipBytes); err != nil {
		return nil, err
	}
	ct := mw.FormDataContentType()
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, base+"/api/market", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Node-Token", a.marketToken())
	resp, err := marketHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		OK   bool        `json:"ok"`
		Data *MarketItem `json:"data"`
	}
	if json.Unmarshal(b, &out) != nil || !out.OK || out.Data == nil {
		return nil, fmt.Errorf("发布失败 HTTP %d: %s", resp.StatusCode, truncateRunes(string(b), 200))
	}
	return out.Data, nil
}

// MCPServerPublishRequest MCP 服务器配置发布入参（设置页 MCP 面板「发布到市场」）。
type MCPServerPublishRequest struct {
	Name string `json:"name"` // mcp-servers.json 中的条目名
}

// PublishMCPToMarket 把手动 MCP 服务器配置导出为插件包发布（kind=mcp）：
// 生成仅含 mcp 段的 plugin.json（transport/command/args/url/env/headers 原样导出，
// env/headers 中的密钥原样上传 —— 市场为私有部署内部信任域，与 mcp-servers.json
// 同级敏感面；后续如需公开市场可加掩码开关）。
// 下载安装后即成为本地插件，经 buildMCPTools 自动接入。
func (a *App) PublishMCPToMarket(req MCPServerPublishRequest) (*MarketItem, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("MCP 服务器名不能为空")
	}
	var target *MCPServer
	for _, s := range a.loadMCPServers() {
		if s.Name == name {
			s2 := s
			target = &s2
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("MCP 服务器不存在: %s", name)
	}
	mcpCfg := &plugin.MCPConfig{
		Transport: target.Transport,
		Command:   target.Command,
		Args:      target.Args,
		URL:       target.URL,
		Env:       target.Env,
		Headers:   target.Headers,
	}
	man := &plugin.Manifest{
		ID:          sanitizeMarketID("mcp-" + name),
		Name:        name + "（MCP）",
		Version:     "1.0.0",
		Description: "MCP 服务器配置包：安装后自动接入 " + name + "（" + target.Transport + "）",
		Author:      "Kaelvoris 导出",
		Source:      "market",
		MCP:         mcpCfg,
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	{
		mb, err := json.MarshalIndent(man, "", "  ")
		if err != nil {
			zw.Close()
			return nil, err
		}
		w, err := zw.Create(man.ID + "/plugin.json")
		if err != nil {
			zw.Close()
			return nil, err
		}
		if _, err := w.Write(mb); err != nil {
			zw.Close()
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return a.marketPublishBytes(marketKindMCP, man.ID, buf.Bytes(), man.Name, man.Version, man.Description, man.Author)
}

// marketPublish 打 zip 并发布（统一收口；技能包单文件也走 zip 通道）。
func (a *App) marketPublish(kind, id string, content []byte, name, version, description, author string) (*MarketItem, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// zipEntry 市场包内主文件名（skill → SKILL.md，其余 → plugin.json）。
	w, err := zw.Create(id + "/" + zipEntry(kind, id))
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(content); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return a.marketPublishBytes(kind, id, buf.Bytes(), name, version, description, author)
}

// marketToken 节点令牌（发布鉴权；与 marketBase 同源）。
func (a *App) marketToken() string {
	_, token, _ := a.marketBase()
	return token
}

// zipEntry 市场包内主文件名（skill → SKILL.md，其余 → plugin.json）。
func zipEntry(kind, id string) string {
	if kind == marketKindSkill {
		return "SKILL.md"
	}
	return "plugin.json"
}

// sanitizeMarketID 从任意字符串推导合法市场 ID（小写化 + 非法字符折叠为连字符）。
func sanitizeMarketID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b []byte
	lastDash := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			b = append(b, c)
			lastDash = false
		case !lastDash && len(b) > 0:
			b = append(b, '-')
			lastDash = true
		}
	}
	out := strings.Trim(string(b), "-")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// zipDir 整目录打包（插件发布用；跳过 .git/node_modules）。
func zipDir(zw *zip.Writer, root, prefix string) (int, error) {
	count := 0
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		if err == nil {
			count++
		}
		return err
	})
	return count, err
}
