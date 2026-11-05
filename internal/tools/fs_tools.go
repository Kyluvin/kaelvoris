// Package tools 工具集：fs_read / fs_write / fs_apply_diff / fs_list / fs_search。
//
// 移植自 Zoo-Code src/core/tools/ 的 ReadFileTool、WriteToFileTool、
// ApplyDiffTool、ListFilesTool、SearchFilesTool，安全约束保持一致：
// 路径限定工作区内 + .rooignore 过滤（P1 简化为跳过隐藏目录）。
package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"kaelvoris/internal/sysproc"
)

// ---------- fs_read ----------

type ReadFileTool struct{ Workspace string }

func (t *ReadFileTool) Name() string { return "fs_read" }

// ReadDefaultLines / ReadMaxLines fs_read 分页默认与上限。
// 对齐 Zoo-Code read_file.ts DEFAULT_LINE_LIMIT = 2000（每次最多返回 2000
// 行）。修复性能根因：旧值默认 100 行/次、上限 1000 行/次，读一个 2000 行
// 文件要发 20 次请求——每次都是一轮完整 LLM 请求且历史越来越长，任务耗时
// 平方级放大。413 防护不受影响：引擎 clampToolResult（单条 40k 字符钳制）
// + 三层上下文压缩兜底，分页参数只在文件超出单次预算时才真正需要。
const (
	ReadDefaultLines = 2000
	ReadMaxLines     = 2000
)

// readCharBudget fs_read 单次输出的字符预算上限（≈36KB）。长行文件即使
// 行数不超 2000，字节量也可能超出引擎 clampToolResult 的 40k 字符钳制——
// 被引擎硬截断后「more lines remain」续读提示丢失，模型只能盲猜 offset
// 反复分页（2026-10-01 会话复盘：单文件最多连续读取 18 次、单会话重复
// 读同一文件 71 次）。在工具内按预算提前停住并给出精确续读 offset，
// 一次翻页即可精准续读。留 4k 余量给 header 与截断提示。
const readCharBudget = 36000

// readWholeGuardBytes G3 整读护栏阈值（字节）。超过该体积的文件在「从头
// 整读」（未带 offset）时返回标题/声明索引而非全文——大文档多数任务只需
// 其中一节，整读一次性烧光上下文。固定值不配置（一个护栏不该带配置面）。
const readWholeGuardBytes = 40000

func (t *ReadFileTool) Description() string {
	return "Read the contents of a file at the given path (relative to workspace root). " +
		"Returns contents with line numbers. By default, returns up to 2000 lines per call. " +
		"Use offset+limit to page through very large files instead of reading them whole."
}

func (t *ReadFileTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"path":   strProp("File path relative to workspace root"),
		"offset": numProp("1-based start line (default 1)"),
		"limit":  numProp("Lines to read per call, 1-2000 (default 2000)"),
	}, []string{"path"})
}

