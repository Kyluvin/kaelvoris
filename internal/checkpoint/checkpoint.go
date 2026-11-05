// Package checkpoint 任务检查点与回滚（B1，对标 Cline Checkpoints 的零 git 简版）。
//
// Cline 语义：每步文件修改自动快照，可一键 Restore。Kaelvoris 简化对齐：
//   - 仅记录写类工具（fs_write / fs_apply_diff）触达文件的「修改前内容」；
//   - 存储 ~/.kaelvoris/checkpoints/<sessionID>/：index.jsonl 行式索引 +
//     blobs/<seq>-<hash8>.snap 内容分片（零 git 依赖，纯文件实现）；
//   - 恢复语义：Restore(id) 先把当前内容记为新检查点（tool="restore"，
//     恢复本身可回滚），再按记录写回 / 删除新建文件。
//
// 非目标：不做 git shadow repo（Cline 原版形态）；不做目录递归快照；
// 超大文件（>2MB）跳过内容仅留痕。检查点永不阻塞主流程：Record 失败仅返回 err。
package checkpoint

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxFileBytes 单文件快照上限（超过则内容留空仅记录路径，恢复时跳过）。
const MaxFileBytes = 2 << 20 // 2MB

// Entry 单文件快照记录（index.jsonl 的一行）。
type Entry struct {
	ID     string `json:"id"`               // 检查点 ID（随机 hex）
	Seq    int    `json:"seq"`              // 会话内序号（1 起）
	Time   int64  `json:"time"`             // unix 毫秒
	Tool   string `json:"tool"`             // 触发工具（fs_write/fs_apply_diff/restore）
	Path   string `json:"path"`             // 规范化相对路径（正斜杠）
	Blob   string `json:"blob,omitempty"`   // 内容分片文件名（内容留空时为空）
	IsNew  bool   `json:"isNew,omitempty"`  // 记录时文件不存在（新建场景，恢复=删除）
	Toobig bool   `json:"tooBig,omitempty"` // 超大文件仅留痕
}

// Store 单会话检查点存储。
type Store struct {
	dir       string // <base>/<sessionID>
	root      string // 恢复时的文件根目录（workspace）
	maxBytes  int64  // 会话限额（blob 总量）
	mu        sync.Mutex
	seq       int
	lastEntry *Entry // 最近一次记录（任务收尾摘要用）
}

// NewStore 创建存储（baseDir 通常为 ~/.kaelvoris/checkpoints；sessionID 为会话 ID）。
// 目录惰性创建（首次 Record 时）。
func NewStore(baseDir, sessionID string, maxSessionBytes int64) *Store {
	if strings.TrimSpace(sessionID) == "" {
		sessionID = "default"
	}
	if maxSessionBytes <= 0 {
		maxSessionBytes = 200 << 20 // 200MB
	}
	return &Store{
		dir:      filepath_join(baseDir, sessionID),
		maxBytes: maxSessionBytes,
	}
}

// filepath_join 兼容包装（避免直接依赖 path/filepath 的语义噪声——实际就是 filepath.Join）。
func filepath_join(a, b string) string { return filepath.Join(a, b) }

// Record 记录一次写前快照。oldContent 为修改前完整内容（调用方已读取）；
// isNew = 文件此前不存在。返回记录条目；失败（磁盘等）返回 err，调用方静默降级。
func (s *Store) Record(tool, relPath, oldContent string, isNew bool) (Entry, error) {
	if s == nil || strings.TrimSpace(relPath) == "" {
		return Entry{}, fmt.Errorf("checkpoint: invalid record args")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(s.dir, "blobs"), 0o755); err != nil {
		return Entry{}, err
	}
	s.seq++
	e := Entry{
		ID:    newID(),
		Seq:   s.seq,
		Time:  nowMillis(),
		Tool:  tool,
		Path:  norm(relPath),
		IsNew: isNew,
	}
	// 超大文件仅留痕。
	if len(oldContent) > MaxFileBytes {
		e.Toobig = true
	} else if isNew {
		e.Blob = "" // 新建文件：无旧内容，恢复 = 删除
	} else {
		name := fmt.Sprintf("%06d-%s.snap", e.Seq, hash8(oldContent))
		if err := os.WriteFile(filepath.Join(s.dir, "blobs", name), []byte(oldContent), 0o644); err != nil {
			s.seq--
			return Entry{}, err
		}
		e.Blob = name
	}
	if err := appendIndex(s.dir, e); err != nil {
		return Entry{}, err
	}
	s.lastEntry = &e
	s.enforceQuota()
	return e, nil
}

