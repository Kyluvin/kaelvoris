package cozeflow

import (
	"context"
	"strconv"
	"testing"

	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/coze-dev/coze-studio/backend/infra/document"
	"github.com/coze-dev/coze-studio/backend/infra/document/searchstore"
)

// mkDoc 构造切片文档（形态对齐 knowledge service s2dMapping 的 DocumentTypeText 分支）。
func mkDoc(id int64, content string, knowledgeID int64) *schema.Document {
	return &schema.Document{
		ID:      strconv.FormatInt(id, 10),
		Content: content,
		MetaData: map[string]any{
			document.MetaDataKeyCreatorID: int64(7700),
			document.MetaDataKeyExternalStorage: map[string]any{
				"document_id": knowledgeID,
			},
		},
	}
}

// TestLocalTextStoreEndToEnd 全文 store：索引 → DSL 过滤检索 → 删除 → 重载持久化。
func TestLocalTextStoreEndToEnd(t *testing.T) {
	ctx := context.Background()
	mgr := newLocalTextManager(t.TempDir())

	const coll = "coll_text_1"
	if err := mgr.Create(ctx, &searchstore.CreateRequest{CollectionName: coll}); err != nil {
		t.Fatalf("create: %v", err)
	}
	ss, err := mgr.GetSearchStore(ctx, coll)
	if err != nil {
		t.Fatalf("get store: %v", err)
	}
	st := ss.(*localTextStore)

	// ---- 索引 3 个切片（2 个属于 document_id=101，1 个属于 102）----
	ids, err := st.Store(ctx, []*schema.Document{
		mkDoc(1, "Kaelvoris 是一个 Go 内核的 AI Agent 应用", 101),
		mkDoc(2, "嵌入式 coze 工作流内核支持创建保存发布执行", 101),
		mkDoc(3, "数据库节点支持 MySQL 方言建表", 102),
	})
	if err != nil || len(ids) != 3 {
		t.Fatalf("store: ids=%v err=%v", ids, err)
	}

	// ---- 无过滤检索："agent" 应命中文档 1 ----
	docs, err := st.Retrieve(ctx, "Go AI Agent 应用", retriever.WithTopK(10))
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(docs) == 0 || docs[0].ID != "1" {
		t.Fatalf("retrieve expect top1 id=1, got %+v", docs)
	}
	if docs[0].Score() <= 0 || docs[0].Score() > 1.001 {
		t.Fatalf("score not normalized: %v", docs[0].Score())
	}

	// ---- DSL 过滤（document_id in [102]）：检索 "方言" 只能命中文档 3 ----
	dsl := (&searchstore.DSL{Op: searchstore.OpIn, Field: "document_id", Value: []any{int64(102)}}).DSL()
	docs, err = st.Retrieve(ctx, "方言", retriever.WithTopK(10), retriever.WithDSLInfo(dsl))
	if err != nil {
		t.Fatalf("retrieve dsl: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "3" {
		t.Fatalf("dsl retrieve expect only id=3, got %+v", docs)
	}
	// 回读 document_id 扩展字段
	ext, ok := docs[0].MetaData[document.MetaDataKeyExternalStorage].(map[string]any)
	if !ok || ext["document_id"] != int64(102) {
		t.Fatalf("ext document_id lost: %+v", docs[0].MetaData)
	}

	// ---- 删除后不可再检索到 ----
	if err = st.Delete(ctx, []string{"1"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	docs, _ = st.Retrieve(ctx, "Go AI Agent 应用", retriever.WithTopK(10))
	for _, d := range docs {
		if d.ID == "1" {
			t.Fatalf("deleted doc still retrieved")
		}
	}

	// ---- 持久化重载：新实例应看到文档 2/3 ----
	ss2, err := mgr.GetSearchStore(ctx, coll)
	if err != nil {
		t.Fatalf("reload get store: %v", err)
	}
	docs, err = ss2.Retrieve(ctx, "工作流 内核", retriever.WithTopK(10))
	if err != nil {
		t.Fatalf("reload retrieve: %v", err)
	}
	if len(docs) == 0 || docs[0].ID != "2" {
		t.Fatalf("reload expect top1 id=2, got %+v", docs)
	}
}

// TestLocalVectorStoreEndToEnd 向量 store：索引 → 余弦检索 → Drop。
func TestLocalVectorStoreEndToEnd(t *testing.T) {
	ctx := context.Background()
	mgr := newLocalVectorManager(t.TempDir(), nil)

	const coll = "coll_vec_1"
	if err := mgr.Create(ctx, &searchstore.CreateRequest{CollectionName: coll}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if mgr.GetType() != searchstore.TypeVectorStore {
		t.Fatalf("unexpected type: %s", mgr.GetType())
	}
	ss, err := mgr.GetSearchStore(ctx, coll)
	if err != nil {
		t.Fatalf("get store: %v", err)
	}
	vs := ss.(*localVectorStore)

	if _, err = vs.Store(ctx, []*schema.Document{
		mkDoc(11, "Kaelvoris AI Agent 桌面端", 201),
		mkDoc(12, "知识库检索支持全文与向量双路", 201),
	}); err != nil {
		t.Fatalf("store: %v", err)
	}

	docs, err := vs.Retrieve(ctx, "知识库 向量 检索", retriever.WithTopK(1))
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "12" {
		t.Fatalf("vector retrieve expect top1 id=12, got %+v", docs)
	}
	if docs[0].Score() < 0 || docs[0].Score() > 1 {
		t.Fatalf("cosine-normalized score out of [0,1]: %v", docs[0].Score())
	}

	if err = mgr.Drop(ctx, &searchstore.DropRequest{CollectionName: coll}); err != nil {
		t.Fatalf("drop: %v", err)
	}
	ss2, _ := mgr.GetSearchStore(ctx, coll)
	docs, _ = ss2.Retrieve(ctx, "知识库", retriever.WithTopK(10))
	if len(docs) != 0 {
		t.Fatalf("dropped collection still returns docs")
	}
}
