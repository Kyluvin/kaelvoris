package desktop

// plugin_panel.go —— 插件面板（Wails 绑定）。
//
// 插件体系（对齐 Claude Code 插件模式 + MCP 接入）：
//   - 插件 = ~/.kaelvoris/plugins/<id>/ 目录（plugin.json 清单 + entry 可执行体），
//     通过 MCP 协议（stdio 子进程 / http|sse 远程服务）与内核通信；
//   - 实现语言不限（Go/Python/Node/任意），完全隔离于主工程（零 import）；
//   - 启用插件自动接入任务级 MCP 运行时（buildMCPTools 发现并连接，
//     tools 以 mcp_<plugin>_<tool> 注册给模型）；
//   - 开关状态落 ~/.kaelvoris/plugins.json（默认启用）。
//
// 安装方式：
//   - InstallPluginFromDir：从本地目录复制（office 插件内置发行即此路径）；
//   - InstallPluginFromURL：从 URL 下载 zip 压缩包解压安装（插件市场/下载安装）；
//   - ListAvailablePlugins：内置可下载插件目录（前端「插件市场」展示 + 一键安装）。

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kaelvoris/internal/plugin"
)

// PluginView 前端插件视图（manifest + 启用状态 + 工具数预估）。
type PluginView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	Source      string `json:"source,omitempty"`
	Enabled     bool   `json:"enabled"`
	InstalledAt int64  `json:"installedAt,omitempty"`
	// MCPInfo 端点摘要（前端展示「stdio 子进程」或「远程 http」）。
	MCPInfo string `json:"mcpInfo,omitempty"`
	// SkillCount 附带技能数（纯技能插件也可见）。
	SkillCount int `json:"skillCount,omitempty"`
}

// toView Manifest → PluginView。
func toView(m *plugin.Manifest) PluginView {
	v := PluginView{
		ID:          m.ID,
		Name:        m.Name,
		Version:     m.Version,
		Description: m.Description,
		Author:      m.Author,
		Source:      m.Source,
		Enabled:     plugin.IsEnabled(m.ID),
		InstalledAt: m.InstalledAt,
		SkillCount:  len(m.Skills),
	}
	if m.MCP != nil {
		if m.MCP.Transport == "stdio" || m.MCP.Transport == "" {
			v.MCPInfo = "stdio 子进程"
		} else {
			v.MCPInfo = "远程 " + m.MCP.Transport
		}
	} else if strings.TrimSpace(m.Entry) != "" {
		v.MCPInfo = "stdio 子进程"
	}
	return v
}

// ListPlugins 列出全部已安装插件（含启用状态）。
func (a *App) ListPlugins() []PluginView {
	list := plugin.List()
	out := make([]PluginView, 0, len(list))
	for _, m := range list {
		out = append(out, toView(m))
	}
	return out
}

// InstallPluginFromDirRequest 安装入参（本地目录复制）。
type InstallPluginFromDirRequest struct {
	Dir string `json:"dir"` // 插件源码目录（含 plugin.json）
}

