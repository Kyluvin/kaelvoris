// Package tools 危险命令检测（codex 对齐补全 ②）。
//
// 对齐 codex-rs core/src/exec_policy.rs 的 DangerousCommand 平台化清单思路：
// 不是简单的子串黑名单，而是按平台整理高危命令模式 + 建议（BANNED_PREFIX_
// SUGGESTIONS 语义：拒绝时告知替代命令）；kaelvoris 无沙箱层，危险命令的
// 防线是「检测 → 强制人工审批」（比普通工具审批更高一级，AutoApprove 失效）。
//
// 设计要点：
//   - Check 返回 (isDanger, reason)：危险命令不直接拒绝，而是升级为强制
//     ask（用户可见 reason 说明为什么危险），既拦呆模型也保留人工决断；
//   - 平台化：Windows（cmd/PowerShell）与 Unix（bash/zsh）清单分开，
//     按 runtime.GOOS 选用；
//   - 分隔符感知：`;`/`&&`/`|`/反引号 后的第二段命令同样参与检测，
//     防止 `echo ok && rm -rf /` 绕过（对齐 codex execpolicy 的 rules DSL
//     分段思想——Go 简版按分隔符切分逐段匹配）；
//   - 工作区降级（2026-10-03 会话复盘新增）：删除「工作区内临时/构建产物
//     目录」（dist/build/tmp_*/_gofmt_head 等高频重构建目标）不触发强制
//     人工审批，降级为普通工具审批——3 个会话（8e1d/975/204）停在
//     「删除 dist 重建」审批卡上，用户离席即任务中断；该类操作本就是
//     打包链路的常规步骤，强制卡点只会逼模型绕道（改写脚本投递执行），
//     反而绕过检测。系统盘/工作区外的删除仍是硬红线。
package tools

import (
	"path/filepath"
	"runtime"
	"strings"
)

// dangerPattern 单条危险命令模式：匹配子串（小写化后）+ 风险说明。
type dangerPattern struct {
	match  string // 匹配子串（小写）
	reason string // 风险说明（审批卡片展示）
}

// windowsDangerPatterns Windows（cmd/PowerShell）高危命令清单。
var windowsDangerPatterns = []dangerPattern{
	{"format ", "格式化磁盘"},
	{"format/", "格式化磁盘"},
	{"del /f /s /q", "强制递归删除文件"},
	{"del /s /q c:\\", "递归删除系统盘文件"},
	{"rd /s /q", "递归删除目录树"},
	{"rmdir /s /q", "递归删除目录树"},
	{"remove-item -recurse -force", "PowerShell 强制递归删除"},
	{"rm -rf", "递归强制删除（PowerShell/别名场景）"},
	{"reg delete", "删除注册表项"},
	{"reg add hkey_local_machine", "写系统级注册表"},
	{"reg add hkey_current_user\\software\\microsoft\\windows\\currentversion\\run", "写注册表自启动项"},
	{"shutdown", "关机/重启"},
	{"restart-computer", "重启计算机"},
	{"stop-computer", "关机"},
	{"diskpart", "磁盘分区操作"},
	{"cipher /w", "擦除磁盘空闲空间"},
	{"vssadmin delete shadows", "删除卷影副本（备份）"},
	{"wbadmin delete catalog", "删除备份目录"},
	{"bcdedit", "修改启动配置"},
	{"set-executionpolicy", "修改 PowerShell 执行策略"},
	{"invoke-webrequest", "远程脚本下载"},
	{"iex ", "动态执行任意代码"},
	{"schtasks /delete", "删除计划任务"},
	{"net user ", "用户账户管理"},
	{"netsh advfirewall", "修改防火墙规则"},
	{"taskkill /f /im", "强制结束进程"},
	{"taskkill /f /pid", "强制结束进程"},
}

