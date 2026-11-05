// Package tools 画图工具：generate_image（文生图/图生图，OpenAI images API 兼容端点）。
//
// 聊天内直接生图：LLM 调用本工具 → 图像落盘 ~/.kaelvoris/images/ → 结果尾部
// 以 <<<KAELVORIS_IMAGES:...>>> 载荷标记携带 {path, dataUrl, fileName} 列表 →
// 引擎 execTool 剥离标记后回填纯文本（base64 不进模型上下文），图片以
// SayImage 消息推送聊天流（实时用 dataUrl 展示；会话历史只留 path，
// 回放经 Wails assetserver /kaelvoris-image/ 路由回挂，见 main.go）。
//
// 端点兼容 OpenAI POST {base}/images/generations（gpt-image-1 / dall-e-3 /
// gemini-2.5-flash-image 等 OpenAI 兼容网关）。响应同时兼容 b64_json 与 url
// 两种返回（url 自动下载转 data URL）；不主动传 response_format（gpt-image-1
// 不支持该参数会 400，dall-e 返回 url 由本工具下载，兼容性最优）。
package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ImageGenSettings 画图端点配置（通道层按当前设置注入，任务运行中热读）。
type ImageGenSettings struct {
	BaseURL string // OpenAI 兼容根地址（如 https://api.openai.com）；空 = 未启用
	Model   string // 图像模型（gpt-image-1 / dall-e-3 / gemini-2.5-flash-image …）
	APIKey  string // 鉴权 Bearer（空 = 不带认证头，本地网关场景）
}

// GenerateImageTool 文生图工具。
type GenerateImageTool struct {
	// Settings 提供当前画图配置（nil 或配置不全 = 工具报错引导去设置页）。
	Settings func() ImageGenSettings
	// Dir 图片保存目录（空 = ~/.kaelvoris/images）。
	Dir string
}

func (t *GenerateImageTool) Name() string { return "generate_image" }

func (t *GenerateImageTool) Description() string {
	return "Generate an image from a text prompt (text-to-image), or edit/transform input images (image-to-image). " +
		"Use it whenever the user asks to draw / paint / generate a picture (e.g. 画一张熊猫图, draw a panda). " +
		"Pass image (array of local file paths, URLs or data URIs) to use reference images: " +
		"multi-image composition, style transfer, or character consistency (人物一致性：先用一张角色定妆照作参考，后续每张分镜图都带它). " +
		"The generated image is saved to disk AND displayed to the user in the chat automatically — " +
		"after this tool returns, briefly confirm what was drawn in one or two sentences; " +
		"never print base64 data and never embed markdown images. " +
		"If the tool reports that image generation is not configured, tell the user to set it up in 设置 → 模型 → 图像生成."
}

func (t *GenerateImageTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"prompt": strProp("Image description. Write concrete visual details: subject, scene, composition, style, colors, lighting."),
		"image":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Reference image(s) for image-to-image / multi-image composition: local file paths, URLs or data URIs. Keep the same character/subject across shots by reusing its portrait as a reference."},
		"ratio":  strProp(`Aspect ratio like "16:9" / "9:16" / "1:1" — used with tiered size (1K/2K/…) on Agnes-style endpoints; omit to use endpoint default`),
		"n":      numProp("Number of images to generate (default 1, max 4)"),
		"size":   strProp(`Image size like "1024x1024" / "1024x1792" / "1792x1024" / "512x512", or tiered "1K"/"2K" — must be supported by the model; omit to use the endpoint default`),
		"model":  strProp("Override image model for this call (default: the model configured in settings)"),
	}, []string{"prompt"})
}

