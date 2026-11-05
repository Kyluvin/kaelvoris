package desktop

// open_in_explorer.go 外部打开目录：系统文件管理器 / Zed 编辑器 / 系统默认
// 程序（GUI 进程隐藏控制台）。

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"

	"kaelvoris/internal/sysproc"
)

// openInExplorer 用系统文件管理器打开目录。
// 注意（崩溃排查结论）：explorer 绝不能加 sysproc.Hide——Windows 的
// explorer.exe 把「打开文件夹」请求转交给常驻桌面实例处理，新窗口继承
// 调用方 STARTUPINFO 的 STARTF_USESHOWWINDOW(SW_HIDE)，结果窗口创建但
// 被隐藏（用户表现：「点了没反应」）。explorer 是 GUI 子系统程序，
// 不经 cmd 包装不会闪现控制台，无需隐藏标志。
func openInExplorer(dir string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	return cmd.Start()
}

// openWithDefault 用系统默认关联程序打开文件（聊天 markdown 非代码文件
// 点击：图片 → 照片查看器，pptx/docx/pdf → Office/PDF 阅读器，视频音频 →
// 播放器等）。Windows 经 `cmd /C start "" <file>`（start 的第一个引号参数
// 是窗口标题占位，路径含空格时必须带上，否则被吞成标题）；macOS/Linux
// 走各自开放接口。不等待打开的程序退出（阅读器常驻，Wait 会阻塞 GUI）。
// 注意：绝不加 sysproc.Hide——explorer 同理，关联程序（rundll32 中转）
// 可能继承 SW_HIDE 导致窗口创建但被隐藏（「点了没反应」）；GUI 子系统
// 程序不经控制台包装不会闪黑框。
func openWithDefault(file string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/C", "start", "", file)
	case "darwin":
		cmd = exec.Command("open", file)
	default:
		cmd = exec.Command("xdg-open", file)
	}
	return cmd.Start()
}

// openInZed 在 dir 目录执行 `zed .` 打开项目（用户要求：当前目录执行 zed 命令）。
// Windows 的 zed 是 .cmd 包装（与 npx 同理），必须经 cmd /C 包装才能 spawn；
// 启动后不等待编辑器退出（Zed 常驻，Wait 会永久阻塞 GUI 进程）。
func openInZed(dir string) error {
	return zedCmd(dir, ".")
}

// openInZedAt 用 Zed 打开 file 并定位到 line（聊天 markdown 代码链接点击）。
// dir 为要打开的项目目录；file 为绝对路径。Zed CLI 原生支持 `path:line:column`
// 定位语法（crates/util/src/paths.rs 的 PathWithPosition::parse_str，行号解析仅
// 作用于 file_name 后缀，Windows 盘符冒号不受影响）；同时传入 dir 使 Zed 打开
// 对应项目工作树并复用/新建窗口，file:line 在其内定位。
func openInZedAt(dir, file string, line int) error {
	if line < 1 {
		line = 1
	}
	return zedCmd(dir, file+":"+strconv.Itoa(line))
}

// zedCmd 在 dir 下执行 `zed <args...>`（不得包含空白参数；Zed 常驻不等待）。
func zedCmd(dir string, args ...string) error {
	if _, err := exec.LookPath("zed"); err != nil {
		return fmt.Errorf("未找到 zed 命令（请确认已安装并加入 PATH）: %w", err)
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", append([]string{"/C", "zed"}, args...)...)
	} else {
		cmd = exec.Command("zed", args...)
	}
	cmd.Dir = dir
	cmd.Env = os.Environ()
	// 丢弃输出：zed 前台模式会向 stderr 写日志，不丢弃会阻塞管道。
	cmd.Stdout = nil
	cmd.Stderr = nil
	sysproc.Hide(cmd)
	return cmd.Start()
}
