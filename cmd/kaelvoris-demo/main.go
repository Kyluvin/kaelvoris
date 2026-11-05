// kaelvoris-demo：内置 mock LLM 服务器，端到端演示 P2 编排能力。
//
// 演示场景（orchestrator → new_task → code 子任务 → 结果回传 → 父任务完成）：
//
//	第 1 轮  orchestrator 调用 update_todo_list 建立任务清单
//	第 2 轮  orchestrator 调用 new_task 委派子任务（code 模式）
//	第 3 轮  子任务（code）调用 fs_write 写文件
//	第 4 轮  子任务调用 attempt_completion（子任务结果）
//	第 5 轮  父任务收到子任务结果，更新清单并 attempt_completion 汇总
//
// 运行：go run ./cmd/kaelvoris-demo
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"kaelvoris/internal/agent"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/modes"
	"kaelvoris/internal/skills"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// chunkTool 构造工具调用 chunk。
func chunkTool(id, name, args string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]interface{}{
				"tool_calls": []map[string]interface{}{
					{"index": 0, "id": id, "type": "function",
						"function": map[string]interface{}{"name": name, "arguments": args}},
				},
			}},
		},
	}
}

// chunkText 构造文本 chunk。
func chunkText(text string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]interface{}{"content": text}},
		},
	}
}

