// version_panel.go —— 版本与升级面板（Wails 绑定）。
//
// 面板能力（设置 → 「版本与升级」页 + 顶栏「有新版本」徽标）：
//   - GetVersionInfo：当前版本 / 构建信息 / 内置更新日志；
//   - CheckUpdate：拉取升级源清单，比较版本，返回是否有新版与新增功能说明；
//   - ApplyUpdate：下载新版 → 暂存 → 写标记 → 替换脚本 → 退出重启；
//   - DismissUpdate：「忽略此版本」（~/.kaelvoris/update-dismissed.json）；
//   - 启动时自检：后台拉取清单，发现新版推 kaelvoris:update_available 事件。
package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kaelvoris/internal/version"
)

// 升级源配置（持久化 ~/.kaelvoris/update-config.json；空 = 不检查）。
// 示例：{"source": "http://192.168.1.10/kaelvoris/update.json"}
// 或本地部署同盘：{"source": "D:\\service\\kaelvoris\\update.json"}
type UpdateConfig struct {
	Source string `json:"source"` // 清单 URL / 文件路径（空 = 不检查更新）
	// AutoCheck 启动时自动检查（默认 true）。
	AutoCheck *bool `json:"autoCheck,omitempty"`
}

// VersionInfo 版本面板数据。
type VersionInfo struct {
	Number    string                  `json:"number"`            // 当前版本号
	BuildDate string                  `json:"buildDate"`         // 构建时间（可空）
	Commit    string                  `json:"commit"`            // 构建提交（可空）
	OS        string                  `json:"os"`                // 运行平台
	Changelog []version.Entry         `json:"changelog"`         // 内置更新日志（新版在前）
	Source    string                  `json:"source"`            // 升级源（空 = 未配置）
	AutoCheck bool                    `json:"autoCheck"`         // 启动自动检查
	Dismissed string                  `json:"dismissed"`         // 已忽略的版本
	Pending   *version.PendingUpgrade `json:"pending,omitempty"` // 待完成的升级（重启后替换）
}

// updateConfigPath 升级源配置文件路径。
func updateConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "update-config.json")
}

// defaultUpdateSource 默认升级源。不内置硬编码地址：升级源一律由用户在
// 设置 → 「版本与升级」面板配置（或 ~/.kaelvoris/update-config.json 填 source），
// 未配置 = 不检查更新（面板与顶栏自检均静默跳过）。
const defaultUpdateSource = ""

// dismissedPath 忽略版本记录路径。
func dismissedPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "update-dismissed.json")
}

// loadUpdateConfig 读取升级源配置（无文件/未填 Source = 默认空，即不检查；AutoCheck 默认 true）。
func loadUpdateConfig() UpdateConfig {
	c := UpdateConfig{Source: defaultUpdateSource, AutoCheck: boolPtr(true)}
	if err := jsonReadFileBOM(updateConfigPath(), &c); err != nil && !os.IsNotExist(err) {
		slog.Warn("update.json 解析失败（已忽略，按默认值处理）", "err", err)
	}
	if c.AutoCheck == nil {
		c.AutoCheck = boolPtr(true)
	}
	if strings.TrimSpace(c.Source) == "" {
		// 显式保存过空 Source = 用户不想检查更新（面板提示「空 = 不检查」），尊重之。
		c.Source = ""
	}
	return c
}

