// Package tools 工具注册中心（移植自 Zoo-Code src/core/tools 的工具协议）。
//
// 每个工具实现 Tool 接口，注册到 Registry；Registry 统一导出为
// LLM function-calling schema（llm.ToolDef）并按名分发执行。
package tools

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Tool 工具接口（对应 Zoo-Code BaseTool）。
type Tool interface {
	// Name 工具名（LLM 可见的 function name）。
	Name() string
	// Description 工具描述（LLM 可见）。
	Description() string
	// Parameters 参数 JSONSchema。
	Parameters() map[string]interface{}
	// Execute 执行工具。args 为 LLM 给出的 JSON 参数串。
	Execute(ctx context.Context, args string) (result string, err error)
}

// Registry 工具注册中心。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// defsCache ToolDefs 结果缓存（每次 LLM 请求都会调用 ToolDefs；注册表
	// 内容仅在启动期与 MCP 插件热插拔时变化，缓存消除稳态下的重复构建）。
	defsCache []llmToolDef
}

// NewRegistry 创建注册中心。
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register 注册工具；重名返回错误。
func (r *Registry) Register(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[t.Name()]; dup {
		return fmt.Errorf("tool %q already registered", t.Name())
	}
	r.tools[t.Name()] = t
	r.defsCache = nil // 注册表变化：失效缓存（MCP 插件热插拔路径）
	return nil
}

// Get 按名获取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names 已注册工具名（稳定排序）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ToolDefs 导出为 LLM 工具定义列表（OpenAI function 格式）。
// 带缓存：注册表未变化时直接返回上次构建结果（稳态零重建）。
func (r *Registry) ToolDefs() []llmToolDef {
	r.mu.RLock()
	if r.defsCache != nil {
		defer r.mu.RUnlock()
		return r.defsCache
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	// 双重检查：RLock 释放到 Lock 获取之间可能有并发 Register。
	if r.defsCache != nil {
		return r.defsCache
	}
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)

	defs := make([]llmToolDef, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		defs = append(defs, llmToolDef{
			Type: "function",
			Function: llmToolFunc{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	r.defsCache = defs
	return defs
}

// llmToolDef 避免直接依赖 internal/llm 的别名定义。
type llmToolDef struct {
	Type     string      `json:"type"`
	Function llmToolFunc `json:"function"`
}

type llmToolFunc struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ExecuteResult 工具执行结果（供 Agent 引擎回填消息）。
type ExecuteResult struct {
	Content string
}
