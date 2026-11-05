// Package tools 媒体生成工具族：generate_video（文生视频/图生视频）与 generate_speech（语音合成/TTS）。
//
// 与 generate_image（image_tools.go）同族：LLM 调用工具 → 媒体落盘
// ~/.kaelvoris/media/{video,audio}/ → 结果尾部以 <<<KAELVORIS_MEDIA:...:KAELVORIS_MEDIA>>>
// 载荷标记携带 {kind, path, dataUrl, fileName, mime} 列表 → 引擎 execTool 剥离
// 标记后回填纯文本（二进制不进模型上下文），媒体以 SayVideo / SayAudio 消息
// 推送聊天流（实时用 dataUrl 展示；会话历史只留 path，回放经 Wails assetserver
// /kaelvoris-media/ 路由回挂，见 main.go）。
//
// 视频端点：POST {base}/videos（OpenAI Sora 视频家族兼容 + Agnes 扩展）。
// 图生视频（Agnes 系网关）三种模式：
//   - mode=keyframe：首尾帧控制。first_frame / last_frame 至少一个（图片本地路径
//     自动经 Upload 钩子换取公网 URL —— 视频网关要求媒体公网可访问）；
//   - mode=reference：图片/音频参考生成。images（≤5）参考图保人物/风格一致性；
//   - mode=text：纯文生视频（默认）。响应三种形态兼容：
//     2. Sora 异步任务：响应含 id 无 url/b64 → 轮询 GET {base}/videos/{id} 至
//     status=succeeded → GET {base}/videos/{id}/content 下载（最长 10 分钟）；
//     3. 直接 200 二进制流（Content-Type video/*，部分网关直落字节）。
//
// 语音端点：POST {base}/audio/speech（OpenAI /v1/audio/speech 完全兼容）：
// 请求 {model, input, voice, response_format?, speed?}，响应为音频二进制流
// （mp3/wav/opus…按 Content-Type 嗅探扩展名落盘）。
package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// VideoGenSettings 视频生成端点配置（通道层按当前设置注入，任务运行中热读）。
type VideoGenSettings struct {
	BaseURL string // OpenAI videos 兼容根地址；空 = 未启用
	Model   string // 视频模型（sora-2 / veo-3 / kling-v2 …）
	APIKey  string // 鉴权 Bearer（空 = 不带认证头，本地网关场景）
	// Mode 请求模式（Agnes 系网关必填：text / keyframe / reference；空 = 不携带，
	// 走 Sora 原生语义）。同时作为轮询路径判据：Mode 非空 → Agnes /agnesapi 查询。
	Mode string
	// Size 默认分辨率（Agnes 仅支持 "720P"；空 = 不携带，Sora 用 1280x720 形态）。
	Size string
}

// GenerateVideoTool 追加：媒体上传钩子。
//
// Upload 把本地图片字节换取公网 URL（图生视频 first_frame/last_frame/images
// 参数要求媒体 URL 公网可访问；桌面端用服务端图床 /api/images → /img/<file>，
// 未配置服务端或上传失败时返回错误，工具报错引导模型改走文生视频）。
type uploadFn func(ctx context.Context, data []byte, name string) (string, error)

// SpeechSettings 语音合成（TTS）端点配置。
type SpeechSettings struct {
	BaseURL string // OpenAI audio/speech 兼容根地址；空 = 未启用
	Model   string // TTS 模型（tts-1 / tts-1-hd / gpt-4o-mini-tts …）
	APIKey  string // 鉴权 Bearer（空 = 不带认证头）
	Voice   string // 默认音色（工具未显式指定时使用；空 = 服务端默认）
}

// GenerateVideoTool 文生视频/图生视频工具。
type GenerateVideoTool struct {
	// Settings 提供当前视频生成配置（nil 或配置不全 = 工具报错引导去设置页）。
	Settings func() VideoGenSettings
	// Dir 视频保存目录（空 = ~/.kaelvoris/media/video）。
	Dir string
	// Upload 本地图片 → 公网 URL 钩子（图生视频用；nil = 仅支持直连 URL）。
	Upload uploadFn
}

