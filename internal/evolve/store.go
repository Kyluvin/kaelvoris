// Package evolve 自进化闭环：定时自检 → 发现薄弱点 → 沉淀经验卡 → 注入提示词。
//
// 借鉴思路（不引外部依赖，纯 Go 内实现）：
//   - Aider 的 CONVENTIONS.md：把反复纠正过的行为写成规则文件，每次任务注入；
//   - OpenHands/SWE-agent 的 microagents：触发词驱动的小知识卡（< 500 字），
//     命中关键词才注入，避免提示词膨胀；
//   - Cline/Zoo-Code 的 memory 机制：会话级 MEMORY.md + 全局经验沉淀分离。
//
// 闭环流程：
//  1. 用户（或 cron_job 工具）创建"代码自检"定时任务，提示词要求 Agent
//     复盘近期会话中的失误/纠偏，调用 evolve_record 工具写入经验卡；
//  2. 经验卡持久化于 ~/.kaelvoris/evolve/cards.json（含触发关键词与正文）；
//  3. 每次任务启动时（通道层组装引擎后）调用 ContextSnippet(workspace)：
//     根据任务文本命中的关键词挑选经验卡，生成 <self_evolution> 提示段；
//  4. Agent 编码能力随经验卡积累逐步增强，且提示词只增命中卡，不无限膨胀。
//
// 约束：单卡正文 ≤ 600 字符；总卡数 ≤ 64（超出淘汰最久未命中）；全部本地。
package evolve

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Card 一张经验卡（microagent 风格：关键词触发 + 精炼正文）。
type Card struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`            // 一句话规则（列表展示）
	Keywords  []string  `json:"keywords"`         // 触发关键词（任务文本命中即注入）
	Body      string    `json:"body"`             // 正文经验（≤600 字符）
	Source    string    `json:"source,omitempty"` // 来源（manual / cron:<jobID> / session:<id>）
	Hits      int       `json:"hits"`             // 累计命中次数（前端排序展示）
	LastHitAt time.Time `json:"lastHitAt,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store 经验卡持久化（cards.json，原子写盘）。
type Store struct {
	dir   string
	mu    sync.Mutex
	cards []Card
}

// 限制常量（防提示词膨胀）。
const (
	maxCards      = 64  // 总卡数上限（超出淘汰最久未命中）
	maxBodyRunes  = 600 // 单卡正文上限（rune）
	maxKeywordLen = 24  // 单关键词上限（rune）
)

// evictGrace 淘汰宽限期（出生保护，2026-10-10 定时自检实锤）：恢复重放的卡
// Hits=0、LastHitAt 为零值，直接参与 (Hits, LastHitAt) 淘汰竞争恒判最差——
// 满库时任何一次 Add 都会把它挤出（实测：并发 add 风暴三波 8 进 8 出，
// 5 张恢复卡全灭，reloadMergeLocked 快照回滚修复后的二次丢卡形态）。
// 宽限期（UpdatedAt 起 48h）内不参与淘汰，期间经 Match 命中累加计数后
// 回归正常竞争。var 供测试注入 0 复现旧语义。
var evictGrace = 48 * time.Hour

// NewStore 创建存储并加载既有数据（目录不存在自动创建）。
func NewStore(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s.load()
	return s, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "cards.json") }

func (s *Store) load() {
	b, err := os.ReadFile(s.path())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &s.cards)
}

