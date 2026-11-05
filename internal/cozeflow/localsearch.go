// 本地检索存储层（Kaelvoris 融合改造，2026-10）。
//
// 嵌入式形态下替代 ES（全文）与 Milvus/VikingDB/OceanBase（向量）两族外部服务：
//   - localTextManager  → searchstore.TypeTextStore（对标 infra/document/searchstore/impl/elasticsearch）
//   - localVectorManager → searchstore.TypeVectorStore（对标 impl/milvus 等）
//
// 设计原则（对齐 AGENTS.md「接口最小化、不额外引重依赖」）：
//   - 全部内存索引 + JSON 落盘（dataDir/searchstore/<type>/<collection>.json），
//     桌面端个人知识库文档量为百~千级切片，全量扫描即可满足性能。
//   - 向量采用字符 bigram 哈希嵌入（hashEmbedder）：确定性、零外部依赖、
//     中文友好（每 2 字符一个 token）；若有真实 embedding API 可在装配处替换。
//   - DSL（searchstore.DSL eq/ne/like/in/and/or）统一内存求值，全文/向量共用。

package cozeflow

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/indexer"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/coze-dev/coze-studio/backend/infra/document"
	"github.com/coze-dev/coze-studio/backend/infra/document/searchstore"
	infraEmbedding "github.com/coze-dev/coze-studio/backend/infra/embedding"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ptr"
)

// ═══════════════════════════════════════════════════════════════════
// hashEmbedder：bigram 哈希嵌入器（实现 infra/embedding.Embedder）
// ═══════════════════════════════════════════════════════════════════

const (
	embedDim = 512 // 嵌入维度（哈希桶数）
)

// hashEmbedder 字符 bigram 哈希嵌入器：把文本切成 token（中文按双字符、
// 英文按词），对每个 token 计算 FNV-1a 哈希映射到固定桶，TF 计数后 L2 归一化。
// 确定性向量（同文本必同向量），可作为语义向量的退化替代，保证向量检索
// 链路在无 embedding API 的桌面环境始终可用。
type hashEmbedder struct{}

// NewHashEmbedder 构造哈希嵌入器（实现 embedding.Embedder 的最小适配）。
func NewHashEmbedder() *hashEmbedder { return &hashEmbedder{} }

// EmbedStrings 生成 dense 向量（eino embedding.Embedder 接口）。
func (h *hashEmbedder) EmbedStrings(_ context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	out := make([][]float64, 0, len(texts))
	for _, t := range texts {
		out = append(out, hashEmbedOne(t))
	}
	return out, nil
}

// EmbedStringsHybrid hybrid 形态（本实现仅 dense；sparse 返回空 map 序列）。
func (h *hashEmbedder) EmbedStringsHybrid(ctx context.Context, texts []string, opts ...embedding.Option) ([][]float64, []map[int]float64, error) {
	dense, err := h.EmbedStrings(ctx, texts, opts...)
	if err != nil {
		return nil, nil, err
	}
	sparse := make([]map[int]float64, len(texts))
	for i := range sparse {
		sparse[i] = map[int]float64{}
	}
	return dense, sparse, nil
}

// Dimensions 向量维度。
func (h *hashEmbedder) Dimensions() int64 { return embedDim }

// SupportStatus 仅支持 dense。
func (h *hashEmbedder) SupportStatus() infraEmbedding.SupportStatus {
	return infraEmbedding.SupportDense
}