func (t *GenerateVideoTool) Name() string { return "generate_video" }

func (t *GenerateVideoTool) Description() string {
	return "Generate a video from a text prompt (text-to-video), or animate images (image-to-video). " +
		"Use it whenever the user asks to create / generate a video clip (e.g. 生成一段日出延时视频, make a video of a cat). " +
		"Image-to-video (Agnes-style gateways): pass mode=keyframe with first_frame/last_frame (local paths or URLs) for start/end frame control, " +
		"or mode=reference with images (up to 5) to keep character/style consistent (人物一致性：同一张角色图作参考，每段视频都带上). " +
		"Local image paths are auto-uploaded to a public URL when the server storage is configured. " +
		"The generated video is saved to disk AND displayed to the user in the chat automatically — " +
		"after this tool returns, briefly confirm what was generated in one or two sentences. " +
		"If the tool reports that video generation is not configured, tell the user to set it up in 设置 → 模型 → 视频生成; but note that when frame/reference images are given, generate_video automatically falls back to stitching them into a looping animated GIF locally (free, no video model needed)."
}

func (t *GenerateVideoTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"prompt":       strProp("Video description. Write concrete visual details: subject, scene, camera movement, style, lighting, duration feel."),
		"mode":         strProp(`Generation mode: "text" (default) / "keyframe" (first/last frame control) / "reference" (image/audio reference; Agnes-style gateways)`),
		"first_frame":  strProp(`keyframe mode: first frame image — local path or public URL; provide at least one of first_frame/last_frame`),
		"last_frame":   strProp(`keyframe mode: last frame image — local path or public URL`),
		"images":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": `reference mode: up to 5 reference image paths/URLs to keep character/style consistent across shots`},
		"aspect_ratio": strProp(`Aspect ratio like "16:9" / "9:16" / "1:1" (Agnes-style gateways; omit for default)`),
		"seconds":      numProp("Video duration in seconds (default 5; clamp to what the endpoint supports, e.g. 4/8/12 for sora-2)"),
		"size":         strProp(`Resolution like "1280x720" / "720x1280" — must be supported by the model; omit to use the endpoint default`),
		"model":        strProp("Override video model for this call (default: the model configured in settings)"),
	}, []string{"prompt"})
}

