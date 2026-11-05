// Package tools 学术搜索工具：arxiv_search —— arXiv 论文检索（零依赖，HTTP API）。
//
// 设计约束：
//   - 仅用标准库 net/http 请求 arXiv API（http://export.arxiv.org/api/query，
//     Atom XML 响应），解析 <entry> 节点提取 title/summary/authors/links/categories。
//   - 支持中文摘要查询（search_query 传英文关键词更稳；中文由 LLM 翻译后传入，
//     工具仅如实执行）。
//   - 输出截断至 maxToolOutput。
package tools

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ArxivSearchTool arXiv 学术论文检索。
type ArxivSearchTool struct {
	// Endpoint 覆盖 API 地址（测试注入）。
	Endpoint string
}

func (t *ArxivSearchTool) Name() string { return "arxiv_search" }

func (t *ArxivSearchTool) Description() string {
	return "Search arXiv for academic papers (computer science, math, physics, etc.). " +
		"Returns title, authors, abstract snippet, PDF link and categories. " +
		"Use for literature review, finding papers on a topic, or reading abstracts. " +
		"Query in English works best (translate Chinese keywords to English first)."
}

func (t *ArxivSearchTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"query":       strProp(`Search query, e.g. "large language model agents" or "cat:cs.AI AND attention"`),
		"max_results": numProp("Max results (default 5, cap 10)"),
		"sort_by":     strProp(`Sort: "relevance" (default) / "submittedDate"`),
	}, []string{"query"})
}

// arxivEntry 单篇论文。
type arxivEntry struct {
	Title   string `xml:"title"`
	Summary string `xml:"summary"`
	ID      string `xml:"id"`
	Authors []struct {
		Name string `xml:"name"`
	} `xml:"author"`
	Categories []struct {
		Term string `xml:"term,attr"`
	} `xml:"category"`
	Links []struct {
		Href string `xml:"href,attr"`
	} `xml:"link"`
	Published string `xml:"published"`
}

type arxivFeed struct {
	Entries []arxivEntry `xml:"entry"`
}

func (t *ArxivSearchTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
		SortBy     string `json:"sort_by"`
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
		maxRes = 5
	}
	if maxRes > 10 {
		maxRes = 10
	}
	sortBy := p.SortBy
	if sortBy == "" {
		sortBy = "relevance"
	}
	if sortBy != "relevance" && sortBy != "submittedDate" {
		return "", fmt.Errorf("sort_by must be relevance or submittedDate")
	}

	endpoint := t.Endpoint
	if endpoint == "" {
		endpoint = "http://export.arxiv.org/api/query"
	}
	q := url.Values{}
	q.Set("search_query", p.Query)
	q.Set("max_results", fmt.Sprint(maxRes))
	q.Set("sortBy", sortBy)
	q.Set("sortOrder", "descending")

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "kaelvoris-arxiv/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("arxiv request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("arxiv API %d: %s", resp.StatusCode, clipStr(string(body), 300))
	}

	var feed arxivFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return "", fmt.Errorf("parse arxiv feed: %w", err)
	}
	if len(feed.Entries) == 0 {
		return "no results found for query " + p.Query, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "arxiv search: %q, %d result(s)\n\n", p.Query, len(feed.Entries))
	for i, e := range feed.Entries {
		title := strings.TrimSpace(e.Title)
		title = strings.ReplaceAll(title, "\n", " ")
		title = strings.Join(strings.Fields(title), " ")
		fmt.Fprintf(&b, "%d. %s\n", i+1, title)
		// 作者（前 3 个 + 省略号）。
		names := make([]string, 0, len(e.Authors))
		for _, a := range e.Authors {
			names = append(names, strings.TrimSpace(a.Name))
		}
		if len(names) > 3 {
			fmt.Fprintf(&b, "   authors: %s et al.\n", strings.Join(names[:3], ", "))
		} else if len(names) > 0 {
			fmt.Fprintf(&b, "   authors: %s\n", strings.Join(names, ", "))
		}
		// 分类。
		cats := make([]string, 0, len(e.Categories))
		for _, c := range e.Categories {
			if c.Term != "" {
				cats = append(cats, c.Term)
			}
		}
		if len(cats) > 0 {
			fmt.Fprintf(&b, "   categories: %s\n", strings.Join(cats, ", "))
		}
		// 首选 PDF 链接（link 的 title=pdf 或者 href 含 /pdf/ 路径——arXiv 的
		// PDF 链接是 /pdf/2401.xxxxx 路径形式，无 .pdf 后缀，两种都兼容）。
		pdf := ""
		for _, l := range e.Links {
			if strings.Contains(l.Href, ".pdf") || strings.Contains(l.Href, "/pdf/") {
				pdf = l.Href
				break
			}
		}
		fmt.Fprintf(&b, "   id: %s\n", strings.TrimSpace(e.ID))
		if pdf != "" {
			fmt.Fprintf(&b, "   pdf: %s\n", pdf)
		}
		if t := strings.TrimSpace(e.Published); t != "" {
			fmt.Fprintf(&b, "   published: %s\n", t[:10])
		}
		// 摘要（截断 500 字符）。
		summary := strings.TrimSpace(e.Summary)
		summary = strings.ReplaceAll(summary, "\n", " ")
		summary = strings.Join(strings.Fields(summary), " ")
		if len(summary) > 500 {
			summary = summary[:500] + "..."
		}
		fmt.Fprintf(&b, "   abstract: %s\n\n", summary)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > maxToolOutput {
		out = truncOut(out, maxToolOutput)
	}
	return out, nil
}