// saveUpdateConfigFile 持久化升级源配置。
func saveUpdateConfigFile(c UpdateConfig) error {
	if err := os.MkdirAll(filepath.Dir(updateConfigPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(updateConfigPath(), b, 0o600)
}

// loadDismissed 读取忽略的版本（空 = 无）。
func loadDismissed() string {
	b, err := os.ReadFile(dismissedPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// saveDismissed 持久化忽略版本。
func saveDismissed(v string) error {
	if err := os.MkdirAll(filepath.Dir(dismissedPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dismissedPath(), []byte(v), 0o600)
}

func boolPtr(b bool) *bool { return &b }

// ---------- 绑定方法 ----------

// GetVersionInfo 版本面板数据（版本 / 构建 / 更新日志 / 升级配置）。
func (a *App) GetVersionInfo() VersionInfo {
	cfg := loadUpdateConfig()
	info := VersionInfo{
		Number:    version.Number,
		BuildDate: version.BuildDate,
		Commit:    version.BuildCommit,
		OS:        goos(),
		Changelog: version.Changelog,
		Source:    cfg.Source,
		AutoCheck: cfg.AutoCheck != nil && *cfg.AutoCheck,
		Dismissed: loadDismissed(),
	}
	if p := version.ReadPending(); p != nil {
		info.Pending = p
	}
	return info
}

// SaveUpdateConfig 保存升级源配置（Source 空 = 不检查更新）。
func (a *App) SaveUpdateConfig(source string, autoCheck bool) error {
	c := UpdateConfig{Source: strings.TrimSpace(source), AutoCheck: &autoCheck}
	if err := saveUpdateConfigFile(c); err != nil {
		return err
	}
	// 保存后立即按新源检查一次（有新版实时提示）。
	if c.Source != "" && autoCheck {
		go a.checkUpdateAndNotify()
	}
	return nil
}

// CheckUpdate 立即检查更新（面板「检查更新」按钮）。
// 面板主动检查不受「忽略此版本」影响（用户主动查看展示真实状态）。
func (a *App) CheckUpdate() (*version.CheckResult, error) {
	src := loadUpdateConfig().Source
	if strings.TrimSpace(src) == "" {
		return nil, errors.New("未配置升级源（请填写 update.json 清单地址）")
	}
	r, err := version.Check(src)
	if err != nil {
		return nil, err
	}
	a.updateMu.Lock()
	a.lastCheck = r
	a.updateMu.Unlock()
	return r, nil
}

// ApplyUpdate 执行升级：下载 → 暂存 → 写标记 → 替换脚本 → 退出重启。
// 返回错误时应用保持运行（升级未执行）；成功则进程退出、新版自启。
func (a *App) ApplyUpdate(target string) error {
	src := loadUpdateConfig().Source
	if strings.TrimSpace(src) == "" {
		return errors.New("未配置升级源")
	}
	// 重新检查（拿最新清单与下载地址；防面板缓存过期地址）。
	res, err := version.Check(src)
	if err != nil {
		return err
	}
	if !res.Update {
		return errors.New("已是最新版本，无需升级")
	}
	if target != "" && version.Compare(res.Latest, strings.TrimPrefix(target, "v")) != 0 {
		return errors.New("升级源版本已变化（" + res.Latest + "），请刷新后重试")
	}
	// 下载到临时目录（Download 内部已做体积门禁 + PE 头 + 清单 SHA256 校验，
	// 校验失败不会返回路径——坏包在进入替换链路前即被拦截）。
	downloaded, err := version.Download(res.AssetURL, tempUpdateDir(), res.Sha256)
	if err != nil {
		return err
	}
	// 暂存到 exe 同目录（同盘原子）。升级后续步骤任何失败都要清理暂存，
	// 避免坏包残留在部署目录（下次启动误判为待重试升级）。
	staged, err := version.StageUpgrade(downloaded)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(staged)
		}
	}()
	// 双保险：暂存后再次校验（磁盘写入损坏/杀软改写等极端情况）。
	if err := version.ValidateExecutable(staged, 0, res.Sha256); err != nil {
		return fmt.Errorf("新版产物校验未通过，已取消升级: %w", err)
	}
	// 写标记 + 启动替换脚本 + 退出。
	p := version.PendingUpgrade{
		Target: res.Latest,
		Staged: staged,
		Notes:  strings.Join(res.Notes, "\n"),
	}
	if err := version.ApplyAndRestart(p); err != nil {
		return err
	}
	// 替换脚本已启动（等待本进程退出）；优雅退出。
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
	}()
	return nil
}

// DismissUpdate 忽略此版本（顶栏不再提示；面板手动检查仍显示）。
func (a *App) DismissUpdate(v string) error {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return errors.New("版本号不能为空")
	}
	return saveDismissed(v)
}

// LastUpdateCheck 面板打开时回放最近一次静默检查结果（免再次网络请求）。
func (a *App) LastUpdateCheck() *version.CheckResult {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	return a.lastCheck
}

// checkUpdateAndNotify 启动/配置保存后的静默检查：有新版且未被忽略时
// 推送 kaelvoris:update_available（顶栏徽标 + toast 提示新版新增功能）。
func (a *App) checkUpdateAndNotify() {
	src := loadUpdateConfig().Source
	if strings.TrimSpace(src) == "" {
		return
	}
	res, err := version.Check(src)
	if err != nil {
		return // 静默检查失败不打扰用户（面板手动检查可见错误）
	}
	a.updateMu.Lock()
	a.lastCheck = res
	a.updateMu.Unlock()
	if !res.Update {
		return
	}
	if loadDismissed() == res.Latest {
		return // 用户已忽略此版本
	}
	a.emitEvent("kaelvoris:update_available", map[string]any{
		"current": res.Current,
		"latest":  res.Latest,
		"notes":   res.Notes,
		"date":    res.Date,
	})
}

// tempUpdateDir 下载暂存目录。
func tempUpdateDir() string {
	return filepath.Join(os.TempDir(), "kaelvoris-update")
}

// pendingUpgradeNotice 启动时检查升级标记：上一次升级成功后新版进程首启
// 返回提示信息（「已升级到 vX + 新功能」）并清除标记；脚本失败留下待重试
// 标记时返回说明。startup 中组装事件推给前端。
func pendingUpgradeNotice() map[string]any {
	// 升级失败回滚记录优先：脚本探活失败已自动回滚旧版，此处必须先于
	// 「升级成功」判定（回滚后暂存文件同样消失，会被旧逻辑误报为升级成功）。
	if msg := version.ReadFailedNotice(); msg != "" {
		version.ClearFailedNotice()
		version.ClearPending()
		return map[string]any{
			"upgraded":   false,
			"rolledBack": true,
			"message":    msg,
		}
	}
	p := version.ReadPending()
	if p == nil {
		return nil
	}
	// 标记存在但暂存文件已消失 → 批处理已替换完成（新版首启）。
	if _, err := os.Stat(p.Staged); err != nil {
		version.ClearPending()
		return map[string]any{
			"upgraded": true,
			"target":   p.Target,
			"notes":    splitLines(p.Notes),
		}
	}
	// 暂存文件仍在 → 批处理未执行成功（升级中断），保留标记待重试。
	return map[string]any{
		"upgraded": false,
		"target":   p.Target,
		"staged":   p.Staged,
	}
}

// splitLines 多行文本 → 行数组（空行剔除）。
func splitLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}
