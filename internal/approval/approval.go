// Package approval 工具审批矩阵 + 参数级规则（C2 对标 OpenCode permissions，
// https://github.com/sst/opencode packages/core/src/v1/config/permission.ts
// Action = ["ask","allow","deny"] 三态；D2 第三批升级参数级规则，对标
// Gemini CLI policy engine：toolName + argsPattern + decision + priority 四要素）。
//
// 判定优先级（从高到低）：
//  1. 红线（危险命令 / 敏感文件写入）：引擎更早分支判定，任何规则不可覆盖。
//  2. Rules 参数级规则：tool 精确匹配 + pattern（参数 JSON 原文正则，空 = 不限
//     参数）命中且 priority 最高（同分取数组靠后）者生效。
//  3. Matrix 工具级矩阵（C2 兼容）：工具名 → 动作。
//  4. 全局 AutoApprove：全部未命中时走旧逻辑（零破坏）。
//
// 语义：
//   - deny → 引擎直接拒绝该工具调用（回填 denied by approval rules）。
//   - allow → 直通（即使 AutoApprove 未开启）。
//   - ask → 强制弹审批卡（即使 AutoApprove 已开启）。
//
// 持久化：~/.kaelvoris/approvals.json，文件双形态兼容：
//   - 旧形态：顶层对象即矩阵（工具名 → 动作字符串）；
//   - 新形态：{"rules":[Rule...], "<tool>":"<action>"}（矩阵键与 rules 并存）。
//
// 写出始终新形态；旧版二进制读新形态时反序列化失败 → 按「损坏 = 空矩阵」
// 约定安全降级（跳过不致命）。宿主经 SetApproval/SetApprovalRule 写入。
package approval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Action 审批动作三态（对标 OpenCode Permission Action）。
type Action string

const (
	ActionAsk   Action = "ask"   // 强制弹审批卡
	ActionAllow Action = "allow" // 直通免审
	ActionDeny  Action = "deny"  // 直接拒绝
)

// Matrix 工具名 → 动作。JSON 顶层对象；空矩阵（nil 或空 map）= 全部旧逻辑。
type Matrix map[string]Action

// Rule 参数级审批规则（D2 对标 Gemini CLI policy engine 四要素）。
type Rule struct {
	ID       string `json:"id,omitempty"`       // 随机 hex（SetApprovalRule 定位更新/删除）
	Tool     string `json:"tool"`               // 工具名（精确匹配；MCP 工具名本身唯一，不做通配）
	Pattern  string `json:"pattern,omitempty"`  // 参数 JSON 原文正则（空 = 不限参数）
	Action   Action `json:"action"`             // ask | allow | deny
	Priority int    `json:"priority,omitempty"` // 缺省 0；同分取 rules 数组靠后者
}

// Store 升级后的审批存储：旧工具级矩阵 + 新参数级规则（内存态；nil = 全部旧逻辑）。
type Store struct {
	Matrix Matrix `json:"-"`     // 旧形态：工具名 → 动作（顶层键直读直写）
	Rules  []Rule `json:"rules"` // 新形态：参数级规则数组
}

// Valid 判断动作字符串是否合法（ask/allow/deny）。
func Valid(s string) bool {
	switch Action(strings.ToLower(strings.TrimSpace(s))) {
	case ActionAsk, ActionAllow, ActionDeny:
		return true
	}
	return false
}

// Decide 判定工具是否放行（C2 矩阵语义，保留供旧绑定/单测使用）。返回 (allow, forcedAsk)：
//   - 未命中 → (autoApprove, false)：完全走旧逻辑（弹卡与否由 AutoApprove 决定）。
//   - deny  → (false, false)：引擎拒绝。
//   - allow → (true, false)：直通。
//   - ask   → (false, true)：强制弹卡（即使 autoApprove=true 也弹，由引擎据此绕过自动批准）。
func (m Matrix) Decide(tool string, autoApprove bool) (allow bool, forcedAsk bool) {
	if len(m) == 0 {
		return autoApprove, false
	}
	act, ok := m[tool]
	if !ok {
		return autoApprove, false
	}
	switch act {
	case ActionDeny:
		return false, false
	case ActionAllow:
		return true, false
	case ActionAsk:
		// ask 语义：必须弹卡。autoApprove=false 时旧逻辑本来就弹卡，
		// 两者等价；autoApprove=true 时 forcedAsk 用于绕过自动批准。
		return false, true
	}
	return autoApprove, false
}

