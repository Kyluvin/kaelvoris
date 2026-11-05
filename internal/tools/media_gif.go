// GIF 降级合成：视频模型未配置（关闭/付费未开通）时，generate_video 把
// keyframe/reference 帧图本地合成为循环 GIF 动图——免费、零依赖、无需图床。
//
// 设计取舍（ponytail）：
//   - 标准库 image/gif + palette.Plan9 全局调色板 + FloydSteinberg 抖动，
//     不引 golang.org/x/image（自适应调色板/高质量缩放对分镜预览不值当）；
//   - 帧不缩放：画布取各帧最大宽高，小图左上对齐、黑底补边——分镜管线同一批
//     keyframe 尺寸一致，混尺寸场景补边即可，需要时再上 x/image/draw 缩放；
//   - GIF 帧延迟以 10ms 为单位，下限 50ms（跨浏览器一致下限）。
package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color/palette"
	"image/draw"
	"image/gif"
	_ "image/jpeg" // 注册 JPEG 解码器（帧图来源之一）
	_ "image/png"  // 注册 PNG 解码器（generate_image 默认输出）
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// synthesizeGIF 把帧图字节列表合成为循环 GIF（每帧时长 per）。
// webp 等未注册格式解码报错，引导用 png/jpg/gif 帧图。
func synthesizeGIF(frames [][]byte, per time.Duration) ([]byte, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("no frames to synthesize")
	}
	if per < 50*time.Millisecond {
		per = 50 * time.Millisecond
	}
	var imgs []image.Image
	maxX, maxY := 0, 0
	for i, raw := range frames {
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decode frame %d: %w (frames must be png/jpg/gif)", i+1, err)
		}
		imgs = append(imgs, img)
		if b := img.Bounds(); b.Max.X > maxX {
			maxX = b.Max.X
		}
		if b := img.Bounds(); b.Max.Y > maxY {
			maxY = b.Max.Y
		}
	}
	var g gif.GIF
	g.LoopCount = 0 // 0 = 无限循环
	delay := int(per / (10 * time.Millisecond))
	if delay < 5 {
		delay = 5
	}
	for _, img := range imgs {
		pm := image.NewPaletted(image.Rect(0, 0, maxX, maxY), palette.Plan9)
		draw.FloydSteinberg.Draw(pm, pm.Bounds(), img, img.Bounds().Min)
		g.Image = append(g.Image, pm)
		g.Delay = append(g.Delay, delay)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &g); err != nil {
		return nil, fmt.Errorf("encode gif: %w", err)
	}
	return buf.Bytes(), nil
}

// frameBytes 帧图引用 → 原始字节：data URI 解码 / URL 下载 / 本地路径读盘
// （GIF 降级专用；不走 Upload——合成直接用本地字节，无需图床）。
func (t *GenerateVideoTool) frameBytes(ctx context.Context, client *http.Client, ref, what string) ([]byte, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(ref, "data:"):
		j := strings.Index(ref, ",")
		if j < 0 {
			return nil, fmt.Errorf("%s: malformed data URI", what)
		}
		b, err := base64.StdEncoding.DecodeString(ref[j+1:])
		if err != nil {
			return nil, fmt.Errorf("%s: decode data URI: %w", what, err)
		}
		return b, nil
	case strings.Contains(ref, "://"):
		raw, err := t.download(ctx, client, ref)
		if err != nil {
			return nil, fmt.Errorf("%s: download: %w", what, err)
		}
		return raw, nil
	default:
		raw, err := os.ReadFile(ref)
		if err != nil {
			return nil, fmt.Errorf("%s: read local file: %w", what, err)
		}
		return raw, nil
	}
}

// gifFallback 视频模型未配置时的免费降级：把帧图（keyframe 首/尾帧 + reference
// 参考图）本地合成为循环 GIF 动图并推送聊天流。GIF 无法表现 prompt 描述的运动
// （只是帧序列轮播），结果文案如实告知，并引导配置视频模型获得真 MP4。
func (t *GenerateVideoTool) gifFallback(ctx context.Context, first, last string, images []string, seconds float64) (string, error) {
	type frameRef struct {
		val  string
		what string
	}
	refs := make([]frameRef, 0, len(images)+2)
	if v := strings.TrimSpace(first); v != "" {
		refs = append(refs, frameRef{v, "first_frame"})
	}
	if v := strings.TrimSpace(last); v != "" {
		refs = append(refs, frameRef{v, "last_frame"})
	}
	for i, im := range images {
		if v := strings.TrimSpace(im); v != "" {
			refs = append(refs, frameRef{v, fmt.Sprintf("images[%d]", i)})
		}
	}
	if len(refs) == 0 {
		return "", fmt.Errorf("no frames to synthesize GIF from")
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	frames := make([][]byte, 0, len(refs))
	for _, r := range refs {
		raw, err := t.frameBytes(ctx, client, r.val, r.what)
		if err != nil {
			return "", err
		}
		frames = append(frames, raw)
	}
	// 每帧时长：seconds 均摊到各帧（缺省 2s/帧；分镜帧是可读场景，限幅 0.5s~10s）。
	per := 2 * time.Second
	if seconds > 0 {
		per = time.Duration(seconds / float64(len(frames)) * float64(time.Second))
	}
	if per < 500*time.Millisecond {
		per = 500 * time.Millisecond
	}
	if per > 10*time.Second {
		per = 10 * time.Second
	}
	raw, err := synthesizeGIF(frames, per)
	if err != nil {
		return "", err
	}
	return t.saveGIFAndReport(raw, len(frames))
}

// saveGIFAndReport GIF 动图落盘 + KAELVORIS_MEDIA 载荷（kind=video + image/gif
// MIME，前端视频卡按 image/* MIME 渲染 <img> 动图；体积原因不走 data URL，
// 实时/回放均经 /kaelvoris-media/ 路由回挂，与视频同语义）。
func (t *GenerateVideoTool) saveGIFAndReport(raw []byte, frames int) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("gif synthesize returned empty body")
	}
	dir := t.videoDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create video dir: %w", err)
	}
	name := fmt.Sprintf("%s-video.gif", time.Now().Format("20060102-150405"))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", fmt.Errorf("save gif: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Video model is not configured — stitched %d frame image(s) into a looping animated GIF locally (free fallback, no video-model call).\n", frames)
	b.WriteString("Saved to: " + path + "\n")
	b.WriteString("The GIF has been shown to the user in the chat. Briefly confirm completion; do NOT print base64 data. " +
		"For a real MP4 video, set 视频生成 Base URL and 视频模型 in 设置 → 模型.")
	b.WriteString(BuildMediaPayload([]MediaPayload{{
		Path:     path,
		DataURL:  "", // GIF 可达数 MB，不构造 data URL，前端经 /kaelvoris-media/ 回挂
		FileName: name,
		MIME:     "image/gif",
		Kind:     "video",
	}}))
	return b.String(), nil
}
