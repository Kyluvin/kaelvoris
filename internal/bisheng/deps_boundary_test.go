// deps_boundary_test.go 守护 internal/bisheng 的零依赖设计约束（经验卡：
// 「bisheng workflow Go 移植已落地：internal/bisheng 零依赖引擎 + desktop 桥接」）。
// 现有 verify_state 快检（vet/headless/gofmt/git）均不含依赖方向检查，本测试
// 用 go list -deps 取依赖闭包逐项断言：只允许标准库 + 白名单豁免的自有 internal 包；
// wails/桌面桥接层与第三方模块越界即 fail 并打印违规 import。
package bisheng

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDepsBoundary 断言 bisheng 依赖闭包无越界。go test 的工作目录即本包目录，
// 故 go list 目标为 "."。
func TestDepsBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps",
		"-f", "{{if not .Standard}}{{.ImportPath}}{{end}}",
		".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list 执行失败（需 go 工具链在 PATH）: %v\n%s", err, out)
	}

	// 白名单：允许出现在依赖闭包中的非标准库包（以当前代码实态盘点，为零——
	// 引擎仅用标准库）。若确需引入自有 internal 包（如 pkg/message 之类纯 Go
	// 内核件），逐项登记并注明理由；第三方模块与 wails/desktop 桥接层禁止进入，
	// 否则破坏可移植性与可测性。
	allow := map[string]bool{}

	var viol []string
	for _, line := range strings.Split(string(out), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" || pkg == "kaelvoris/internal/bisheng" {
			continue // 自身必然在闭包内
		}
		if allow[pkg] {
			continue
		}
		viol = append(viol, pkg)
	}
	if len(viol) > 0 {
		t.Fatalf("internal/bisheng 出现越界依赖（只允许标准库 + 白名单自有包）:\n  %s",
			strings.Join(viol, "\n  "))
	}
}
