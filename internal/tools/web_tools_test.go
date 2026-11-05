package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ---- web_fetch ----

func TestWebFetchHTMLToText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>T</title><style>body{color:red}</style></head>
<body><nav>skip me</nav><h1>Hello</h1><p>World &amp; more</p><script>alert(1)</script>
<div>tail text</div></body></html>`)
	}))
	defer srv.Close()

	out, err := (&WebFetchTool{}).Execute(context.Background(),
		fmt.Sprintf(`{"url":%q}`, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "World & more") || !strings.Contains(out, "tail text") {
		t.Fatalf("missing visible content: %q", out)
	}
	if strings.Contains(out, "alert(1)") || strings.Contains(out, "color:red") || strings.Contains(out, "skip me") {
		t.Fatalf("noise not stripped: %q", out)
	}
}

func TestWebFetchJSONPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"data":"<b>raw-kept</b>"}`)
	}))
	defer srv.Close()
	out, err := (&WebFetchTool{}).Execute(context.Background(), fmt.Sprintf(`{"url":%q}`, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"ok":true`) || strings.Contains(out, "\nstatus:") == false {
		t.Fatalf("json passthrough failed: %q", out)
	}
	if !strings.Contains(out, "<b>raw-kept</b>") {
		t.Fatalf("json body must not be html-converted: %q", out)
	}
}

// ---- web_search（自建 mock 后端：SEARXNG_URL 走 json 接口，稳定可测）----

func TestWebSearchSearXNGBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "format=json") {
			http.Error(w, "need format=json", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "Result One", "url": "https://a.example/1", "content": "snippet one"},
				{"title": "Result Two", "url": "https://b.example/2", "content": "snippet two"},
			},
		})
	}))
	defer srv.Close()

	os.Setenv("SEARXNG_URL", srv.URL)
	defer os.Unsetenv("SEARXNG_URL")
	// 防本机真有 BRAVE_API_KEY 干扰（先 DDG 后 Brave；DDG 会失败/超时外网——
	// 但 SEARXNG 分支仅在 DDG 无结果后尝试；离线环境下 DDG 失败即落到这里）。
	os.Unsetenv("BRAVE_API_KEY")

	out, err := (&WebSearchTool{}).Execute(context.Background(), `{"query":"go generics","max_results":2,"timeout_sec":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Result One") || !strings.Contains(out, "https://b.example/2") {
		t.Fatalf("searxng results missing: %q", out)
	}
	if !strings.Contains(out, "searxng") {
		t.Fatalf("backend label missing: %q", out)
	}
}

// ---- HTML 转换单元 ----

func TestHTMLToTextUnits(t *testing.T) {
	in := `<h2>Header</h2><script>var x=1;</script><p>a&amp;b</p><!-- c -->tail`
	out := htmlToText(in)
	if !strings.Contains(out, "Header") || !strings.Contains(out, "a&b") || !strings.Contains(out, "tail") {
		t.Fatalf("bad conversion: %q", out)
	}
	if strings.Contains(out, "var x") || strings.Contains(out, "<") {
		t.Fatalf("tags/script leaked: %q", out)
	}
	if !isJSONContentType("application/JSON") || isJSONContentType("text/html") {
		t.Fatal("isJSONContentType wrong")
	}
}
