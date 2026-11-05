// Worktree 领域层：隔离草稿 + 生命周期。
//
// 移植自 dsh-univer-office 的 worktree 语义（src/host/provider/worktree-operations.ts
// 与 gateway collab-service）：
//   - 每个文档有 trunk（主线）与若干 worktree（隔离草稿）；
//   - 草稿状态机：draft → ready → merged|discarded（终态不可复用）；
//   - ready 后拒绝写入，需 reopen 才能继续修改；
//   - merged/discarded 为终态，只能新建 worktree；
//   - merge/discard 属于用户审阅动作，模型不应擅自调用。
//
// 实现采用纯文件副本（不依赖 git）：草稿文件存于
// <workspace>/.goffice/<docKey>/worktrees/<wtId>/draft.<ext>，
// merge 时草稿覆盖 trunk 文件，discard 时删除草稿目录。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// worktreeStatus 草稿状态。
type worktreeStatus string

const (
	statusDraft     worktreeStatus = "draft"
	statusReady     worktreeStatus = "ready"
	statusMerged    worktreeStatus = "merged"
	statusDiscarded worktreeStatus = "discarded"
)

// worktreeMeta 草稿元数据（落盘 <wtId>/meta.json）。
type worktreeMeta struct {
	ID        string         `json:"id"`
	Name      string         `json:"name,omitempty"`
	Status    worktreeStatus `json:"status"`
	Ext       string         `json:"ext"` // 文档扩展名（.xlsx/.docx/.pptx/.csv）
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	Units     []string       `json:"units,omitempty"` // 内容分区名（sheet 页名/slide 页 id）
}

// wtStore worktree 存储（一个插件进程内共享）。
type wtStore struct {
	root string // <workspace>/.goffice
}

// newWTStore 创建 store（root 为插件数据根）。
func newWTStore(workspace string) *wtStore {
	return &wtStore{root: filepath.Join(workspace, ".goffice")}
}

// docKey 文档相对路径 → 稳定 key（路径安全化：分隔符替换 + 摘要防超长）。
func docKey(rel string) string {
	clean := filepath.ToSlash(rel)
	clean = strings.TrimPrefix(clean, "./")
	// 目录分隔符替换为下划线，避免嵌套目录；超过 64 字符截断并加哈希。
	name := strings.ReplaceAll(clean, "/", "__")
	key := name
	if len(key) > 64 {
		sum := sha256.Sum256([]byte(clean))
		key = key[:40] + "-" + hex.EncodeToString(sum[:6])
	}
	return key
}

// worktreeDir 返回草稿目录。
func (s *wtStore) worktreeDir(rel, wtID string) string {
	return filepath.Join(s.root, docKey(rel), "worktrees", wtID)
}

// metaPath 草稿 meta 路径。
func (s *wtStore) metaPath(rel, wtID string) string {
	return filepath.Join(s.worktreeDir(rel, wtID), "meta.json")
}

// draftPath 草稿内容文件路径。
func (s *wtStore) draftPath(rel, wtID string) string {
	return filepath.Join(s.worktreeDir(rel, wtID), "draft"+mimeExt(rel))
}

// mimeExt 按文档路径推断扩展名（.xlsx/.docx/.pptx/.csv/.tsv）。
func mimeExt(rel string) string {
	e := strings.ToLower(filepath.Ext(rel))
	switch e {
	case ".xlsx", ".docx", ".pptx", ".csv", ".tsv":
		return e
	default:
		return ".xlsx"
	}
}

