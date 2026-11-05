// 代码转换节点（transform）执行器 —— 2026-10 Dify 编排语义融入。
//
// 对齐 Dify 的 template-transform / code 节点思想：在流程中用一小段脚本对
// 上游变量做确定性加工，输出写回变量池（state.pools）供下游模板与分支条件
// 以 {nodeID.output} 引用。与 Dify 的差异（有意为之，保持零依赖、无沙箱）：
//   - 不运行真实 Python/JS，而是「双大括号变量替换 + 四则运算 + if 控制流 +
//     少量内置函数」的轻量子集（python / javascript 两种书写风格）；
//   - 需要真实代码/命令执行时走 tools 节点（shell / unified_exec），边界更清晰。
//
// 语法（python 风格）：
//
//	{{x}}               变量替换（支持 {nodeID.output} 引用上游节点输出）
//	{{x + 1}}           四则运算（+ - * /，右操作数为数值字面量）
//	{{upper name}}      内置函数：upper / lower / trim / len
//	if x > 10:          条件控制流（> >= < <= == != 与 contains，语义同分支节点；
//	else:                 字符串字面量需加引号），以 endif 收尾
//	  小值
//	endif
//
// javascript 风格仅控制流写法不同：if (x > 10) { … } else { … }（} 收尾），
// 其余语法完全一致。循环不支持（流程内循环属迭代节点范畴，后续规划）。
//
// 双大括号（{{x}}）与模板节点单大括号（{x}）刻意区分：transform 脚本中
// 单大括号是代码字面量（dict/JSON/函数体），不做替换。
package flow

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

// ── 正则清单（全部锚定明确边界，避免误伤代码字面量）─────────────

// pyIfRe python 条件行：if <cond>: / elif <cond>:（行尾注释容错）。
var pyIfRe = regexp.MustCompile(`^if\s+(.+?)\s*:\s*(?:#.*)?$`)

// pyElifRe / jsElifRe elif 行（javascript 风格写作 `} else if (<cond>) {`）。
var (
	pyElifRe = regexp.MustCompile(`^elif\s+(.+?)\s*:\s*$`)
	jsElifRe = regexp.MustCompile(`^\}?\s*else\s+if\s*\((.+?)\)\s*\{?\s*$`)
)

// jsIfRe javascript 条件行：if (<cond>) {（大括号可省略）。
var jsIfRe = regexp.MustCompile(`^if\s*\((.+?)\)\s*\{?\s*$`)

// pyElseRe / jsElseRe / jsEndRe 控制流边界行。
var (
	pyElseRe = regexp.MustCompile(`^else\s*:\s*$`)
	jsElseRe = regexp.MustCompile(`^}?\s*else\s*\{\s*$`)
	jsEndRe  = regexp.MustCompile(`^\}\s*$`)
)

// arithRe 算术替换：{{var + 2}}（左操作数 = 变量引用，右 = 数值字面量）。
var arithRe = regexp.MustCompile(
	`\{\{\s*([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*([-+*/])\s*([0-9.]+)\s*\}\}`)

// actRe 动作替换：{{ref}} → {{.ref}}、{{fn ref}} → {{fn .ref}}（text/template 语法）。
var actRe = regexp.MustCompile(
	`\{\{\s*(upper|lower|trim|len)?\s*([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\}\}`)

// condTokRe 条件表达式分词：字符串字面量（保持原样）| 标识符（转 {var}）。
var condTokRe = regexp.MustCompile(`(?:"[^"]*"|'[^']*')|[A-Za-z_][A-Za-z0-9_.]*`)

// convertVarTokens 把条件表达式中的裸标识符包成 {var}（字符串字面量与
// true/false/contains 关键字保持原样）。
func convertVarTokens(c string) string {
	return condTokRe.ReplaceAllStringFunc(c, func(tok string) string {
		if strings.HasPrefix(tok, `"`) || strings.HasPrefix(tok, `'`) {
			return tok
		}
		switch tok {
		case "true", "false", "contains":
			return tok
		}
		return "{" + tok + "}"
	})
}

// execTransform transform 节点执行入口：输入 = 用户变量 + 变量池。
func (r *Runner) execTransform(_ context.Context, st *state, step Step) (string, error) {
	lang := strings.ToLower(strings.TrimSpace(step.Role))
	if lang == "" {
		lang = "python" // Dify code 节点默认 python
	}
	if lang != "python" && lang != "javascript" {
		return "", fmt.Errorf("不支持的语言: %s（仅 python / javascript）", lang)
	}
	if strings.TrimSpace(step.Template) == "" {
		return "", fmt.Errorf("转换脚本为空")
	}
	return renderTransform(step.Template, st.mergedVars(), lang)
}

// renderTransform 三段流水线：条件块过滤 → 算术求值 → 变量替换（text/template）。
func renderTransform(script string, vars map[string]any, lang string) (string, error) {
	// 1. 条件块过滤（if/elif/else → 只保留命中分支的正文行）。
	lines, err := filterBranches(strings.Split(script, "\n"), lang, vars)
	if err != nil {
		return "", err
	}
	s := strings.Join(lines, "\n")

	// 2. 算术替换（{{x + 1}} → 数值文本）。
	s = transformArith(s, vars)

	// 3. 动作改写 + text/template 执行（missingkey=error：缺变量快速失败）。
	s = actRe.ReplaceAllStringFunc(s, func(m string) string {
		g := actRe.FindStringSubmatch(m)
		if g[1] != "" {
			return "{{" + g[1] + " ." + g[2] + "}}"
		}
		return "{{." + g[2] + "}}"
	})
	tpl, err := template.New("transform").Option("missingkey=error").Funcs(template.FuncMap{
		"upper": strings.ToUpper,
		"lower": strings.ToLower,
		"trim":  strings.TrimSpace,
		"len":   func(v any) int { return len(fmt.Sprint(v)) }, // 覆盖内置 len：任意值安全取长
	}).Parse(s)
	if err != nil {
		return "", fmt.Errorf("转换脚本解析失败: %w", err)
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, vars); err != nil {
		return "", fmt.Errorf("转换脚本执行失败: %w", err)
	}
	return sb.String(), nil
}

