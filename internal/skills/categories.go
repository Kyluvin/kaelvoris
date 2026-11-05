// 技能分类：技能库面板的展示性分组（不影响 Agent available_skills 注入语义）。
//
// 设计：
//   - 持久化 ~/.kaelvoris/skill-categories.json（JSON 文件；损坏按空处理不致命，
//     对齐 AGENTS.md「文件损坏按跳过不致命处理」约定）；
//   - 分类以稳定 ID（cat_<UnixMilli>）标识，重命名不影响技能绑定；
//   - 「未分类」为内置虚拟区（不落盘）：无分配记录或指向已删除分类的技能
//     一律归入未分类，对齐需求「已有未分类的默认的其他区」；
//   - 分类为全局用户偏好（home 层），跨工作区共享。
package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// constCategoriesFile 分类持久化文件名（位于 ~/.kaelvoris/ 下，与 skills-settings.json 同层）。
const constCategoriesFile = "skill-categories.json"

// Category 技能分类（ID 稳定标识；Sort 决定面板导航顺序）。
type Category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Sort int    `json:"sort"`
}

// categorization 分类 + 技能分配持久化结构（单文件整体读写）。
type categorization struct {
	Categories  []Category        `json:"categories"`
	Assignments map[string]string `json:"assignments"` // skillName -> categoryID（空缺 = 未分类）
}

// catMu 保护分类文件读写（Manager 实例可被多宿主并发持有：技能面板/远程同步）。
var catMu sync.Mutex

// CategoriesPath 分类 JSON 文件路径。
func (m *Manager) CategoriesPath() string {
	return filepath.Join(m.home, ".kaelvoris", constCategoriesFile)
}

// LoadCategories 读取分类定义与技能分配（文件不存在/损坏返回空值，不致命）。
func (m *Manager) LoadCategories() ([]Category, map[string]string) {
	catMu.Lock()
	defer catMu.Unlock()
	return m.loadCategoriesLocked()
}

// loadCategoriesLocked 读文件（调用方需持 catMu）。
func (m *Manager) loadCategoriesLocked() ([]Category, map[string]string) {
	b, err := os.ReadFile(m.CategoriesPath())
	if err != nil {
		return []Category{}, map[string]string{}
	}
	var c categorization
	if err := json.Unmarshal(b, &c); err != nil {
		return []Category{}, map[string]string{}
	}
	if c.Categories == nil {
		c.Categories = []Category{}
	}
	if c.Assignments == nil {
		c.Assignments = map[string]string{}
	}
	return c.Categories, c.Assignments
}

// SaveCategories 整体写入（分类管理批量保存入口：重排序/重命名后的整体写入）。
// assignments 传 nil 时保留现有分配不动（重排序场景不清空技能归属）。
func (m *Manager) SaveCategories(cats []Category, assignments map[string]string) error {
	catMu.Lock()
	defer catMu.Unlock()
	if assignments == nil {
		_, assignments = m.loadCategoriesLocked()
	}
	return m.saveCategoriesLocked(&categorization{Categories: cats, Assignments: assignments})
}

// saveCategoriesLocked 写文件（调用方需持 catMu）。
func (m *Manager) saveCategoriesLocked(c *categorization) error {
	if err := os.MkdirAll(filepath.Dir(m.CategoriesPath()), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.CategoriesPath(), b, 0o600)
}

// SetSkillCategory 设置技能所属分类（catID 空 = 移入未分类）。
// 校验：技能必须已发现；catID 非空时分类必须存在（防前端脏数据）。
func (m *Manager) SetSkillCategory(skill, catID string) error {
	catMu.Lock()
	defer catMu.Unlock()
	cats, as := m.loadCategoriesLocked()
	m.mu.RLock()
	_, ok := m.skills[skill]
	m.mu.RUnlock()
	if !ok {
		m.Discover()
		m.mu.RLock()
		_, ok = m.skills[skill]
		m.mu.RUnlock()
	}
	if !ok {
		return fmt.Errorf("skill %q not found", skill)
	}
	if catID != "" && !hasCategory(cats, catID) {
		return fmt.Errorf("category %q not found", catID)
	}
	if catID == "" {
		delete(as, skill)
	} else {
		as[skill] = catID
	}
	return m.saveCategoriesLocked(&categorization{Categories: cats, Assignments: as})
}

// AddCategory 新增分类（名称非空、按名去重；Sort 追加到末尾）。
func (m *Manager) AddCategory(name string) (*Category, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("分类名不能为空")
	}
	catMu.Lock()
	defer catMu.Unlock()
	cats, as := m.loadCategoriesLocked()
	for _, c := range cats {
		if c.Name == name {
			return nil, fmt.Errorf("分类已存在: %s", name)
		}
	}
	maxSort := 0
	for _, c := range cats {
		if c.Sort > maxSort {
			maxSort = c.Sort
		}
	}
	nc := Category{ID: fmt.Sprintf("cat_%d", time.Now().UnixMilli()), Name: name, Sort: maxSort + 1}
	cats = append(cats, nc)
	if err := m.saveCategoriesLocked(&categorization{Categories: cats, Assignments: as}); err != nil {
		return nil, err
	}
	return &nc, nil
}

// UpdateCategory 重命名分类（ID 不变，技能绑定不受影响）。
func (m *Manager) UpdateCategory(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("分类名不能为空")
	}
	catMu.Lock()
	defer catMu.Unlock()
	cats, as := m.loadCategoriesLocked()
	for i := range cats {
		if cats[i].ID != id {
			continue
		}
		for j, c := range cats {
			if j != i && c.Name == name {
				return nil // 同名幂等：直接返回成功
			}
		}
		cats[i].Name = name
		return m.saveCategoriesLocked(&categorization{Categories: cats, Assignments: as})
	}
	return fmt.Errorf("category %q not found", id)
}

// DeleteCategory 删除分类；归属技能自动落入未分类（清除对应 assignment）。
func (m *Manager) DeleteCategory(id string) error {
	catMu.Lock()
	defer catMu.Unlock()
	cats, as := m.loadCategoriesLocked()
	out := make([]Category, 0, len(cats))
	found := false
	for _, c := range cats {
		if c.ID == id {
			found = true
			continue
		}
		out = append(out, c)
	}
	if !found {
		return fmt.Errorf("category %q not found", id)
	}
	for skill, cid := range as {
		if cid == id {
			delete(as, skill)
		}
	}
	return m.saveCategoriesLocked(&categorization{Categories: out, Assignments: as})
}

// hasCategory 判断分类 ID 是否存在。
func hasCategory(cats []Category, id string) bool {
	for _, c := range cats {
		if c.ID == id {
			return true
		}
	}
	return false
}

// HasCategoryID 判断分类 ID 是否存在（宿主层投影用：skill_panel.go 校验
// 指向已删除分类的分配记录，命中失败归入未分类）。
func HasCategoryID(cats []Category, id string) bool {
	return hasCategory(cats, id)
}