// DecideEx 全量判定（D2 引擎审批分支唯一入口）：参数级规则优先 → 工具级矩阵。
// 返回 (allow, forcedAsk, decided)；decided=false = 全部未命中 → 引擎走 AutoApprove。
//   - rules 命中：tool 相等 且 pattern 为空 或 regexp 匹配 argsJSON（参数 JSON 原文，
//     与 Gemini CLI "converted to a stable JSON string, tested against regex" 一致，
//     不 re-marshal 保持模型提交键序）；多条命中取 priority 最高，同分取数组靠后。
//   - 非法正则条目视为未命中（LoadStore 已在装载期丢弃，此处兜底）。
func (s *Store) DecideEx(tool, argsJSON string, autoApprove bool) (allow, forcedAsk, decided bool) {
	if s == nil {
		return false, false, false
	}
	var best *Rule
	for i := range s.Rules {
		r := &s.Rules[i]
		if r.Tool != tool {
			continue
		}
		if r.Pattern != "" {
			re, err := regexp.Compile(r.Pattern)
			if err != nil || !re.MatchString(argsJSON) {
				continue
			}
		}
		if best == nil || r.Priority >= best.Priority {
			best = r
		}
	}
	if best != nil {
		switch best.Action {
		case ActionDeny:
			return false, false, true
		case ActionAllow:
			return true, false, true
		case ActionAsk:
			return false, true, true
		}
		return false, false, false // 非法 action 条目视为未命中
	}
	if act, ok := s.Matrix[tool]; ok {
		switch act {
		case ActionDeny:
			return false, false, true
		case ActionAllow:
			return true, false, true
		case ActionAsk:
			return false, true, true
		}
	}
	return false, false, false
}

// readRaw 读文件顶层对象原始形态（新旧通用探测）。不存在/损坏 → ok=false。
func readRaw(path string) (map[string]json.RawMessage, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, false
	}
	return raw, true
}

