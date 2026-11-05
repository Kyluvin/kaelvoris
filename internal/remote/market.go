package remote

// market.go —— 插件市场（服务端存储 + REST 分发）。
//
// 定位（替换 PC 端硬编码「内置插件市场」）：
//   - PC 节点把技能（SKILL.md 包）/ 插件（plugin.json 包）/ MCP 服务器配置
//     打包为 zip 发布到服务端，全节点可浏览与下载安装 —— 技能、MCP、插件
//     统一进商店（kind: skill | plugin | mcp）；
//   - 存储全部在数据目录 market/ 下：market/<id>/meta.json + pkg.zip，
//     服务重启天然持久（与 tasks.json 同层，无独立数据库）；
//   - REST 端点（见 registerMarketRoutes）：
//       GET    /api/market              市场列表（公开读，与 /img/ 同级分发面）
//       POST   /api/market              发布/更新（multipart；节点令牌或 REST 凭据）
//       GET    /api/market/download     下载 pkg.zip（?id=，公开读）
//       DELETE /api/market?id=          删除（仅 REST 凭据；管理动作）
//   - 同 ID 重复发布 = 升级（覆盖 meta + zip），PC 端「发布到市场」幂等。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 市场包上传上限（插件含二进制可达数十 MB；256MB 对齐媒体上传上限）。
const maxMarketUploadBytes = 256 << 20

// marketIDRE 市场 ID 规范：小写字母/数字/连字符，1~64 位（兼作目录名安全校验）。
var marketIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// MarketKind 市场条目类别。
const (
	MarketKindPlugin = "plugin" // 插件包（plugin.json + entry/skills）
	MarketKindSkill  = "skill"  // 技能包（SKILL.md）
	MarketKindMCP    = "mcp"    // MCP 服务器配置包（plugin.json 的 mcp 段）
)

// MarketItem 市场条目元数据（market/<id>/meta.json）。
type MarketItem struct {
	ID          string `json:"id"`                    // 唯一 ID（同 ID 覆盖发布 = 升级）
	Kind        string `json:"kind"`                  // plugin | skill | mcp
	Name        string `json:"name"`                  // 显示名
	Version     string `json:"version,omitempty"`     // 版本
	Description string `json:"description,omitempty"` // 一句话描述
	Author      string `json:"author,omitempty"`      // 作者
	FileName    string `json:"fileName,omitempty"`    // 包内主文件名（下载后展示用）
	Size        int64  `json:"size,omitempty"`        // zip 字节数
	PublishedBy string `json:"publishedBy,omitempty"` // 发布者（节点 ID 或账号名）
	PublishedAt int64  `json:"publishedAt"`           // 发布时间 ms（升级覆盖）
}

// validateMarketKind 类别白名单（缺省按 plugin 解读，兼容旧客户端）。
func validateMarketKind(kind string) (string, error) {
	switch kind {
	case "", MarketKindPlugin:
		return MarketKindPlugin, nil
	case MarketKindSkill, MarketKindMCP:
		return kind, nil
	}
	return "", fmt.Errorf("无效的市场条目类别: %s（plugin|skill|mcp）", kind)
}

// SetMarketDir 注入市场存储目录（数据目录下 market/；NewServer 后调用；
// 空串 = 市场未启用，/api/market* 返回 404）。
func (s *Server) SetMarketDir(dir string) {
	s.mu.Lock()
	s.marketDir = dir
	s.mu.Unlock()
}

// marketDirOf 当前市场目录（每请求动态读取；空 = 未启用）。
func (s *Server) marketDirOf() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marketDir
}

// registerMarketRoutes 挂载市场 REST 路由（NewServer 内调用；未启用时 handler 内 404；统一经 s.route）。
func (s *Server) registerMarketRoutes(mux *http.ServeMux) {
	s.route(mux, "/api/market", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleMarketList(w, r) // 列表：节点令牌或 REST 凭据（imageAuth 已包装）
		case http.MethodPost:
			s.handleMarketPublish(w, r) // 发布：节点令牌或 REST 凭据
		case http.MethodDelete:
			s.handleMarketDelete(w, r) // 删除：节点令牌或 REST 凭据（与列表/发布同域）
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST/DELETE")
		}
	}, s.imageAuth)
	// 下载公开读（zip 为安全文件名目录内的固定文件，无穿越面）。
	s.route(mux, "/api/market/download", s.handleMarketDownload)
}

// marketItemDir 单条目目录（ID 已过 marketIDRE 校验，无穿越面）。
func marketItemDir(root, id string) string {
	return filepath.Join(root, id)
}

// handleMarketList GET /api/market：扫描 market/ 全部条目 meta.json，按发布时间倒序。
func (s *Server) handleMarketList(w http.ResponseWriter, r *http.Request) {
	root := s.marketDirOf()
	if root == "" {
		writeJSONError(w, http.StatusNotFound, "插件市场未启用")
		return
	}
	items, err := marketList(root)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "读取市场失败: "+err.Error())
		return
	}
	writeJSONOK(w, items)
}

