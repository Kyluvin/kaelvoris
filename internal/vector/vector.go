// Package vector 向量知识库（对齐 Zoo-Code src/services/code-index 语义的最小落地）：
//
//	文件扫描/分块 → OpenAI 兼容 /embeddings 向量化 → 本地持久化存储（JSON，余弦相似度检索）
//	→ codebase_search 工具供 Agent 检索 → 提示词注入向量能力段。
//
// 与 Zoo-Code 的对应关系（移植蓝本）：
//   - embedders/openai-compatible.ts → Embedder（POST {baseURL}/embeddings）
//   - processors/scanner.ts          → chunkFile（分块）/ Indexer（增量扫描 + 批处理）
//   - vector-store/qdrant-client.ts  → Store（本地 JSON 替代 Qdrant：零部署依赖，
//     余弦相似度暴力检索足够中小型代码库/文档库规模）
//   - CodebaseSearchTool.ts          → internal/tools 内 codebase_search 工具
//   - manager.ts（开关/状态）+ cache-manager.ts → Indexer（manifest 增量：
//     mtime+size+hash 未变跳过，删除文件同步清理）
//
// 设计约定（AGENTS.md）：持久化 JSON 落 ~/.kaelvoris/vector/；文件损坏按
// 「跳过不致命」处理；零重依赖（net/http + encoding/json 原生实现）。
package vector

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 配置 ----------

// Config 向量化配置（持久化于 desktop.json 的 vector 字段）。
type Config struct {
	Enabled    bool    `json:"enabled"`              // 总开关：开启后索引与 codebase_search 工具可用
	AnswerWith bool    `json:"answerWith"`           // 向量回答：写码/答题前自动检索注入
	Dir        string  `json:"dir"`                  // 待向量化目录（项目 docs/、doc/ 等）
	Schedule   string  `json:"schedule"`             // 自动化时间（cron 表达式，空 = 不自动）
	EmbedURL   string  `json:"embedURL"`             // OpenAI 兼容 embeddings 端点（空 = 用主 BaseURL）
	EmbedModel string  `json:"embedModel"`           // embedding 模型（如 text-embedding-3-small / bge-m3）
	EmbedKey   string  `json:"embedKey,omitempty"`   // API Key（空 = 用主 APIKey）
	MaxResults int     `json:"maxResults,omitempty"` // 检索条数上限（默认 8）
	MinScore   float64 `json:"minScore,omitempty"`   // 相似度阈值（默认 0.35，对齐 Zoo-Code 0.4 邻域）
}

// MaxResultsOr 默认检索条数。
func (c *Config) MaxResultsOr() int {
	if c.MaxResults > 0 {
		return c.MaxResults
	}
	return 8
}

// MinScoreOr 默认相似度阈值。
func (c *Config) MinScoreOr() float64 {
	if c.MinScore > 0 {
		return c.MinScore
	}
	return 0.35
}

// ---------- 分块（chunker，对应 processors/scanner.ts 的块切分） ----------

// 分块参数：按空行分段聚合到 ~chunkMaxChars 字符；超长单段硬切并在切点
// 回看 chunkOverlap 字符保留上下文衔接。
const (
	chunkMaxChars = 1200
	chunkOverlap  = 100
)

// Chunk 单个文本块（对应 Zoo-Code CodeBlock：语义单位 + 行号元数据）。
type Chunk struct {
	Text  string `json:"text"`  // 块正文
	Start int    `json:"start"` // 起始行（1-based）
	End   int    `json:"end"`   // 结束行（1-based，含）
}

// chunkFile 把文件内容分块：空行分段（代码函数间/文档段落天然以空行分隔），
// 段落聚合至 chunkMaxChars；超长单段硬切（行号统一标注段落起止，内容完整
// 优先于行号精确——极长单段如压缩 JS/JSON 场景）。
func chunkFile(content string) []Chunk {
	lines := strings.Split(content, "\n")
	var chunks []Chunk

	pushChunk := func(text string, startLine int) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		chunks = append(chunks, Chunk{
			Text:  text,
			Start: startLine + 1,
			End:   startLine + strings.Count(text, "\n") + 1,
		})
	}

	var cur []string
	curStart := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		text := strings.Join(cur, "\n")
		if len(text) <= chunkMaxChars {
			pushChunk(text, curStart)
		} else {
			for _, piece := range hardSplit(text) {
				pushChunk(piece, curStart)
			}
		}
		cur = nil
	}

	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			flush()
			continue
		}
		if len(cur) == 0 {
			curStart = i
		}
		cur = append(cur, ln)
	}
	flush()
	return chunks
}

