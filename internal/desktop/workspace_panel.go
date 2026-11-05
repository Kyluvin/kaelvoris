// workspace_panel.go —— 工作区面板（Wails 绑定：任意目录文件管理 + 会话关联）。
//
// 用户需求 2026-12：左侧导航新增「工作区」入口。点击后：
//   - 上方选择工作区文件夹（PickWorkspaceDir，不动全局当前项目）；
//   - 下方展示该文件夹文件树；点文件右侧编辑（读/写/新建文件/新建文件夹/重命名/删除）；
//   - 工具条「执行」：把该文件夹绑定为任务工作区，关联会话（该工作区已有会话
//     则跳转复用，没有则新建），启动任务；任务结束后把工作内容总结回写该
//     项目根的 README.md（WS_PROJECT_LOG 区块，见 ws_tasks.go）。
//   - 新建会话时读取该工作区一级目录下的 README.md（Markdown），随任务文本
//     注入作为工作区说明，引擎据此理解项目背景。
//
// 与 doc_panel.go 的差异：doc 面板操作根固定为 workspace/doc；本面板操作根
// 是用户选择的任意目录（Config.WSPanelDir），安全约束相同（resolve 校验 +
// 符号链接拒绝 + 原子写 + 树深/节点数限制），实现方式对齐复用同一套惯例。
//
// 事件：变更后广播 kaelvoris:ws_changed（action/path），前端刷新文件树。
package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 常量与限制（对齐 doc_panel.go 的三重限制） ----------

const (
	wsReadLimit  = 2 << 20 // 单文件读取上限 2MB
	wsWriteLimit = 2 << 20 // 单文件写入上限 2MB
	wsTreeMax    = 2000    // 文件树节点总数上限
	wsTreeDepth  = 12      // 文件树展开深度上限
)

// wsOpMu 串行化工作区面板写操作（写/建/删/改名）。
var wsOpMu sync.Mutex

// ---------- 数据结构 ----------

// WSNode 工作区文件树节点（对齐 DocNode 字段形态，前端可复用渲染逻辑）。
type WSNode struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName,omitempty"`
	Path        string    `json:"path"` // 相对工作区根，统一正斜杠
	IsDir       bool      `json:"isDir"`
	Size        int64     `json:"size"`
	ModTime     string    `json:"modTime"`
	Children    []*WSNode `json:"children,omitempty"`
}

// WSTreeResult 目录树 + 根信息（exists=false 时前端引导选择目录）。
type WSTreeResult struct {
	Root   string    `json:"root"`   // 当前操作根（绝对路径；空 = 未选择）
	Exists bool      `json:"exists"` // 目录是否存在（已被删除等）
	Nodes  []*WSNode `json:"nodes"`
}

// WSFileContent 文件读取结果（对齐 DocFileContent；truncated=true 表示内容超过
// 读取上限被截断，前端据此禁用「编辑」——避免把截断后的不完整内容写回覆盖原文件）。
type WSFileContent struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	ModTime   string `json:"modTime"`
	Content   string `json:"content"`
	IsBinary  bool   `json:"isBinary"`
	Truncated bool   `json:"truncated,omitempty"`
}

