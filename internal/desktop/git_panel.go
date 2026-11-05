// git_panel.go —— IDEA 风格 Git 集成数据面（Wails 绑定）。
//
// 对齐 JetBrains IDE 的 Commit / Git 工具窗语义：
//   - 变更列表（Changelist）: GitStatus → 未暂存/已暂存分区
//   - Diff 预览: GitFileDiff → 统一 diff 文本（前端着色）
//   - 局部提交: GitStage/GitUnstage + GitCommit（仅提交已暂存文件）
//   - 历史: GitLog（oneline + hash + 相对时间）
//   - 分支: GitBranches（当前 + 本地/远端列表）/ GitCheckout
//   - 推拉: GitPush/GitPull（透出命令输出）
//
// 全部经 `git -C <workspace>` 执行，输入经白名单过滤（文件路径不接受 shell 元字符：
// 由 exec.Command 参数化传递，无注入面；分支名/提交信息同理按参数传 git）。
package desktop

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"kaelvoris/internal/sysproc"
)

// GitFileEntry 变更文件条目（IDEA Changelist 行）。
type GitFileEntry struct {
	Path   string `json:"path"`   // 相对仓库根路径
	Status string `json:"status"` // M / A / D / R / C / U / ?（porcelain 首列）
	Staged bool   `json:"staged"` // 是否已在暂存区（porcelain 第二列 X）
}

// GitStatusView 工作区 git 状态（分支 + 变更分区）。
type GitStatus struct {
	Repo       bool           `json:"repo"`       // 是否 git 仓库
	Branch     string         `json:"branch"`     // 当前分支（detached 时为短 hash）
	Upstream   string         `json:"upstream"`   // 上游分支（空 = 无）
	Ahead      int            `json:"ahead"`      // 领先上游提交数
	Behind     int            `json:"behind"`     // 落后上游提交数
	Unstaged   []GitFileEntry `json:"unstaged"`   // 未暂存变更（含未跟踪）
	Staged     []GitFileEntry `json:"staged"`     // 已暂存变更
	RemoteRepo bool           `json:"remoteRepo"` // 本地仓库根是否存在
}

// GitCommitInfo 提交历史条目。
type GitCommitInfo struct {
	Hash    string `json:"hash"`
	Author  string `json:"author"`
	Date    string `json:"date"` // 相对时间（如 "3 hours ago"）
	Subject string `json:"subject"`
}

// GitBranchInfo 分支条目。
type GitBranchInfo struct {
	Name     string `json:"name"`
	Current  bool   `json:"current"`
	IsRemote bool   `json:"isRemote"`
}

func (a *App) gitWorkspace() string { return a.cfgWorkspace() }

// runGit 在工作区执行 git 子命令（超时 15s；错误文本带 stderr）。
func runGit(ws string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", ws}, args...)...)
	sysproc.Hide(cmd) // GUI 进程下隐藏控制台黑框。
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out.String(), nil
}

