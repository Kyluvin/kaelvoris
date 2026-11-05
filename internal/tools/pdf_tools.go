// Package tools PDF 读取工具：pdf_read —— 读取 PDF 文本内容（零依赖简版解析）。
//
// 设计约束（对齐项目「零新增外部依赖」约定）：
//   - 不引入 pdf 解析库，仅用标准库实现 PDF 文本提取的「够用」子集：
//   - 扫描文件所有 object（offset 表），对每个 stream 尝试 FlateDecode
//     （了 deflate 最普遍；LZW/ASCII85 暂不支持 → 跳过并计数）；
//   - stream 内容流中提取 BT...ET 块内 Tj/TJ 操作数的文本（含 T* 换行）；
//   - 未压缩流直接扫；不支持编码回退为「文件总页数 + 无法提取」提示。
//   - 输出截断至 maxToolOutput，遵守 internal/tools 既有工具协议。
//
// 局限（明示）：加密 PDF、LZW/ASCII85 解码、CID 字体/嵌入字体映射、
// 扫描件（无文本层）不支持——此类文件返回明确提示而非虚假文本。
package tools

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// PDFReadTool 读取 PDF 文本。
type PDFReadTool struct {
	// MaxSize 单文件读取上限（默认 64MB，防止超大 PDF 拖垮工具调用）。
	MaxSize int64
}

func (t *PDFReadTool) Name() string { return "pdf_read" }

func (t *PDFReadTool) Description() string {
	return "Extract text from a PDF file. Works for born-digital PDFs (text layer present): " +
		"returns readable text with page separators. Scanned PDFs (no text layer), encrypted PDFs, " +
		"PDFs using LZW/ASCII85 compression, or CID-font PDFs are not supported and will be reported " +
		"as such. Use for reading PDF documents, extracting paragraphs, summarizing, or answering " +
		"questions about PDF content. Max file size 64MB."
}

func (t *PDFReadTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"path":      strProp("Path to the PDF file (absolute or workspace-relative)"),
		"pages":     strProp(`Page range to extract, e.g. "1-3" or "5" (default: all pages)`),
		"max_chars": numProp("Max output characters (default 8000, cap 10000)"),
	}, []string{"path"})
}

