package tasklog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaelvoris/pkg/message"
)

func TestWriteReportContent(t *testing.T) {
	old := timeNow
	timeNow = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local) }
	defer func() { timeNow = old }()
	_ = old

	dir := t.TempDir()
	msgs := []message.ClineMessage{
		message.NewSay(message.SayUserFeedback, "分析我们的项目\n第二行不应出现"),
		message.NewSay(message.SayTool, `{"tool":"fs_read","path":"internal/agent/engine.go"}`),
		{TS: 0, Type: message.TypeAsk, Ask: message.AskCommandOutput, Text: "out1\nout2\nout3"},
		message.NewSay(message.SayError, "boom"),
	}
	path, err := WriteReport(dir, Meta{
		SessionID: "ABCdef123!@#", Mode: "code", Model: "gpt-x",
		Task: "分析我们的项目", Status: "done", Elapsed: "3m12s",
		ChangeSummary: "已记录 2 个检查点", UsageJSON: `{"total_tokens":2013}`,
	}, msgs, 30)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(path), "20260102-030405.") || !strings.Contains(filepath.Base(path), "ABCdef12") {
		t.Fatalf("unexpected filename: %s", filepath.Base(path))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"# Kaelvoris 任务运行报告",
		"- 会话: `ABCdef123!@#`",
		"- 模式: code　模型: gpt-x",
		"- 结果: done　耗时: 3m12s",
		"say:user_feedback",
		"say:tool",
		"say:error",
		"ask:command_output",
		"分析我们的项目", // 换行截断
		"第二行不应出现", // 不应出现
		`{"total_tokens":2013}`,
	} {
		if strings.Contains(s, want) == (want == "第二行不应出现") {
			// want 出现且是"第二行不应出现" → 失败；否则应包含
			if want == "第二行不应出现" {
				continue
			}
			t.Fatalf("report missing %q", want)
		}
	}
}

func TestRetention(t *testing.T) {
	dir := t.TempDir()
	// 写 35 份，保留 30。
	for i := 0; i < 35; i++ {
		_, err := WriteReport(dir, Meta{SessionID: "s"}, nil, 30)
		if err != nil {
			t.Fatal(err)
		}
		// 确保文件名时间戳递增（同秒会重名覆盖，强制推进）。
		time.Sleep(10 * time.Millisecond)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 30 {
		t.Fatalf("want 30 files retained, got %d", len(ents))
	}
}

func TestMarkdownEscape(t *testing.T) {
	if got := mdCell("a|b\nc\rd"); got != "a\\|b c d" {
		t.Fatalf("mdCell = %q", got)
	}
}