// WSSaveRequest 保存编辑（path 相对工作区根；content 全文）。
type WSSaveRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WSCreateRequest 新建文件/目录（path 可含子目录）。
type WSCreateRequest struct {
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

// WSRenameRequest 重命名/移动。
type WSRenameRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ---------- 操作根 ----------

// wsPanelDir 面板当前操作根（记忆于 Config.WSPanelDir；空 = 未选择）。
func (a *App) wsPanelDir() string {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return a.cfg.WSPanelDir
}

// setWSPanelDir 记住面板操作根（仅 UI 状态，不触发内核重建）。
func (a *App) setWSPanelDir(dir string) {
	a.cfgMu.Lock()
	cfg := a.cfg
	cfg.WSPanelDir = dir
	a.cfg = cfg
	a.cfgMu.Unlock()
	_ = saveConfigFile(cfg)
}

// resolveWSPath 把前端相对路径安全解析为操作根内绝对路径。
// 防护点对齐 resolveDocPath：绝对路径 / 盘符冒号 / .. 逃逸 / 保留设备名。
func resolveWSPath(root, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("路径不能为空")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") {
		return "", errors.New("仅允许工作区内的相对路径")
	}
	if strings.ContainsRune(rel, ':') {
		return "", errors.New("路径不允许包含冒号")
	}
	cleaned := filepath.Clean(filepath.FromSlash(strings.ReplaceAll(rel, "\\", "/")))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("路径不允许跳出工作区")
	}
	for _, part := range strings.Split(filepath.ToSlash(cleaned), "/") {
		if part == "" || part == "." {
			continue
		}
		if err := validateDocName(part); err != nil { // 复用 doc 面板的名称校验（同 Windows 约束）
			return "", err
		}
	}
	abs := filepath.Join(root, cleaned)
	if !underRoot(root, abs) {
		return "", errors.New("路径不允许跳出工作区")
	}
	return abs, nil
}

// sniffHead 打开文件读前 512 字节（独立句柄，不推进调用方文件偏移）。
func sniffHead(path string, buf []byte) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.ReadFull(f, buf)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		return n, nil // 不足 512 字节按实际读到的算
	}
	return n, err
}

// checkWSSymlinks 沿路径逐级 Lstat 拒绝符号链接（对齐 checkDocSymlinks）。
func checkWSSymlinks(root, abs string) error {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return fmt.Errorf("路径越界: %w", err)
	}
	if rel == "." {
		return nil
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("无法访问 %s: %w", part, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("工作区内不允许使用符号链接")
		}
	}
	return nil
}

