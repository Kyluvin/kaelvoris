package desktop

// builtin_plugins.go —— 内置插件发行目录（go:embed 随二进制分发）。
//
// 为什么 embed：市场「下载安装」对内置项（office/ponytail）走本地发行目录
// 安装（无服务端下载源）；旧实现按工作目录相对路径找 plugins/<id>-plugin，
// 桌面端部署后工作目录非仓库根必报「发行目录不存在」。embed 后任何分发
// 渠道（本地部署 / 公网下载升级 / 便携包）都能安装，目录内布局与安装布局
// 一致（plugin.json + entry 二进制平铺，mcp.command 相对插件根可直接解析）。
//
// 同步约定：bin/ 由各插件 build.ps1 产出（gitignore，不入库）；发布前运行
// sync-builtin-plugins.ps1 从 plugins/*/ 刷新本目录再 wails build。

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"kaelvoris/internal/plugin"
)

//go:embed all:builtin_plugins
var builtinPluginsFS embed.FS

// constBuiltinPluginsDir embed 根目录（子目录 = 插件 ID）。
const constBuiltinPluginsDir = "builtin_plugins"

// builtinPluginIDs embed 发行目录内全部插件 ID（子目录名）。
func builtinPluginIDs() []string {
	entries, err := fs.ReadDir(builtinPluginsFS, constBuiltinPluginsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// hasBuiltinPlugin 内置发行目录是否收录该 ID。
func hasBuiltinPlugin(id string) bool {
	for _, name := range builtinPluginIDs() {
		if name == id {
			return true
		}
	}
	return false
}

// installBuiltinFromDir 内置目录项安装：从 embed 发行目录解包复制到
// ~/.kaelvoris/plugins/<id>/（远程未配置时「下载安装」的可用路径，与
// builtinPlugins 清单 ID 对应；替代旧的工作目录相对路径查找——部署后必失败）。
func (a *App) installBuiltinFromDir(id string) (*PluginView, error) {
	id = strings.TrimSpace(id)
	if !hasBuiltinPlugin(id) {
		return nil, fmt.Errorf("内置插件 %s 未随发行版打包（内置目录：%v）", id, builtinPluginIDs())
	}
	root, err := pluginsRootDir()
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(root, id)
	if _, err := os.Stat(dst); err == nil {
		return nil, fmt.Errorf("插件 %s 已安装（如需更新请先卸载）", id)
	}
	if err := plugin.CopyFromFS(builtinPluginsFS, constBuiltinPluginsDir+"/"+id, dst); err != nil {
		_ = os.RemoveAll(dst)
		return nil, err
	}
	// 复制后以目标清单为准，补安装时间与来源（同 InstallPluginFromURL 语义）。
	installed, err := plugin.ReadManifest(dst)
	if err != nil || installed == nil {
		_ = os.RemoveAll(dst)
		return nil, fmt.Errorf("插件 %s 复制后清单不可读", id)
	}
	installed.InstalledAt = nowMs()
	installed.Source = orString(installed.Source, "builtin")
	if err := plugin.WriteManifest(dst, installed); err != nil {
		_ = os.RemoveAll(dst)
		return nil, err
	}
	v := toView(installed)
	a.emitPluginsRefresh()
	return &v, nil
}
