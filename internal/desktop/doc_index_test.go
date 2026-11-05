// docindex_test.go —— doc/ 知识库索引同步校验（2026-10-06 复盘缺口 #4）：
// 新增/移动 doc/ 文档后若忘记更新 doc/README.md 索引，用户看不到归类结果
// → 同一需求重复提交 2 次（「文档进行标准文件夹分类」案例）。本测试静态
// 扫描仓库 doc/ 目录与 README 索引交叉比对，不落盘、只读校验。
//
// 校验规则（宽松设计，防误报阻断正常任务）：
//  1. doc/ 下每个 .md 文档（不含 README.md 自身）应在 README.md 中被引用
//     至少一次（按文件名匹配，路径分隔符无关）；
//  2. README.md 引用的相对链接目标应真实存在（防改名后索引悬空）。
//
// 两者皆查 README 索引段落与目录树的一致性，方向互补。
package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoDocRoot 定位仓库 doc/ 目录（测试文件位于 internal/desktop/，
// 迁包后需向上递归回仓根找 doc/，找不到才 Skip）。
func repoDocRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	dir := wd
	for i := 0; i < 6; i++ {
		root := filepath.Join(dir, "doc")
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			return root
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skipf("doc/ 目录不存在（非仓库内运行？）: 起点 %s", wd)
	return ""
}

// listRepoDocs 递归收集 doc/ 下全部 .md 相对路径（'/' 分隔，含子目录）。
func listRepoDocs(t *testing.T, root string) []string {
	t.Helper()
	var docs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".md") && d.Name() != "README.md" {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			docs = append(docs, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 doc/ 失败: %v", err)
	}
	return docs
}

// TestDocIndexSync doc/ 全部 .md 文档必须被 doc/README.md 索引引用；
// README 引用的相对链接必须指向真实存在的文件。双向防「改文档不改索引」。
func TestDocIndexSync(t *testing.T) {
	root := repoDocRoot(t)
	docs := listRepoDocs(t, root)
	if len(docs) == 0 {
		t.Skip("doc/ 无文档，跳过")
	}
	b, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("读 doc/README.md 失败: %v", err)
	}
	readme := string(b)

	// 1) 每个文档应在 README 中按文件名（或相对路径）被引用。
	for _, rel := range docs {
		base := filepath.Base(rel)
		if !strings.Contains(readme, rel) && !strings.Contains(readme, base) {
			t.Errorf("doc/%s 未被 doc/README.md 索引引用（改文档须同步索引，防用户重复提交）", rel)
		}
	}

	// 2) README 中的相对链接目标应存在（markdown 链接形式 (xxx/yyy.md)）。
	for _, line := range strings.Split(readme, "\n") {
		// 逐行提取 ](...md) 形式的链接目标。
		i := strings.Index(line, "](")
		for i >= 0 {
			rest := line[i+2:]
			j := strings.Index(rest, ")")
			if j < 0 {
				break
			}
			target := rest[:j]
			i = strings.Index(rest[j+1:], "](")
			if i >= 0 {
				i += j + 1
			}
			target = strings.TrimPrefix(target, "./")
			if target == "" || strings.Contains(target, "://") || strings.Contains(target, "#") {
				continue // 外链/锚点跳过
			}
			if !strings.HasSuffix(strings.ToLower(target), ".md") {
				continue // 非文档链接（目录/图片）跳过
			}
			full := filepath.Join(root, filepath.FromSlash(target))
			if _, err := os.Stat(full); err != nil {
				t.Errorf("doc/README.md 引用了不存在的文档: %s", target)
			}
		}
	}
}
