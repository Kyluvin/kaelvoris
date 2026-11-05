// Package autocommit 任务收尾 git 自动提交（C3，对标 Aider auto-commits）。
//
// Aider 语义（https://aider.chat/docs/git.html）：
//   - 「Whenever aider edits a file, it commits those changes with a
//     descriptive commit message.」——每次编辑自动 commit；
//   - dirty 文件保护：编辑前先把用户既有未提交变更单独 commit，
//     用户工作与 AI 工作分离，AI 改坏可整体 revert；
//   - commit 附 (aider) 归因标记。
//
// Kaelvoris 简化对齐：
//   - 触发点：任务收尾（OnTaskFinished，仅顶层任务、任务成功）一次性提交，
//     非 Aider 的每编辑提交（我们的粒度是任务级 net diff，与 turn_diff/checkpoint 对齐）；
//   - dirty 保护：提交前若工作区有非本次任务的未提交变更，先打 wip commit；
//   - 归因：commit 正文附 "🤖 Generated with Kaelvoris"；
//   - 默认关闭（desktop.json gitAutocommit.enabled，显式开启才生效——
//     自动改用户 git 历史属高敏感行为，这是与 Aider 唯一的默认差异）；
//   - 任何失败静默（仅日志），不影响任务收尾。
package autocommit

import (
	"fmt"
	"os/exec"
	"strings"

	"kaelvoris/internal/sysproc"
)

// commitFirstLineMax commit 首行任务摘要上限（rune）。
const commitFirstLineMax = 60

// gitCmd 构造 git 命令（cwd=workspace；core.quotepath=false 防中文路径乱码）。
func gitCmd(workspace string, args ...string) *exec.Cmd {
	full := append([]string{"-c", "core.quotepath=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = workspace
	sysproc.Hide(cmd) // GUI 进程下隐藏控制台黑框（共享构造点，覆盖全部 git 调用）。
	return cmd
}

// IsGitRepo 工作区是否 git 仓库（rev-parse 失败 = 否）。
func IsGitRepo(workspace string) bool {
	cmd := gitCmd(workspace, "rev-parse", "--is-inside-work-tree")
	return cmd.Run() == nil
}

// dirtyFiles 返回工作区已跟踪文件的未提交变更清单（porcelain -uno；失败返回 nil）。
// 排除 untracked（??）：AI 新建的文件不应被 dirty 保护先行提交，留给 AI 提交。
func dirtyFiles(workspace string) []string {
	cmd := gitCmd(workspace, "status", "--porcelain", "--untracked-files=no")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// porcelain 格式：XY <path>（Y 可能含重命名箭头，取后者）。
		if len(line) > 3 {
			p := line[3:]
			if i := strings.Index(p, " -> "); i >= 0 {
				p = p[i+4:]
			}
			files = append(files, strings.Trim(p, "\""))
		}
	}
	return files
}

// CommitMessage 生成提交信息（纯函数）：
// 首行 = 任务摘要（前 60 rune，压平换行）；正文 = 变更文件清单 + 归因尾注。
func CommitMessage(task string, files []string) string {
	first := strings.Join(strings.Fields(task), " ") // 压平换行/多空白
	r := []rune(first)
	if len(r) > commitFirstLineMax {
		first = string(r[:commitFirstLineMax]) + "…"
	}
	var b strings.Builder
	b.WriteString(first)
	b.WriteString("\n\n")
	for _, f := range files {
		b.WriteString("- " + f + "\n")
	}
	b.WriteString("\n🤖 Generated with Kaelvoris")
	return b.String()
}

// Run 执行收尾提交全流程（dirty 保护 → add 变更文件 → commit）。
// 返回 commit hash 前 8 位；任何一步失败返回 err（调用方静默）。
// files 为本次任务变更的相对路径清单（引擎 DiffFiles() 产出）。
func Run(workspace string, files []string, task string) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("autocommit: no changed files")
	}
	if !IsGitRepo(workspace) {
		return "", fmt.Errorf("autocommit: not a git repo")
	}

	// dirty 保护（Aider dirty-commits 对齐）：工作区已有已跟踪文件的未提交
	// 变更时，先把既有变更整体 commit 为 wip，保证用户工作与 AI 工作分离
	// 可独立 revert。untracked（??）不参与：AI 新建的文件留给下方 AI 提交。
	if dirty := dirtyFiles(workspace); len(dirty) > 0 {
		// wip 提交只含用户既有变更，必须排除 AI 的 files（AI 文件可能是 untracked
		// 新文件，add -A 会把它们一并卷进 wip，导致下方 AI 提交 "nothing to commit"）。
		addArgs := []string{"add", "-A", "--"}
		for _, f := range files {
			addArgs = append(addArgs, ":(exclude)"+f)
		}
		add := gitCmd(workspace, addArgs...)
		if err := add.Run(); err != nil {
			return "", fmt.Errorf("autocommit: dirty add: %w", err)
		}
		commit := gitCmd(workspace, "commit", "-m", "wip: user changes before kaelvoris task")
		commit.Run() // 无用户身份配置等场景失败可容忍（后续 AI 提交仍会尝试）
	}

	// 仅 add 本次任务变更文件（不做 add -A，避免把无关脏文件卷入）。
	add := gitCmd(workspace, append([]string{"add", "--"}, files...)...)
	if out, err := add.CombinedOutput(); err != nil {
		return "", fmt.Errorf("autocommit: add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	msg := CommitMessage(task, files)
	commit := gitCmd(workspace, "commit", "-m", msg)
	if out, err := commit.CombinedOutput(); err != nil {
		return "", fmt.Errorf("autocommit: commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	out, err := gitCmd(workspace, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("autocommit: rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
