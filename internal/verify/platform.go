package verify

// 平台/编码辅助（与 internal/hooks 同构的薄封装，避免跨包耦合）。

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"runtime"
	"time"
)

// goosWindows 当前是否 Windows。
func goosWindows() bool { return runtime.GOOS == "windows" }

// timeoutContext 按秒数创建超时 context（0 = 默认 60s）。
func timeoutContext(sec int) (context.Context, context.CancelFunc) {
	if sec <= 0 {
		sec = 60
	}
	return context.WithTimeout(context.Background(), time.Duration(sec)*time.Second)
}

// readFile 读文件（供 Load 使用；错误上抛由调用方静默处理）。
func readFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// 剥 BOM（Windows PowerShell 写出的 UTF-8 文件默认带 BOM）。
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	return b, nil
}

// jsonUnmarshal JSON 解析薄封装。
func jsonUnmarshal(b []byte, v interface{}) error {
	return json.NewDecoder(bytes.NewReader(b)).Decode(v)
}
