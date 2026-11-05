// ignore.go —— .rooignore/.gitignore 规则过滤（RooIgnoreController 简版）。
//
// 语义子集（对齐 gitignore 惯例）：
//   - 空行与 # 注释忽略；
//   - 行首 ! 取反（顺序评估、最后命中生效，与 gitignore 一致）；
//   - 尾部 / 仅匹配目录；
//   - 含 / 的模式相对工作区根锚定；否则对任意层级的名字生效；
//   - glob：*（不含 /）、?（单字符）、**（跨目录段）。
//
// 仅解析工作区根目录的 .rooignore 与 .gitignore（子目录级 ignore 文件不
// 递归解析，保持简版；.rooignore 规则先于 .gitignore 评估）。
package tools

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ignoreMatcher ignore 规则集（nil = 未启用，调用零开销直通）。
type ignoreMatcher struct {
	rules []ignoreRule
}

type ignoreRule struct {
	re      *regexp.Regexp // 编译后的锚定正则
	raw     string         // 原始模式（判断是否锚定）
	negate  bool           // ! 取反
	dirOnly bool           // 尾部 / 仅匹配目录
}

// loadIgnoreMatcher 读取工作区根的 .rooignore 与 .gitignore 构建 matcher；
// 两个文件都不存在（或无有效规则）时返回 nil。
func loadIgnoreMatcher(workspace string) *ignoreMatcher {
	var rules []ignoreRule
	for _, name := range []string{".rooignore", ".gitignore"} {
		f, err := os.Open(filepath.Join(workspace, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if r, ok := parseIgnoreRule(sc.Text()); ok {
				rules = append(rules, r)
			}
		}
		f.Close()
	}
	if len(rules) == 0 {
		return nil
	}
	return &ignoreMatcher{rules: rules}
}

// rooignoreGlobs 把 .rooignore 规则转成 rg 的否定 --glob 参数。
// （rg 原生遵守 .gitignore/.ignore，.rooignore 需显式传参排除；取反行跳过。）
func rooignoreGlobs(workspace string) []string {
	f, err := os.Open(filepath.Join(workspace, ".rooignore"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var globs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		globs = append(globs, "!"+line)
	}
	return globs
}

// parseIgnoreRule 解析单行规则（简版：不支持转义与字符组）。
func parseIgnoreRule(line string) (ignoreRule, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	r := ignoreRule{}
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return ignoreRule{}, false
	}
	re, err := compileIgnoreGlob(line)
	if err != nil {
		return ignoreRule{}, false
	}
	return ignoreRule{re: re, raw: line, negate: r.negate, dirOnly: r.dirOnly}, true
}

// ignore 判定 rel（工作区相对路径、'/' 分隔）是否被忽略。
func (m *ignoreMatcher) ignore(rel string, isDir bool) bool {
	if m == nil {
		return false
	}
	// 任一祖先目录被忽略 → 子项忽略（gitignore 目录语义）。
	segs := strings.Split(rel, "/")
	for i := 1; i < len(segs); i++ {
		if m.match(strings.Join(segs[:i], "/"), true) {
			return true
		}
	}
	return m.match(rel, isDir)
}

// match 单路径匹配；多规则命中时最后一条生效（gitignore 语义）。
func (m *ignoreMatcher) match(rel string, isDir bool) bool {
	ignored := false
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		hit := r.re.MatchString(rel)
		// 未锚定模式（不含 /）对任意层级的名字生效（gitignore 语义）。
		if !hit && !strings.Contains(r.raw, "/") {
			for _, seg := range strings.Split(rel, "/") {
				if r.re.MatchString(seg) {
					hit = true
					break
				}
			}
		}
		if hit {
			ignored = !r.negate
		}
	}
	return ignored
}

// compileIgnoreGlob 把 gitignore 风格模式编译为锚定正则：
// ** 跨目录段（吞掉随后的 /）、* 单段内任意、? 单字符，其余字面量。
func compileIgnoreGlob(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pattern) {
		c := pattern[i]
		switch {
		case c == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
			j := i + 2
			if j < len(pattern) && pattern[j] == '/' {
				// "**/" 匹配零层或多层目录段。
				b.WriteString(`(?:[^/]+/)*`)
				i = j + 1
			} else {
				b.WriteString(`.*`)
				i = j
			}
		case c == '*':
			b.WriteString(`[^/]*`)
			i++
		case c == '?':
			b.WriteString(`[^/]`)
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
