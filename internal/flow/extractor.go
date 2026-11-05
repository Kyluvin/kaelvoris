// 参数提取器节点（extractor）执行器 —— 2026-10 Dify parameter-extractor 对齐。
//
// Dify 对应节点：api/core/workflow/nodes/parameter-extractor（LLM 按描述从
// 输入文本提取结构化参数，JSON 解析失败可走失败分支）。
// Kaelvoris 语义（Step 复用 + LLM 复用 invokeLLM）：
//   - Step.Model    提取用模型（空 = 全局 LLM，经 llmFor 解析）
//   - Step.Template 提取指令 JSON：{"text": "{input}", "params": [{"name": "city",
//     "desc": "城市名", "required": true}, ...]}；text 支持 {var}/{nodeID.output}。
//   - 提取提示词自动拼装（指令 + 参数清单 + 输入文本），要求模型只回 JSON。
//   - 输出写回变量池：{nodeID.output} = 提取结果 JSON 串；解析后的每个参数
//     另写 {nodeID.<paramName>} 供下游点分引用。
//   - 严格模式：required 参数缺失 → 报错走失败路径（Dify fail-branch 同语义，
//     失败信息进轨迹，错误上抛由调用方决定是否中断）。
package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// extractParam 提取参数定义。
type extractParam struct {
	Name     string `json:"name"`
	Desc     string `json:"desc"`
	Required bool   `json:"required"`
}

// extractDef 提取指令定义（Step.Template 反序列化目标）。
type extractDef struct {
	Text   string         `json:"text"`
	Params []extractParam `json:"params"`
}

// extractJSONRe 无关前缀剥离用：提取首个 { 到末个 } 之间的内容（模型常见
// ```json 围栏/说明文字容错）。
func extractJSONStr(s string) string {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return ""
	}
	return s[i : j+1]
}

// execExtractor extractor 节点执行：拼装提取提示词 → LLM 单轮 → JSON 严格解析
// → 必填校验 → 结果写回变量池。返回（结果 JSON 串, 解析后参数 map, 错误）。
func (r *Runner) execExtractor(ctx context.Context, st *state, step Step, dryRun bool) (string, map[string]any, error) {
	def := extractDef{}
	if s := strings.TrimSpace(step.Template); s != "" {
		if err := json.Unmarshal([]byte(s), &def); err != nil {
			return "", nil, fmt.Errorf("提取指令 JSON 解析失败: %w", err)
		}
	}
	if len(def.Params) == 0 {
		return "", nil, fmt.Errorf("参数提取器未定义提取参数（params）")
	}
	mvars := st.mergedVars()
	text := def.Text
	if strings.Contains(text, "{") {
		if out, err := renderVars(ctx, text, mvars); err == nil {
			text = out
		}
	}
	if strings.TrimSpace(text) == "" {
		return "", nil, fmt.Errorf("参数提取器输入文本为空")
	}

	// 拼装提取提示词（对齐 Dify：指令 + 参数 schema + 输入，只回 JSON）。
	var sb strings.Builder
	sb.WriteString("你是参数提取引擎。从下面的输入文本中提取指定参数，只输出一个 JSON 对象，不要任何解释或代码围栏。\n\n提取参数：\n")
	for _, p := range def.Params {
		req := "可选"
		if p.Required {
			req = "必填"
		}
		sb.WriteString(fmt.Sprintf("- %s（%s）：%s\n", p.Name, req, p.Desc))
	}
	sb.WriteString("\n输入文本：\n" + text)

	if dryRun {
		return "(dry-run 参数提取，未调用模型)", nil, nil
	}

	// 单轮 LLM（复用 invokeLLM：模型选择 + 无 MCP/技能注入即单次 Generate）。
	ls := step
	ls.Type = StepLLM
	ls.MCPServers = nil
	ls.Skills = nil
	out, closeRes, err := r.invokeLLM(ctx, invokeLLMIn{
		step: ls,
		msgs: []*schema.Message{{Role: schema.User, Content: sb.String()}},
	})
	if closeRes != nil {
		defer closeRes()
	}
	if err != nil {
		return "", nil, fmt.Errorf("参数提取模型调用失败: %w", err)
	}

	// JSON 严格解析（容错剥围栏）。
	raw := extractJSONStr(out)
	if raw == "" {
		return "", nil, fmt.Errorf("参数提取失败：模型未返回 JSON（输出 %.120s）", out)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", nil, fmt.Errorf("参数提取失败：JSON 解析错误 %v（输出 %.120s）", err, raw)
	}
	// 必填校验 + 参数逐个写回变量池 {nodeID.<name>}。
	var missing []string
	for _, p := range def.Params {
		v, ok := parsed[p.Name]
		if !ok || v == nil || fmt.Sprint(v) == "" {
			if p.Required {
				missing = append(missing, p.Name)
			}
			continue
		}
	}
	if len(missing) > 0 {
		return "", nil, fmt.Errorf("参数提取失败：必填参数缺失 %s", strings.Join(missing, ", "))
	}
	// 全部通过：结果 JSON 作为节点输出，参数逐个入池（调用方以 NodeID 键写回）。
	resJSON, _ := json.Marshal(parsed)
	return string(resJSON), parsed, nil
}
