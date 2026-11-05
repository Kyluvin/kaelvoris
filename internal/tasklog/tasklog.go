// Package tasklog 任务运行日志导出（B2，对标 OpenHands Event Stream 回放 / Goose
// 会话诊断导出的同类能力）。
//
// 任务结束后把会话消息流（ClineMessage 序列）序列化为单文件 Markdown 运行报告，
// 落 ~/.kaelvoris/tasklogs/，滚动保留最近 N 份。纯本地序列化，无 LLM 依赖；
// 失败由调用方静默降级（不影响主流程）。
package tasklog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kaelvoris/pkg/message"
)

// timeNow 可测试时间源（默认 time.Now）。
var timeNow = time.Now

// Meta 报告头部元信息。
type Meta struct {
	SessionID     string
	Mode          string
	Model         string
	Task          string
	Status        string // done | failed | stopped
	Elapsed       string // 人类可读耗时（如 3m12s）
	ErrText       string // 错误（空 = 无）
	ChangeSummary string // 文件变更摘要（turn diff / checkpoint 摘要，可空）
	UsageJSON     string // token 用量 JSON（usage.Counter 快照，可空）
}

// WriteReport 生成 Markdown 运行报告并执行滚动保留。返回文件路径。
func WriteReport(dir string, meta Meta, msgs []message.ClineMessage, maxFiles int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s.md", timeNow().Format("20060102-150405.000"), shortID(meta.SessionID))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(render(meta, msgs)), 0o644); err != nil {
		return "", err
	}
	enforceRetention(dir, maxFiles)
	return path, nil
}

// render 渲染 Markdown 报告正文。
func render(meta Meta, msgs []message.ClineMessage) string {
	var b strings.Builder
	b.WriteString("# Kaelvoris 任务运行报告\n\n")
	fmt.Fprintf(&b, "- 会话: `%s`\n", meta.SessionID)
	fmt.Fprintf(&b, "- 模式: %s　模型: %s\n", orDash(meta.Mode), orDash(meta.Model))
	fmt.Fprintf(&b, "- 任务: %s\n", firstLine(meta.Task, 200))
	fmt.Fprintf(&b, "- 结果: %s　耗时: %s\n", orDash(meta.Status), orDash(meta.Elapsed))
	if meta.ErrText != "" {
		fmt.Fprintf(&b, "- 错误: %s\n", firstLine(meta.ErrText, 300))
	} else {
		b.WriteString("- 错误: 无\n")
	}
	if meta.ChangeSummary != "" {
		fmt.Fprintf(&b, "- 文件变更: %s\n", firstLine(meta.ChangeSummary, 500))
	}
	b.WriteString("\n## 时间线\n\n")
	b.WriteString("| # | 类型 | 内容 |\n|---|------|------|\n")
	i := 0
	for _, m := range msgs {
		i++
		kind, text := describe(m)
		if text == "" {
			text = "—"
		}
		fmt.Fprintf(&b, "| %d | %s | %s |\n", i, kind, mdCell(text))
	}
	if i == 0 {
		b.WriteString("| 1 | — | （无消息） |\n")
	}
	if meta.UsageJSON != "" {
		fmt.Fprintf(&b, "\n## Token 用量\n\n%s\n", meta.UsageJSON)
	}
	return b.String()
}

// describe 把单条 ClineMessage 转为（类型标签, 摘要文本）。
func describe(m message.ClineMessage) (string, string) {
	if m.Type == message.TypeAsk {
		return "ask:" + string(m.Ask), firstLine(m.Text, 300)
	}
	txt := m.Text
	limit := 300
	switch m.Say {
	case message.SayTool:
		limit = 300
	case message.SayReasoning, message.SayAPIReqStarted, message.SayAPIReqFinished,
		message.SayAIContext, message.SayUsage:
		limit = 120 // 低信息密度载荷只留头部
	}
	return "say:" + string(m.Say), firstLine(txt, limit)
}

// enforceRetention 滚动保留最新 maxFiles 份报告。
func enforceRetention(dir string, maxFiles int) {
	if maxFiles <= 0 {
		maxFiles = 30
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= maxFiles {
		return
	}
	sort.Strings(names) // 文件名含时间戳前缀 = 字典序即时间序
	for _, n := range names[:len(names)-maxFiles] {
		_ = os.Remove(filepath.Join(dir, n))
	}
}

// ---- 小工具 ----

func shortID(id string) string {
	id = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, id)
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		id = "sess"
	}
	return id
}

func firstLine(s string, maxRune int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > maxRune {
		return string(r[:maxRune]) + "…"
	}
	return s
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// mdCell Markdown 表格单元格转义（竖线与换行）。
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
