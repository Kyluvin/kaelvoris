// Package remote 知识库（doc/）REST 端点：移动端/管理后台经 REST 读写部署目录
// 下 doc/ 内的 Markdown 文档（APK WebView 壳加载的 H5「知识」页数据源）。
//
// 端点（全部经 docAuth 鉴权，见下方说明）：
//
//	GET  /api/docs                 列表（含每个文档的字节大小与修改时间）
//	GET  /api/docs/read?name=x.md  读取单个文档全文
//	POST /api/docs/write           写入（创建/覆盖）{name, content}
//	POST /api/docs/delete          删除 {name}
//
// 鉴权（docAuth，区别于 restAuth 的单一 admin Basic 账号）：
//   - Basic → hub.AuthClient（统一账号表）：任意有效账号可读
//     （role admin/client 皆可），写/删仅 admin；
//   - clientToken（Bearer/X-Access-Token/?token=）视作可读+可写
//     （与 restAuth 语义对齐：持共享令牌者可信）；
//   - 未配置任何账号与 token（纯内网部署）→ 直通。
package remote

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// docMaxBytes 单文档读写上限（2MB）。Markdown 文档远小于此；硬上限防御
// 异常请求把内存/磁盘打爆。
const docMaxBytes = 2 << 20

// docExts 允许的文档扩展名白名单（知识库定位是文档；禁二进制）。
var docExts = map[string]bool{".md": true, ".markdown": true, ".txt": true}

// errDocInvalidName 文件名非法错误（docResolve 校验失败统一返回，不回显原始输入）。
var errDocInvalidName = errors.New("文件名非法：仅允许 doc 目录下的 .md/.markdown/.txt 文档（可含子目录相对路径）")

// docItem 知识库列表条目。
type docItem struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
}

// SetDocsDir 配置知识库根目录（服务端部署目录下的 doc/）。
// 空串 = 未启用：/api/docs* 返回 404（保持旧行为）。
func (s *Server) SetDocsDir(dir string) {
	s.mu.Lock()
	s.docsDir = dir
	s.mu.Unlock()
}