func (t *ReadFileTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Path   string          `json:"path"`
		Offset json.RawMessage `json:"offset"`
		Limit  json.RawMessage `json:"limit"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	offset, err := parseIntArg(p.Offset, 1, "offset")
	if err != nil {
		return "", fmt.Errorf("invalid offset: %w", err)
	}
	limit, err := parseIntArg(p.Limit, ReadDefaultLines, "limit")
	if err != nil {
		return "", fmt.Errorf("invalid limit: %w", err)
	}
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 {
		limit = ReadDefaultLines
	}
	if limit > ReadMaxLines {
		limit = ReadMaxLines
	}
	abs, err := resolveInWorkspace(t.Workspace, p.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		// 错误分类提示（2026-10-04 复盘：目录路径透传 Windows "Incorrect
		// function"，不存在路径只透传 OS 文本——模型无从判断原因，
		// 单会话盲试 fs_read 达 30~112 次）。给下一步动作指引。
		if st, serr := os.Stat(abs); serr == nil && st.IsDir() {
			return "", fmt.Errorf("%s is a directory, not a file: use fs_list to browse it", p.Path)
		}
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s (verify the path with fs_list on its parent directory)", p.Path)
		}
		return "", err
	}
	// 写冲突防护记录（2026-10-05 复盘）：fs_read 触达即登记文件 mtime，
	// 供 fs_write/fs_apply_diff 写前比对（读取后被他人改写 → 拒绝盲写，
	// 从源头消除多会话并发编辑互踩）。
	if fi, serr := os.Stat(abs); serr == nil {
		readGuardRecord(abs, fi.ModTime())
	}
	// G3 整读护栏（CCGS validate-read 对标）：超阈值文件的「从头整读」
	// 不返回全文，改为标题/声明索引 + 按节读取指引。带 offset（含 offset=1
	// 显式分页）不受影响；索引本身也登记读触达（上方已统一登记）。
	if offset == 1 && len(data) > readWholeGuardBytes {
		return readWholeGuard(p.Path, data), nil
	}
	if isBinary(data) {
		return "(binary file, content omitted)", nil
	}
	// 行号标注（对齐 Zoo-Code read_file 输出形态）+ 分页：
	// 只保留 [offset, offset+limit) 区间的行，避免单次读取撑爆上下文
	//（413 Request Entity Too Large 防护，用户可调上限）。
	// 字符预算感知（readCharBudget）：行数未超但字节数超预算时提前停住，
	// header 给出精确续读 offset，替代「被引擎截断后盲猜 offset」的低效模式。
	sc := bufio.NewScanner(bytesReader(data))
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	var b strings.Builder
	b.Grow(4096)
	line := 0
	start := offset
	end := offset + limit // 半开区间
	budgetHit := false
	nextRead := 0  // 下一个未读行号（header 提示用）
	lastShown := 0 // 最后一个已输出的行号
	for sc.Scan() {
		line++
		if budgetHit {
			continue // 命中预算后仅继续计数，保证 total 行数准确
		}
		if line < start || line >= end {
			continue
		}
		l := sc.Text()
		// 预算检查：当前行放不进剩余预算则提前停（本行不计入输出，
		// 续读 offset 指向本行，无重叠无遗漏）。
		if b.Len()+len(l)+8 > readCharBudget && lastShown >= start {
			budgetHit = true
			nextRead = line
			continue
		}
		// 兼容单行超长文件（如压缩后的 main.js）：首页首行超预算时强制
		// 截断该行输出，避免「什么都读不到 → 盲猜 offset」死循环。
		if len(l) > readCharBudget {
			l = truncUTF8(l, readCharBudget) + " ... (single line truncated)"
		}
		fmt.Fprintf(&b, "%6d|\t%s\n", line, l)
		lastShown = line
	}
	total := line
	if total == 0 {
		return "(empty file)", nil
	}
	if start > total {
		return fmt.Sprintf("offset %d beyond end of file (%d lines total; use offset <= %d)", start, total, total), nil
	}
	if lastShown == 0 {
		lastShown = start - 1 // 区间内无行可读（防御，理论上不可达）
	}
	header := fmt.Sprintf("[file %s | %d lines total | showing %d-%d",
		p.Path, total, start, lastShown)
	// 续读提示：预算命中或行数未读尽时给出精确 offset（无重叠无遗漏）。
	if budgetHit {
		header += fmt.Sprintf(" | more lines remain: call again with offset=%d (char budget reached; this page is smaller than requested limit)", nextRead)
	} else if end <= total {
		header += fmt.Sprintf(" | more lines remain: call again with offset=%d", end)
	}
	header += "]\n"
	return header + b.String(), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// readWholeGuard 构建超阈值文件的整读护栏响应（G3 对标 CCGS
// validate-read：>40KB 整读拒绝，返回标题+行号清单让模型按节读）。
// 索引策略按文件类型分流：markdown 提取标题层级；go/py 提取顶层声明
//（大仓这类文件远多于 markdown，仅标题索引对代码无用）；其他类型
// 退化为开头预览（仍足以定位分页点）。只报告观察，不下判词。
// 输出体量受 readCharBudget 约束（索引自身超长则截断，尾部给裁剪说明）。
func readWholeGuard(path string, data []byte) string {
	ext := filepath.Ext(path)
	sc := bufio.NewScanner(bytesReader(data))
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	line := 0
	var b strings.Builder
	b.Grow(2048)
	fmt.Fprintf(&b, "[file %s | %d bytes | whole-read guard (G3): file exceeds %d bytes; full content not returned - use offset/limit to read one section at a time]\n", path, len(data), readWholeGuardBytes)
	switch ext {
	case ".md", ".markdown":
		b.WriteString("Heading index (line: heading) - call fs_read with offset=<line> to read that section:\n")
		for sc.Scan() {
			line++
			t := sc.Text()
			if strings.HasPrefix(t, "#") {
				fmt.Fprintf(&b, "%6d| %s\n", line, strings.TrimRight(t, " \t"))
			}
		}
	case ".go":
		b.WriteString("Top-level declaration index (line: declaration) - call fs_read with offset=<line> to read that section:\n")
		for sc.Scan() {
			line++
			t := sc.Text()
			// 顶层声明 = 行首关键字（无缩进）。缩进声明的非顶层行不索引，
			// 避免结构体内部方法/字段把索引榜爆。
			if isTopLevelDecl(t) {
				fmt.Fprintf(&b, "%6d| %s\n", line, truncUTF8(strings.TrimRight(t, " \t"), 120))
			}
		}
	case ".py":
		b.WriteString("Top-level definition index (line: definition) - call fs_read with offset=<line> to read that section:\n")
		for sc.Scan() {
			line++
			t := sc.Text()
			if strings.HasPrefix(t, "def ") || strings.HasPrefix(t, "class ") ||
				strings.HasPrefix(t, "async def ") || strings.HasPrefix(t, "import ") ||
				strings.HasPrefix(t, "from ") {
				fmt.Fprintf(&b, "%6d| %s\n", line, truncUTF8(strings.TrimRight(t, " \t"), 120))
			}
		}
	default:
		// 通用退路：开头 60 行预览（截断到预算），定位文件开头形态后按节读。
		b.WriteString("Preview (first 60 lines) - call fs_read with offset/limit to continue:\n")
		for sc.Scan() && line < 60 {
			line++
			fmt.Fprintf(&b, "%6d| %s\n", line, sc.Text())
		}
	}
	out := b.String()
	if len(out) > readCharBudget {
		out = truncUTF8(out, readCharBudget) + "\n... (index truncated at char budget; narrow with fs_search or read specific offsets)\n"
	}
	return out
}

// isTopLevelDecl 判断 Go 源码行是否为顶层声明（行首无缩进的声明关键字）。
func isTopLevelDecl(t string) bool {
	if t == "" || t[0] == ' ' || t[0] == '\t' {
		return false
	}
	for _, kw := range []string{"package ", "import ", "const ", "var ", "type ", "func "} {
		if strings.HasPrefix(t, kw) {
			return true
		}
	}
	return false
}

// truncUTF8 按字节上限截断字符串并保证 UTF-8 边界安全（fs_read 超长单行
// 强制截断用；与引擎层 truncBytes 同语义，此处独立实现避免循环依赖）。
func truncUTF8(s string, maxBytes int) string {
	if maxBytes >= len(s) {
		return s
	}
	cut := s[:maxBytes]
	for i := 0; i < 3 && len(cut) > 0; i++ {
		if r, _ := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

// parseIntArg 宽容解析整数参数：接受 JSON number / JSON string 数字
// （"100"）/ 缺省。模型偶发把 integer 参数传成字符串（历史 schema 曾把
// limit 声明为 string，旧会话残留旧格式），严格 int 解析会直接报错。
func parseIntArg(raw json.RawMessage, def int, name string) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return def, nil
	}
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer (got %s)", name, string(raw))
	}
	return n, nil
}

// ---------- fs_write ----------

type WriteFileTool struct {
	Workspace string
	// OnAsk 允许 Agent 引擎把写文件转成 ask 审批（P1 CLI 直通批准）。
	OnApprove func(info WriteInfo) bool
}

type WriteInfo struct{ Path, Content string }

func (t *WriteFileTool) Name() string { return "fs_write" }

func (t *WriteFileTool) Description() string {
	return "Write content to a file at the given path (creates or overwrites). " +
		"Path is relative to workspace root. Returns a write confirmation with byte/line " +
		"counts — do NOT re-read the file to verify; the confirmation is authoritative." +
		" For appending partial content or very large payloads (>30KB) that risk " +
		"argument truncation, write in sections: first call with section 1, then " +
		"use fs_apply_diff with a search anchored on the last line to append section 2."
}

func (t *WriteFileTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"path":    strProp("File path relative to workspace root"),
		"content": strProp("Complete file content to write"),
	}, []string{"path", "content"})
}

func (t *WriteFileTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := resolveWritePath(t.Workspace, p.Path)
	if err != nil {
		return "", err
	}
	// 写互斥 + 写前新鲜度校验（2026-10-05 会话复盘：多会话并发改同一文件
	// 导致对方 SEARCH 集体失效）。fs_write 整文件覆盖破坏性最强，冲突检测
	// 必须先行：目标在读取后被他人改写时拒绝盲覆盖，引导重读。
	return withFileWriteLock(abs, func() (string, error) {
		var existing time.Time
		exists := false
		if fi, err := os.Stat(abs); err == nil {
			exists = true
			existing = fi.ModTime()
		}
		if hint, ok := writeGuardCheck(abs, existing, exists); !ok {
			return "", fmt.Errorf("write conflict on %s: %s", p.Path, hint)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return "", err
		}
		if err := writeFileAtomic(abs, []byte(p.Content), 0o644); err != nil {
			return "", err
		}
		writeGuardAfterWrite(abs)
		// 增强写入确认（2026-10-01 会话复盘：fs_write 后同路径回读验证 32 次）：
		// 带字节数 + 行数，明确告知「内容已精确落盘」，模型无需回读确认。
		lines := strings.Count(p.Content, "\n") + 1
		return fmt.Sprintf("Wrote %s (%d lines, %d bytes). File content is exactly what you submitted — do not re-read to verify.", p.Path, lines, len(p.Content)), nil
	})
}

// ---------- fs_apply_diff ----------

type ApplyDiffTool struct{ Workspace string }

type diffEdit struct {
	Search  string `json:"search"`
	Replace string `json:"replace"`
}

func (t *ApplyDiffTool) Name() string { return "fs_apply_diff" }

func (t *ApplyDiffTool) Description() string {
	return "Apply one or more SEARCH/REPLACE edits to an existing file. " +
		"Matching is fault-tolerant (aligned with Zoo-Code MultiSearchReplace): " +
		"exact match, then per-line trimmed match, then ':start_line:N' line " +
		"anchor, then fuzzy block matching anchored on the first/last lines. " +
		"If a match fails, prefix the search text with a line anchor like ':start_line:42\n' " +
		"(1-based, from the latest fs_read output) to pin the location."
}

func (t *ApplyDiffTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"path": strProp("File path relative to workspace root"),
		"edits": map[string]interface{}{
			"type": "array",
			"items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"search":  strProp("Exact existing text to find"),
					"replace": strProp("New text to replace with"),
				},
			},
		},
	}, []string{"path", "edits"})
}

func (t *ApplyDiffTool) Execute(ctx context.Context, args string) (string, error) {
	type argsShape struct {
		Path  string          `json:"path"`
		Edits json.RawMessage `json:"edits"`
	}
	var raw argsShape
	if err := json.Unmarshal([]byte(args), &raw); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	var p struct {
		Path  string     `json:"path"`
		Edits []diffEdit `json:"edits"`
	}
	p.Path = raw.Path
	// edits 多形态容错（2026-10-04 全量会话复盘：fs_apply_diff 失败率 10.2%，
	// TOP 失败全为畸形参数而非匹配问题）。按序尝试：
	//   A) 标准数组 [{search,replace}] → 直接用；
	//   B) 字符串形态：本身是 JSON 数组文本（\"[{...}]\"）→ 二次解析（模型把
	//      数组整体字符串化的常见形态，历史失败样本中 3 例）；
	//      纯文本 → 按 <<<<<<< SEARCH/=======/>>>>>>> REPLACE 或单块拆分；
	//   C) 嵌套形态 [{path,edits:[...]}]（模型把多文件编辑误包一层，历史
	//      失败样本中 2 例）→ 展平：path 取外层，edits 合并。
	if len(raw.Edits) > 0 {
		p.Edits, p.Path = normalizeEdits(raw.Edits, p.Path)
		if len(p.Edits) == 0 {
			// 参数形态分型报错（2026-10-06 复盘：edits 传了数组但缺 search 键的失败
			// 样本被旧文案统一报成 "must be an array"，模型误以为形态错而反复改包法）:
			// 先判 raw 是否为数组，再给出对应可操作提示 + 一行正确示例。
			var arr []json.RawMessage
			isArr := json.Unmarshal(raw.Edits, &arr) == nil
			var shapeMsg string
			if isArr {
				shapeMsg = "edits 数组中的条目缺少非空 search 字段，字段应为 {\"search\":\"原文\",\"replace\":\"新文\"}；" +
					"示例：{\"path\":\"a.txt\",\"edits\":[{\"search\":\"旧文本\",\"replace\":\"新文本\"}]}"
			} else {
				shapeMsg = "edits 必须是数组 [{\"search\":\"原文\",\"replace\":\"新文\"},...]（纯文本 SEARCH/REPLACE 也接受）；" +
					"示例：{\"path\":\"a.txt\",\"edits\":[{\"search\":\"旧文本\",\"replace\":\"新文本\"}]}"
			}
			return "", fmt.Errorf("invalid args: edits could not be parsed (got: %.80s)\n参数错误：%s", string(raw.Edits), shapeMsg)
		}
	}
	if p.Path == "" {
		return "", fmt.Errorf("path is required (path 必须是顶层字段；" +
			"示例：{\"path\":\"a.txt\",\"edits\":[{\"search\":\"...\",\"replace\":\"...\"}]})")
	}
	if len(p.Edits) == 0 {
		return "", fmt.Errorf("edits are required (edits 必须是非空数组；" +
			"示例：{\"path\":\"a.txt\",\"edits\":[{\"search\":\"...\",\"replace\":\"...\"}]})")
	}
	// 逐条参数形态校验（2026-10-06 复盘：search 缺失时，旧实现让模型在同参上循环重试）：
	// search 必须非空。search==replace 不再在此硬拒绝：模型常用它表达「该段保持不变」
	// 的占位/确认语义（2026-10-07 会话复盘：历史 333 次调用中 134 次失败为此，且其中
	// 多数是合法场景），统一移到 applyEditsOnce 内按「能否在文件中找到」分型处理。
	for i, e := range p.Edits {
		if strings.TrimSpace(e.Search) == "" {
			return "", fmt.Errorf("edit %d: search is empty (第 %d 条编辑缺少 search 文本；"+
				"字段应为 {\"search\":\"原文\",\"replace\":\"新文\"})", i+1, i+1)
		}
	}
	abs, err := resolveWritePath(t.Workspace, p.Path)
	if err != nil {
		return "", err
	}
	// 写互斥（同文件串行）：锁内完成 读取→匹配→写盘 全程，消除多会话
	// 并发改同一文件时「读到一半被对方写入」的竞态窗口。
	return withFileWriteLock(abs, func() (string, error) {
		return t.applyEdits(abs, p.Path, p.Edits)
	})
}

// applyEdits fs_apply_diff 主体（调用方已持文件写锁）：读取→逐条匹配替换
// →写盘→更新写记录。
//
// 文件不存在预检（2026-10-07 复盘：_build_gate.bat 案例——模型对不存在文件
// 发 SEARCH，os.ReadFile 返回原始 ENOENT，缺「文件不存在→先 fs_list 确认」
// 的可操作指引，模型只能盲试路径）。
//
// 防陈旧匹配重试（2026-10-05 复盘扩展）：写前新鲜度校验通过后，SEARCH 仍可能
// 因「读取快照与磁盘实际内容有差异」而落空（同会话最近一次 fs_read 至今文件被
// 另一会话改写，且 mtime 未越过 2s 粒度容差未触发 write conflict——典型症状即
// 「SEARCH block not found … file now has N lines」）。此时在写锁内自动重读
// 磁盘最新内容并重跑全部编辑一次：文件确有并发变更时基于新内容重试即成功；
// 内容并未变化仍失败 → 说明模型 SEARCH 本身陈旧/错误，按原流程报错（附定位
// 提示），明确要求重读新内容而非盲发同参。
func (t *ApplyDiffTool) applyEdits(abs, dispPath string, edits []diffEdit) (string, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s (文件不存在：先用 fs_list 确认路径，或确认文件未被并发修改/删除；"+
				"若确需新建请改用 fs_write)", dispPath)
		}
		return "", err
	}
	content := string(data)
	// 写前新鲜度校验（fix 2）：文件在本会话最近一次 fs_read 之后被外部
	// 改写时拒绝执行——模型记忆的 SEARCH 已过时，继续匹配只会产出 40+ 条
	// 同参重发失败（复盘首位问题）。冲突错误携带重读指引。
	if fi, err := os.Stat(abs); err == nil {
		if hint, ok := writeGuardCheck(abs, fi.ModTime(), true); !ok {
			return "", fmt.Errorf("write conflict on %s: %s", dispPath, hint)
		}
	}
	// retryOnMismatch 是否已做过「读取快照 vs 磁盘」自动重读重试：仅一次，
	// 避免并发写活跃时无限循环（每次重读成本 = 一次全量读取）。
	retryOnMismatch := false
	for {
		content, err = applyEditsOnce(dispPath, content, edits, retryOnMismatch)
		if err == nil {
			break
		}
		// SEARCH 未命中且未重试过 → 自动重读磁盘最新内容后重跑（写锁内无竞态）。
		if !retryOnMismatch && isSearchNotFoundErr(err) {
			retryOnMismatch = true
			fresh, rerr := os.ReadFile(abs)
			if rerr != nil {
				return "", rerr
			}
			content = string(fresh)
			continue
		}
		// 连续 SEARCH 失败计数（2026-10-06 复盘：同参重发 3-6 次死循环；第 2 次起
		// 提示更强 STOP 引导——内容可能已变，先 fs_read 最新内容再用行锚点重试，
		// 或整文件重写；计数成功时清零（diffFailReset）。fs_read 触达同样清零
		// （readGuardRecord）：重读后的重试视为全新一轮，凭最新内容构造的合法
		// 重试不应被旧计数误 STOP（2026-10-30 publish-client.ps1 误报复盘）。
		if n := diffFailIncr(abs); n >= 2 {
			return "", fmt.Errorf("%w STOP(重复失败 %d 次): 内容可能已变——请先 fs_read 该文件获取最新内容（重读会重置本计数，之后按新内容重试即重新计起），用行锚点 :start_line: 重试，或整文件 fs_write 重写；不要再次重发相同 SEARCH", err, n)
		}
		return "", err
	}
	if err := writeFileAtomic(abs, []byte(content), 0o644); err != nil {
		return "", err
	}
	// 写成功后更新记录：后续连续编辑不被自身写入误判为并发冲突。
	writeGuardAfterWrite(abs)
	// 成功即清零该文件的 SEARCH 失败计数：下一次失败重新从 1 计起。
	diffFailReset(abs)
	return fmt.Sprintf("Applied %d edit(s) to %s", len(edits), dispPath), nil
}

// isSearchNotFoundErr 判断错误是否为「SEARCH 未命中」（区别于 write conflict、
// 空 search、文件读写错误等）。匹配成功后返回错误字符串以最小侵入复用现有报错
// 文案——此处仅判定错误类别，供 applyEdits 自动重读重试决策。
func isSearchNotFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SEARCH block not found")
}

// writeFileAtomic 原子落盘：写同目录临时文件（os.CreateTemp 同目录保证同卷，
// Windows 上同卷 os.Rename 才是原子替换）→ Sync 刷盘 → Rename 覆盖目标。
// 崩溃/断电时目标文件要么是旧完整内容、要么是新完整内容，不再出现
// 「os.WriteFile 直写写一半被截断损坏」且调用方不知情的问题。
// fs_write 与 applyEdits 两处落盘统一走此 helper（写守卫/protect 检查逻辑
// 顺序不变，仅替换最后的落盘动作）。
// 失败时清理临时文件不残留；目标为已存在目录等 Rename 必败场景同样走清理路径。
// ponytail: 无多份 .bak 版本化备份——单份临时文件已覆盖崩溃窗口，需要版本化
// 回滚时再扩展。
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kaelvoris-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 失败清理：临时文件句柄先关（Windows 上打开句柄会阻止删除）再移除。
	// 成功路径 Rename 后 tmpName 已不存在，本 defer 不触发（err==nil）。
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// findOnly 仅定位（不做替换）：按 matchAndReplace 相同的四种策略判断 search 是否
// 存在于 content 中（供 search==replace 的 no-op 确认路径使用）。
func findOnly(content, search string) (string, bool) {
	if strings.Contains(content, search) {
		return content, true
	}
	src := strings.Split(content, "\n")
	orig := patternLines(search)
	if len(orig) == 0 || len(src) == 0 {
		return "", false
	}
	if _, size := indexOfTrim(src, orig); size > 0 {
		return content, true
	}
	if rest, lineNo, has := splitLineAnchor(search); has && strings.TrimSpace(rest) != "" {
		if _, size := matchAnchor(src, patternLines(rest), lineNo); size > 0 {
			return content, true
		}
	}
	if _, size := fuzzyBlockMatch(src, orig, fuzzyThreshold); size > 0 {
		return content, true
	}
	return "", false
}

// clampStr 超长文本截断用于错误信息（避免整块原文撑爆报错）。
func clampStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// applyEditsOnce 对 content 执行全部 edits（单次快照）；返回 (更新后内容, 首个错误)。
// 任一条 SEARCH 未命中即返回哨兵错误（携带定位提示），由调用方决定是否重读重试。
//
// search==replace 分型（2026-10-07 会话复盘：历史失败 134/188 次是 identical 被拒，
// 但样本显示模型多数是故意用「search==replace」表达「该段保持不变」的占位/确认语义）：
//   - SEARCH 能在文件中找到 → 该 edit 等价 no-op（确认存在且不变），不报错、不修改；
//   - SEARCH 找不到 → 才可能是真写反（replace 应放新内容）或内容已变，按 SEARCH 未命中
//     报错（携带「identical + 找不到 = 写反或内容已变」的额外提示），模型可据此修正。
func applyEditsOnce(dispPath, content string, edits []diffEdit, reread bool) (string, error) {
	// 逐条匹配替换；全部成功后返回（content, nil）。
	for i, e := range edits {
		if e.Search == "" {
			return "", fmt.Errorf("edit %d: search is empty", i)
		}
		// search==replace：找到即 no-op（保持不变是明确的意图）；找不到才报错。
		if e.Search == e.Replace {
			if !strings.Contains(content, e.Search) {
				return "", fmt.Errorf("edit %d: search and replace are identical (%q) but the search text was NOT found - "+
					"this is likely a swapped/placeholder mistake or the content has changed on disk; "+
					"re-check with fs_read and put the NEW text in replace (search 与 replace 相同但原文未找到："+
					"极可能写反了或文件已变；请 fs_read 最新内容，确认后把新内容填入 replace 字段)", i, truncUTF8(e.Search, 120))
			}
			continue // 确认存在且无需修改：跳过
		}
		nc, ok := matchAndReplace(content, e.Search, e.Replace)
		if !ok {
			// replace-exists 检测（对标 Aider editblock_coder.py："Are you sure you
			// need this SEARCH/REPLACE block? The REPLACE lines are already in %s!"）。
			// SEARCH 未命中但 REPLACE 已在文件中 → 大概率内容已改过/写反/重复编辑，
			// 引导核实而非盲试。仅对多行 replace 检测（单行误报率高）。
			repHint := ""
			if strings.Count(strings.TrimRight(e.Replace, "\n"), "\n") >= 1 && strings.Contains(content, strings.TrimRight(e.Replace, "\n")) {
				repHint = "; the REPLACE text is already present in the file - the edit may have already been applied or search/replace are swapped; fs_read the file to verify and do NOT re-send the same edit"
			}
			// 定位提示（2026-10-03 第三轮复盘：SEARCH 未命中时旧报错无任何
			// 定位线索，模型只能盲改重试——单会话最多 11 连失败）。附加
			// 「最相似候选块」行号 + 首行内容，引导模型直接重读该位置。
			hint := nearestBlockHint(content, e.Search)
			// 新鲜度提示（2026-10-05 会话复盘：同参数原样重发占据失败首位）——
			// 附加文件当前规模（行数/字节），模型若发现行数与记忆不符即可判定
			// 文件已被并发修改，直接重读而非重发同一 SEARCH。
			totalLines := strings.Count(content, "\n")
			if len(content) > 0 && !strings.HasSuffix(content, "\n") {
				totalLines++
			}
			// reread 为 true 说明已自动重读磁盘最新内容并重试仍失败：内容确已
			// 被并发修改（且 mtime 粒度容差未触发 write conflict），模型记忆与
			// 磁盘不一致——重申同参无意义，必须 fs_read 获取最新内容（2026-10-05
			// 复盘：同参重发占据失败首位；此前报错未区分是否已重读，模型无法
			// 判断该继续重试还是转重读，产生 40+ 条重复失败）。
			readHint := ""
			if reread {
				readHint = " (auto re-read of the current disk content was already attempted " +
					"and still not found: the file has changed on disk - fs_read it now and " +
					"redo the edit from fresh content; do NOT re-send the same SEARCH)"
			}
			return "", fmt.Errorf("edit %d: SEARCH block not found in %s "+
				"(tried exact, trimmed, line-anchor and fuzzy matching)%s%s "+
				"[file now has %d lines / %d bytes; if this differs from what you last read, "+
				"another agent or process has modified the file - fs_read it again before retrying, "+
				"and never retry with the identical SEARCH block]%s", i+1, dispPath, repHint, hint, totalLines, len(content), readHint)
		}
		content = nc
	}
	return content, nil
}

// matchAndReplace 在 content 中定位 search 并替换（只替换首个命中处），
// 依序尝试四种定位策略（对齐 Zoo-Code MultiSearchReplaceDiffStrategy）：
// 精确匹配 → 行首尾空白松匹配 → :start_line: 行号锚定 → 首尾行块锚 + 相似度。
func matchAndReplace(content, search, replace string) (string, bool) {
	// 1) 精确匹配（CRLF 原样保留）
	if strings.Contains(content, search) {
		return strings.Replace(content, search, replace, 1), true
	}
	src := strings.Split(content, "\n")
	orig := patternLines(search)
	if len(orig) == 0 || len(src) == 0 {
		return "", false
	}
	// 2) 行级松匹配：忽略每行首尾空白与行尾 \r
	if start, size := indexOfTrim(src, orig); size > 0 {
		return spliceLines(src, start, size, replace), true
	}
	// 3) :start_line:N 行号锚定（N 为 1-based，内容自该行起松匹配）
	if rest, lineNo, has := splitLineAnchor(search); has && strings.TrimSpace(rest) != "" {
		if start, size := matchAnchor(src, patternLines(rest), lineNo); size > 0 {
			return spliceLines(src, start, size, replace), true
		}
	}
	// 4) 首尾行块锚 + 行相似度均值确认
	if start, size := fuzzyBlockMatch(src, orig, fuzzyThreshold); size > 0 {
		return spliceLines(src, start, size, replace), true
	}
	return "", false
}

// fuzzyThreshold 模糊块匹配的行相似度均值下限（取高阈值避免误替换）。
const fuzzyThreshold = 0.9

// normalizeEdits edits 参数多形态归一化（2026-10-04 全量会话复盘）：返回解析
// 出的编辑对与（若嵌套形态携带）目标 path。处理四种历史失败形态：
//  1. 标准数组 [{search,replace}] → 直接返回（path 不变）；
//  2. 字符串化 JSON 数组（模型把整个数组二次编码为字符串）→ 二次解析；
//  3. 纯文本 SEARCH/REPLACE 块（含标准标记）→ parsePlainDiff 拆分；
//  4. 嵌套形态 [{path,edits:[...]}]（模型把单文件编辑误包一层）→ 取内层
//     edits 与 path；字段名同义变体（old text/new text、oldText/newText、
//     old_search/new_search 等）一并归一化为 search/replace。
//
// 解析不出任何编辑对时返回 nil（由调用方报错）。
func normalizeEdits(raw json.RawMessage, defPath string) ([]diffEdit, string) {
	// 1) 标准数组形态（须至少一条 search 非空：未知字段会被 Go 忽略，
	// 嵌套/同义字段形态也会在这里 unmarshal 成功但内容全空，不能提前返回）。
	var edits []diffEdit
	if err := json.Unmarshal(raw, &edits); err == nil && len(edits) > 0 && hasNonEmptySearch(edits) {
		return edits, defPath
	}
	// 2) 嵌套形态 [{path,edits:[...]}]（含同义字段名变体）。
	var nested []struct {
		Path     string          `json:"path"`
		EditsRaw json.RawMessage `json:"edits"`
		// 同义变体直接落在编辑对象上（无 edits 嵌套时）。
		Search    string `json:"search"`
		Replace   string `json:"replace"`
		OldSearch string `json:"old search"`
		NewSearch string `json:"new search"`
		OldText   string `json:"oldText"`
		NewText   string `json:"newText"`
		OldStr    string `json:"old_string"`
		NewStr    string `json:"new_string"`
	}
	if err := json.Unmarshal(raw, &nested); err == nil && len(nested) > 0 {
		hasInner := false
		for _, n := range nested {
			if len(n.EditsRaw) > 0 {
				hasInner = true
				break
			}
		}
		if hasInner {
			out := make([]diffEdit, 0, len(nested))
			p := defPath
			for _, n := range nested {
				if n.Path != "" {
					p = n.Path
				}
				var inner []diffEdit
				if jerr := json.Unmarshal(n.EditsRaw, &inner); jerr == nil {
					for _, ie := range inner {
						out = append(out, ie)
					}
					continue
				}
				// 内层 edits 也可能是字符串化数组或纯文本块。
				var s string
				if serr := json.Unmarshal(n.EditsRaw, &s); serr == nil {
					var inner2 []diffEdit
					if jerr := json.Unmarshal([]byte(s), &inner2); jerr == nil {
						out = append(out, inner2...)
					} else {
						out = append(out, parsePlainDiff(s)...)
					}
				}
			}
			if len(out) > 0 {
				return out, p
			}
		}
	}
	// 3) 字符串形态：本身是 JSON 数组文本 → 二次解析；纯文本 → diff 块拆分。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if arr := []byte(s); json.Unmarshal(arr, &edits) == nil && len(edits) > 0 {
			return edits, defPath
		}
		if e := parsePlainDiff(s); len(e) > 0 {
			return e, defPath
		}
		// 字符串内容本身可能是嵌套对象数组（少见）→ 递归归一化。
		if inner := json.RawMessage(s); len(inner) > 0 {
			if e2, p2 := normalizeEdits(inner, defPath); len(e2) > 0 {
				return e2, p2
			}
		}
	}
	// 4) 数组形态但字段名是同义变体（如 old search/new search）：
	//    逐条扫描对象键，命中任一变体即归一化为 search/replace。
	var loose []map[string]interface{}
	if err := json.Unmarshal(raw, &loose); err == nil && len(loose) > 0 {
		out := make([]diffEdit, 0, len(loose))
		for _, m := range loose {
			get := func(keys ...string) string {
				for _, k := range keys {
					if v, ok := m[k]; ok {
						if sv, ok2 := v.(string); ok2 && sv != "" {
							return sv
						}
					}
				}
				return ""
			}
			se := get("search", "old search", "old_search", "oldText", "old_string", "old", "find")
			re := get("replace", "new search", "new_search", "newText", "new_string", "new", "replacement")
			if strings.TrimSpace(se) != "" {
				out = append(out, diffEdit{Search: se, Replace: re})
			}
		}
		if len(out) > 0 {
			return out, defPath
		}
	}
	return nil, defPath
}

// hasNonEmptySearch 判断编辑对中是否至少一条 search 非空（标准形态判定）。
func hasNonEmptySearch(edits []diffEdit) bool {
	for _, e := range edits {
		if strings.TrimSpace(e.Search) != "" {
			return true
		}
	}
	return false
}

// parsePlainDiff 把模型误传的整段 SEARCH/REPLACE 文本拆为编辑对。
// 支持两种形态：标准 diff 标记（<<<<<<< SEARCH / ======= / >>>>>>> REPLACE，
// 对齐 Zoo-Code/roo diff 格式）与无标记单块（首段 SEARCH、空 REPLACE 不成立
// 时报 false，避免误替换）。
func parsePlainDiff(s string) []diffEdit {
	const head, sep, tail = "<<<<<<< SEARCH", "=======", ">>>>>>> REPLACE"
	if !strings.Contains(s, head) {
		return nil
	}
	var edits []diffEdit
	blocks := strings.Split(s, head)
	for _, b := range blocks[1:] {
		ti := strings.Index(b, tail)
		if ti < 0 {
			continue
		}
		b = b[:ti]
		// 尾部 REPLACE 标记可能带 \r，统一处理。
		b = strings.TrimSuffix(b, "\n")
		b = strings.TrimSuffix(b, "\r")
		si := strings.Index(b, "\n"+sep+"\n")
		var search, replace string
		if si < 0 {
			// 单行 SEARCH：整段 = "search\n=======\nreplace"
			lines := patternLines(b)
			mid := -1
			for j, l := range lines {
				if strings.TrimSpace(l) == sep {
					mid = j
					break
				}
			}
			if mid <= 0 || mid == len(lines)-1 {
				continue
			}
			search = strings.Join(lines[:mid], "\n")
			replace = strings.Join(lines[mid+1:], "\n")
		} else {
			search = strings.TrimSuffix(b[:si], "\r")
			replace = strings.TrimPrefix(b[si+len(sep)+2:], "\n")
			replace = strings.TrimSuffix(replace, "\r")
			search = strings.TrimPrefix(search, "\n")
		}
		if strings.TrimSpace(search) == "" {
			continue
		}
		edits = append(edits, diffEdit{Search: search, Replace: replace})
	}
	return edits
}

// nearestBlockHint SEARCH 未命中时的定位提示：以 SEARCH 首行（trim 后）为锚
// 在源文本中找最相似窗口（行相似度均值最高），返回「最近似位置：行 N：<首行>」
// 形式的提示；无任何锚点命中时返回空串（完全无线索时不再硬猜）。
// 目的：fs_apply_diff 失败时模型凭行号直接重读，不再逐段 fs_read 试探。
func nearestBlockHint(content, search string) string {
	src := strings.Split(content, "\n")
	pat := patternLines(search)
	if len(pat) == 0 || len(pat) > len(src) {
		return ""
	}
	first := trimLine(pat[0])
	if first == "" {
		return ""
	}
	best, bestSim := -1, 0.0
	for i := 0; i+len(pat) <= len(src); i++ {
		if tf := trimLine(src[i]); tf != first && lineSim(tf, first) < 0.6 {
			continue // 首行相似度过低的窗口直接跳过（省编辑距离计算）
		}
		sim := 0.0
		for j := range pat {
			sim += lineSim(src[i+j], pat[j])
		}
		sim /= float64(len(pat))
		if sim > bestSim {
			best, bestSim = i, sim
		}
	}
	// 兜底：SEARCH 比文件还长（窗口滑不动）时退化为单行锚——仅按首行
	// 在全文件中找最相似行给出行号，仍有定位价值。
	if best < 0 {
		for i := range src {
			sim := lineSim(src[i], first)
			if sim > bestSim {
				best, bestSim = i, sim
			}
		}
		if best < 0 || bestSim < 0.6 {
			// 全无线索或相似度过低：仍附最相似行位置（best>=0 时），避免大文件盲重读。
			if best < 0 {
				return "; no similar line found in file - fs_read the file to see its current content"
			}
			return fmt.Sprintf("; best resembles line %d: %q (low similarity %.2f - content may be heavily refactored; fs_read around that line)", best+1, trimLine(src[best]), bestSim)
		}
		return fmt.Sprintf("; search start line resembles file line %d: %q (search block longer than file - re-read the file)", best+1, trimLine(src[best]))
	}
	if bestSim < 0.6 {
		// 相似度过低：内容可能已大幅重构。不再丢弃行号线索（2026-10-28 复盘：
		// cozeflow_panel.go 1074 行大文件连续 3 次失败，报错只说 "changed significantly"
		// 无任何位置线索，模型只能整文件盲重读，浪费多轮）。附最相似行位置引导
		// 定向重读，与正常路径同等定位价值。
		return fmt.Sprintf("; best resembles line %d: %q (low similarity %.2f - content may be heavily refactored; fs_read around that line)", best+1, trimLine(src[best]), bestSim)
	}
	return fmt.Sprintf("; nearest similar block at line %d: %q", best+1, trimLine(src[best]))
}

// patternLines 把文本切成比对行：统一 CRLF→LF，去掉结尾换行产生的末尾空行。
func patternLines(s string) []string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// indexOfTrim 行级松匹配：忽略每行首尾空白（含行尾 \r）逐行比对。
func indexOfTrim(src, pat []string) (start, size int) {
	if len(pat) == 0 || len(pat) > len(src) {
		return -1, 0
	}
	for i := 0; i+len(pat) <= len(src); i++ {
		ok := true
		for j := range pat {
			if trimLine(src[i+j]) != trimLine(pat[j]) {
				ok = false
				break
			}
		}
		if ok {
			return i, len(pat)
		}
	}
	return -1, 0
}

// matchAnchor 锚定匹配：要求 pat 自 src 的 lineNo-1 行（1-based）起松匹配。
func matchAnchor(src, pat []string, lineNo int) (start, size int) {
	i := lineNo - 1
	if i < 0 || i+len(pat) > len(src) {
		return -1, 0
	}
	for j := range pat {
		if trimLine(src[i+j]) != trimLine(pat[j]) {
			return -1, 0
		}
	}
	return i, len(pat)
}

// fuzzyBlockMatch 块级模糊匹配：以 pat 首尾行（trim 后）为锚滑动窗口，
// 命中窗口后按行相似度均值 ≥ 阈值确认（对齐 Zoo-Code fuzzySearch + getSimilarity）。
func fuzzyBlockMatch(src, pat []string, threshold float64) (start, size int) {
	if len(pat) == 0 || len(pat) > len(src) {
		return -1, 0
	}
	first, last := trimLine(pat[0]), trimLine(pat[len(pat)-1])
	for i := 0; i+len(pat) <= len(src); i++ {
		if trimLine(src[i]) != first || trimLine(src[i+len(pat)-1]) != last {
			continue
		}
		sim := 0.0
		for j := range pat {
			sim += lineSim(src[i+j], pat[j])
		}
		if sim/float64(len(pat)) >= threshold {
			return i, len(pat)
		}
	}
	return -1, 0
}

// spliceLines 用 replace 的行替换 src[start:start+size] 并重组；
// 若被替换块为 CRLF 风格，替换行保持一致（补 \r）。
func spliceLines(src []string, start, size int, replace string) string {
	repl := patternLines(replace)
	if start < len(src) && strings.HasSuffix(src[start], "\r") {
		for i, l := range repl {
			repl[i] = strings.TrimSuffix(l, "\r") + "\r"
		}
	}
	out := make([]string, 0, len(src)-size+len(repl))
	out = append(out, src[:start]...)
	out = append(out, repl...)
	out = append(out, src[start+size:]...)
	return strings.Join(out, "\n")
}

// lineAnchorRe :start_line:N 行号锚标记（对齐 Zoo-Code SEARCH 头部行号提示）。
var lineAnchorRe = regexp.MustCompile(`^:start_line:(\d+)\s*$`)

// splitLineAnchor 识别 search 首部的 :start_line:N 标记（可选跟随 ------- 分隔线），
// 返回去除标记后的剩余文本与 1-based 行号。
func splitLineAnchor(search string) (rest string, lineNo int, ok bool) {
	lines := patternLines(search)
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) {
		return "", 0, false
	}
	m := lineAnchorRe.FindStringSubmatch(lines[i])
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return "", 0, false
	}
	i++
	if i < len(lines) && strings.TrimSpace(lines[i]) == "-------" {
		i++
	}
	if i >= len(lines) {
		return "", n, true // 仅锚点无内容
	}
	return strings.Join(lines[i:], "\n"), n, true
}

// trimLine 单行归一化（去行尾 \r 与首尾空白）。
func trimLine(s string) string {
	return strings.TrimSpace(strings.TrimSuffix(s, "\r"))
}

// lineSim 两行相似度 [0,1]（1 - 编辑距离/最大 rune 长度，trim 后比较）。
func lineSim(a, b string) float64 {
	a = strings.TrimSpace(strings.TrimSuffix(a, "\r"))
	b = strings.TrimSpace(strings.TrimSuffix(b, "\r"))
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	maxLen := len(ra)
	if len(rb) > maxLen {
		maxLen = len(rb)
	}
	if maxLen == 0 {
		return 1
	}
	return 1 - float64(editDistance(ra, rb))/float64(maxLen)
}

// editDistance 编辑距离 DP（行内文本短，O(n·m) 可接受）。
func editDistance(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// ---------- fs_list ----------

type ListFilesTool struct{ Workspace string }

func (t *ListFilesTool) Name() string { return "fs_list" }

func (t *ListFilesTool) Description() string {
	return "List files and directories at the given path (top-level by default). " +
		"Path is relative to workspace root; use \"\" for root."
}

func (t *ListFilesTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"path":      strProp("Directory path relative to workspace root; empty for root"),
		"recursive": map[string]interface{}{"type": "boolean", "description": "List recursively"},
	}, []string{})
}

func (t *ListFilesTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	abs, err := resolveInWorkspace(t.Workspace, p.Path)
	if err != nil {
		return "", err
	}
	// 存在性预检（2026-10-04 复盘：旧实现 WalkDir 回调把错误吞成 return
	// nil，不存在路径静默返回 (empty directory)，模型误判后瞎猜路径）。
	if st, serr := os.Stat(abs); serr != nil {
		if os.IsNotExist(serr) {
			return "", fmt.Errorf("path not found: %s (verify against fs_list of the workspace root or parent directory)", p.Path)
		}
		return "", serr
	} else if !st.IsDir() {
		// 传文件路径：旧实现返回含单个文件名的清单，易被误读为目录内容；
		// 改为明确提示单文件场景应直接用 fs_read。
		return fmt.Sprintf("%s is a file, not a directory: use fs_read to read its contents", p.Path), nil
	}
	// 只读白名单目录（如 ~/.kaelvoris/sessions）：不在工作区内，工作区根的
	// .rooignore/.gitignore 对其不适用（规则按工作区相对路径锚定），直接列目录。
	inWhitelist := inReadOnlyWhitelist(abs)
	ign := loadIgnoreMatcher(t.Workspace)
	wsAbs, _ := filepath.Abs(t.Workspace)
	if inWhitelist {
		ign = nil // 白名单目录不受工作区 ignore 规则过滤
	}
	var lines []string
	depth := 1
	if p.Recursive {
		depth = 5
	}
	err = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过不可读项
		}
		rel, _ := filepath.Rel(abs, path)
		if rel == "." {
			return nil
		}
		// .rooignore/.gitignore 过滤（目录命中整棵剪枝，见 ignore.go）；
		// 白名单目录跳过（inWhitelist 时 ign 已置 nil）。
		if !inWhitelist {
			if relWs, rerr := filepath.Rel(wsAbs, path); rerr == nil &&
				ign.ignore(filepath.ToSlash(relWs), d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			if !inWhitelist && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			if !p.Recursive && depth < 1 {
				return filepath.SkipDir
			}
			lines = append(lines, filepath.ToSlash(rel)+"/")
			if !p.Recursive {
				depth--
				if depth <= 0 {
					return filepath.SkipDir
				}
			}
			return nil
		}
		lines = append(lines, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "(empty directory)", nil
	}
	if len(lines) > 500 {
		lines = lines[:500]
		lines = append(lines, "... (truncated)")
	}
	return strings.Join(lines, "\n"), nil
}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "__pycache__", ".venv", "vendor", "dist", "build", "参考":
		return true
	}
	return strings.HasPrefix(name, ".") && name != ".rooignore"
}

// ---------- fs_search ----------

// fs_search 输出与扫描预算：单次工具结果控制在可回灌上下文的量级内
// （引擎侧另有 clampToolResult 40k 字符钳制兜底）。
const (
	maxSearchHits     = 200     // 全局命中行上限（超出截断并提示）
	maxHitsPerFile    = 10      // 单文件命中行上限（生成产物/锁文件常整文件重复命中）
	binaryProbeBytes  = 8192    // 二进制探测只读文件头 8KB，不再整读 exe/zip/镜像
	maxSearchFileSize = 2 << 20 // 超过 2MB 的文件跳过（对齐 rg --max-filesize 2M，两条路径行为一致）
)

type SearchFilesTool struct{ Workspace string }

func (t *SearchFilesTool) Name() string { return "fs_search" }

func (t *SearchFilesTool) Description() string {
	return "Search file contents with a regular expression (ripgrep-backed, pure-Go fallback). " +
		"Pass `path` to scope the search to a subdirectory (much faster on large workspaces) or to a single file, " +
		"and `file_pattern` to filter by filename glob (e.g. *.go). " +
		"Heavy dependency/build directories (node_modules, dist, build, vendor, __pycache__, .venv, dot-dirs) " +
		"are ignored by default to save CPU on large workspaces; pass `ignore_dirs` " +
		"(comma-separated dir names or relative paths, e.g. \"logs, tmp, a/b\") to ignore more. " +
		"Returns matching lines as file:line: content, with paths relative to the workspace root."
}

func (t *SearchFilesTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"regex": strProp("Regular expression (Go syntax)"),
		"file_pattern": map[string]interface{}{
			"type":        "string",
			"description": "Optional glob to filter files, e.g. *.go",
		},
		"path": map[string]interface{}{
			"type":        "string",
			"description": "Optional subdirectory (relative to workspace root, e.g. gocoder/webview-shell) or single file to limit the search scope. Prefer this over full-workspace search when the target area is known - it is dramatically faster.",
		},
		"ignore_dirs": map[string]interface{}{
			"type":        "string",
			"description": "Optional comma-separated directory names or relative paths to skip, e.g. \"logs, tmp, a/b\". Default-ignored anyway: node_modules, dist, build, vendor, __pycache__, .venv and dot-dirs.",
		},
	}, []string{"regex"})
}

func (t *SearchFilesTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Regex       string `json:"regex"`
		FilePattern string `json:"file_pattern"`
		Path        string `json:"path"`
		IgnoreDirs  string `json:"ignore_dirs"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Regex == "" {
		return "", fmt.Errorf("regex is required")
	}
	if _, err := regexp.Compile(p.Regex); err != nil {
		return "", fmt.Errorf("invalid regex: %w", err)
	}
	// ignore_dirs 参数：逗号分隔的目录名/相对路径，叠加在默认忽略目录之上
	//（node_modules/dist/build/vendor 等始终跳过，见 defaultIgnoreDirs）。
	ignoreDirs := parseIgnoreDirs(p.IgnoreDirs)

	// path 参数：把搜索范围限定到工作区子目录/单文件（性能关键——大工作区
	// 全量遍历可达数万文件，定向子目录快一个数量级以上）。解析复用
	// resolveInWorkspace：越界拒绝与白名单放行语义一致。
	targetRel := "."          // rg 的搜索路径参数（工作区相对、'/' 分隔）
	targetRoot := t.Workspace // walkSearch 的遍历根
	if strings.TrimSpace(p.Path) != "" {
		abs, err := resolveInWorkspace(t.Workspace, p.Path)
		if err != nil {
			return "", err
		}
		if _, statErr := os.Stat(abs); statErr != nil {
			return "", fmt.Errorf("path %q: %w", p.Path, statErr)
		}
		// 只读白名单目录（如 ~/.kaelvoris/sessions，绝对路径形态）：非工作区、
		// 无 .rooignore 语义，直接在该目录内纯 Go 遍历（文件量级小无性能问题）。
		if inReadOnlyWhitelist(abs) {
			return walkSearch(abs, abs, re2Golang(p.Regex), p.FilePattern, ignoreDirs)
		}
		rel, relErr := filepath.Rel(t.Workspace, abs)
		if relErr != nil {
			return "", relErr
		}
		targetRel = filepath.ToSlash(rel)
		targetRoot = abs
	}

	// 优先 ripgrep（对齐 Zoo-Code SearchFilesTool 实现），失败回退纯 Go 遍历。
	// file_pattern 路径形态（含 /）在两条路径下均拆为目录收窄 + basename 匹配。
	if out, err := ripgrepSearch(t.Workspace, targetRel, p.Regex, p.FilePattern, ignoreDirs); err == nil {
		return out, nil
	}
	return walkSearchScoped(t.Workspace, targetRoot, re2Golang(p.Regex), p.FilePattern, ignoreDirs)
}