// listWorktrees 列出文档全部草稿（按创建时间倒序）。
func (s *wtStore) listWorktrees(rel string) []worktreeMeta {
	dir := filepath.Join(s.root, docKey(rel), "worktrees")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []worktreeMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta := s.readMeta(rel, e.Name())
		if meta != nil {
			out = append(out, *meta)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// readMeta 读取草稿元数据（不存在返回 nil）。
func (s *wtStore) readMeta(rel, wtID string) *worktreeMeta {
	b, err := os.ReadFile(s.metaPath(rel, wtID))
	if err != nil {
		return nil
	}
	var m worktreeMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return &m
}

// writeMeta 原子写草稿元数据。
func (s *wtStore) writeMeta(rel string, m *worktreeMeta) error {
	m.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.metaPath(rel, m.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.metaPath(rel, m.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath(rel, m.ID))
}

// resolveScope 校验文档路径合法（拒绝绝对路径/越界），返回工作区相对路径。
func resolveScope(workspace, rel string) (string, error) {
	if rel == "" {
		return "", derr(errInvalidRequest, "file is required")
	}
	rel = filepath.ToSlash(rel)
	if strings.Contains(rel, "..") || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") {
		return "", derr(errScopeDenied, "path %q is outside the workspace scope", rel)
	}
	return strings.TrimPrefix(rel, "./"), nil
}

// trunkPath 文档主线绝对路径。
func trunkPath(workspace, rel string) string {
	return filepath.Join(workspace, filepath.FromSlash(rel))
}

// ---------- 生命周期 ----------

// createWorktree 创建草稿：复制 trunk 到草稿目录，状态 draft。
func (s *wtStore) createWorktree(workspace, rel, name, agentID string) (worktreeMeta, error) {
	if _, err := os.Stat(trunkPath(workspace, rel)); err != nil {
		if os.IsNotExist(err) {
			return worktreeMeta{}, derr(errFileNotFound, "trunk file %q not found; create it with office_new first", rel)
		}
		return worktreeMeta{}, err
	}
	wtID := fmt.Sprintf("wt-%d", time.Now().UnixMilli())
	meta := worktreeMeta{
		ID:        wtID,
		Name:      name,
		Status:    statusDraft,
		Ext:       mimeExt(rel),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Units:     nil,
	}
	if agentID != "" {
		meta.Name = strings.TrimSpace(meta.Name)
		if meta.Name == "" {
			meta.Name = agentID
		}
	}
	dir := s.worktreeDir(rel, wtID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return meta, err
	}
	// 复制 trunk 内容到草稿（保留扩展名）。
	src, err := os.ReadFile(trunkPath(workspace, rel))
	if err != nil {
		return meta, err
	}
	if err := os.WriteFile(filepath.Join(dir, "draft"+meta.Ext), src, 0o644); err != nil {
		return meta, err
	}
	if err := s.writeMeta(rel, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// setStatus 修改草稿状态（带状态机校验：draft↔ready，终态不可变）。
func (s *wtStore) setStatus(rel, wtID string, next worktreeStatus) error {
	meta := s.readMeta(rel, wtID)
	if meta == nil {
		return derr(errWorktreeState, "worktree %q not found for %q", wtID, rel)
	}
	// 状态机：draft ↔ ready；merged/discarded 终态。
	switch next {
	case statusMerged, statusDiscarded:
		// 允许从任何终态前状态进入终态（由 merge/discard 流程控制）。
		if meta.Status == statusMerged || meta.Status == statusDiscarded {
			return derr(errWorktreeState, "worktree %q is already %s (terminal)", wtID, meta.Status)
		}
	case statusReady:
		if meta.Status != statusDraft {
			return derr(errWorktreeState, "worktree %q is %s; only draft can become ready", wtID, meta.Status)
		}
	case statusDraft:
		if meta.Status == statusReady || meta.Status == statusDraft {
			// reopen ready → draft
		} else {
			return derr(errWorktreeState, "worktree %q is %s (terminal); cannot reopen", wtID, meta.Status)
		}
	default:
		return derr(errInvalidRequest, "unknown status %q", next)
	}
	meta.Status = next
	return s.writeMeta(rel, meta)
}

// readyWorktree draft → ready（提交修改等待审阅）。
func (s *wtStore) readyWorktree(rel, wtID string) error { return s.setStatus(rel, wtID, statusReady) }

// reopenWorktree ready → draft（重新打开继续修改）。
func (s *wtStore) reopenWorktree(rel, wtID string) error { return s.setStatus(rel, wtID, statusDraft) }

// mergeWorktree 合入主线：草稿内容覆盖 trunk，草稿标记 merged。
func (s *wtStore) mergeWorktree(workspace, rel, wtID string) error {
	meta := s.readMeta(rel, wtID)
	if meta == nil {
		return derr(errWorktreeState, "worktree %q not found for %q", wtID, rel)
	}
	if meta.Status != statusReady {
		return derr(errWorktreeState, "worktree %q is %s; only ready can merge (mark ready first)", wtID, meta.Status)
	}
	// 校验草稿文件存在。
	draft := s.draftPath(rel, wtID)
	if _, err := os.Stat(draft); err != nil {
		return derr(errWorktreeState, "draft file missing for %q: %v", wtID, err)
	}
	b, err := os.ReadFile(draft)
	if err != nil {
		return err
	}
	// 覆盖 trunk（先备份到 .goffice backup 供撤销？dsh 语义不撤销，确认即发布）。
	if err := os.WriteFile(trunkPath(workspace, rel), b, 0o644); err != nil {
		return err
	}
	return s.setStatus(rel, wtID, statusMerged)
}

// discardWorktree 放弃草稿：删除草稿目录，标记 discarded。
func (s *wtStore) discardWorktree(rel, wtID string) error {
	meta := s.readMeta(rel, wtID)
	if meta == nil {
		return derr(errWorktreeState, "worktree %q not found for %q", wtID, rel)
	}
	dir := s.worktreeDir(rel, wtID)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// 目录删除后 meta 也丢了，直接重建废弃标记（仅用于状态查询）。
	m2 := *meta
	m2.Status = statusDiscarded
	_ = s.writeMeta(rel, &m2)
	return nil
}
