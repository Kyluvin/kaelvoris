// doc_panel.go —— 文档面板（Wails 绑定：doc 目录文件管理）
//
// 职责（对齐其它 *_panel.go 的拆分惯例）：
//   - 把工作区 doc/ 目录以文件树形式暴露给前端「文档」面板；
//   - 提供查看/编辑/新建/删除/重命名等文件 CRUD 绑定；
//   - 变更后广播 kaelvoris:doc_changed 事件，前端据此刷新文件树。
//
// 安全边界（本面板的硬约束，所有操作仅限 doc 目录内）：
//  1. 操作根固定为 workspace/doc（docRoot）；workspace 未配置时回退到可执行文件
//     所在目录下的 doc 子目录（对齐其它面板"降级不阻断启动"的语义）；
//  2. resolveDocPath 统一做斜杠归一 + Clean + underRoot 校验：拒绝绝对路径、
//     盘符/冒号（NTFS 数据流）与 .. 逃逸；
//  3. checkDocSymlinks 沿路径逐级 Lstat，禁止借符号链接 / NTFS junction 跳出 doc 根；
//  4. 读取/写入限制单文件大小，文件树限制深度与节点总数，防止超大目录拖垮 UI；
//  5. 所有写操作经 docOpMu 串行化；写入采用临时文件 + rename 原子替换；
//     递归删除前先扫描目录内部符号链接，防止越权扩大删除范围。
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

// ---------- 常量与限制 ----------

const (
	docReadLimit  = 2 << 20 // 单文件读取上限 2MB，超出截断并在结果中标记
	docWriteLimit = 2 << 20 // 单文件写入上限 2MB
	docTreeMax    = 2000    // 文件树节点总数上限
	docTreeDepth  = 12      // 文件树展开深度上限
	docTreeBudget = 8 << 20 // 遍历字节预算 8MB，超出后不再展开更深的目录
)

var (
	// docOpMu 串行化文档写操作（写/建/删/改名），避免并发写同一文件或边删边读
	docOpMu sync.Mutex

	// docReservedNames：Windows 保留设备名（不区分大小写），禁止用作文件/目录名
	docReservedNames = map[string]bool{
		"con": true, "prn": true, "aux": true, "nul": true,
		"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
		"com6": true, "com7": true, "com8": true, "com9": true,
		"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
		"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
	}

	// docBinaryExts：视为二进制的扩展名（面板中仅提示，不进入编辑器）
	docBinaryExts = map[string]bool{
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
		".ico": true, ".bmp": true, ".pdf": true,
		".zip": true, ".gz": true, ".tgz": true, ".rar": true, ".7z": true,
		".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true,
		".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
		".mp3": true, ".mp4": true, ".wav": true, ".avi": true, ".mov": true, ".webm": true,
		".xlsx": true, ".docx": true, ".pptx": true, ".xls": true, ".doc": true, ".ppt": true,
	}
)

// ---------- 数据结构 ----------

// DocNode —— 文件树节点（目录或文件）
type DocNode struct {
	Name        string     `json:"name"`                  // 真实文件名（操作时使用）
	DisplayName string     `json:"displayName,omitempty"` // 展示名（.md 去后缀，更友好）
	Path        string     `json:"path"`                  // 相对 doc 根的路径（统一正斜杠）
	IsDir       bool       `json:"isDir"`
	Size        int64      `json:"size"`
	ModTime     string     `json:"modTime"`            // RFC3339
	Children    []*DocNode `json:"children,omitempty"` // 仅目录有
}

// DocInfo —— doc 目录概况（面板头部展示）
type DocInfo struct {
	Root       string `json:"root"`       // doc 根目录绝对路径
	Exists     bool   `json:"exists"`     // doc 目录是否存在（不存在时前端引导创建）
	FileCount  int    `json:"fileCount"`  // 文件数
	DirCount   int    `json:"dirCount"`   // 子目录数（不含根）
	TotalBytes int64  `json:"totalBytes"` // 总大小
}

// DocFileContent —— 文件读取结果
type DocFileContent struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	ModTime   string `json:"modTime"`
	Content   string `json:"content"`   // 文本内容（二进制文件为空）
	IsBinary  bool   `json:"isBinary"`  // 二进制文件：仅提示，不进编辑器
	Truncated bool   `json:"truncated"` // 超过 docReadLimit 被截断
}

// DocWriteRequest —— 保存编辑内容
type DocWriteRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// DocCreateRequest —— 新建文件/目录
type DocCreateRequest struct {
	Path  string `json:"path"` // 相对 doc 根，可含子目录
	IsDir bool   `json:"isDir"`
}