// defaultIgnoreDirs fs_search 默认忽略的依赖/构建产物目录（与 walkSearch 的
// skipDir 对齐）：前端 node_modules、前端/桌面构建 dist/build、Go vendor、
// Python 缓存等——目录内文件量巨大且几乎从不是检索目标，全量扫描徒耗 CPU。
// 隐藏目录（.git/.venv 等）rg 默认跳过 hidden、walkSearch 的 skipDir 兜底，
// 不在此重复列出。2026-10-02 复盘：rg 路径此前只排除 .rooignore/.gitignore，
// 项目缺 .gitignore 时（如工作区根散装脚本区）node_modules 仍被全量扫描。
var defaultIgnoreDirs = []string{"node_modules", "dist", "build", "vendor", "__pycache__", "参考"}

// parseIgnoreDirs 解析 ignore_dirs 参数：逗号分隔的目录名或工作区相对路径。
// 统一 '/' 分隔，去空白、首尾 '/' 与 "./" 前缀；空段跳过。
func parseIgnoreDirs(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		p = filepath.ToSlash(strings.TrimPrefix(p, "./"))
		p = strings.Trim(p, "/")
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	return out
}

// isUserIgnoredDir 用户 ignore_dirs 目录命中判定（纯 Go 回退路径）：
// 名字形态（不含 /）按目录名在任意层级匹配；路径形态按工作区相对路径
// 全等或以 "/dir" 结尾（祖先目录命中即整枝跳过）。
func isUserIgnoredDir(relWs, name string, ignoreDirs []string) bool {
	for _, g := range ignoreDirs {
		if !strings.Contains(g, "/") {
			if g == name {
				return true
			}
			continue
		}
		if relWs == g || strings.HasSuffix(relWs, "/"+g) {
			return true
		}
	}
	return false
}

