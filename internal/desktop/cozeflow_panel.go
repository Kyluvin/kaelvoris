package desktop

// cozeflow_panel.go：嵌入式 Coze 工作流内核（internal/cozeflow）的 Wails 绑定面板。
//
// 提供 AI Flow 的后端引擎接入：工作流创建/画布保存/发布/同步执行/删除，
// 全部能力来自 coze-studio 源码融合（SQLite + 内存缓存/事件总线/本地存储，
// 零外部服务依赖）。Bootstrap 惰性执行（首次访问任一 Coze* 方法时装配，
// 失败状态缓存避免反复重试刷日志）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"kaelvoris/internal/cozeflow"
	"kaelvoris/internal/mcp"

	"github.com/coze-dev/coze-studio/backend/api/model/data/database/table"
	database "github.com/coze-dev/coze-studio/backend/crossdomain/database/model"
	knowledgeModel "github.com/coze-dev/coze-studio/backend/crossdomain/knowledge/model"
	kentity "github.com/coze-dev/coze-studio/backend/domain/knowledge/entity"
	knowledgeSvc "github.com/coze-dev/coze-studio/backend/domain/knowledge/service"
	dbservice "github.com/coze-dev/coze-studio/backend/domain/memory/database/service"

	"github.com/coze-dev/coze-studio/backend/api/model/workflow"
	"github.com/coze-dev/coze-studio/backend/bizpkg/config"
	model "github.com/coze-dev/coze-studio/backend/crossdomain/workflow/model"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
)

// cozeDir 嵌入式 coze 内核数据根目录（~/.kaelvoris/coze/：coze.db / oss/ / resources/）。
func cozeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "coze")
}

// CozeStatus 嵌入式内核装配状态（前端面板顶栏渲染）。
type CozeStatus struct {
	Ready           bool   `json:"ready"`           // Bootstrap 是否已完成
	ModelConfigured bool   `json:"modelConfigured"` // 内置 LLM（agnes）是否探测可用
	DataDir         string `json:"dataDir"`         // 数据目录
	Error           string `json:"error,omitempty"` // 装配失败原因（Ready=false 时）
}

// CozeWorkflowMeta 工作流列表条目（对应 coze workflow_meta 表）。
// Kaelvoris 补丁（2026-10）：ID 用 string 传输——int64 超出 JS Number 安全整数
// 范围（2^53），Wails JSON 序列化会精度丢失（尾数归零），前端回传后查询 not found。
type CozeWorkflowMeta struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Desc          string `json:"desc"`
	HasPublished  bool   `json:"hasPublished"`
	LatestVersion string `json:"latestVersion,omitempty"`
	Mode          int64  `json:"mode"` // 0=workflow 3=chatflow
	UpdatedAtUnix int64  `json:"updatedAtUnix"`
}

// CozeWorkflowDetail 工作流详情（编辑器载入）。
type CozeWorkflowDetail struct {
	CozeWorkflowMeta
	Canvas     string `json:"canvas"`               // 画布 JSON（前端编辑器数据源）
	CommitID   string `json:"commitId"`             // 草稿 commit（发布时引用）
	InputJSON  string `json:"inputJSON,omitempty"`  // 输入参数定义（调试表单渲染）
	OutputJSON string `json:"outputJSON,omitempty"` // 输出参数定义
}

// CozeRunResult 同步执行结果。
type CozeRunResult struct {
	Status     string         `json:"status"` // success/failed/cancelled/interrupted
	Output     map[string]any `json:"output"` // 结束节点返回变量（returnVariables 形态）
	OutputRaw  string         `json:"outputRaw,omitempty"`
	DurationMs int64          `json:"durationMs"`
	FailReason string         `json:"failReason,omitempty"`
	ExecID     string         `json:"execId"`              // int64 字符串化（同 ID 精度原因）
	NodeExecs  []CozeNodeExec `json:"nodeExecs,omitempty"` // 节点级执行轨迹（Dify 式逐节点状态）
}