// DocRenameRequest —— 重命名/移动（源与目标均须在 doc 根内）
type DocRenameRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// docTreeStats —— buildDocTree 的遍历统计（节点数/字节预算用）
type docTreeStats struct {
	files int
	dirs  int
	bytes int64
	nodes int
}

// ---------- 路径安全 ----------

// docRoot 返回文档面板的操作根目录：workspace/doc。
// workspace 未配置时回退到可执行文件所在目录的 doc 子目录（降级不阻断启动）。
func (a *App) docRoot() string {
	if ws := strings.TrimSpace(a.cfgWorkspace()); ws != "" {
		return filepath.Join(ws, "doc")
	}
	exe, err := os.Executable()
	if err != nil {
		return "doc"
	}
	return filepath.Join(filepath.Dir(exe), "doc")
}

// samePath：路径等价比较（Windows 文件系统大小写不敏感，统一按忽略大小写处理）
func samePath(x, y string) bool {
	return strings.EqualFold(filepath.Clean(x), filepath.Clean(y))
}

// underRoot 校验 p（应为 Clean 过的绝对路径）是否位于 root 内（允许等于 root）。
func underRoot(root, p string) bool {
	if samePath(root, p) {
		return true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveDocPath 把前端传来的相对路径安全解析为 doc 根内的绝对路径。
// 防护点：绝对路径 / 盘符与冒号（NTFS 数据流）/ 反斜杠混用 / .. 逃逸 / 非法组件名。
func (a *App) resolveDocPath(rel string) (string, error) {
	root := a.docRoot()
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("路径不能为空")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") {
		return "", errors.New("仅允许 doc 目录内的相对路径")
	}
	if strings.ContainsRune(rel, ':') {
		// 拦截盘符（C:）与 NTFS 备用数据流（file.txt:stream）
		return "", errors.New("路径不允许包含冒号")
	}
	cleaned := filepath.Clean(filepath.FromSlash(strings.ReplaceAll(rel, "\\", "/")))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("路径不允许跳出 doc 目录")
	}
	// 逐组件复检：拦住 Windows 会静默改写的名字（结尾点/空格）与保留设备名，
	// 避免 "foo. " 之类路径在 Win32 层被解析成 "foo" 造成越权别名。
	for _, part := range strings.Split(filepath.ToSlash(cleaned), "/") {
		if part == "" || part == "." {
			continue
		}
		if err := validateDocName(part); err != nil {
			return "", err
		}
	}
	abs := filepath.Join(root, cleaned)
	if !underRoot(root, abs) {
		return "", errors.New("路径不允许跳出 doc 目录")
	}
	return abs, nil
}

// checkDocSymlinks 从 root 向下逐级 Lstat：路径任何一段是符号链接即拒绝，
// 防止 doc 内的软链/junction 指向根目录之外。目标不存在时（如新建前检查）放行。
func checkDocSymlinks(root, abs string) error {
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
				return nil // 中间组件尚不存在，交由后续创建逻辑处理
			}
			return fmt.Errorf("无法访问 %s: %w", part, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("doc 目录内不允许使用符号链接")
		}
	}
	return nil
}

// docRelPath 把 doc 内绝对路径转回正斜杠相对路径（事件与前端统一使用相对路径）。
func docRelPath(root, abs string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// ---------- 名称与内容校验 ----------

// validateDocName 校验单个文件/目录名：非法字符、保留设备名、结尾点/空格、长度。
func validateDocName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("名称不能为空")
	}
	if name == "." || name == ".." {
		return errors.New("名称不能是 . 或 ..")
	}
	if strings.ContainsAny(name, `\/:*?"<>|`) {
		return errors.New(`名称不能包含 \ / : * ? " < > | 等字符`)
	}
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errors.New("名称不能包含控制字符")
	}
	if len(name) > 120 {
		return errors.New("名称过长（上限 120 字符）")
	}
	lower := strings.ToLower(name)
	if docReservedNames[lower] {
		return fmt.Errorf("名称 %s 是 Windows 保留设备名", name)
	}
	// 旧版 Windows 对 CON.txt 这类"保留名+扩展名"同样受限，统一拒绝更稳妥
	if i := strings.IndexByte(lower, '.'); i > 0 && docReservedNames[lower[:i]] {
		return fmt.Errorf("名称 %s 含 Windows 保留设备名", name)
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return errors.New("名称不能以点或空格结尾")
	}
	return nil
}

// isDocBinaryExt：按扩展名判断二进制
func isDocBinaryExt(name string) bool {
	return docBinaryExts[strings.ToLower(filepath.Ext(name))]
}