// hardSplit 超长段落硬切：返回多个 ≤chunkMaxChars 的块（块间以 chunkOverlap
// 字符重叠衔接，保上下文连续性）。切割预算 = chunkMaxChars − 重叠尾巴长度，
// 保证「重叠 + 切片」拼出的块不超限。
func hardSplit(text string) []string {
	var out []string
	overlapTail := ""
	for text != "" {
		budget := chunkMaxChars - len(overlapTail)
		if budget < chunkMaxChars/3 {
			budget = chunkMaxChars / 3 // 重叠过长时压缩预算（块不至于过碎）
		}
		cut := text
		if len(cut) > budget {
			cut = cut[:budget]
			if nl := strings.LastIndexByte(cut, '\n'); nl > budget/2 {
				cut = cut[:nl]
			}
		}
		if piece := strings.TrimSpace(overlapTail + cut); piece != "" {
			out = append(out, piece)
		}
		overlapTail = overlapTailOf(cut, chunkOverlap)
		text = strings.TrimLeft(text[len(cut):], "\n")
		if strings.TrimSpace(text) == "" {
			break
		}
	}
	return out
}

// overlapTailOf 取 s 末尾不超过 max 的片段（从最近行首开始）。
func overlapTailOf(s string, max int) string {
	if len(s) > max {
		s = s[len(s)-max:]
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	return strings.TrimSpace(s)
}

// fileID 文件唯一标识（路径 hash）：块 ID 前缀。
func fileID(path string) string {
	h := sha1.Sum([]byte(path))
	return hex.EncodeToString(h[:])[:16]
}

// ---------- 存储（本地 JSON 向量库，对应 vector-store/qdrant-client.ts） ----------

// Point 一条向量（对应 Zoo-Code PointStruct）。
type Point struct {
	ID     string    `json:"id"`     // fileID:序号
	File   string    `json:"file"`   // 相对索引根的路径（/ 分隔）
	Start  int       `json:"start"`  // 起始行
	End    int       `json:"end"`    // 结束行
	Text   string    `json:"text"`   // 块正文（命中后直接回给 LLM）
	Vec    []float32 `json:"vec"`    // 嵌入向量
	Source string    `json:"source"` // 索引根目录（多目录并存时区分来源）
}

// Store JSON 文件向量库（~/.kaelvoris/vector/store.json）。
// 每次操作重读磁盘（进程内/多实例共享文件不撕裂）；保存整体重写——中小规模
// （万级块）延迟可接受，避免引入向量数据库依赖（对齐「接口最小化」约定）。
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore 打开（或创建）位于 dir 目录的向量库。
func NewStore(dir string) *Store {
	return &Store{path: filepath.Join(dir, "store.json")}
}

// load 从磁盘加载（不存在/损坏 = 空库，不致命）。
func (s *Store) load() []Point {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	var pts []Point
	if json.Unmarshal(b, &pts) != nil {
		return nil
	}
	return pts
}

// save 整体重写。
func (s *Store) save(pts []Point) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(pts)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}

// Replace 替换某文件的全部向量点（pts 为 nil/空 = 删除该文件全部点）。
func (s *Store) Replace(file string, pts []Point) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.load()
	out := make([]Point, 0, len(all)+len(pts))
	for _, p := range all {
		if p.File == file {
			continue
		}
		out = append(out, p)
	}
	out = append(out, pts...)
	return s.save(out)
}

// Count 统计点数与文件数。
func (s *Store) Count() (points, files int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.load()
	seen := map[string]bool{}
	for _, p := range all {
		seen[p.File] = true
	}
	return len(all), len(seen)
}

// Clear 清空库。
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(nil)
}

