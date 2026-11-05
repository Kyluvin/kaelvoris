// aux_model.go —— 辅助模型分流（E2，第四批，对标 Plandex summarizer 角色 /
// claude-task-master research model）。
//
// 问题：condense 摘要压缩、selflearn 复盘、taskSummary 总结等辅助 LLM 调用
// 全部走主模型——高频摘要时主模型成本被无谓消耗。对标 Plandex 把
// summarizer 设为独立便宜角色：desktop.json 配置 auxBaseURL/auxAPIKey/
// auxModel 三项齐备时，辅助环节切换到辅助（便宜）模型；任一缺失回落主模型
// （零破坏：不配置 = 行为与现在完全一致）。
//
// 边界：辅助调用失败不自动回落主模型重试（保持各调用点既有失败静默/降级
// 语义，仅日志说明所用模型便于排查）。hourlyreview 已自带独立模型配置，
// 不在本文件接线范围。
package desktop

import (
	"log/slog"

	"kaelvoris/internal/llm"
)

// auxClient 返回辅助环节（摘要压缩/复盘/总结）使用的 LLM 客户端：
// 辅助配置族三项齐备时用辅助模型，否则回落主模型（primary 为 nil 时按
// 当前全局配置构造）。对齐 Plandex summarizer 角色语义——辅助环节用
// 便宜小模型，主模型留给主循环。
func (a *App) auxClient(primary *llm.Client) *llm.Client {
	cfg := a.GetConfig()
	if cfg.AuxBaseURL != "" && cfg.AuxAPIKey != "" && cfg.AuxModel != "" {
		slog.Info("辅助模型分流启用（E2）", "auxModel", cfg.AuxModel)
		return llm.NewClient(cfg.AuxBaseURL, cfg.AuxAPIKey, cfg.AuxModel)
	}
	if primary != nil {
		return primary
	}
	return llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
}

// fallbackClient 返回主模型失败降级备用客户端（E3，对标 Plandex
// errorFallback）：fallbackBaseURL/fallbackAPIKey/fallbackModel 三项齐备
// 且与主模型（baseURL+model）不完全相同时才启用——相同 = 降级无意义，
// 返回 nil（行为与现状一致）。主模型 client 为 nil（测试场景）时仅按
// 配置齐备判定。
func (a *App) fallbackClient(primary *llm.Client) *llm.Client {
	cfg := a.GetConfig()
	if cfg.FallbackBaseURL == "" || cfg.FallbackAPIKey == "" || cfg.FallbackModel == "" {
		return nil
	}
	if primary != nil && primary.BaseURL == cfg.FallbackBaseURL && primary.Model == cfg.FallbackModel {
		return nil // 与主模型同配置，降级无意义
	}
	return llm.NewClient(cfg.FallbackBaseURL, cfg.FallbackAPIKey, cfg.FallbackModel)
}