func (t *GenerateVideoTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Prompt      string   `json:"prompt"`
		Mode        string   `json:"mode"`
		FirstFrame  string   `json:"first_frame"`
		LastFrame   string   `json:"last_frame"`
		Images      []string `json:"images"`
		AspectRatio string   `json:"aspect_ratio"`
		Seconds     float64  `json:"seconds"`
		Size        string   `json:"size"`
		Model       string   `json:"model"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return "", fmt.Errorf("prompt is required")
	}
	// 图生视频模式判定：显式 mode 优先；给帧/参考图未写 mode 时自动归入
	// keyframe/reference（宽容处理，避免模型漏传 mode 直接 400）。
	mode := strings.ToLower(strings.TrimSpace(p.Mode))
	if mode == "" {
		if p.FirstFrame != "" || p.LastFrame != "" {
			mode = "keyframe"
		} else if len(p.Images) > 0 {
			mode = "reference"
		}
	}
	switch mode {
	case "", "text": // 纯文生视频
	case "keyframe":
		if strings.TrimSpace(p.FirstFrame) == "" && strings.TrimSpace(p.LastFrame) == "" {
			return "", fmt.Errorf("keyframe mode requires first_frame or last_frame (at least one)")
		}
	case "reference":
		if len(p.Images) == 0 {
			return "", fmt.Errorf("reference mode requires images (up to 5)")
		}
		if len(p.Images) > 5 {
			return "", fmt.Errorf("images length must not exceed 5")
		}
	default:
		return "", fmt.Errorf("invalid mode %q: want text / keyframe / reference", mode)
	}
	// 视频模型未配置（Settings nil / BaseURL·Model 空）判定需在 resolve（图床
	// 上传）之前：未配置时 keyframe/reference 直接走本地 GIF 降级（无需图床）。
	var s VideoGenSettings
	if t.Settings != nil {
		s = t.Settings()
	}
	model := strings.TrimSpace(p.Model)
	if model == "" {
		model = strings.TrimSpace(s.Model)
	}
	if strings.TrimSpace(s.BaseURL) == "" || model == "" {
		switch mode {
		case "keyframe":
			return t.gifFallback(ctx, p.FirstFrame, p.LastFrame, nil, p.Seconds)
		case "reference":
			return t.gifFallback(ctx, "", "", p.Images, p.Seconds)
		}
		return "", fmt.Errorf("video generation is not configured: set 视频生成 Base URL and 视频模型 in 设置 → 模型")
	}
	// 帧图与参考图解析为公网 URL：本地路径经 Upload 钩子换取图床 URL（视频网关
	// 要求媒体公网可访问，见 SKILL.md storyboard-video 工作流）；URL/data URI 原样透传。
	resolve := func(ref, what string) (string, error) {
		ref = strings.TrimSpace(ref)
		if ref == "" || strings.Contains(ref, "://") {
			return ref, nil // 空或已是 URL
		}
		// 本地路径 / data URI：先探 Upload 钩子（错误信息前置，未配图床时
		// 引导清晰；再读文件，避免不存在文件时误报「未配图床」）。
		if t.Upload == nil {
			return "", fmt.Errorf("%s: local file given but no media upload configured (set 服务端图床 or pass a public URL)", what)
		}
		var raw []byte
		if strings.HasPrefix(ref, "data:") {
			// data URI：解开成字节再上传（部分网关不收 data URI，统一走图床最稳）。
			j := strings.Index(ref, ",")
			if j < 0 {
				return "", fmt.Errorf("%s: malformed data URI", what)
			}
			b, err := base64.StdEncoding.DecodeString(ref[j+1:])
			if err != nil {
				return "", fmt.Errorf("%s: decode data URI: %w", what, err)
			}
			raw = b
		} else {
			r, err := os.ReadFile(ref)
			if err != nil {
				return "", fmt.Errorf("%s: read local file: %w", what, err)
			}
			raw = r
		}
		return t.uploadMedia(ctx, raw, what)
	}
	first, err := resolve(p.FirstFrame, "first_frame")
	if err != nil {
		return "", err
	}
	last, err := resolve(p.LastFrame, "last_frame")
	if err != nil {
		return "", err
	}
	imgURLs := make([]string, 0, len(p.Images))
	for i, im := range p.Images {
		u, err := resolve(im, fmt.Sprintf("images[%d]", i))
		if err != nil {
			return "", err
		}
		if u != "" {
			imgURLs = append(imgURLs, u)
		}
	}
	if mode == "reference" && len(imgURLs) == 0 {
		return "", fmt.Errorf("reference mode: all images failed to resolve to URLs")
	}

	// 请求体：可选字段仅在显式给出时携带（部分模型对空字段最敏感）。
	reqBody := map[string]interface{}{"model": model, "prompt": p.Prompt}
	// mode：Agnes 系网关必填（缺失 400：mode is required）。工具参数优先，
	// 其次设置默认值；图生视频分支已在上方校验必需媒体。
	if mode != "" {
		reqBody["mode"] = mode
	} else if m := strings.TrimSpace(s.Mode); m != "" {
		reqBody["mode"] = m
	}
	// 图生视频媒体字段（keyframe/reference 模式；text 模式下发会被网关 400 拒绝）。
	if first != "" {
		reqBody["first_frame"] = first
	}
	if last != "" {
		reqBody["last_frame"] = last
	}
	if len(imgURLs) > 0 {
		reqBody["images"] = imgURLs
	}
	if ar := strings.TrimSpace(p.AspectRatio); ar != "" {
		reqBody["aspect_ratio"] = ar // Agnes 画幅（16:9/9:16/…，size=720P 时选择输出尺寸）
	}
	if p.Seconds > 0 {
		if strings.TrimSpace(s.Mode) != "" {
			reqBody["seconds"] = strconv.FormatFloat(p.Seconds, 'f', -1, 64) // Agnes 文档示例为字符串（"5"）
		} else {
			reqBody["seconds"] = p.Seconds
		}
	}
	// size：工具参数优先，其次配置默认值（Agnes 仅支持 "720P"）。
	sz := strings.TrimSpace(p.Size)
	if sz == "" {
		sz = strings.TrimSpace(s.Size)
	}
	if sz != "" {
		reqBody["size"] = sz
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}
	url := strings.TrimRight(s.BaseURL, "/") + "/videos"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
	}
	// 视频生成耗时远高于聊天补全（30s~10min），给独立长超时兜底。
	client := &http.Client{Timeout: 10 * time.Minute}
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
		return "", fmt.Errorf("video API %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), clipStr(string(body), 600))
	}

	// 形态 3：直接二进制流（Content-Type video/* 或 application/octet-stream）。
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "video/") || ct == "application/octet-stream" {
		return t.saveAndReport(model, body, ct)
	}

	// 形态 1/2：JSON 同步返回 或 Sora 异步任务（轮询 /videos/{id} → /content）。
	var out struct {
		ID      string `json:"id"`
		VideoID string `json:"video_id"` // Agnes：任务查询专用 ID（/agnesapi?video_id=）
		TaskID  string `json:"task_id"`
		Status  string `json:"status"`
		Data    []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
		URL      string `json:"url"`
		B64      string `json:"b64_json"`
		VideoURL string `json:"video_url"`
		Error    *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse response: %w (body: %s)", err, clipStr(string(body), 400))
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", fmt.Errorf("video API error: %s", out.Error.Message)
	}

	var url2 string
	var b64 string
	if len(out.Data) > 0 {
		url2, b64 = strings.TrimSpace(out.Data[0].URL), strings.TrimSpace(out.Data[0].B64)
	}
	if url2 == "" {
		url2 = strings.TrimSpace(out.VideoURL)
	}
	if url2 == "" {
		url2 = strings.TrimSpace(out.URL)
	}
	if b64 == "" {
		b64 = strings.TrimSpace(out.B64)
	}
	switch {
	case b64 != "":
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", fmt.Errorf("decode video: %w", err)
		}
		return t.saveAndReport(model, raw, "")
	case url2 != "":
		raw, err := t.download(ctx, client, url2)
		if err != nil {
			return "", fmt.Errorf("download video: %w", err)
		}
		return t.saveAndReport(model, raw, "")
	case strings.TrimSpace(out.VideoID) != "":
		// Agnes 系网关：video_id 优先，走 /agnesapi?video_id=&model_name= 查询端点
		//（响应顶层 url 为成片地址；文档建议 1-2s 轮询，最长 15 分钟）。
		raw, ct, err := t.pollAgnes(ctx, client, s, strings.TrimSpace(out.VideoID), model)
		if err != nil {
			return "", err
		}
		return t.saveAndReport(model, raw, ct)
	case strings.TrimSpace(out.ID) != "":
		// Sora 异步任务：轮询任务状态直至 succeeded/failed（最长 10 分钟）。
		raw, ct, err := t.pollSora(ctx, client, s, strings.TrimSpace(out.ID))
		if err != nil {
			return "", err
		}
		return t.saveAndReport(model, raw, ct)
	default:
		return "", fmt.Errorf("video API returned no video data: %s", clipStr(string(body), 400))
	}
}

// pollAgnes Agnes 系网关任务轮询：GET {base去/v1}/agnesapi?video_id=&model_name=
// 直至 status=completed（顶层 url 为成片地址）或 failed。2s 间隔，最长 15 分钟。
// 文档：https://wiki.agnes-ai.cn/zh-Hans/docs/agnes-video-25-flash
func (t *GenerateVideoTool) pollAgnes(ctx context.Context, client *http.Client, s VideoGenSettings, videoID, model string) ([]byte, string, error) {
	base := strings.TrimRight(s.BaseURL, "/")
	pollURL := strings.TrimSuffix(base, "/v1") + "/agnesapi"
	deadline := time.Now().Add(15 * time.Minute)
	for {
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("video task %s timed out after 15m", videoID)
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL+"?video_id="+url.QueryEscape(videoID)+"&model_name="+url.QueryEscape(model), nil)
		if err != nil {
			return nil, "", err
		}
		if s.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+s.APIKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("poll failed: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, "", fmt.Errorf("read poll response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("poll status %d: %s", resp.StatusCode, clipStr(string(body), 400))
		}
		var st struct {
			Status string `json:"status"`
			URL    string `json:"url"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &st); err != nil {
			return nil, "", fmt.Errorf("parse poll response: %w (body: %s)", err, clipStr(string(body), 300))
		}
		if st.Error != nil && st.Error.Message != "" {
			return nil, "", fmt.Errorf("video API error: %s", st.Error.Message)
		}
		switch strings.ToLower(strings.TrimSpace(st.Status)) {
		case "completed", "succeeded", "success":
			if u := strings.TrimSpace(st.URL); u != "" {
				raw, err := t.download(ctx, client, u)
				if err != nil {
					return nil, "", fmt.Errorf("download video: %w", err)
				}
				return raw, "", nil
			}
			return nil, "", fmt.Errorf("video task %s completed but no url in response: %s", videoID, clipStr(string(body), 300))
		case "failed", "error", "cancelled":
			msg := ""
			if st.Error != nil {
				msg = st.Error.Message
			}
			return nil, "", fmt.Errorf("video task %s failed: %s", st.Status, clipStr(msg, 300))
		default:
			// queued / in_progress / processing … 继续轮询。
		}
	}
}