// filterBranches 条件块过滤：if/elif/else 行求值路由，仅保留命中分支正文。
// 条件求值复用分支节点同一套 evalBranchExpr（语义一致），裸标识符先转 {var}。
func filterBranches(lines []string, lang string, vars map[string]any) ([]string, error) {
	condToExpr := func(c string) string {
		// contains 右操作数 = 字符串字面量（裸词保持原样，不转 {var}）；
		// 其余 token（左右两侧）转 {var} 交由 evalBranchExpr 渲染。
		if i := strings.Index(c, " contains "); i > 0 {
			return convertVarTokens(c[:i]) + " contains " + c[i+len(" contains "):]
		}
		return convertVarTokens(c)
	}
	isIf := func(t string) (string, bool) {
		if lang == "javascript" {
			if m := jsIfRe.FindStringSubmatch(t); m != nil {
				return m[1], true
			}
			return "", false
		}
		if m := pyIfRe.FindStringSubmatch(t); m != nil {
			return m[1], true
		}
		return "", false
	}
	isElse := func(t string) bool {
		if lang == "javascript" {
			return jsElseRe.MatchString(t)
		}
		return pyElseRe.MatchString(t)
	}
	isElif := func(t string) (string, bool) {
		if lang == "javascript" {
			if m := jsElifRe.FindStringSubmatch(t); m != nil {
				return m[1], true
			}
			return "", false
		}
		if m := pyElifRe.FindStringSubmatch(t); m != nil {
			return m[1], true
		}
		return "", false
	}
	isEnd := func(t string) bool {
		if lang == "javascript" {
			return jsEndRe.MatchString(t)
		}
		return t == "endif"
	}

	// 块栈：active = 当前层是否在输出；taken = 本层已有命中分支。
	type blk struct{ active, taken bool }
	var stack []*blk
	var out []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if cond, ok := isIf(t); ok {
			hit, err := evalBranchExpr(context.Background(), condToExpr(cond), vars)
			if err != nil {
				return nil, fmt.Errorf("转换脚本条件 %q 求值失败: %w", cond, err)
			}
			stack = append(stack, &blk{active: hit, taken: hit})
			continue // if 行本身不输出
		}
		if isElse(t) && len(stack) > 0 {
			b := stack[len(stack)-1]
			b.active = !b.taken
			if b.active {
				b.taken = true
			}
			continue
		}
		// elif（python）/ } else if (...) {（javascript）：与 else 同层闭合上一链、
		// 开启新条件，避免被当作嵌套新块导致 endif 对不上。
		if cond, ok := isElif(t); ok && len(stack) > 0 {
			b := stack[len(stack)-1]
			hit := false
			if !b.taken {
				var err error
				hit, err = evalBranchExpr(context.Background(), condToExpr(cond), vars)
				if err != nil {
					return nil, fmt.Errorf("转换脚本条件 %q 求值失败: %w", cond, err)
				}
				if hit {
					b.taken = true
				}
			}
			b.active = hit && b.taken
			continue
		}
		if isEnd(t) && len(stack) > 0 {
			stack = stack[:len(stack)-1]
			continue
		}
		if len(stack) == 0 || stack[len(stack)-1].active {
			out = append(out, ln)
		}
	}
	if len(stack) > 0 {
		return nil, fmt.Errorf("转换脚本 if 块缺少收尾（endif 或 }）")
	}
	return out, nil
}

// transformArith 算术替换：{{x + 2}} → 求值文本；变量缺失/非数值时保留原文。
func transformArith(s string, vars map[string]any) string {
	return arithRe.ReplaceAllStringFunc(s, func(m string) string {
		g := arithRe.FindStringSubmatch(m)
		lf, err := varNum(g[1], vars)
		if err != nil {
			return m
		}
		rf, rerr := strconv.ParseFloat(g[3], 64)
		if rerr != nil {
			return m
		}
		var v float64
		switch g[2] {
		case "+":
			v = lf + rf
		case "-":
			v = lf - rf
		case "*":
			v = lf * rf
		case "/":
			if rf == 0 {
				return m // 除零容错：保留原文
			}
			v = lf / rf
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	})
}

// varNum 取变量数值（支持 a.b 嵌套引用，值可为数值或数值字符串）。
func varNum(ref string, vars map[string]any) (float64, error) {
	v, ok := lookupRef(ref, vars)
	if !ok {
		return 0, fmt.Errorf("未定义变量: %s", ref)
	}
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(n), 64)
	}
	return 0, fmt.Errorf("非数值变量: %s", ref)
}

// lookupRef 按点分路径取嵌套 map 值（与 FString {a.b} 同构）。
func lookupRef(ref string, vars map[string]any) (any, bool) {
	var cur any = vars
	for _, p := range strings.Split(ref, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}