// targetOnDefaultIgnoreDir 判断定向搜索目标是否落在默认忽略目录内（或就是
// 该目录本身，如 path=frontend/dist）：用户显式定向到构建产物目录时应返回
// 其内容，该目录的默认忽略 glob 需豁免，否则 rg 根目录即被排除、恒空结果。
func targetOnDefaultIgnoreDir(targetRel string) bool {
	if targetRel == "." {
		return false
	}
	for _, name := range defaultIgnoreDirs {
		if targetRel == name || strings.HasPrefix(targetRel, name+"/") ||
			strings.HasSuffix(targetRel, "/"+name) || strings.Contains(targetRel, "/"+name+"/") {
			return true
		}
	}
	return false
}

// rg 查找结果缓存：exec.LookPath("rg") 每次调用都遍历 PATH 全部目录，GUI 进程
// PATH 含 Go 工具链等深目录（目录内文件量上万），实测每次查找秒级卡顿。
// rg 是否存在在进程生命周期内不变，缓存查找结果即可。
var (
	rgMu      sync.Mutex
	rgBin     string
	rgMissing bool
)

// lookRg 带缓存的 exec.LookPath("rg")。
func lookRg() (string, error) {
	rgMu.Lock()
	defer rgMu.Unlock()
	if rgBin != "" {
		return rgBin, nil
	}
	if rgMissing {
		return "", exec.ErrNotFound
	}
	bin, err := exec.LookPath("rg")
	if err != nil {
		rgMissing = true
		return "", err
	}
	rgBin = bin
	return bin, nil
}

