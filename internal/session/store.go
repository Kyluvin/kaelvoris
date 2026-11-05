// Package session 会话持久化存储（P2.6，P3.1 异步化改造）。
//
// 每个会话一个 JSON 文件（~/.kaelvoris/sessions/<id>.json），原子写入。
// P3.1 卡顿修复：Save 改为「内存缓存 + 后台异步刷盘」——say 桥高频事件
// 不再同步 MarshalIndent 整个会话并写盘（会话文件数 MB 时每 chunk 写盘
// 是 UI 卡顿根因）。策略：
//   - Save：合并到内存缓存并标记 dirty，立即返回（不碰磁盘）；
//   - flusher 后台 goroutine：≥200ms 间隔批量刷盘（同会话多次 Save 合并为
//     一次写，流式期间的 N 次增量写 → 每 200ms 至多一次）；
//   - FlushAll/Close：退出前强制刷盘，保证数据不丢。
//   - List/Get 走内存缓存（会话切换/列表刷新零 IO）。
package session

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

// flushInterval 刷盘节流间隔：流式期间同会话多次 Save 合并为一次磁盘写。
const flushInterval = 200 * time.Millisecond

// Store 会话持久化存储（内存缓存 + 异步刷盘）。
type Store struct {
	dir string

	mu    sync.Mutex
	cache map[string]*Session // 内存缓存（id → 会话对象，dirty 与否统一管理）
	dirty map[string]bool     // 待刷盘集合
	// flushMu 序列化单会话刷盘（同一会话只允许一个 goroutine 在写文件，
	// 后续到达的刷盘请求见 dirty=true 重新排队）。
	flushMu sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	stop    chan struct{}
}

// NewStore 创建存储（目录不存在时在 Save 时创建）并启动后台刷盘。
func NewStore(dir string) *Store {
	st := &Store{
		dir:   dir,
		cache: map[string]*Session{},
		dirty: map[string]bool{},
		stop:  make(chan struct{}),
	}
	// 循环重建版 flusher：flusherOnce panic 退出后重建（Close 关闭 stop 后
	// flusherOnce 立即 return，外层循环消费到 stop 关闭自然退出，无忙等待）。
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()
		for {
			select {
			case <-st.stop:
				return // Close 已关闭：退出重建循环（close 后 stop 永远可读）
			default:
			}
			st.flusherOnce()
		}
	}()
	return st
}

func (st *Store) file(id string) string { return filepath.Join(st.dir, id+".json") }

// flusherOnce 单轮刷盘循环：启动 ticker → 消费 dirty → panic 由外层重建。
func (st *Store) flusherOnce() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("会话刷盘协程 panic（已拦截，重建循环）", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-st.stop:
			// 退出前清空 dirty（尽力而为；Close 再兜底一次）。
			st.flushDirty()
			return
		case <-ticker.C:
			st.flushDirty()
		}
	}
}

// flushDirty 把当前 dirty 集合内的会话写盘。
func (st *Store) flushDirty() {
	st.mu.Lock()
	if len(st.dirty) == 0 {
		st.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(st.dirty))
	for id := range st.dirty {
		ids = append(ids, id)
	}
	st.dirty = map[string]bool{}
	st.mu.Unlock()

	for _, id := range ids {
		st.mu.Lock()
		s, ok := st.cache[id]
		st.mu.Unlock()
		if !ok {
			continue
		}
		// 落盘失败重新标记 dirty：下个 tick 自动重试（磁盘瞬时故障自愈；
		// 旧实现失败即丢，dirty 已清空，数据静默丢失）。
		if err := st.write(s.Snapshot()); err != nil {
			st.mu.Lock()
			st.dirty[id] = true
			st.mu.Unlock()
		}
	}
}

// write 单会话落盘（原子替换；调用方不持锁）。
// 可靠性加固（2026-10-03 会话复盘：~/.kaelvoris/sessions 出现 378KB 全 NUL
// 的损坏会话文件——tmp 写入后未 fsync 即 Rename，异常断电时 NTFS 元数据
// 先落、数据未刷；且三处失败全部静默裸 return，磁盘满/权限问题无任何
// 痕迹，dirty 已被清空导致数据静默丢失）：
//   - tmp 写入后 f.Sync() 强制刷盘再 Rename（防全 NUL 损坏文件）；
//   - 失败记日志并返回 error，由 flushDirty/FlushAll 重新标记 dirty 重试；
//   - Rename 失败（Windows 文件占用等）保留 tmp 副本供排查。
func (st *Store) write(s SessionData) error {
	// schema 版本打标（写入单点）：落盘前统一置当前版本，旧格式会话一经
	// 任何改写即升级为带版本的新格式（读取期判别依据随之收敛）。
	s.SchemaVersion = CurrentSchemaVersion
	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		slog.Error("会话落盘失败（创建目录）", "id", s.ID, "err", err)
		return err
	}
	b, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		slog.Error("会话序列化失败", "id", s.ID, "err", err)
		return err
	}
	tmp := st.file(s.ID) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		slog.Error("会话落盘失败（创建临时文件）", "id", s.ID, "err", err)
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		slog.Error("会话落盘失败（写临时文件）", "id", s.ID, "err", err)
		return err
	}
	if err := f.Sync(); err != nil { // 关键：数据强制落盘后再改名（防全 NUL）
		f.Close()
		slog.Error("会话落盘失败（fsync）", "id", s.ID, "err", err)
		return err
	}
	f.Close()
	if err := os.Rename(tmp, st.file(s.ID)); err != nil {
		// Windows 下目标被占用（回放/上传）时保留 .tmp 副本，下次刷盘重试。
		slog.Error("会话落盘失败（原子替换，保留 .tmp 副本）", "id", s.ID, "err", err)
		return err
	}
	return nil
}