// wsRelPath 绝对路径 → 正斜杠相对路径。
func wsRelPath(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// ---------- 文件树 ----------

// wsTreeStats 遍历统计。
type wsTreeStats struct{ nodes int }

// buildWSTree 递归构建子树（目录优先 + 名称排序；深度/节点数上限内尽量多给）。
func buildWSTree(root, rel string, depth int, st *wsTreeStats) *WSNode {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Stat(abs)
	if err != nil {
		return nil
	}
	node := &WSNode{
		Name:        fi.Name(),
		DisplayName: docDisplayName(fi.Name()),
		Path:        filepath.ToSlash(rel),
		IsDir:       fi.IsDir(),
		Size:        fi.Size(),
		ModTime:     fi.ModTime().Format(time.RFC3339),
		Children:    []*WSNode{},
	}
	st.nodes++
	if !fi.IsDir() {
		return node
	}
	if depth >= wsTreeDepth || st.nodes >= wsTreeMax {
		return node
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return node
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	for _, e := range entries {
		if st.nodes >= wsTreeMax {
			break
		}
		name := e.Name()
		// 跳过隐藏文件/临时文件；node_modules 等重目录按前缀点规则已隐藏，
		// 其余目录照常展示（工作区面板是通用文件浏览，不只面向 Go 项目）。
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~$") || strings.HasSuffix(name, ".kaelvoris-tmp") {
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		childRel := name
		if rel != "." && rel != "" {
			childRel = rel + "/" + name
		}
		if child := buildWSTree(root, childRel, depth+1, st); child != nil {
			node.Children = append(node.Children, child)
		}
	}
	return node
}

// ---------- Wails 绑定 ----------

// SetWSPanelDir 选择/切换工作区面板操作目录（前端目录选择器回传绝对路径；
// 空串 = 清除记忆）。校验目录存在；不切换全局项目（面板目录与顶栏工作区解耦）。
func (a *App) SetWSPanelDir(dir string) error {
	p := strings.TrimSpace(dir)
	if p == "" {
		a.setWSPanelDir("")
		a.emitEvent("kaelvoris:ws_changed", map[string]string{"action": "root", "path": ""})
		return nil
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return fmt.Errorf("目录不存在: %s", p)
	}
	a.setWSPanelDir(p)
	a.emitEvent("kaelvoris:ws_changed", map[string]string{"action": "root", "path": p})
	return nil
}

// GetWSTree 工作区文件树（root 为空返回空树；目录已删除时 exists=false）。
func (a *App) GetWSTree() WSTreeResult {
	root := a.wsPanelDir()
	out := WSTreeResult{Root: root, Nodes: []*WSNode{}}
	if root == "" {
		return out
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return out
	}
	out.Exists = true
	st := &wsTreeStats{}
	if rn := buildWSTree(root, ".", 0, st); rn != nil {
		out.Nodes = rn.Children
	}
	return out
}

// ReadWSFile 读取工作区内文件（仅文本；二进制只回元信息；超限截断）。
func (a *App) ReadWSFile(rel string) (*WSFileContent, error) {
	root := a.wsPanelDir()
	if root == "" {
		return nil, errors.New("尚未选择工作区目录")
	}
	abs, err := resolveWSPath(root, rel)
	if err != nil {
		return nil, err
	}
	if err := checkWSSymlinks(root, abs); err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("文件不存在: %w", err)
	}
	if fi.IsDir() {
		return nil, errors.New("目标是目录，无法按文件读取")
	}
	out := &WSFileContent{
		Name:    fi.Name(),
		Size:    fi.Size(),
		ModTime: fi.ModTime().Format(time.RFC3339),
		Path:    wsRelPath(root, abs),
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()
	// 内容嗅探：前 512 字节含 NUL 视为二进制（对齐 isDocBinary 的兜底逻辑）。
	// 注意用独立句柄嗅探（对齐 doc_panel.isDocBinary）：若在 f 上先 Read 嗅探再
	// ReadAll，文件偏移已被推进 512 字节，小文件读回为空、大文件丢失前 512 字节。
	buf := make([]byte, 512)
	if nb, err := sniffHead(abs, buf); err == nil && nb > 0 && bytes.IndexByte(buf[:nb], 0) >= 0 {
		out.IsBinary = true
		return out, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(wsReadLimit)+1))
		if err != nil {
			return nil, fmt.Errorf("读取文件失败: %w", err)
		}
		if len(data) > wsReadLimit {
			data = data[:wsReadLimit]
			out.Truncated = true
		}
	out.Content = string(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}))
	return out, nil
}

// WriteWSFile 保存编辑（原子写：临时文件 + rename；上限 2MB）。
func (a *App) WriteWSFile(req WSSaveRequest) error {
	root := a.wsPanelDir()
	if root == "" {
		return errors.New("尚未选择工作区目录")
	}
	rel := strings.TrimSpace(req.Path)
	if rel == "" {
		return errors.New("文件路径不能为空")
	}
	if len(req.Content) > wsWriteLimit {
		return fmt.Errorf("内容超过大小限制（上限 %d MB）", wsWriteLimit>>20)
	}
	abs, err := resolveWSPath(root, rel)
	if err != nil {
		return err
	}
	wsOpMu.Lock()
	defer wsOpMu.Unlock()
	if err := checkWSSymlinks(root, abs); err != nil {
		return err
	}
	if fi, err := os.Stat(abs); err != nil {
		return fmt.Errorf("文件不存在（请先新建）: %w", err)
	} else if fi.IsDir() {
		return errors.New("目标是目录，无法写入")
	}
	tmp := abs + ".kaelvoris-tmp"
	if err := os.WriteFile(tmp, []byte(req.Content), 0o644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存失败: %w", err)
	}
	a.emitEvent("kaelvoris:ws_changed", map[string]string{"action": "write", "path": rel})
	return nil
}