// reloadMergeLocked 从磁盘重读并合并其他进程的改动（多宿主共用 cards.json）。
//
// 双写者问题（2026-10-10 定时自检实锤）：Store 只在 NewStore 时 load 一次，此后
// 每次 saveLocked 都是内存全量覆盖写盘——桌面/CLI/server/cron 多进程共用
// cards.json 时，先启动的进程会用陈旧快照覆盖掉其他进程新增/更新的卡
// （实测：恢复的经验卡隔轮反复消失，即跷跷板回滚的根因）。
// 合并语义（按 ID 对齐）：磁盘有内存无→采纳（他人新增）；内存有磁盘无→丢弃
// （他人已删，不复活）；双方都有→Hits/LastHitAt 取较大/较新，正文等取
// UpdatedAt 较新一方（最新编辑胜出）。读取失败按既有约定跳过不致命。
func (s *Store) reloadMergeLocked() {
	b, err := os.ReadFile(s.path())
	if err != nil {
		return
	}
	var disk []Card
	if err := json.Unmarshal(b, &disk); err != nil {
		return
	}
	idx := make(map[string]int, len(s.cards))
	for i := range s.cards {
		idx[s.cards[i].ID] = i
	}
	// ponytail: 全量重建而非原地复用底层数组——mc 指向 s.cards，原地 append 会
	// 覆写尚未匹配的内存卡（≤64 张，分配开销可忽略）。
	merged := make([]Card, 0, len(s.cards)+len(disk))
	for _, dc := range disk {
		if i, ok := idx[dc.ID]; ok {
			mc := &s.cards[i]
			if dc.Hits > mc.Hits {
				mc.Hits = dc.Hits
			}
			if dc.LastHitAt.After(mc.LastHitAt) {
				mc.LastHitAt = dc.LastHitAt
			}
			if dc.UpdatedAt.After(mc.UpdatedAt) {
				mc.Title, mc.Body, mc.Keywords, mc.Source, mc.UpdatedAt =
					dc.Title, dc.Body, dc.Keywords, dc.Source, dc.UpdatedAt
			}
			merged = append(merged, *mc)
			delete(idx, dc.ID)
		} else {
			merged = append(merged, dc) // 采纳他人新增
		}
	}
	// 内存有、磁盘无的卡不保留：其他进程已删除，防陈旧快照复活已删卡。
	s.cards = merged
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.cards, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "ev_" + hex.EncodeToString(b)
}

// List 全部经验卡（命中数倒序，其次更新时间）。
func (s *Store) List() []Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Card, len(s.cards))
	copy(out, s.cards)
	// 命中多的在前（能力沉淀的"常用规则"优先展示）。
	for i := 0; i < len(out); i++ {
		for k := i + 1; k < len(out); k++ {
			a, b := out[i], out[k]
			if b.Hits > a.Hits || (b.Hits == a.Hits && b.UpdatedAt.After(a.UpdatedAt)) {
				out[i], out[k] = b, a
			}
		}
	}
	return out
}

// Add 新增经验卡（自动去重：同 Title 合并 → 更新正文并保留命中数）。
func (s *Store) Add(c Card) (Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadMergeLocked() // 先合并其他进程的改动，防陈旧快照整库覆盖（跷跷板回滚）
	now := time.Now()
	c.Title = strings.TrimSpace(c.Title)
	c.Body = clampRunes(strings.TrimSpace(c.Body), maxBodyRunes)
	c.Keywords = normalizeKeywords(c.Keywords)
	if c.Title == "" || c.Body == "" {
		return c, errCardEmpty
	}
	// 去重合并：同标题视为同一规则的更新（经验迭代而非重复堆叠）。
	for i := range s.cards {
		if s.cards[i].Title == c.Title {
			s.cards[i].Body = c.Body
			s.cards[i].Keywords = mergeKeywords(s.cards[i].Keywords, c.Keywords)
			if c.Source != "" {
				s.cards[i].Source = c.Source
			}
			s.cards[i].UpdatedAt = now
			merged := s.cards[i] // 拷贝返回值：淘汰可能移除前面的元素致索引漂移
			err := s.evictIfNeededLocked(merged.ID)
			if err == nil {
				err = s.saveLocked()
			}
			return merged, err
		}
	}
	c.ID = newID()
	c.CreatedAt = now
	c.UpdatedAt = now
	s.cards = append(s.cards, c)
	err := s.evictIfNeededLocked(c.ID)
	if err == nil {
		err = s.saveLocked()
	}
	return c, err
}

// Delete 删除经验卡。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadMergeLocked() // 先合并他人改动：他人新增的卡保持可见，他人删除的卡不复活
	for i := range s.cards {
		if s.cards[i].ID == id {
			s.cards = append(s.cards[:i], s.cards[i+1:]...)
			return s.saveLocked()
		}
	}
	return os.ErrNotExist
}

