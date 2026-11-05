// Package plugin 插件体系：Claude Code 风格的插件包 + MCP 接入。
//
// 设计目标（对齐用户需求）：
//   - 插件以独立进程形态运行，通过 MCP 协议（stdio: 本地子进程 / http|sse:
//     远程服务）与 Kaelvoris 通信——实现语言不限（Go/Python/Node/任意）；
//   - 插件完全隔离于主工程：独立 manifest（plugin.json）+ 独立安装目录
//     ~/.kaelvoris/plugins/<id>/，内核不 import 插件任何代码；
//   - 安装/启用/卸载与 MCP 服务器配置打通：启用插件自动把其 MCP 端点注册到
//     任务级工具 Registry（models 经 function calling 直接调用）；
//   - 对齐 Claude Code 插件生态语义：目录内 plugin.json 声明 name/description/
//     version/entry（启动命令）/mcp（端点配置）/skills（可选技能包）。
//
// 目录约定（对齐 Claude Code ~/.claude/plugins/）：
//
//	~/.kaelvoris/plugins/<id>/plugin.json   ← manifest
//	~/.kaelvoris/plugins/<id>/<二进制/脚本>  ← entry 启动物
//	~/.kaelvoris/plugins.json               ← 全局开关表（id → enabled）
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Manifest 插件清单（plugin.json）——schema 对齐 Claude Code 插件清单的
// 最小子集 + kaelvoris 扩展字段（mcp 端点描述）。
type Manifest struct {
	ID          string     `json:"id"`                    // 唯一 ID（目录名）
	Name        string     `json:"name"`                  // 显示名
	Version     string     `json:"version,omitempty"`     // 版本
	Description string     `json:"description,omitempty"` // 一句话描述
	Author      string     `json:"author,omitempty"`      // 作者/仓库
	Entry       string     `json:"entry,omitempty"`       // stdio 启动命令（可用绝对路径或"cmd args"）
	MCP         *MCPConfig `json:"mcp,omitempty"`         // MCP 端点配置（entry 与 mcp 二选一）
	Skills      []string   `json:"skills,omitempty"`      // 附带技能目录（可选）
	Source      string     `json:"source,omitempty"`      // builtin | manual | git
	InstalledAt int64      `json:"installedAt,omitempty"` // 安装时间 ms
	// Dir 插件安装目录（运行时填充，供命令相对路径解析）。
	Dir string `json:"-"`
}

// MCPConfig 插件的 MCP 端点配置（stdio 或远程 http/sse）。
type MCPConfig struct {
	Transport string            `json:"transport"`         // stdio | http | sse
	Command   string            `json:"command,omitempty"` // stdio：启动命令
	Args      []string          `json:"args,omitempty"`    // stdio：命令参数
	URL       string            `json:"url,omitempty"`     // 远程：服务端 URL
	Headers   map[string]string `json:"headers,omitempty"` // 远程：认证头
	Env       map[string]string `json:"env,omitempty"`     // 环境变量
}

// pluginsRoot 插件安装根目录（~/.kaelvoris/plugins）。
func pluginsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kaelvoris", "plugins"), nil
}

// statePath 全局开关表（~/.kaelvoris/plugins.json）。
func statePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kaelvoris", "plugins.json"), nil
}

// ReadManifest 读取单个插件目录的 plugin.json（不存在/损坏返回 nil, nil）。
func ReadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// 剥 BOM（PowerShell 5.1 写出的 UTF-8 文件默认带 BOM）。
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("plugin.json 解析失败: %w", err)
	}
	if m.ID == "" {
		m.ID = filepath.Base(dir)
	}
	return &m, nil
}

// WriteManifest 写入插件清单（安装时用）。
func WriteManifest(dir string, m *Manifest) error {
	if m == nil {
		return errors.New("manifest is nil")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "plugin.json"), b, 0o644)
}

