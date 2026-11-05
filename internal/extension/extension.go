// Package extension 扩展商店：GitHub 仓库形式的技能扩展包。
//
// 扩展包结构（对齐 agentskills.io / npx skills add 语义）：
//
//	<repo>/
//	  SKILL.md            ← 可选：仓库根即技能包（如 tt-a1i/archify/archify 子目录形式也支持）
//	  <sub>/SKILL.md      ← 可选：多技能仓库
//
// 扩展元数据持久化于 ~/.kaelvoris/extensions/<id>.json；技能部署到
// ~/.kaelvoris/skills/<skillName>/（global 级，自动被 skills.Manager 发现）。
package extension

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kaelvoris/internal/sysproc"
)

// Ext 一个商店扩展（对齐一个 GitHub 仓库）。
type Ext struct {
	ID          string   `json:"id"`                    // 仓库标识 "owner/repo"
	Repo        string   `json:"repo"`                  // GitHub 仓库 tt-a1i/archify
	Name        string   `json:"name"`                  // 显示名（AI/用户填写）
	Description string   `json:"description"`           // 一句话描述
	Author      string   `json:"author,omitempty"`      // 作者
	Version     string   `json:"version,omitempty"`     // 版本（仓库 tag/package.json）
	Skills      []string `json:"skills,omitempty"`      // 包含的技能名（安装后部署的技能目录）
	SkillDir    string   `json:"skillDir,omitempty"`    // 仓库内技能子目录（空 = 仓库根；archify 为 "archify"）
	Category    string   `json:"category,omitempty"`    // 分类（可视化/开发/文档…）
	Source      string   `json:"source"`                // "builtin" 预置 | "ai" AI 添加 | "manual" 手动
	Installed   bool     `json:"installed"`             // 已安装
	InstalledAt int64    `json:"installedAt,omitempty"` // 安装时间 ms
	AddedAt     int64    `json:"addedAt"`               // 入库时间 ms
}

// storeDir 扩展元数据目录。
func storeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kaelvoris", "extensions"), nil
}

// installRoot 技能安装根目录（global：~/.kaelvoris/skills，被 skills.Manager 自动扫描）。
func installRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kaelvoris", "skills"), nil
}

// cloneDir 扩展仓库克隆位置 ~/.kaelvoris/extensions/repos/<owner>_<repo>。
func cloneDir(id string) (string, error) {
	base, err := storeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "repos", strings.ReplaceAll(id, "/", "_")), nil
}

// Load 读取全部扩展元数据。
func Load() ([]*Ext, error) {
	dir, err := storeDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Ext
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var x Ext
		if err := json.Unmarshal(b, &x); err != nil || x.ID == "" {
			continue
		}
		out = append(out, &x)
	}
	return out, nil
}

// Save 写入单个扩展元数据。
func Save(x *Ext) error {
	dir, err := storeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(x, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, strings.ReplaceAll(x.ID, "/", "_")+".json"), b, 0o644)
}