// Save 合并到内存缓存并标记 dirty，异步刷盘（不阻塞调用方）。
// p 可为外部构造的会话（如 runTask 读取的持久化副本）或内存活跃会话。
func (st *Store) Save(s *Session) {
	if s == nil {
		return
	}
	st.mu.Lock()
	st.cache[s.ID] = s
	st.dirty[s.ID] = true
	st.mu.Unlock()
}

// FlushAll 立即刷盘全部 dirty 会话（任务完成/停止后调用，保证关键节点持久化）。
func (st *Store) FlushAll() {
	st.mu.Lock()
	if len(st.dirty) == 0 {
		st.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(st.dirty))
	for id := range st.dirty {
		ids = append(ids, id)
	}
	st.dirty = map[string]bool{}
	st.mu.Unlock()
	for _, id := range ids {
		st.mu.Lock()
		s, ok := st.cache[id]
		st.mu.Unlock()
		if !ok {
			continue
		}
		if err := st.write(s.Snapshot()); err != nil {
			st.mu.Lock()
			st.dirty[id] = true
			st.mu.Unlock()
		}
	}
}

// Close 停止后台刷盘并落盘全部 dirty（应用退出时调用）。
func (st *Store) Close() {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	st.closed = true
	close(st.stop)
	st.mu.Unlock()
	st.wg.Wait() // flusherOnce 消费到 stop 关闭后返回，wg 释放
	st.FlushAll()
}

// List 全部会话摘要（置顶优先（PinnedAt 新→旧），其余按更新时间倒序；
// 优先内存缓存，缓存未命中时从盘加载）。
// 置顶语义对齐服务端 ListSessionSummaries（pinned 优先 + 时间倒序）。
func (st *Store) List() []Summary {
	st.mu.Lock()
	st.loadAllLocked()
	out := make([]Summary, 0, len(st.cache))
	for _, s := range st.cache {
		out = append(out, s.Summary())
	}
	st.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := out[i].PinnedAt > 0, out[j].PinnedAt > 0
		if pi != pj {
			return pi // 置顶会话恒排前
		}
		if pi && pj {
			return out[i].PinnedAt > out[j].PinnedAt // 均置顶：新置顶在前
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// Get 读取会话（内存缓存优先，未命中时从盘加载）。
func (st *Store) Get(id string) (*Session, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s, ok := st.cache[id]; ok {
		return s, nil
	}
	s, err := st.loadLocked(id)
	if err != nil {
		return nil, err
	}
	st.cache[id] = s
	return s, nil
}

// Delete 删除会话（内存 + 磁盘 + 记忆文档）。
func (st *Store) Delete(id string) error {
	st.mu.Lock()
	delete(st.cache, id)
	delete(st.dirty, id)
	err := os.Remove(st.file(id))
	_ = os.Remove(memoryPath(st.dir, id)) // 记忆文档一并清理
	st.mu.Unlock()
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// loadAllLocked 扫描目录加载缺失的会话到缓存（加锁调用）。
func (st *Store) loadAllLocked() {
	entries, err := os.ReadDir(st.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := filepath.Base(e.Name())
		id = id[:len(id)-len(".json")]
		if _, ok := st.cache[id]; ok {
			continue
		}
		if s, err := st.loadLocked(id); err == nil {
			st.cache[id] = s
		}
	}
}

// loadLocked 从盘加载单个会话（加锁调用；JSON 直灌 SessionData 后转活跃对象）。
// 损坏容错（2026-10-03 复盘）：历史损坏文件（如全 NUL 文件）解析失败时
// 不再永久报错阻塞列表/回放——把坏文件改名隔离为 <id>.json.corrupt，
// 返回空会话（保留 id/元数据），用户仍可删除或新建该会话。
func (st *Store) loadLocked(id string) (*Session, error) {
	b, err := os.ReadFile(st.file(id))
	if err != nil {
		return nil, err
	}
	var d SessionData
	if err := json.Unmarshal(b, &d); err != nil {
		slog.Error("会话文件损坏（已隔离为 .corrupt）", "id", id, "size", len(b), "err", err)
		_ = os.Rename(st.file(id), st.file(id)+".corrupt")
		now := time.Now()
		d = SessionData{ID: id, CreatedAt: now, UpdatedAt: now}
		return d.IntoSession(), nil
	}
	// 旧格式归一化（读取单点）：schemaVersion==0 = 旧格式会话（历史文件无此
	// 字段），在此走缺省分支补齐显式判别依据，替代逐字段嗅探；后续 schema
	// 迁移（v1→v2+）按版本号在处分派（旧版本字段换名/结构变形时逐级改写）。
	if d.SchemaVersion == 0 {
		d.SchemaVersion = CurrentSchemaVersion
	}
	return d.IntoSession(), nil
}
