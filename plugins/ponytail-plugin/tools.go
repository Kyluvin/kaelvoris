// 工具定义与 Kaelvoris 特有领域逻辑。
//
// 三个工具对应 Ponytail 守则的三个使用场景，对齐开源新版语义：
//   - ponytail_review：改代码前/后的 YAGNI 阶梯审查（精确输出协议：6 标签 + net: 计分）；
//   - ponytail_audit：对文件/目录做过度工程审计（仓库级扫描提示 + 清单）；
//   - ponytail_lint：对单个代码片段做「输出克制」检查（支持 lite/full 强度）；
//   - ponytail_debt：真实扫描工作区 `ponytail:` 注释，生成债务台账（含 no-trigger 腐化标记）。
//
// 与开源版差异：开源是纯提示词（模型自己跑 grep），此处 debt 为真实文件扫描，
// 模型拿到的事实（行号/内容）由工具保证，不依赖模型 grep 的准确性。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Tool 单个 MCP 工具定义。
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
	Execute     func(args json.RawMessage) (string, error)
}

// bindArgs 解析参数到结构体（宽容模式：未知字段忽略，缺省字段零值）。
func bindArgs(raw json.RawMessage, v interface{}) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// ---- JSON Schema 辅助 ----

// objectSchema 构造 object 类型 schema（properties + required）。
func objectSchema(props map[string]interface{}, required ...string) map[string]interface{} {
	s := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// strProp 字符串属性 schema。
func strProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

// ---- 工具注册 ----

// registerTools 注册全部工具。
func registerTools(s *Server) {
	register(s, &Tool{
		Name:        "ponytail_review",
		Description: "对一次代码改动做 YAGNI 阶梯审查（懒人资深工程师视角）。传入改动描述/要点或 diff，输出：该跳过的（speculative）、该复用的（已存在）、可简化的（一行能搞定）、以及提出的反问。用于改代码前后自检，防过度工程。",
		Parameters: objectSchema(map[string]interface{}{
			"changes": strProp("本次改动的代码/文件描述（粘贴 diff 或要点）"),
			"context": strProp("可选：任务背景、涉及的功能点"),
		}, "changes"),
		Execute: execReview,
	})
	register(s, &Tool{
		Name:        "ponytail_audit",
		Description: "对文件或目录做过度工程审计：找出多余抽象（单实现接口）、多余依赖（标准库能搞定的）、样板代码、未使用的配置。输入路径，输出审计清单 + 建议。",
		Parameters: objectSchema(map[string]interface{}{
			"path":  strProp("文件或目录路径（工作区相对路径）"),
			"depth": strProp("可选：扫描深度，默认 2"),
		}, "path"),
		Execute: execAudit,
	})
	register(s, &Tool{
		Name:        "ponytail_lint",
		Description: "对单段代码做「输出克制」检查：代码能否更短？解释是否比代码长？是否引入了没要求的抽象/依赖？输出精简建议（可一行化的行、可删的注释/解释）。",
		Parameters: objectSchema(map[string]interface{}{
			"code":      strProp("待检查的代码片段"),
			"verbosity": strProp("可选：检查强度 lite/full，默认 full"),
		}, "code"),
		Execute: execLint,
	})
	register(s, &Tool{
		Name:        "ponytail_debt",
		Description: "扫描工作区中所有 `ponytail:` 注释，生成债务台账：每个简写的位置、天花板（ceiling）、升级触发条件（upgrade）。无 upgrade 触发条件的标记 no-trigger（腐化风险）。输入可选 root（默认工作区根）。",
		Parameters: objectSchema(map[string]interface{}{
			"root": strProp("可选：扫描根目录（默认 os.Getwd()，即插件启动目录）"),
		}),
		Execute: execDebt,
	})
}

// register 注册工具到服务器。
func register(s *Server, t *Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[t.Name] = t
}

// ---- 共享常量 ----

// ladderRungs YAGNI 阶梯（七级）。
var ladderRungs = []string{
	"1. Does this need to be built at all? (YAGNI)",
	"2. Already in this codebase? Reuse the helper/util that exists.",
	"3. Standard library does it? Use it.",
	"4. Native platform feature covers it? Use it.",
	"5. Already-installed dependency solves it? Use it.",
	"6. Can it be one line? Make it one line.",
	"7. Only then: write the minimum code that works.",
}

// reviewTags 精确输出协议的 6 个标签（对齐开源 ponytail-review）。
var reviewTags = []string{
	"delete:", // 死代码/未用灵活性/投机特性 → 无替换
	"stdlib:", // 标准库已有 → 点名函数
	"native:", // 平台原生已有 → 点名特性
	"reuse:",  // 仓库已有等价实现 → 点名路径
	"yagni:",  // 单实现抽象/无人设置配置/单调用者层 → 内联
	"shrink:", // 同逻辑更少行 → 给出更短形式
}

// ---- ponytail_review ----

func execReview(args json.RawMessage) (string, error) {
	var req struct {
		Changes string `json:"changes"`
		Context string `json:"context"`
	}
	if err := bindArgs(args, &req); err != nil {
		return "", err
	}
	if strings.TrimSpace(req.Changes) == "" {
		return "", derr(errInvalidRequest, "changes 不能为空")
	}
	var b strings.Builder
	b.WriteString("PONYTAIL REVIEW — YAGNI 阶梯自检（对齐开源精确输出协议）\n")
	b.WriteString("改动要点: " + truncate(req.Changes, 400) + "\n")
	if strings.TrimSpace(req.Context) != "" {
		b.WriteString("任务背景: " + truncate(req.Context, 200) + "\n")
	}
	b.WriteString("\n逐级问自己（在理解问题之后跑，不在之前）:\n")
	for _, r := range ladderRungs {
		b.WriteString("  " + r + "\n")
	}
	b.WriteString("\n输出格式（每条一行，编号跨报告连续，用户可说「修 2 和 5」）:\n")
	b.WriteString("  <N>. L<line>: <tag> <what>. <replacement>.\n")
	b.WriteString("  tags: " + strings.Join(reviewTags, " ") + "\n")
	b.WriteString("  net: -<N> lines possible.\n")
	b.WriteString("\n铁律: 先读后懒。不理解的问题不允许懒——最小改动放在错误位置 = 第二个 bug。\n")
	b.WriteString("无物可砍时说: Lean already. Ship.\n")
	return b.String(), nil
}

// ---- ponytail_audit ----

func execAudit(args json.RawMessage) (string, error) {
	var req struct {
		Path  string `json:"path"`
		Depth string `json:"depth"`
	}
	if err := bindArgs(args, &req); err != nil {
		return "", err
	}
	if strings.TrimSpace(req.Path) == "" {
		return "", derr(errInvalidRequest, "path 不能为空")
	}
	depth := 2
	if strings.TrimSpace(req.Depth) != "" {
		fmt.Sscanf(req.Depth, "%d", &depth)
	}
	// 审计清单：静态规则（无文件系统依赖，保持插件零依赖）。
	var b strings.Builder
	b.WriteString("PONYTAIL AUDIT — 过度工程审计（仓库级扫描语义）\n")
	b.WriteString("目标: " + req.Path + " (depth=" + itoa(depth) + ")\n\n")
	b.WriteString("猎取目标（对齐开源 ponytail-audit Hunt 列表）:\n")
	b.WriteString("  - 标准库/平台已提供的依赖\n")
	b.WriteString("  - 单实现接口、单产品工厂\n")
	b.WriteString("  - 只做转发的 wrapper\n")
	b.WriteString("  - 只导出一个东西的文件\n")
	b.WriteString("  - 死 flag / 死配置\n")
	b.WriteString("  - 手造的标准库、与仓库已有等价实现重复的 helper\n")
	b.WriteString("发出 delete: 前必须全树 grep 该符号（含测试/fixture/字符串/动态引用）\n\n")
	b.WriteString("检查清单（逐项对照此文件/目录）:\n")
	b.WriteString("  [ ] 多余抽象: 接口只有单个实现？工厂只为单个产品？配置给永不变化的值?\n")
	b.WriteString("  [ ] 多余依赖: 标准库能搞定的却引了新依赖?\n")
	b.WriteString("  [ ] 样板代码: 没人要的 boilerplate / '为了以后' 的脚手架?\n")
	b.WriteString("  [ ] 未使用配置: config 字段从不读取?\n")
	b.WriteString("  [ ] 过度分层: 一层转发一层的 wrapper?\n")
	b.WriteString("  [ ] 重复实现: 项目里已有此功能（复用了没有）?\n")
	b.WriteString("\n输出格式（每条一行，按砍掉收益排序）:\n")
	b.WriteString("  <N>. <tag> <what to cut>. <replacement>. [path]\n")
	b.WriteString("  结尾: net: -<N> lines, -<M> deps possible.\n")
	b.WriteString("  无物可砍: Lean already. Ship.\n")
	b.WriteString("\n边界: 只查过度工程。正确性 bug/安全洞/性能 明确出界，交给常规 review。\n")
	return b.String(), nil
}

// ---- ponytail_lint ----

func execLint(args json.RawMessage) (string, error) {
	var req struct {
		Code      string `json:"code"`
		Verbosity string `json:"verbosity"`
	}
	if err := bindArgs(args, &req); err != nil {
		return "", err
	}
	if strings.TrimSpace(req.Code) == "" {
		return "", derr(errInvalidRequest, "code 不能为空")
	}
	full := strings.TrimSpace(req.Verbosity) != "lite"
	var b strings.Builder
	b.WriteString("PONYTAIL LINT — 输出克制检查\n")
	b.WriteString("代码行数: " + itoa(strings.Count(req.Code, "\n")+1) + "\n\n")
	b.WriteString("检查规则:\n")
	if full {
		b.WriteString("  [ ] 能否更短? 标准库/已有依赖/一行能搞定?\n")
		b.WriteString("  [ ] 解释比代码长? → 删解释（用户明确要的说明不算债）\n")
		b.WriteString("  [ ] 没要求的抽象? 单实现接口/单产品工厂?\n")
		b.WriteString("  [ ] 没要求的依赖? 能避免吗?\n")
	} else {
		b.WriteString("  [ ] 能否更短?\n  [ ] 解释比代码长?\n")
	}
	b.WriteString("\n可信输出:\n")
	b.WriteString("  - SIMPLIFY: <可一行化的行>\n")
	b.WriteString("  - TRIM: <可删的注释/解释>\n")
	b.WriteString("  - ABSTRACT: <去掉的没要求抽象>\n")
	b.WriteString("\n但: 非平凡逻辑（分支/循环/解析/资金安全路径）必须留一个可运行检查——\n")
	b.WriteString("  assert 自检或一个小测试，无框架无夹具（YAGNI 对测试同样适用）。\n")
	return b.String(), nil
}

// ---- ponytail_debt（真实扫描，非提示词）----

// debtMarker 匹配注释中的 ponytail: 标记（行内任意位置）。
// 覆盖 // # /* */ 前缀；排除仅提及"ponytail"但无冒号键的散文（由 : 保证）。
var debtMarker = regexp.MustCompile(`(?i)(?:ponytail\s*:)(.*)$`)

// debtEntry 一条债务记录。
type debtEntry struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Text      string `json:"text"` // 标记后至行尾
	NoTrigger bool   `json:"noTrigger"`
}