// Delete 删除扩展元数据（不卸载技能）。
func Delete(id string) error {
	dir, err := storeDir()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, strings.ReplaceAll(id, "/", "_")+".json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Get 按 ID 查找扩展。
func Get(id string) (*Ext, error) {
	list, err := Load()
	if err != nil {
		return nil, err
	}
	for _, x := range list {
		if x.ID == id {
			return x, nil
		}
	}
	return nil, fmt.Errorf("扩展不存在: %s", id)
}

// FindInstalled 检测技能目录中已部署的技能（安装状态回读：目录存在即视为已装）。
func FindInstalled(skillNames []string) bool {
	root, err := installRoot()
	if err != nil {
		return false
	}
	for _, s := range skillNames {
		if _, err := os.Stat(filepath.Join(root, s, "SKILL.md")); err == nil {
			return true
		}
	}
	return false
}

// Install 安装扩展：浅克隆仓库 → 定位技能目录（根或子目录）→ 拷贝到
// ~/.kaelvoris/skills/<skillName>/。skillName 取 SKILL.md frontmatter name
// 或目录名（skills.loadMetadata 同规则）。幂等：重复安装覆盖更新。
func Install(x *Ext) error {
	if strings.TrimSpace(x.Repo) == "" || !strings.Contains(x.Repo, "/") {
		return errors.New("仓库标识无效（应为 owner/repo）")
	}
	dst, err := cloneDir(x.ID)
	if err != nil {
		return err
	}
	// 已有克隆则拉取更新，否则浅克隆。
	if _, err := os.Stat(filepath.Join(dst, ".git")); err == nil {
		cmd := exec.Command("git", "-C", dst, "pull", "--ff-only")
		sysproc.Hide(cmd) // GUI 进程下隐藏控制台黑框。
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("更新仓库失败: %w: %s", err, strings.TrimSpace(string(out)))
		}
	} else {
		_ = os.RemoveAll(dst)
		url := "https://github.com/" + x.Repo + ".git"
		cmd := exec.Command("git", "clone", "--depth", "1", url, dst)
		sysproc.Hide(cmd) // GUI 进程下隐藏控制台黑框。
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("克隆仓库失败: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	// 定位技能目录：优先 x.SkillDir，其次仓库根，最后扫一级子目录找 SKILL.md。
	root, err := installRoot()
	if err != nil {
		return err
	}
	candidates := []string{dst}
	if d := strings.TrimSpace(x.SkillDir); d != "" {
		candidates = []string{filepath.Join(dst, d), dst}
	}
	var skillDirs []string
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "SKILL.md")); err == nil {
			skillDirs = append(skillDirs, c)
			break
		}
	}
	if len(skillDirs) == 0 {
		// 多技能仓库：全部含 SKILL.md 的一级子目录。
		entries, err := os.ReadDir(dst)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				if _, err := os.Stat(filepath.Join(dst, e.Name(), "SKILL.md")); err == nil {
					skillDirs = append(skillDirs, filepath.Join(dst, e.Name()))
				}
			}
		}
	}
	if len(skillDirs) == 0 {
		return errors.New("仓库中未找到 SKILL.md（非技能扩展仓库）")
	}
	var deployed []string
	for _, sd := range skillDirs {
		name := skillNameOf(sd)
		if name == "" {
			continue
		}
		target := filepath.Join(root, name)
		_ = os.RemoveAll(target)
		if err := copyDir(sd, target); err != nil {
			return fmt.Errorf("部署技能 %s 失败: %w", name, err)
		}
		deployed = append(deployed, name)
	}
	if len(deployed) == 0 {
		return errors.New("未能确定技能名（SKILL.md 缺少合法 name）")
	}
	x.Skills = deployed
	x.Installed = true
	x.InstalledAt = time.Now().UnixMilli()
	return Save(x)
}

// Uninstall 卸载扩展：移除部署的技能目录 + 删除克隆 + 清理元数据。
func Uninstall(id string) error {
	x, err := Get(id)
	if err != nil {
		return err
	}
	root, err := installRoot()
	if err != nil {
		return err
	}
	for _, s := range x.Skills {
		_ = os.RemoveAll(filepath.Join(root, s))
	}
	if cd, err := cloneDir(id); err == nil {
		_ = os.RemoveAll(cd)
	}
	return Delete(id)
}

// skillNameOf 从技能目录读取技能名（frontmatter name 或目录名）。
func skillNameOf(skillDir string) string {
	b, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return ""
	}
	if name := frontmatterName(b); name != "" {
		return name
	}
	return filepath.Base(skillDir)
}

// frontmatterName 解析 SKILL.md frontmatter 中的 name 字段（--- 包裹的 YAML 简版）。
func frontmatterName(b []byte) string {
	s := string(b)
	if !strings.HasPrefix(s, "---") {
		return ""
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return ""
	}
	for _, line := range strings.Split(s[3:3+end], "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "name:")), "\"'")
		}
	}
	return ""
}

// copyDir 递归拷贝目录（跳过 .git 与 node_modules）。
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if name == ".gitignore" || name == ".gitattributes" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
}

// SortByAdded 按入库时间倒序（前端展示顺序）。
func SortByAdded(list []*Ext) {
	sort.Slice(list, func(i, j int) bool { return list[i].AddedAt > list[j].AddedAt })
}