// InstallPluginFromDir 从本地目录安装插件：复制到 ~/.kaelvoris/plugins/<id>/。
// dir 内必须含 plugin.json（id 取清单 ID 或目录名）。
func (a *App) InstallPluginFromDir(req InstallPluginFromDirRequest) (*PluginView, error) {
	dir := strings.TrimSpace(req.Dir)
	if dir == "" {
		return nil, errors.New("请填写插件目录路径")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	m, err := plugin.ReadManifest(abs)
	if err != nil {
		return nil, fmt.Errorf("读取插件清单失败: %w", err)
	}
	if m == nil {
		return nil, errors.New("目录中未找到 plugin.json")
	}
	if m.ID == "" {
		m.ID = strings.ToLower(filepath.Base(abs))
	}
	if m.ID == "" {
		return nil, errors.New("无法确定插件 ID（plugin.json 缺失 id 且目录名无效）")
	}
	// 目标目录。
	root, err := pluginsRootDir()
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(root, m.ID)
	if _, err := os.Stat(dst); err == nil {
		return nil, fmt.Errorf("插件 %s 已安装（如需更新请先卸载）", m.ID)
	}
	if err := plugin.CopyFrom(abs, dst); err != nil {
		return nil, fmt.Errorf("复制插件失败: %w", err)
	}
	// 重新读取（复制后以目标为准）。
	installed, err := plugin.ReadManifest(dst)
	if err != nil || installed == nil {
		_ = os.RemoveAll(dst)
		return nil, errors.New("插件复制后清单不可读")
	}
	installed.InstalledAt = nowMs()
	installed.Source = orString(installed.Source, "local")
	if err := plugin.WriteManifest(dst, installed); err != nil {
		_ = os.RemoveAll(dst)
		return nil, err
	}
	v := toView(installed)
	a.emitPluginsRefresh()
	return &v, nil
}

// UninstallPlugin 卸载插件（删除目录 + 关闭开关记录）。
func (a *App) UninstallPlugin(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("插件 ID 不能为空")
	}
	if err := plugin.Remove(id); err != nil {
		return err
	}
	a.emitPluginsRefresh()
	return nil
}

// TogglePlugin 切换插件启用状态（下次任务生效）。
func (a *App) TogglePlugin(id string, enabled bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("插件 ID 不能为空")
	}
	if _, err := plugin.Get(id); err != nil {
		return err
	}
	if err := plugin.SetEnabled(id, enabled); err != nil {
		return err
	}
	a.emitPluginsRefresh()
	return nil
}

// emitPluginsRefresh 广播插件列表刷新。
func (a *App) emitPluginsRefresh() {
	uiEmit(a.ctx, "kaelvoris:plugins", a.ListPlugins())
}

// pluginsRootDir ~/.kaelvoris/plugins 根（复用 plugin 包逻辑）。
func pluginsRootDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kaelvoris", "plugins"), nil
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}

func orString(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// AvailablePlugin 内置可下载插件目录项（插件市场展示）。
type AvailablePlugin struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	DownloadURL string `json:"downloadUrl,omitempty"`
	Installed   bool   `json:"installed"`
}

// builtinPlugins 内置可下载插件目录（硬编码清单；downloadURL 为占位，
// 可由发布源/插件市场填充。当前收录 ponytail 与 office 两个官方插件）。
func builtinPlugins() []AvailablePlugin {
	return []AvailablePlugin{
		{
			ID:          "ponytail",
			Name:        "Ponytail 懒人资深工程师",
			Version:     "0.2.0",
			Description: "治 agent 话痨 / 过度工程：YAGNI 阶梯审查、过度工程审计、输出克制检查、ponytail: 债务台账（mcp_ponytail_* 工具，含 6 个内置技能）。",
			DownloadURL: "https://github.com/kaelvoris/plugins/releases/download/ponytail/ponytail.zip",
		},
		{
			ID:          "office",
			Name:        "Office 办公文档",
			Version:     "0.1.0",
			Description: "Excel/Word/PPT 办公文档创建、编辑、导入与导出（xlsx/csv/tsv/docx/pptx）。",
			DownloadURL: "https://github.com/kaelvoris/plugins/releases/download/office/office.zip",
		},
		{
			ID:          "rea",
			Name:        "REA 逆向一切",
			Version:     "6.5.0",
			Description: "逆向工程一切（Reverse Engineer Anything）：原生二进制 / JS·Electron / .NET / 网页的逆向分析 MCP（委托官方 rea-agents，需 Node 22+），附 reverse-engineer-anything 工作流技能。",
			DownloadURL: "https://github.com/kaelvoris/plugins/releases/download/rea/rea.zip",
		},
	}
}

// ListAvailablePlugins 列出内置可下载插件目录（含是否已安装）。
func (a *App) ListAvailablePlugins() []AvailablePlugin {
	out := builtinPlugins()
	for i := range out {
		if _, err := plugin.Get(out[i].ID); err == nil {
			out[i].Installed = true
		}
	}
	return out
}

