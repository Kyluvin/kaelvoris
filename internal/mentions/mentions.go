// Package mentions @mention 解析：移植自 Zoo-Code src/core/mentions/index.ts +
// src/shared/context-mentions.ts。用户在任务文本中以 @/相对路径、@https://...、
// @git-changes 显式引用上下文；Parse 把 mention 替换为干净引用（'path'）并把
// 对应内容（文件正文 / 目录清单 / git 状态）作为内容块返回，由引擎把内容块
// 附加到首条 user 消息（聊天区仍显示用户原始输入，对齐 Zoo-Code 行为）。
//
// Go regexp（RE2）不支持 lookbehind/lookahead，因此 mentionRegex 的边界语义
// （行首或空白后、非反斜杠转义、尾部单个标点留在正文）用手工状态机复刻。
package mentions

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"kaelvoris/internal/sysproc"
)

// 大小上限（对齐 Zoo-Code extract-text 默认：2000 行 + 行宽 2000 字符）。
const (
	maxFileLines     = 2000
	maxLineRunes     = 2000
	maxFolderEntries = 200
)

// Block 单个 mention 解析出的内容块。
type Block struct {
	Type    string // "file" | "folder" | "git_changes"
	Path    string // 相对路径（或 git_changes 固定 "."）
	Content string // 注入模型的完整块文本
}

// Result 解析结果：Text 为 mention 替换后的正文；Blocks 按出现顺序排列（同
// 一路径多次 mention 只注入一次，对齐 Zoo-Code uniquePaths 去重）。
type Result struct {
	Text   string
	Blocks []Block
}

var (
	urlPrefixRe = regexp.MustCompile(`^\w+://`)
	hexRe       = regexp.MustCompile(`^[a-f0-9]{7,40}$`)
)

// Parse 解析 text 中的 @mentions。text 或 workspace 为空时原样返回。
func Parse(text, workspace string) Result {
	res := Result{Text: text}
	if strings.TrimSpace(text) == "" || strings.TrimSpace(workspace) == "" {
		return res
	}
	var out strings.Builder
	seen := map[string]bool{}
	gitProbed, gitOK := false, false
	i := 0
	for i < len(text) {
		if text[i] != '@' {
			out.WriteByte(text[i])
			i++
			continue
		}
		// 边界：行首或前一字符为空白，且未被反斜杠转义（对齐 (?<=\s)(?<!\\)）。
		if i > 0 {
			prev := text[i-1]
			if prev == '\\' || !isSpaceByte(prev) {
				out.WriteByte(text[i])
				i++
				continue
			}
		}
		kind, val, end := matchMention(text, i)
		if kind == "" {
			out.WriteByte(text[i])
			i++
			continue
		}
		switch kind {
		case "url": // URL 仅替换为引用形态（内容由模型按需用 shell 抓取）
			out.WriteString("'" + val + "'")
		case "path":
			out.WriteString("'" + strings.TrimPrefix(val, "/") + "'")
			if !seen[val] {
				seen[val] = true
				res.Blocks = append(res.Blocks, fileOrFolderBlock(workspace, val))
			}
		case "git-changes":
			if !gitProbed { // 探测一次：成功则全部替换，失败保留原文
				gitProbed = true
				if b, ok := gitChangesBlock(workspace); ok {
					gitOK = true
					res.Blocks = append(res.Blocks, b)
				}
			}
			if gitOK {
				out.WriteString("Workspace Changes (see below)")
			} else {
				out.WriteString("@git-changes")
			}
		default: // problems/terminal/commit-hash：当前环境无对应数据源，保留原文
			out.WriteString("@" + val)
		}
		i = end
	}
	res.Text = out.String()
	return res
}

// Expand 解析并把内容块拼接为投喂模型的完整文本（替换后正文 + 空行 + 块）。
func Expand(text, workspace string) string {
	res := Parse(text, workspace)
	if len(res.Blocks) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(res.Text)
	for _, blk := range res.Blocks {
		b.WriteString("\n\n")
		b.WriteString(blk.Content)
	}
	return b.String()
}

// matchMention 从 text[at] 的 '@' 起匹配一个 mention，返回类别、值与结束下标
// （end 不含留在正文的尾部标点）。kind 为空表示不是 mention。
func matchMention(text string, at int) (kind, val string, end int) {
	j := at + 1
	for j < len(text) {
		if text[j] == '\\' && j+1 < len(text) && text[j+1] == ' ' { // 转义空格：\ 表示路径含空格
			j += 2
			continue
		}
		if isSpaceByte(text[j]) {
			break
		}
		j++
	}
	raw := text[at+1 : j]
	val = strings.ReplaceAll(raw, "\\ ", " ")
	if val == "" {
		return "", "", 0
	}
	// 尾部单个标点留在正文（对齐 (?=[.,;:!?]?(?=[\s\r\n]|$)) lookahead）。
	if last := val[len(val)-1]; last == '.' || last == ',' || last == ';' || last == ':' || last == '!' || last == '?' {
		val = val[:len(val)-1]
		end = j - 1
	} else {
		end = j
	}
	if val == "" {
		return "", "", 0
	}
	switch {
	case urlPrefixRe.MatchString(val) && strings.Index(val, "://")+3 < len(val):
		return "url", val, end
	case val == "problems" || val == "terminal":
		return "unsupported", val, end
	case val == "git-changes":
		return "git-changes", val, end
	case strings.HasPrefix(val, "/"):
		if len(val) > 1 { // @/ 至少需要一个路径字符
			return "path", val, end
		}
		return "", "", 0
	case hexRe.MatchString(val): // commit hash：简版保留原文
		return "unsupported", val, end
	}
	return "", "", 0
}

