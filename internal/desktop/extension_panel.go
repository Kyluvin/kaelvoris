package desktop

// extension_panel.go —— 扩展商店面板（Wails 绑定）。
//
// 商店语义：
//   - 内置目录预置精选扩展（当前： tt-a1i/archify 交互式架构图技能）
//   - 安装按钮 → 克隆 GitHub 仓库 → 定位 SKILL.md → 部署到
//     ~/.kaelvoris/skills/<name>/（global 技能目录，skills.Manager 自动发现，
//     下一任务注入 available_skills，Agent 使用时自动调用）
//   - ＋ 增加按钮 → AI 读取 GitHub 仓库（README/SKILL.md/package.json）
//     自动生成扩展元数据（名称/描述/技能子目录/分类）入库商店

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"kaelvoris/internal/extension"
	"kaelvoris/internal/llm"
)

// builtinExtensions 预置精选扩展（首次启动自动入库）。
func builtinExtensions() []*extension.Ext {
	return []*extension.Ext{
		{
			ID:          "tt-a1i/archify",
			Repo:        "tt-a1i/archify",
			Name:        "Archify 交互式架构图",
			Description: "生成可交互验证的架构/流程/时序/数据流/状态图 HTML（SVG 内联、深浅主题、路径探索、导出）。说「用 Archify 画出…」即可生成独立 HTML 图表。",
			Author:      "tt-a1i",
			Version:     "2.17",
			SkillDir:    "archify",
			Category:    "可视化",
			Source:      "builtin",
		},
	}
}

// ListExtensions 商店扩展列表（内置预置 + AI/手动添加），按入库时间倒序。
// 安装状态实时回读（技能目录存在 = 已安装，容忍用户手动删除）。
func (a *App) ListExtensions() []*extension.Ext {
	list, err := extension.Load()
	if err != nil {
		return []*extension.Ext{}
	}
	// 首次启动播种内置扩展。
	if len(list) == 0 {
		now := time.Now().UnixMilli()
		for _, b := range builtinExtensions() {
			b.AddedAt = now
			if extension.FindInstalled(b.Skills) {
				b.Installed = true
			}
			if err := extension.Save(b); err == nil {
				list = append(list, b)
			}
		}
	}
	for _, x := range list {
		x.Installed = extension.FindInstalled(x.Skills)
	}
	extension.SortByAdded(list)
	if list == nil {
		list = []*extension.Ext{}
	}
	return list
}

// InstallExtensionRequest 安装入参。
type InstallExtensionRequest struct {
	Repo string `json:"repo"` // "owner/repo"
}

// InstallExtension 安装扩展（商店「安装」按钮）：
//  1. 元数据不存在则先用最简记录（AI 信息可在后续 enrich）
//  2. git clone --depth 1 → 部署 SKILL.md 技能目录到 ~/.kaelvoris/skills/
//  3. 广播刷新（技能列表 / 商店列表）
func (a *App) InstallExtension(req InstallExtensionRequest) (*extension.Ext, error) {
	id := strings.TrimSpace(req.Repo)
	if id == "" || !strings.Contains(id, "/") {
		return nil, errors.New("仓库标识无效（应为 owner/repo）")
	}
	x, err := extension.Get(id)
	if err != nil {
		// 未入库的仓库直接装：ID 即仓库，名称取 repo 尾段。
		x = &extension.Ext{
			ID:      id,
			Repo:    id,
			Name:    id[strings.LastIndex(id, "/")+1:],
			Source:  "manual",
			AddedAt: time.Now().UnixMilli(),
		}
	}
	if err := extension.Install(x); err != nil {
		return nil, err
	}
	a.emitExtensionRefresh()
	return x, nil
}

// UninstallExtension 卸载扩展（移除技能目录与元数据）。
func (a *App) UninstallExtension(repo string) error {
	id := strings.TrimSpace(repo)
	if err := extension.Uninstall(id); err != nil {
		return err
	}
	a.emitExtensionRefresh()
	return nil
}

// DeleteExtension 从商店移除记录（保留已部署技能目录）。
func (a *App) DeleteExtension(repo string) error {
	id := strings.TrimSpace(repo)
	if err := extension.Delete(id); err != nil {
		return err
	}
	a.emitExtensionRefresh()
	return nil
}

// emitExtensionRefresh 广播商店与技能列表刷新。
func (a *App) emitExtensionRefresh() {
	uiEmit(a.ctx, "kaelvoris:extensions", a.ListExtensions())
	uiEmit(a.ctx, "kaelvoris:skills", a.ListSkills())
}

// AddExtensionRequest AI 增加扩展入参。
type AddExtensionRequest struct {
	Repo string `json:"repo"` // GitHub 仓库 "owner/repo" 或完整 URL
}