// isDocBinary：扩展名判定 + 内容嗅探兜底（前 512 字节含 NUL 即视为二进制）
func isDocBinary(path string, fi os.FileInfo) bool {
	if isDocBinaryExt(fi.Name()) {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false // 打不开就按文本处理，让读取流程去报真实错误
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return n > 0 && bytes.IndexByte(buf[:n], 0) >= 0
}

// docDisplayName：文件树中的友好展示名（.md 去掉后缀，其余原样）
func docDisplayName(name string) string {
	if ext := filepath.Ext(name); strings.EqualFold(ext, ".md") && len(name) > len(ext) {
		return name[:len(name)-len(ext)]
	}
	return name
}

// ---------- 文件树 ----------

// buildDocTree 递归构建以 root 为根、rel 为相对位置的子树。
// 受 docTreeDepth / docTreeMax / docTreeBudget 三重限制，超限即停止展开（尽量多给，不报错）。
func buildDocTree(root, rel string, depth int, st *docTreeStats) *DocNode {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Stat(abs)
	if err != nil {
		return nil
	}
	node := &DocNode{
		Name:        fi.Name(),
		DisplayName: docDisplayName(fi.Name()),
		Path:        filepath.ToSlash(rel),
		IsDir:       fi.IsDir(),
		Size:        fi.Size(),
		ModTime:     fi.ModTime().Format(time.RFC3339),
		Children:    []*DocNode{},
	}
	st.nodes++
	if !fi.IsDir() {
		st.files++
		st.bytes += fi.Size()
		return node
	}
	st.dirs++
	if depth >= docTreeDepth || st.nodes >= docTreeMax || st.bytes >= docTreeBudget {
		return node // 达到深度/节点数/字节预算上限：目录保留但不再展开
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return node
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir() // 目录优先
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	for _, e := range entries {
		if st.nodes >= docTreeMax {
			break
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~$") || strings.HasSuffix(name, ".kaelvoris-tmp") {
			continue // 跳过隐藏文件 / Office 临时文件 / 本面板写盘临时文件
		}
		if e.Type()&fs.ModeSymlink != 0 {
			continue // 符号链接不进树（安全边界：禁止借软链逃逸）
		}
		childRel := name
		if rel != "." && rel != "" {
			childRel = rel + "/" + name
		}
		child := buildDocTree(root, childRel, depth+1, st)
		if child != nil {
			node.Children = append(node.Children, child)
		}
	}
	return node
}

// ---------- 事件 ----------

// emitDocChanged：文档变更后广播事件（action: write/create/delete/rename），前端刷新树与编辑器
func (a *App) emitDocChanged(action, relPath string) {
	a.emitEvent("kaelvoris:doc_changed", map[string]string{"action": action, "path": relPath})
}

// ---------- Wails 绑定 ----------

// GetDocInfo —— doc 目录概况（根路径、是否存在、文件/目录数、总大小）。
func (a *App) GetDocInfo() DocInfo {
	root := a.docRoot()
	info := DocInfo{Root: root}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return info // Exists=false：前端提示可一键创建 doc 目录
	}
	info.Exists = true
	st := &docTreeStats{}
	buildDocTree(root, ".", 0, st)
	info.FileCount = st.files
	info.TotalBytes = st.bytes
	info.DirCount = st.dirs
	if info.DirCount > 0 {
		info.DirCount-- // buildDocTree 会把根目录自身计入，这里扣除
	}
	return info
}

// ListDocTree —— 返回 doc 目录文件树（顶层节点列表；目录尚未创建时返回空树）。
func (a *App) ListDocTree() ([]*DocNode, error) {
	root := a.docRoot()
	fi, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []*DocNode{}, nil // doc 目录尚未创建：空树，前端引导创建
		}
		return nil, fmt.Errorf("读取文档目录失败: %w", err)
	}
	if !fi.IsDir() {
		return nil, errors.New("doc 路径不是目录")
	}
	st := &docTreeStats{}
	rootNode := buildDocTree(root, ".", 0, st)
	if rootNode == nil {
		return []*DocNode{}, nil
	}
	return rootNode.Children, nil
}

// ReadDocFile —— 读取 doc 内文件（仅文本；二进制只回元信息；超限截断）。
func (a *App) ReadDocFile(rel string) (*DocFileContent, error) {
	root := a.docRoot()
	abs, err := a.resolveDocPath(rel)
	if err != nil {
		return nil, err
	}
	if err := checkDocSymlinks(root, abs); err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("文件不存在: %w", err)
	}
	if fi.IsDir() {
		return nil, errors.New("目标是目录，无法按文件读取")
	}
	out := &DocFileContent{
		Name:    fi.Name(),
		Size:    fi.Size(),
		ModTime: fi.ModTime().Format(time.RFC3339),
	}
	if r, err := docRelPath(root, abs); err == nil {
		out.Path = r
	}
	if isDocBinary(abs, fi) {
		out.IsBinary = true
		return out, nil
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(docReadLimit)+1))
	if err != nil {
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}
	if len(data) > docReadLimit {
		data = data[:docReadLimit]
		out.Truncated = true
	}
	// 容忍 UTF-8 BOM（对齐项目内 jsonReadFileBOM 的兼容习惯）
	out.Content = string(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}))
	return out, nil
}

