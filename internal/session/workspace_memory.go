// Package session 工作空间记忆（用户需求 2026-10-31：记忆分两层——
// 会话级 MEMORY.md（memory.go）+ 项目工作空间级 WORKSPACE.md（本文件））。
//
// 工作空间记忆跨会话共享、按项目目录隔离：同一项目下的所有会话读写同一份
// 记忆，会话结束后经验沉淀留给后续会话续用。落盘位置：
//
//	~/.kaelvoris/workspace-memory/<工作区绝对路径的 SHA-256 前 16 hex>.memory.md
//
// 用哈希而非路径转义做目录名：Windows 路径含盘符冒号/反斜杠，直接转义跨平台
// 歧义多；原始路径以首行注释形式写进文档体，面板展示时人类可读。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// WorkspaceMemoryStore 工作空间记忆存储（按工作区绝对路径隔离，低频写入无缓存）。
type WorkspaceMemoryStore struct {
	dir string
	mu  sync.Mutex
}

// NewWorkspaceMemoryStore 创建工作空间记忆存储（dir = 根目录，其下按哈希分文件）。
func NewWorkspaceMemoryStore(dir string) *WorkspaceMemoryStore {
	return &WorkspaceMemoryStore{dir: dir}
}

// workspaceMemoryDir 工作空间记忆根目录（~/.kaelvoris/workspace-memory）。
func workspaceMemoryDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".kaelvoris", "workspace-memory")
}

// workspaceKey 工作区路径 → 存储文件键（SHA-256 前 16 hex；大小写不敏感归一，
// Windows 盘符大小写差异不产生两份记忆）。
func workspaceKey(ws string) string {
	norm := strings.ToLower(strings.TrimSpace(filepath.Clean(ws)))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:8])
}

// Path 工作区记忆文档绝对路径（绑定层展示 + 提示词注入；ws 为空返回空串）。
func (w *WorkspaceMemoryStore) Path(ws string) string {
	if strings.TrimSpace(ws) == "" {
		return ""
	}
	return filepath.Join(w.dir, workspaceKey(ws)+memorySuffix)
}

// Load 读取工作区记忆文档（不存在/工作区为空返回空串）。
func (w *WorkspaceMemoryStore) Load(ws string) string {
	if strings.TrimSpace(ws) == "" {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	b, err := os.ReadFile(w.Path(ws))
	if err != nil {
		return ""
	}
	return string(b)
}

// lastActiveHeading 「最近交互」节标题（每次交互自动更新；单节覆盖式）。
const lastActiveHeading = "## 最近交互"

// Touch 每次交互自动更新「最近交互」节（宿主 say 桥拦截 api_req_finished 调用）：
//   - 无该节：追加到文档末尾（空文档时先建骨架）；
//   - 已有该节：整节替换（同一会话多轮只保留最新时间，防膨胀）。
//
// 与 Save（用户编辑覆盖）互不干扰：Touch 只动「最近交互」节，正文原样保留。
func (w *WorkspaceMemoryStore) Touch(ws, line string) error {
	if strings.TrimSpace(ws) == "" {
		return fmt.Errorf("工作区路径为空，无法更新工作空间记忆")
	}
	old := w.Load(ws)
	var b strings.Builder
	if old == "" {
		b.WriteString("# 工作空间记忆\n\n跨会话共享的项目级记忆（架构决策/构建命令/偏好/坑点）。\n")
	} else {
		b.WriteString(old)
		if !strings.HasSuffix(old, "\n") {
			b.WriteString("\n")
		}
	}
	body := b.String()
	var out string
	if i := strings.Index(body, lastActiveHeading); i >= 0 {
		out = body[:i] + lastActiveHeading + "\n\n- " + line + "\n"
	} else {
		out = strings.TrimRight(body, "\n") + "\n\n" + lastActiveHeading + "\n\n- " + line + "\n"
	}
	return w.Save(ws, out)
}

// Save 覆盖写入工作区记忆文档（原子替换；自动补首行工作区路径注释供人类辨识）。
func (w *WorkspaceMemoryStore) Save(ws, content string) error {
	if strings.TrimSpace(ws) == "" {
		return fmt.Errorf("工作区路径为空，无法保存工作空间记忆")
	}
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// 首行路径标注：哈希文件名不可读，文档体内保留原始路径（Save 全量覆盖，
	// 每次重写标注防用户编辑时误删后失去可读性）。
	marker := "<!-- workspace: " + filepath.Clean(ws) + " -->\n"
	if strings.HasPrefix(content, "<!-- workspace:") {
		// 已带标注（Load→Save 回写）：替换为当前路径，防目录迁移后标注漂移。
		if i := strings.Index(content, "\n"); i >= 0 {
			content = content[i+1:]
		} else {
			content = ""
		}
	}
	tmp := w.Path(ws) + ".tmp"
	if err := os.WriteFile(tmp, []byte(marker+content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.Path(ws))
}