// Search 向量检索：余弦相似度 Top-N（score ≥ minScore；对应 vectorStore.search）。
func (s *Store) Search(queryVec []float32, maxResults int, minScore float64) []Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.load()
	type scored struct {
		p     Point
		score float64
	}
	var hits []scored
	for _, p := range all {
		if sc := cosine(queryVec, p.Vec); sc >= minScore {
			hits = append(hits, scored{p, sc})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > maxResults {
		hits = hits[:maxResults]
	}
	out := make([]Point, len(hits))
	for i, h := range hits {
		out[i] = h.p
	}
	return out
}

// cosine 余弦相似度（空向量/维度不匹配返回 0）。
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---------- Embedder（OpenAI 兼容 /embeddings，对应 embedders/openai-compatible.ts） ----------

// Embedder OpenAI 兼容 embeddings 客户端。
type Embedder struct {
	BaseURL string // 形如 https://api.openai.com 或 http://host:11434/v1（自动拼 /embeddings）
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// NewEmbedder 创建（HTTP 超时 5 分钟：批量嵌入大库时单请求较慢）。
func NewEmbedder(baseURL, apiKey, model string) *Embedder {
	return &Embedder{
		BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, Model: model,
		HTTP: &http.Client{Timeout: 5 * time.Minute},
	}
}

// Embed 批量嵌入（OpenAI input 数组语义；返回与 texts 等长且按 index 对齐的向量）。
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(map[string]interface{}{"model": e.Model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, "POST", e.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings api error %d: %s", resp.StatusCode, truncate(string(rb), 300))
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("embeddings 响应解析失败: %w", err)
	}
	// 按 index 回填（OpenAI 规范不保证 data 元素顺序）。
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index >= 0 && d.Index < len(vecs) {
			vecs[d.Index] = d.Embedding
		}
	}
	for i, v := range vecs {
		if len(v) == 0 {
			return nil, fmt.Errorf("embeddings 响应缺少第 %d 条向量", i)
		}
	}
	return vecs, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------- 索引器（processors/scanner.ts + orchestrator.ts 合并最小落地） ----------

const (
	indexBatchSize = 16         // 每次嵌入请求的文本条数（保守批大小，兼容各类网关）
	maxFileSize    = 512 * 1024 // 单文件读取上限
	maxFiles       = 20000      // 扫描文件数上限（防失控）
)

// textExts 参与向量化的扩展名（代码 + 文档 + 配置；二进制跳过）。
var textExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".vue": true,
	".py": true, ".java": true, ".kt": true, ".rs": true, ".c": true, ".h": true,
	".cpp": true, ".hpp": true, ".cs": true, ".rb": true, ".php": true, ".swift": true,
	".md": true, ".txt": true, ".rst": true, ".json": true, ".yaml": true, ".yml": true,
	".toml": true, ".ini": true, ".sql": true, ".sh": true, ".bat": true, ".ps1": true,
	".html": true, ".css": true, ".scss": true, ".proto": true, ".gradle": true, ".xml": true,
}

// skipDirs 跳过目录（依赖/构建产物/VCS/参考快照）。
var skipDirs = map[string]bool{
	".git": true, ".svn": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "target": true, "out": true, "bin": true,
	".idea": true, ".vscode": true, "__pycache__": true, ".gradle": true, "参考": true,
}

// Manifest 增量索引清单：rel path → (mtime, size, hash)。hash 非空才可跳过
// （mtime+size 命中但从未算过 hash 的文件仍要读一次内容，防原子替换漏更新）。
type Manifest map[string]manifestEntry

type manifestEntry struct {
	MTime int64  `json:"mtime"`
	Size  int64  `json:"size"`
	Hash  string `json:"hash,omitempty"`
}

// Indexer 索引器：扫描目录 → 分块 → 嵌入 → 落库（增量）。
type Indexer struct {
	Root     string // 索引根目录
	Store    *Store // 向量库
	Embedder *Embedder
	// Log 进度日志（可 nil）。
	Log func(format string, args ...interface{})
	// Progress 进度回调（done, total；可 nil）。
	Progress func(done, total int)

	manifestPath string
}

// NewIndexer 创建索引器（manifest 与 store 同目录持久化）。
func NewIndexer(root string, store *Store, embedder *Embedder) *Indexer {
	return &Indexer{
		Root: root, Store: store, Embedder: embedder,
		manifestPath: filepath.Join(filepath.Dir(store.path), "manifest.json"),
	}
}

func (ix *Indexer) logf(format string, args ...interface{}) {
	if ix.Log != nil {
		ix.Log(format, args...)
	}
}

// loadManifest 读取清单（损坏视为空，不致命）。
func (ix *Indexer) loadManifest() Manifest {
	m := Manifest{}
	if b, err := os.ReadFile(ix.manifestPath); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// saveManifest 落盘清单。
func (ix *Indexer) saveManifest(m Manifest) error {
	if err := os.MkdirAll(filepath.Dir(ix.manifestPath), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(ix.manifestPath, b, 0o644)
}

// contentHash 文件内容指纹。
func contentHash(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}

// Run 执行一次索引（增量）。返回 (扫描文件数, 新增/更新块数, 错误)。
func (ix *Indexer) Run(ctx context.Context) (files, blocks int, err error) {
	root, err := filepath.Abs(ix.Root)
	if err != nil {
		return 0, 0, err
	}
	oldManifest := ix.loadManifest()
	newManifest := Manifest{}

	// 1. 扫描文本文件并增量过滤（mtime+size 未变且 hash 在案 → 跳过）
	type item struct {
		path, rel string
		mtime     int64
	}
	var todo []item
	scanCount := 0
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil || ctx.Err() != nil {
			return nil // 单点失败跳过（不致命）
		}
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && d.Name() != ".kaelvoris")) {
				return filepath.SkipDir
			}
			return nil
		}
		if scanCount >= maxFiles {
			return filepath.SkipAll
		}
		if !textExts[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 || info.Size() > maxFileSize {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		scanCount++
		rel = filepath.ToSlash(rel)
		mtime := info.ModTime().Unix()
		newManifest[rel] = manifestEntry{MTime: mtime, Size: info.Size()}
		if old, ok := oldManifest[rel]; ok && old.MTime == mtime && old.Size == info.Size() && old.Hash != "" {
			newManifest[rel] = old // 未变更：跳过（保留 hash）
			return nil
		}
		todo = append(todo, item{path: p, rel: rel, mtime: mtime})
		return nil
	})
	if walkErr != nil {
		return scanCount, 0, walkErr
	}
	if ix.Progress != nil {
		ix.Progress(0, len(todo))
	}
	ix.logf("向量索引: 扫描 %d 个文件，%d 个待更新", scanCount, len(todo))

	// 2. 逐批：读文件 → 分块 → 批量嵌入 → 按文件 upsert
	changed := 0
	for start := 0; start < len(todo); start += indexBatchSize {
		if ctx.Err() != nil {
			return scanCount, changed, ctx.Err()
		}
		end := min(start+indexBatchSize, len(todo))
		batch := todo[start:end]

		// 2a. 读文件 + 分块（收集本批全部文本）
		type fileChunks struct {
			rel    string
			chunks []Chunk
		}
		var fc []fileChunks
		var texts []string
		for _, it := range batch {
			b, rerr := os.ReadFile(it.path)
			if rerr != nil {
				continue
			}
			newManifest[it.rel] = manifestEntry{MTime: it.mtime, Size: int64(len(b)), Hash: contentHash(b)}
			chs := chunkFile(string(b))
			if len(chs) == 0 {
				continue
			}
			fc = append(fc, fileChunks{rel: it.rel, chunks: chs})
			for _, c := range chs {
				texts = append(texts, c.Text)
			}
		}
		if len(texts) == 0 {
			continue
		}

		// 2b. 批量嵌入（每次请求 ≤indexBatchSize 条）
		vecs := make([][]float32, 0, len(texts))
		for tb := 0; tb < len(texts); tb += indexBatchSize {
			tend := min(tb+indexBatchSize, len(texts))
			vs, eerr := ix.Embedder.Embed(ctx, texts[tb:tend])
			if eerr != nil {
				return scanCount, changed, fmt.Errorf("嵌入失败（已完成 %d 块）: %w", changed, eerr)
			}
			vecs = append(vecs, vs...)
		}

		// 2c. 按文件分组 upsert（cursor 顺序与本批 texts 收集顺序一致）
		cursor := 0
		for _, f := range fc {
			pts := make([]Point, 0, len(f.chunks))
			for _, c := range f.chunks {
				pts = append(pts, Point{
					ID: fileID(f.rel) + ":" + fmt.Sprintf("%d", cursor), File: f.rel,
					Start: c.Start, End: c.End, Text: c.Text,
					Vec: vecs[cursor], Source: filepath.ToSlash(root),
				})
				cursor++
			}
			if err := ix.Store.Replace(f.rel, pts); err != nil {
				return scanCount, changed, err
			}
			changed += len(pts)
		}
		if ix.Progress != nil {
			ix.Progress(end, len(todo))
		}
	}

	// 3. 同步删除：旧清单有、新清单无的文件 → 清空其向量点
	// （对齐 Zoo-Code file-watcher 删除同步）
	for rel := range oldManifest {
		if _, ok := newManifest[rel]; !ok {
			_ = ix.Store.Replace(rel, nil)
		}
	}
	if err := ix.saveManifest(newManifest); err != nil {
		return scanCount, changed, err
	}
	return scanCount, changed, nil
}

// ---------- 检索 ----------

// SearchRequest 检索请求。
type SearchRequest struct {
	Query    string  // 查询文本（先嵌入再检索）
	MaxRes   int     // 条数（0 = 8）
	MinScore float64 // 阈值（0 = 0.35）
}

// Search 检索：查询嵌入 → Store.Search → 格式化为 LLM 友好文本。
// 返回（格式化文本, 命中点, 错误）。
func (ix *Indexer) Search(ctx context.Context, req SearchRequest) (string, []Point, error) {
	maxRes := req.MaxRes
	if maxRes <= 0 {
		maxRes = 8
	}
	minScore := req.MinScore
	if minScore <= 0 {
		minScore = 0.35
	}
	vecs, err := ix.Embedder.Embed(ctx, []string{req.Query})
	if err != nil {
		return "", nil, err
	}
	if len(vecs) == 0 {
		return "", nil, fmt.Errorf("查询嵌入为空")
	}
	pts := ix.Store.Search(vecs[0], maxRes, minScore)
	var sb strings.Builder
	for i, p := range pts {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		fmt.Fprintf(&sb, "%s:%d-%d\n%s", p.File, p.Start, p.End, p.Text)
	}
	return sb.String(), pts, nil
}
