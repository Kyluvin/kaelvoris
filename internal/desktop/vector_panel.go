// vector_panel.go —— 向量知识库面板（Wails 绑定 + 定时调度 + Agent 工具集成）。
//
// 对应 Zoo-Code code-index 的 manager/orchestrator 角色最小落地：
//   - 配置面：向量化总开关 / 待索引目录 / 自动化时间（cron）/ embeddings 端点；
//   - 调度面：分钟级 tick 复用 cron.Parse（5 段 + @ 描述符 + @every），
//     到期自动解析目录内文件增量向量化（防重入 + @every 间隔推进）；
//   - 状态面：点数/文件数/最近索引/运行中/最近错误，经 kaelvoris:vector_status 推送；
//   - 工具面：codebase_search（对齐 CodebaseSearchTool.ts）注册进任务级
//     Registry（buildRunEngine），LLM 以自然语言检索向量库。
package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/cron"
	"kaelvoris/internal/vector"
)

// vectorPanel 向量知识库运行态（索引互斥 + 状态快照；存储复用全局 vector.Store）。
type vectorPanel struct {
	mu         sync.Mutex
	running    bool      // 索引进行中（防重入：调度/手动共享同一互斥标志）
	lastErr    string    // 最近一次索引错误（空 = 无错误）
	lastDone   time.Time // 最近一次成功索引时间
	lastFiles  int       // 最近一次成功索引扫描的文件数
	lastBlocks int       // 最近一次成功索引新增/更新的块数
	lastFire   time.Time // 最近一次调度触发时间（@every 间隔推进依据）
}

// vectorDir 向量库持久化目录（~/.kaelvoris/vector：store.json + manifest.json）。
func vectorDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "vector")
}

// vectorConfig 读取当前向量化配置（nil 安全）。
func (a *App) vectorConfig() *vector.Config {
	cfg := a.GetConfig()
	return cfg.Vector
}

// newVectorPanel 初始化向量库存储（失败退临时目录，不阻断启动——对齐
// cron/employee 面板的降级语义）。
func newVectorPanel() *vectorPanel {
	return &vectorPanel{}
}

// vectorStore 返回全局向量库（懒初始化；目录不可写时由 store.save 报错，不致命）。
func (a *App) vectorStore() *vector.Store {
	return vector.NewStore(vectorDir())
}

// vectorEmbedder 按 vector.Config 构建 embeddings 客户端（EmbedURL/EmbedKey
// 空时回退主 BaseURL/APIKey——单网关场景免重复配置）。
func (a *App) vectorEmbedder(vc *vector.Config) (*vector.Embedder, error) {
	if vc == nil || !vc.Enabled {
		return nil, errors.New("向量化未开启")
	}
	baseURL := strings.TrimSpace(vc.EmbedURL)
	if baseURL == "" {
		baseURL = a.GetConfig().BaseURL
	}
	if baseURL == "" {
		return nil, errors.New("embeddings 端点未配置（EmbedURL 与主 Base URL 均为空）")
	}
	apiKey := vc.EmbedKey
	if apiKey == "" {
		apiKey = a.GetConfig().APIKey
	}
	model := strings.TrimSpace(vc.EmbedModel)
	if model == "" {
		return nil, errors.New("embedding 模型未配置（如 text-embedding-3-small / bge-m3）")
	}
	return vector.NewEmbedder(baseURL, apiKey, model), nil
}

// vectorSearch 向量检索（codebase_search 工具后端；检索参数取自配置默认值）。
func (a *App) vectorSearch(ctx context.Context, query string, maxRes int) (string, error) {
	vc := a.vectorConfig()
	emb, err := a.vectorEmbedder(vc)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(vc.Dir) == "" {
		return "", errors.New("未配置待向量化目录")
	}
	ix := vector.NewIndexer(vc.Dir, a.vectorStore(), emb)
	out, pts, err := ix.Search(ctx, vector.SearchRequest{
		Query:    query,
		MaxRes:   maxRes,
		MinScore: vc.MinScoreOr(),
	})
	if err != nil {
		return "", err
	}
	if len(pts) == 0 {
		return "（向量库中无命中内容）", nil
	}
	return out, nil
}

// vectorRunIndex 触发一次索引（异步；防重入）。完成/失败经 pushVectorStatus
// 推送前端，错误记录到状态面板。
func (a *App) vectorRunIndex() {
	a.vec.mu.Lock()
	if a.vec.running {
		a.vec.mu.Unlock()
		return
	}
	a.vec.running = true
	a.vec.lastErr = ""
	a.vec.mu.Unlock()
	a.pushVectorStatus()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("向量索引 panic（已拦截）", "panic", r)
				a.vec.mu.Lock()
				a.vec.lastErr = fmt.Sprintf("内部错误: %v", r)
				a.vec.running = false
				a.vec.mu.Unlock()
				a.pushVectorStatus()
			}
		}()
		a.vecIndexOnce()
		a.vec.mu.Lock()
		a.vec.running = false
		a.vec.mu.Unlock()
		a.pushVectorStatus()
	}()
}