// Restore 按 ID 恢复：先把当前磁盘内容记为 tool="restore" 的新检查点（可回滚），
// 再写回记录内容（IsNew → 删除文件）。返回恢复的条目。
func (s *Store) Restore(id string) (Entry, error) {
	if s == nil {
		return Entry{}, fmt.Errorf("checkpoint: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := readIndex(s.dir)
	if err != nil {
		return Entry{}, err
	}
	var target *Entry
	for i := range entries {
		if entries[i].ID == id {
			target = &entries[i]
			break
		}
	}
	if target == nil {
		return Entry{}, fmt.Errorf("checkpoint: id %q not found", id)
	}
	if target.Toobig {
		return *target, fmt.Errorf("checkpoint: %s was too large to snapshot, cannot restore", target.Path)
	}
	// 1) 恢复前快照当前内容（可回滚保障）。
	abs := s.absOf(target.Path)
	cur, curErr := os.ReadFile(abs)
	switch {
	case curErr == nil && !target.IsNew:
		if _, rerr := s.recordLocked("restore", target.Path, string(cur), false); rerr != nil {
			return *target, rerr
		}
	case curErr == nil && target.IsNew:
		// 新建文件场景，文件又被改过：先记当前内容再删（保守）。
		if _, rerr := s.recordLocked("restore", target.Path, string(cur), false); rerr != nil {
			return *target, rerr
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return *target, err
		}
		return *target, nil
	case os.IsNotExist(curErr) && target.IsNew:
		// 已是新建前状态，幂等成功。
		return *target, nil
	case os.IsNotExist(curErr):
		// 记录时文件存在、现已不在：无法凭快照重建目录结构之外的语义，
		// 但旧内容仍在 blob 中——恢复 = 写回（目录若被删则重建父目录）。
	}
	if target.IsNew {
		// cur 读不到且 isNew 已在上方处理；防御性再判。
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return *target, err
		}
		return *target, nil
	}
	// 2) 写回旧内容。
	content, err := s.blobOf(target)
	if err != nil {
		return *target, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return *target, err
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return *target, err
	}
	return *target, nil
}

// record 无锁记录（Restore 内部持锁路径使用）。与 Record 相同逻辑但假定已持锁。
func (s *Store) recordLocked(tool, relPath, oldContent string, isNew bool) (Entry, error) {
	s.seq++
	e := Entry{
		ID:    newID(),
		Seq:   s.seq,
		Time:  nowMillis(),
		Tool:  tool,
		Path:  norm(relPath),
		IsNew: isNew,
	}
	if len(oldContent) > MaxFileBytes {
		e.Toobig = true
	} else if !isNew {
		name := fmt.Sprintf("%06d-%s.snap", e.Seq, hash8(oldContent))
		if err := os.WriteFile(filepath.Join(s.dir, "blobs", name), []byte(oldContent), 0o644); err != nil {
			s.seq--
			return Entry{}, err
		}
		e.Blob = name
	}
	if err := appendIndex(s.dir, e); err != nil {
		return Entry{}, err
	}
	s.lastEntry = &e
	return e, nil
}

// List 全部检查点（按 Seq 升序）。
func (s *Store) List() []Entry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := readIndex(s.dir)
	if err != nil {
		return nil
	}
	return entries
}

