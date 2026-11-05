// Package tools Web 工具面（Claude Code WebSearch/WebFetch 对齐补全）：
//
//	web_fetch  —— 抓取 URL 正文并转为可读文本（HTML→Markdown 简版转换），
//	              弥补 http_request 仅返回原始响应体（HTML 标签噪音大、
//	              无法直接阅读）的短板。支持 markdown/html 明文与 JSON。
//	web_search —— 网页搜索：依次尝试 DuckDuckGo HTML（免 Key）、Brave
//	              Search API（需 BRAVE_API_KEY 环境变量）、SearXNG 实例
//	              （需 SEARXNG_URL 环境变量），全部失败回退提示直接用
//	              web_fetch 抓取搜索引擎结果页。
//
// 设计约束：零新增外部依赖（标准库 net/http + 极简 HTML 标签剥离/实体解码），
// 输出截断至 maxToolOutput，遵守 internal/tools 既有工具协议。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// ===========================================================================
// web_fetch —— 网页抓取 + HTML→Markdown 可读转换
// ===========================================================================

// WebFetchTool 抓取网页正文（HTML→文本/Markdown 简版）。
type WebFetchTool struct{}

func (t *WebFetchTool) Name() string { return "web_fetch" }

func (t *WebFetchTool) Description() string {
	return "Fetch a web page and return readable text (HTML converted to clean markdown-like text). " +
		"Use for reading documentation pages, articles, API responses (JSON passes through) and any " +
		"public URL content. Unlike http_request (raw response preview), this strips scripts/styles/nav " +
		"noise so the content is directly readable. Set raw=true to return the original body instead."
}

func (t *WebFetchTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"url":         strProp("Target URL (http/https)"),
		"raw":         map[string]interface{}{"type": "boolean", "description": "Return original body without HTML-to-text conversion (default false)"},
		"timeout_sec": numProp("Request timeout in seconds (default 20, max 120)"),
		"max_chars":   numProp("Max output characters (default 8000, cap 10000)"),
	}, []string{"url"})
}

func (t *WebFetchTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		URL        string `json:"url"`
		Raw        bool   `json:"raw"`
		TimeoutSec int    `json:"timeout_sec"`
		MaxChars   int    `json:"max_chars"`
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
	timeout := p.TimeoutSec
	if timeout <= 0 {
		timeout = 20
	}
	if timeout > 120 {
		timeout = 120
	}
	maxChars := p.MaxChars
	if maxChars <= 0 {
		maxChars = 8000
	}
	if maxChars > maxToolOutput {
		maxChars = maxToolOutput
	}

	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	// 常规浏览器 UA：部分站点（GitHub/文档站）对无 UA 请求返回 403。
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/json,text/plain;q=0.9,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()
	body := io_ReadLimitString(resp.Body, int64(maxToolOutput*4)) // 原始最多 40KB 参与转换

	var b strings.Builder
	fmt.Fprintf(&b, "url: %s\nstatus: %d %s\ncontent-type: %s\n\n",
		p.URL, resp.StatusCode, http.StatusText(resp.StatusCode), resp.Header.Get("Content-Type"))
	if resp.StatusCode >= 400 {
		b.WriteString("note: HTTP error status; body may be an error page.\n\n")
	}
	if p.Raw || isJSONContentType(resp.Header.Get("Content-Type")) {
		b.WriteString(body)
	} else {
		b.WriteString(htmlToText(body))
	}
	out := strings.TrimSpace(b.String())
	if len(out) > maxChars {
		out = truncOut(out, maxChars)
	}
	return out, nil
}

// ===========================================================================
// web_search —— 网页搜索（多后端自适应）
// ===========================================================================

// WebSearchTool 网页搜索工具。
type WebSearchTool struct{}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Description() string {
	return "Search the web and return a list of results (title, URL, snippet). " +
		"Backends tried in order: DuckDuckGo HTML (no key needed), Brave Search API " +
		"(set BRAVE_API_KEY env), SearXNG instance (set SEARXNG_URL env). " +
		"Follow up with web_fetch on a result URL to read the full page."
}

func (t *WebSearchTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"query":       strProp("Search query text"),
		"max_results": numProp("Max results (default 6, cap 10)"),
		"timeout_sec": numProp("Request timeout in seconds (default 15, max 60)"),
	}, []string{"query"})
}

// searchResult 单条搜索结果。
type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