// CozeNodeExec 节点级执行摘要（对应 coze NodeExecution）。
type CozeNodeExec struct {
	NodeID     string `json:"nodeId"`
	NodeType   string `json:"nodeType"`
	Status     string `json:"status"` // waiting/running/success/fail
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	FailReason string `json:"failReason,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

// cozeEnsure 惰性装配嵌入式内核（幂等；失败缓存错误避免重试风暴）。
func (a *App) cozeEnsure() (*cozeflow.Coze, error) {
	a.cozeMu.Lock()
	defer a.cozeMu.Unlock()
	if a.coze != nil {
		return a.coze, nil
	}
	if a.cozeErr != "" {
		return nil, errors.New(a.cozeErr)
	}
	c, err := cozeflow.Bootstrap(context.Background(), cozeDir())
	if err != nil {
		a.cozeErr = err.Error()
		slog.Error("cozeflow: 嵌入式内核装配失败", "err", err)
		return nil, err
	}
	a.coze = c
	slog.Info("cozeflow: 嵌入式内核已装配", "dir", cozeDir())
	return c, nil
}

// CozeInit 显式装配（前端进入 AI Flow 页时调用；返回当前状态）。
func (a *App) CozeInit() CozeStatus {
	if _, err := a.cozeEnsure(); err != nil {
		return CozeStatus{DataDir: cozeDir(), Error: err.Error()}
	}
	a.cozeMu.Lock()
	defer a.cozeMu.Unlock()
	return CozeStatus{
		Ready:           true,
		ModelConfigured: a.coze.ModelConfigured,
		DataDir:         a.coze.DataDir,
	}
}

// CozeStatus 查询装配状态（不触发装配）。
func (a *App) CozeStatus() CozeStatus {
	a.cozeMu.Lock()
	defer a.cozeMu.Unlock()
	s := CozeStatus{DataDir: cozeDir()}
	if a.coze != nil {
		s.Ready = true
		s.ModelConfigured = a.coze.ModelConfigured
		s.DataDir = a.coze.DataDir
	} else if a.cozeErr != "" {
		s.Error = a.cozeErr
	}
	return s
}

// CozeListWorkflows 工作流列表（keyword 非空时按名称/描述模糊过滤，本地内存过滤）。
func (a *App) CozeListWorkflows(keyword string) ([]CozeWorkflowMeta, error) {
	c, err := a.cozeEnsure()
	if err != nil {
		return nil, err
	}
	wfs, _, err := c.Workflow.DomainSVC.MGet(context.Background(), &vo.MGetPolicy{
		MetaQuery: vo.MetaQuery{
			SpaceID: ptrI64(1),
			Mode:    modeWorkflow(),
			// Kaelvoris 补丁（2026-10）：MGetMetas 校验 IDs/Page/Name/AppID 至少一项非空，
			// 仅传 SpaceID 会报 720701007 insufficient query parameters；补分页参数走全量列表。
			Page: &vo.Page{Page: 1, Size: 500},
		},
		MetaOnly: true,
	})
	if err != nil {
		return nil, fmt.Errorf("查询工作流列表失败: %w", err)
	}
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	out := make([]CozeWorkflowMeta, 0, len(wfs))
	for _, w := range wfs {
		if w.Meta == nil {
			continue
		}
		if keyword != "" &&
			!strings.Contains(strings.ToLower(w.Meta.Name), keyword) &&
			!strings.Contains(strings.ToLower(w.Meta.Desc), keyword) {
			continue
		}
		m := CozeWorkflowMeta{
			ID:            strconv.FormatInt(w.ID, 10),
			Name:          w.Meta.Name,
			Desc:          w.Meta.Desc,
			HasPublished:  w.Meta.HasPublished,
			Mode:          int64(w.Meta.Mode),
			UpdatedAtUnix: 0,
		}
		if w.Meta.UpdatedAt != nil {
			m.UpdatedAtUnix = w.Meta.UpdatedAt.UnixMilli()
		}
		if w.Meta.LatestPublishedVersion != nil {
			m.LatestVersion = *w.Meta.LatestPublishedVersion
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAtUnix > out[j].UpdatedAtUnix })
	return out, nil
}

// cozeDefaultCanvas 返回新建工作流的画布 JSON 模板（start → 大模型 → end，可直接整图运行）。
// 注意：2026-10-28 曾出现两处括号错误（sonic 720701011）：① outputs 数组条目缺收尾 '}'；
// ② 节点对象缺收尾 '}'（每个节点完整收尾 = outputs']' + data'}' + node'}'）。
// 模板改动后必须过 TestCozeDefaultCanvasJSON 断言。
func cozeDefaultCanvas() string {
	return `{"nodes":[` +
		`{"id":"100001","type":"1","data":{"nodeMeta":{"title":"开始"},` +
		`"outputs":[{"name":"query","type":"string","description":"用户输入"}]}},` +
		`{"id":"100002","type":"3","data":{"nodeMeta":{"title":"大模型"},` +
		`"inputs":{"llmParam":[` +
		`{"name":"modelType","input":{"type":"literal","value":{"type":"literal","content":"69010"}}},` +
		`{"name":"modleName","input":{"type":"literal","value":{"type":"literal","content":"agnes-2.5-flash"}}},` +
		`{"name":"systemPrompt","input":{"type":"literal","value":{"type":"literal","content":"你是一个有帮助的助手。"}}},` +
		`{"name":"prompt","input":{"type":"literal","value":{"type":"literal","content":"{{query}}"}}},` +
		`{"name":"responseFormat","input":{"type":"literal","value":{"type":"literal","content":"0"}}},` +
		`{"name":"temperature","input":{"type":"literal","value":{"type":"literal","content":"0.7"}}}]},` +
		`"outputs":[{"name":"output","type":"string","description":"模型回答"}]}},` +
		`{"id":"900001","type":"2","data":{"nodeMeta":{"title":"结束"},` +
		`"inputs":{"terminatePlan":"returnVariables",` +
		`"inputParameters":[{"name":"answer","input":{"type":"string","value":` +
		`{"type":"ref","content":{"blockID":"100002","name":"output","source":"block-output"}}}}]},` +
		`"outputs":[{"name":"answer","type":"string"}]}}],` +
		`"edges":[{"sourceNodeID":"100001","targetNodeID":"100002"},` +
		`{"sourceNodeID":"100002","targetNodeID":"900001"}]}`
}

// CozeCreateWorkflow 创建工作流（空画布：start → end 基础模板）。
func (a *App) CozeCreateWorkflow(name, desc string) (*CozeWorkflowMeta, error) {
	c, err := a.cozeEnsure()
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("工作流名称不能为空")
	}
	// 基础模板（Kaelvoris 补丁 2026-10）：start → 大模型 → end 可直接整图运行；
	// 旧纯结构模板（start→end）执行会报 720701013 no tasks to execute after graph start。
	// 大模型节点用内置 agnes-2.5-flash（modelType=69010，Bootstrap 日志确认），
	// llmParam 数值参数必须是 JSON 字符串（内核 Content.(string) 强转）。
	canvas := cozeDefaultCanvas()
	id, err := c.Workflow.DomainSVC.Create(context.Background(), &vo.MetaCreate{
		Name:             name,
		Desc:             strings.TrimSpace(desc),
		SpaceID:          1,
		CreatorID:        1,
		ContentType:      0, // WorkFlowType_User
		Mode:             0, // WorkflowMode_Workflow
		InitCanvasSchema: canvas,
	})
	if err != nil {
		return nil, fmt.Errorf("创建工作流失败: %w", err)
	}
	return &CozeWorkflowMeta{
		ID:            strconv.FormatInt(id, 10),
		Name:          name,
		Desc:          strings.TrimSpace(desc),
		UpdatedAtUnix: time.Now().UnixMilli(),
	}, nil
}

// CozeGetWorkflow 读取工作流详情（草稿形态画布 + 输入/输出参数定义）。
// id 为 int64 字符串（前端传输，避免 JS Number 精度丢失）。
func (a *App) CozeGetWorkflow(id string) (*CozeWorkflowDetail, error) {
	c, err := a.cozeEnsure()
	if err != nil {
		return nil, err
	}
	wfID, err := cozeParseID(id)
	if err != nil {
		return nil, err
	}
	wf, err := c.Workflow.DomainSVC.Get(context.Background(), &vo.GetPolicy{ID: wfID})
	if err != nil {
		return nil, fmt.Errorf("读取工作流失败: %w", err)
	}
	if wf == nil {
		return nil, fmt.Errorf("工作流不存在: %s", id)
	}
	d := &CozeWorkflowDetail{
		Canvas:     wf.CanvasInfo.Canvas,
		CommitID:   wf.CommitID,
		InputJSON:  wf.CanvasInfo.InputParamsStr,
		OutputJSON: wf.CanvasInfo.OutputParamsStr,
	}
	if wf.Meta != nil {
		d.ID = strconv.FormatInt(wf.ID, 10)
		d.Name = wf.Meta.Name
		d.Desc = wf.Meta.Desc
		d.HasPublished = wf.Meta.HasPublished
		d.Mode = int64(wf.Meta.Mode)
		if wf.Meta.LatestPublishedVersion != nil {
			d.LatestVersion = *wf.Meta.LatestPublishedVersion
		}
		if wf.Meta.UpdatedAt != nil {
			d.UpdatedAtUnix = wf.Meta.UpdatedAt.UnixMilli()
		}
	}
	// CanvasInfo 由 repo DraftV2/GetVersion 填充（Get 默认走草稿形态）。
	return d, nil
}

// CozeSaveWorkflow 保存画布草稿（canvas 为画布 JSON 全量字符串；id 为字符串形态）。
func (a *App) CozeSaveWorkflow(id string, canvas string) error {
	c, err := a.cozeEnsure()
	if err != nil {
		return err
	}
	wfID, err := cozeParseID(id)
	if err != nil {
		return err
	}
	return c.Workflow.DomainSVC.Save(context.Background(), wfID, canvas)
}

// CozePublishWorkflow 发布工作流（version 需自增，如 0.0.1 → 0.0.2；force 绕过试运行前置）。
func (a *App) CozePublishWorkflow(id string, version, desc string, force bool) error {
	c, err := a.cozeEnsure()
	if err != nil {
		return err
	}
	wfID, err := cozeParseID(id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(version) == "" {
		// 未传版本号：取最新版本号 +0.0.1 自增
		wf, gerr := c.Workflow.DomainSVC.Get(context.Background(), &vo.GetPolicy{ID: wfID})
		if gerr != nil {
			return gerr
		}
		if wf != nil && wf.Meta != nil && wf.Meta.LatestPublishedVersion != nil {
			version = bumpVersion(*wf.Meta.LatestPublishedVersion)
		} else {
			version = "0.0.1"
		}
	}
	return c.Workflow.DomainSVC.Publish(context.Background(), &vo.PublishPolicy{
		ID:                 wfID,
		Version:            strings.TrimSpace(version),
		VersionDescription: strings.TrimSpace(desc),
		CreatorID:          1,
		Force:              force,
	})
}

// CozeModelOption 模型下拉选项（大模型节点表单）。
type CozeModelOption struct {
	ID   string `json:"id"`   // modelType（模型表主键，字符串化防精度丢失）
	Name string `json:"name"` // 展示名
}

// CozeListModels 内核在线模型列表（modelmgr.GetOnlineModelList）——大模型节点模型下拉数据源。
func (a *App) CozeListModels() ([]*CozeModelOption, error) {
	if _, err := a.cozeEnsure(); err != nil {
		return nil, err
	}
	models, err := config.ModelConf().GetOnlineModelList(context.Background())
	if err != nil {
		return nil, fmt.Errorf("读取模型列表失败: %w", err)
	}
	out := make([]*CozeModelOption, 0, len(models))
	for _, m := range models {
		name := ""
		if m.DisplayInfo != nil {
			name = m.DisplayInfo.Name
		}
		if name == "" {
			name = strconv.FormatInt(m.ID, 10)
		}
		out = append(out, &CozeModelOption{ID: strconv.FormatInt(m.ID, 10), Name: name})
	}
	return out, nil
}

// CozeMCPTool MCP 工具条目（工作流 MCP 节点的工具下拉）。
type CozeMCPTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CozeListMCPTools 连接设置中的指定 MCP 服务器（http/sse 传输）并列出工具清单。
// 工作流 MCP 节点（HTTP 预设）选择服务器/工具后自动生成 tools/call 请求；
// stdio 传输无法转为 HTTP 节点（前端下拉禁用该项并提示）。
func (a *App) CozeListMCPTools(serverName string) ([]*CozeMCPTool, error) {
	name := strings.TrimSpace(serverName)
	if name == "" {
		return nil, errors.New("请选择 MCP 服务器")
	}
	for _, s := range a.loadMCPServers() {
		if s.Name != name {
			continue
		}
		if !s.Enabled {
			return nil, fmt.Errorf("MCP 服务器「%s」已禁用，请先在设置中开启", name)
		}
		if s.Transport != "http" && s.Transport != "sse" {
			return nil, fmt.Errorf("仅支持 http/sse 传输的 MCP 服务器（「%s」为 %s）", name, s.Transport)
		}
		cfg := mcp.ServerConfig{Name: s.Name, URL: s.URL, Headers: s.Headers}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cli, _, err := mcp.RegisterAll(ctx, cfg, func(string, *mcp.RemoteTool) error { return nil })
		if err != nil {
			return nil, fmt.Errorf("连接 MCP 服务器失败: %w", err)
		}
		defer cli.Close()
		infos, err := cli.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("枚举工具失败: %w", err)
		}
		out := make([]*CozeMCPTool, 0, len(infos))
		for _, t := range infos {
			out = append(out, &CozeMCPTool{Name: t.Name, Description: t.Description})
		}
		return out, nil
	}
	return nil, fmt.Errorf("未找到 MCP 服务器「%s」", name)
}

// CozeImageGenConfig 图片生成模型配置（设置 → 图片生成模型，OpenAI images API 兼容端点）。
type CozeImageGenConfig struct {
	Configured bool   `json:"configured"`
	BaseURL    string `json:"baseURL"`
	Model      string `json:"model"`
	APIKey     string `json:"apiKey"`
}

// CozeKnowledgeBase 知识库条目（知识库检索节点下拉数据源）。
type CozeKnowledgeBase struct {
	ID          string `json:"id"` // datasetID（字符串化防精度丢失）
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// CozeListKnowledgeBases 嵌入内核的知识库列表（space 1 全量）。
func (a *App) CozeListKnowledgeBases() ([]*CozeKnowledgeBase, error) {
	c, err := a.cozeEnsure()
	if err != nil {
		return nil, err
	}
	page, size := 1, 200
	resp, err := c.KnowledgeDomainSVC.ListKnowledge(context.Background(), &knowledgeSvc.ListKnowledgeRequest{
		SpaceID:  ptrI64(1),
		Page:     &page,
		PageSize: &size,
	})
	if err != nil {
		return nil, fmt.Errorf("读取知识库列表失败: %w", err)
	}
	out := make([]*CozeKnowledgeBase, 0, len(resp.KnowledgeList))
	for _, kn := range resp.KnowledgeList {
		if kn == nil {
			continue
		}
		out = append(out, &CozeKnowledgeBase{
			ID:          strconv.FormatInt(kn.ID, 10),
			Name:        kn.Name,
			Description: kn.Description,
		})
	}
	return out, nil
}

// CozeCreateKnowledge 创建文本知识库并写入一段自定义文本文档（本地切片索引后可被检索节点召回）。
// 文档经事件总线异步切片入库；小文本通常毫秒级完成。
func (a *App) CozeCreateKnowledge(name, content string) (*CozeKnowledgeBase, error) {
	c, err := a.cozeEnsure()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("知识库名称不能为空")
	}
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("知识内容不能为空")
	}
	resp, err := c.KnowledgeDomainSVC.CreateKnowledge(context.Background(), &knowledgeSvc.CreateKnowledgeRequest{
		Name:       strings.TrimSpace(name),
		CreatorID:  1,
		SpaceID:    1,
		FormatType: knowledgeModel.DocumentTypeText,
	})
	if err != nil {
		return nil, fmt.Errorf("创建知识库失败: %w", err)
	}
	if _, err := c.KnowledgeDomainSVC.CreateDocument(context.Background(), &knowledgeSvc.CreateDocumentRequest{
		Documents: []*kentity.Document{{
			KnowledgeID: resp.KnowledgeID,
			Info: knowledgeModel.Info{ // 嵌入字段需显式写 Info（go1.25 不支持 promoted field 字面量）
				CreatorID: 1,
				SpaceID:   1,
			},
			Type:       knowledgeModel.DocumentTypeText,
			Source:     kentity.DocumentSourceCustom, // 自定义文本
			RawContent: content,
		}},
	}); err != nil {
		return nil, fmt.Errorf("写入知识文档失败: %w", err)
	}
	return &CozeKnowledgeBase{
		ID:          strconv.FormatInt(resp.KnowledgeID, 10),
		Name:        strings.TrimSpace(name),
		Description: "",
	}, nil
}

// ── 数据源（外部 MySQL 连接，JDBC 式；SQL 自定义节点 + 设置面板共用）──

// CozeDatasource 数据源连接（设置面板管理；ID 为负数字符串，即 SQL 节点 databaseInfoID）。
type CozeDatasource struct {
	ID        string `json:"id"`        // 负数 ID（字符串化传输）
	Name      string `json:"name"`      // 连接名称
	Host      string `json:"host"`      // 主机
	Port      int    `json:"port"`      // 端口（默认 3306）
	User      string `json:"user"`      // 用户名
	Password  string `json:"password"`  // 密码（桌面单机，本地明文存储）
	DBName    string `json:"dbName"`    // 库名
	TableName string `json:"tableName"` // 绑定表名（可选；查询/新增/更新/删除节点需要，SQL 自定义节点不需要）
	JDBCURL   string `json:"jdbcURL"`   // 展示用 JDBC 连接串
}

// cozeDatasource 面板结构 → 内核存储结构。
func cozeDatasource(c *CozeDatasource) *cozeflow.Datasource {
	return &cozeflow.Datasource{
		ID:        parseCozeID(c.ID),
		Name:      strings.TrimSpace(c.Name),
		Host:      strings.TrimSpace(c.Host),
		Port:      c.Port,
		User:      c.User,
		Password:  c.Password,
		DBName:    strings.TrimSpace(c.DBName),
		TableName: strings.TrimSpace(c.TableName),
	}
}

// cozeDatasourceOut 内核存储结构 → 面板结构（附 JDBC 展示串）。
func cozeDatasourceOut(d *cozeflow.Datasource) *CozeDatasource {
	return &CozeDatasource{
		ID:        strconv.FormatInt(d.ID, 10),
		Name:      d.Name,
		Host:      d.Host,
		Port:      d.Port,
		User:      d.User,
		Password:  d.Password,
		DBName:    d.DBName,
		TableName: d.TableName,
		JDBCURL:   d.JDBCURL(),
	}
}

// CozeListDatasources 外部 MySQL 连接列表（设置面板）。
func (a *App) CozeListDatasources() ([]*CozeDatasource, error) {
	list := cozeflow.LoadDatasources()
	out := make([]*CozeDatasource, 0, len(list))
	for _, d := range list {
		out = append(out, cozeDatasourceOut(d))
	}
	return out, nil
}

// CozeSaveDatasource 新增（ID 为空）或更新连接；保存前先测连接。
func (a *App) CozeSaveDatasource(ds *CozeDatasource) (*CozeDatasource, error) {
	d := cozeDatasource(ds)
	if d.Name == "" {
		return nil, errors.New("连接名称不能为空")
	}
	if d.Port == 0 {
		d.Port = 3306
	}
	if err := cozeflow.TestDatasource(d); err != nil {
		return nil, fmt.Errorf("连接测试失败（未保存）: %w", err)
	}
	_, saved, err := cozeflow.UpsertDatasource(d)
	if err != nil {
		return nil, err
	}
	return cozeDatasourceOut(saved), nil
}

// CozeDeleteDatasource 删除连接。
func (a *App) CozeDeleteDatasource(id string) error {
	id64 := parseCozeID(id)
	if id64 == 0 {
		return errors.New("数据源 ID 无效")
	}
	_, err := cozeflow.RemoveDatasource(id64)
	return err
}

// CozeTestDatasource 测试连接（不保存）。
func (a *App) CozeTestDatasource(ds *CozeDatasource) error {
	return cozeflow.TestDatasource(cozeDatasource(ds))
}

// CozeListDatabases SQL 自定义节点的数据源列表（= 外部 MySQL 连接，负数 ID）。
// 保留原名兼容前端 dbLoader；fields 恒空（连接无 schema 字段）。
func (a *App) CozeListDatabases() ([]*CozeDatabase, error) {
	list := cozeflow.LoadDatasources()
	out := make([]*CozeDatabase, 0, len(list))
	for _, d := range list {
		out = append(out, &CozeDatabase{
			ID:   strconv.FormatInt(d.ID, 10),
			Name: d.Name,
			Desc: fmt.Sprintf("%s@%s:%d/%s（MySQL）", d.User, d.Host, d.Port, d.DBName),
		})
	}
	return out, nil
}

// ── 以下为嵌入式内部表绑定（早期方案遗留；SQL 节点已切换外部 MySQL 连接，UI 不再引用）──

// CozeDBField 数据表字段定义（内部表建表单）。
type CozeDBField struct {
	Name     string `json:"name"`     // 字段名（英文标识，作为列名）
	Type     string `json:"type"`     // text | number | float | boolean | date
	Desc     string `json:"desc"`     // 字段描述
	Required bool   `json:"required"` // 是否必填
}

// CozeDatabase 数据源（数据库表）条目（SQL 节点下拉 + 设置面板列表）。
type CozeDatabase struct {
	ID     string         `json:"id"`   // online 库 ID（字符串化防精度丢失，即 SQL 节点 databaseInfoID）
	Name   string         `json:"name"` // 表名
	Desc   string         `json:"desc,omitempty"`
	Fields []*CozeDBField `json:"fields,omitempty"`
}

// cozeDBFieldType 字符串类型 → 内核枚举（table.FieldItemType）。
func cozeDBFieldType(s string) table.FieldItemType {
	switch s {
	case "number":
		return table.FieldItemType_Number
	case "float":
		return table.FieldItemType_Float
	case "boolean":
		return table.FieldItemType_Boolean
	case "date":
		return table.FieldItemType_Date
	default:
		return table.FieldItemType_Text
	}
}

// cozeDBFieldTypeName 内核枚举 → 字符串类型（回显用）。
func cozeDBFieldTypeName(t table.FieldItemType) string {
	switch t {
	case table.FieldItemType_Number:
		return "number"
	case table.FieldItemType_Float:
		return "float"
	case table.FieldItemType_Boolean:
		return "boolean"
	case table.FieldItemType_Date:
		return "date"
	default:
		return "text"
	}
}

// CozeCreateDatabase 创建数据库表（字段列表 + 可选初始记录），返回 online 库条目。
// 建库同时创建 draft/online 两张物理表（SQLite）；初始记录经 AddDatabaseRecord 写 online 表。
func (a *App) CozeCreateDatabase(name, desc string, fields []*CozeDBField, records []map[string]string) (*CozeDatabase, error) {
	c, err := a.cozeEnsure()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("表名不能为空")
	}
	if len(fields) == 0 {
		return nil, errors.New("至少定义一个字段")
	}
	fieldItems := make([]*database.FieldItem, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		n := strings.TrimSpace(f.Name)
		if n == "" || seen[n] {
			return nil, fmt.Errorf("字段名「%s」为空或重复", n)
		}
		seen[n] = true
		fieldItems = append(fieldItems, &database.FieldItem{
			Name:         n,
			Desc:         f.Desc,
			Type:         cozeDBFieldType(f.Type),
			MustRequired: f.Required,
		})
	}
	resp, err := c.DatabaseDomainSVC.CreateDatabase(context.Background(), &dbservice.CreateDatabaseRequest{
		Database: &database.Database{
			TableName: strings.TrimSpace(name),
			TableDesc: desc,
			CreatorID: 1,
			SpaceID:   1,
			RwMode:    table.BotTableRWMode_UnlimitedReadWrite,
			FieldList: fieldItems,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("创建数据表失败: %w", err)
	}
	// 可选初始记录：列名 → 字符串值（内核按字段类型转换）
	if len(records) > 0 {
		if err := c.DatabaseDomainSVC.AddDatabaseRecord(context.Background(), &dbservice.AddDatabaseRecordRequest{
			DatabaseID: resp.Database.ID,
			TableType:  table.TableType_OnlineTable,
			UserID:     1,
			Records:    records,
		}); err != nil {
			return nil, fmt.Errorf("写入初始记录失败: %w", err)
		}
	}
	return &CozeDatabase{
		ID:     strconv.FormatInt(resp.Database.ID, 10),
		Name:   resp.Database.TableName,
		Desc:   resp.Database.TableDesc,
		Fields: fields,
	}, nil
}

// CozeDeleteDatabase 删除数据库表（online + draft 一并删除）。
func (a *App) CozeDeleteDatabase(id string) error {
	c, err := a.cozeEnsure()
	if err != nil {
		return err
	}
	id64, err := strconv.ParseInt(id, 10, 64)
	if err != nil || id64 == 0 {
		return errors.New("数据源 ID 无效")
	}
	if err := c.DatabaseDomainSVC.DeleteDatabase(context.Background(), &database.DeleteDatabaseRequest{ID: id64}); err != nil {
		return fmt.Errorf("删除数据表失败: %w", err)
	}
	return nil
}

// CozeGetImageGenConfig 读取设置中的图片生成模型配置——工作流图片生成节点
// 的数据源（节点按 HTTP 类型执行 POST {baseURL}/images/generations）。
// 桌面端单机场景：APIKey 原样返回（仅注入节点请求头，抽屉不回显明文）。
func (a *App) CozeGetImageGenConfig() *CozeImageGenConfig {
	cfg := a.GetConfig()
	base := strings.TrimRight(strings.TrimSpace(cfg.ImageGenBaseURL), "/")
	return &CozeImageGenConfig{
		Configured: base != "" && strings.TrimSpace(cfg.ImageGenModel) != "",
		BaseURL:    base,
		Model:      strings.TrimSpace(cfg.ImageGenModel),
		APIKey:     cfg.ImageGenAPIKey,
	}
}

// CozeRunWorkflow 同步执行工作流（fromDraft=true 跑草稿，false 跑最新发布版）。
func (a *App) CozeRunWorkflow(id string, input map[string]any, fromDraft bool) (*CozeRunResult, error) {
	c, err := a.cozeEnsure()
	if err != nil {
		return nil, err
	}
	wfID, err := cozeParseID(id)
	if err != nil {
		return nil, err
	}
	from := model.FromLatestVersion
	if fromDraft {
		from = model.FromDraft
	}
	if input == nil {
		input = map[string]any{}
	}
	// 运行前画布校验（Kaelvoris 补丁 2026-10）：纯结构图（仅 start/end）执行会报
	// 720701013 no tasks to execute after graph start，端节点引用不存在的上游会报
	// can not find dependency——提前给出可操作的错误提示。
	if err := cozeValidateCanvas(c, wfID, from); err != nil {
		return nil, err
	}
	start := time.Now()
	exe, _, err := c.Workflow.DomainSVC.SyncExecute(context.Background(), model.ExecuteConfig{
		ID:           wfID,
		From:         from,
		Operator:     1,
		Mode:         model.ExecuteModeRelease,
		SyncPattern:  model.SyncPatternSync,
		BizType:      model.BizTypeWorkflow,
		WorkflowMode: 0, // WorkflowMode_Workflow
	}, input)
	if err != nil {
		return nil, err
	}
	r := &CozeRunResult{
		Status:     execStatusStr(exe.Status),
		DurationMs: time.Since(start).Milliseconds(),
		ExecID:     strconv.FormatInt(exe.ID, 10),
	}
	if exe.FailReason != nil {
		r.FailReason = *exe.FailReason
	}
	if exe.Output != nil {
		r.OutputRaw = *exe.Output
		_ = json.Unmarshal([]byte(*exe.Output), &r.Output)
	}
	// 节点级执行轨迹（GetExecution includeNodes 聚合后的 Nodes 列表）
	fillNodeExecs(r, exe)
	// 执行结果事件推送给前端（流式渲染侧栏运行记录）
	uiEmit(a.ctx, "kaelvoris:coze:run", r)
	return r, nil
}

// cozeValidateCanvas 整图运行前的画布合法性校验：
//  1. 至少一个可执行节点（type 非 1 开始 / 2 结束），否则内核报 720701013；
//  2. 结束节点的输出映射 ref 目标必须存在于画布（防"引用了未连线的上游"）。
func cozeValidateCanvas(c *cozeflow.Coze, wfID int64, from model.Locator) error {
	wf, err := c.Workflow.DomainSVC.Get(context.Background(), &vo.GetPolicy{ID: wfID})
	if err != nil || wf == nil {
		return nil // 查不到画布时交给内核原始报错
	}
	canvasStr := wf.CanvasInfo.Canvas
	if from == model.FromLatestVersion && wf.CommitID != "" {
		// 发布态画布略过（发布时已校验）
	}
	var cv struct {
		Nodes []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Data struct {
				Inputs struct {
					InputParameters []struct {
						Name  string `json:"name"`
						Input struct {
							Value struct {
								Type    string `json:"type"`
								Content struct {
									BlockID string `json:"blockID"`
								} `json:"content"`
							} `json:"value"`
						} `json:"input"`
					} `json:"inputParameters"`
				} `json:"inputs"`
			} `json:"data"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(canvasStr), &cv); err != nil {
		return nil // 画布非法时交内核报错
	}
	ids := map[string]bool{}
	executable := 0
	for _, n := range cv.Nodes {
		ids[n.ID] = true
		if n.Type != "1" && n.Type != "2" {
			executable++
		}
	}
	if executable == 0 {
		return errors.New("画布中没有可执行的节点：请从左侧节点库拖入大模型/代码/输出等节点并连线后再运行")
	}
	// 结束节点引用校验
	for _, n := range cv.Nodes {
		if n.Type != "2" {
			continue
		}
		for _, p := range n.Data.Inputs.InputParameters {
			ref := p.Input.Value
			if ref.Type == "ref" && ref.Content.BlockID != "" && !ids[ref.Content.BlockID] {
				return fmt.Errorf("结束节点的「%s」引用了画布中不存在的节点（%s），请检查连线或重新选择引用", p.Name, ref.Content.BlockID)
			}
		}
	}
	return nil
}

