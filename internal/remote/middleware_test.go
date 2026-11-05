package remote

// middleware_test.go —— 统一中间件链式注册点单测：
// Chain 声明顺序语义（先声明者最外层）、Server.use 全局链 Start 包裹、
// 空中间件一行接入与顺序可编排（规划板验收项）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// tagMW 构造打标中间件：请求经过时向 *trace 追加标记（顺序断言用）。
func tagMW(trace *[]string, tag string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*trace = append(*trace, tag)
			next.ServeHTTP(w, r)
		})
	}
}

// TestChainOrder 链式顺序：Chain(final, a, b) 请求依次经过 a→b→final（先声明者最外层）。
func TestChainOrder(t *testing.T) {
	var trace []string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace = append(trace, "final")
	})
	h := Chain(final, tagMW(&trace, "a"), tagMW(&trace, "b"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if strings.Join(trace, ">") != "a>b>final" {
		t.Fatalf("Chain 顺序 = %v, want [a b final]", trace)
	}
}

// TestChainEmpty 空中间件列表零包装：请求直达 final（与旧版直挂等价）。
func TestChainEmpty(t *testing.T) {
	hit := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true })
	Chain(final).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !hit {
		t.Fatal("空链应直达 final handler")
	}
}

// TestServerUseGlobalChain 全局链：use 注册的两个空中间件经 Start 包在 mux 外层，
// 顺序可编排（先注册者先经过），路由正常响应。
func TestServerUseGlobalChain(t *testing.T) {
	hub := NewHub("", map[string]string{"tok-mw": "node-mw"}, "", nil, nil, nil)
	srv := NewServer(hub, "")
	var trace []string
	// 一行接入一个空中间件（验收项）：仅 use + Middleware 字面量，无路由改动。
	srv.use(tagMW(&trace, "outer"))
	srv.use(tagMW(&trace, "inner"))
	addr := "127.0.0.1:18919"
	go func() { _ = srv.Start(addr) }() // Start 内 Chain(s.mux, globalMW...) 真实包裏
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	resp, err := http.Get("http://" + addr + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", resp.StatusCode)
	}
	if strings.Join(trace, ">") != "outer>inner" {
		t.Fatalf("全局链经过顺序 = %v, want [outer inner]（先注册者最外层）", trace)
	}
}
