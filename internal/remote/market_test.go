package remote

// market_test.go —— 插件市场 REST 端到端（httptest）：
// 列表（空）→ 发布（节点令牌鉴权）→ 列表 → 下载（字节一致）→ 重复发布升级 → 删除。
// 鉴权负路径：无凭据发布 401 / 无效 id 400 / 未启用 404。

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// newMarketTestServer httptest 服务器 + REST 凭据（发布用 X-Node-Token 走 imageAuth；
// 列表/删除走 Basic）。
func newMarketTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	hub := NewHub(t.TempDir(), map[string]string{"node-default": "tok-1"}, "", nil, nil, []Account{
		{User: "admin", Pass: "admin123", Role: "admin"},
	})
	t.Cleanup(hub.Stop) // 停 persistLoop/巡检：防落盘与 TempDir 清理竞态（Windows unlinkat flake，下同）
	srv := NewServer(hub, "")
	srv.SetAgentTokens(map[string]string{"tok-1": "node-default"}) // 发布鉴权（X-Node-Token；与 hub tokens 同源快照）
	srv.SetRESTCredentials("admin", "admin123", "")
	srv.SetMarketDir(filepath.Join(t.TempDir(), "market"))
	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)
	return ts, srv
}

// marketZip 构造测试 zip（内含 plugin.json）。
func marketZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("rea/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`{"id":"rea","name":"REA 逆向分析","version":"6.4.0"}`))
	zw.Close()
	return buf.Bytes()
}

// marketPost 发布 multipart（tok 空 = 不带节点令牌）。
func marketPost(t *testing.T, ts *httptest.Server, tok string, fields map[string]string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", "pkg.zip")
	_, _ = fw.Write(marketZip(t))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/market", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if tok != "" {
		req.Header.Set("X-Node-Token", tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp
}

// TestMarket_CRUD 全链路：发布 → 列表 → 下载字节一致 → 同 ID 升级 → 删除。
func TestMarket_CRUD(t *testing.T) {
	ts, _ := newMarketTestServer(t)

	// 发布（节点令牌鉴权）。
	resp := marketPost(t, ts, "tok-1", map[string]string{
		"id": "rea", "kind": "plugin", "name": "REA", "version": "6.4.0",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish = %d", resp.StatusCode)
	}

	// 列表可见（Basic 鉴权）。
	r2, err := http.Get(ts.URL + "/api/market?token=") // 无凭据应 401
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("list without creds = %d, want 401", r2.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/market", nil)
	req.SetBasicAuth("admin", "admin123")
	r3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b3, _ := io.ReadAll(r3.Body)
	r3.Body.Close()
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("list = %d: %s", r3.StatusCode, b3)
	}
	var out struct {
		OK   bool         `json:"ok"`
		Data []MarketItem `json:"data"`
	}
	if json.Unmarshal(b3, &out) != nil || !out.OK || len(out.Data) != 1 {
		t.Fatalf("list body = %s", b3)
	}
	if out.Data[0].PublishedBy != "node-default" {
		t.Fatalf("publishedBy = %q", out.Data[0].PublishedBy)
	}

	// 下载字节一致。
	r4, err := http.Get(ts.URL + "/api/market/download?id=rea")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r4.Body)
	r4.Body.Close()
	if r4.StatusCode != http.StatusOK || !bytes.Equal(got, marketZip(t)) {
		t.Fatalf("download = %d, %d bytes", r4.StatusCode, len(got))
	}

	// 同 ID 升级（发布第二次，列表仍 1 条，时间戳更新）。
	resp5 := marketPost(t, ts, "tok-1", map[string]string{"id": "rea", "kind": "plugin", "name": "REA", "version": "6.5.0"})
	if resp5.StatusCode != http.StatusOK {
		t.Fatalf("upgrade publish = %d", resp5.StatusCode)
	}
	req6, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/market", nil)
	req6.SetBasicAuth("admin", "admin123")
	r6, _ := http.DefaultClient.Do(req6)
	b6, _ := io.ReadAll(r6.Body)
	r6.Body.Close()
	var out6 struct {
		Data []MarketItem `json:"data"`
	}
	_ = json.Unmarshal(b6, &out6)
	if len(out6.Data) != 1 || out6.Data[0].Version != "6.5.0" {
		t.Fatalf("after upgrade: %+v", out6)
	}

	// 删除（Basic）→ 404。
	rq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/market?id=rea", nil)
	rq.SetBasicAuth("admin", "admin123")
	r7, _ := http.DefaultClient.Do(rq)
	r7.Body.Close()
	if r7.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", r7.StatusCode)
	}
	r8, _ := http.Get(ts.URL + "/api/market/download?id=rea")
	r8.Body.Close()
	if r8.StatusCode != http.StatusNotFound {
		t.Fatalf("download after delete = %d, want 404", r8.StatusCode)
	}
}

// TestMarket_AuthAndValidation 鉴权负路径与参数校验。
func TestMarket_AuthAndValidation(t *testing.T) {
	ts, _ := newMarketTestServer(t)

	// 无凭据发布 401。
	resp := marketPost(t, ts, "", map[string]string{"id": "x"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("publish without auth = %d, want 401", resp.StatusCode)
	}
	// 带令牌但 id 非法 400。
	resp2 := marketPost(t, ts, "tok-1", map[string]string{"id": "../evil"})
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", resp2.StatusCode)
	}
	// 类别非法 400。
	resp3 := marketPost(t, ts, "tok-1", map[string]string{"id": "x", "kind": "bogus"})
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad kind = %d, want 400", resp3.StatusCode)
	}
	// 下载不存在的条目 404。
	r4, _ := http.Get(ts.URL + "/api/market/download?id=nope")
	r4.Body.Close()
	if r4.StatusCode != http.StatusNotFound {
		t.Fatalf("download missing = %d", r4.StatusCode)
	}
}