// CreateWSFile 新建文件/目录（已存在报错；文件自动建父目录）。
func (a *App) CreateWSFile(req WSCreateRequest) error {
	root := a.wsPanelDir()
	if root == "" {
		return errors.New("尚未选择工作区目录")
	}
	rel := strings.TrimSpace(req.Path)
	if rel == "" {
		return errors.New("路径不能为空")
	}
	abs, err := resolveWSPath(root, rel)
	if err != nil {
		return err
	}
	wsOpMu.Lock()
	defer wsOpMu.Unlock()
	if err := checkWSSymlinks(root, filepath.Dir(abs)); err != nil {
		return err
	}
	if _, err := os.Lstat(abs); err == nil {
		return errors.New("同名文件或目录已存在")
	}
	if req.IsDir {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return fmt.Errorf("创建目录失败: %w", err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return fmt.Errorf("创建父目录失败: %w", err)
		}
		if err := os.WriteFile(abs, []byte{}, 0o644); err != nil {
			return fmt.Errorf("创建文件失败: %w", err)
		}
	}
	a.emitEvent("kaelvoris:ws_changed", map[string]string{"action": "create", "path": rel})
	return nil
}

// DeleteWSFile 删除文件/目录（目录递归；根目录本身不可删；删除前扫描软链）。
func (a *App) DeleteWSFile(rel string) error {
	root := a.wsPanelDir()
	if root == "" {
		return errors.New("尚未选择工作区目录")
	}
	abs, err := resolveWSPath(root, rel)
	if err != nil {
		return err
	}
	if samePath(abs, root) {
		return errors.New("不能删除工作区根目录")
	}
	wsOpMu.Lock()
	defer wsOpMu.Unlock()
	if err := checkWSSymlinks(root, abs); err != nil {
		return err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("目标不存在: %w", err)
	}
	if fi.IsDir() {
		var linkErr error
		_ = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				linkErr = errors.New("工作区内不允许使用符号链接，已中止删除")
				return filepath.SkipAll
			}
			return nil
		})
		if linkErr != nil {
			return linkErr
		}
		if err := os.RemoveAll(abs); err != nil {
			return fmt.Errorf("删除目录失败: %w", err)
		}
	} else {
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("删除文件失败: %w", err)
		}
	}
	a.emitEvent("kaelvoris:ws_changed", map[string]string{"action": "delete", "path": rel})
	return nil
}

// RenameWSFile 重命名/移动（源与目标均须在工作区根内）。
func (a *App) RenameWSFile(req WSRenameRequest) error {
	root := a.wsPanelDir()
	if root == "" {
		return errors.New("尚未选择工作区目录")
	}
	from := strings.TrimSpace(req.From)
	to := strings.TrimSpace(req.To)
	if from == "" || to == "" {
		return errors.New("原路径与目标路径均不能为空")
	}
	absFrom, err := resolveWSPath(root, from)
	if err != nil {
		return err
	}
	absTo, err := resolveWSPath(root, to)
	if err != nil {
		return err
	}
	if samePath(absFrom, root) {
		return errors.New("不能重命名工作区根目录")
	}
	if samePath(absFrom, absTo) {
		return nil
	}
	wsOpMu.Lock()
	defer wsOpMu.Unlock()
	if err := checkWSSymlinks(root, absFrom); err != nil {
		return err
	}
	fi, err := os.Lstat(absFrom)
	if err != nil {
		return fmt.Errorf("原文件不存在: %w", err)
	}
	if fi.IsDir() && underRoot(absFrom, absTo) {
		return errors.New("不能把目录移动到它自己内部")
	}
	if err := checkWSSymlinks(root, absTo); err != nil {
		return err
	}
	if _, err := os.Lstat(absTo); err == nil {
		return errors.New("目标位置已存在同名文件或目录")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查目标失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absTo), 0o755); err != nil {
		return fmt.Errorf("创建目标父目录失败: %w", err)
	}
	if err := os.Rename(absFrom, absTo); err != nil {
		return fmt.Errorf("重命名失败: %w", err)
	}
	a.emitEvent("kaelvoris:ws_changed", map[string]string{"action": "rename", "path": to})
	return nil
}
