// Package tools Notion 笔记工具：notion_search / notion_read / notion_append。
//
// 使用 Notion REST API（api.notion.com/v1），Bearer Token 认证：
//   - notion_search：搜索工作区笔记（POST /v1/search，query + filter）
//   - notion_read：读取页面/数据库块（GET /v1/blocks/{id}/children）
//   - notion_append：向页面追加块（PATCH /v1/blocks/{id}/children）
//
// 配置：NotionAPIKey（Integration Token，设置页注入）。
// 设计约束：零新增依赖（标准库 net/http），错误透传 HTTP 状态与正文。
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// NotionSettings Notion 配置。
type NotionSettings struct {
	APIKey string // Integration token（Bearer）
}

// NotionTool Notion 笔记工具（search/read/append 三个动作）。
type NotionTool struct {
	// Settings 提供当前配置（nil 或 APIKey 空 = 未启用）。
	Settings func() NotionSettings
}

func (t *NotionTool) Name() string { return "notion" }

func (t *NotionTool) Description() string {
	return "Access Notion notes (search / read / append). " +
		"Search workspace pages and databases, read page content blocks, or append paragraphs to a page. " +
		"Use when the user asks to 查笔记 / 读 Notion / 记录到 Notion. " +
		"Requires a Notion integration token configured in 设置."
}

func (t *NotionTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"action":      strProp(`Action: "search" | "read" | "append"`),
		"query":       strProp(`Search query (for action=search)`),
		"page_id":     strProp(`Notion page/block ID (for action=read/append; 32-hex or with dashes)`),
		"content":     strProp(`Text to append (for action=append; one or more paragraphs, newline-separated)`),
		"max_results": numProp("Max search results (default 5, cap 10)"),
	}, []string{"action"})
}

// notionSearchResult 搜索结果条目。
type notionSearchResult struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Type  string `json:"type"`
	URL   string `json:"url"`
}

// notionBlock 块（读取/追加）。
type notionBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Children int    `json:"children,omitempty"`
}

// notionRichText 富文本。
type notionRichText struct {
	Type string `json:"type"`
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
}

// notionAPI 执行 Notion REST 请求（JSON body；返回状态 + body）。
func (t *NotionTool) notionAPI(ctx context.Context, method, path string, body interface{}) (int, []byte, error) {
	if t.Settings == nil {
		return 0, nil, fmt.Errorf("notion is not configured")
	}
	key := strings.TrimSpace(t.Settings().APIKey)
	if key == "" {
		return 0, nil, fmt.Errorf("notion is not configured: set Notion API Key in 设置")
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	url := "https://api.notion.com/v1/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Notion-Version", "2022-06-28") // 稳定 API 版本
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("notion request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, raw, fmt.Errorf("notion API %d: %s", resp.StatusCode, clipStr(string(raw), 400))
	}
	return resp.StatusCode, raw, nil
}

func (t *NotionTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Action     string `json:"action"`
		Query      string `json:"query"`
		PageID     string `json:"page_id"`
		Content    string `json:"content"`
		MaxResults int    `json:"max_results"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	switch p.Action {
	case "search":
		return t.actionSearch(ctx, p)
	case "read":
		return t.actionRead(ctx, p)
	case "append":
		return t.actionAppend(ctx, p)
	default:
		return "", fmt.Errorf("action must be search/read/append")
	}
}

// actionSearch 搜索工作区。
func (t *NotionTool) actionSearch(ctx context.Context, p struct {
	Action     string `json:"action"`
	Query      string `json:"query"`
	PageID     string `json:"page_id"`
	Content    string `json:"content"`
	MaxResults int    `json:"max_results"`
}) (string, error) {
	maxRes := p.MaxResults
	if maxRes <= 0 {
		maxRes = 5
	}
	if maxRes > 10 {
		maxRes = 10
	}
	body := map[string]interface{}{
		"page_size": maxRes,
	}
	if strings.TrimSpace(p.Query) != "" {
		body["query"] = p.Query
	}
	_, raw, err := t.notionAPI(ctx, "POST", "search", body)
	if err != nil {
		return "", err
	}
	var resp struct {
		Results []struct {
			ID    string           `json:"id"`
			URL   string           `json:"url"`
			Type  string           `json:"object"`
			Title []notionRichText `json:"title"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parse search result: %w", err)
	}
	if len(resp.Results) == 0 {
		return "no Notion pages found" + map[bool]string{true: " for query " + p.Query, false: ""}[strings.TrimSpace(p.Query) != ""], nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "notion search (query=%q): %d result(s)\n\n", p.Query, len(resp.Results))
	for i, r := range resp.Results {
		title := ""
		if len(r.Title) > 0 {
			title = r.Title[0].Text.Content
		}
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "%d. %s\n   id: %s\n   %s\n\n", i+1, title, r.ID, r.URL)
	}
	return strings.TrimSpace(b.String()), nil
}