// splitPathGlob 拆解路径形态 glob（含 / 的 file_pattern）：
//
//	"a/b/server.go" → dirGlob="a/b/**"，fileGlob="server.go"
//	"a/b/*.go"      → dirGlob="a/b/**"，fileGlob="*.go"
//	"*.go"          → ""，"*.go"（非路径形态，原样返回）
//
// 修复（2026-10-03 复盘）：模型常把 file_pattern 传成路径形态（
// "gocoder/internal/remote/server.go"），旧实现整串塞进 --glob/basename
// 匹配永不命中，定向搜索恒空结果（会话 1790944175628 尾部 3 连空搜）。
func splitPathGlob(glob string) (dirPart, filePart string) {
	g := filepath.ToSlash(strings.TrimSpace(glob))
	if g == "" || !strings.Contains(g, "/") {
		return "", g
	}
	// 反否定 glob（!.rooignore 形态）不拆解，交由调用方原样处理。
	if strings.HasPrefix(g, "!") {
		return "", g
	}
	dir, file := filepath.Split(filepath.FromSlash(g))
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return "", file
	}
	return dir + "/**", file
}

// ripgrepSearch 优先用 rg，输出直接复用。
// targetRel 为相对工作区根的搜索目标（"." = 全工作区，或 "gocoder/webview-shell"
// 这类子目录）：rg 以 workspace 为 cwd、以 targetRel 为搜索路径，输出路径自动
// 保持工作区相对形态，与 walkSearch 输出布局一致。
func ripgrepSearch(workspace, targetRel, pattern, glob string, ignoreDirs []string) (string, error) {
	bin, err := lookRg()
	if err != nil {
		return "", err
	}
	// 路径形态 glob 拆解：目录前缀收窄搜索根（targetRel="." 时），basename
	// 部分作为真正的文件名 glob（修复 file_pattern 传路径时恒空结果）。
	if dirPart, filePart := splitPathGlob(glob); dirPart != "" {
		if dir := strings.TrimSuffix(dirPart, "/**"); dir != "" && targetRel == "." {
			targetRel = dir
		}
		glob = filePart
	}
	// --path-separator /：Windows 下 rg 默认输出 `sub\code.go`，与纯 Go 回退
	// 路径的 `/` 分隔输出不一致（消费方正则按 / 匹配）；统一为 /。
	// --with-filename：rg 对单一文件搜索默认省略文件名前缀，输出无法定位
	// 来源，强制与多文件模式一致输出 file:line: 格式。
	// --max-count 对齐 walkSearch 的 maxHitsPerFile（生成产物/锁文件整文件
	// 重复命中时截断，保多文件覆盖面），两条路径行为一致。
	// 全局命中上限见下方 capSearchOutput（rg 无全局上限旗标，输出侧统计补齐）。
	args := []string{"--line-number", "--with-filename", "--path-separator", "/",
		"--max-count", strconv.Itoa(maxHitsPerFile), "--max-filesize", "2M"}
	if glob != "" {
		args = append(args, "--glob", glob)
	}
	// 默认忽略目录 + 用户 ignore_dirs：全部转成否定 --glob 显式排除。
	// rg 仅原生遵守 .gitignore/.ignore，项目缺 .gitignore 时 node_modules/dist
	// 会被全量扫描；目录 glob（`!name/`）命中任意层级，目录整体排除后其下
	// 文件不再读入，扫描开销大幅下降。定向目标本身在默认忽略目录内时
	//（path=frontend/dist 这类显式检索产物）豁免，否则恒空结果。
	if !targetOnDefaultIgnoreDir(targetRel) {
		for _, name := range defaultIgnoreDirs {
			args = append(args, "--glob", "!"+name+"/")
		}
	}
	for _, name := range ignoreDirs {
		if strings.Contains(name, "/") {
			// 路径形态：名字 glob 对任意层级生效，故追加 **/ 前缀精确锚定。
			args = append(args, "--glob", "!**/"+name+"/")
		} else {
			args = append(args, "--glob", "!"+name+"/")
		}
	}
	// .rooignore 以否定 --glob 显式排除（rg 原生遵守 .gitignore）
	for _, g := range rooignoreGlobs(workspace) {
		args = append(args, "--glob", g)
	}
	args = append(args, pattern, targetRel)
	cmd := exec.Command(bin, args...)
	cmd.Dir = workspace
	sysproc.Hide(cmd) // GUI 进程下隐藏控制台黑框。
	out, err := cmd.Output()
	if err != nil {
		// rg 退出码约定：0 = 有匹配；1 = 无匹配（非错误，旧实现误当失败
		// 回退全量遍历）；2 = 遇到无法读取的文件（如被其他进程锁定的
		// 日志/profile 文件）——此时 stdout 已有部分有效结果，直接采用而非
		// 回退慢速全量遍历；其余退出码才真正回退。
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee) && ee.ExitCode() == 1:
			return "No matches found", nil
		case errors.As(err, &ee) && ee.ExitCode() == 2 && len(out) > 0:
			return capSearchOutput(string(out)), nil
		default:
			return "", err
		}
	}
	if len(out) == 0 {
		return "No matches found", nil
	}
	return capSearchOutput(string(out)), nil
}