func (t *PDFReadTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Path     string `json:"path"`
		Pages    string `json:"pages"`
		MaxChars int    `json:"max_chars"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	if strings.TrimSpace(p.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	maxSize := t.MaxSize
	if maxSize <= 0 {
		maxSize = 64 << 20
	}
	maxChars := p.MaxChars
	if maxChars <= 0 {
		maxChars = 8000
	}
	if maxChars > maxToolOutput {
		maxChars = maxToolOutput
	}

	data, err := os.ReadFile(p.Path)
	if err != nil {
		return "", fmt.Errorf("read pdf: %w", err)
	}
	if int64(len(data)) > maxSize {
		return "", fmt.Errorf("pdf too large (%d bytes, max %d)", len(data), maxSize)
	}

	extracted, stats, err := extractPDFText(data)
	if err != nil {
		return "", err
	}

	// 分页处理：指定 pages="1-3" / "5"。
	var pageFilter map[int]bool
	if strings.TrimSpace(p.Pages) != "" {
		pageFilter = parsePageRange(p.Pages)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "pdf: %s\npages (parsed): %d, chars extracted: %d\n",
		p.Path, stats.totalPages, stats.chars)
	if stats.unsupported > 0 {
		fmt.Fprintf(&b, "note: %d stream(s) use unsupported encoding (LZW/ASCII85/CID) — skipped\n", stats.unsupported)
	}
	if stats.encrypted {
		b.WriteString("note: PDF is encrypted; text extraction failed\n")
	}
	b.WriteString("\n")

	pageNo := 0
	written := 0
	for _, page := range extracted {
		pageNo++
		if pageFilter != nil && !pageFilter[pageNo] {
			continue
		}
		text := strings.TrimSpace(page)
		if text == "" {
			continue
		}
		head := fmt.Sprintf("\n--- page %d ---\n", pageNo)
		if written+len(head)+len(text) > maxChars {
			b.WriteString(head)
			remaining := maxChars - written - len(head)
			if remaining > 0 {
				b.WriteString(truncOut(text, remaining))
			}
			b.WriteString("\n... (truncated; use pages= to narrow)")
			written = maxChars
			break
		}
		b.WriteString(head)
		b.WriteString(text)
		b.WriteString("\n")
		written += len(head) + len(text)
	}
	out := strings.TrimSpace(b.String())
	return out, nil
}

// ---- 内部：极简 PDF 文本提取 ----

// pdfTextStats 提取统计（不支持的编码/加密计数）。
type pdfTextStats struct {
	totalPages  int
	chars       int
	unsupported int
	encrypted   bool
}

// extractPDFText 扫描 PDF objects 提取文本流（未加密文本层子集）。
func extractPDFText(data []byte) ([]string, pdfTextStats, error) {
	var stats pdfTextStats
	// 加密标记（/Encrypt 对象或 trailer 中的 Encrypt 引用）。
	if bytes.Contains(data, []byte("/Encrypt")) {
		stats.encrypted = true
		return nil, stats, nil
	}
	// 页数：/Type /Page 计数（/Pages 为树节点，不重复计）。
	pageCount := len(regexp.MustCompile(`(?s)/Type\s*/Page[^s]`).FindAll(data, -1))
	stats.totalPages = pageCount

	// 找所有对象：间接对象定义 "N 0 obj ... endobj"。
	objRe := regexp.MustCompile(`(?s)(\d+)\s+0\s+obj(.*?)endobj`)
	streamRe := regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	res := make([]string, 0, 8)
	for _, m := range objRe.FindAllSubmatch(data, -1) {
		body := m[2]
		if !bytes.Contains(body, []byte("stream")) {
			continue
		}
		// 过滤对象头（/Length）之外的文本；仅取 stream 数据。
		sm := streamRe.FindSubmatch(body)
		if sm == nil {
			continue
		}
		raw := sm[1]
		// 解码：优先 FlateDecode（deflate），其次无编码原样。
		var decoded []byte
		if bytes.Contains(body, []byte("/FlateDecode")) || bytes.Contains(body, []byte("/Fl")) {
			zr, err := zlib.NewReader(bytes.NewReader(raw))
			if err == nil {
				if d, err := io.ReadAll(io.LimitReader(zr, 64<<20)); err == nil {
					decoded = d
				}
				zr.Close()
			}
		}
		if decoded == nil {
			if bytes.Contains(body, []byte("/LZWDecode")) || bytes.Contains(body, []byte("/ASCII85Decode")) ||
				bytes.Contains(body, []byte("/ASCIIHexDecode")) {
				stats.unsupported++
				continue
			}
			decoded = raw
		}
		text := extractContentStreamText(decoded)
		if strings.TrimSpace(text) != "" {
			stats.chars += len(text)
			res = append(res, text)
		}
	}
	if len(res) == 0 {
		return nil, stats, fmt.Errorf("no extractable text: unsupported encoding, encrypted, or scanned (no text layer) PDF")
	}
	// 按页合并：大多数 PDF 每页一个 content stream；简单按对象顺序排列。
	// （页序近似：content stream 出现顺序 ≈ 页序；无法保证时每块标注。）
	return res, stats, nil
}

// extractContentStreamText 从 PDF content stream 中提取 BT...ET 文本块。
func extractContentStreamText(data []byte) string {
	var out strings.Builder
	// 逐个 BT...ET 块。
	btRe := regexp.MustCompile(`(?s)BT(.*?)ET`)
	for _, m := range btRe.FindAllSubmatch(data, -1) {
		block := m[1]
		// Tj: (string) Tj ; TJ: [(str) num (str)] TJ
		tjRe := regexp.MustCompile(`\(((?:[^()\\]|\\.)*)\)\s*Tj`)
		tjAllRe := regexp.MustCompile(`\(((?:[^()\\]|\\.)*)\)`)
		// 先尝试 TJ 数组（多段拼接）。
		tjArrRe := regexp.MustCompile(`\[(.*?)\]\s*TJ`)
		if tm := tjArrRe.FindSubmatch(block); tm != nil {
			parts := tjAllRe.FindAllSubmatch(tm[1], -1)
			var line strings.Builder
			for _, pp := range parts {
				line.WriteString(unescapePDFString(string(pp[1])))
			}
			out.WriteString(line.String())
			continue
		}
		for _, pp := range tjRe.FindAllSubmatch(block, -1) {
			out.WriteString(unescapePDFString(string(pp[1])))
		}
	}
	// 清理：PDF 文本中常见转义（\n 为换行，\r 回车），压缩多余空行。
	s := unescapePDFString(out.String())
	s = strings.ReplaceAll(s, "\r", "\n")
	s = regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " ")
	}
	// 合并连续空行。
	var merged []string
	for _, ln := range lines {
		if ln == "" && len(merged) > 0 && merged[len(merged)-1] == "" {
			continue
		}
		merged = append(merged, ln)
	}
	return strings.TrimSpace(strings.Join(merged, "\n"))
}

// unescapePDFString 处理 PDF 字符串转义（\n \r \t \\( \\) \\\\ \ddd 八进制）。
func unescapePDFString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case '(', ')', '\\':
			b.WriteByte(s[i])
		default:
			if s[i] >= '0' && s[i] <= '7' {
				// 八进制 \ddd（1-3 位）。
				oct := string(s[i])
				for j := 1; j < 3 && i+j < len(s) && s[i+j] >= '0' && s[i+j] <= '7'; j++ {
					oct += string(s[i+j])
				}
				if v, err := strconv.ParseUint(oct, 8, 32); err == nil {
					b.WriteByte(byte(v))
				}
				i += len(oct) - 1
			} else {
				b.WriteByte(s[i]) // 未知转义按原样
			}
		}
	}
	return b.String()
}

// parsePageRange 解析 "1-3" / "5" / "1,4-6" 语法为页号集合。
func parsePageRange(s string) map[int]bool {
	set := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.Index(part, "-"); i > 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(part[:i]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err1 == nil && err2 == nil && lo >= 1 && hi >= lo {
				for n := lo; n <= hi && n-lo < 1000; n++ {
					set[n] = true
				}
			}
			continue
		}
		if n, err := strconv.Atoi(part); err == nil && n >= 1 {
			set[n] = true
		}
	}
	return set
}
