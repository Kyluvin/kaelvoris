// HTTP 请求节点（http）执行器 —— 2026-10 Dify/Coze http-request 节点对齐。
//
// Dify 对应节点：api/core/workflow/nodes/http-request（方法+URL+头+体，
// 响应写回变量池供下游 {nodeID.output} / {nodeID.status} 引用）。
// Kaelvoris 语义（零新依赖，net/http 标准库直跑）：
//   - Step.Name   节点显示名（同其他节点）
//   - Step.Role   HTTP 方法（GET/POST/PUT/DELETE/PATCH/HEAD，默认 GET）
//   - Step.Template 请求定义 JSON：
//     {"url": "...", "headers": {"K": "V"}, "body": "...", "timeoutSec": 30}
//     url/headers 值/body 均支持 {var} 与 {nodeID.output} 渲染。
//   - 输出写回变量池：{nodeID.status} 状态码（数值）、{nodeID.output} 响应体
//     （文本；Content-Type 为 JSON 时输出原始体，模板可再经 transform 加工）。
//
// dry-run 不发请求，仅渲染 URL 并回显（与 knowledge 节点同语义）。
package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpDef 请求定义（Step.Template 反序列化目标）。
type httpDef struct {
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	TimeoutSec int               `json:"timeoutSec"`
}

// httpMethods http 节点合法方法（Step.Role 存储）。
var httpMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodDelete: true, http.MethodPatch: true, http.MethodHead: true,
}

// execHTTP http 节点执行：渲染定义 → 发请求 → 状态码+响应体写回变量池。
// 返回（轨迹文本, 响应体, 状态码, 错误）。
func (r *Runner) execHTTP(ctx context.Context, st *state, step Step, dryRun bool) (string, string, int, error) {
	method := strings.ToUpper(strings.TrimSpace(step.Role))
	if method == "" {
		method = http.MethodGet
	}
	if !httpMethods[method] {
		return "", "", 0, fmt.Errorf("不支持的 HTTP 方法: %s", step.Role)
	}
	def := httpDef{TimeoutSec: 30}
	if s := strings.TrimSpace(step.Template); s != "" {
		if err := json.Unmarshal([]byte(s), &def); err != nil {
			return "", "", 0, fmt.Errorf("请求定义 JSON 解析失败: %w", err)
		}
	}
	if strings.TrimSpace(def.URL) == "" {
		return "", "", 0, fmt.Errorf("HTTP 节点未配置 URL")
	}
	mvars := st.mergedVars()
	// 渲染：URL 与头值、体均支持 {var} / {nodeID.output}（渲染失败按原串）。
	render := func(s string) string {
		if s == "" || !strings.Contains(s, "{") {
			return s
		}
		if out, err := renderVars(ctx, s, mvars); err == nil {
			return out
		}
		return s
	}
	def.URL = render(def.URL)
	for k, v := range def.Headers {
		def.Headers[k] = render(v)
	}
	def.Body = render(def.Body)

	if dryRun {
		return fmt.Sprintf("(dry-run %s %s，未发送)", method, def.URL), "", 0, nil
	}

	timeout := time.Duration(def.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var bodyReader io.Reader
	if def.Body != "" && method != http.MethodGet && method != http.MethodHead {
		bodyReader = strings.NewReader(def.Body)
	}
	req, err := http.NewRequestWithContext(ctx2, method, def.URL, bodyReader)
	if err != nil {
		return "", "", 0, fmt.Errorf("构造请求失败: %w", err)
	}
	for k, v := range def.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // ponytail: 响应体上限 1MB，流式/大文件需要时再升级
	if err != nil {
		return "", "", 0, fmt.Errorf("读取响应失败: %w", err)
	}
	out := string(b)
	trace := fmt.Sprintf("%s %s → %d（%d 字节）", method, def.URL, resp.StatusCode, len(b))
	return trace, out, resp.StatusCode, nil
}
