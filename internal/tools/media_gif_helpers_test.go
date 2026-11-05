package tools

// 测试辅助：构造真实可解码 PNG / 回读 GIF（media_gif_test.go 专用）。

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// encodeTestPNG 构造 w×h 的 PNG，颜色由 fn(x,y) 逐像素给出。
func encodeTestPNG(t *testing.T, w, h int, fn func(x, y int) (uint8, uint8, uint8)) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b := fn(x, y)
			img.Set(x, y, color.RGBA{r, g, b, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// writeTestPNG 编码 PNG 并落盘，返回路径。
func writeTestPNG(t *testing.T, dir, name string, w, h int, r, g, b uint8) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, encodeTestPNG(t, w, h, func(int, int) (uint8, uint8, uint8) { return r, g, b }), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// gifDecode 回读 GIF 校验帧结构。
func gifDecode(raw []byte) (*gif.GIF, error) {
	return gif.DecodeAll(bytes.NewReader(raw))
}

// assertGIFPayload 断言工具结果含 KAELVORIS_MEDIA 载荷（kind=video + image/gif）
// 且落盘文件存在。
func assertGIFPayload(t *testing.T, res string, frames int) {
	t.Helper()
	_, medias := ExtractMediaPayload(res)
	if len(medias) != 1 {
		t.Fatalf("media payload count = %d, want 1", len(medias))
	}
	m := medias[0]
	if m.Kind != "video" || m.MIME != "image/gif" {
		t.Errorf("media = kind %q mime %q, want video + image/gif", m.Kind, m.MIME)
	}
	raw, err := os.ReadFile(m.Path)
	if err != nil {
		t.Fatalf("read gif: %v", err)
	}
	g, err := gif.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gif.Decode: %v", err)
	}
	_ = g
	if _, err := gifDecode(raw); err != nil {
		t.Fatalf("gifDecode: %v", err)
	}
}
