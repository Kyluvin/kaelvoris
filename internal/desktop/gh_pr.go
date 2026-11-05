// gh_pr.go —— PR/CI 集成（Wails 绑定，对齐 Claude Code cloud→GitHub PR 与
// GitHub Actions /claude 工作流的桌面端能力位）：
//
//	GhAvailable —— gh CLI 探测（未安装时前端展示安装指引而非报错）
//	GhPRList    —— 当前仓库 PR 列表（标题/编号/状态/CI 汇总）
//	GhPRCreate  —— 从当前分支创建 PR（title/body/draft/base）
//	GhPRView    —— PR 详情 + CI 检查矩阵
//	GhPRMerge   —— 合并 PR（merge/squash/rebase）
//
// 全部经 `gh` 子命令参数化执行（exec.Command 直跑，无 shell 注入面）；
// gh 未登录等错误透出 stderr 指引。gh 探测失败属预期降级（前端隐藏 PR 区）。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"kaelvoris/internal/sysproc"
)

// ghBin gh CLI 可执行路径（懒探测缓存；"" = 未安装/不可用）。
var (
	ghBin    string
	ghProbed bool
)

// ghPath 探测 gh CLI（PATH 查找；进程内缓存一次）。
func ghPath() string {
	if !ghProbed {
		ghProbed = true
		ghBin, _ = exec.LookPath("gh")
	}
	return ghBin
}

// GhAvailable gh CLI 是否可用（前端决定 PR 分区显隐 + 安装指引）。
func (a *App) GhAvailable() bool { return ghPath() != "" }

// runGh 执行 gh 子命令（工作区目录；超时 30s；错误带 stderr）。
func runGh(ws string, args ...string) (string, error) {
	bin := ghPath()
	if bin == "" {
		return "", fmt.Errorf("gh CLI 未安装（https://cli.github.com/ 安装并 `gh auth login` 后使用）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = ws
	sysproc.Hide(cmd)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", args[0], msg)
	}
	return out.String(), nil
}

// GhCheckInfo 单条 CI 检查。
type GhCheckInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`               // QUEUED / IN_PROGRESS / COMPLETED
	Concl  string `json:"conclusion,omitempty"` // SUCCESS / FAILURE / …
}

// GhPRInfo PR 列表条目（gh pr list --json 字段子集）。
type GhPRInfo struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"` // OPEN / MERGED / CLOSED
	HeadRef    string `json:"headRefName"`
	Author     string `json:"author"`
	IsDraft    bool   `json:"isDraft"`
	CheckState string `json:"checkState,omitempty"` // CI 汇总：success/failure/pending/""（无检查）
	URL        string `json:"url,omitempty"`
}

// summarizeRollup CI 汇总：任一失败类 → failure；任一进行中 → pending；
// 全部成功 → success；无检查 → ""。失败类结论含 FAILURE/TIMED_OUT/CANCELLED/STARTUP_FAILURE。
func summarizeRollup(rollup []ghRollupEntry) string {
	if len(rollup) == 0 {
		return ""
	}
	anyFail, anyPending := false, false
	for _, c := range rollup {
		switch strings.ToUpper(c.Conclusion) {
		case "FAILURE", "TIMED_OUT", "CANCELLED", "STARTUP_FAILURE", "ACTION_REQUIRED":
			anyFail = true
		case "":
			if !strings.EqualFold(c.Status, "COMPLETED") {
				anyPending = true
			}
		}
		if anyFail {
			break
		}
	}
	switch {
	case anyFail:
		return "failure"
	case anyPending:
		return "pending"
	default:
		return "success"
	}
}

// ghRollupEntry statusCheckRollup 通用字段（CheckRun / CheckContext 并集）。
type ghRollupEntry struct {
	Name       string `json:"name,omitempty"`
	Workflow   string `json:"workflow,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
}

// GhPRList 当前仓库 PR 列表（state: OPEN 默认 / ALL / MERGED / CLOSED）。
func (a *App) GhPRList(state string) ([]GhPRInfo, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return nil, msgWsEmpty()
	}
	if state == "" {
		state = "OPEN"
	}
	out, err := runGh(ws, "pr", "list", "--state", state, "--limit", "30",
		"--json", "number,title,state,headRefName,author,isDraft,statusCheckRollup,url")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		State   string `json:"state"`
		HeadRef string `json:"headRefName"`
		Author  struct {
			Login string `json:"login"`
		} `json:"author"`
		IsDraft bool            `json:"isDraft"`
		Rollup  []ghRollupEntry `json:"statusCheckRollup"`
		URL     string          `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("gh 输出解析失败: %w", err)
	}
	list := make([]GhPRInfo, 0, len(raw))
	for _, r := range raw {
		list = append(list, GhPRInfo{
			Number: r.Number, Title: r.Title, State: r.State,
			HeadRef: r.HeadRef, Author: r.Author.Login, IsDraft: r.IsDraft,
			CheckState: summarizeRollup(r.Rollup), URL: r.URL,
		})
	}
	return list, nil
}

