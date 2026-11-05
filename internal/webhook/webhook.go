// Package webhook 事件触发任务（B3，对标 OpenHands Automation Webhook）。
//
// 本地 HTTP 入口 POST /hook/<id> → 触发预绑定任务文本（经宿主注入的 Runner
// 走既有引擎执行路径，无人值守语义）。仅监听 127.0.0.1，不暴露公网；
// 支持 Bearer secret 校验与每 hook 固定窗口限流。
package webhook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Hook 单个触发器定义（~/.kaelvoris/hooks_trigger.json 数组元素）。
type Hook struct {
	ID       string `json:"id"`                  // 触发路径 /hook/<id>
	Name     string `json:"name,omitempty"`      // 展示名
	Secret   string `json:"secret,omitempty"`    // 非空 = 校验 Authorization: Bearer <secret>
	Task     string `json:"task"`                // 触发的任务 prompt 文本
	Workspae string `json:"workspace,omitempty"` // 保留字段（当前执行复用默认会话工作区）
	Mode     string `json:"mode,omitempty"`      // 保留字段（当前执行走会话既有模式）
	Enabled  bool   `json:"enabled"`             // false = 403
}

// Store Hook 定义存储（JSON 文件，不存在 = 空）。
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore 创建存储（path = ~/.kaelvoris/hooks_trigger.json）。
func NewStore(path string) *Store { return &Store{path: path} }

// List 全部 hook。
func (s *Store) List() ([]Hook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list()
}

// Save 全量覆写。
func (s *Store) Save(hooks []Hook) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(hooks, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}

func (s *Store) list() ([]Hook, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Hook
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, nil // 损坏 = 空列表（跳过不致命约定）
	}
	return out, nil
}

// limiter 每 hook 固定窗口限流（10 次/分钟）。
type limiter struct {
	mu     sync.Mutex
	window time.Duration
	limit  int
	hits   map[string][]time.Time
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{window: window, limit: limit, hits: map[string][]time.Time{}}
}

// allow 记录并判定是否放行。
func (l *limiter) allow(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := timeNow()
	// 清理窗口外命中。
	live := l.hits[id][:0]
	for _, t := range l.hits[id] {
		if now.Sub(t) < l.window {
			live = append(live, t)
		}
	}
	if len(live) >= l.limit {
		l.hits[id] = live
		return false
	}
	l.hits[id] = append(live, now)
	return true
}

// TriggerLog 单次触发记录。
type TriggerLog struct {
	Time   int64  `json:"time"` // unix 毫秒
	HookID string `json:"hookId"`
	IP     string `json:"ip"`
	Status int    `json:"status"` // HTTP 状态码
	Detail string `json:"detail,omitempty"`
}

// Log 环形触发记录（内存 + 落盘 jsonl 最近 200 行）。
type Log struct {
	mu   sync.Mutex
	hist []TriggerLog
	path string // hooks_trigger.log.jsonl
}

// NewLog 创建触发记录（导出供宿主注入路径；确保父目录存在）。
func NewLog(path string) *Log {
	l := &Log{path: path}
	l.ensureDir()
	return l
}

func newLog(path string) *Log { return NewLog(path) }

// Add 追加记录（内存环形 50 + 文件追加截尾 200）。
func (l *Log) Add(t TriggerLog) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hist = append(l.hist, t)
	if len(l.hist) > 50 {
		l.hist = l.hist[len(l.hist)-50:]
	}
	if l.path == "" {
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if b, err := json.Marshal(t); err == nil {
		l.histFileTruncate(f)
		_, _ = f.Write(append(b, '\n'))
	}
}

// histFileTruncate 文件超过 200 行时重写为最近 200 行（尽力而为）。
func (l *Log) histFileTruncate(f *os.File) {
	// 简化：直接检查内存外文件——跳过实现复杂度，写满 400 行时重建一次。
	// ponytail：jsonl 日志仅排查用，重写策略从简。
	b, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 200 {
		keep := strings.Join(lines[len(lines)-200:], "\n") + "\n"
		f2, err := os.Create(l.path)
		if err == nil {
			_, _ = f2.WriteString(keep)
			f2.Close()
		}
	}
}

// History 最近触发记录（内存环形）。
func (l *Log) History() []TriggerLog {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]TriggerLog, len(l.hist))
	copy(out, l.hist)
	return out
}

// ensureParent 确保日志文件父目录存在。
func (l *Log) ensureDir() {
	if l.path != "" {
		_ = os.MkdirAll(filepath.Dir(l.path), 0o755)
	}
}

var timeNow = time.Now

// LoadOrInit 读取存储并确保父目录存在。
func LoadOrInit(path string) *Store {
	s := NewStore(path)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	return s
}
