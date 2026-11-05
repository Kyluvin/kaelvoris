// server.go HTTP 触发入口：仅绑定 127.0.0.1，POST /hook/<id>。
//
// 校验顺序：路径 ID → 存在/启用 → secret → 限流 → 触发（202 异步）。
package webhook

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Runner 任务执行回调（宿主注入：task 文本 → 既有引擎无人值守执行路径）。
// 阻塞与否由宿主决定（app.go 内部再异步化，server 侧不等待结果）。
type Runner func(hook Hook)

// Server 触发 HTTP 服务。
type Server struct {
	Store  *Store
	Log    *Log
	Runner Runner
	// MaxPerMinute 每 hook 触发上限（默认 10）。
	MaxPerMinute int
	limiter      *limiter
	hist         *Log // 触发记录（与 Log 同义别名，避免与 slog 混淆）
	log          *slog.Logger
	mu           sync.Mutex
	srv          *http.Server
}

// NewServer 创建（不启动）。logger 可 nil（用 slog.Default）。
func NewServer(store *Store, hist *Log, runner Runner) *Server {
	return &Server{Store: store, Log: hist, Runner: runner, limiter: newLimiter(10, time.Minute)}
}

// limiterFor 取当前限流器（容量可被 MaxPerMinute 覆盖：重建）。
func (s *Server) limiterFor(limit int) *limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limiter.limit != limit {
		s.limiter = newLimiter(limit, time.Minute)
	}
	return s.limiter
}

// Start 启动监听（仅 127.0.0.1）；失败返回错误（宿主仅日志告警不 crash）。
func (s *Server) Start(port int) error {
	mux := http.NewServeMux()
	mux.Handle("/hook/", s)
	s.srv = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: mux,
	}
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("webhook listen %s: %w", s.srv.Addr, err)
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			if s.lgr() != nil {
				s.lgr().Error("webhook server stopped", "err", err)
			}
		}
	}()
	return nil
}

// Stop 优雅关闭（幂等）。
func (s *Server) Stop() {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// record 追加触发记录（log 可能未初始化，容错）。
func (s *Server) record(status int, hookID, ip, detail string) {
	if s.Log == nil {
		return
	}
	s.Log.Add(TriggerLog{Time: timeNow().UnixMilli(), HookID: hookID, IP: ip, Status: status, Detail: detail})
}

// ServeHTTP 处理 /hook/<id>。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		reply(w, 405, map[string]any{"ok": false, "error": "method not allowed, use POST"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/hook/")
	id = strings.Trim(id, "/")
	if id == "" {
		reply(w, 404, map[string]any{"ok": false, "error": "missing hook id"})
		return
	}
	ip := clientIP(r)
	hooks, err := s.Store.List()
	if err != nil {
		reply(w, 500, map[string]any{"ok": false, "error": "store read failed"})
		return
	}
	var h *Hook
	for i := range hooks {
		if hooks[i].ID == id {
			h = &hooks[i]
			break
		}
	}
	if h == nil {
		s.record(404, id, ip, "unknown hook")
		reply(w, 404, map[string]any{"ok": false, "error": "unknown hook"})
		return
	}
	if !h.Enabled {
		s.record(403, id, ip, "hook disabled")
		reply(w, 403, map[string]any{"ok": false, "error": "hook disabled"})
		return
	}
	if h.Secret != "" && !checkSecret(r, h.Secret) {
		s.record(401, id, ip, "bad secret")
		reply(w, 401, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	limit := s.MaxPerMinute
	if limit <= 0 {
		limit = 10
	}
	if !s.limiterFor(limit).allow(id) {
		s.record(429, id, ip, "rate limited")
		reply(w, 429, map[string]any{"ok": false, "error": "rate limit exceeded"})
		return
	}
	if strings.TrimSpace(h.Task) == "" {
		s.record(400, id, ip, "empty task")
		reply(w, 400, map[string]any{"ok": false, "error": "hook has empty task"})
		return
	}
	// 异步触发：Runner 由宿主注入，202 立即返回（不等待任务完成）。
	task := h.Task
	hookID := h.ID
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if s.lgr() != nil {
					s.lgr().Error("webhook runner panic", "hook", hookID, "panic", r)
				}
			}
		}()
		s.Runner(Hook{ID: hookID, Task: task})
	}()
	s.record(202, id, ip, "triggered")
	reply(w, 202, map[string]any{"ok": true, "hook": id})
}

// lgr 延迟初始化的 slog（避免构造参数膨胀）。
func (s *Server) lgr() *slog.Logger {
	return slog.Default()
}

// checkSecret 常量时间比较 Bearer secret。
func checkSecret(r *http.Request, secret string) bool {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(auth[len(prefix):])), []byte(secret)) == 1
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
