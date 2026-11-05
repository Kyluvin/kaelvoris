package desktop

// 技能库面板（PC 端左侧入口「技能库」）：
//   - SkillRaw 读取技能 SKILL.md 原始全文（frontmatter+body，编辑器回显）；
//   - SaveSkill 保存/新建（统一落 project 层 <workspace>/.kaelvoris/skills/<name>/SKILL.md）；
//   - DeleteSkill 删除（builtin 禁删）；
//   - 分类管理（ListSkillCategories/GetSkillAssignments/SetSkillCategory/
//     SaveSkillCategories/CategoryAdd/CategoryRename/CategoryDelete，
//     持久化 ~/.kaelvoris/skill-categories.json）。
// 保存/删除/改分类成功后 EventsEmit("kaelvoris:skills", 列表) 刷新前端；
// 若远程连接在线，同步 client.SendSkillsList() 上报服务端（移动端技能库随之刷新）。

import (
	"os"
	"sort"

	"kaelvoris/internal/skills"
)

// currentSkillMgr 取当前技能管理器（kmu 保护；内核未建时懒建兜底，
// 保证设置页/技能库在首个任务前也可读写技能）。
func (a *App) currentSkillMgr() *skills.Manager {
	a.kmu.Lock()
	m := a.skillMgr
	a.kmu.Unlock()
	if m != nil {
		return m
	}
	home, _ := os.UserHomeDir()
	return skills.NewManager(home, a.cfg.Workspace)
}

// SkillRaw 读取技能 SKILL.md 原始全文（编辑器回显；builtin 可读只读）。
func (a *App) SkillRaw(name string) (string, error) {
	return a.currentSkillMgr().LoadRaw(name)
}

// SaveSkill 保存/新建技能（project 层；校验名规范与 frontmatter 一致性）。
// 成功后广播 kaelvoris:skills 刷新 PC 前端，并上报服务端（远程技能库同步）。
func (a *App) SaveSkill(name, content string) error {
	if err := a.currentSkillMgr().Save(name, content); err != nil {
		return err
	}
	a.emitSkillsChanged()
	return nil
}

// DeleteSkill 删除技能（builtin 禁删；同名多层只删 Path 所在层目录）。
// 成功后广播 kaelvoris:skills 刷新 PC 前端，并上报服务端。
func (a *App) DeleteSkill(name string) error {
	if err := a.currentSkillMgr().Delete(name); err != nil {
		return err
	}
	a.emitSkillsChanged()
	return nil
}

// SkillCategoryView 分类导航条目（三级横向导航第一级）。
// 未分类为虚拟区（ID 固定 ""，不落盘）：无分配记录或指向已删除分类的技能自动归入。
// Count 为该分类下技能数（前端导航徽标直接可用，后端一次算好避免前端二次映射）。
type SkillCategoryView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Sort  int    `json:"sort"`
	Count int    `json:"count"`
}

// ListSkillCategories 分类导航（含未分类虚拟区；按 Sort 升序，未分类固定在末尾）。
// 返回分类定义 + 每区技能数；前端据此渲染横向一级导航与数量徽标。
func (a *App) ListSkillCategories() []SkillCategoryView {
	mgr := a.currentSkillMgr()
	cats, as := mgr.LoadCategories()
	count := map[string]int{}
	for _, s := range mgr.All() {
		cid := as[s.Name]
		if cid != "" && !skills.HasCategoryID(cats, cid) {
			cid = "" // 指向已删除分类 → 未分类
		}
		count[cid]++
	}
	out := make([]SkillCategoryView, 0, len(cats)+1)
	for _, c := range cats {
		out = append(out, SkillCategoryView{ID: c.ID, Name: c.Name, Sort: c.Sort, Count: count[c.ID]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sort < out[j].Sort })
	out = append(out, SkillCategoryView{ID: "", Name: "未分类", Sort: 1 << 30, Count: count[""]})
	return out
}

// GetSkillAssignments 技能→分类归属明细（skillName -> catID；空缺 = 未分类）。
// 前端卡片「所属分类」角标与 L1 分类过滤均依赖此映射（含已删除分类的
// 指向一并清空，口径与 ListSkillCategories 一致）。
func (a *App) GetSkillAssignments() map[string]string {
	mgr := a.currentSkillMgr()
	cats, as := mgr.LoadCategories()
	out := map[string]string{}
	for name, cid := range as {
		if cid == "" {
			continue
		}
		if !skills.HasCategoryID(cats, cid) {
			continue // 指向已删除分类 → 未分类
		}
		out[name] = cid
	}
	return out
}

// SetSkillCategory 设置技能所属分类（catID 空 = 移入未分类）。
func (a *App) SetSkillCategory(skill, catID string) error {
	if err := a.currentSkillMgr().SetSkillCategory(skill, catID); err != nil {
		return err
	}
	a.emitSkillsChanged()
	return nil
}

// SaveSkillCategories 批量保存分类定义（含重排序/重命名/删除后的整体写入）。
// 被删除分类下的技能分配由前端先调 CategoryDelete 或后端回落未分类兜底。
func (a *App) SaveSkillCategories(cats []skills.Category) error {
	if err := a.currentSkillMgr().SaveCategories(cats, nil); err != nil {
		return err
	}
	a.emitSkillsChanged()
	return nil
}

// CategoryAdd 新增分类（名称唯一；Sort 追加末尾）。
func (a *App) CategoryAdd(name string) (*skills.Category, error) {
	c, err := a.currentSkillMgr().AddCategory(name)
	if err != nil {
		return nil, err
	}
	a.emitSkillsChanged()
	return c, nil
}

// CategoryRename 重命名分类（ID 稳定，技能绑定不受影响）。
func (a *App) CategoryRename(id, name string) error {
	if err := a.currentSkillMgr().UpdateCategory(id, name); err != nil {
		return err
	}
	a.emitSkillsChanged()
	return nil
}

// CategoryDelete 删除分类；归属技能自动落入未分类（后端清 assignment）。
func (a *App) CategoryDelete(id string) error {
	if err := a.currentSkillMgr().DeleteCategory(id); err != nil {
		return err
	}
	a.emitSkillsChanged()
	return nil
}

// emitSkillsChanged 技能变更后广播列表（PC 前端刷新）+ 上报服务端（若在线）。
// 列表经 ListSkills 重新发现（Save/Delete 内部已 Discover，此处双保险）。
func (a *App) emitSkillsChanged() {
	list := a.ListSkills()
	if a.ctx != nil {
		uiEmit(a.ctx, "kaelvoris:skills", list)
	}
	a.remoteSendSkillsList()
}

// remoteSendSkillsList 远程连接在线时主动上报技能列表（nil 安全）。
func (a *App) remoteSendSkillsList() {
	client := a.remoteClientSnapshot()
	if client != nil && client.Online() {
		client.SendSkillsList()
	}
}
