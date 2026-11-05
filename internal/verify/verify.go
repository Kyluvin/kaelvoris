// Package verify 编辑后验证反馈环（C1，对标 Aider auto-lint / auto-test）。
//
// Aider 语义（https://aider.chat/docs/usage/lint-test.html）：每次 AI 修改
// 文件后自动 lint 被改文件；命令非零退出时把错误输出回灌对话，AI 立即修复。
// Kaelvoris 简化对齐：
//   - 触发点：fs_write / fs_apply_diff 成功后（引擎挂点，见 engine.go）；
//   - 规则：扩展名 → 检查命令（verify.json 可配置；空 = 内置默认 .go → go vet ./...）；
//   - 反馈：检查失败把截断错误附加到工具结果回灌模型 + say verify 推送前端；
//   - 约束：检查失败不改变工具成功状态（编辑本身已成功，反馈是增强不是拦截）；
//     超时/启动失败/输出空一律静默；同路径 30s 冷却防检查风暴。
package verify

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/sysproc"
)

// defaultCooldown 同路径检查失败后的冷却窗口（防写坏文件循环时检查风暴）。
const defaultCooldown = 30 * time.Second

// errTailMax 错误输出回灌上限（字节）。
const errTailMax = 1500

// Rule 单语言验证规则。
type Rule struct {
	Ext     []string `json:"ext"`     // 触发扩展名（含点，如 [".go"]）
	Command string   `json:"command"` // 命令模板；{file} 占位符替换为被改文件相对路径（正斜杠）
}

// Config verify.json 配置（损坏/不存在 = 零值 = 开启 + 内置默认规则）。
type Config struct {
	Disabled bool   `json:"disabled"` // true = 关闭检查
	Timeout  int    `json:"timeout"`  // 单次检查超时秒数（默认 60，上限 300）
	Rules    []Rule `json:"rules"`    // 空 = 内置默认规则
}

// defaultRules 内置默认规则：Go 项目改完即 vet（一条命令同时覆盖编译错误
// 与静态检查，是 Go 最高频的「改完就坏」检测点）。其他语言不内置——不假设
// 用户环境装了 linter，用户可在 verify.json 自行增加。
var defaultRules = []Rule{
	{Ext: []string{".go"}, Command: "go vet ./..."},
}

// Runner 编辑后验证执行器。
type Runner struct {
	Workspace string // 工作区根（检查命令 cwd）
	Timeout   int    // 单次检查超时秒数
	Rules     []Rule

	mu       sync.Mutex
	cooldown map[string]time.Time // relPath → 上次检查时间（无论成败）
}

// Load 读配置文件（不存在/损坏返回 nil，调用方按零值处理）。
func Load(path string) *Config {
	b, err := readFile(path)
	if err != nil {
		return nil
	}
	var cfg Config
	if unmarshalErr := jsonUnmarshal(b, &cfg); unmarshalErr != nil {
		return nil
	}
	return &cfg
}

// NewRunner 构造执行器；cfg 为 nil 时用零值（开启 + 默认规则 + 60s 超时）。
func NewRunner(workspace string, cfg *Config) *Runner {
	r := &Runner{Workspace: workspace, cooldown: map[string]time.Time{}}
	if cfg == nil {
		r.Rules = defaultRules
		r.Timeout = 60
		return r
	}
	if cfg.Disabled {
		r.Rules = nil // nil Rules = 不检查
		return r
	}
	r.Rules = cfg.Rules
	if len(r.Rules) == 0 {
		r.Rules = defaultRules
	}
	r.Timeout = cfg.Timeout
	if r.Timeout <= 0 {
		r.Timeout = 60
	}
	if r.Timeout > 300 {
		r.Timeout = 300
	}
	return r
}

// match 返回第一条命中扩展名的规则。
func (r *Runner) match(relPath string) *Rule {
	ext := strings.ToLower(filepath.Ext(relPath))
	for i := range r.Rules {
		for _, e := range r.Rules[i].Ext {
			if strings.ToLower(e) == ext {
				return &r.Rules[i]
			}
		}
	}
	return nil
}

