package desktop

// skill_ai_categorize.go —— 技能库面板「AI 分类」能力（2026-12 用户需求）。
//
// 需求：技能面板增加 AI 分类，AI 读取技能后自动生成分类名称进行划分；
// 自动生成分类（新增分类）时通过开关设置是否允许。
//
// 实现（对齐 task_summary / roadmap 规划的后台增强模式）：
//   - AICategorizeSkills 面板按钮入口：读全量技能（name+description）→ 一次
//     非流式 LLM 调用产出 JSON 协议 {groups:[{name,skills}]} → 按开关决定
//     是否自动新增缺失分类 → 逐技能落 SetSkillCategory 分配；
//   - 开关持久化 skills-settings.json（SkillSettings.AIAutoCreate，app.go）：
//     开（默认）= AI 可自动创建新分类；关 = 仅归入既有分类，无法归类的保持未分类；
//   - 全程后台 goroutine + 单飞互斥（aiCatRunning）：结果经 kaelvoris:skills 事件刷新面板，进度/失败经 kaelvoris:toast 提示。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/llm"
	"kaelvoris/internal/skills"
)

// aiCat 单飞互斥（同一时刻仅一个 AI 分类任务在跑；进程内存态，重启自然清零）。
var aiCatMu sync.Mutex
var aiCatRunning bool

// aiCatGroupsPrompt AI 分类提示词（JSON 输出协议；与 taskSummary/roadmapReview
// 同款「仅输出一个 JSON 对象」协议，便于容错解析）。
func aiCatGroupsPrompt(items []skills.Metadata, existing []string) string {
	var b strings.Builder
	b.WriteString(`你是技能库分类器。下面是全部已安装技能的清单（名称 + 一句话描述），
请按技能的用途与主题给它们划分分类。

规则：
- 分类名用简短中文（2~6 字，如：前端开发、文档写作、数据处理、DevOps）；
- 每个技能必须归入且仅归入一个分类；
- 用途相近的技能归入同一分类，分类数量按需收敛（通常 3~8 个，不要每个技能一类）；
`)
	if len(existing) > 0 {
		b.WriteString("- 优先复用下方已有分类（名称完全一致时视为同一分类，无需新建）；\n")
		b.WriteString("- 既有分类均不适用时才新建分类。\n")
		b.WriteString("---- 已有分类 ----\n")
		for _, n := range existing {
			b.WriteString("- " + n + "\n")
		}
	}
	b.WriteString(`
输出协议：仅输出一个 JSON 对象，不要任何解释、注释或代码围栏：
	{"groups":[{"name":"分类名","skills":["技能名","技能名"]}]}
	
---- 技能清单 ----
`)
	for _, s := range items {
		desc := s.Description
		if r := []rune(desc); len(r) > 120 {
			desc = string(r[:120])
		}
		fmt.Fprintf(&b, "- %s：%s\n", s.Name, desc)
	}
	return b.String()
}

// aiCatGroupOutput 模型输出协议（单组）。
// 新建分类是否放行由用户开关统一决定，不依赖模型输出——避免模型对
// 「是否新建」的判定与用户意图不一致。
type aiCatGroupOutput struct {
	Name       string   `json:"name"`
	SkillNames []string `json:"skills"`
}

// aiCatGroupsOutput 模型输出协议（整体）。
type aiCatGroupsOutput struct {
	Groups []aiCatGroupOutput `json:"groups"`
}

// parseAICatGroups 解析模型输出（容错：剥代码围栏/解释文本，取首个 { 到末个 }）。
func parseAICatGroups(s string) aiCatGroupsOutput {
	var out aiCatGroupsOutput
	s = strings.TrimSpace(s)
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return out
	}
	_ = json.Unmarshal([]byte(s[i:j+1]), &out)
	return out
}

// GetSkillAICategorizeEnabled 读取 AI 自动新增分类开关（前端设置项回显）。
func (a *App) GetSkillAICategorizeEnabled() bool {
	return loadSkillSettings().AIAutoCreate
}