// vecIndexOnce 同步执行一轮增量索引（goroutine 内调用；错误落状态不返回）。
func (a *App) vecIndexOnce() {
	vc := a.vectorConfig()
	if vc == nil || !vc.Enabled {
		a.vec.mu.Lock()
		a.vec.lastErr = "向量化未开启"
		a.vec.mu.Unlock()
		return
	}
	dir := strings.TrimSpace(vc.Dir)
	if dir == "" {
		a.vec.mu.Lock()
		a.vec.lastErr = "未配置待向量化目录"
		a.vec.mu.Unlock()
		return
	}
	emb, err := a.vectorEmbedder(vc)
	if err != nil {
		a.vec.mu.Lock()
		a.vec.lastErr = err.Error()
		a.vec.mu.Unlock()
		return
	}
	ix := vector.NewIndexer(dir, a.vectorStore(), emb)
	ix.Log = func(f string, args ...interface{}) { slog.Info(fmt.Sprintf(f, args...)) }
	files, blocks, err := ix.Run(context.Background())
	a.vec.mu.Lock()
	if err != nil {
		a.vec.lastErr = err.Error()
	} else {
		a.vec.lastDone = time.Now()
		a.vec.lastFiles = files
		a.vec.lastBlocks = blocks
	}
	a.vec.mu.Unlock()
	slog.Info("向量索引完成", "files", files, "blocks", blocks, "err", err != nil)
}

// vectorStartScheduler 定时向量化调度循环（分钟级 tick；常驻 goroutine，
// 每 tick 重读最新配置——保存配置即生效，无需重启调度器）。
//   - 标准表达式：cron.Schedule.Match(now) 命中即触发；
//   - @every 间隔：按 lastFire + 间隔推进（对齐 robfig/cron 定时器语义）；
//   - 防重入：索引进行中跳过本次（vectorRunIndex 内互斥）。
func (a *App) vectorStartScheduler() {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for now := range t.C {
			vc := a.vectorConfig()
			if vc == nil || !vc.Enabled {
				continue
			}
			expr := strings.TrimSpace(vc.Schedule)
			if expr == "" || strings.TrimSpace(vc.Dir) == "" {
				continue
			}
			sched, err := cron.Parse(expr)
			if err != nil {
				continue // 表达式非法：跳过（保存时已校验，此处防御旧配置）
			}
			a.vec.mu.Lock()
			last := a.vec.lastFire
			a.vec.mu.Unlock()
			if d := sched.Every(); d > 0 {
				if !last.IsZero() && now.Sub(last) < d {
					continue
				}
			} else {
				// 30s tick 同一分钟可能命中两次（整分前后各一 tick），
				// 按分钟去重防止双触发（对齐 cron 调度器分钟语义）。
				if !last.IsZero() && last.Truncate(time.Minute).Equal(now.Truncate(time.Minute)) {
					continue
				}
				if !sched.Match(now) {
					continue
				}
			}
			a.vec.mu.Lock()
			a.vec.lastFire = now
			a.vec.mu.Unlock()
			slog.Info("定时向量化触发", "schedule", expr)
			a.vectorRunIndex()
		}
	}()
}

// pushVectorStatus 推送状态到前端（kaelvoris:vector_status；前端刷新状态行）。
func (a *App) pushVectorStatus() {
	if a.ctx == nil {
		return
	}
	st, _ := a.VectorStatus()
	uiEmit(a.ctx, "kaelvoris:vector_status", st)
}

// ---------- Wails 绑定（前端设置页「向量库」） ----------

// GetVectorConfig 读取向量化配置（nil = 未配置，返回零值）。
func (a *App) GetVectorConfig() vector.Config {
	if vc := a.vectorConfig(); vc != nil {
		return *vc
	}
	return vector.Config{}
}

// SetVectorConfig 保存向量化配置（校验 schedule/目录；立即生效：调度循环
// 每 tick 重读配置，下一任务 buildRunEngine 重建工具集）。
func (a *App) SetVectorConfig(c vector.Config) error {
	c.Dir = strings.TrimSpace(c.Dir)
	c.Schedule = strings.TrimSpace(c.Schedule)
	c.EmbedURL = strings.TrimSpace(c.EmbedURL)
	c.EmbedModel = strings.TrimSpace(c.EmbedModel)
	if c.Schedule != "" {
		if _, err := cron.Parse(c.Schedule); err != nil {
			return fmt.Errorf("自动化时间非法: %w", err)
		}
	}
	if c.Enabled && c.Dir == "" {
		return errors.New("启用向量化前需先配置待向量化目录")
	}
	if c.Enabled && strings.TrimSpace(c.EmbedModel) == "" {
		return errors.New("embedding 模型不能为空（如 text-embedding-3-small）")
	}
	cfg := a.GetConfig()
	cfg.Vector = &c
	if err := a.SaveConfig(cfg); err != nil {
		return err
	}
	a.pushVectorStatus()
	return nil
}