// pollSora Sora 异步任务轮询：GET {base}/videos/{id} 直至 status=succeeded，
// 再 GET {base}/videos/{id}/content 下载视频字节。10s 间隔，最长 10 分钟。
func (t *GenerateVideoTool) pollSora(ctx context.Context, client *http.Client, s VideoGenSettings, id string) ([]byte, string, error) {
	base := strings.TrimRight(s.BaseURL, "/")
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("video task %s timed out after 10m", id)
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/videos/"+id, nil)
		if err != nil {
			return nil, "", err
		}
		if s.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+s.APIKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("poll failed: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, "", fmt.Errorf("read poll response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("poll status %d: %s", resp.StatusCode, clipStr(string(body), 400))
		}
		var st struct {
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &st); err != nil {
			return nil, "", fmt.Errorf("parse poll response: %w", err)
		}
		if st.Error != nil && st.Error.Message != "" {
			return nil, "", fmt.Errorf("video API error: %s", st.Error.Message)
		}
		switch strings.ToLower(st.Status) {
		case "succeeded", "completed", "success":
			// 下载成片（/videos/{id}/content；部分网关直接在任务对象给 url）。
			var withURL struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(body, &withURL)
			if u := strings.TrimSpace(withURL.URL); u != "" {
				raw, err := t.download(ctx, client, u)
				if err != nil {
					return nil, "", fmt.Errorf("download video: %w", err)
				}
				return raw, "", nil
			}
			req2, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/videos/"+id+"/content", nil)
			if err != nil {
				return nil, "", err
			}
			if s.APIKey != "" {
				req2.Header.Set("Authorization", "Bearer "+s.APIKey)
			}
			resp2, err := client.Do(req2)
			if err != nil {
				return nil, "", fmt.Errorf("download content failed: %w", err)
			}
			defer resp2.Body.Close()
			if resp2.StatusCode != http.StatusOK {
				return nil, "", fmt.Errorf("download content %d %s", resp2.StatusCode, http.StatusText(resp2.StatusCode))
			}
			raw, err := io.ReadAll(io.LimitReader(resp2.Body, 512<<20))
			if err != nil {
				return nil, "", fmt.Errorf("read video: %w", err)
			}
			return raw, resp2.Header.Get("Content-Type"), nil
		case "failed", "error", "cancelled":
			msg := ""
			if st.Error != nil {
				msg = st.Error.Message
			}
			return nil, "", fmt.Errorf("video task %s failed: %s", st.Status, clipStr(msg, 300))
		default:
			// queued / in_progress … 继续轮询。
		}
	}
}