// SetSkillAICategorizeEnabled 设置 AI 自动新增分类开关（持久化 skills-settings.json）。
func (a *App) SetSkillAICategorizeEnabled(enabled bool) error {
	s := loadSkillSettings()
	s.AIAutoCreate = enabled
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(skillSettingsPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(skillSettingsPath(), b, 0o600); err != nil {
		return err
	}
	return nil
}

// aiCatResult AI 分类执行结果（toast 汇报文案素材）。
type aiCatResult struct {
	Created int // 新增分类数
	Applied int // 已分配技能数
	Kept    int // 关开关时无法归入既有分类、保持未分类的技能数
}

// AICategorizeSkills 面板「AI 分类」按钮入口：后台执行，受理即返回。
// 结果经 kaelvoris:skills 刷新 + kaelvoris:toast 汇报；并发调用直接拒绝（单飞）。
func (a *App) AICategorizeSkills() error {
	aiCatMu.Lock()
	if aiCatRunning {
		aiCatMu.Unlock()
		return fmt.Errorf("AI 分类正在进行中，请稍候")
	}
	aiCatRunning = true
	aiCatMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("AI 分类 panic（已拦截）", "panic", r)
			}
			aiCatMu.Lock()
			aiCatRunning = false
			aiCatMu.Unlock()
		}()
		res, err := a.aiCategorizeRun()
		if err != nil {
			a.pushToast("🤖 AI 分类失败：" + truncateRunes(err.Error(), 120))
			slog.Warn("AI 分类失败", "err", err)
			return
		}
		msg := fmt.Sprintf("🤖 AI 分类完成：划分 %d 组，分配 %d 个技能", res.createdGroups(), res.Applied)
		if res.Kept > 0 {
			msg += fmt.Sprintf("；%d 个技能暂留未分类（自动新增分类已关闭）", res.Kept)
		}
		a.pushToast(msg)
	}()
	return nil
}

// createdGroups 新增分类数（冗余字段聚合，模板里少写一段 if）。
func (r aiCatResult) createdGroups() int { return r.Created }

// aiCategorizeRun 执行一轮 AI 分类：清单 → LLM 分组 → 按开关落分类与分配。
func (a *App) aiCategorizeRun() (*aiCatResult, error) {
	cfg := a.GetConfig()
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("请先在设置中填写 API Key")
	}
	mgr := a.currentSkillMgr()
	all := mgr.All()
	if len(all) == 0 {
		return nil, fmt.Errorf("没有可分类的技能")
	}
	// 按名稳定排序（提示词顺序稳定，便于复现与测试）。
	sortSkillsByName(all)

	res := &aiCatResult{}
	cats, _ := mgr.LoadCategories()
	existing := make([]string, 0, len(cats))
	for _, c := range cats {
		existing = append(existing, c.Name)
	}

	// 一次非流式 LLM 调用产出分组方案。
	client := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	resp, err := client.Generate(ctx, llm.ChatRequest{Messages: []llm.Message{{
		Role:    llm.RoleUser,
		Content: aiCatGroupsPrompt(all, existing),
	}}})
	if err != nil {
		return nil, fmt.Errorf("分类模型调用失败: %w", err)
	}
	out := parseAICatGroups(resp.Content)
	if len(out.Groups) == 0 {
		return nil, fmt.Errorf("AI 未产出有效分类方案（模型输出无法解析）")
	}

	autoCreate := loadSkillSettings().AIAutoCreate
	// catIDByName 分类名 → ID（含运行中新建的分类；技能名 → 分类名映射由模型输出直接给出）。
	catIDByName := map[string]string{}
	for _, c := range cats {
		catIDByName[c.Name] = c.ID
	}
	for _, g := range out.Groups {
		name := strings.TrimSpace(g.Name)
		if name == "" || len(g.SkillNames) == 0 {
			continue
		}
		if catIDByName[name] == "" {
			// 新分类：开关决定是否允许自动创建（用户需求核心分支）。
			// 开 = 自动 AddCategory；关 = 本组技能保持未分类（计入 Kept）。
			if !autoCreate {
				res.Kept += len(g.SkillNames)
				continue
			}
			nc, err := mgr.AddCategory(name)
			if err != nil {
				// 同名竞态等失败：按已存在继续（映射仍可能为空则本组技能落未分类）。
				slog.Warn("AI 分类新建分类失败（跳过该组）", "name", name, "err", err)
				continue
			}
			catIDByName[name] = nc.ID
			res.Created++
		}
		for _, sn := range g.SkillNames {
			sn = strings.TrimSpace(sn)
			if sn == "" || catIDByName[name] == "" {
				continue
			}
			if err := mgr.SetSkillCategory(sn, catIDByName[name]); err != nil {
				continue // 未知技能名（模型幻觉/拼写偏差）：跳过不致命
			}
			res.Applied++
		}
	}
	if res.Applied == 0 && res.Created == 0 {
		return nil, fmt.Errorf("AI 分类方案未能落任何分类（请重试或检查模型输出）")
	}
	a.emitSkillsChanged()
	return res, nil
}

// sortSkillsByName 技能按名升序（AI 分类清单稳定顺序）。
func sortSkillsByName(all []skills.Metadata) {
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].Name < all[j-1].Name; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
}
