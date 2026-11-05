package employee

// helpers.go 包内小工具：JSON 落盘、目录复制、文件名处理。

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func joinPath(elem ...string) string          { return filepath.Join(elem...) }
func jsonUnmarshal(b []byte, v any) error     { return json.Unmarshal(b, v) }
func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
func hexEncode(b []byte) string               { return hex.EncodeToString(b) }
func trimExt(name string) string              { return strings.TrimSuffix(name, filepath.Ext(name)) }

// copyTree 递归复制目录（Duplicate 员工工作空间用；已存在文件覆盖）。
func copyTree(src, dst string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return
	}
	_ = os.MkdirAll(dst, 0o755)
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyTree(s, d)
			continue
		}
		b, err := os.ReadFile(s)
		if err != nil {
			continue
		}
		_ = os.WriteFile(d, b, 0o644)
	}
}