func (t *GenerateVideoTool) videoDir() string {
	dir := t.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			dir = filepath.Join(os.TempDir(), "kaelvoris-media", "video")
		} else {
			dir = filepath.Join(home, ".kaelvoris", "media", "video")
		}
	}
	return dir
}

// saveAndReport 视频字节落盘 + 构造 KAELVORIS_MEDIA 载荷。
func (t *GenerateVideoTool) saveAndReport(model string, raw []byte, ct string) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("video API returned empty body")
	}
	dir := t.videoDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create video dir: %w", err)
	}
	stamp := time.Now().Format("20060102-150405")
	name := fmt.Sprintf("%s-video.mp4", stamp)
	if ext := MediaExtFromMIME(ct); ext != "" && ext != "mp4" {
		name = fmt.Sprintf("%s-video.%s", stamp, ext)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", fmt.Errorf("save video: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Generated 1 video (model %s).\n", model)
	b.WriteString("Saved to: " + path + "\n")
	b.WriteString("The video has been shown to the user in the chat. Briefly confirm completion; " +
		"do NOT print base64 data and do NOT embed markdown links to the raw file.")
	b.WriteString(BuildMediaPayload([]MediaPayload{{
		Path:     path,
		DataURL:  "", // 视频体积大（可达百 MB），不构造 data URL，前端经 /kaelvoris-media/ 回挂
		FileName: name,
		MIME:     MediaMIMEFromName(name),
		Kind:     "video",
	}}))
	return b.String(), nil
}

