// bootstrap.go：Coze Studio 嵌入式装配入口（对齐 application.Init 的 workflow 最小闭环）。
//
// 装配链（对照 coze-studio/backend/application/base/appinfra/app_infra.go 的 Init 顺序，
// 外部服务全部替换为 internal/cozeflow 的嵌入式实现）：
//
//	coze 原装配                    →  Kaelvoris 嵌入式装配
//	mysql.New（MySQL DSN）         →  OpenSQLite（modernc.org/sqlite + 方言转换 DDL）
//	redis.New（REDIS_ADDR）        →  NewMemCache（内存 Cmdable，TTL 惰性过期）
//	storage.New（MinIO/TOS/S3）    →  NewLocalStorage（本地文件树，file:// URL）
//	imagex.NewDefault（veimagex）  →  NewLocalImageX（Storage 顶替，对齐 minio_imagex.go 模式）
//	es.New / searchstore.New       →  Phase 2（bleve 全文检索 + chromem-go 向量），暂缺省
//	eventbus MQ 生产者             →  MemEventBus（进程内 topic 直投递）
//	checkpoint.NewRedisStore       →  checkpoint.NewInMemoryStore（coze 自带 mem 实现）
//	coderunner.New（sandbox）      →  direct.NewRunner（Python 直跑，Windows 命令名已适配）
//	modelbuilder.GetBuiltinChatModel → 保持原链路：config.Init 加载嵌入的 agnes YAML 模型
//
// workflow 域经 application/workflow.InitService 完整装配（原代码零改动）：
// RegisterAllNodeAdaptors → initWorkflowConfig → NewWorkflowRepository → SetRepository
// → NewWorkflowService → wrapPlugin.SetOSS → coderunner.SetCodeRunner → 全局回调 → setEventBus
//
// Phase 1 边界（注释即契约）：
//   - PluginDomainSVC 暂不装配（依赖 user 域串联）：plugin 节点执行时才解引用，最小闭环不可用；
//   - KnowledgeDomainSVC 已组装但 SearchStoreManagers 为空：检索节点优雅返回空结果；
//   - rdb（数据库节点）底层 SQL 为 MySQL 方言：建表/信息查询在 SQLite 下部分不可用（Phase 2 补丁）。
package cozeflow

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"gorm.io/gorm"

	"github.com/coze-dev/coze-studio/backend/application/memory"
	appWorkflow "github.com/coze-dev/coze-studio/backend/application/workflow"
	"github.com/coze-dev/coze-studio/backend/bizpkg/config"
	"github.com/coze-dev/coze-studio/backend/bizpkg/llm/modelbuilder"
	crossdatabase "github.com/coze-dev/coze-studio/backend/crossdomain/database"
	databaseImpl "github.com/coze-dev/coze-studio/backend/crossdomain/database/impl"
	knowledgeSvc "github.com/coze-dev/coze-studio/backend/domain/knowledge/service"
	dbservice "github.com/coze-dev/coze-studio/backend/domain/memory/database/service"
	rdbService "github.com/coze-dev/coze-studio/backend/infra/rdb/impl/rdb"
	"github.com/coze-dev/coze-studio/backend/infra/checkpoint"
	"github.com/coze-dev/coze-studio/backend/infra/coderunner/impl/direct"
	idgenImpl "github.com/coze-dev/coze-studio/backend/infra/idgen/impl/idgen"
	"github.com/coze-dev/coze-studio/backend/pkg/kvstore"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ptr"
	"github.com/coze-dev/coze-studio/backend/types/consts"
)

// Coze 装配完成后的运行态句柄：桌面端（Wails）/CLI/通道各宿主持有并调用。
type Coze struct {
	DataDir  string        // 数据根目录（~/.kaelvoris/coze/）：db/oss/资源释放地
	DB       *gorm.DB      // 嵌入式 SQLite（coze 全量 55 表）
	Cache    *MemCache     // 内存缓存（替代 Redis）
	Bus      *MemEventBus  // 内存事件总线（替代 NSQ）
	Storage  *LocalStorage // 本地对象存储（替代 MinIO）
	ImageX   *LocalImageX  // 本地 ImageX（替代 veimagex）
	IDGen    idgenImpl.IDGenerator
	ChatModel modelbuilder.BaseChatModel // 内置对话模型（agnes YAML 装配）
	ModelConfigured bool                  // ChatModel 是否实际可用（网络/密钥探测结果）
	Workflow *appWorkflow.ApplicationService
	// KnowledgeDomainSVC 知识库域服务（检索节点执行 + 知识库管理面板共用）。
	KnowledgeDomainSVC knowledgeSvc.Knowledge
	// DatabaseDomainSVC 数据库域服务（memory 域接口：建库/列表/记录管理，数据源面板共用）。
	DatabaseDomainSVC dbservice.Database
}

