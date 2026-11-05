// 知识库（doc/）REST 端点测试：列表/读/写/删 + 鉴权（账号角色/clientToken）+
// 防目录穿越校验。见 docs.go。
package remote

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newDocsTestServer 组装带知识库目录与统一账号表的测试服务器（httptest 级，
// 不落盘 accounts，直接经 NewHub 注入 users）。
func newDocsTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	// 统一账号：admin 可写删，user 只读。
	users := []Account{
		{User: "admin", Pass: "apass", Role: "admin"},
		{User: "user", Pass: "upass", Role: "user"},
	}
	hub := NewHub(dir, map[string]string{"tok-n": "node-x"}, "client-tok", nil, nil, users)
	t.Cleanup(hub.Stop) // 停 persistLoop/巡检：防落盘与 TempDir 清理竞态（Windows unlinkat flake，下同）
	srv := NewServer(hub, "")
	srv.SetRESTCredentials("", "", "client-tok") // clientToken 走 REST 凭据（docAuth 读 s.restToken）
	srv.SetDocsDir(dir)
	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)
	return ts, dir
}

// docGet / docPost REST 辅助（附带 Basic 认证）。
func docGet(ts *httptest.Server, path, user, pass string) *http.Response {
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func docPost(ts *httptest.Server, path, body, user, pass string) *http.Response {
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

// respJSON 解析 {ok, data|error} 信封。
func respJSON(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("响应 JSON 解析失败: %v", err)
	}
	return m
}

func TestDocs_CRUD(t *testing.T) {
	ts, dir := newDocsTestServer(t)

	// ---- 初始空列表 ----
	resp := docGet(ts, "/api/docs", "admin", "apass")
	if resp.StatusCode != 200 {
		t.Fatalf("list 状态码 = %d", resp.StatusCode)
	}
	m := respJSON(t, resp)
	if m["ok"] != true {
		t.Fatalf("list ok = %v", m["ok"])
	}

	// ---- 写入（创建） ----
	resp = docPost(ts, "/api/docs/write", `{"name":"hello.md","content":"# 你好"}`, "admin", "apass")
	if resp.StatusCode != 200 {
		t.Fatalf("write 状态码 = %d", resp.StatusCode)
	}
	b, err := os.ReadFile(dir + "/hello.md")
	if err != nil || string(b) != "# 你好" {
		t.Fatalf("写入内容不匹配: %q err=%v", b, err)
	}

	// ---- 读取 ----
	resp = docGet(ts, "/api/docs/read?name=hello.md", "admin", "apass")
	if resp.StatusCode != 200 {
		t.Fatalf("read 状态码 = %d", resp.StatusCode)
	}
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if strings.TrimSpace(string(body[:n])) != "# 你好" {
		t.Fatalf("read 内容 = %q", string(body[:n]))
	}

	// ---- 列表含新文档 ----
	resp = docGet(ts, "/api/docs", "admin", "apass")
	m = respJSON(t, resp)
	data, _ := json.Marshal(m["data"])
	if !strings.Contains(string(data), "hello.md") {
		t.Fatalf("列表缺 hello.md: %s", data)
	}

	// ---- 覆盖写入 ----
	resp = docPost(ts, "/api/docs/write", `{"name":"hello.md","content":"v2"}`, "admin", "apass")
	if resp.StatusCode != 200 {
		t.Fatalf("rewrite 状态码 = %d", resp.StatusCode)
	}
	if b, _ := os.ReadFile(dir + "/hello.md"); string(b) != "v2" {
		t.Fatalf("覆盖后内容 = %q", b)
	}

	// ---- 删除 ----
	resp = docPost(ts, "/api/docs/delete", `{"name":"hello.md"}`, "admin", "apass")
	if resp.StatusCode != 200 {
		t.Fatalf("delete 状态码 = %d", resp.StatusCode)
	}
	if _, err := os.Stat(dir + "/hello.md"); !os.IsNotExist(err) {
		t.Fatalf("文件应已删除, err=%v", err)
	}
	resp.Body.Close()
}

func TestDocs_Auth(t *testing.T) {
	ts, dir := newDocsTestServer(t)

	// 普通账号可读。
	if resp := docGet(ts, "/api/docs", "user", "upass"); resp.StatusCode != 200 {
		t.Fatalf("user read list 状态码 = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 普通账号不可写 → 403。
	resp := docPost(ts, "/api/docs/write", `{"name":"x.md","content":"y"}`, "user", "upass")
	respJSON(t, resp)
	if resp.StatusCode != 403 {
		t.Fatalf("user write 状态码 = %d（期望 403）", resp.StatusCode)
	}
	resp = docPost(ts, "/api/docs/delete", `{"name":"x.md"}`, "user", "upass")
	respJSON(t, resp)
	if resp.StatusCode != 403 {
		t.Fatalf("user delete 状态码 = %d（期望 403）", resp.StatusCode)
	}

	// 错误密码 → 401。
	resp = docGet(ts, "/api/docs", "user", "wrong")
	respJSON(t, resp)
	if resp.StatusCode != 401 {
		t.Fatalf("bad pass 状态码 = %d（期望 401）", resp.StatusCode)
	}

	// clientToken 可读写。
	req, _ := http.NewRequest("POST", ts.URL+"/api/docs/write", strings.NewReader(`{"name":"tk.md","content":"T"}`))
	req.Header.Set("X-Access-Token", "client-tok")
	r2, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respJSON(t, r2)
	if r2.StatusCode != 200 {
		t.Fatalf("clientToken write 状态码 = %d", r2.StatusCode)
	}
	if b, err := os.ReadFile(dir + "/tk.md"); err != nil || string(b) != "T" {
		t.Fatalf("clientToken 写入未落盘: err=%v b=%q", err, b)
	}
}

func TestDocs_Traversal(t *testing.T) {
	ts, dir := newDocsTestServer(t)
	_ = dir

	// 目录穿越 / 非法名全部 400。
	for _, name := range []string{
		"../secret.md", "..\\secret.md", "a\\b.md", ".hidden.md",
		"C:evil.md", "evil.exe", "evil", "", strings.Repeat("a", 600) + ".md",
	} {
		resp := docGet(ts, "/api/docs/read?name="+name, "admin", "apass")
		respJSON(t, resp)
		if resp.StatusCode != 400 {
			t.Errorf("name=%q 状态码 = %d（期望 400）", name, resp.StatusCode)
		}
	}

	// 写入同样拒绝。
	resp := docPost(ts, "/api/docs/write", `{"name":"../x.md","content":"y"}`, "admin", "apass")
	respJSON(t, resp)
	if resp.StatusCode != 400 {
		t.Fatalf("traversal write 状态码 = %d（期望 400）", resp.StatusCode)
	}
}

func TestDocs_Disabled(t *testing.T) {
	users := []Account{{User: "admin", Pass: "p", Role: "admin"}}
	hub := NewHub(t.TempDir(), nil, "", nil, nil, users)
	defer hub.Stop()
	srv := NewServer(hub, "") // 未 SetDocsDir
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	resp := docGet(ts, "/api/docs", "admin", "p")
	m := respJSON(t, resp)
	if resp.StatusCode != 404 {
		t.Fatalf("未启用时状态码 = %d（期望 404）", resp.StatusCode)
	}
	if ok, _ := m["ok"].(bool); ok {
		t.Fatal("未启用时 ok 应为 false")
	}
}

func TestDocs_URLEncodedName(t *testing.T) {
	// 中文名文件端到端（query 参数 → 读取回原内容；read 成功返回纯文本）。
	ts, dir := newDocsTestServer(t)
	name := "中文文档.md"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("内容X"), 0o644); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/docs/read", nil)
	req.SetBasicAuth("admin", "apass")
	q := req.URL.Query()
	q.Set("name", name) // Query().Encode() 负责 URL 编码
	req.URL.RawQuery = q.Encode()
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("中文名读取状态码 = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(b)) != "内容X" {
		t.Fatalf("中文名读取内容 = %q", string(b))
	}
}

func TestDocs_SubdirCRUD(t *testing.T) {
	// 知识库子目录端到端：写入子目录路径 → 列表含相对路径 → 读取 → 覆盖 → 删除。
	ts, dir := newDocsTestServer(t)

	// 写入子目录文档（父目录不存在时自动创建）。
	name := "01-产品与需求/PRD.md"
	docPost(ts, "/api/docs/write", `{"name":"`+name+`","content":"# PRD v1"}`, "admin", "apass")
	if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))); err != nil || string(b) != "# PRD v1" {
		t.Fatalf("子目录写入未落盘: err=%v b=%q", err, b)
	}

	// 列表递归包含子目录文档（相对正斜杠路径）。
	resp := docGet(ts, "/api/docs", "admin", "apass")
	m := respJSON(t, resp)
	data, _ := json.Marshal(m["data"])
	if !strings.Contains(string(data), name) {
		t.Fatalf("列表缺子目录文档 %s: %s", name, data)
	}

	// 读取子目录文档。
	resp = docGet(ts, "/api/docs/read?name="+url.QueryEscape(name), "admin", "apass")
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.TrimSpace(string(body[:n])) != "# PRD v1" {
		t.Fatalf("子目录读取失败: status=%d body=%q", resp.StatusCode, string(body[:n]))
	}

	// 覆盖写入子目录。
	docPost(ts, "/api/docs/write", `{"name":"`+name+`","content":"v2"}`, "admin", "apass")
	if b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))); string(b) != "v2" {
		t.Fatalf("子目录覆盖失败: %q", b)
	}

	// 删除子目录文档。
	docPost(ts, "/api/docs/delete", `{"name":"`+name+`"}`, "admin", "apass")
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); !os.IsNotExist(err) {
		t.Fatalf("子目录文档应已删除, err=%v", err)
	}
}

func TestDocs_ListRecursiveOrder(t *testing.T) {
	// 分类后知识库列表：根 + 子目录文档混合，扁平排序且相对路径正确。
	ts, dir := newDocsTestServer(t)

	// 预置：根文档 + 两个子目录文档（含中文目录名）。
	for _, n := range []string{"README.md", "02-架构设计/architecture.md", "06-移动端/指南.md"} {
		p := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	resp := docGet(ts, "/api/docs", "admin", "apass")
	m := respJSON(t, resp)
	arr, _ := m["data"].([]interface{})
	names := make([]string, 0, len(arr))
	for _, it := range arr {
		obj := it.(map[string]interface{})
		names = append(names, obj["name"].(string))
	}
	if len(names) != 3 {
		t.Fatalf("列表条目数 = %d（期望 3）: %v", len(names), names)
	}
	// 排序按完整相对路径字符串升序："02-..." < "06-..." < "README.md"。
	if names[0] != "02-架构设计/architecture.md" || names[1] != "06-移动端/指南.md" || names[2] != "README.md" {
		t.Fatalf("列表排序/相对路径错误: %v", names)
	}
}