// AddExtensionByAI ＋ 增加按钮：AI 自动读取 GitHub 仓库并生成扩展元数据入库。
// 流程：抓取 README.md / SKILL.md / package.json（GitHub raw，小体积截断）→
// LLM 提炼 name/description/skillDir/category/version → 入库（不自动安装）。
func (a *App) AddExtensionByAI(req AddExtensionRequest) (*extension.Ext, error) {
	repo := normalizeRepo(req.Repo)
	if repo == "" {
		return nil, errors.New("请输入 GitHub 仓库（owner/repo 或完整 URL）")
	}
	id := repo
	if existing, err := extension.Get(id); err == nil {
		return nil, fmt.Errorf("扩展已存在: %s（%s）", existing.Name, existing.ID)
	}
	cfg := a.GetConfig()
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("AI 读取仓库需先在设置中配置 API Key")
	}

	// 1) 抓取仓库关键文件（都失败则报错）。
	files := map[string]string{}
	for _, f := range []string{"README.md", "SKILL.md", "package.json"} {
		if b, err := fetchGitHubFile(repo, f); err == nil && len(b) > 0 {
			files[f] = string(truncateBytes(b, 24<<10))
		}
	}
	if len(files) == 0 {
		return nil, errors.New("读取仓库失败：请确认仓库存在且公开（owner/repo）")
	}

	// 2) LLM 提炼扩展元数据（严格 JSON 输出）。
	home, _ := os.UserHomeDir()
	ws := a.cfgWorkspace()
	client := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	prompt := buildExtPrompt(repo, files)
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "你是扩展商店收录助手。只输出一个 JSON 对象，不要 markdown 代码块。"},
		{Role: llm.RoleUser, Content: prompt},
	}
	msg, err := client.Generate(context.Background(), llm.ChatRequest{Messages: msgs, MaxTokens: intPtr(1200)})
	if err != nil {
		return nil, fmt.Errorf("AI 分析仓库失败: %w", err)
	}
	ext, perr := parseExtJSON(msg.Content, repo)
	if perr != nil {
		return nil, fmt.Errorf("AI 输出解析失败: %w", perr)
	}
	ext.ID = repo
	ext.Repo = repo
	ext.Source = "ai"
	ext.AddedAt = time.Now().UnixMilli()
	if err := extension.Save(ext); err != nil {
		return nil, err
	}
	_ = home
	_ = ws
	a.emitExtensionRefresh()
	return ext, nil
}

// buildExtPrompt 构造仓库分析提示词（要求严格 JSON 输出）。
func buildExtPrompt(repo string, files map[string]string) string {
	var sb strings.Builder
	sb.WriteString("分析以下 GitHub 仓库 " + repo + " 的文件内容，判断它是否是一个「Agent 技能扩展包」（含 SKILL.md 或等价物）。\n")
	sb.WriteString("输出严格 JSON（无其他文字）：\n")
	sb.WriteString(`{"name":"中文显示名","description":"80字内中文描述（它给Agent带来什么能力、怎么触发使用）","author":"作者","version":"版本号或空","skillDir":"仓库内技能子目录路径（SKILL.md 在仓库根则留空字符串；不存在 SKILL.md 填最可能的一级子目录名）","category":"分类（可视化/开发/文档/测试/数据/其他）"}` + "\n\n")
	for name, content := range files {
		sb.WriteString("===== " + name + " =====\n")
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// parseExtJSON 从 LLM 输出中提取 JSON 对象并映射为 Ext。
func parseExtJSON(text, repo string) (*extension.Ext, error) {
	s := strings.TrimSpace(text)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var raw struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Author      string `json:"author"`
		Version     string `json:"version"`
		SkillDir    string `json:"skillDir"`
		Category    string `json:"category"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		name = repo[strings.LastIndex(repo, "/")+1:]
	}
	desc := strings.TrimSpace(raw.Description)
	if desc == "" {
		return nil, errors.New("AI 未生成描述")
	}
	x := &extension.Ext{
		Name:        name,
		Description: desc,
		Author:      strings.TrimSpace(raw.Author),
		Version:     strings.TrimSpace(raw.Version),
		Category:    strings.TrimSpace(raw.Category),
	}
	if d := strings.Trim(strings.TrimSpace(raw.SkillDir), "/"); d != "" && d != "." {
		x.SkillDir = d
	}
	if x.Author == "" {
		x.Author = strings.SplitN(repo, "/", 2)[0]
	}
	return x, nil
}

// fetchGitHubFile 抓取仓库默认分支文件（raw.githubusercontent.com）。
func fetchGitHubFile(repo, file string) ([]byte, error) {
	url := "https://raw.githubusercontent.com/" + repo + "/main/" + file
	resp, err := http.Get(url) // #nosec G107 -- repo 经规范校验（owner/repo 两段）
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// main 失败回退 master。
		if resp.StatusCode == http.StatusNotFound {
			return fetchGitHubFileFrom(repo, "master", file)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func fetchGitHubFileFrom(repo, branch, file string) ([]byte, error) {
	url := "https://raw.githubusercontent.com/" + repo + "/" + branch + "/" + file
	resp, err := http.Get(url) // #nosec G107
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// normalizeRepo 统一仓库输入为 "owner/repo"（支持完整 URL / .git 后缀）。
func normalizeRepo(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

// truncateBytes 按 UTF-8 安全截断（rune 边界）。
func truncateBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	for n > 0 && (b[n]&0xC0) == 0x80 { // 回退到字符边界
		n--
	}
	return b[:n]
}

func intPtr(v int) *int { return &v }

// runGitListRemote 预检仓库存在性（可选优化，保留给后续 ls-remote 校验）。
var _ = func(repo string) error {
	cmd := exec.Command("git", "ls-remote", "--heads", "https://github.com/"+repo+".git")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}