func (t *WebSearchTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
		TimeoutSec int    `json:"timeout_sec"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	if strings.TrimSpace(p.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	maxRes := p.MaxResults
	if maxRes <= 0 {
		maxRes = 6
	}
	if maxRes > 10 {
		maxRes = 10
	}
	timeout := p.TimeoutSec
	if timeout <= 0 {
		timeout = 15
	}
	if timeout > 60 {
		timeout = 60
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}

	// 后端 0：SearXNG 实例（环境变量 URL；用户显式配置 = 最高优先，
	// 且离线/内网环境免受外网 DDG 超时拖累）。
	if base := osGetenv("SEARXNG_URL"); base != "" {
		if res, err := searchSearXNG(cctx, client, base, p.Query, maxRes); err == nil && len(res) > 0 {
			return renderSearchResults("searxng", res), nil
		}
	}
	// 后端 1：DuckDuckGo HTML（免 Key，正则解析 lite 版结果）。
	if res, err := searchDuckDuckGo(cctx, client, p.Query, maxRes); err == nil && len(res) > 0 {
		return renderSearchResults("duckduckgo", res), nil
	}
	// 后端 2：Brave Search API（环境变量 Key）。
	if key := osGetenv("BRAVE_API_KEY"); key != "" {
		if res, err := searchBrave(cctx, client, key, p.Query, maxRes); err == nil && len(res) > 0 {
			return renderSearchResults("brave", res), nil
		}
	}
	return "no search backend available or no results. " +
		"Fallback: use web_fetch on a search-engine results URL " +
		"(e.g. https://duckduckgo.com/html/?q=" + url.QueryEscape(p.Query) + ")", nil
}

func renderSearchResults(backend string, res []searchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "search backend: %s, %d result(s)\n\n", backend, len(res))
	for i, r := range res {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			sn := r.Snippet
			if len(sn) > 200 {
				sn = sn[:200]
			}
			fmt.Fprintf(&b, "   %s\n", html.UnescapeString(sn))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// searchDuckDuckGo 解析 DuckDuckGo lite HTML（结构稳定：result-link / result-snippet）。
func searchDuckDuckGo(ctx context.Context, client *http.Client, query string, maxRes int) ([]searchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://lite.duckduckgo.com/lite/?q="+url.QueryEscape(query), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/126.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo status %d", resp.StatusCode)
	}
	body := io_ReadLimitString(resp.Body, 1<<20)
	// lite 版结构：<a rel="nofollow" href="URL" class='result-link'>TITLE</a> ... <td class='result-snippet'>SNIP</td>
	linkRe := regexp.MustCompile(`(?s)<a[^>]*class='result-link'[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	snipRe := regexp.MustCompile(`(?s)<td[^>]*class='result-snippet'>\s*(.*?)\s*</td>`)
	var links [][2]string
	for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
		u := html.UnescapeString(m[1])
		if i := strings.Index(u, "uddg="); i >= 0 { // duckduckgo 跳转包装：提取真实 URL
			if q, err := url.QueryUnescape(u[i+5:]); err == nil {
				u = q
			}
		}
		title := strings.TrimSpace(stripTags(m[2]))
		if u == "" || title == "" {
			continue
		}
		links = append(links, [2]string{title, u})
		if len(links) >= maxRes {
			break
		}
	}
	snips := snipRe.FindAllStringSubmatch(body, -1)
	out := make([]searchResult, 0, len(links))
	for i, l := range links {
		r := searchResult{Title: l[0], URL: l[1]}
		if i < len(snips) {
			r.Snippet = strings.TrimSpace(stripTags(snips[i][1]))
		}
		out = append(out, r)
	}
	return out, nil
}

// searchBrave Brave Search API（免费档需 Key；JSON API）。
func searchBrave(ctx context.Context, client *http.Client, key, query string, maxRes int) ([]searchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.search.brave.com/res/v1/web/search?q="+url.QueryEscape(query)+"&count="+fmt.Sprint(maxRes), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave status %d", resp.StatusCode)
	}
	var br struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		return nil, err
	}
	out := make([]searchResult, 0, len(br.Web.Results))
	for _, r := range br.Web.Results {
		if len(out) >= maxRes {
			break
		}
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return out, nil
}

// searchSearXNG 自建/公共 SearXNG 实例（JSON 格式接口）。
func searchSearXNG(ctx context.Context, client *http.Client, baseURL, query string, maxRes int) ([]searchResult, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/search?q=" + url.QueryEscape(query) + "&format=json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng status %d", resp.StatusCode)
	}
	var sx struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sx); err != nil {
		return nil, err
	}
	out := make([]searchResult, 0, len(sx.Results))
	for _, r := range sx.Results {
		if len(out) >= maxRes {
			break
		}
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

// ===========================================================================
// HTML→可读文本 简版转换（无依赖，标签剥离 + 块级换行 + 实体解码）
// ===========================================================================

var (
	// Go RE2 不支持反向引用 \1，script/style 等成对标签逐个剥离（RE2 兼容写法）。
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head|nav|footer|iframe|form)\b[^>]*>[\s\S]*?</(script|style|noscript|svg|head|nav|footer|iframe|form)>`)
	reComments    = regexp.MustCompile(`(?s)<!--.*?-->`)
	reBlockOpen   = regexp.MustCompile(`(?i)</?(p|div|br|li|ul|ol|h[1-6]|tr|table|section|article|blockquote|pre|header|main|td|th)[^>]*>`)
	reTags        = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpaces      = regexp.MustCompile(`[ \t]+`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
)

// htmlToText HTML→可读文本：剥脚本样式导航噪音，块级标签转换行，实体解码。
func htmlToText(s string) string {
	s = reScriptStyle.ReplaceAllString(s, "")
	s = reBlockOpen.ReplaceAllString(s, "\n")
	s = reTags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// 逐行压缩空白（保留列表/标题结构感）。
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(reSpaces.ReplaceAllString(ln, " "))
	}
	out := reBlankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(out)
}

// stripTags 剥除字符串内残余标签并解实体（搜索结果标题/摘要清洗用）。
func stripTags(s string) string {
	s = reTags.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}

func isJSONContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "json")
}

// io_ReadLimitString 读取 reader 至多 n 字节并转字符串（web_fetch 用；
// 独立命名避免与其他工具文件的 io 别名冲突）。
func io_ReadLimitString(r io.Reader, n int64) string {
	b, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil {
		return ""
	}
	return string(b)
}

// osGetenv 环境变量读取薄封装（便于测试注入）。
func osGetenv(key string) string { return os.Getenv(key) }

// truncOut 按字符上限截断并附加提示（与 clampOut 语义一致但可自定义上限）。
func truncOut(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n... (truncated; raise max_chars for more)"
}
