// Package promptconfig 系统提示词模板配置（用户需求 2026-11：提示词配置化）。
//
// 背景：提示词在多个位置硬编码（internal/agent/prompts.go 的 SystemPrompt 组装、
// internal/condense 摘要提示词、task_summary/selflearn/hourlyreview/roadmap_panel/
// cron_panel 的各功能提示词）。用户要求「所有设计提示词的地方，至此通过配置进行
// 修改」——本包提供统一存储与读取：
//
//	存储：~/.kaelvoris/prompts.json（与 desktop.json 同层；损坏/缺失按默认兜底）
//	读取：Get 按段名返回配置文本（空 = 使用内置默认，不注入覆盖）
//	生效：任务开始时热读（与 desktop.json 一致，下一任务生效）
//
// 与 Codex（codex-rs/prompts + protocol/base_instructions/default.md +
// openai_models ModelMessages catalog 覆盖）的对应：Codex 以「模型指令模板 +
// 基础指令 + 每模型 catalog 覆盖」分层，本包以「段配置 + 内置默认」分层，
// 配置文本为空时回退内置默认（对齐 catalog 缺失时使用 bundled default 语义）。
package promptconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Config 提示词模板配置（JSON 持久化结构）。
// 键 = 段名（见 templates 内置默认表），值 = 用户配置文本（空 = 使用内置默认）。
type Config struct {
	// System 系统提示词整段覆盖（空 = 使用内置 SystemPrompt 组装）。
	// 非空时 SystemPrompt 组装逻辑仍执行，但用户文本追加在末尾（USER-CUSTOM
	// PROMPT OVERRIDE 段），优先级最高——幂等叠加，不破坏既有段落结构。
	System string `json:"system,omitempty"`
	// Summarize 摘要压缩提示词（internal/condense summarizePrompt）。
	// 非空时替换内置摘要指令。
	Summarize string `json:"summarize,omitempty"`
	// TaskSummary 会话总结提示词（taskSummaryPrompt）。
	TaskSummary string `json:"taskSummary,omitempty"`
	// SelfReview 自学习复盘提示词（selfReviewPrompt）。
	SelfReview string `json:"selfReview,omitempty"`
	// HourlyReview 每小时复盘提示词（hourlyReviewPrompt）。
	HourlyReview string `json:"hourlyReview,omitempty"`
	// RoadmapReview 规划板 AI 审核提示词（roadmapReviewPrompt 的补充判据；
	// 注意：roadmap 的 extraPrompt 已有独立配置项 autoDev.reviewPrompt）。
	RoadmapReview string `json:"roadmapReview,omitempty"`
	// SelfCheckTask 定时自检任务提示词（selfReviewTaskPrompt）。
	SelfCheckTask string `json:"selfCheckTask,omitempty"`
	// RoadmapSystem 自主开发会话系统提示词（roadmapSystemPromptDefault，
	// launchDevelop 每次拉起开发会话时前置注入；非空覆盖内置默认）。
	RoadmapSystem string `json:"roadmapSystem,omitempty"`
	// AutonomousDev 自主开发守则段（internal/agent prompts.go 的 AUTONOMOUS
	// DEVELOPMENT 段；非空时整段替换内置默认，空 = 用内置默认）。
	AutonomousDev string `json:"autonomousDev,omitempty"`
}

// Store 提示词配置存储（内存缓存 + 文件持久化）。
type Store struct {
	mu   sync.RWMutex
	cfg  Config
	path string
}

// DefaultPath 配置文件路径（~/.kaelvoris/prompts.json）。
func DefaultPath(home string) string {
	return filepath.Join(home, ".kaelvoris", "prompts.json")
}

// New 创建存储并加载磁盘配置（不存在/损坏按空配置兜底 = 全部使用内置默认）。
func New(path string) *Store {
	s := &Store{path: path}
	if b, err := os.ReadFile(path); err == nil {
		var c Config
		if json.Unmarshal(b, &c) == nil {
			s.cfg = c
		}
	}
	return s
}

// Get 返回指定段名的用户配置文本（空 = 使用内置默认）。
func (s *Store) Get(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch key {
	case "system":
		return s.cfg.System
	case "summarize":
		return s.cfg.Summarize
	case "taskSummary":
		return s.cfg.TaskSummary
	case "selfReview":
		return s.cfg.SelfReview
	case "hourlyReview":
		return s.cfg.HourlyReview
	case "roadmapReview":
		return s.cfg.RoadmapReview
	case "selfCheckTask":
		return s.cfg.SelfCheckTask
	case "roadmapSystem":
		return s.cfg.RoadmapSystem
	case "autonomousDev":
		return s.cfg.AutonomousDev
	}
	return ""
}

// All 返回完整配置（设置面板展示/编辑用）。
func (s *Store) All() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Save 保存完整配置并落盘（原子写：先写 tmp 再 rename）。
func (s *Store) Save(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.cfg = c
	return nil
}