// Summary 任务收尾摘要（无记录返回 ""）。
func (s *Store) Summary() string {
	if s == nil {
		return ""
	}
	entries := s.List()
	if len(entries) == 0 {
		return ""
	}
	news := 0
	paths := map[string]bool{}
	for _, e := range entries {
		if e.IsNew && e.Tool != "restore" {
			paths[e.Path] = true
		} else if e.Tool != "restore" {
			paths[e.Path] = true
		}
		if e.IsNew {
			news++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "已记录 %d 个检查点（覆盖 %d 个文件，其中新建 %d 个）；支持按检查点回滚。", len(entries), len(paths), news)
	if s.lastEntry != nil {
		fmt.Fprintf(&b, "最近快照：#%d %s", s.lastEntry.Seq, s.lastEntry.Path)
	}
	return b.String()
}

// enforceLimit 会话 blob 总量超限时从最旧 blob 删除（index 保留条目但清空 blob 引用）。
func (s *Store) enforceQuota() {
	s.enforceLimit()
}

// absOf 把规范化相对路径解析为绝对路径（root 为 workspace 根，宿主注入）。
func (s *Store) absOf(rel string) string {
	root := s.root
	if root == "" {
		root = filepath.Dir(s.dir)
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

// SetWorkspaceRoot 设置恢复时的文件根目录（宿主在创建 Store 后调用；
// 空 = 沿用 base 父目录）。
func (s *Store) SetWorkspaceRoot(root string) {
	if s != nil && strings.TrimSpace(root) != "" {
		s.root = root
	}
}

// enforceLimit 会话 blob 总量超限时从最旧 blob 删除（index 保留条目但清空 blob 引用）。
func (s *Store) enforceLimit() {
	entries, err := readIndex(s.dir)
	if err != nil {
		return
	}
	var total int64
	for _, e := range entries {
		if e.Blob == "" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(s.dir, "blobs", e.Blob)); err == nil {
			total += fi.Size()
		}
	}
	if total <= s.maxBytes {
		return
	}
	for _, e := range entries { // 从最旧开始删（index.jsonl 本身按序追加）
		if total <= s.maxBytes {
			break
		}
		if e.Blob == "" {
			continue
		}
		p := filepath.Join(s.dir, "blobs", e.Blob)
		if fi, err := os.Stat(p); err == nil {
			total -= fi.Size()
		}
		_ = os.Remove(p)
		rewriteIndexWithoutBlob(s.dir, e.ID)
	}
}

// blobOf 读取条目 blob 内容。
func (s *Store) blobOf(e *Entry) (string, error) {
	if e.Blob == "" {
		return "", nil
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "blobs", e.Blob))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---- 存储细节 ----

func index_path(dir string) string { return filepath.Join(dir, "index.jsonl") }
func appendIndex(dir string, e Entry) error {
	f, err := os.OpenFile(index_path(dir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func readIndex(dir string) ([]Entry, error) {
	b, err := os.ReadFile(index_path(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // 损坏行跳过（追加写崩溃安全）
		}
		out = append(out, e)
	}
	return out, nil
}

// rewriteIndexWithoutBlob 把指定 ID 条目的 Blob 字段清空后重写索引（限额清理用）。
func rewriteIndexWithoutBlob(dir, id string) {
	entries, err := readIndex(dir)
	if err != nil {
		return
	}
	var sb strings.Builder
	for _, e := range entries {
		if e.ID == id {
			e.Blob = ""
		}
		if b, err := json.Marshal(e); err == nil {
			sb.Write(b)
			sb.WriteByte('\n')
		}
	}
	_ = os.WriteFile(index_path(dir), []byte(sb.String()), 0o644)
}

func norm(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	// 剥离盘符/绝对路径前缀（workspace 由宿主保证）
	if i := strings.LastIndex(p, ":/"); i >= 0 {
		p = p[i+2:]
	}
	for strings.HasPrefix(p, "/") {
		p = p[1:]
	}
	// 剥离 ../ 前缀（防越界，宿主已校验边界，此处仅规范化展示）
	for strings.HasPrefix(p, "../") {
		p = p[3:]
	}
	return p
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hash8(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return strconv.FormatUint(h.Sum64(), 36)
}

func nowMillis() int64 { return time.Now().UnixMilli() }