// GitStatus 当前工作区 git 状态（IDEA Commit 工具窗数据面）。
func (a *App) GitStatus() (*GitStatus, error) {
	ws := a.gitWorkspace()
	st := &GitStatus{Unstaged: []GitFileEntry{}, Staged: []GitFileEntry{}}
	if ws == "" {
		return st, nil
	}
	if st0, _ := filepath.Abs(ws); st0 != "" {
		ws = st0
	}
	probe := exec.Command("git", "-C", ws, "rev-parse", "--is-inside-work-tree")
	sysproc.Hide(probe)
	out, err := probe.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return st, nil // 非仓库：Repo=false，前端显示「未启用版本控制」
	}
	st.Repo = true

	// 分支（detached → 短 hash）。
	if b, err := runGit(ws, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		st.Branch = strings.TrimSpace(b)
		if st.Branch == "HEAD" {
			if h, err := runGit(ws, "rev-parse", "--short", "HEAD"); err == nil {
				st.Branch = strings.TrimSpace(b0(h)) + " (detached)"
			}
		}
	}
	// 上游与领先/落后。
	if up, err := runGit(ws, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		st.Upstream = strings.TrimSpace(up)
		if ab, err := runGit(ws, "rev-list", "--left-right", "--count", "HEAD...@{upstream}"); err == nil {
			fields := strings.Fields(ab)
			if len(fields) == 2 {
				fmt.Sscanf(fields[0], "%d", &st.Ahead)
				fmt.Sscanf(fields[1], "%d", &st.Behind)
			}
		}
	}

	// porcelain v1：XY path；X=暂存区状态，Y=工作区状态。
	// core.quotepath=false：中文路径以 UTF-8 原样输出，否则被转义为
	// `"doc/03-\345\274\200..."`（带引号），前端回传 git add 时 pathspec 不匹配。
	porcelain, err := runGit(ws, "-c", "core.quotepath=false", "status", "--porcelain")
	if err != nil {
		return st, nil
	}
	seen := map[string]bool{}
	for _, ln := range strings.Split(porcelain, "\n") {
		if len(ln) < 4 {
			continue
		}
		x, y := ln[0], ln[1] // X = index（staged），Y = worktree
		path := renamePath(ln[3:])
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if x != ' ' && x != '?' {
			st.Staged = append(st.Staged, GitFileEntry{Path: path, Status: string(x), Staged: true})
		}
		if y != ' ' {
			code := string(y)
			if y == '?' {
				code = "A" // 未跟踪在 IDEA 中呈现为新增（unversioned）
			}
			st.Unstaged = append(st.Unstaged, GitFileEntry{Path: path, Status: code, Staged: false})
		}
	}
	return st, nil
}

// b0 取字符串首行。
func b0(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// renamePath 处理 porcelain 的 "old -> new" 重命名路径（取 new）。
func renamePath(p string) string {
	if i := strings.Index(p, " -> "); i >= 0 {
		return strings.TrimSpace(p[i+4:])
	}
	return porcelainPath(strings.TrimSpace(p))
}

// porcelainPath 还原 porcelain 输出的 C 风格引号路径（core.quotepath 场景下
// 仍可能有控制字符被引号包裹，如 "a\tb.txt"）：解引为真实路径，失败则原样返回。
func porcelainPath(p string) string {
	if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
		if s, err := strconv.Unquote(p); err == nil {
			return s
		}
	}
	return p
}

// GitFileDiff 单文件统一 diff（--unified=3；新文件全量 +）。
func (a *App) GitFileDiff(path string) (string, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return "", fmt.Errorf("未设置工作区")
	}
	out, err := runGit(ws, "diff", "--unified=3", "--no-color", "--", path)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		// 已暂存或未跟踪：回退 staged diff / 空文件内容。
		if out, err = runGit(ws, "diff", "--cached", "--unified=3", "--no-color", "--", path); err == nil && strings.TrimSpace(out) != "" {
			return out, nil
		}
		if head, err2 := runGit(ws, "show", "HEAD:"+path); err2 != nil {
			// 未跟踪新文件：读工作区文件全文作为新增 diff。
			if b, err3 := osReadFileSafe(filepath.Join(ws, path)); err3 == nil {
				lines := strings.Split(strings.TrimRight(b, "\n"), "\n")
				var sb strings.Builder
				for _, l := range lines {
					sb.WriteString("+" + l + "\n")
				}
				return sb.String(), nil
			}
			return "", fmt.Errorf("无法读取差异")
		} else {
			return head, nil
		}
	}
	return out, nil
}

// GitStage 把文件加入暂存区（IDEA 的 Move to Staged）。
func (a *App) GitStage(paths []string) error {
	ws := a.gitWorkspace()
	if ws == "" {
		return fmt.Errorf("未设置工作区")
	}
	if len(paths) == 0 {
		return fmt.Errorf("未选择文件")
	}
	_, err := runGit(ws, append([]string{"add", "--"}, paths...)...)
	return err
}

// GitUnstage 把文件移出暂存区（保留工作区修改）。
func (a *App) GitUnstage(paths []string) error {
	ws := a.gitWorkspace()
	if ws == "" {
		return msgWsEmpty()
	}
	if len(paths) == 0 {
		return fmt.Errorf("未选择文件")
	}
	_, err := runGit(ws, append([]string{"reset", "-q", "HEAD", "--"}, paths...)...)
	return err
}