// CozeTestNode 单节点试运行（对标 Dify 的节点级测试）：
// 对当前工作流【草稿】上的指定节点发起内核原生 NodeDebug 执行
// （AsyncExecuteNode + ExecuteModeNodeDebug，内核自动以该节点为根构造单节点图），
// 轮询至终态后返回节点级执行详情。节点配置变更后需先保存草稿再试运行。
func (a *App) CozeTestNode(workflowID, nodeID string, input map[string]any) (*CozeRunResult, error) {
	c, err := a.cozeEnsure()
	if err != nil {
		return nil, err
	}
	wfID, err := cozeParseID(workflowID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(nodeID) == "" {
		return nil, fmt.Errorf("节点 ID 为空")
	}
	// 结构节点不可单节点试运行：Entry 只声明输入（单节点图无可执行任务），
	// Exit 依赖上游数据（单节点图无引用来源），二者均会报
	// 720701013 no tasks to execute after graph start。
	if nodeID == "100001" {
		return nil, fmt.Errorf("开始节点无需试运行（它只声明运行输入）；请从中间节点开始")
	}
	if nodeID == "900001" {
		return nil, fmt.Errorf("结束节点请使用顶部「运行」整图执行（其输出映射依赖上游节点数据）")
	}
	if input == nil {
		input = map[string]any{}
	}
	exeID, err := c.Workflow.DomainSVC.AsyncExecuteNode(context.Background(), nodeID, model.ExecuteConfig{
		ID:          wfID,
		From:        model.FromDraft,
		Operator:    1,
		Mode:        model.ExecuteModeNodeDebug,
		SyncPattern: model.SyncPatternSync,
		BizType:     model.BizTypeWorkflow,
	}, input)
	if err != nil {
		return nil, fmt.Errorf("节点试运行发起失败: %w", err)
	}

	// 轮询被测节点执行记录（200ms 间隔，60s 超时）。
	// 不走 GetExecution：其内部查 interrupt event 表，NodeDebug 短执行下
	// SQLite 存在空串记录导致 EOF；GetNodeExecution 直查节点执行表无此问题。
	start := time.Now()
	var nodeExe *entity.NodeExecution
	for {
		ne, _, err := c.Workflow.DomainSVC.GetNodeExecution(context.Background(), exeID, nodeID)
		if err == nil {
			if ne.Status == entity.NodeSuccess {
				// 终态但输出尚未落库（status 先于 output 写入的时序窗口）：重读一次
				if ne.Output == nil {
					time.Sleep(200 * time.Millisecond)
					if ne2, _, err2 := c.Workflow.DomainSVC.GetNodeExecution(context.Background(), exeID, nodeID); err2 == nil && ne2.Output != nil {
						ne = ne2
					}
				}
				nodeExe = ne
				break
			}
			if ne.Status == entity.NodeFailed {
				nodeExe = ne
				break
			}
		}
		if time.Since(start) > 60*time.Second {
			return nil, fmt.Errorf("节点试运行超时（60s），exeID=%d", exeID)
		}
		time.Sleep(200 * time.Millisecond)
	}

	r := &CozeRunResult{
		Status:     nodeExecStatusStr(nodeExe.Status),
		DurationMs: nodeExe.Duration.Milliseconds(),
		ExecID:     strconv.FormatInt(exeID, 10),
	}
	if nodeExe.ErrorInfo != nil {
		r.FailReason = *nodeExe.ErrorInfo
	}
	if nodeExe.Output != nil {
		r.OutputRaw = *nodeExe.Output
		_ = json.Unmarshal([]byte(*nodeExe.Output), &r.Output)
	}
	fillNodeExecs(r, &entity.WorkflowExecution{NodeExecutions: []*entity.NodeExecution{nodeExe}})
	return r, nil
}

// fillNodeExecs 从执行实体提取节点级轨迹（GetExecution includeNodes=true 时聚合于 NodeExecutions）。
func fillNodeExecs(r *CozeRunResult, exe *entity.WorkflowExecution) {
	if exe == nil || len(exe.NodeExecutions) == 0 {
		return
	}
	list := make([]CozeNodeExec, 0, len(exe.NodeExecutions))
	for _, n := range exe.NodeExecutions {
		if n == nil {
			continue
		}
		ne := CozeNodeExec{
			NodeID:     n.NodeID,
			NodeType:   string(n.NodeType),
			Status:     nodeExecStatusStr(n.Status),
			DurationMs: n.Duration.Milliseconds(),
		}
		if n.Input != nil {
			ne.Input = *n.Input
		}
		if n.Output != nil {
			ne.Output = *n.Output
		}
		if n.ErrorInfo != nil {
			ne.FailReason = *n.ErrorInfo
		}
		list = append(list, ne)
	}
	r.NodeExecs = list
}

// nodeExecStatusStr 节点执行状态枚举 → 前端字符串。
func nodeExecStatusStr(s entity.NodeExecuteStatus) string {
	switch workflow.NodeExeStatus(s) {
	case workflow.NodeExeStatus_Waiting:
		return "waiting"
	case workflow.NodeExeStatus_Running:
		return "running"
	case workflow.NodeExeStatus_Success:
		return "success"
	case workflow.NodeExeStatus_Fail:
		return "fail"
	default:
		return "unknown"
	}
}

// CozeDeleteWorkflow 删除工作流（软删；id 为字符串形态）。
func (a *App) CozeDeleteWorkflow(id string) error {
	c, err := a.cozeEnsure()
	if err != nil {
		return err
	}
	wfID, err := cozeParseID(id)
	if err != nil {
		return err
	}
	_, err = c.Workflow.DomainSVC.Delete(context.Background(), &vo.DeletePolicy{ID: &wfID})
	return err
}

// CozeListDraftJSON 调试运行输入表单数据源：解析草稿输入参数定义返回原始 JSON。
func (a *App) CozeDraftParams(id string) (inputJSON, outputJSON string, err error) {
	d, err := a.CozeGetWorkflow(id)
	if err != nil {
		return "", "", err
	}
	return d.InputJSON, d.OutputJSON, nil
}

// ---- 内部工具 ----

// cozeParseID 前端传入的 ID 字符串 → int64（传输层字符串化防 JS 精度丢失）。
func cozeParseID(id string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("工作流 ID 无效: %q", id)
	}
	return v, nil
}