// marketList 扫描市场目录（损坏 meta 跳过不致命）。
func marketList(root string) ([]MarketItem, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []MarketItem{}, nil
		}
		return nil, err
	}
	out := make([]MarketItem, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m MarketItem
		if json.Unmarshal(b, &m) != nil || m.ID == "" {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PublishedAt > out[j].PublishedAt })
	return out, nil
}

// handleMarketPublish POST /api/market（multipart）：
//   - file: zip 包（内含 plugin.json 或 SKILL.md，PC 端打包）；
//   - id/name/version/kind/description/author: 元数据表单字段。
//
// 同 ID 覆盖 = 升级。publishedBy 从鉴权主体推导（节点令牌 → nodeID；Basic → 账号）。
func (s *Server) handleMarketPublish(w http.ResponseWriter, r *http.Request) {
	root := s.marketDirOf()
	if root == "" {
		writeJSONError(w, http.StatusNotFound, "插件市场未启用")
		return
	}
	if err := r.ParseMultipartForm(maxMarketUploadBytes + (8 << 20)); err != nil {
		writeJSONError(w, http.StatusBadRequest, "multipart 解析失败: "+err.Error())
		return
	}
	id := r.FormValue("id")
	if !marketIDRE.MatchString(id) {
		writeJSONError(w, http.StatusBadRequest, "id 无效（小写字母/数字/连字符，1~64 位）")
		return
	}
	kind, err := validateMarketKind(r.FormValue("kind"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "缺少 zip 包（file 字段）")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMarketUploadBytes))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "读取上传内容失败: "+err.Error())
		return
	}
	if len(data) == 0 {
		writeJSONError(w, http.StatusBadRequest, "上传内容为空")
		return
	}

	// 发布者：节点令牌命中 → nodeID；否则 Basic 账号（imageAuth 已保证其一通过）。
	by := s.nodeIDForToken(r)
	if by == "" {
		if u, _, ok := r.BasicAuth(); ok && u != "" {
			by = u
		}
	}

	item := MarketItem{
		ID:          id,
		Kind:        kind,
		Name:        firstNonEmpty(r.FormValue("name"), id),
		Version:     r.FormValue("version"),
		Description: r.FormValue("description"),
		Author:      r.FormValue("author"),
		FileName:    r.FormValue("fileName"),
		Size:        int64(len(data)),
		PublishedBy: by,
		PublishedAt: time.Now().UnixMilli(),
	}
	if err := marketSave(root, &item, data); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeJSONOK(w, item)
}

// marketSave 落盘单条目（meta.json + pkg.zip；先写 zip 再写 meta，读侧以 meta 为准）。
func marketSave(root string, m *MarketItem, zipData []byte) error {
	dir := marketItemDir(root, m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg.zip"), zipData, 0o644); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644)
}

// handleMarketDownload GET /api/market/download?id=<id>：返回 pkg.zip。
func (s *Server) handleMarketDownload(w http.ResponseWriter, r *http.Request) {
	root := s.marketDirOf()
	if root == "" {
		writeJSONError(w, http.StatusNotFound, "插件市场未启用")
		return
	}
	id := r.URL.Query().Get("id")
	if !marketIDRE.MatchString(id) {
		writeJSONError(w, http.StatusBadRequest, "id 无效")
		return
	}
	p := filepath.Join(marketItemDir(root, id), "pkg.zip")
	f, err := os.Open(p)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "条目不存在: "+id)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	noStore(w)
	w.Header().Set("Content-Type", "application/zip")
	if st != nil {
		w.Header().Set("Content-Length", fmt.Sprint(st.Size()))
	}
	_, _ = io.Copy(w, f)
}

// handleMarketDelete DELETE /api/market?id=：删除条目（目录整删）。
func (s *Server) handleMarketDelete(w http.ResponseWriter, r *http.Request) {
	root := s.marketDirOf()
	if root == "" {
		writeJSONError(w, http.StatusNotFound, "插件市场未启用")
		return
	}
	id := r.URL.Query().Get("id")
	if !marketIDRE.MatchString(id) {
		writeJSONError(w, http.StatusBadRequest, "id 无效")
		return
	}
	if err := os.RemoveAll(marketItemDir(root, id)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	writeJSONOK(w, map[string]interface{}{"deleted": id})
}

// firstNonEmpty 第一个非空字符串（市场发布表单兜底）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// marketPackageName zip 附件名（Content-Disposition；防浏览器下载名混乱）。
func marketPackageName(m *MarketItem) string {
	name := m.ID + "-" + strings.ReplaceAll(m.Version, "+", "_")
	if name == "" {
		name = "pkg"
	}
	return name + ".zip"
}