// Match 命中任务文本的经验卡（大小写不敏感；无关键词的卡视为"常驻卡"，
// 始终注入——用于全局编码守则类经验）。命中即累加计数（节流落盘）。
func (s *Store) Match(text string) []Card {
	text = strings.ToLower(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadMergeLocked() // 命中计数落盘前先合并他人改动，防高频 Match 覆盖丢卡
	var out []Card
	dirty := false
	now := time.Now()
	for i := range s.cards {
		c := &s.cards[i]
		if len(c.Keywords) == 0 || matchKeywords(text, c.Keywords) {
			out = append(out, *c)
			c.Hits++
			c.LastHitAt = now
			dirty = true
		}
	}
	if dirty {
		_ = s.saveLocked() // 命中计数落盘（失败无害，仅统计）
	}
	return out
}

// ContextSnippet 生成注入系统提示词的 <self_evolution> 段（无命中返回空）。
// text 为当前任务文本；经验卡按命中关键词过滤，控制提示词体积。
func (s *Store) ContextSnippet(text string) string {
	cards := s.Match(text)
	if len(cards) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<self_evolution>\n")
	b.WriteString("以下经验来自本项目历史任务的复盘沉淀（自进化闭环），与当前任务相关时务必遵守：\n")
	for _, c := range cards {
		b.WriteString("- " + c.Title + "：" + c.Body + "\n")
	}
	b.WriteString("</self_evolution>")
	return b.String()
}

// matchKeywords 任一关键词命中（子串匹配，小写比较）。
func matchKeywords(text string, kws []string) bool {
	for _, k := range kws {
		if k != "" && strings.Contains(text, k) {
			return true
		}
	}
	return false
}

// transientStatePatterns 临时状态型断言模式（G2 对标 CCGS agent-memory 规则：
// 不把临时缺席记成持久事实）。命中正文的卡大概率是「当前没有 X / 不存在 X」
// 这类必然过期的观察——两周后会让未来会话对着已建成事实做出错误判断。
// 检测面刻意保守（仅开头/明确句式）：宁可漏报不误报，提示而非拦截。
	var transientStatePatterns = []string{
		"没有实现", "没有游戏", "没有代码", "没有测试", "没有配置", "没有资产",
		"尚未实现", "尚未创建", "尚不存在",
		"不存在", "缺失", "缺少", "还没有",
		"no game code", "no tests", "not implemented", "no assets", "no engine",
		"missing ", "doesn't exist", "does not exist", "not yet",
	}

// IsTransientState 判断经验卡正文是否为「临时状态型断言」（G2）：
// 大小写不敏感子串命中任一模式即返回 true。调用方（工具层）据此返回
// 改写提醒（写入不拦截——状态卡也有价值，但要带失效条件）。
func IsTransientState(body string) bool {
	b := strings.ToLower(body)
	for _, p := range transientStatePatterns {
		if strings.Contains(b, p) {
			return true
		}
	}
	return false
}

// normalizeKeywords 清洗关键词：小写、去空、限长、去重。
func normalizeKeywords(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, k := range in {
		k = strings.ToLower(strings.TrimSpace(k))
		k = clampRunes(k, maxKeywordLen)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// mergeKeywords 合并去重。
func mergeKeywords(a, b []string) []string {
	return normalizeKeywords(append(append([]string{}, a...), b...))
}

// evictIfNeededLocked 卡数超限时淘汰最久未命中（Hits 最少 → LastHitAt 最旧），
// 宽限期（evictGrace）内的卡除外。protectID 为本次 Add 的卡：满库时新卡
// Hits=0、LastHitAt 零值会被判为最差，不保护会导致「新增即自淘汰」
// （实测：64 张满库时新增卡落盘后又消失）。
func (s *Store) evictIfNeededLocked(protectID string) error {
	for len(s.cards) > maxCards {
		worst := -1
		for i := range s.cards {
			if s.cards[i].ID == protectID {
				continue
			}
			if time.Since(s.cards[i].UpdatedAt) < evictGrace {
				continue // 出生保护：宽限期内不参与淘汰（恢复卡 Hits=0 恒判最差）
			}
			if worst == -1 {
				worst = i
				continue
			}
			a, w := s.cards[i], s.cards[worst]
			if a.Hits < w.Hits || (a.Hits == w.Hits && a.LastHitAt.Before(w.LastHitAt)) {
				worst = i
			}
		}
		if worst == -1 {
			return nil // 全员受保护（宽限期+protectID）：库短暂超限，卡过期后自愈
		}
		s.cards = append(s.cards[:worst], s.cards[worst+1:]...)
	}
	return nil
}

// clampRunes 按 rune 截断。
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

const errCardEmpty = simpleErr("经验卡标题与正文不能为空")
