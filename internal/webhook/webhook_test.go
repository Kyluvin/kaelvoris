package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testHook(id string) Hook {
	return Hook{ID: id, Name: "t", Task: "run tests", Enabled: true}
}

func setup(t *testing.T) (*Server, *Store, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "hooks.json"))
	log := newLog(filepath.Join(dir, "log.jsonl"))
	var runs atomic.Int32
	srv := NewServer(store, log, func(h Hook) { runs.Add(1) })
	return srv, store, &runs
}

func post(path, secret string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte("{}")))
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	return r
}

func TestEndToEnd(t *testing.T) {
	srv, store, runs := setup(t)
	if err := store.Save([]Hook{testHook("gh-push")}); err != nil {
		t.Fatal(err)
	}
	// 命中。
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post("/hook/gh-push", ""))
	if w.Code != 202 {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}
	// 等异步 Runner 执行。
	deadline := time.Now().Add(2 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runs.Load() != 1 {
		t.Fatalf("runner ran %d times", runs.Load())
	}
	// 未知 hook。
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, post("/hook/nope", ""))
	if w.Code != 404 {
		t.Fatalf("want 404, got %d", w.Code)
	}
	// GET 拒绝。
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hook/gh-push", nil))
	if w.Code != 405 {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestSecretAndDisabled(t *testing.T) {
	srv, store, _ := setup(t)
	h := testHook("sec")
	h.Secret = "s3cret"
	if err := store.Save([]Hook{h}); err != nil {
		t.Fatal(err)
	}
	// 无 secret。
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post("/hook/sec", ""))
	if w.Code != 401 {
		t.Fatalf("want 401, got %d", w.Code)
	}
	// 错 secret。
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, post("/hook/sec", "wrong"))
	if w.Code != 401 {
		t.Fatalf("want 401, got %d", w.Code)
	}
	// 对 secret。
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, post("/hook/sec", "s3cret"))
	if w.Code != 202 {
		t.Fatalf("want 202, got %d", w.Code)
	}
	// 禁用。
	h.Enabled = false
	if err := store.Save([]Hook{h}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, post("/hook/sec", "s3cret"))
	if w.Code != 403 {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestRateLimit(t *testing.T) {
	srv, store, _ := setup(t)
	if err := store.Save([]Hook{testHook("rl")}); err != nil {
		t.Fatal(err)
	}
	srv.MaxPerMinute = 2
	codes := []int{}
	for i := 0; i < 4; i++ {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, post("/hook/rl", ""))
		codes = append(codes, w.Code)
	}
	if codes[0] != 202 || codes[1] != 202 || codes[2] != 429 || codes[3] != 429 {
		t.Fatalf("rate limit sequence wrong: %v", codes)
	}
}

func TestTriggerLogPersisted(t *testing.T) {
	srv, store, _ := setup(t)
	if err := store.Save([]Hook{testHook("lg")}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post("/hook/lg", ""))
	if w.Code != 202 {
		t.Fatalf("want 202, got %d", w.Code)
	}
	if len(srv.Log.History()) == 0 {
		t.Fatal("history should not be empty")
	}
	// 文件存在。
	if _, err := os.Stat(srv.Log.path); err != nil {
		t.Fatalf("log file: %v", err)
	}
}

func TestStoreRoundtripAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hooks.json")
	s := NewStore(p)
	if hs, _ := s.List(); len(hs) != 0 {
		t.Fatal("missing file = empty list")
	}
	if err := s.Save([]Hook{testHook("a"), {ID: "b", Task: "x", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	hs, _ := s.List()
	if len(hs) != 2 {
		t.Fatalf("want 2, got %d", len(hs))
	}
	// 损坏文件 = 空（跳过不致命）。
	_ = os.WriteFile(p, []byte("{broken"), 0o644)
	if hs, _ := s.List(); len(hs) != 0 {
		t.Fatal("corrupt file = empty list")
	}
}

func TestEmptyTaskRejected(t *testing.T) {
	srv, store, _ := setup(t)
	h := testHook("et")
	h.Task = "  "
	if err := store.Save([]Hook{h}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post("/hook/et", ""))
	if w.Code != 400 {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestRunnerPanicContained(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "h.json"))
	log := newLog("")
	srv := NewServer(store, log, func(h Hook) { panic("boom") })
	if err := store.Save([]Hook{testHook("p")}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post("/hook/p", ""))
	if w.Code != 202 {
		t.Fatalf("want 202, got %d", w.Code)
	}
	deadline := time.Now().Add(time.Second)
	for srv.Log.History() == nil || len(srv.Log.History()) < 2 {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// panic 后仍有 triggered 记录（record 在 goroutine 之外先记 202）。
	if len(srv.Log.History()) == 0 {
		t.Fatal("at least the 202 record should exist")
	}
}

func TestJSONReply(t *testing.T) {
	w := httptest.NewRecorder()
	reply(w, 202, map[string]any{"ok": true})
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("unexpected body: %v", body)
	}
	fmt.Fprint(os.Stderr, "")
}