// GitCommit 提交（IDEA Commit：仅提交已暂存内容；message 必填）。
func (a *App) GitCommit(msgText string, stageAll bool) (string, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return "", fmt.Errorf("未设置工作区")
	}
	msgText = strings.TrimSpace(msgText)
	if msgText == "" {
		return "", fmt.Errorf("提交信息不能为空")
	}
	if stageAll {
		if _, err := runGit(ws, "add", "-A"); err != nil {
			return "", err
		}
	}
	out, err := runGit(ws, "commit", "-m", msgText)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// GitPush 推送到上游。
func (a *App) GitPush() (string, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return "", msgWsEmpty()
	}
	out, err := runGit(ws, "push")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// GitPull 拉取远端更新。
func (a *App) GitPull() (string, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return "", msgWsEmpty()
	}
	out, err := runGit(ws, "pull", "--no-edit")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// GitLog 提交历史（IDEA Git 工具窗 Log 标签）。
func (a *App) GitLog(limit int) ([]GitCommitInfo, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return nil, msgWsEmpty()
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// --pretty 用 TAB 分隔四列（hash/author/relative-date/subject）。
	out, err := runGit(ws, "log", "--pretty=format:%h%x09%an%x09%ad%x09%s", "--date=relative", "-n", fmt.Sprint(limit))
	if err != nil {
		return nil, err
	}
	var list []GitCommitInfo
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		parts := strings.SplitN(ln, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		list = append(list, GitCommitInfo{Hash: parts[0], Author: parts[1], Date: parts[2], Subject: parts[3]})
	}
	return list, nil
}

// GitBranches 全部分支（本地 + 远端，当前分支置顶）。
func (a *App) GitBranches() ([]GitBranchInfo, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return nil, msgWsEmpty()
	}
	out, err := runGit(ws, "branch", "-a", "--no-color")
	if err != nil {
		return nil, err
	}
	var list []GitBranchInfo
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		cur := strings.HasPrefix(ln, "* ")
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(ln, "* "), "+ "))
		if name == "" || strings.Contains(name, "HEAD ->") {
			continue
		}
		isRemote := strings.HasPrefix(ln, "  remotes/")
		name = strings.TrimPrefix(name, "remotes/")
		list = append(list, GitBranchInfo{Name: name, Current: cur && strings.HasPrefix(ln, "*"), IsRemote: isRemote})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Current != list[j].Current {
			return list[i].Current
		}
		if list[i].IsRemote != list[j].IsRemote {
			return !list[i].IsRemote
		}
		return list[i].Name < list[j].Name
	})
	return list, nil
}

// GitCheckout 切换分支（IDEA Checkout）。
func (a *App) GitCheckout(branch string) (string, error) {
	ws := a.gitWorkspace()
	if ws == "" {
		return "", msgWsEmpty()
	}
	b := strings.TrimSpace(branch)
	if b == "" || strings.ContainsAny(b, " \t;|&$`") {
		return "", fmt.Errorf("非法分支名")
	}
	out, err := runGit(ws, "checkout", b)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// GitDiscard 丢弃指定文件未提交修改（IDEA Rollback；危险操作需前端二次确认）。
func (a *App) GitDiscard(paths []string) error {
	ws := a.gitWorkspace()
	if ws == "" {
		return msgWsEmpty()
	}
	if len(paths) == 0 {
		return fmt.Errorf("未选择文件")
	}
	if _, err := runGit(ws, append([]string{"checkout", "--"}, paths...)...); err != nil {
		return err
	}
	// 未跟踪文件需额外清理（checkout 对 untracked 无效）。
	_, _ = runGit(ws, append([]string{"clean", "-f", "--"}, paths...)...)
	return nil
}

func msgWsEmpty() error { return fmt.Errorf("未设置工作区") }

// osReadFileSafe 读文件（限制大小 1MB，避免 diff 预览读超大文件）。
func osReadFileSafe(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(b) > 1<<20 {
		b = b[:1<<20]
	}
	return string(b), nil
}
