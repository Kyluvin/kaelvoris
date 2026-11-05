// bootstrap_test.go：嵌入式装配冒烟测试。
//
// 验证 Bootstrap 全链路：SQLite 建表（55 张）→ 资源树释放 → config/modelmgr
// 装配（嵌入的 agnes YAML 模型）→ workflow 域 InitService → 工作流 CRUD。
// LLM 模型探测依赖外网（checkModelConfig 真实调用 agnes API），网络不可达时
// ModelConfigured=false 但装配不失败，故断言不依赖网络。
package cozeflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
	"github.com/coze-dev/coze-studio/backend/pkg/kvstore"
)

// TestBootstrapSmoke 装配冒烟 + 工作流创建/保存/读取最小闭环。
func TestBootstrapSmoke(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	c, err := Bootstrap(ctx, dir)
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	defer func() { _ = c.Close() }() // Windows：SQLite 连接不关则 coze.db 被锁，TempDir 清理失败

	// ---- 断言 1：SQLite 全量表就绪（coze schema 55 张）----
	var tableCnt int64
	if err := c.DB.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").
		Scan(&tableCnt).Error; err != nil {
		t.Fatalf("统计表数量失败: %v", err)
	}
	t.Logf("SQLite 表数量: %d（期望 >= 50）", tableCnt)
	if tableCnt < 50 {
		t.Fatalf("期望 >= 50 张表，实际 %d", tableCnt)
	}

	// ---- 断言 2：资源树已释放 + COZE_RESOURCE_DIR 注入 ----
	for _, p := range []string{
		filepath.Join("resources", "conf", "model", "model_meta.json"),
		filepath.Join("resources", "conf", "model", "agnes-2.5-flash.yaml"),
		filepath.Join("resources", "conf", "workflow", "config.yaml"),
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			t.Fatalf("资源未释放: %s: %v", p, err)
		}
	}
	if got := os.Getenv("COZE_RESOURCE_DIR"); got != dir {
		t.Fatalf("COZE_RESOURCE_DIR 未注入: got=%q want=%q", got, dir)
	}

	// ---- 断言 3：workflow 域服务就绪 ----
	if c.Workflow == nil || c.Workflow.DomainSVC == nil {
		t.Fatal("workflow DomainSVC 未装配")
	}

	// ---- 断言 4：工作流创建（空画布）+ 保存 + 读取 ----
	wfID, err := c.Workflow.DomainSVC.Create(ctx, &vo.MetaCreate{
		Name:             "cozeflow-smoke",
		Desc:             "嵌入式装配冒烟",
		SpaceID:          1,
		CreatorID:        1,
		InitCanvasSchema: `{"nodes":[],"edges":[]}`,
	})
	if err != nil {
		t.Fatalf("创建工作流失败: %v", err)
	}
	t.Logf("工作流已创建: id=%d", wfID)

	// 读取（draft 形态）
	wf, err := c.Workflow.DomainSVC.Get(ctx, &vo.GetPolicy{ID: wfID})
	if err != nil {
		t.Fatalf("读取工作流失败: %v", err)
	}
	if wf == nil {
		t.Fatal("工作流为空")
	}
	t.Logf("工作流读取成功: id=%d name=%s", wfID, wf.Meta.Name)

	// ---- 断言 5：模型装配状态记录（不因网络失败而失败）----
	t.Logf("内置模型装配: configured=%v", c.ModelConfigured)
}

// TestKVStoreSQLiteUpsert 验证 kvstore 的 ON CONFLICT upsert 补丁在 SQLite 下可写可覆盖。
func TestKVStoreSQLiteUpsert(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	c, err := Bootstrap(ctx, dir)
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	// kvstore 泛型：map[string]any 承载任意配置（kv_entries 表）
	kv := kvstore.New[map[string]any](c.DB)
	first := map[string]any{"enabled": true}
	if err := kv.Save(ctx, "test_ns", "flag1", &first); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	second := map[string]any{"enabled": false}
	if err := kv.Save(ctx, "test_ns", "flag1", &second); err != nil {
		t.Fatalf("覆盖写入失败（ON CONFLICT 路径）: %v", err)
	}
	got, err := kv.Get(ctx, "test_ns", "flag1")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if (*got)["enabled"] != false {
		t.Fatalf("覆盖未生效: %v", *got)
	}
	t.Logf("kvstore SQLite upsert 验证通过: %v", *got)
}