// docAuth 知识库接口鉴权：读=任意统一账号/clientToken；写删=admin/clientToken。
// 与 restAuth 的差异：restAuth 只认启动参数注入的单一 admin Basic 凭据，
// 移动端普通账号（user）会被 401；知识库走 hub.AuthClient 完整账号表，
// 且 H5 页面只有登录用户才能进入（UI 层面已挡一层）。
func (s *Server) docAuth(write bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			docsDir, token := s.docsDir, s.restToken
			s.mu.Unlock()
			if docsDir == "" {
				writeJSONError(w, http.StatusNotFound, "知识库未启用（服务端未配置 doc 目录）")
				return
			}

			// clientToken 匹配 → 可读可写（与 restAuth 对齐）。
			if tok := docToken(r); tok != "" && token != "" &&
				subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			// 统一账号 Basic（完整账号表，任意角色可读）。
			u, p, ok := r.BasicAuth()
			if !ok || u == "" || p == "" {
				s.docUnauthorized(w, "需要登录凭据（Basic 认证）或访问令牌")
				return
			}
			role, _, ok := s.hub.AuthClient(u, p, "")
			if !ok {
				s.docUnauthorized(w, "账号或密码错误")
				return
			}
			if write && role != "admin" {
				writeJSONError(w, http.StatusForbidden, "仅管理员可修改知识库")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// docUnauthorized 401 响应（响应体脱敏，不含账号信息）。
func (s *Server) docUnauthorized(w http.ResponseWriter, msg string) {
	noStore(w)
	w.Header().Set("WWW-Authenticate", `Basic realm="kaelvoris-docs"`)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": msg})
}

// docToken 提取请求中的访问令牌（X-Access-Token / Bearer / ?token=，同 restAuth）。
func docToken(r *http.Request) string {
	if tok := r.Header.Get("X-Access-Token"); tok != "" {
		return tok
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// docResolve 校验 name 并返回其绝对路径。
// 允许 doc 根下的单层或多层相对路径（仅正斜杠分隔，如 "01-产品与需求/PRD.md"），
// 防目录穿越：拒绝绝对路径 / 盘符与冒号（NTFS 数据流）/ 反斜杠（跨平台混淆）/ 前导点；
// 逐组件校验（. / .. / 空组件拒绝），扩展名必须在白名单内，总长 ≤512。
func docResolve(dir, name string) (string, error) {
	if name == "" || len(name) > 512 ||
		strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) ||
		strings.Contains(name, ":") || strings.Contains(name, `\`) {
		return "", errDocInvalidName
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return "", errDocInvalidName
		}
	}
	if !docExts[strings.ToLower(filepath.Ext(name))] {
		return "", errDocInvalidName
	}
	p := filepath.Join(dir, filepath.FromSlash(name))
	// 兜底：Clean 后必须仍在 dir 内（防多层拼接绕过）。
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errDocInvalidName
	}
	return p, nil
}

// ---------- REST handlers ----------

// handleDocsList GET /api/docs：文档列表（名称/大小/修改时间，按名称排序）。
// 递归遍历 doc 根（含子目录），name 为相对根的正斜杠路径（如 "03-开发指南/development.md"），
// 与 docResolve 的写入/读取路径语义一致；隐藏文件与临时文件跳过。
func (s *Server) handleDocsList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	dir := s.docsDir
	s.mu.Unlock()
	docs := make([]docItem, 0, 64)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		// 目录不存在视为空知识库（首次部署 doc/ 尚未同步），不报错。
		writeJSONOK(w, docs)
		return
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir // 子目录不可读：跳过该目录
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".kaelvoris-tmp") {
			return nil
		}
		if !docExts[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		docs = append(docs, docItem{
			Name:    filepath.ToSlash(rel),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixMilli(),
		})
		return nil
	})
	sort.Slice(docs, func(i, j int) bool { return docs[i].Name < docs[j].Name })
	writeJSONOK(w, docs)
}

// handleDocsRead GET /api/docs/read?name=x.md：返回文档全文（纯文本）。
func (s *Server) handleDocsRead(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	dir := s.docsDir
	s.mu.Unlock()
	path, err := docResolve(dir, r.URL.Query().Get("name"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "文档不存在: "+filepath.Base(path))
		return
	}
	if len(b) > docMaxBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "文档过大")
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

// docWriteReq /api/docs/write 请求体。
type docWriteReq struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// handleDocsWrite POST /api/docs/write：创建/覆盖文档（仅 admin/clientToken）。
func (s *Server) handleDocsWrite(w http.ResponseWriter, r *http.Request) {
	var req docWriteReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, docMaxBytes+1024)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体无效或超过大小上限")
		return
	}
	s.mu.Lock()
	dir := s.docsDir
	s.mu.Unlock()
	path, err := docResolve(dir, req.Name)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 支持子目录路径写入：父目录不存在时逐级创建。
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "创建目录失败")
		return
	}
	if int64(len(req.Content)) > docMaxBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "文档内容超过 2MB 上限")
		return
	}
	if err := os.WriteFile(path, []byte(req.Content), 0o644); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "写入失败")
		return
	}
	writeJSONOK(w, map[string]interface{}{"name": req.Name, "bytes": len(req.Content)})
}

// handleDocsDelete POST /api/docs/delete：删除文档（仅 admin/clientToken）。
func (s *Server) handleDocsDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	s.mu.Lock()
	dir := s.docsDir
	s.mu.Unlock()
	path, err := docResolve(dir, req.Name)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			writeJSONError(w, http.StatusNotFound, "文档不存在: "+filepath.Base(path))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "删除失败")
		return
	}
	writeJSONOK(w, map[string]interface{}{"name": req.Name})
}