// execDebt 递归扫描 root 下所有文本文件的 ponytail: 注释，生成台账。
// 跳过 .git、node_modules、dist、build、bin、vendor 等噪音目录（避免误报 + 快）。
func execDebt(args json.RawMessage) (string, error) {
	var req struct {
		Root string `json:"root"`
	}
	if err := bindArgs(args, &req); err != nil {
		return "", err
	}
	root := strings.TrimSpace(req.Root)
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", derr(errNoContent, "cannot determine working directory: %v", err)
		}
		root = wd
	}
	root, _ = filepath.Abs(root)

	entries, err := scanDebt(root)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "No ponytail: debt. Clean ledger.\n", nil
	}

	// 按文件分组（对齐开源输出：每组文件一行记录）。
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].File != entries[j].File {
			return entries[i].File < entries[j].File
		}
		return entries[i].Line < entries[j].Line
	})

	noTrigger := 0
	var b strings.Builder
	b.WriteString("PONYTAIL DEBT — 债务台账（真实扫描 " + root + "）\n\n")
	curFile := ""
	for _, e := range entries {
		if e.File != curFile {
			curFile = e.File
			b.WriteString("\n" + relPath(root, e.File) + "\n")
		}
		tag := ""
		if e.NoTrigger {
			tag = " no-trigger"
			noTrigger++
		}
		// ceiling/upgrade 由模型从文本中提取（保持输出一行可读）。
		b.WriteString(fmt.Sprintf("  L%d, %s. %s%s\n", e.Line, e.Text, "ceiling/upgrade: 见注释", tag))
	}
	b.WriteString(fmt.Sprintf("\n%d markers, %d with no trigger.\n", len(entries), noTrigger))
	return b.String(), nil
}