// uploadMedia 本地图片字节 → 公网 URL（Upload 钩子桥接；nil/失败报错，
// 引导模型改走文生视频或直连 URL）。
func (t *GenerateVideoTool) uploadMedia(ctx context.Context, data []byte, what string) (string, error) {
	if t.Upload == nil {
		return "", fmt.Errorf("%s: local file given but no media upload configured (set 服务端图床 or pass a public URL)", what)
	}
	name := fmt.Sprintf("i2v-%s.png", strings.ReplaceAll(what, "[", ""))
	u, err := t.Upload(ctx, data, name)
	if err != nil {
		return "", fmt.Errorf("%s: upload to media storage failed: %w", what, err)
	}
	if u == "" {
		return "", fmt.Errorf("%s: upload returned empty url", what)
	}
	return u, nil
}

// download 下载 url 媒体（上限 512MB，视频场景）。
func (t *GenerateVideoTool) download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
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
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}

// GenerateSpeechTool 语音合成（TTS）工具：OpenAI POST {base}/audio/speech 兼容。
type GenerateSpeechTool struct {
	// Settings 提供当前 TTS 配置（nil 或配置不全 = 工具报错引导去设置页）。
	Settings func() SpeechSettings
	// Dir 音频保存目录（空 = ~/.kaelvoris/media/audio）。
	Dir string
}

func (t *GenerateSpeechTool) Name() string { return "generate_speech" }

func (t *GenerateSpeechTool) Description() string {
	return "Convert text to speech audio (text-to-speech / TTS). " +
		"Use it when the user asks to read text aloud / 朗读 / 配音 / generate an audio clip, " +
		"or when producing narration for a video. The audio file is saved to disk AND playable " +
		"in the chat automatically. After this tool returns, briefly confirm in one sentence; " +
		"never print base64 data. If the tool reports speech is not configured, tell the user " +
		"to set it up in 设置 → 模型 → 语音合成."
}

func (t *GenerateSpeechTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"text":   strProp("The text to speak. Keep it under ~4000 characters per call; split long scripts into multiple calls."),
		"voice":  strProp(`Voice id like "alloy" / "nova" / "echo" — must be supported by the model; omit to use the default configured in settings`),
		"model":  strProp("Override TTS model for this call (default: the model configured in settings)"),
		"format": strProp(`Response audio format: "mp3" (default) / "wav" / "opus" / "flac" — must be supported by the model`),
		"speed":  numProp("Speech speed 0.25–4.0 (default 1.0; some models ignore it)"),
	}, []string{"text"})
}