// unixDangerPatterns Unix（bash/zsh）高危命令清单。
// 注：`curl xxx | sh` 管道执行不可用整串模式匹配——splitCommandChain 先按 `|`
// 切段（链式每段独立检测），管道两端永不同段；改用 isRemotePipeExec 对
// 「下载命令 | 动态执行」的相邻段对做专项检测（2026-10-03 第三轮复盘修复：
// 旧清单里 curl|sh / curl|bash 是永不命中的死模式，远程脚本管道执行漏拦）。
var unixDangerPatterns = []dangerPattern{
	{"rm -rf /", "递归删除根目录"},
	{"rm -fr /", "递归删除根目录"},
	{"rm -rf ~", "递归删除用户主目录"},
	{"rm -rf .", "递归删除当前目录"},
	{"mkfs", "格式化文件系统"},
	{"dd if=", "裸设备写入"},
	{"dd of=", "裸设备写入"},
	{":(){ :|:& };:", "fork 炸弹"},
	{"shutdown", "关机/重启"},
	{"reboot", "重启"},
	{"init 0", "关机"},
	{"init 6", "重启"},
	{"halt", "关机"},
	{"poweroff", "关机"},
	{"> /dev/sda", "覆盖磁盘设备"},
	{"mkfs.ext4", "格式化文件系统"},
	{"chmod -r 000 /", "移除根目录权限"},
	{"chown -r nobody /", "改写根目录属主"},
	{"mv / ", "移动根目录"},
	{"history -c", "清除命令历史"},
	{"iptables -f", "清空防火墙规则"},
	{"crontab -r", "删除 crontab"},
	{"kill -9 -1", "杀死全部进程"},
	{"pkill -9", "按名强杀进程"},
	{"wget -o-", "远程内容下载（管道执行需确认）"},
	{"curl -o-", "远程内容下载（管道执行需确认）"},
	{"curl -o -", "远程内容下载（管道执行需确认）"},
}

// platformDangerPatterns 按 GOOS 返回当前平台的危险命令清单。
func platformDangerPatterns() []dangerPattern {
	if runtime.GOOS == "windows" {
		return windowsDangerPatterns
	}
	return unixDangerPatterns
}

// chainOperators 命令链分隔符：链式命令的每一段独立参与危险检测。
// （对齐 codex execpolicy 分段 rules 思想——`echo ok && rm -rf /` 不允许
// 因前半段安全而放行。）
var chainOperators = []string{"&&", "||", ";", "|", "`", "\n"}

// splitCommandChain 按链式分隔符切分命令为各段（每段独立参与危险匹配）。
func splitCommandChain(cmd string) []string {
	segs := []string{cmd}
	for _, op := range chainOperators {
		var next []string
		for _, s := range segs {
			next = append(next, strings.Split(s, op)...)
		}
		segs = next
	}
	return segs
}

// DangerCheck 危险命令检测结果：Danger 时引擎强制人工审批（AutoApprove 失效）。
type DangerCheck struct {
	// Danger 是否命中危险命令模式。
	Danger bool
	// Reason 风险说明（审批卡片展示，对齐 BANNED_PREFIX_SUGGESTIONS 的
	// 「拒绝时给出原因」语义）。
	Reason string
	// Downgrade 是否命中「工作区内临时产物删除」降级场景：仍返回 Danger=true
	//（走一次审批确认），但 Reason 标注为低风险，供用户快速放行；
	// 且 DangerWorkspaceFile 判定为 true 时引擎可降级为普通审批
	//（AutoApprove 生效，不再强制人工卡点——2026-10-03 复盘修复）。
	WorkspaceScoped bool
	// AutoConfirm 全量自动确认模式标记（用户需求 2026-10）：由
	// CheckDangerousCommandAuto 统一填充，引擎结合 DangerAutoApprove 开关
	// 决定是否免审放行；普通 CheckDangerousCommand(Scoped) 恒为 false，
	// 原有强制人工语义不变（既有调用点零改动）。
	AutoConfirm bool
}

// workspaceScopedDirNames 视为「临时/构建产物」的目录名（小写精确匹配）。
// 这些目录删除后可由构建流程完整重建，删除不构成不可逆损失：
// dist / build 产物、前端 node_modules（lockfile 可恢复）、Python 缓存、
// Go 测试临时目录、以及本工作区历史沉淀的 tmp_*/_* 调试脚本前缀。
var workspaceScopedDirNames = map[string]bool{
	"dist":          true,
	"build":         true,
	"bin":           true,
	"node_modules":  true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	"tmp":           true,
	"temp":          true,
	".tmp":          true,
	".kaelvoris-bg": true,
}

// workspaceScopedPrefixes 临时文件前缀（工作区根散装的调试脚本，历史会话
// 沉淀的 _*.ps1/_*.mjs/tmp_*.html 系列——删除属清理动作，非数据损失）。
var workspaceScopedPrefixes = []string{"tmp_", "_tmp", "_sess", "_pub_", "_ws_", "_ui_", "_check_", "_scan_", "_cmp_", "_read_", "_probe_", "_gofmt_"}

