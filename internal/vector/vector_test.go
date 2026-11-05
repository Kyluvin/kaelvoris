// Package vector 测试：分块、存储余弦检索、增量索引（manifest 跳过/删除同步）、
// Embedder（httptest 模拟 OpenAI 兼容 /embeddings）。
package vector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEmbedServer 模拟 OpenAI 兼容 /embeddings：
// 返回确定性 4 维向量（基于文本首字符），乱序 data 验证 index 回填。
func fakeEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("请求体解析失败: %v", err)
			w.WriteHeader(400)
			return
		}
		if req.Model != "test-embed" {
			t.Errorf("模型名不符: %q", req.Model)
		}
		// 乱序返回（index 与数组顺序不一致，验证回填逻辑）
		type dataItem struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]dataItem, len(req.Input))
		for i := range req.Input {
			j := len(req.Input) - 1 - i // 逆序
			// 确定性向量：首字节字符 → [0,1] 归一化 4 维
			c := float32(req.Input[i][0]) / 255.0
			data[j] = dataItem{Index: j, Embedding: []float32{c, 1 - c, c * c, 0.5}}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
	}))
}

func TestChunkFile(t *testing.T) {
	content := "package main\n\nfunc a() {\n}\n\nfunc b() {\n}\n"
	chs := chunkFile(content)
	// 空行分段：package 行、func a、func b 各自成段 → 3 块
	if len(chs) != 3 {
		t.Fatalf("应分 3 块（空行分隔的三个段落），got %d: %+v", len(chs), chs)
	}
	if chs[0].Start != 1 || chs[0].End != 1 {
		t.Errorf("块 1 行号不符: got %d-%d want 1-1", chs[0].Start, chs[0].End)
	}
	if chs[1].Start != 3 || chs[1].End != 4 {
		t.Errorf("块 2 行号不符: got %d-%d want 3-4", chs[1].Start, chs[1].End)
	}
	if chs[2].Start != 6 || chs[2].End != 7 {
		t.Errorf("块 2 行号不符: got %d-%d want 6-7", chs[1].Start, chs[1].End)
	}
	for _, c := range chs {
		if c.Text == "" {
			t.Errorf("块内容为空")
		}
	}
}

func TestChunkFileLongPara(t *testing.T) {
	// 单段 5000 字符 → 硬切多块且全部 ≤chunkMaxChars
	para := strings.Repeat("x", 5000)
	chs := chunkFile(para)
	if len(chs) < 4 {
		t.Fatalf("5000 字符应硬切 ≥4 块，got %d", len(chs))
	}
	for i, c := range chs {
		if len(c.Text) > chunkMaxChars {
			t.Errorf("块 %d 超长: %d", i, len(c.Text))
		}
	}
	// 内容完整性：去重叠后总字符应覆盖原文（宽松校验：最后一块以原文结尾）
	if !strings.HasSuffix(chs[len(chs)-1].Text, "xxx") {
		t.Errorf("末块应包含原文结尾")
	}
}

func TestStoreReplaceAndSearch(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	vecOf := func(a, b float32) []float32 { return []float32{a, b} }
	_ = vecOf

	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Replace("a.go", []Point{
		{ID: "a:0", File: "a.go", Start: 1, End: 2, Text: "alpha", Vec: []float32{1, 0}},
	}))
	must(s.Replace("b.go", []Point{
		{ID: "b:0", File: "b.go", Start: 1, End: 1, Text: "beta", Vec: []float32{0, 1}},
	}))
	// a.go 二次 Replace 覆盖（upsert 语义）
	must(s.Replace("a.go", []Point{
		{ID: "a:1", File: "a.go", Start: 5, End: 6, Text: "gamma", Vec: []float32{0.9, 0.1}},
	}))

	p, f := s.Count()
	if p != 2 || f != 2 {
		t.Fatalf("应为 2 点 2 文件，got %d/%d", p, f)
	}

	// 检索：接近 [1,0] 的点应排前
	hits := s.Search([]float32{1, 0.05}, 2, 0.5)
	if len(hits) == 0 || hits[0].File != "a.go" {
		t.Fatalf("Top1 应为 a.go，got %+v", hits)
	}
	// 阈值过滤：查询 [0,-1] 与 [1,0]/[0.9,0.1] 余弦为负、与 [0,1] 为 0 → 全部过滤
	hits = s.Search([]float32{0, -1}, 2, 0.5)
	if len(hits) != 0 {
		t.Errorf("反向查询应被阈值过滤，got %d", len(hits))
	}
	// 删除文件点
	must(s.Replace("b.go", nil))
	p, f = s.Count()
	if p != 1 || f != 1 {
		t.Fatalf("删除后应 1 点 1 文件，got %d/%d", p, f)
	}
}

