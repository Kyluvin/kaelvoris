// PromptTemplateParser 移植（逐字对齐 nodes/prompt_template.py）：
// 单大括号 {var} 提取与替换。蓝本用 re.findall + format_map；
// Go 用逐字符扫描（识别 {var} 与 {{、}} 转义语义与蓝本一致）。
package bisheng

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// varPattern 合法变量名：node_id.key / node_id.key#idx / key.key（蓝本
// extract 正则 [{]+(.*?)[}]+ 匹配后交给 get_variable_by_str 校验，此处
// 收敛为「点分 + 可选下标」形态，避免把 dict 字面量当变量）。
var varPattern = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+(?:#[^\}\.]+)?)\}`)

// ExtractTemplateVars 提取模板中的变量 key（去重，保序）。
func ExtractTemplateVars(template string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range varPattern.FindAllStringSubmatch(template, -1) {
		key := m[1]
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// FormatTemplate 变量替换（值以 %v 渲染；map 展开为 JSON 以便嵌套引用，
// 切片同）。缺失变量渲染为空串（蓝本 format_map(defaultdict) 语义）。
func FormatTemplate(template string, varMap map[string]any) string {
	return varPattern.ReplaceAllStringFunc(template, func(m string) string {
		key := varPattern.FindStringSubmatch(m)[1]
		v, ok := varMap[key]
		if !ok || v == nil {
			return ""
		}
		return renderValue(v)
	})
}

// renderValue 变量值渲染：字符串原样；map/slice 转 JSON（ensure_ascii=False
// 语义，Go 原生 UTF-8）；其余 %v。
func renderValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any, []any:
		return fmtJSON(t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// fmtSprintf workflow.go 用的小 fmt 别名（保持文件专注）。
func fmtSprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// fmtJSON 紧凑 JSON 序列化（失败回退 %v）。
func fmtJSON(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%v", v)
	}
	return strings.TrimRight(sb.String(), "\n")
}