// scanDebt 递归扫描目录，收集所有 ponytail: 标记。
func scanDebt(root string) ([]debtEntry, error) {
	var out []debtEntry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 不可读目录/文件跳过（不致命）
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isTextFile(d.Name()) {
			return nil
		}
		if entries, ok := scanFile(path); ok {
			out = append(out, entries...)
		}
		return nil
	})
	return out, err
}

// skipDir 跳过噪音目录（对齐开源 --exclude-dir 语义，含构建产物/依赖/二进制/前端源码）。
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "build", "bin", "vendor", "__pycache__",
		".venv", "target", ".idea", ".vscode", "frontend-vue", "frontend",
		"web-dist", "release", "out", "coverage", ".next", ".nuxt":
		return true
	}
	return false
}

// isTextFile 按扩展名粗判文本文件：只扫**代码文件**（债务标记只出现在代码注释中，
// 文档/配置里的解释性提及不入台账——对齐开源「注释前缀排除散文」语义）。
func isTextFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java", ".c", ".cpp", ".h",
		".sh", ".ps1", ".sql", ".vue", ".svelte", ".rb", ".php", ".kt", ".swift", ".cs":
		return true
	}
	return false
}

// scanFile 扫描单个文件，收集 ponytail: 标记（限制扫描行以防超大文件/二进制）。
func scanFile(path string) ([]debtEntry, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var out []debtEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		if lineNo > 20000 {
			break
		}
		line := sc.Text()
		if !strings.Contains(strings.ToLower(line), "ponytail:") {
			continue
		}
		m := debtMarker.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		text := strings.TrimSpace(m[1])
		if text == "" {
			text = "(empty marker)"
		}
		if isExplanatory(text) {
			continue // 仅提及约定的散文（如规则说明注释），不入台账
		}
		out = append(out, debtEntry{
			File:      path,
			Line:      lineNo,
			Text:      truncate(text, 160),
			NoTrigger: !hasUpgrade(text),
		})
	}
	return out, len(out) > 0
}