func (t *GenerateImageTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Prompt string   `json:"prompt"`
		Image  []string `json:"image"` // 参考图（本地路径 / URL / data URI），非空 = 图生图/多图合成
		Ratio  string   `json:"ratio"`
		N      int      `json:"n"`
		Size   string   `json:"size"`
		Model  string   `json:"model"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	// 参考图解析：本地路径读文件转 data URI（Agnes image 参数同时接受 URL 与
	// Data URI Base64，本地路径 Agent 侧最常见）；URL 原样透传。
	for i, ref := range p.Image {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if !strings.HasPrefix(ref, "data:") && !strings.Contains(ref, "://") {
			raw, err := os.ReadFile(ref)
			if err != nil {
				return "", fmt.Errorf("read reference image %d: %w", i+1, err)
			}
			ref = "data:" + DetectImageMIME(raw) + ";base64," + base64.StdEncoding.EncodeToString(raw)
		}
		p.Image[i] = ref
	}
	var refs []string
	for _, ref := range p.Image {
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return "", fmt.Errorf("prompt is required")
	}
	if t.Settings == nil {
		return "", fmt.Errorf("image generation is not configured")
	}
	s := t.Settings()
	model := strings.TrimSpace(p.Model)
	if model == "" {
		model = strings.TrimSpace(s.Model)
	}
	if strings.TrimSpace(s.BaseURL) == "" || model == "" {
		return "", fmt.Errorf("image generation is not configured: set 图像生成 Base URL and 图像模型 in 设置 → 模型")
	}
	n := p.N
	if n <= 0 {
		n = 1
	}
	if n > 4 {
		n = 4
	}

	// 请求体：size 仅在显式给出时携带（部分模型只支持固定尺寸，空字段最兼容）。
	reqBody := map[string]interface{}{"model": model, "prompt": p.Prompt, "n": n}
	if sz := strings.TrimSpace(p.Size); sz != "" {
		reqBody["size"] = sz
	}
	if r := strings.TrimSpace(p.Ratio); r != "" {
		reqBody["ratio"] = r // Agnes 档位式 size 配套宽高比（16:9 / 9:16 / …）
	}
	if len(refs) > 0 {
		// 图生图/多图合成（Agnes image 参数：公共 URL 或 Data URI Base64）。
		// n 固定 1（图生图多张返回在多数网关不可用，缺省 1 最兼容）。
		reqBody["image"] = refs
		n = 1
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}
	url := strings.TrimRight(s.BaseURL, "/") + "/images/generations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
	}
	// 生图耗时显著高于聊天补全（10s~2min），给独立长超时兜底。
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image API %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), clipStr(string(body), 600))
	}
	var out struct {
		Data []struct {
			B64     string `json:"b64_json"`
			URL     string `json:"url"`
			Revised string `json:"revised_prompt"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse response: %w (body: %s)", err, clipStr(string(body), 400))
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", fmt.Errorf("image API error: %s", out.Error.Message)
	}
	if len(out.Data) == 0 {
		return "", fmt.Errorf("image API returned no data: %s", clipStr(string(body), 400))
	}

	// 逐图落盘 + 构造载荷（b64 直解码；url 先下载转 data URL）。
	dir := t.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			dir = filepath.Join(os.TempDir(), "kaelvoris-images")
		} else {
			dir = filepath.Join(home, ".kaelvoris", "images")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create image dir: %w", err)
	}
	stamp := time.Now().Format("20060102-150405")
	imgs := make([]ImagePayload, 0, len(out.Data))
	saved := make([]string, 0, len(out.Data))
	var revised string
	for i, d := range out.Data {
		var raw []byte
		switch {
		case strings.TrimSpace(d.B64) != "":
			raw, err = base64.StdEncoding.DecodeString(strings.TrimSpace(d.B64))
			if err != nil {
				return "", fmt.Errorf("decode image %d: %w", i+1, err)
			}
		case strings.TrimSpace(d.URL) != "":
			raw, err = t.download(ctx, client, strings.TrimSpace(d.URL))
			if err != nil {
				return "", fmt.Errorf("download image %d: %w", i+1, err)
			}
		default:
			return "", fmt.Errorf("image %d: response has neither b64_json nor url", i+1)
		}
		ext := ImageExt(DetectImageMIME(raw))
		name := fmt.Sprintf("%s-%d.%s", stamp, i+1, ext)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return "", fmt.Errorf("save image: %w", err)
		}
		imgs = append(imgs, ImagePayload{
			Path:     path,
			DataURL:  MustDataURL(raw),
			FileName: name,
		})
		saved = append(saved, path)
		if strings.TrimSpace(d.Revised) != "" && revised == "" {
			revised = strings.TrimSpace(d.Revised)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Generated %d image(s) (model %s", len(imgs), model)
	if sz := strings.TrimSpace(p.Size); sz != "" {
		fmt.Fprintf(&b, ", size %s", sz)
	}
	b.WriteString(").\n")
	for _, p := range saved {
		b.WriteString("Saved to: " + p + "\n")
	}
	if revised != "" {
		b.WriteString("Revised prompt: " + revised + "\n")
	}
	b.WriteString("The image has been shown to the user in the chat. Briefly confirm completion; " +
		"do NOT print base64 data and do NOT embed markdown images.")
	// 内嵌媒体载荷标记：引擎 execTool 剥离后推送聊天流展示，
	// 不进入 LLM 上下文（base64 动辄数 MB，回填即撑爆窗口）。
	b.WriteString(BuildMediaPayload(imgsToMedia(imgs)))
	return b.String(), nil
}

// download 下载 url 图片（上限 32MB）。
func (t *GenerateImageTool) download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// clipStr 截断字符串（错误信息防超长）。
func clipStr(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

var _ = base64.StdEncoding // 保持 base64 引用（MustDataURL 在 image_payload.go）