func (t *GenerateSpeechTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Text   string  `json:"text"`
		Voice  string  `json:"voice"`
		Model  string  `json:"model"`
		Format string  `json:"format"`
		Speed  float64 `json:"speed"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	p.Text = strings.TrimSpace(p.Text)
	if p.Text == "" {
		return "", fmt.Errorf("text is required")
	}
	if t.Settings == nil {
		return "", fmt.Errorf("speech synthesis is not configured")
	}
	s := t.Settings()
	model := strings.TrimSpace(p.Model)
	if model == "" {
		model = strings.TrimSpace(s.Model)
	}
	if strings.TrimSpace(s.BaseURL) == "" || model == "" {
		return "", fmt.Errorf("speech synthesis is not configured: set 语音合成 Base URL and 语音模型 in 设置 → 模型")
	}
	voice := strings.TrimSpace(p.Voice)
	if voice == "" {
		voice = strings.TrimSpace(s.Voice)
	}

	reqBody := map[string]interface{}{"model": model, "input": p.Text}
	if voice != "" {
		reqBody["voice"] = voice
	}
	format := strings.TrimSpace(p.Format)
	if format != "" {
		reqBody["response_format"] = format
	}
	if p.Speed > 0 && p.Speed <= 4 {
		reqBody["speed"] = p.Speed
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}
	url := strings.TrimRight(s.BaseURL, "/") + "/audio/speech"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
	}
	// 长文本合成可达数分钟，独立长超时兜底。
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("speech API %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), clipStr(string(raw), 600))
	}
	// 部分网关以 JSON 包装 {audio: b64} / {data:[{b64_json}]}；二进制直落为主。
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		var wrapped struct {
			Audio string `json:"audio"`
			Data  []struct {
				B64 string `json:"b64_json"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &wrapped) == nil {
			if strings.TrimSpace(wrapped.Audio) != "" {
				if raw, err = base64.StdEncoding.DecodeString(wrapped.Audio); err != nil {
					return "", fmt.Errorf("decode audio: %w", err)
				}
			} else if len(wrapped.Data) > 0 && strings.TrimSpace(wrapped.Data[0].B64) != "" {
				if raw, err = base64.StdEncoding.DecodeString(wrapped.Data[0].B64); err != nil {
					return "", fmt.Errorf("decode audio: %w", err)
				}
			}
		}
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("speech API returned empty audio")
	}

	dir := t.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			dir = filepath.Join(os.TempDir(), "kaelvoris-media", "audio")
		} else {
			dir = filepath.Join(home, ".kaelvoris", "media", "audio")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create audio dir: %w", err)
	}
	mime := resp.Header.Get("Content-Type")
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mime == "" || mime == "application/octet-stream" {
		mime = DetectAudioMIME(raw) // 内容嗅探兜底（wav/ogg/…）
	}
	ext := AudioExt(mime)
	if format != "" && ext == "mp3" && format != "mpeg" {
		ext = strings.TrimPrefix(strings.ToLower(format), ".") // 显式 format 优先于嗅探默认
		mime = AudioMIMEFromExt(ext)
	}
	name := fmt.Sprintf("%s-speech.%s", time.Now().Format("20060102-150405"), ext)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", fmt.Errorf("save audio: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Generated speech audio (model %s, voice %s, %d bytes).\n", model, voice, len(raw))
	b.WriteString("Saved to: " + path + "\n")
	b.WriteString("The audio has been shown to the user in the chat. Briefly confirm completion; do NOT print base64 data.")
	b.WriteString(BuildMediaPayload([]MediaPayload{{
		Path:     path,
		DataURL:  MustDataURL(raw), // 音频通常较小（KB~MB 级），data URL 实时直播
		FileName: name,
		MIME:     mime,
		Kind:     "audio",
	}}))
	return b.String(), nil
}