// capSearchOutput rg 路径全局命中上限对齐：rg 无全局上限旗标（只有 per-file
// 的 --max-count），按输出行数（--line-number --with-filename 下每命中一行）
// 统计，达到 maxSearchHits 时末尾补截断提示——文案与 walkSearch 的 Go 路径
// 完全一致，两条路径行为对齐（2026-11 可靠性加固）。≥ 而非 >：恰为 200 条
// 时无法确认 rg 是否还有更多命中，与 Go 路径「达到即标截断」语义一致。
func capSearchOutput(s string) string {
	if strings.Count(s, "\n") >= maxSearchHits {
		return s + "... (truncated)\n"
	}
	return s
}

// walkSearch 纯 Go 回退实现（应用 .rooignore/.gitignore 过滤）：
// file_pattern 路径形态（含 /）拆为目录收窄 + basename 匹配（修复③）。
// workspace 用于结果路径与 ignore 规则基准；target 为实际遍历根（工作区、
// path 参数定向子目录或白名单目录），结果路径恒相对 workspace 输出。
func walkSearchScoped(workspace, target, pattern, glob string, ignoreDirs []string) (string, error) {
	// splitPathGlob 判定：含 / 的 glob 视为路径形态 → 目录收窄 + basename 匹配。
	if dirPart, filePart := splitPathGlob(glob); dirPart != "" {
		root := target
		if dirPart != "/**" {
			root = filepath.Join(target, filepath.FromSlash(strings.TrimSuffix(dirPart, "/**")))
		}
		return walkSearchBasename(workspace, root, pattern, filePart, ignoreDirs)
	}
	if b, serr := os.Stat(target); serr == nil && !b.IsDir() {
		return walkSearchBasename(workspace, target, pattern, "", ignoreDirs)
	}
	return walkSearch(workspace, target, pattern, glob, ignoreDirs)
}