// writeSSE 输出一组 chunk 为 SSE 响应。
func writeSSE(w http.ResponseWriter, chunks []map[string]interface{}) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func main() {
	ws, err := os.MkdirTemp("", "kaelvoris-demo-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(ws)
	fmt.Printf("workspace: %s\n\n", ws)

	// ---- 演示脚本：mock LLM 响应序列（每轮一组 chunk）----
	script := [][]map[string]interface{}{
		{ // 父任务 1：建立任务清单
			chunkText("I will break this into subtasks."),
			chunkTool("r1", "update_todo_list", `{"todos":[
				{"content":"Delegate file creation to code mode","status":"in_progress"},
				{"content":"Summarize results","status":"pending"}]}`),
		},
		{ // 父任务 2：new_task 委派（单独一轮，验证隔离规则）
			chunkTool("r2", "new_task", `{"mode":"code","message":"Create demo.txt containing delegated content","todos":[
				{"content":"Create demo.txt","status":"pending"}]}`),
		},
		{ // 子任务 1：code 模式写文件
			chunkTool("s1", "fs_write", `{"path":"demo.txt","content":"created by delegated subtask"}`),
		},
		{ // 子任务 2：子任务完成
			chunkTool("s2", "attempt_completion", `{"result":"demo.txt created with delegated content"}`),
		},
		{ // 父任务 3：更新清单
			chunkTool("r3", "update_todo_list", `{"todos":[
				{"content":"Delegate file creation to code mode","status":"completed"},
				{"content":"Summarize results","status":"completed"}]}`),
		},
		{ // 父任务 4：汇总完成
			chunkTool("r4", "attempt_completion",
				`{"result":"Orchestration demo complete: subtask created demo.txt via code-mode delegation."}`),
		},
	}

	// 兜底响应：脚本耗尽后返回 attempt_completion（避免 mock 重发导致死循环）。
	fallback := []map[string]interface{}{
		chunkTool("fallback", "attempt_completion", `{"result":"demo finished (fallback)"}`),
	}

	req := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 打印每轮请求的模式上下文（验证模式/子任务提示词注入）。
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if b, err := io.ReadAll(r.Body); err == nil {
			if json.Unmarshal(bytes.TrimSpace(b), &body) == nil && len(body.Messages) > 0 {
				sys := body.Messages[0].Content
				kind := "parent"
				if strings.Contains(sys, "SUBTASK CONTEXT") {
					kind = "SUBTASK"
				}
				mode := "(default)"
				for _, line := range strings.Split(sys, "\n") {
					if strings.HasPrefix(line, "CURRENT MODE: ") {
						mode = strings.TrimPrefix(line, "CURRENT MODE: ")
					}
				}
				fmt.Printf("── LLM request #%d (mode=%s, %s)\n", req+1, mode, kind)
			}
		}
		idx := req
		resp := script[idx]
		if req >= len(script) {
			resp = fallback
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range resp {
			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		req++
	}))
	defer ts.Close()

	// ---- 组装内核（与 CLI 相同装配）----
	modeMgr := modes.NewManager()
	_ = modeMgr.LoadProject(ws)
	home, _ := os.UserHomeDir()
	skillMgr := skills.NewManager(home, ws)
	todoStore := tools.NewTodoStore()

	registry := tools.NewRegistry()
	must(registry.Register(&tools.ReadFileTool{Workspace: ws}))
	must(registry.Register(&tools.WriteFileTool{Workspace: ws}))
	must(registry.Register(&tools.ApplyDiffTool{Workspace: ws}))
	must(registry.Register(&tools.ListFilesTool{Workspace: ws}))
	must(registry.Register(&tools.SearchFilesTool{Workspace: ws}))
	must(registry.Register(&tools.AskFollowupTool{}))
	must(registry.Register(&tools.AttemptCompletionTool{}))
	must(registry.Register(&tools.UpdateTodoListTool{Store: todoStore}))
	// 无 SubtaskRunner（demo 串行）：new_task 保持 mustBeLastTool 串行语义。
	must(registry.Register(&tools.NewTaskTool{}))
	must(registry.Register(&tools.SwitchModeTool{}))
	must(registry.Register(&tools.DesignSystemTool{}))
	// open-design 全量融合：design_template（design-templates 模板浏览/加载）。
	must(registry.Register(&tools.DesignTemplateTool{}))
	// 诊断三件套（mock 演示不主动触发，仅保持工具集与真实宿主一致）。
	must(registry.Register(&tools.ProcessListTool{}))
	must(registry.Register(&tools.FileLockerTool{}))
	must(registry.Register(&tools.HTTPRequestTool{}))
	must(registry.Register(&tools.SkillTool{
		Loader: func(name string) (string, error) {
			c, err := skillMgr.Load(name)
			if err != nil {
				return "", err
			}
			return c.Body, nil
		},
		Names: skillMgr.Names,
		// 技能包内文件读取（内置技能 references/ 二段式加载）。
		ReadFile: skillMgr.ReadFile,
	}))

	client := llm.NewClient(ts.URL, "demo-key", "mock-model")
	engine := agent.New(client, registry, nil)
	engine.Modes = modeMgr
	engine.Todos = todoStore
	engine.MaxSubtaskDepth = 3
	engine.AutoApprove = func(message.ClineMessage) bool { return true }
	engine.AutoDecide = true // 演示端全自动：ask_followup 不阻塞，模型自主决策。

	// say 渲染：关键事件展示。
	say := func(m message.ClineMessage) {
		switch {
		case m.Say == message.SayTool:
			var info message.ToolCallInfo
			_ = json.Unmarshal([]byte(m.Text), &info)
			fmt.Printf("   [tool] %s %s%s\n", info.Tool, info.Path, info.Cmd)
		case m.Say == message.SaySubtaskStarted:
			fmt.Printf("   >>> %s\n", m.Text)
		case m.Say == message.SaySubtaskFinished:
			fmt.Printf("   <<< subtask result: %s\n", m.Text)
		case m.Type == message.TypeAsk && m.Ask == message.AskCompletionResult:
			fmt.Printf("   [completion] %s\n", m.Text)
		}
	}

	result, err := engine.RunInMode(context.Background(), "orchestrator",
		"Create demo.txt via a delegated code-mode subtask.", false, 0, say, nil)
	fmt.Println()
	if err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}

	// 验证子任务产物真实落盘。
	b, readErr := os.ReadFile(filepath.Join(ws, "demo.txt"))
	fmt.Printf("\n=== RESULT ===\n%s\n", result)
	if readErr != nil {
		fmt.Printf("=== subtask artifact demo.txt read FAILED: %v ===\n", readErr)
	} else {
		fmt.Printf("=== subtask artifact demo.txt: %q ===\n", string(b))
	}
	fmt.Printf("=== messages recorded: %d ===\n", len(engine.Messages))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