// Load 读矩阵文件（C2 旧 API，保留给旧绑定）：忽略 rules 键，仅返回工具级矩阵。
// 不存在/损坏/空 → nil（调用方按旧逻辑处理）。
func Load(path string) Matrix {
	raw, ok := readRaw(path)
	if !ok {
		return nil
	}
	m := Matrix{}
	for k, v := range raw {
		if k == "rules" {
			continue
		}
		var a Action
		if json.Unmarshal(v, &a) == nil && a != "" {
			m[k] = a
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// LoadStore 读全量审批存储（D2 新 API）：探测顶层 "rules" 键分新旧形态；
// 非法正则条目丢弃（跳过不致命约定）；不存在/损坏/全空 → nil。
func LoadStore(path string) *Store {
	raw, ok := readRaw(path)
	if !ok {
		return nil
	}
	s := &Store{}
	if r, ok := raw["rules"]; ok {
		_ = json.Unmarshal(r, &s.Rules)
		kept := make([]Rule, 0, len(s.Rules))
		for _, r := range s.Rules {
			if strings.TrimSpace(r.Tool) == "" || !Valid(string(r.Action)) {
				continue
			}
			if r.Pattern != "" {
				if _, err := regexp.Compile(r.Pattern); err != nil {
					continue // 非法正则：丢弃该条
				}
			}
			kept = append(kept, r)
		}
		s.Rules = kept
	}
	for k, v := range raw {
		if k == "rules" {
			continue
		}
		var a Action
		if json.Unmarshal(v, &a) == nil && a != "" {
			if s.Matrix == nil {
				s.Matrix = Matrix{}
			}
			s.Matrix[k] = a
		}
	}
	if len(s.Rules) == 0 && len(s.Matrix) == 0 {
		return nil
	}
	return s
}

// writeNewForm 以新形态原子写出：{"rules":[...], "<tool>":"<action>"}。
func writeNewForm(path string, rules []Rule, m Matrix) error {
	out := make(map[string]json.RawMessage, len(m)+1)
	if len(rules) > 0 {
		b, err := json.Marshal(rules)
		if err != nil {
			return err
		}
		out["rules"] = b
	}
	for k, v := range m {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out[k] = b
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Save 持久化矩阵（C2 旧 API）：写前合并磁盘最新内容（防多进程/多窗口覆盖丢失），
// 规则键原样保留（旧绑定写矩阵不丢参数级规则）；空动作删除该键；矩阵清空且无
// 规则 → 删除文件。
func Save(path string, m Matrix) error {
	raw, _ := readRaw(path)
	merged := Matrix{}
	for k, v := range raw {
		if k == "rules" {
			continue
		}
		var a Action
		if json.Unmarshal(v, &a) == nil && a != "" {
			merged[k] = a
		}
	}
	for k, v := range m {
		if v == "" {
			delete(merged, k)
		} else {
			merged[k] = v
		}
	}
	var rules []Rule
	if r, ok := raw["rules"]; ok {
		_ = json.Unmarshal(r, &rules)
	}
	if len(merged) == 0 && len(rules) == 0 {
		// 全清空：删掉文件，让 Load 回到 nil（旧逻辑）。
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeNewForm(path, rules, merged)
}

// SaveStore 持久化全量审批存储（D2 新 API）：先读盘最新合并（防多进程覆盖），
// rules 按 ID 合并（已有 ID 替换、新 ID 追加），matrix 合并语义同 Save；
// 空动作规则丢弃；全部清空 → 删除文件。写出始终新形态。
func SaveStore(path string, s *Store) error {
	raw, _ := readRaw(path)
	merged := &Store{}
	if r, ok := raw["rules"]; ok {
		_ = json.Unmarshal(r, &merged.Rules)
	}
	for k, v := range raw {
		if k == "rules" {
			continue
		}
		var a Action
		if json.Unmarshal(v, &a) == nil && a != "" {
			if merged.Matrix == nil {
				merged.Matrix = Matrix{}
			}
			merged.Matrix[k] = a
		}
	}
	if s != nil {
		for _, r := range s.Rules {
			// Action=="" 且 ID!=""：删除该 ID 规则（矩阵空串删除语义对齐）。
			if strings.TrimSpace(string(r.Action)) == "" && r.ID != "" {
				for i := range merged.Rules {
					if merged.Rules[i].ID == r.ID {
						merged.Rules = append(merged.Rules[:i], merged.Rules[i+1:]...)
						break
					}
				}
				continue
			}
			replaced := false
			for i := range merged.Rules {
				if r.ID != "" && merged.Rules[i].ID == r.ID {
					merged.Rules[i] = r
					replaced = true
					break
				}
			}
			if !replaced {
				merged.Rules = append(merged.Rules, r)
			}
		}
		for k, v := range s.Matrix {
			if merged.Matrix == nil {
				merged.Matrix = Matrix{}
			}
			if v == "" {
				delete(merged.Matrix, k)
			} else {
				merged.Matrix[k] = v
			}
		}
	}
	// 丢弃空动作且无 ID 的规则（无效条目兜底；带 ID 空动作已在上方作删除消费）。
	kept := merged.Rules[:0]
	for _, r := range merged.Rules {
		if strings.TrimSpace(string(r.Action)) != "" || r.ID == "" {
			kept = append(kept, r)
		}
	}
	merged.Rules = kept
	if len(merged.Rules) == 0 && len(merged.Matrix) == 0 {
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeNewForm(path, merged.Rules, merged.Matrix)
}

// String 规范化动作字符串（供宿主绑定校验入参）。
func String(s string) (Action, error) {
	a := Action(strings.ToLower(strings.TrimSpace(s)))
	switch a {
	case ActionAsk, ActionAllow, ActionDeny:
		return a, nil
	}
	return "", fmt.Errorf("approval: invalid action %q (want ask|allow|deny)", s)
}
