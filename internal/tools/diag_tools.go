// Package tools 系统诊断工具（会话复盘新增，弥补三大短板）：
//
//	process_list  —— 进程/端口/命令行诊断（会话中反复用 Get-CimInstance /
//	                netstat / tasklist 拼 PowerShell 管道查 PID/端口/命令行，
//	                且屡遭 $ 转义吞失；纯 Go 实现零转义问题）。
//	file_locker   —— 文件占用者检测（_sessions_digest.txt 被占用导致
//	                fs_read/copy/move 全部失败，反复试错 20+ 次无解；
//	                Windows 需枚举进程句柄表定位持锁进程）。
//	http_request  —— HTTP 探测/健康检查（健康检查、公网隧道验证、
//	                下载完整性校验高频出现；GET/HEAD + 延迟 + 摘要输出）。
//
// 设计约束（对齐项目编码规范）：
//   - 零新增外部依赖（golang.org/x/sys 已在依赖树，直接引用其 windows 包）；
//   - Windows 优先，非 Windows 平台优雅降级（file_locker 提示不支持）；
//   - 输出截断至 10000 字符（与 shell 工具一致）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- 跨平台分发约定 ----------
// 平台专属实现收敛在带构建标签的文件里（diag_windows.go / diag_other.go），
// 各自提供同名入口 listProcs / collectPorts / findFileLockers；
// 本文件不掺入 runtime.GOOS 分支，避免非 Windows 编译引用缺失。

// maxToolOutput 工具输出截断上限（对齐 shell 工具）。
const maxToolOutput = 10000

// clampOut 按上限截断输出（头尾保留，对标 mini-swe-agent observation_template；
// 错误汇总/堆栈末帧多在尾部，只留头部会吞关键信息）。
func clampOut(s string) string {
	if len(s) > maxToolOutput {
		return truncateMiddle(s, maxToolOutput)
	}
	return s
}

// ===========================================================================
// process_list —— 进程 / 端口 / 命令行诊断
// ===========================================================================

// ProcInfo 单个进程快照。
type ProcInfo struct {
	PID         int      `json:"pid"`
	PPID        int      `json:"ppid,omitempty"`
	Name        string   `json:"name"`
	CmdLine     string   `json:"cmdline,omitempty"`
	ExePath     string   `json:"exe,omitempty"`
	Ports       []string `json:"ports,omitempty"` // LISTENING 端口（含 IP:port）
	Established []string `json:"established,omitempty"`
}

// ProcessListTool 进程/端口诊断工具。
type ProcessListTool struct{}

func (t *ProcessListTool) Name() string { return "process_list" }

func (t *ProcessListTool) Description() string {
	return "List running processes with PID, parent PID, name, command line and listening/established " +
		"ports. Use to diagnose port conflicts, find which process holds a port, check whether a " +
		"service/binary is actually running, or inspect how a process was launched. " +
		"Filter by process name substring and/or listening port to narrow results."
}

func (t *ProcessListTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"name":  strProp("Optional: filter processes whose name contains this substring (case-insensitive)"),
		"port":  numProp("Optional: only show processes that listen on / connect via this local port"),
		"limit": numProp("Optional: max processes returned (default 50, cap 200)"),
	}, nil)
}