// InstallPluginFromURLRequest 下载安装入参。
type InstallPluginFromURLRequest struct {
	URL string `json:"url"` // zip 包下载地址（内含 plugin.json）
}

// InstallPluginFromURL 从 URL 下载 zip 压缩包并解压安装到 ~/.kaelvoris/plugins/<id>/。
// zip 内需含 plugin.json（id 取清单 ID 或包内顶层目录名）。
// 下载后解压到临时目录，先校验 plugin.json 再复制到目标（避免半成品安装）。
func (a *App) InstallPluginFromURL(req InstallPluginFromURLRequest) (*PluginView, error) {
	url := strings.TrimSpace(req.URL)
	if url == "" {
		return nil, errors.New("请填写插件下载地址")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, errors.New("下载地址必须是 http(s) 链接")
	}

	// 1) 下载 zip 到内存（插件包一般很小）。
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20)) // 256MB 上限
	if err != nil {
		return nil, fmt.Errorf("读取下载内容失败: %w", err)
	}

	// 2) 解压到临时目录。
	tmp, err := os.MkdirTemp("", "kaelvoris-plugin-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip 解析失败: %w", err)
	}
	if err := extractZip(zr, tmp); err != nil {
		return nil, fmt.Errorf("zip 解压失败: %w", err)
	}

	// 3) 定位清单目录（包内任意层级含 plugin.json 的目录）。
	srcDir := findManifestDir(tmp)
	if srcDir == "" {
		return nil, errors.New("压缩包内未找到 plugin.json")
	}
	m, err := plugin.ReadManifest(srcDir)
	if err != nil || m == nil {
		return nil, errors.New("压缩包内 plugin.json 不可读")
	}
	if m.ID == "" {
		m.ID = strings.ToLower(filepath.Base(srcDir))
	}
	if m.ID == "" {
		return nil, errors.New("无法确定插件 ID")
	}

	// 4) 安装到目标（若已存在则升级：先清理旧目录再复制）。
	root, err := pluginsRootDir()
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(root, m.ID)
	if _, err := os.Stat(dst); err == nil {
		if err := os.RemoveAll(dst); err != nil {
			return nil, fmt.Errorf("清理旧插件失败: %w", err)
		}
	}
	if err := plugin.CopyFrom(srcDir, dst); err != nil {
		return nil, fmt.Errorf("复制插件失败: %w", err)
	}

	// 5) 重新读取（复制后以目标为准），补时间戳与来源。
	installed, err := plugin.ReadManifest(dst)
	if err != nil || installed == nil {
		_ = os.RemoveAll(dst)
		return nil, errors.New("插件复制后清单不可读")
	}
	installed.InstalledAt = nowMs()
	installed.Source = orString(installed.Source, "download")
	if err := plugin.WriteManifest(dst, installed); err != nil {
		_ = os.RemoveAll(dst)
		return nil, err
	}
	v := toView(installed)
	a.emitPluginsRefresh()
	return &v, nil
}

// extractZip 将 zip 内容解压到 dst（防 zip-slip：拒绝 .. 路径）。
func extractZip(zr *zip.Reader, dst string) error {
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel := filepath.Clean(f.Name)
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("非法路径: %s", f.Name)
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 64<<20))
		rc.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, b, f.Mode()|0o600); err != nil {
			return err
		}
	}
	return nil
}

// findManifestDir 在解压目录中定位含 plugin.json 的目录（任意层级，优先顶层）。
func findManifestDir(root string) string {
	// 顶层直接有 plugin.json。
	if _, err := os.Stat(filepath.Join(root, "plugin.json")); err == nil {
		return root
	}
	// 扫描一级子目录（zip 包通常把内容放在单个顶层目录）。
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(d, "plugin.json")); err == nil {
			return d
		}
	}
	return ""
}
