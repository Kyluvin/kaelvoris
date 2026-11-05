package tools

// 图生视频/图生图工具测试（httptest 假网关）：
//  1. keyframe 模式：本地帧图经 Upload 钩子换公网 URL，请求体含 first_frame/last_frame/mode；
//  2. keyframe 校验：缺 first_frame/last_frame 直接报错；
//  3. reference 模式：images 解析为 URL 列表注入请求体；>5 张报错；
//  4. 无 Upload 钩子时本地路径报错（引导配图床或传 URL）；
//  5. generate_image 图生图：image 参数注入请求体且 n 固定 1。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngBytesMinimal 最小 PNG（1x1）。
func pngBytesMinimal() []byte {
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
		0x0D, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x62, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}
}

// fakeVideoServer 假 Agnes /videos 端点：同步返回成片 URL + 记录请求体。
// GET /clip.mp4 回最小 mp4 头字节（下载校验用）。
func fakeVideoServer(t *testing.T, captured *map[string]interface{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/videos", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if captured != nil {
			*captured = body
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"url":"%s/clip.mp4"}`, "http://"+r.Host)
	})
	mux.HandleFunc("/clip.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte("\x00\x00\x00\x18ftypmp42fake"))
	})
	return httptest.NewServer(mux)
}

func TestGenerateVideoKeyframeUploadsLocalFrames(t *testing.T) {
	var captured map[string]interface{}
	ts := fakeVideoServer(t, &captured)
	defer ts.Close()

	// 本地帧图文件。
	dir := t.TempDir()
	first := filepath.Join(dir, "first.png")
	last := filepath.Join(dir, "last.png")
	for _, p := range []string{first, last} {
		if err := os.WriteFile(p, pngBytesMinimal(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var uploaded int
	tool := &GenerateVideoTool{
		Settings: func() VideoGenSettings {
			return VideoGenSettings{BaseURL: ts.URL, Model: "agnes-video-2.5-flash", Size: "720P"}
		},
		Upload: func(ctx context.Context, data []byte, name string) (string, error) {
			uploaded++
			return "https://pub.example/img/" + name, nil
		},
		Dir: t.TempDir(),
	}
	// mode 省略 + 帧图给出 → 自动 keyframe；上传 2 次。
	args := fmt.Sprintf(`{"prompt":"人物转身","first_frame":%q,"last_frame":%q,"seconds":4,"aspect_ratio":"16:9"}`, first, last)
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if uploaded != 2 {
		t.Errorf("Upload called %d times, want 2", uploaded)
	}
	if captured["mode"] != "keyframe" {
		t.Errorf("mode = %v, want keyframe", captured["mode"])
	}
	if captured["first_frame"] != "https://pub.example/img/i2v-first_frame.png" {
		t.Errorf("first_frame = %v", captured["first_frame"])
	}
	if captured["last_frame"] != "https://pub.example/img/i2v-last_frame.png" {
		t.Errorf("last_frame = %v", captured["last_frame"])
	}
	if captured["aspect_ratio"] != "16:9" {
		t.Errorf("aspect_ratio = %v", captured["aspect_ratio"])
	}
	// 轮询超时类错误可接受（假服务器不轮询完成），但请求体捕获已完成即认为通过。
	_ = res
}

func TestGenerateVideoKeyframeRequiresFrame(t *testing.T) {
	tool := &GenerateVideoTool{
		Settings: func() VideoGenSettings {
			return VideoGenSettings{BaseURL: "http://x", Model: "m"}
		},
	}
	_, err := tool.Execute(context.Background(), `{"prompt":"p","mode":"keyframe"}`)
	if err == nil || !strings.Contains(err.Error(), "first_frame or last_frame") {
		t.Fatalf("err = %v, want keyframe frame required", err)
	}
}

func TestGenerateVideoReferenceUploadsImages(t *testing.T) {
	var captured map[string]interface{}
	ts := fakeVideoServer(t, &captured)
	defer ts.Close()

	dir := t.TempDir()
	ref := filepath.Join(dir, "char.png")
	if err := os.WriteFile(ref, pngBytesMinimal(), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GenerateVideoTool{
		Settings: func() VideoGenSettings {
			return VideoGenSettings{BaseURL: ts.URL, Model: "agnes-video-2.5-flash"}
		},
		Upload: func(ctx context.Context, data []byte, name string) (string, error) {
			return "https://pub.example/char.png", nil
		},
		Dir: t.TempDir(),
	}
	// 显式 mode=reference + 参考图。
	args := fmt.Sprintf(`{"prompt":"以图1角色参考奔跑","mode":"reference","images":[%q],"seconds":5}`, ref)
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if captured["mode"] != "reference" {
		t.Errorf("mode = %v, want reference", captured["mode"])
	}
	imgs, ok := captured["images"].([]interface{})
	if !ok || len(imgs) != 1 || imgs[0] != "https://pub.example/char.png" {
		t.Errorf("images = %v", captured["images"])
	}
}

func TestGenerateVideoReferenceMaxFive(t *testing.T) {
	tool := &GenerateVideoTool{
		Settings: func() VideoGenSettings {
			return VideoGenSettings{BaseURL: "http://x", Model: "m"}
		},
	}
	_, err := tool.Execute(context.Background(),
		`{"prompt":"p","mode":"reference","images":["a","b","c","d","e","f"]}`)
	if err == nil || !strings.Contains(err.Error(), "must not exceed 5") {
		t.Fatalf("err = %v, want images limit", err)
	}
}

func TestGenerateVideoLocalPathNoUploadHook(t *testing.T) {
	tool := &GenerateVideoTool{
		Settings: func() VideoGenSettings {
			return VideoGenSettings{BaseURL: "http://x", Model: "m"}
		},
	}
	_, err := tool.Execute(context.Background(), `{"prompt":"p","first_frame":"C:/local/frame.png"}`)
	if err == nil || !strings.Contains(err.Error(), "no media upload configured") {
		t.Fatalf("err = %v, want upload-not-configured error", err)
	}
}

func TestGenerateVideoInvalidMode(t *testing.T) {
	tool := &GenerateVideoTool{
		Settings: func() VideoGenSettings {
			return VideoGenSettings{BaseURL: "http://x", Model: "m"}
		},
	}
	_, err := tool.Execute(context.Background(), `{"prompt":"p","mode":"magic"}`)
	if err == nil || !strings.Contains(err.Error(), "invalid mode") {
		t.Fatalf("err = %v, want invalid mode", err)
	}
}

// 漏传 mode 但给了首帧 → 自动归入 keyframe；无 Upload 钩子 + 本地路径在
// 读盘/上传解析阶段报错，证明自动归入分支生效。
func TestGenerateVideoAutoModeKeyframe(t *testing.T) {
	tool := &GenerateVideoTool{
		Settings: func() VideoGenSettings {
			return VideoGenSettings{BaseURL: "http://x", Model: "m"}
		},
	}
	_, err := tool.Execute(context.Background(), `{"prompt":"p","first_frame":"Z:/no/such.png"}`)
	if err == nil || (!strings.Contains(err.Error(), "read local file") && !strings.Contains(err.Error(), "no media upload configured")) {
		t.Fatalf("err = %v, want keyframe resolve-stage error", err)
	}
}

// data URI 首帧：解 base64 → 走 Upload 换 URL → 进入请求阶段。
func TestGenerateVideoDataURIUpload(t *testing.T) {
	var captured map[string]interface{}
	ts := fakeVideoServer(t, &captured)
	defer ts.Close()

	tool := &GenerateVideoTool{
		Settings: func() VideoGenSettings {
			return VideoGenSettings{BaseURL: ts.URL, Model: "m"}
		},
		Upload: func(ctx context.Context, data []byte, name string) (string, error) {
			return "https://pub.example/d.png", nil
		},
		Dir: t.TempDir(),
	}
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("datauri"))
	if _, err := tool.Execute(context.Background(), `{"prompt":"p","mode":"keyframe","first_frame":"`+uri+`"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if captured["first_frame"] != "https://pub.example/d.png" {
		t.Errorf("first_frame = %v, want uploaded URL", captured["first_frame"])
	}
}

// Upload 钩子失败：错误带 what 前缀与原始错误，引导模型可诊断。
func TestGenerateVideoUploadFailure(t *testing.T) {
	tool := &GenerateVideoTool{
		Settings: func() VideoGenSettings {
			return VideoGenSettings{BaseURL: "http://x", Model: "m"}
		},
		Upload: func(ctx context.Context, data []byte, name string) (string, error) {
			return "", errors.New("boom")
		},
	}
	_, err := tool.Execute(context.Background(), `{"prompt":"p","mode":"keyframe","first_frame":"data:image/png;base64,AAAA"}`)
	if err == nil || !strings.Contains(err.Error(), "first_frame: upload to media storage failed: boom") {
		t.Fatalf("err = %v, want prefixed upload failure", err)
	}
}

func TestGenerateImageImageToImage(t *testing.T) {
	var captured map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured = body
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(pngBytesMinimal()))
	}))
	defer ts.Close()

	dir := t.TempDir()
	ref := filepath.Join(dir, "char.png")
	if err := os.WriteFile(ref, pngBytesMinimal(), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GenerateImageTool{
		Settings: func() ImageGenSettings {
			return ImageGenSettings{BaseURL: ts.URL, Model: "agnes-image-2.5-flash"}
		},
		Dir: t.TempDir(),
	}
	args := fmt.Sprintf(`{"prompt":"角色在花田奔跑","image":[%q],"ratio":"16:9","size":"1K"}`, ref)
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := captured["image"].([]interface{}); len(got) != 1 {
		t.Errorf("image = %v, want 1 ref", captured["image"])
	} else if s := got[0].(string); !strings.HasPrefix(s, "data:image/png;base64,") {
		t.Errorf("ref not converted to data URI: %s", s[:40])
	}
	if captured["ratio"] != "16:9" {
		t.Errorf("ratio = %v", captured["ratio"])
	}
	if captured["size"] != "1K" {
		t.Errorf("size = %v", captured["size"])
	}
	// 图生图 n 固定 1。
	if n, ok := captured["n"].(float64); !ok || n != 1 {
		t.Errorf("n = %v, want 1", captured["n"])
	}
	if !strings.Contains(res, "Generated 1 image") {
		t.Errorf("result = %s", res[:80])
	}
}