// GhPRDetail PR 详情 + CI 检查矩阵。
type GhPRDetail struct {
	Number     int           `json:"number"`
	Title      string        `json:"title"`
	State      string        `json:"state"`
	Body       string        `json:"body,omitempty"`
	HeadRef    string        `json:"headRefName"`
	BaseRef    string        `json:"baseRefName"`
	Author     string        `json:"author"`
	URL        string        `json:"url,omitempty"`
	Mergeable  string        `json:"mergeable,omitempty"` // MERGEABLE / CONFLICTING / UNKNOWN
	Review     string        `json:"reviewDecision,omitempty"`
	Checks     []GhCheckInfo `json:"checks,omitempty"`
	CheckState string        `json:"checkState,omitempty"`
}

// GhPRView 读取 PR 详情（含 CI 检查矩阵）。
func (a *App) GhPRView(number int) (*GhPRDetail, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return nil, msgWsEmpty()
	}
	out, err := runGh(ws, "pr", "view", fmt.Sprint(number), "--json",
		"number,title,state,body,headRefName,baseRefName,author,url,mergeable,reviewDecision,statusCheckRollup")
	if err != nil {
		return nil, err
	}
	var raw struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		State       string `json:"state"`
		Body        string `json:"body"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
		URL            string          `json:"url"`
		Mergeable      string          `json:"mergeable"`
		ReviewDecision string          `json:"reviewDecision"`
		Rollup         []ghRollupEntry `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("gh 输出解析失败: %w", err)
	}
	checks := make([]GhCheckInfo, 0, len(raw.Rollup))
	for _, c := range raw.Rollup {
		name := c.Name
		if name == "" {
			name = c.Workflow
		}
		if name == "" {
			name = c.Bucket
		}
		checks = append(checks, GhCheckInfo{Name: name, Status: c.Status, Concl: c.Conclusion})
	}
	return &GhPRDetail{
		Number: raw.Number, Title: raw.Title, State: raw.State, Body: raw.Body,
		HeadRef: raw.HeadRefName, BaseRef: raw.BaseRefName, Author: raw.Author.Login,
		URL: raw.URL, Mergeable: raw.Mergeable, Review: raw.ReviewDecision,
		Checks: checks, CheckState: summarizeRollup(raw.Rollup),
	}, nil
}

// GhPRCreate 从当前分支创建 PR。
// title 必填；body 可空；draft = 草稿 PR；base 空 = 仓库默认分支。
func (a *App) GhPRCreate(title, body string, draft bool, base string) (*GhPRInfo, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return nil, msgWsEmpty()
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("PR 标题不能为空")
	}
	args := []string{"pr", "create", "--title", title, "--body", body}
	if draft {
		args = append(args, "--draft")
	}
	if b := strings.TrimSpace(base); b != "" {
		args = append(args, "--base", b)
	}
	out, err := runGh(ws, args...)
	if err != nil {
		return nil, err
	}
	// gh 输出为 PR URL（ Creating pull request... \n <url>）；提取编号回查详情。
	url := ""
	for _, ln := range strings.Split(out, "\n") {
		if i := strings.Index(ln, "https://"); i >= 0 {
			url = strings.TrimSpace(ln[i:])
			break
		}
	}
	// 从 URL 提取 PR 编号（/pull/N）。
	num := 0
	if i := strings.LastIndex(url, "/pull/"); i >= 0 {
		fmt.Sscanf(url[i+6:], "%d", &num)
	}
	return &GhPRInfo{Number: num, Title: title, State: "OPEN", URL: url}, nil
}

// GhPRMerge 合并 PR（method: merge / squash / rebase；空 = 仓库默认）。
func (a *App) GhPRMerge(number int, method string) (string, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return "", msgWsEmpty()
	}
	switch method {
	case "", "merge", "squash", "rebase":
	default:
		return "", fmt.Errorf("不支持的合并方式: %s", method)
	}
	args := []string{"pr", "merge", fmt.Sprint(number), "--" + method}
	out, err := runGh(ws, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// GhCIOf 查询当前分支最新 commit 的 CI 检查矩阵（无 PR 时也可看 Actions 状态）。
func (a *App) GhCIOf() ([]GhCheckInfo, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return nil, msgWsEmpty()
	}
	out, err := runGh(ws, "pr", "checks", "--json", "name,status,bucket")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Bucket string `json:"bucket"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("gh 输出解析失败: %w", err)
	}
	checks := make([]GhCheckInfo, 0, len(raw))
	for _, c := range raw {
		concl := ""
		switch strings.ToUpper(c.Bucket) {
		case "PASS":
			concl = "SUCCESS"
		case "FAIL":
			concl = "FAILURE"
		case "PENDING", "SKIPPED", "CANCEL":
			concl = strings.ToUpper(c.Bucket)
		}
		checks = append(checks, GhCheckInfo{Name: c.Name, Status: strings.ToUpper(c.Status), Concl: concl})
	}
	return checks, nil
}