// cooled 同路径是否在冷却窗口内（是则跳过检查）。
func (r *Runner) cooled(relPath string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cooldown == nil {
		r.cooldown = map[string]time.Time{} // 直接构造的 Runner（测试/字面量）兜底
	}
	if t, ok := r.cooldown[relPath]; ok && time.Since(t) < defaultCooldown {
		return true
	}
	r.cooldown[relPath] = time.Now()
	return false
}

// AfterEdit 写类工具成功后调用：返回附加到工具结果的反馈文本（无检查/通过/静默均返回 ""）。
// 反馈格式（回灌模型）：[verify] <relPath> 检查未通过(<command>)：\n<errTail>\n请立即修复上述问题再继续。
func (r *Runner) AfterEdit(relPath string) string {
	if r == nil || r.Rules == nil {
		return ""
	}
	rule := r.match(relPath)
	if rule == nil || strings.TrimSpace(rule.Command) == "" {
		return ""
	}
	if r.cooled(relPath) {
		return ""
	}
	cmd := strings.ReplaceAll(rule.Command, "{file}", filepath.ToSlash(relPath))
	if !r.resolvable(cmd) {
		// 命令不可解析（工具链不在 PATH / 路径不存在）：静默跳过。
		// 命令不存在不是代码问题，回灌只会产生无法修复的噪音反馈
		//（缺命令退出码随环境漂移，运行前预判最稳，Aider 同语义）。
		return ""
	}
	code, out := r.run(cmd)
	if code == 0 {
		return ""
	}
	if code < 0 {
		// 启动失败/超时（exec 未找到等）：静默（约定见包注释）。
		return ""
	}
	out = strings.TrimSpace(out)
	if out == "" {
		out = fmt.Sprintf("exit code %d（无错误输出）", code)
	}
	if len(out) > errTailMax {
		out = out[len(out)-errTailMax:] // 保留尾部（错误汇总一般在末尾）
	}
	return fmt.Sprintf("[verify] %s 检查未通过(%s)：\n%s\n请立即修复上述问题再继续。", relPath, cmd, out)
}

// run 执行检查命令（cwd=工作区；Windows cmd /C，Unix sh -c），返回 (exitCode, 合并输出)。
// 启动失败按 -1 处理、输出为错误说明；超时由 Timeout 控制。
func (r *Runner) run(cmdText string) (int, string) {
	cctx, cancel := timeoutContext(r.Timeout)
	defer cancel()
	var cmd *exec.Cmd
	if goosWindows() {
		cmd = exec.CommandContext(cctx, "cmd", "/C", cmdText)
	} else {
		cmd = exec.CommandContext(cctx, "sh", "-c", cmdText)
	}
	sysproc.Hide(cmd) // GUI 进程下隐藏控制台黑框（每次编辑后跑 vet，高频）。
	cmd.Dir = r.Workspace
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		// 启动失败/超时：返回 -1 与说明（输出可能为空）。
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return -1, msg
	}
	return 0, string(out)
}

// shellBuiltins shell 内建命令白名单（LookPath 解析不到但 shell 能执行）。
// 覆盖检查命令常用的内建（echo/exit 等），白名单外再放行会误放真缺失。
var shellBuiltins = map[string]bool{
	"echo": true, "exit": true, "cd": true, "set": true, "type": true,
	"dir": true, "del": true, "copy": true, "move": true, "mkdir": true,
	"rmdir": true, "if": true, "for": true, "call": true, "rem": true,
}

// resolvable 判断检查命令的可执行体是否可解析（首个 token：内建白名单 /
// 路径存在性 / PATH 查找）。不可解析 = 调用方静默跳过检查（工具链未安装
// 时每次编辑都回灌「不是内部或外部命令」属纯噪音）。
func (r *Runner) resolvable(cmdText string) bool {
	token := strings.TrimSpace(cmdText)
	if i := strings.IndexAny(token, " \t"); i >= 0 {
		token = token[:i]
	}
	token = strings.Trim(token, `"'`)
	if token == "" {
		return true // 空命令交 shell 处理，不拦
	}
	if shellBuiltins[strings.ToLower(token)] {
		return true
	}
	if strings.ContainsAny(token, `/\`) {
		_, err := os.Stat(token)
		return err == nil
	}
	_, err := exec.LookPath(token)
	return err == nil
}