// WriteDocFile —— 保存对 doc 内文件的修改（原子写：临时文件 + rename 替换）。
func (a *App) WriteDocFile(req DocWriteRequest) error {
	rel := strings.TrimSpace(req.Path)
	if rel == "" {
		return errors.New("文件路径不能为空")
	}
	if len(req.Content) > docWriteLimit {
		return fmt.Errorf("内容超过大小限制（上限 %d MB）", docWriteLimit>>20)
	}
	root := a.docRoot()
	abs, err := a.resolveDocPath(rel)
	if err != nil {
		return err
	}
	docOpMu.Lock()
	defer docOpMu.Unlock()
	if err := checkDocSymlinks(root, abs); err != nil {
		return err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("文件不存在（请先新建）: %w", err)
	}
	if fi.IsDir() {
		return errors.New("目标是目录，无法写入")
	}
	// 先写临时文件再原子替换，避免写一半崩溃损坏原文档
	tmp := abs + ".kaelvoris-tmp"
	if err := os.WriteFile(tmp, []byte(req.Content), 0o644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存失败: %w", err)
	}
	relOut, _ := docRelPath(root, abs)
	a.emitDocChanged("write", relOut)
	return nil
}

// CreateDocFile —— 在 doc 目录内新建文件或目录（路径逐级校验；已存在则报错）。
func (a *App) CreateDocFile(req DocCreateRequest) error {
	rel := strings.TrimSpace(req.Path)
	if rel == "" {
		return errors.New("路径不能为空")
	}
	root := a.docRoot()
	abs, err := a.resolveDocPath(rel)
	if err != nil {
		return err
	}
	docOpMu.Lock()
	defer docOpMu.Unlock()
	if err := checkDocSymlinks(root, filepath.Dir(abs)); err != nil {
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
	relOut, _ := docRelPath(root, abs)
	a.emitDocChanged("create", relOut)
	return nil
}

// DeleteDocFile —— 删除 doc 内文件或目录（目录递归删除；doc 根本身不可删）。
func (a *App) DeleteDocFile(rel string) error {
	root := a.docRoot()
	abs, err := a.resolveDocPath(rel)
	if err != nil {
		return err
	}
	if samePath(abs, root) {
		return errors.New("不能删除 doc 根目录")
	}
	docOpMu.Lock()
	defer docOpMu.Unlock()
	if err := checkDocSymlinks(root, abs); err != nil {
		return err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("目标不存在: %w", err)
	}
	if fi.IsDir() {
		// 递归删除前先扫描目录内部：防止软链把删除范围放大到 doc 之外
		var linkErr error
		_ = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				linkErr = errors.New("doc 目录内不允许使用符号链接，已中止删除")
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
	relOut, _ := docRelPath(root, abs)
	a.emitDocChanged("delete", relOut)
	return nil
}

// RenameDocFile —— 重命名/移动 doc 内文件或目录（源与目标都必须仍在 doc 根内）。
func (a *App) RenameDocFile(req DocRenameRequest) error {
	from := strings.TrimSpace(req.From)
	to := strings.TrimSpace(req.To)
	if from == "" || to == "" {
		return errors.New("原路径与目标路径均不能为空")
	}
	root := a.docRoot()
	absFrom, err := a.resolveDocPath(from)
	if err != nil {
		return err
	}
	absTo, err := a.resolveDocPath(to)
	if err != nil {
		return err
	}
	if samePath(absFrom, root) {
		return errors.New("不能重命名 doc 根目录")
	}
	if samePath(absFrom, absTo) {
		return nil // 同名重复提交：幂等成功
	}
	docOpMu.Lock()
	defer docOpMu.Unlock()
	if err := checkDocSymlinks(root, absFrom); err != nil {
		return err
	}
	fi, err := os.Lstat(absFrom)
	if err != nil {
		return fmt.Errorf("原文件不存在: %w", err)
	}
	if fi.IsDir() && underRoot(absFrom, absTo) {
		return errors.New("不能把目录移动到它自己内部")
	}
	if err := checkDocSymlinks(root, absTo); err != nil {
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
	relOut, _ := docRelPath(root, absTo)
	a.emitDocChanged("rename", relOut)
	return nil
}
