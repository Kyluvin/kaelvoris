package tools

// GIF 降级合成测试：视频模型未配置时 generate_video 的免费降级路径。
//  1. keyframe + 未配置 → 本地合成 GIF（kind=video / image/gif MIME，无限循环）；
//  2. reference + 未配置 → images 帧图同样降级合成；
//  3. text 模式 + 未配置 → 仍报配置引导错误（无帧图可合成）；
//  4. 已配置 → 不触发降级，正常走远端网关（请求体已由 i2v 测试覆盖）；
//  5. synthesizeGIF 纯函数：帧解码失败报错 / 正常出 GIF 头与帧数。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// realPNG 生成一张真实可解码的 PNG（RGBA 渐变，供 image.Decode 用）。
func realPNG(t *testing.T, w, h int, r, g, b uint8) []byte {
	t.Helper()
	return encodeTestPNG(t, w, h, func(x, y int) (uint8, uint8, uint8) {
		return r + uint8(x%16), g + uint8(y%16), b
	})
}

func TestGenerateVideoGIFFallbackKeyframe(t *testing.T) {
	dir := t.TempDir()
	first := writeTestPNG(t, dir, "first.png", 8, 8, 10, 20, 30)
	last := writeTestPNG(t, dir, "last.png", 8, 8, 200, 100, 50)

	tool := &GenerateVideoTool{Dir: t.TempDir()} // 无 Settings = 未配置
	args := fmt.Sprintf(`{"prompt":"人物转身","mode":"keyframe","first_frame":%q,"last_frame":%q,"seconds":4}`, first, last)
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute (fallback expected, not error): %v", err)
	}
	if !strings.Contains(res, "not configured") || !strings.Contains(res, "GIF") {
		t.Errorf("result should state fallback fired: %.120s", res)
	}
	assertGIFPayload(t, res, 2)
}

func TestGenerateVideoGIFFallbackReference(t *testing.T) {
	dir := t.TempDir()
	a := writeTestPNG(t, dir, "a.png", 8, 8, 1, 2, 3)
	b := writeTestPNG(t, dir, "b.png", 8, 8, 4, 5, 6)

	tool := &GenerateVideoTool{Dir: t.TempDir()}
	args := fmt.Sprintf(`{"prompt":"分镜轮播","mode":"reference","images":[%q,%q],"seconds":3}`, a, b)
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertGIFPayload(t, res, 2)
}

func TestGenerateVideoTextModeStillRequiresConfig(t *testing.T) {
	tool := &GenerateVideoTool{Dir: t.TempDir()}
	_, err := tool.Execute(context.Background(), `{"prompt":"日出延时"}`)
	if err == nil || !strings.Contains(err.Error(), "video generation is not configured") {
		t.Fatalf("err = %v, want not-configured error for text mode", err)
	}
}

func TestGenerateVideoConfiguredDoesNotFallback(t *testing.T) {
	var captured map[string]interface{}
	ts := fakeVideoServer(t, &captured)
	defer ts.Close()
	dir := t.TempDir()
	first := writeTestPNG(t, dir, "first.png", 8, 8, 9, 9, 9)

	tool := &GenerateVideoTool{
		Settings: func() VideoGenSettings {
			return VideoGenSettings{BaseURL: ts.URL, Model: "agnes-video-2.5-flash"}
		},
		Upload: func(ctx context.Context, data []byte, name string) (string, error) {
			return "https://pub.example/f.png", nil
		},
		Dir: t.TempDir(),
	}
	if _, err := tool.Execute(context.Background(),
		fmt.Sprintf(`{"prompt":"p","mode":"keyframe","first_frame":%q}`, first)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 已配置 → 请求体走远端（mode=keyframe + 上传 URL），不产 GIF。
	if captured["mode"] != "keyframe" {
		t.Errorf("mode = %v, want remote keyframe call", captured["mode"])
	}
}

func TestSynthesizeGIFDecodeFailure(t *testing.T) {
	if _, err := synthesizeGIF([][]byte{[]byte("not an image")}, time.Second); err == nil {
		t.Fatal("expected decode error for garbage frame")
	}
}

func TestSynthesizeGIFFrames(t *testing.T) {
	frames := [][]byte{
		realPNG(t, 8, 8, 1, 2, 3),
		realPNG(t, 4, 6, 9, 9, 9), // 小图混尺寸 → 补边到最大画布
	}
	raw, err := synthesizeGIF(frames, 1500*time.Millisecond)
	if err != nil {
		t.Fatalf("synthesizeGIF: %v", err)
	}
	// GIF 头 + NETSCAPE 无限循环扩展。
	if !strings.HasPrefix(string(raw[:6]), "GIF89a") && !strings.HasPrefix(string(raw[:6]), "GIF87a") {
		t.Fatalf("not a GIF: %x", raw[:6])
	}
	if !strings.Contains(string(raw), "NETSCAPE") {
		t.Error("missing NETSCAPE loop extension")
	}
	g, err := gifDecode(raw)
	if err != nil {
		t.Fatalf("decode back: %v", err)
	}
	if len(g.Image) != 2 {
		t.Errorf("frames = %d, want 2", len(g.Image))
	}
	if len(g.Delay) != 2 || g.Delay[0] != 150 { // 1500ms / 10ms
		t.Errorf("delay = %v, want [150 150]", g.Delay)
	}
	// 画布取最大宽高（8x8）。
	if b := g.Image[0].Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Errorf("canvas = %dx%d, want 8x8", b.Dx(), b.Dy())
	}
}
