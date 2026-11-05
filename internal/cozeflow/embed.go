// embed.go：静态资源嵌入。
//
// Kaelvoris 融合改造（2026-10）：coze 独立部署时 conf/ 树经 Dockerfile 拷贝到
// /app/resources/conf/（运行时按 CWD 相对路径读取）；融合形态下 Kaelvoris 是
// 单二进制桌面应用，故将资源树经 go:embed 打进二进制，bootstrap 时释放到
// ~/.kaelvoris/coze/ 并设置 COZE_RESOURCE_DIR 环境变量（见 fileutil/fileutil.go
// 的 GetWorkingDirectory 补丁）。
//
// 资源清单（源自 coze-studio/backend/conf，Apache 2.0）：
//   - conf/model/model_meta.json        模型元数据（参数面板/能力声明）
//   - conf/model/agnes-2.5-flash.yaml   内置模型（OpenAI 协议，复用 Kaelvoris 桌面端 API 中转）
//   - conf/workflow/config.yaml         workflow 域配置（code 节点第三方模块白名单）
//   - conf/prompt/*.json                nl2sql / messages2query 的 Jinja2 提示词模板（知识库域）
//   - conf/plugin/...                   内置插件产品定义（Phase 1 未启用，随树嵌入备用）
//   - ddl/coze_schema.sql               MySQL 全量建表语句（经 sqlite.go 运行时转换）
package cozeflow

import "embed"

// resourcesFS 嵌入 coze conf 资源树（model/workflow/prompt/plugin）。
//
//go:embed resources/conf
var resourcesFS embed.FS

// ddlFS 嵌入 MySQL DDL（sqlite.go 运行时转换为 SQLite 方言）。
//
//go:embed ddl/coze_schema.sql
var ddlFS embed.FS