// isWorkspaceScopedDeletion 删除目标是否为工作区内可重建的临时/构建产物。
// 仅识别「删除单个显式目标」的安全形态：
//   - cmd:  rmdir /s /q <abs>、rd /s /q <abs>、del /q <abs>
//   - PS:   Remove-Item [-Recurse] [-Force] <abs>
//   - Unix: rm -rf <abs>
//
// 命中条件：目标路径（1）位于工作区内，（2）最后一段目录名/文件名命中
// 临时产物名单或前缀。通配符（*）、多目标、工作区外路径一律不降级。
func isWorkspaceScopedDeletion(cmd, workspace string) bool {
	segs := splitCommandChain(strings.ToLower(cmd))
	for _, seg := range segs {
		seg = strings.TrimSpace(seg)
		// 逐词切分，找删除动词后跟的目标 token。
		toks := strings.Fields(seg)
		for i, tk := range toks {
			tk = strings.Trim(tk, `"`)
			isDel := tk == "rmdir" || tk == "rd" || tk == "rm" || tk == "del" ||
				tk == "remove-item" || tk == "ri" || tk == "remove-item.exe"
			if !isDel {
				continue
			}
			// 取删除动词之后的第一个非开关 token 作为目标。
			for j := i + 1; j < len(toks); j++ {
				t := strings.Trim(toks[j], `"`)
				if strings.HasPrefix(t, "-") || strings.HasPrefix(t, "/") && len(t) <= 4 && !strings.Contains(t, `\`) && !strings.Contains(t, ":") {
					continue // 开关（-r / -rf / /s /q）
				}
				if strings.ContainsAny(t, "*?") {
					return false // 通配符不降级
				}
				return workspaceScopedTarget(t, workspace)
			}
		}
	}
	return false
}

// workspaceScopedTarget 单个删除目标判定：工作区内 + 临时产物名/前缀。
func workspaceScopedTarget(target, workspace string) bool {
	target = strings.Trim(target, `"'`)
	if target == "" {
		return false
	}
	// 工作区内才降级（系统盘/用户目录/工作区外一律硬红线）。
	if !withinWorkspace(target, workspace) {
		return false
	}
	base := strings.ToLower(target)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if base == "" || base == "." || base == ".." {
		return false
	}
	if workspaceScopedDirNames[base] {
		return true
	}
	for _, p := range workspaceScopedPrefixes {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// withinWorkspace 判定路径是否位于工作区内（宽容处理相对路径=工作区内）。
func withinWorkspace(target, workspace string) bool {
	if strings.TrimSpace(workspace) == "" {
		return false // 无工作区上下文不降级
	}
	if !strings.Contains(target, ":") && !strings.HasPrefix(target, "/") {
		return true // 相对路径：shell 工具恒在工作区执行
	}
	return strings.HasPrefix(strings.ToLower(filepath.Clean(target)), strings.ToLower(filepath.Clean(workspace)))
}

// CheckDangerousCommand 危险命令检测：小写化后逐段（链式分隔符切分）匹配
// 当前平台清单。空命令安全返回。命中「工作区内临时产物删除」时附带
// WorkspaceScoped 标记（引擎降级为普通审批）。
func CheckDangerousCommand(cmd string) DangerCheck {
	return CheckDangerousCommandScoped(cmd, "")
}

// CheckDangerousCommandAuto 带「危险操作自动确认」语义的检测（用户需求
// 2026-10）：autoConfirm=true 时命中项附带 AutoConfirm=true，引擎据此免审
// 放行（say 风险提示照发，会话历史留痕不缺失）；false 时与 Scoped 完全一致。
func CheckDangerousCommandAuto(cmd, workspace string, autoConfirm bool) DangerCheck {
	dc := CheckDangerousCommandScoped(cmd, workspace)
	if dc.Danger && autoConfirm {
		dc.AutoConfirm = true
	}
	return dc
}

// CheckDangerousCommandScoped 带 workspace 上下文的危险命令检测：
// workspace 非空且删除目标为工作区内临时/构建产物时标记 WorkspaceScoped=true。
func CheckDangerousCommandScoped(cmd, workspace string) DangerCheck {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return DangerCheck{}
	}
	lower := strings.ToLower(trimmed)
	// 本机回环白名单（2026-10-03 第三轮复盘：Invoke-WebRequest
	// http://127.0.0.1:9333/... 被「远程脚本下载」整段误拦，会话 1791004065714
	// 停在审批卡——对回环/本机地址的 GET 探测是健康检查/本地服务调试的常规
	// 动作，下载执行风险场景不适用，直接放行）。必须先于模式匹配：
	// invoke-webrequest 模式命中即提前 return，放后面永远执行不到。
	if isLocalhostProbe(trimmed) {
		return DangerCheck{}
	}
	for _, seg := range splitCommandChain(lower) {
		seg := strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		for _, p := range platformDangerPatterns() {
			if strings.Contains(seg, strings.ToLower(p.match)) {
				// PowerShell 参数误伤排除（2026-10-10 复盘实锤：Get-Date -Format
				// 'yyyy-MM-dd' 撞上磁盘「format 」模式被误拦为格式化磁盘）。
				// 段内出现 -format 参数形态时，命中的是 PS 参数而非 format 命令。
				if strings.Contains(p.match, "format ") && isPSParamSwitch(seg, "format") {
					continue
				}
				dc := DangerCheck{Danger: true, Reason: p.reason}
				// 降级判定：删除动词 + 工作区内临时产物目标。
				if isWorkspaceScopedDeletion(trimmed, workspace) {
					dc.WorkspaceScoped = true
					dc.Reason = "工作区内临时/构建产物删除（可重建，低风险）：" + p.reason
				}
				return dc
			}
		}
	}
	// 本机回环白名单判定已前置（见函数头部）。
	// 远程脚本管道执行（curl ... | sh / bash / pwsh）：分段检测拦不住的
	// 下载即执行形态，此处整串二次确认（见 unixDangerPatterns 注释）。
	if isRemotePipeExec(lower) {
		return DangerCheck{Danger: true, Reason: "远程脚本管道直接执行（下载即运行）"}
	}
	return DangerCheck{}
}

// localhostHosts 本机回环/本机地址主机名单（小写匹配）。
var localhostHosts = []string{
	"localhost", "127.0.0.1", "[::1]", "::1", "0.0.0.0", "127.0.0.",
}

// isLocalhostProbe 命令是否仅为对本机地址的 HTTP 探测（Invoke-WebRequest /
// Invoke-RestMethod / curl / wget + localhost/127.x/[::1]，无管道外传、无下载执行）。
// 典型场景：健康检查（/api/health）、本地 dev server / CDP 端口探测。
func isLocalhostProbe(cmd string) bool {
	lower := strings.ToLower(cmd)
	isProbe := false
	for _, kw := range []string{"invoke-webrequest", "invoke-restmethod", "curl", "wget", "iwr "} {
		if strings.Contains(lower, kw) {
			isProbe = true
			break
		}
	}
	if !isProbe {
		return false
	}
	// 管道转发/执行不适用（curl ... | sh 之类在 isRemotePipeExec 里拦）。
	for _, op := range []string{"|", "&&", ";", "`"} {
		if strings.Contains(cmd, op) {
			return false
		}
	}
	// 必须显式含本机地址（含 127.0.0. 前缀覆盖任意回环端口）。
	hasLocal := false
	for _, h := range localhostHosts {
		if strings.Contains(lower, h) {
			hasLocal = true
			break
		}
	}
	return hasLocal
}

// isPSParamSwitch 判定段内是否含 PowerShell 参数开关 -<name>（如 -Format）。
// 用于区分「format 磁盘命令」与「Get-Date -Format yyyy-MM-dd 参数」：
// 参数形态存在时，子串命中的是 PS 参数而非独立 format 命令，放行。
func isPSParamSwitch(seg, name string) bool {
	// 段内查 "-format"（PS 参数大小写无关，seg 已小写化）：
	// 匹配 -format、-format: 值或独立 token；不匹配 xformat/portal 等词中子串。
	for i := 0; i+1+len(name) <= len(seg); i++ {
		if seg[i] != '-' {
			continue
		}
		rest := seg[i+1:]
		if !strings.HasPrefix(rest, name) {
			continue
		}
		// 参数名后必须是边界：空格/冒号/行尾（-format: 'yyyy' / -format yyyy）。
		tail := rest[len(name):]
		if tail == "" || tail[0] == ' ' || tail[0] == ':' || tail[0] == '"' || tail[0] == '\'' {
			return true
		}
	}
	return false
}

// isRemotePipeExec 远程脚本管道执行检测（补 splitCommandChain 分段盲区）：
// 命令串中「curl/wget 下载」与「sh/bash/pwsh/iex」被 | 分隔在不同段里，
// 逐段匹配永远命不中整串模式；这里按段序判定——下载段之后存在执行段
// （curl x | sh / curl -o f url && sh f）才触发。单段包装（powershell -Command
// "curl -O url"，同段同时含下载与执行关键字）不命中，不扩大误报面。
func isRemotePipeExec(lowerCmd string) bool {
	downloadSeg := -1
	for i, seg := range splitCommandChain(lowerCmd) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		for _, kw := range []string{"curl ", "curl.exe", "wget ", "wget.exe", "invoke-webrequest", "invoke-restmethod", "iwr "} {
			if strings.HasPrefix(seg, kw) || strings.Contains(seg, "&& "+kw) || strings.Contains(seg, "; "+kw) {
				downloadSeg = i
			}
		}
		if downloadSeg >= 0 && i > downloadSeg {
			for _, kw := range []string{"sh", "bash", "pwsh", "powershell", "iex", "python", "python3"} {
				if seg == kw || strings.HasPrefix(seg, kw+" ") {
					return true
				}
			}
		}
	}
	return false
}