// VectorStatus 向量库状态（点数/文件数/运行中/最近索引/最近错误/调度描述）。
func (a *App) VectorStatus() (map[string]any, error) {
	a.vec.mu.Lock()
	lastFiles := a.vec.lastFiles
	lastBlocks := a.vec.lastBlocks
	st := map[string]any{
		"running":  a.vec.running,
		"lastErr":  a.vec.lastErr,
		"lastDone": a.vec.lastDone.Format("2006-01-02 15:04"),
		"lastFire": a.vec.lastFire.Format("2006-01-02 15:04"),
	}
	a.vec.mu.Unlock()

	// store 统计（含全部历史索引）；lastFiles/lastBlocks 仅最近一次增量结果。
	pts, nFiles := a.vectorStore().Count()
	st["points"] = pts
	st["files"] = nFiles
	st["lastFiles"] = lastFiles
	st["lastBlocks"] = lastBlocks
	if vc := a.vectorConfig(); vc != nil {
		st["enabled"] = vc.Enabled
		st["dir"] = vc.Dir
		st["schedule"] = vc.Schedule
		if expr := strings.TrimSpace(vc.Schedule); expr != "" {
			st["scheduleDesc"] = cron.Describe(expr)
		}
	}
	return st, nil
}

// VectorIndexNow 手动触发一次索引（异步；结果经 kaelvoris:vector_status 推送）。
func (a *App) VectorIndexNow() error {
	vc := a.vectorConfig()
	if vc == nil || !vc.Enabled {
		return errors.New("向量化未开启，请先在设置中启用并保存")
	}
	if strings.TrimSpace(vc.Dir) == "" {
		return errors.New("未配置待向量化目录")
	}
	a.vec.mu.Lock()
	busy := a.vec.running
	a.vec.mu.Unlock()
	if busy {
		return errors.New("索引进行中，请稍候")
	}
	a.vectorRunIndex()
	return nil
}

// VectorClear 清空向量库（含索引清单；配置保留）。
func (a *App) VectorClear() error {
	if err := a.vectorStore().Clear(); err != nil {
		return err
	}
	// 同步清 manifest（增量清单与库保持一致，避免下次索引误判跳过）。
	_ = os.Remove(filepath.Join(vectorDir(), "manifest.json"))
	a.vec.mu.Lock()
	a.vec.lastFiles, a.vec.lastBlocks = 0, 0
	a.vec.mu.Unlock()
	a.pushVectorStatus()
	return nil
}

// PickVectorDir 打开目录选择对话框（待向量化目录；无头构建降级为报错）。
func (a *App) PickVectorDir() (string, error) {
	dir, err := uiPickDir(a.ctx, "选择待向量化目录")
	if err != nil {
		return "", err
	}
	return dir, nil
}

// ---------- codebase_search 工具（对齐 Zoo-Code CodebaseSearchTool.ts） ----------

// CodebaseSearchTool 语义检索工具：LLM 以自然语言查询向量知识库，
// 返回 "file:start-end\n正文" 格式的命中片段（注册处注入 Search 闭包，
// 检索参数（条数/阈值）由 vector.Config 决定）。
type CodebaseSearchTool struct {
	Search func(ctx context.Context, query string, maxResults int) (string, error)
}

func (t *CodebaseSearchTool) Name() string { return "codebase_search" }

func (t *CodebaseSearchTool) Description() string {
	return "Perform semantic search over a pre-indexed vector knowledge base " +
		"(project docs / code directories embedded into a local vector store). " +
		"Use this BEFORE writing code or answering questions when project-specific " +
		"knowledge may exist in the indexed directory (conventions, design docs, " +
		"similar implementations, past decisions). Returns matched snippets as " +
		"\"relative/path:start-end\\n content\" — follow up with fs_read on the " +
		"cited ranges for full context. Query with natural language; lexical exact " +
		"matching is NOT required."
}

func (t *CodebaseSearchTool) Parameters() map[string]interface{} {
	// JSONSchema 内联构造（objectSchema/strProp 为 internal/tools 包内私有，
	// main 包不可引用；保持与 tools.objectSchema 相同的输出形状）。
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "Natural-language search query (e.g. \"websocket 断线重连逻辑\", \"上传接口鉴权约定\")",
			},
		},
		"required": []string{"query"},
	}
}

func (t *CodebaseSearchTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Query string `json:"query"`
	}
	if strings.TrimSpace(args) == "" {
		return "", fmt.Errorf("query 不能为空")
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(p.Query) == "" {
		return "", fmt.Errorf("query 不能为空")
	}
	return t.Search(ctx, p.Query, 0)
}