// Close 释放嵌入式内核持有的资源（当前仅 SQLite 连接池）。
// Windows 下必须显式关闭：连接未释放时 coze.db 文件被锁，目录无法清理。
func (c *Coze) Close() error {
	if c.DB == nil {
		return nil
	}
	sqlDB, err := c.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Bootstrap 完成 coze 内核全量装配。dataDir 传入 Kaelvoris 数据目录下的
// coze 子目录（建议 ~/.kaelvoris/coze/），可重复调用（资源释放与 DDL 均幂等）。
func Bootstrap(ctx context.Context, dataDir string) (*Coze, error) {
	c := &Coze{DataDir: dataDir}

	// ---- 1. 静态资源：嵌入树 → 磁盘释放 + COZE_RESOURCE_DIR 注入 ----
	if err := c.ensureResources(); err != nil {
		return nil, fmt.Errorf("释放 coze 资源失败: %w", err)
	}

	// ---- 2. SQLite：DDL 释放 + 方言转换执行（替代 mysql.New）----
	ddlPath := filepath.Join(dataDir, "coze_schema.sql")
	if err := releaseFile(ddlFS, "ddl/coze_schema.sql", ddlPath); err != nil {
		return nil, err
	}
	db, err := OpenSQLite(filepath.Join(dataDir, "coze.db"), ddlPath)
	if err != nil {
		return nil, err
	}
	c.DB = db
	kvstore.SetDefault(db) // pkg/kvstore 全局默认库（config base/knowledge 等消费）

	// ---- 3. 基础件：内存缓存 / ID 生成 / 对象存储 / ImageX ----
	c.Cache = NewMemCache()
	idGen, err := idgenImpl.New(c.Cache)
	if err != nil {
		return nil, fmt.Errorf("初始化 idgen 失败: %w", err)
	}
	c.IDGen = idGen

	st, err := NewLocalStorage(filepath.Join(dataDir, "oss"))
	if err != nil {
		return nil, err
	}
	c.Storage = st
	c.ImageX = NewLocalImageX(st)

	// ---- 4. 内存事件总线（替代 NSQ；知识域文档索引事件走此通道）----
	c.Bus = NewMemEventBus()

	// ---- 5. 配置装配（对齐 config.Init）：modelmgr 读嵌入资源里的
	//      model_meta.json + agnes-2.5-flash.yaml（COZE_RESOURCE_DIR 已注入）----
	if err := config.Init(ctx, db, st); err != nil {
		return nil, fmt.Errorf("coze config init: %w", err)
	}

	// ---- 6. 内置对话模型（三级回退最终命中 oldModels 的 agnes YAML；
	//      checkModelConfig 会真实调用一次 API 探测，失败仅降级不阻断启动）----
	chatModel, ok, err := modelbuilder.GetBuiltinChatModel(ctx, "WKR_")
	if err != nil {
		slog.Warn("cozeflow: 内置模型构建失败（LLM 节点将不可用）", "err", err)
	}
	c.ChatModel = chatModel
	c.ModelConfigured = ok && chatModel != nil
	if !c.ModelConfigured {
		slog.Warn("cozeflow: 内置模型未配置成功（检查网络与 agnes API Key），LLM 类节点不可用")
	}

	// ---- 7. 知识域组装（本地全文/向量检索：内存索引 + JSON 落盘，见 localsearch.go）----
	knowledgeDomainSVC, knowledgeHandler := knowledgeSvc.NewKnowledgeSVC(&knowledgeSvc.KnowledgeSVCConfig{
		DB:                  db,
		IDGen:               idGen,
		RDB:                 rdbService.NewService(db, idGen),
		Producer:            c.Bus.NewProducer(consts.RMQTopicKnowledge),
		SearchStoreManagers: NewLocalSearchManagers(dataDir), // 本地 bleve 等价实现（全文 + 向量）
		Storage:             st,
		Reranker:            nil, // 默认 RRF
		Rewriter:            nil, // 查询改写可选
		NL2Sql:              nil, // 表格 NL2SQL 可选
		EnableCompactTable:  ptr.Of(true),
		CacheCli:            c.Cache,
	})
	// 文档索引异步消费：文档切片后经 MQ 事件触发索引写入（无 store 时报错仅日志）
	if err := c.Bus.RegisterConsumer("", consts.RMQTopicKnowledge, consts.RMQConsumeGroupKnowledge, knowledgeHandler); err != nil {
		return nil, fmt.Errorf("注册知识域消费者失败: %w", err)
	}

	// ---- 8. memory 域组装（数据库/变量节点依赖；组装本身无 SQL 执行）----
	memSvc := memory.InitService(&memory.ServiceComponents{
		IDGen:                  idGen,
		DB:                     db,
		TosClient:              st,
		CacheCli:               c.Cache,
		ResourceDomainNotifier: NewNoopResourceEventBus(),
	})
	// SQL 节点运行时走 crossdomain 全局实例（crossdatabase.DefaultSVC().Execute/Query/…），
	// 对齐 application.go:154 的装配顺序；不设置则 SQL 节点执行报 nil。
	// 外层再包 externalDB 装饰器：负数 databaseInfoID 分流到外部 MySQL 连接
	// （设置 → 数据源），正数透传内部 SQLite 表——见 datasource.go。
	innerDB := databaseImpl.InitDomainService(memSvc.DatabaseDomainSVC)
	crossdatabase.SetDefaultSVC(&externalDB{Database: innerDB})

	// ---- 9. workflow 域完整装配（application/workflow.InitService，原代码零改动）----
	components := &appWorkflow.ServiceComponents{
		IDGen:              idGen,
		DB:                 db,
		Cache:              c.Cache,
		DatabaseDomainSVC:  memSvc.DatabaseDomainSVC,
		VariablesDomainSVC: memSvc.VariablesDomainSVC,
		PluginDomainSVC:    nil, // Phase 1：插件域依赖 user 域串联，暂不装配（plugin 节点不可用）
		KnowledgeDomainSVC: knowledgeDomainSVC,
		DomainNotifier:     NewNoopResourceEventBus(), // Phase 2：替换为 search 域真实投递
		Tos:                st,
		ImageX:             c.ImageX,
		CPStore:            checkpoint.NewInMemoryStore(), // 工作流中断/恢复断点
		CodeRunner:         direct.NewRunner(),            // Python 直跑（非沙箱）
		WorkflowBuildInChatModel: c.ChatModel,
	}
	appSVC, err := appWorkflow.InitService(ctx, components)
	if err != nil {
		return nil, fmt.Errorf("workflow InitService: %w", err)
	}
	c.Workflow = appSVC
	c.KnowledgeDomainSVC = knowledgeDomainSVC // 面板共用（CozeListKnowledgeBases / CozeCreateKnowledge）
	c.DatabaseDomainSVC = memSvc.DatabaseDomainSVC // 数据源面板共用（CozeListDatabases / CozeCreateDatabase）

	slog.Info("cozeflow: 嵌入式 coze 内核装配完成",
		"dataDir", dataDir,
		"modelConfigured", c.ModelConfigured)
	return c, nil
}

// ensureResources 释放嵌入的 conf 资源树到 <dataDir>/resources/，并注入
// COZE_RESOURCE_DIR（third_party 的 fileutil.GetWorkingDirectory 优先读取）。
// 幂等策略：资源已存在（探针文件 model_meta.json 存在）则跳过，避免覆盖用户改动。
func (c *Coze) ensureResources() error {
	probe := filepath.Join(c.DataDir, "resources", "conf", "model", "model_meta.json")
	if _, err := os.Stat(probe); err != nil {
		// 不存在 → 全量释放
		if err := fs.WalkDir(resourcesFS, "resources/conf", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			target := filepath.Join(c.DataDir, filepath.FromSlash(path))
			if d.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			return releaseFile(resourcesFS, path, target)
		}); err != nil {
			return fmt.Errorf("释放资源树失败: %w", err)
		}
		slog.Info("cozeflow: coze 资源树已释放", "dir", filepath.Join(c.DataDir, "resources"))
	}
	return os.Setenv("COZE_RESOURCE_DIR", c.DataDir)
}

// releaseFile 从嵌入 FS 释放单个文件到磁盘目标路径（含父目录创建）。
func releaseFile(efs fs.FS, name, target string) error {
	b, err := fs.ReadFile(efs, name)
	if err != nil {
		return fmt.Errorf("读取嵌入文件 %s: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, b, 0o644)
}
