// Package agent AGENTS.md 发现（codex 对齐补全 ④）。
//
// 对齐 codex-rs core/src/agents_md.rs 的发现语义（Go 简版）：
//   - 从当前工作目录向上查找最近的项目根（含默认 root marker `.git`），
//     项目根到 cwd 之间每一层目录的 AGENTS.md 均注入（层级越深越具体）；
//   - 同一目录候选文件优先级：AGENTS.override.md > AGENTS.md > AGENT.md
//     （override 语义对齐 LOCAL_AGENTS_MD_FILENAME；每目录只取首个命中）；
//   - cwd 的 AGENTS.local.md 始终最高优先（本地私有覆盖，不入库的约束）；
//   - project_doc_max_bytes 单文件截断（对齐 config.project_doc_max_bytes
//     默认 32KiB；超限截断并附说明，防超大文档撑爆上下文）。
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// projectRootMarkers 项目根标记（对齐 default_project_root_markers 简版）：
// 向上查找时首个含任一标记的目录视为项目根。
var projectRootMarkers = []string{".git", ".hg"}

// agentsMDCandidates 同一目录的候选文件名（优先级从高到低；override > 默认）。
var agentsMDCandidates = []string{"AGENTS.override.md", "AGENTS.md", "AGENT.md"}

// agentsMDMaxBytes 单文件读取上限（对齐 codex project_doc_max_bytes 默认）。
const agentsMDMaxBytes = 32 * 1024

// agentsMDPaths 发现 cwd 的全部 AGENTS.md 绝对路径（项目根 → cwd 自上而下）。
// 找不到项目根时仅查 cwd 本身；目录无候选文件则跳过（对齐 agents_md_paths）。
func agentsMDPaths(cwd string) []string {
	if cwd == "" {
		return nil
	}
	root := findProjectRoot(cwd)
	dirs := ancestorDirs(cwd, root) // 自上而下：根在最前（层级语义）
	var paths []string
	for _, d := range dirs {
		for _, name := range agentsMDCandidates {
			p := filepath.Join(d, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				paths = append(paths, p)
				break // 每目录仅取首个命中（override 优先）
			}
		}
	}
	return paths
}

// findProjectRoot 从 dir 向上查找最近项目根（含 dir 自身）；无标记返回空串。
func findProjectRoot(dir string) string {
	cursor, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		for _, m := range projectRootMarkers {
			if st, err := os.Stat(filepath.Join(cursor, m)); err == nil {
				_ = st
				return cursor
			}
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return "" // 到达文件系统根
		}
		cursor = parent
	}
}

// ancestorDirs 返回 dir → root 的目录链（自上而下：root 最前；root 为空时仅 [dir]）。
func ancestorDirs(dir, root string) []string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	// 自下而上收集到根为止；再反转成自上而下。
	var chain []string
	cursor := abs
	for {
		chain = append(chain, cursor)
		if absRoot != "" && sameDir(cursor, absRoot) {
			break
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			break
		}
		cursor = parent
	}
	if absRoot == "" {
		return []string{abs} // 无根：仅 cwd 一层
	}
	// 反转：根 → cwd
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// sameDir 同一目录判定（大小写不敏感，适配 Windows）。
func sameDir(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// readAgentsMDTruncated 读取单文件，超 agentsMDMaxBytes 截断并附说明
// （对齐 codex read_agents_md 的 max bytes 语义）。
func readAgentsMDTruncated(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b) > agentsMDMaxBytes {
		note := fmt.Sprintf("\n\n[AGENTS.md truncated at %d bytes of %d]", agentsMDMaxBytes, len(b))
		return string(b[:agentsMDMaxBytes]) + note
	}
	return string(b)
}

// loadAgentsMDChain AGENTS.md 发现注入主入口：项目根 → cwd 逐层加载 +
// cwd 的 AGENTS.local.md 兜底（对齐 load_project_instructions 的 entries 拼接）。
// 每层标注来源相对路径（层级越深越具体，模型可区分覆盖关系）。
func loadAgentsMDChain(cwd string) string {
	var results []string
	for _, p := range agentsMDPaths(cwd) {
		c := strings.TrimSpace(readAgentsMDTruncated(p))
		if c == "" {
			continue
		}
		results = append(results, "# Agent Rules ("+filepath.ToSlash(p)+"):\n"+c)
	}
	// cwd 的 AGENTS.local.md 最高优先（本地私有覆盖；对齐 AGENTS.local 语义）。
	if p := filepath.Join(cwd, "AGENTS.local.md"); p != "" {
		if c := strings.TrimSpace(readAgentsMDTruncated(p)); c != "" {
			results = append(results, "# Agent Rules Local ("+filepath.ToSlash(p)+"):\n"+c)
		}
	}
	return strings.Join(results, "\n\n")
}