func (t *ProcessListTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Name  string `json:"name"`
		Port  int    `json:"port"`
		Limit int    `json:"limit"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	procs, err := listProcs("")
	if err != nil {
		return "", err
	}
	// 端口过滤：命中端口的 PID 集合。
	var portPids map[int]bool
	var portLines map[int][]string
	if p.Port > 0 {
		portPids = map[int]bool{}
		portLines = map[int][]string{}
		collectPorts(p.Port, portPids, portLines)
	}
	var out []ProcInfo
	for _, pr := range procs {
		if portPids != nil && !portPids[pr.PID] {
			continue
		}
		if p.Name != "" && !containsFold(pr.Name, p.Name) && !containsFold(pr.CmdLine, p.Name) {
			continue
		}
		if portLines != nil {
			pr.Ports = portLines[pr.PID]
		}
		out = append(out, pr)
		if len(out) >= limit {
			break
		}
	}
	if len(out) == 0 {
		return "no matching processes found", nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d matching process(es):\n\n", len(out)))
	for _, pr := range out {
		fmt.Fprintf(&b, "PID %d  %s\n", pr.PID, pr.Name)
		if pr.PPID > 0 {
			fmt.Fprintf(&b, "  ppid: %d\n", pr.PPID)
		}
		if pr.ExePath != "" {
			fmt.Fprintf(&b, "  exe: %s\n", pr.ExePath)
		}
		if pr.CmdLine != "" {
			fmt.Fprintf(&b, "  cmdline: %s\n", pr.CmdLine)
		}
		if len(pr.Ports) > 0 {
			fmt.Fprintf(&b, "  ports: %s\n", strings.Join(pr.Ports, "  "))
		}
		b.WriteString("\n")
	}
	return clampOut(b.String()), nil
}

func containsFold(s, sub string) bool {
	return len(sub) == 0 ||
		(len(s) >= len(sub) && indexOfFold(s, sub) >= 0)
}

func indexOfFold(s, sub string) int {
	ls, lsub := len(s), len(sub)
	for i := 0; i+lsub <= ls; i++ {
		if equalFoldASCII(s[i:i+lsub], sub) {
			return i
		}
	}
	return -1
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// ===========================================================================
// file_locker —— 文件占用者检测
// ===========================================================================

// FileLockerTool 检测文件被哪个进程占用/锁定。
type FileLockerTool struct{}

func (t *FileLockerTool) Name() string { return "file_locker" }

func (t *FileLockerTool) Description() string {
	return "Find which processes are holding/locking a file open (classic 'file in use by another " +
		"process' diagnosis). Windows: probes the file with share-mode opens and enumerates " +
		"handle info to identify lockers; also reports whether a plain re-open would succeed. " +
		"Pass an absolute path or a path relative to the workspace root."
}

func (t *FileLockerTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"path": strProp("File path (absolute, or relative to workspace root)"),
	}, []string{"path"})
}

func (t *FileLockerTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(p.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	abs := p.Path
	if !filepath.IsAbs(abs) {
		wd, _ := os.Getwd()
		abs = filepath.Join(wd, p.Path)
	}
	abs = filepath.Clean(abs)

	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", abs, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "file: %s\nsize: %d bytes\nmodified: %s\n\n", abs, fi.Size(), fi.ModTime().Format("2006-01-02 15:04:05"))

	lockers := findFileLockers(abs)
	if len(lockers) == 0 {
		// 无句柄级锁定者，再做一次写打开探测（独占语义）。
		if err := probeWriteOpen(abs); err != nil {
			fmt.Fprintf(&b, "no handle-level locker detected, but the file cannot be opened for write: %v\n", err)
			fmt.Fprintf(&b, "likely causes: sharing violation by a reader that opened without FILE_SHARE_WRITE, or read-only attribute.\n")
		} else {
			b.WriteString("no locker detected: file is openable for write (not locked).\n")
		}
		return b.String(), nil
	}
	fmt.Fprintf(&b, "locked by %d process(es):\n\n", len(lockers))
	for _, l := range lockers {
		fmt.Fprintf(&b, "PID %d  %s\n  cmdline: %s\n\n", l.PID, l.Name, l.CmdLine)
	}
	b.WriteString("hint: stop the listed process(es) or close the file handle before overwriting/moving the file.\n")
	return b.String(), nil
}

// probeWriteOpen 用 FILE_SHARE_READ|WRITE|DELETE 语义探测可写打开。
func probeWriteOpen(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_ = f.Close()
	return nil
}

// Locker 进程占用信息。
type Locker struct {
	PID     int
	Name    string
	CmdLine string
}

// ===========================================================================
// http_request —— HTTP 探测 / 健康检查 / 下载校验
// ===========================================================================

// HTTPRequestTool 轻量 HTTP 客户端工具。
type HTTPRequestTool struct{}

func (t *HTTPRequestTool) Name() string { return "http_request" }

func (t *HTTPRequestTool) Description() string {
	return "Perform an HTTP request (GET/HEAD/POST) and return status, latency, response headers " +
		"and a body preview. Use for health checks (/api/health), verifying public URLs/tunnels, " +
		"checking download links (HEAD gives Content-Length without downloading), and probing " +
		"local services. Body preview is capped; set body=false to skip it."
}

func (t *HTTPRequestTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"url":         strProp("Target URL (http/https)"),
		"method":      strProp("HTTP method: GET (default), HEAD, POST, PUT, DELETE"),
		"headers":     map[string]interface{}{"type": "object", "description": "Optional headers map", "additionalProperties": map[string]interface{}{"type": "string"}},
		"body":        strProp("Optional request body (string)"),
		"timeout_sec": numProp("Request timeout in seconds (default 15, max 120)"),
		"max_body":    numProp("Max body bytes to return in preview (default 2000, cap 10000)"),
	}, []string{"url"})
}

func (t *HTTPRequestTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		URL        string            `json:"url"`
		Method     string            `json:"method"`
		Headers    map[string]string `json:"headers"`
		Body       string            `json:"body"`
		TimeoutSec int               `json:"timeout_sec"`
		MaxBody    int               `json:"max_body"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	if strings.TrimSpace(p.URL) == "" {
		return "", fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
		return "", fmt.Errorf("url must start with http:// or https://")
	}
	method := strings.ToUpper(strings.TrimSpace(p.Method))
	if method == "" {
		method = http.MethodGet
	}
	timeout := p.TimeoutSec
	if timeout <= 0 {
		timeout = 15
	}
	if timeout > 120 {
		timeout = 120
	}
	maxBody := p.MaxBody
	if maxBody <= 0 {
		maxBody = 2000
	}
	if maxBody > maxToolOutput {
		maxBody = maxToolOutput
	}

	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	var bodyReader io.Reader
	if p.Body != "" {
		bodyReader = strings.NewReader(p.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.URL, bodyReader)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// 只读 maxBody+1 字节判断是否还有更多。
	limited := io.LimitReader(resp.Body, int64(maxBody)+1)
	bodyBytes, readErr := io.ReadAll(limited)
	readMore := len(bodyBytes) > maxBody // 再多读 1 字节即知还有剩余
	truncated := readMore
	if truncated {
		bodyBytes = bodyBytes[:maxBody]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "status: %d %s\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	fmt.Fprintf(&b, "latency: %s\n", time.Since(started).Round(time.Millisecond))
	fmt.Fprintf(&b, "content-type: %s\n", resp.Header.Get("Content-Type"))
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		fmt.Fprintf(&b, "content-length: %s\n", cl)
	}
	fmt.Fprintf(&b, "content-type response ok, %d header(s)\n", len(resp.Header))
	// 关键响应头
	for _, h := range []string{"Server", "Location", "Content-Disposition"} {
		if v := resp.Header.Get(h); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", h, v)
		}
	}
	if len(bodyBytes) > 0 {
		b.WriteString("\n--- body ---\n")
		if isBinary(bodyBytes) {
			b.WriteString("(binary body, " + strconv.Itoa(len(bodyBytes)) + " bytes, content omitted)")
		} else {
			b.WriteString(string(bodyBytes))
			if truncated || readMore {
				b.WriteString("\n... (body truncated at max_body; raise max_body for more)")
			}
		}
	} else {
		b.WriteString("\n(empty body)")
	}
	if readErr != nil {
		b.WriteString("\n(body read incomplete: " + readErr.Error() + ")")
	}
	return b.String(), nil
}