// actionRead 读取页面/数据库块。
func (t *NotionTool) actionRead(ctx context.Context, p struct {
	Action     string `json:"action"`
	Query      string `json:"query"`
	PageID     string `json:"page_id"`
	Content    string `json:"content"`
	MaxResults int    `json:"max_results"`
}) (string, error) {
	if strings.TrimSpace(p.PageID) == "" {
		return "", fmt.Errorf("page_id is required for action=read")
	}
	id := normalizeBlockID(p.PageID)
	_, raw, err := t.notionAPI(ctx, "GET", "blocks/"+id+"/children", nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Results []struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			HasChild  bool   `json:"has_children"`
			Children  int    `json:"child_count,omitempty"`
			Paragraph struct {
				RichText []notionRichText `json:"rich_text"`
			} `json:"paragraph"`
			Heading1 struct {
				RichText []notionRichText `json:"rich_text"`
			} `json:"heading_1"`
			Heading2 struct {
				RichText []notionRichText `json:"rich_text"`
			} `json:"heading_2"`
			Heading3 struct {
				RichText []notionRichText `json:"rich_text"`
			} `json:"heading_3"`
			BulletedListItem struct {
				RichText []notionRichText `json:"rich_text"`
			} `json:"bulleted_list_item"`
			NumberedListItem struct {
				RichText []notionRichText `json:"rich_text"`
			} `json:"numbered_list_item"`
			Todo struct {
				RichText []notionRichText `json:"rich_text"`
			} `json:"to_do"`
			Code struct {
				RichText []notionRichText `json:"rich_text"`
			} `json:"code"`
			Quote struct {
				RichText []notionRichText `json:"rich_text"`
			} `json:"quote"`
			Callout struct {
				RichText []notionRichText `json:"rich_text"`
			} `json:"callout"`
			Divider map[string]interface{} `json:"divider"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parse blocks: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "notion page %s: %d block(s)\n\n", id, len(resp.Results))
	for i, blk := range resp.Results {
		text := firstRichText(blk.Paragraph.RichText)
		switch blk.Type {
		case "heading_1":
			text = firstRichText(blk.Heading1.RichText)
			fmt.Fprintf(&b, "# %s\n", text)
		case "heading_2":
			text = firstRichText(blk.Heading2.RichText)
			fmt.Fprintf(&b, "## %s\n", text)
		case "heading_3":
			text = firstRichText(blk.Heading3.RichText)
			fmt.Fprintf(&b, "### %s\n", text)
		case "bulleted_list_item":
			text = firstRichText(blk.BulletedListItem.RichText)
			fmt.Fprintf(&b, "- %s\n", text)
		case "numbered_list_item":
			text = firstRichText(blk.NumberedListItem.RichText)
			fmt.Fprintf(&b, "%d. %s\n", i+1, text)
		case "to_do":
			text = firstRichText(blk.Todo.RichText)
			fmt.Fprintf(&b, "- [ ] %s\n", text)
		case "code":
			text = firstRichText(blk.Code.RichText)
			fmt.Fprintf(&b, "```\n%s\n```\n", text)
		case "quote":
			text = firstRichText(blk.Quote.RichText)
			fmt.Fprintf(&b, "> %s\n", text)
		case "callout":
			text = firstRichText(blk.Callout.RichText)
			fmt.Fprintf(&b, "> %s\n", text)
		case "divider":
			fmt.Fprintf(&b, "---\n")
		default:
			if text != "" {
				fmt.Fprintf(&b, "%s\n", text)
			}
		}
		if blk.HasChild {
			fmt.Fprintf(&b, "[+ %d child block(s) — use notion action=read page_id=%s with block id for nested]\n", blk.Children, blk.ID)
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// actionAppend 向页面追加段落块。
func (t *NotionTool) actionAppend(ctx context.Context, p struct {
	Action     string `json:"action"`
	Query      string `json:"query"`
	PageID     string `json:"page_id"`
	Content    string `json:"content"`
	MaxResults int    `json:"max_results"`
}) (string, error) {
	if strings.TrimSpace(p.PageID) == "" {
		return "", fmt.Errorf("page_id is required for action=append")
	}
	if strings.TrimSpace(p.Content) == "" {
		return "", fmt.Errorf("content is required for action=append")
	}
	id := normalizeBlockID(p.PageID)
	var children []map[string]interface{}
	for _, line := range strings.Split(p.Content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		children = append(children, map[string]interface{}{
			"object": "block",
			"type":   "paragraph",
			"paragraph": map[string]interface{}{
				"rich_text": []map[string]interface{}{
					{"type": "text", "text": map[string]interface{}{"content": line}},
				},
			},
		})
	}
	if len(children) == 0 {
		return "", fmt.Errorf("no non-empty content to append")
	}
	_, raw, err := t.notionAPI(ctx, "PATCH", "blocks/"+id+"/children", map[string]interface{}{
		"children": children,
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		Results []map[string]interface{} `json:"results"`
	}
	_ = json.Unmarshal(raw, &resp)
	return fmt.Sprintf("appended %d paragraph(s) to Notion page %s", len(children), id), nil
}

// ---- 辅助 ----

// firstRichText 取第一个富文本的 content。
func firstRichText(rt []notionRichText) string {
	if len(rt) > 0 {
		return rt[0].Text.Content
	}
	return ""
}

// normalizeBlockID 去掉 UUID 连字符并转小写（Notion API 需 32 位 hex）。
func normalizeBlockID(id string) string {
	id = strings.ReplaceAll(strings.TrimSpace(id), "-", "")
	return strings.ToLower(id)
}