// List 发现全部已安装插件（读取每个子目录的 plugin.json；损坏目录跳过）。
func List() []*Manifest {
	root, err := pluginsRoot()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []*Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := ReadManifest(filepath.Join(root, e.Name()))
		if err != nil || m == nil {
			continue
		}
		m.Dir = filepath.Join(root, e.Name())
		out = append(out, m)
	}
	// 按安装时间倒序。
	sort.Slice(out, func(i, j int) bool { return out[i].InstalledAt > out[j].InstalledAt })
	return out
}

// Get 按 ID 查找插件。
func Get(id string) (*Manifest, error) {
	root, err := pluginsRoot()
	if err != nil {
		return nil, err
	}
	m, err := ReadManifest(filepath.Join(root, id))
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("插件未安装: %s", id)
	}
	return m, nil
}

// Remove 删除插件目录（卸载）。
func Remove(id string) error {
	root, err := pluginsRoot()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, id)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	setEnabled(id, false)
	return nil
}

// ---------- 开关状态 ----------

// state 全局开关表（~/.kaelvoris/plugins.json：{id: {"enabled": bool}}）。
type state struct {
	Plugins map[string]pluginState `json:"plugins"`
}

type pluginState struct {
	Enabled bool `json:"enabled"`
}

// loadState 读取开关表（不存在返回空表）。
func loadState() *state {
	p, err := statePath()
	if err != nil {
		return &state{Plugins: map[string]pluginState{}}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return &state{Plugins: map[string]pluginState{}}
	}
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil || s.Plugins == nil {
		s.Plugins = map[string]pluginState{}
	}
	return &s
}

// saveState 写回开关表。
func saveState(s *state) error {
	p, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// IsEnabled 插件是否启用（默认启用：未记录 = 启用）。
func IsEnabled(id string) bool {
	s := loadState()
	st, ok := s.Plugins[id]
	if !ok {
		return true
	}
	return st.Enabled
}

// SetEnabled 设置插件启用状态。
func SetEnabled(id string, enabled bool) error {
	s := loadState()
	if s.Plugins == nil {
		s.Plugins = map[string]pluginState{}
	}
	s.Plugins[id] = pluginState{Enabled: enabled}
	return saveState(s)
}

// setEnabled 静默设置（Remove 内部用）。
func setEnabled(id string, enabled bool) {
	_ = SetEnabled(id, enabled)
}

// ---------- MCP 配置导出 ----------

// MCPServerOf 从插件 manifest 推导 MCP 服务器配置（供 app 层接入 MCP 运行时）。
// 返回 ok=false 表示插件没有 MCP 端点（纯技能插件）。
func (m *Manifest) MCPServerOf() (name, transport, command, url string, args []string, env, headers map[string]string, ok bool) {
	if m.MCP != nil && m.MCP.Transport != "" {
		if m.MCP.Transport == "stdio" {
			return m.ID, "stdio", cmdOr(m.MCP.Command, m.Entry), "", m.MCP.Args, m.MCP.Env, m.MCP.Headers, true
		}
		return m.ID, m.MCP.Transport, "", m.MCP.URL, nil, m.MCP.Env, m.MCP.Headers, true
	}
	if strings.TrimSpace(m.Entry) != "" {
		return m.ID, "stdio", m.Entry, "", nil, nil, nil, true
	}
	return "", "", "", "", nil, nil, nil, false
}

func cmdOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// CopyFrom 从其他插件目录复制（安装用；跨盘安全）。
func CopyFrom(srcDir, dstDir string) error {
	return filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dstDir, 0o755)
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dstDir, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dstDir, rel), b, info.Mode())
	})
}

// CopyFromFS 从 embed.FS（或任意 fs.FS）目录复制（内置插件 embed 解包安装用；
// 语义同 CopyFrom：跳过 .git/node_modules，按 fs 路径逐文件落地）。
func CopyFromFS(fsys fs.FS, srcDir, dstDir string) error {
	return fs.WalkDir(fsys, srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dstDir, 0o755)
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dstDir, rel), 0o755)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dstDir, rel), b, 0o755)
	})
}

// EnsureInstallMarker 安装后记录时间戳（Remove 时不依赖）。
func (m *Manifest) touchInstalled() {
	m.InstalledAt = time.Now().UnixMilli()
}