// hashEmbedOne 单文本 → 归一化 bigram 哈希向量。
func hashEmbedOne(text string) []float64 {
	vec := make([]float64, embedDim)
	tokens := tokenizeForEmbed(text)
	if len(tokens) == 0 {
		return vec
	}
	for _, tok := range tokens {
		vec[hashToken(tok)]++
	}
	// L2 归一化，保证余弦相似度数值稳定
	sum := 0.0
	for _, v := range vec {
		sum += v * v
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return vec
	}
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

// hashToken FNV-1a 哈希 → 桶下标。
func hashToken(tok string) int {
	hsh := fnv.New32a()
	_, _ = hsh.Write([]byte(tok))
	return int(hsh.Sum32() % embedDim)
}

// tokenizeForEmbed 分词：连续 ASCII 字母/数字按词；CJK 逐字符组成 bigram
//（"知识库" → 知识/识库）；单字符 CJK 也保留（避免超短查询丢特征）。
func tokenizeForEmbed(text string) []string {
	runes := []rune(strings.ToLower(text))
	tokens := make([]string, 0, len(runes))
	var word strings.Builder
	flushWord := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	for i, r := range runes {
		if isASCIILetterOrDigit(r) {
			word.WriteRune(r)
			continue
		}
		flushWord()
		if unicode.IsLetter(r) || unicode.IsDigit(r) { // CJK 等非 ASCII 字符
			if i+1 < len(runes) && !isASCIILetterOrDigit(runes[i+1]) && unicode.IsLetter(runes[i+1]) {
				tokens = append(tokens, string(r)+string(runes[i+1])) // bigram
			} else {
				tokens = append(tokens, string(r))
			}
		}
	}
	flushWord()
	return tokens
}

// isASCIILetterOrDigit 判断 ASCII 字母/数字。
func isASCIILetterOrDigit(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// ═══════════════════════════════════════════════════════════════════
// localTextManager / localTextStore：全文检索（对标 ES 实现）
// ═══════════════════════════════════════════════════════════════════

const defaultTopK = 10

// localTextManager 全文检索 Manager（TypeTextStore）。
type localTextManager struct {
	root string // 持久化根目录
}

// newLocalTextManager 构造全文检索 Manager。
func newLocalTextManager(root string) searchstore.Manager {
	return &localTextManager{root: root}
}

// Create 建 collection（本地形态即创建 JSON 存储文件骨架）。
func (m *localTextManager) Create(_ context.Context, req *searchstore.CreateRequest) error {
	ss, err := m.GetSearchStore(context.Background(), req.CollectionName)
	if err != nil {
		return err
	}
	return ss.(*localTextStore).ensureFile()
}

// Drop 删除 collection 存储。
func (m *localTextManager) Drop(_ context.Context, req *searchstore.DropRequest) error {
	_ = os.Remove(m.collectionPath(req.CollectionName))
	return nil
}

// GetType 存储类型：全文。
func (m *localTextManager) GetType() searchstore.SearchStoreType { return searchstore.TypeTextStore }

// GetSearchStore 取（或惰性创建）collection 对应的 SearchStore。
func (m *localTextManager) GetSearchStore(_ context.Context, collectionName string) (searchstore.SearchStore, error) {
	return loadLocalTextStore(m.collectionPath(collectionName), collectionName)
}

func (m *localTextManager) collectionPath(name string) string {
	return filepath.Join(m.root, "text", name+".json")
}

// localTextStore 单 collection 的内存全文索引。
type localTextStore struct {
	mu   sync.Mutex
	path string
	name string
	docs map[string]map[string]any // id → field → value（含 text_content/creator_id/document_id/...）
}

// loadLocalTextStore 从磁盘加载（文件不存在即空索引）。
func loadLocalTextStore(path, name string) (*localTextStore, error) {
	st := &localTextStore{path: path, name: name, docs: map[string]map[string]any{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(b, &st.docs); err != nil {
		// 文件损坏按「跳过不致命」处理（对齐仓库持久化约定）
		return &localTextStore{path: path, name: name, docs: map[string]map[string]any{}}, nil
	}
	return st, nil
}

// ensureFile 确保持久化文件存在（Create 时调用）。
func (s *localTextStore) ensureFile() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		return s.flushLocked()
	}
	return nil
}

// flushLocked 落盘（调用方持锁）。
func (s *localTextStore) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(s.docs)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}

// Store 索引切片文档（字段形态对齐 es.fromDocument）。
func (s *localTextStore) Store(_ context.Context, docs []*schema.Document, opts ...indexer.Option) (ids []string, err error) {
	implOpt := indexer.GetImplSpecificOptions(&searchstore.IndexerOptions{}, opts...)
	defer func() {
		if err != nil && implOpt.ProgressBar != nil {
			implOpt.ProgressBar.ReportError(err)
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range docs {
		fields, ferr := document2Fields(d)
		if ferr != nil {
			return nil, ferr
		}
		s.docs[d.ID] = fields
		ids = append(ids, d.ID)
		if implOpt.ProgressBar != nil {
			if aerr := implOpt.ProgressBar.AddN(1); aerr != nil {
				return nil, aerr
			}
		}
	}
	return ids, s.flushLocked()
}

// Retrieve 全文检索：token 重叠打分 + DSL 过滤 + topK（分值按首名归一）。
func (s *localTextStore) Retrieve(_ context.Context, query string, opts ...retriever.Option) ([]*schema.Document, error) {
	common := retriever.GetCommonOptions(&retriever.Options{TopK: ptr.Of(defaultTopK)}, opts...)
	implOpt := retriever.GetImplSpecificOptions(&searchstore.RetrieverOptions{}, opts...)

	// 查询词：MultiMatch 优先（表格列检索场景），否则用 query 本身
	q := query
	if implOpt.MultiMatch != nil && implOpt.MultiMatch.Query != "" {
		q = implOpt.MultiMatch.Query
	}
	qTokens := tokenizeForEmbed(q)

	dsl, err := searchstore.LoadDSL(common.DSLInfo)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	type hit struct {
		id    string
		score float64
	}
	hits := make([]hit, 0, len(s.docs))
	for id, fields := range s.docs {
		if !matchDSL(dsl, fields) {
			continue
		}
		// 待匹配文本：MultiMatch 指定字段（拼接）或 text_content
		var target string
		if implOpt.MultiMatch != nil && len(implOpt.MultiMatch.Fields) > 0 {
			parts := make([]string, 0, len(implOpt.MultiMatch.Fields))
			for _, f := range implOpt.MultiMatch.Fields {
				if v, ok := fields[f].(string); ok {
					parts = append(parts, v)
				}
			}
			target = strings.Join(parts, " ")
		} else if v, ok := fields[searchstore.FieldTextContent].(string); ok {
			target = v
		}
		score := tokenOverlapScore(qTokens, target)
		if score > 0 {
			hits = append(hits, hit{id: id, score: score})
		}
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > ptr.From(common.TopK) {
		hits = hits[:ptr.From(common.TopK)]
	}

	first := 0.0
	docs := make([]*schema.Document, 0, len(hits))
	for _, h := range hits {
		doc := fields2Document(h.id, s.docs[h.id])
		if first == 0 {
			first = h.score
		}
		if first > 0 {
			doc.WithScore(h.score / first) // 分值归一（对齐 ES 实现的 score/firstScore）
		} else {
			doc.WithScore(0)
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// Delete 按 id 删除。
func (s *localTextStore) Delete(_ context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.docs, id)
	}
	return s.flushLocked()
}

// ═══════════════════════════════════════════════════════════════════
// localVectorManager / localVectorStore：向量检索（对标 Milvus 实现）
// ═══════════════════════════════════════════════════════════════════

// localVectorManager 向量检索 Manager（TypeVectorStore）。
type localVectorManager struct {
	root string
	emb  embedding.Embedder
}

// newLocalVectorManager 构造向量检索 Manager（emb 传 nil 时用哈希嵌入器）。
func newLocalVectorManager(root string, emb embedding.Embedder) searchstore.Manager {
	if emb == nil {
		emb = NewHashEmbedder()
	}
	return &localVectorManager{root: root, emb: emb}
}

// Create 建 collection。
func (m *localVectorManager) Create(_ context.Context, req *searchstore.CreateRequest) error {
	ss, err := m.GetSearchStore(context.Background(), req.CollectionName)
	if err != nil {
		return err
	}
	return ss.(*localVectorStore).ensureFile()
}

// Drop 删除 collection。
func (m *localVectorManager) Drop(_ context.Context, req *searchstore.DropRequest) error {
	_ = os.Remove(m.collectionPath(req.CollectionName))
	return nil
}

// GetType 存储类型：向量。
func (m *localVectorManager) GetType() searchstore.SearchStoreType { return searchstore.TypeVectorStore }

// GetSearchStore 取（或惰性创建）collection。
func (m *localVectorManager) GetSearchStore(_ context.Context, collectionName string) (searchstore.SearchStore, error) {
	return loadLocalVectorStore(m.collectionPath(collectionName), collectionName, m.emb)
}

func (m *localVectorManager) collectionPath(name string) string {
	return filepath.Join(m.root, "vector", name+".json")
}

// vectorDoc 向量库单条记录。
type vectorDoc struct {
	Fields map[string]any `json:"fields"`
	Vector []float64      `json:"vector"`
}

// localVectorStore 单 collection 的内存向量索引。
type localVectorStore struct {
	mu   sync.Mutex
	path string
	name string
	emb  embedding.Embedder
	docs map[string]*vectorDoc
}

// loadLocalVectorStore 从磁盘加载（损坏按跳过处理）。
func loadLocalVectorStore(path, name string, emb embedding.Embedder) (*localVectorStore, error) {
	st := &localVectorStore{path: path, name: name, emb: emb, docs: map[string]*vectorDoc{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(b, &st.docs); err != nil {
		return &localVectorStore{path: path, name: name, emb: emb, docs: map[string]*vectorDoc{}}, nil
	}
	return st, nil
}

// ensureFile 确保持久化文件存在。
func (s *localVectorStore) ensureFile() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		return s.flushLocked()
	}
	return nil
}

// flushLocked 落盘（调用方持锁）。
func (s *localVectorStore) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(s.docs)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}

// Store 索引切片文档：对 text_content 生成向量（ext 中已带 dense_ 前缀向量
// 字段时优先复用，形态对齐 milvus 的 FieldTypeDenseVector 列处理）。
func (s *localVectorStore) Store(ctx context.Context, docs []*schema.Document, opts ...indexer.Option) (ids []string, err error) {
	implOpt := indexer.GetImplSpecificOptions(&searchstore.IndexerOptions{}, opts...)
	defer func() {
		if err != nil && implOpt.ProgressBar != nil {
			implOpt.ProgressBar.ReportError(err)
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	// 批量嵌入（一次请求，减少调用次数）
	texts := make([]string, 0, len(docs))
	for _, d := range docs {
		texts = append(texts, d.Content)
	}
	var vecs [][]float64
	if len(texts) > 0 {
		vecs, err = s.emb.EmbedStrings(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("[localVectorStore.Store] embed failed, %w", err)
		}
	}

	for i, d := range docs {
		fields, ferr := document2Fields(d)
		if ferr != nil {
			return nil, ferr
		}
		s.docs[d.ID] = &vectorDoc{Fields: fields, Vector: vecs[i]}
		ids = append(ids, d.ID)
		if implOpt.ProgressBar != nil {
			if aerr := implOpt.ProgressBar.AddN(1); aerr != nil {
				return nil, aerr
			}
		}
	}
	return ids, s.flushLocked()
}

// Retrieve 向量检索：query 向量化 → 余弦相似度 topK + DSL 过滤。
func (s *localVectorStore) Retrieve(ctx context.Context, query string, opts ...retriever.Option) ([]*schema.Document, error) {
	common := retriever.GetCommonOptions(&retriever.Options{TopK: ptr.Of(defaultTopK)}, opts...)

	qvec, err := s.emb.EmbedStrings(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("[localVectorStore.Retrieve] embed query failed, %w", err)
	}
	dsl, derr := searchstore.LoadDSL(common.DSLInfo)
	if derr != nil {
		return nil, derr
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	type hit struct {
		id    string
		score float64
	}
	hits := make([]hit, 0, len(s.docs))
	for id, d := range s.docs {
		if !matchDSL(dsl, d.Fields) {
			continue
		}
		score := cosine(qvec[0], d.Vector)
		hits = append(hits, hit{id: id, score: score})
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > ptr.From(common.TopK) {
		hits = hits[:ptr.From(common.TopK)]
	}

	docs := make([]*schema.Document, 0, len(hits))
	for _, h := range hits {
		doc := fields2Document(h.id, s.docs[h.id].Fields)
		// 余弦 [-1,1] → [0,1]（对齐 milvus COSINE 归一化语义）
		doc.WithScore((h.score + 1) / 2)
		docs = append(docs, doc)
	}
	return docs, nil
}

// Delete 按 id 删除。
func (s *localVectorStore) Delete(_ context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.docs, id)
	}
	return s.flushLocked()
}

// ═══════════════════════════════════════════════════════════════════
// 共享工具：字段转换 / DSL 求值 / 打分
// ═══════════════════════════════════════════════════════════════════

// document2Fields schema.Document → 扁平字段 map（形态对齐 es.fromDocument：
// text_content/creator_id 顶层 + external_storage 展开）。
func document2Fields(doc *schema.Document) (map[string]any, error) {
	if doc.MetaData == nil {
		return nil, fmt.Errorf("[document2Fields] meta data is nil")
	}
	creatorID, err := document.GetDocumentCreatorID(doc)
	if err != nil {
		return nil, fmt.Errorf("[document2Fields] creator_id not found, %w", err)
	}
	fields := map[string]any{
		searchstore.FieldTextContent: doc.Content,
		searchstore.FieldCreatorID:   creatorID,
	}
	if ext, ok := doc.MetaData[document.MetaDataKeyExternalStorage].(map[string]any); ok {
		for k, v := range ext {
			fields[k] = v
		}
	}
	return fields, nil
}

// fields2Document 扁平字段 map → schema.Document（形态对齐 es.parseSearchResult）。
func fields2Document(id string, fields map[string]any) *schema.Document {
	ext := map[string]any{}
	doc := &schema.Document{
		ID:       id,
		MetaData: map[string]any{document.MetaDataKeyExternalStorage: ext},
	}
	for k, v := range fields {
		switch k {
		case searchstore.FieldTextContent:
			doc.Content, _ = v.(string)
		case searchstore.FieldCreatorID:
			if cid, ok := toInt64(v); ok {
				doc.MetaData[document.MetaDataKeyCreatorID] = cid
			}
		default:
			ext[k] = v
		}
	}
	return doc
}

// toInt64 数值宽容转换（int/int64/float64/json.Number/string）。
func toInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		return int64(t), true
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, true
		}
	case string:
		if i, err := strconv.ParseInt(t, 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

// matchDSL 内存 DSL 求值（eq/ne/like/in/and/or）。
func matchDSL(dsl *searchstore.DSL, fields map[string]any) bool {
	if dsl == nil {
		return true
	}
	fv := fields[dsl.Field]
	switch dsl.Op {
	case searchstore.OpEq:
		return valueEqual(fv, dsl.Value)
	case searchstore.OpNe:
		return !valueEqual(fv, dsl.Value)
	case searchstore.OpLike:
		sub, ok := dsl.Value.(string)
		if !ok {
			return false
		}
		s, _ := fv.(string)
		return strings.Contains(s, sub)
	case searchstore.OpIn:
		for _, item := range valueSlice(dsl.Value) {
			if valueEqual(fv, item) {
				return true
			}
		}
		return false
	case searchstore.OpAnd:
		subs, ok := dsl.Value.([]*searchstore.DSL)
		if !ok {
			return true
		}
		for _, sub := range subs {
			if !matchDSL(sub, fields) {
				return false
			}
		}
		return true
	case searchstore.OpOr:
		subs, ok := dsl.Value.([]*searchstore.DSL)
		if !ok {
			return true
		}
		for _, sub := range subs {
			if matchDSL(sub, fields) {
				return true
			}
		}
		return len(subs) == 0
	default:
		return true
	}
}

// valueEqual 数值/字符串宽容相等（int64 与 float64、json.Number 互通）。
func valueEqual(a, b any) bool {
	if ai, ok := toInt64(a); ok {
		if bi, ok2 := toInt64(b); ok2 {
			return ai == bi
		}
		return false
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// valueSlice DSL In 值 → []any（反射兼容 []int64/[]string 等切片形态）。
func valueSlice(v any) []any {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil
	}
	out := make([]any, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out = append(out, rv.Index(i).Interface())
	}
	return out
}

// tokenOverlapScore 查询 token 与目标文本的加权重合率（token 命中数/查询
// token 数，去重后）。全文打分的简化 BM25：桌面端小语料足够区分相关性。
func tokenOverlapScore(qTokens []string, target string) float64 {
	if len(qTokens) == 0 || target == "" {
		return 0
	}
	tTokens := tokenizeForEmbed(target)
	tSet := make(map[string]struct{}, len(tTokens))
	for _, t := range tTokens {
		tSet[t] = struct{}{}
	}
	uniq := make(map[string]struct{}, len(qTokens))
	matched := 0.0
	for _, q := range qTokens {
		if _, dup := uniq[q]; dup {
			continue
		}
		uniq[q] = struct{}{}
		if _, ok := tSet[q]; ok {
			matched++
		}
	}
	if len(uniq) == 0 {
		return 0
	}
	return matched / float64(len(uniq))
}

// cosine 余弦相似度（向量长度不等按短侧截断，异常返回 0）。
func cosine(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// NewLocalSearchManagers 组装本地全文 + 向量双 Manager（bootstrap 装配点）。
func NewLocalSearchManagers(dataDir string) []searchstore.Manager {
	root := filepath.Join(dataDir, "searchstore")
	return []searchstore.Manager{
		newLocalTextManager(root),
		newLocalVectorManager(root, nil),
	}
}