// fileOrFolderBlock 构建 @/path 的内容块：文件读正文（二进制跳过、行号标注、
// 2000 行截断）；目录列相对路径（最多 200 条）。读取失败注入错误块（对齐
// Zoo-Code 失败形态 `[read_file for 'path']\nError: ...`）。
func fileOrFolderBlock(workspace, mentionPath string) Block {
	rel := strings.TrimPrefix(filepath.ToSlash(mentionPath), "/")
	ws, _ := filepath.Abs(workspace)
	abs, _ := filepath.Abs(filepath.Clean(filepath.Join(workspace, filepath.FromSlash(rel))))
	if abs != ws && !strings.HasPrefix(abs, ws+string(os.PathSeparator)) {
		return Block{Type: "file", Path: rel, Content: fmt.Sprintf("[read_file for '%s']\nError: path escapes workspace", rel)}
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return Block{Type: "file", Path: rel, Content: fmt.Sprintf("[read_file for '%s']\nError: file not found (%v)", rel, err)}
	}
	if fi.IsDir() {
		return Block{Type: "folder", Path: rel, Content: listFolder(ws, rel)}
	}
	return Block{Type: "file", Path: rel, Content: readFileContent(abs, rel)}
}

// readFileContent 读文件正文：二进制跳过；行号标注（对齐 fs_read 输出形态）；
// 超过 2000 行截断并提示用 read_file 续读（对齐 Zoo-Code formatFileReadResult）。
func readFileContent(abs, rel string) string {
	data, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Sprintf("[read_file for '%s']\nError: %v", rel, err)
	}
	if isBinaryData(data) {
		return fmt.Sprintf("[read_file for '%s']\n(binary file, content omitted)", rel)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[read_file for '%s']", rel)
	total := 0
	start := 0
	for start < len(data) {
		var line string
		if nl := bytes.IndexByte(data[start:], '\n'); nl < 0 {
			line = string(data[start:])
			start = len(data)
		} else {
			line = string(data[start : start+nl])
			start += nl + 1
		}
		total++
		if total <= maxFileLines {
			fmt.Fprintf(&b, "\n%6d|\t%s", total, truncateRunes(line, maxLineRunes))
		}
	}
	if total > maxFileLines {
		fmt.Fprintf(&b, "\n\n[IMPORTANT: File content truncated. Status: Showing lines 1-%d of %d total lines. To read more: Use the read_file tool with offset=%d and limit=%d.]",
			maxFileLines, total, maxFileLines+1, maxFileLines)
	}
	return b.String()
}

// listFolder 列目录相对路径（跳过 .git/node_modules，最多 200 条）。
func listFolder(workspace, rel string) string {
	root := filepath.Join(workspace, filepath.FromSlash(rel))
	var entries []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == root {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules":
				return fs.SkipDir
			}
		}
		if len(entries) >= maxFolderEntries {
			return fs.SkipAll
		}
		if r, err := filepath.Rel(root, p); err == nil {
			entries = append(entries, filepath.ToSlash(filepath.Join(rel, r)))
		}
		return nil
	})
	var b strings.Builder
	fmt.Fprintf(&b, "[list_files for '%s']", rel)
	if len(entries) == 0 {
		b.WriteString("\n(empty directory)")
	}
	for _, e := range entries {
		b.WriteString("\n" + e)
	}
	if len(entries) >= maxFolderEntries {
		fmt.Fprintf(&b, "\n\n[Truncation: showing first %d entries. Use the fs_list tool for the full listing.]", maxFolderEntries)
	}
	return b.String()
}

// gitChangesBlock 构建 @git-changes 的内容块（git status --porcelain 简版，
// 对齐 Zoo-Code git_changes 块的"变更文件清单"语义）。非 git 仓库或工作区
// 干净时不注入（mention 保留原文本）。
func gitChangesBlock(workspace string) (Block, bool) {
	cmd := exec.Command("git", "-C", workspace, "-c", "core.quotepath=false", "status", "--porcelain")
	sysproc.Hide(cmd) // GUI 进程下隐藏控制台黑框。
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return Block{}, false
	}
	out := strings.TrimRight(stdout.String(), "\n")
	if strings.TrimSpace(out) == "" {
		return Block{}, false
	}
	return Block{Type: "git_changes", Path: ".", Content: "[git status]\n" + out}, true
}

// isBinaryData 简易二进制探测（与 tools.isBinary 同语义：前 8000 字节含 \0）。
func isBinaryData(data []byte) bool {
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

// truncateRunes 按字符数截断单行（对齐 extract-text 行宽 2000）。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
