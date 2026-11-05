// gitinfo.go 工作区 git 环境快照（environment details 增强）。
//
// 对齐 Zoo-Code environment_details 的 git 状态注入（简版）：系统提示词
// SYSTEM INFORMATION 段附带当前分支与变更概览，模型无需执行 shell 即可
// 感知仓库状态（分支漂移/未提交变更直接影响其决策）。
package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"kaelvoris/internal/sysproc"
)

// gitSnapshot 返回工作区 git 状态一行摘要（分支 + 变更统计 + 变更文件样例）。
// 非 git 仓库 / git 不可用 / 超时（2s）返回空串，调用方零分支处理。
func gitSnapshot(cwd string) string {
	if cwd == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	branchCmd := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD")
	sysproc.Hide(branchCmd)
	branchOut, err := branchCmd.Output()
	if err != nil {
		return "" // 非 git 仓库或 git 不可用
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "" {
		return ""
	}

	stCmd := exec.CommandContext(ctx, "git", "-C", cwd, "-c", "core.quotepath=false", "status", "--porcelain")
	sysproc.Hide(stCmd)
	out, err := stCmd.Output()
	if err != nil {
		return "Git branch: " + branch
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return "Git branch: " + branch + "; working tree clean"
	}
	lines := strings.Split(trimmed, "\n")
	summary := fmt.Sprintf("Git branch: %s; %d changed file(s)", branch, len(lines))

	// 变更文件样例（porcelain 格式前两列为状态码，路径自下标 3 起）。
	const maxList = 5
	var items []string
	for i, ln := range lines {
		if i >= maxList {
			items = append(items, "...")
			break
		}
		if len(ln) > 3 {
			items = append(items, strings.TrimSpace(ln[3:]))
		}
	}
	return summary + ": " + strings.Join(items, ", ")
}
