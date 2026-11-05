// composer_bridge.go —— Composer 输入增强桥接（Claude Code @ 文件补全对齐）：
//
//	ListWorkspaceFiles —— 工作区文件清单（@ 补全数据源；递归列出，跳过
//	.gitignore 命中目录与常见噪音目录，上限 2000 条防卡顿）。
//
// 前端 Composer 监听 @ 触发：输入 "@" 后弹候选，↑↓ 选择、Enter/Tab 补全，
// 补全文本为相对路径（后端 mentions.Expand 已支持 @/path 引用展开）。
package desktop

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxCompletions 补全清单上限（防超大仓库卡顿）。
const maxListWorkspaceFiles = 2000

// skippedDirs @ 补全递归时跳过的目录名（依赖/构建产物/版本控制噪音）。
var skippedDirs = map[string]bool{
	"node_modules": true, ".git": true, ".svn": true, "dist": true,
	"build": true, "out": true, "vendor": true, "__pycache__": true,
	".venv": true, "venv": true, ".idea": true, ".vscode": true,
	"target": true, ".next": true, ".nuxt": true, "coverage": true,
	"dist-newstyle": true, ".gradle": true, "bin": true, "obj": true,
}

// ListWorkspaceFiles 返回工作区相对路径清单（@ 补全数据源）。
// 最多返回 2000 条；按路径排序保证前端稳定渲染。目录以 "/" 结尾。
func (a *App) ListWorkspaceFiles() []string {
	ws := a.cfgWorkspace()
	if ws == "" {
		return nil
	}
	if abs, err := filepath.Abs(ws); err == nil {
		ws = abs
	}
	out := make([]string, 0, 256)
	var walk func(dir, rel string, depth int) bool
	walk = func(dir, rel string, depth int) bool {
		if len(out) >= maxListWorkspaceFiles || depth > 8 { // 深度上限防深层依赖树
			return len(out) >= maxListWorkspaceFiles
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		// 目录优先排序，贴近 IDE 文件树观感。
		dirs, files := []os.DirEntry{}, []os.DirEntry{}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") && name != ".github" {
				continue // 隐藏目录（.git/.venv 等；保留 .github 工作流可见）
			}
			if e.IsDir() {
				if skippedDirs[name] {
					continue
				}
				dirs = append(dirs, e)
			} else {
				files = append(files, e)
			}
		}
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name() < dirs[j].Name() })
		sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Name()) < strings.ToLower(files[j].Name()) })
		for _, e := range dirs {
			rel2 := rel + e.Name() + "/"
			out = append(out, rel2)
			if walk(filepath.Join(dir, e.Name()), rel2, depth+1) {
				return true
			}
		}
		for _, e := range files {
			out = append(out, rel+e.Name())
			if len(out) >= maxListWorkspaceFiles {
				return true
			}
		}
		return false
	}
	walk(ws, "", 0)
	sort.Strings(out)
	return out
}