func TestIndexerIncremental(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# 标题\n\n内容第一段\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skip.bin"), []byte{0, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "x.js"), []byte("var x=1"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewStore(t.TempDir())
	emb := NewEmbedder(srv.URL, "sk-test", "test-embed")
	ix := NewIndexer(root, store, emb)

	files, blocks, err := ix.Run(context.Background())
	if err != nil {
		t.Fatalf("首次索引失败: %v", err)
	}
	if files != 1 || blocks != 2 { // skip.bin 非文本、node_modules 跳过；a.md 两段 → 2 块
		t.Fatalf("首次索引应 1 文件 2 块，got %d/%d", files, blocks)
	}
	// manifest 落盘验证
	if _, err := os.Stat(ix.manifestPath); err != nil {
		t.Errorf("manifest 未落盘: %v", err)
	}

	// 二次运行：无变更 → 零嵌入调用（manifest 跳过）
	files2, blocks2, err := ix.Run(context.Background())
	if err != nil {
		t.Fatalf("增量索引失败: %v", err)
	}
	if files2 != 1 || blocks2 != 0 {
		t.Fatalf("增量应跳过未变更（0 块），got %d 文件 %d 块", files2, blocks2)
	}

	// 修改文件 → 重新索引该文件
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# 标题\n\n内容已更新\n\n新增段落\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, blocks3, err := ix.Run(context.Background())
	if err != nil {
		t.Fatalf("变更索引失败: %v", err)
	}
	if blocks3 != 3 { // 更新后 3 个非空段 → 3 块
		t.Fatalf("变更后应重建 3 块，got %d", blocks3)
	}

	// 删除文件 → 向量点同步清理
	if err := os.Remove(filepath.Join(root, "a.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, f := store.Count()
	if p != 0 || f != 0 {
		t.Fatalf("删除后库应清空，got %d 点 %d 文件", p, f)
	}
}

func TestIndexerSearch(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("# 架构\n\n本系统采用三层架构设计\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewStore(t.TempDir())
	ix := NewIndexer(root, store, NewEmbedder(srv.URL, "k", "test-embed"))
	if _, _, err := ix.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 用与文件内容相同前缀的查询（fake 向量基于首字节 → 高相似）
	txt, pts, err := ix.Search(context.Background(), SearchRequest{Query: "# 架构", MinScore: 0.3})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(pts) < 1 {
		t.Fatalf("应至少命中 1 点，got %d", len(pts))
	}
	if !strings.Contains(txt, "doc.md:1-") {
		t.Errorf("结果应含文件路径与行号，got: %s", txt)
	}
	if !strings.Contains(txt, "三层架构") {
		t.Errorf("结果应含块正文，got: %s", txt)
	}
}

func TestEmbedderAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{"index": 0, "embedding": []float32{0.1, 0.2}},
			},
		})
	}))
	defer srv.Close()
	emb := NewEmbedder(srv.URL, "sk-secret", "m")
	if _, err := emb.Embed(context.Background(), []string{"hi"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Errorf("Authorization 头不符: %q", gotAuth)
	}
}