// isExplanatory 判断标记文本是否是「仅提及约定」的解释性文字（规则说明/示例/文档），
// 而非真实债务记录。对齐开源语义：prose that merely mentions the convention 不入台账。
// 关键词弱启发：解释句通常包含 comment/convention/marker/说明/约定/示例 等元词。
func isExplanatory(text string) bool {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "comment naming"), strings.Contains(t, "the convention"),
		strings.Contains(t, "comment that names"), strings.Contains(t, "into a debt ledger"),
		strings.Contains(t, "shortcut comments into"), strings.Contains(t, "注释标注天花板"),
		strings.Contains(t, "简写注释收集"), strings.Contains(t, "说明"), strings.Contains(t, "示例"),
		strings.Contains(t, "约定"):
		return true
	}
	return false
}

// hasUpgrade 判断标记文本是否包含升级触发条件（关键词弱启发；真实语义由模型提取）。
// 有明确的"升级路径/触发条件"迹象 → 不算 no-trigger。腐化风险仅当完全没有。
func hasUpgrade(text string) bool {
	t := strings.ToLower(text)
	if strings.Contains(t, "if throughput") || strings.Contains(t, "when ") ||
		strings.Contains(t, "if ") || strings.Contains(t, "upgrade") ||
		strings.Contains(t, "whenever") || strings.Contains(t, "需要") ||
		strings.Contains(t, "如果") || strings.Contains(t, "再") || strings.Contains(t, "一旦") {
		return true
	}
	return false
}

// ---- 小工具 ----

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// relPath 相对路径展示（root 下取相对，root 上保留绝对）。
func relPath(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil || strings.HasPrefix(r, "..") {
		return p
	}
	return filepath.ToSlash(r)
}