// execStatusStr 执行状态转字符串（前端渲染）。
func execStatusStr(s entity.WorkflowExecuteStatus) string {
	switch s {
	case entity.WorkflowSuccess:
		return "success"
	case entity.WorkflowFailed:
		return "failed"
	case entity.WorkflowCancel:
		return "cancelled"
	case entity.WorkflowRunning:
		return "running"
	case entity.WorkflowInterrupted:
		return "interrupted"
	default:
		return fmt.Sprintf("unknown(%d)", int32(s))
	}
}

// bumpVersion 语义化版本号 +0.0.1 自增（major.minor.patch → patch+1）。
func bumpVersion(v string) string {
	var a, b, c int
	if _, err := fmt.Sscanf(v, "%d.%d.%d", &a, &b, &c); err != nil {
		return "0.0.1"
	}
	return fmt.Sprintf("%d.%d.%d", a, b, c+1)
}

// ptrI64 int64 指针便捷构造。
func ptrI64(v int64) *int64 { return &v }

// parseCozeID 字符串化 ID → int64（空串/非法返回 0；支持负数外部数据源 ID）。
func parseCozeID(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// modeWorkflow WorkflowMode_Workflow（0）指针（api/model/workflow 具名类型）。
func modeWorkflow() *workflow.WorkflowMode {
	m := workflow.WorkflowMode_Workflow
	return &m
}