// workspace 用于结果路径与 ignore 规则基准；target 为实际遍历根（工作区、
// path 参数定向子目录或白名单目录），结果路径恒相对 workspace 输出。
func walkSearch(workspace, target, pattern, glob string, ignoreDirs []string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}
	ign := loadIgnoreMatcher(workspace)
	wsAbs, _ := filepath.Abs(workspace)
	// 遍历根自身豁免忽略判定：path 显式定向到忽略目录（如 frontend/dist）时，
	// 第一回调即根目录，skipDir/ignore 命中会直接 SkipDir 导致恒空结果。
	targetAbs, _ := filepath.Abs(target)
	targetSlash := filepath.ToSlash(targetAbs)
	var b strings.Builder
	count := 0
	truncated := false
	filepath.WalkDir(target, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if count >= maxSearchHits {
			truncated = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			if filepath.ToSlash(path) == targetSlash {
				return nil // 遍历根自身：显式定向豁免忽略规则。
			}
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			if relWs, rerr := filepath.Rel(wsAbs, path); rerr == nil {
				relWs = filepath.ToSlash(relWs)
				// 默认 skipDir 之外，叠加用户 ignore_dirs（名字/路径两种形态）。
				if isUserIgnoredDir(relWs, d.Name(), ignoreDirs) || ign.ignore(relWs, true) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if relWs, rerr := filepath.Rel(wsAbs, path); rerr == nil &&
			ign.ignore(filepath.ToSlash(relWs), false) {
			return nil
		}
		if glob != "" {
			ok, _ := filepath.Match(glob, filepath.Base(path))
			if !ok {
				return nil
			}
		}
		if fi, serr := d.Info(); serr == nil && fi.Size() > maxSearchFileSize {
			return nil // 超大文件跳过（对齐 rg --max-filesize；日志/镜像类非检索目标）。
		}
		// 二进制探测只读文件头 8KB（旧实现 os.ReadFile 整读，300MB 的 exe/zip
		// 也会整个读进内存）；非二进制才继续读全量内容。
		data, rerr := readTextHead(path, binaryProbeBytes)
		if rerr != nil || data == nil {
			return nil // 读失败或二进制文件。
		}
		rel, _ := filepath.Rel(workspace, path)
		rel = filepath.ToSlash(rel)
		sc := bufio.NewScanner(bytesReader(data))
		lineNo := 0
		fileHits := 0
		for sc.Scan() {
			lineNo++
			if re.Match(sc.Bytes()) {
				fmt.Fprintf(&b, "%s:%d: %s\n", rel, lineNo, sc.Text())
				count++
				fileHits++
				if fileHits >= maxHitsPerFile {
					break // 单文件命中过多（生成产物/锁文件），转下一文件保覆盖面。
				}
				if count >= maxSearchHits {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if b.Len() == 0 {
		return "No matches found", nil
	}
	if truncated {
		b.WriteString("... (truncated)\n")
	}
	return b.String(), nil
}

// walkSearchBasename basename 形态的 walkSearch：glob 仅匹配文件名（配合
// splitPathGlob 拆解后的 filePart 使用；目录收窄由调用方通过 root 完成）。
// glob 为空时不过滤（单文件定向场景）。
func walkSearchBasename(workspace, target, pattern, glob string, ignoreDirs []string) (string, error) {
	return walkSearch(workspace, target, pattern, glob, ignoreDirs)
}

// readTextHead 打开文件读前 limit 字节做二进制探测（NUL 字节判定，见 isBinary）；
// 二进制文件返回 (nil, nil) 由调用方跳过，文本文件返回全量内容。
// 避免对 exe/zip/镜像类大文件做全量 ReadFile（旧实现单文件最大读入 300MB+）。
func readTextHead(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, limit)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	head = head[:n]
	if isBinary(head) {
		return nil, nil
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return append(head, rest...), nil
}

// ---------- 公共辅助 ----------

// ReadOnlyWhitelist 只读白名单目录：允许 fs_read / fs_list / fs_search 以只读方式
// 访问工作区之外的白名单目录（绝对路径；写类工具 fs_write/fs_apply_diff 不受此
// 白名单放行，仍严格限定工作区内）。2026-10-01 复盘改造：会话文件存放在
// ~/.kaelvoris/sessions，此前 resolveInWorkspace 严格限制在工作区内，定时复盘
// 会话只能绕道 shell（cmd /c type）读取会话 JSON——绕道多一轮 LLM 往返且
// shell 编码不稳；加入只读白名单后复盘会话可直接 fs_read/fs_list 会话存档。
var ReadOnlyWhitelist = []string{
	sessionsWhitelistDir(),
}

// sessionsWhitelistDir 只读白名单默认项：~/.kaelvoris/sessions（会话存档目录）。
// 运行期解析（而非 init 时硬编码），保证跨平台语义一致。
func sessionsWhitelistDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".kaelvoris", "sessions")
}

// inReadOnlyWhitelist 判定绝对路径是否位于只读白名单目录内。
func inReadOnlyWhitelist(abs string) bool {
	clean := filepath.Clean(abs)
	for _, dir := range ReadOnlyWhitelist {
		if dir == "" {
			continue
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		absDir = filepath.Clean(absDir)
		if clean == absDir || strings.HasPrefix(clean, absDir+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// resolveInWorkspace 把相对路径解析到工作区内，拒绝越界（对齐 getWorkspacePath 安全约束）。
// 绝对路径放行两类：工作区内绝对路径（读/写均放行，Clean 折叠后仍须在 workspace 内）
// 与只读白名单目录（readonly=true 时，ReadOnlyWhitelist 前缀命中）；其余拒绝。
// 写类工具传 readonly=false（resolveWritePath）：白名单仍拒，保持工作区独占语义。
func resolveInWorkspace(workspace, rel string) (string, error) {
	return resolvePathIn(workspace, rel, true)
}

// resolveWritePath 写类工具（fs_write/fs_apply_diff）专用：拒绝一切工作区外路径，
// 只读白名单对写入不生效（白名单目录只读，防会话存档被引擎改写/删除）。
// 白名单记忆文档豁免（2026-10-04 会话复盘）：提示词（prompts.go SESSION MEMORY
// 段）明确要求模型用 fs_write 维护 ~/.kaelvoris/sessions/<id>.memory.md 记忆文档，
// 但写类工具拒绝白名单路径 → 模型每写一次记忆就被拒一次（历史样本 2 例失败后
// 转而堆内容进聊天流）。仅放行 .memory.md 后缀的会话记忆文档，会话 JSON 本体
// 与 .corrupt 隔离文件仍严格只读。
func resolveWritePath(workspace, rel string) (string, error) {
	if abs, err := resolvePathIn(workspace, rel, true); err == nil && inReadOnlyWhitelist(abs) && isMemoryDocPath(abs) {
		return abs, nil
	}
	return resolvePathIn(workspace, rel, false)
}

// isMemoryDocPath 判定路径是否为会话记忆文档（<id>.memory.md）。
func isMemoryDocPath(abs string) bool {
	return strings.HasSuffix(filepath.Clean(abs), memoryDocSuffix)
}

// memoryDocSuffix 会话记忆文档后缀（与 session 包 memorySuffix 对应）。
// tools 包不能 import session（反向依赖），独立声明同值常量。
const memoryDocSuffix = ".memory.md"

// resolvePathIn 路径解析公共实现：workspace 内绝对安全；readonly 放行白名单。
// 2026-10-04 复盘优化：工作区内绝对路径（如 D:\code\gocoder\a.txt 或正斜杠
// 形态）直接放行——此前只查只读白名单，从不检查"本就在工作区内"，跨 5+
// 会话反复误报 escapes workspace，模型起步探索即被阻断。
func resolvePathIn(workspace, rel string, readonly bool) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(rel)))
	if filepath.IsAbs(clean) {
		// 工作区内绝对路径（读/写均放行）：Clean 折叠 .. 与分隔符归一后，
		// 与 workspace（Abs 归一）全等或为其子路径即合法。
		wsAbs, err := filepath.Abs(workspace)
		if err == nil {
			wsAbs = filepath.Clean(wsAbs)
			if clean == wsAbs || strings.HasPrefix(clean, wsAbs+string(os.PathSeparator)) {
				return clean, nil
			}
		}
		// 只读白名单：绝对路径命中白名单目录前缀时放行（仅读类工具）。
		if readonly {
			for _, dir := range ReadOnlyWhitelist {
				if dir == "" {
					continue
				}
				absDir, err := filepath.Abs(dir)
				if err != nil {
					continue
				}
				absDir = filepath.Clean(absDir)
				if clean == absDir || strings.HasPrefix(clean, absDir+string(os.PathSeparator)) {
					return clean, nil
				}
			}
		}
		// 绝对路径未命中工作区与白名单（或写类调用）：明确拒绝（旧实现
		// Join 拼接产生 ws/D:/... 歧义路径且语义混乱；现在报清晰错误，
		// 并提示改用工作区相对路径，替代盲试）。
		return "", fmt.Errorf("path %q escapes workspace (use a path relative to the workspace root for files inside it)", rel)
	}

	rel = filepath.FromSlash(rel)
	abs := filepath.Clean(filepath.Join(workspace, rel))
	ws, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	abs, err = filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	if abs != ws && !strings.HasPrefix(abs, ws+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes workspace", rel)
	}
	return abs, nil
}

// isBinary 简易二进制探测（对齐 isbinary 依赖的用途）。
func isBinary(data []byte) bool {
	limit := 8000
	if len(data) < limit {
		limit = len(data)
	}
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// patternCompileAndSearch 供 SearchFilesTool 的调用入口复用的别名。
func patternCompileAndSearch(workspace, pattern, glob string) (string, error) {
	return walkSearch(workspace, workspace, pattern, glob, nil)
}

func re2Golang(s string) string { return s }
