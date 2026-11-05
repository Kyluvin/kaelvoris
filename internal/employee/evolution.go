package employee

// evolution.go 进化提案管理（§11.5：写草稿 → 审批 → 落地）。
//
// 模型侧只允许把提案写入 <workspace>/evolution-drafts/pending/<签名>.md
// （Gateway 硬校验）；本文件提供提案的解析/列表/审批/拒绝：
//   - 批准：草稿迁入 approved/，正文按 Target-File 落地到 workspace/memory/
//     （追加式沉淀，带日期 + 提案 ID 分节）；
//   - 拒绝：草稿迁入 rejected/ 留痕；
//   - 用户修改后落地：ApproveProposal 传入修订正文。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fsDirEntry 目录项别名（供 parseProposal 的 en 参数可传 nil）。
type fsDirEntry = os.DirEntry

// Proposal 一个进化提案（§11.5 元数据 + 草稿正文）。
type Proposal struct {
	FileName    string    `json:"fileName"`   // <signature>.md
	Signature   string    `json:"signature"`  // 8 位十六进制内容签名
	ProposalID  string    `json:"proposalId"` // evo-<YYYYMMDD>-<签名>
	TargetFile  string    `json:"targetFile"`
	TriggerType string    `json:"triggerType"`
	Confidence  int       `json:"confidence"`
	EmployeeID  string    `json:"employeeId"`
	Content     string    `json:"content"` // 正文（不含 YAML 头）
	CreatedAt   time.Time `json:"createdAt"`
}

// LoadMemory 读取员工工作空间记忆沉淀（memory/*.md，§11.4 路由目标），
// 拼接为可注入系统提示词的文本（空内容跳过；无记忆返回空串）。
func LoadMemory(ws string) string {
	var b strings.Builder
	for _, f := range []string{"BEHAVIOR.md", "TOOLS.md", "WORKFLOWS.md", "MEMORY.md"} {
		bts, err := os.ReadFile(joinPath(ws, "memory", f))
		if err != nil {
			continue
		}
		if s := strings.TrimSpace(string(bts)); s != "" {
			b.WriteString("\n### " + trimExt(f) + "\n\n" + s + "\n")
		}
	}
	return b.String()
}

// ListProposals 列出待审批提案（pending/*.md；损坏草稿跳过不阻断）。
func (s *Store) ListProposals(empID string) ([]Proposal, error) {
	e, ok := s.Get(empID)
	if !ok {
		return nil, fmt.Errorf("员工不存在: %s", empID)
	}
	dir := joinPath(e.Workspace(s.dir), "evolution-drafts", "pending")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Proposal{}, nil
		}
		return nil, err
	}
	out := []Proposal{}
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".md") {
			continue
		}
		if p, err := parseProposal(empID, dir, en.Name(), en); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// parseProposal 解析提案草稿（--- 包裹 YAML 头 + Markdown 正文，§11.5 格式）。
func parseProposal(empID, dir, fileName string, en fsDirEntry) (Proposal, error) {
	b, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		return Proposal{}, err
	}
	sig := strings.TrimSuffix(fileName, ".md")
	p := Proposal{
		FileName:   fileName,
		Signature:  sig,
		ProposalID: "evo-" + time.Now().Format("20060102") + "-" + sig,
		EmployeeID: empID,
	}
	if en != nil {
		if info, err := en.Info(); err == nil {
			p.CreatedAt = info.ModTime()
		}
	}
	head, body := splitFrontmatter(string(b))
	p.Content = strings.TrimSpace(body)
	for _, line := range strings.Split(head, "\n") {
		k, v, ok := cutYAML(line)
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "proposal-id":
			p.ProposalID = v
		case "signature":
			p.Signature = v
		case "target-file":
			p.TargetFile = v
		case "trigger-type":
			p.TriggerType = v
		case "confidence":
			p.Confidence = atoiSafe(v)
		}
	}
	return p, nil
}

// splitFrontmatter 拆分 YAML 头与正文（无 frontmatter 时 head = ""）。
func splitFrontmatter(text string) (head, body string) {
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, "---") {
		return "", s
	}
	rest := strings.TrimPrefix(s, "---")
	if i := strings.Index(rest, "\n---"); i >= 0 {
		return rest[:i], strings.TrimPrefix(rest[i+4:], "\n")
	}
	return "", rest
}

// cutYAML 解析单行 "key: value"。
func cutYAML(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	i := strings.Index(line, ":")
	if i <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// atoiSafe 宽松整数解析（解析失败返回 0）。
func atoiSafe(s string) int {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ApproveProposal 审批通过：草稿迁入 approved/，正文按 Target-File 追加落地到
// <workspace>/memory/<target>（§11.4 路由）。modifiedContent 非空 = 用户修改后落地。
func (s *Store) ApproveProposal(empID, fileName, modifiedContent string) error {
	e, ok := s.Get(empID)
	if !ok {
		return fmt.Errorf("员工不存在: %s", empID)
	}
	ws := e.Workspace(s.dir)
	pending := joinPath(ws, "evolution-drafts", "pending")
	b, err := os.ReadFile(filepath.Join(pending, fileName))
	if err != nil {
		return fmt.Errorf("提案草稿不存在: %s", fileName)
	}
	p, perr := parseProposal(empID, pending, fileName, nil)
	content := modifiedContent
	if strings.TrimSpace(content) == "" && perr == nil {
		content = p.Content
	}
	if strings.TrimSpace(content) == "" {
		content = string(b)
	}
	// 1) 草稿迁入 approved/（留痕）。
	approvedDir := joinPath(ws, "evolution-drafts", "approved")
	if err := os.MkdirAll(approvedDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(approvedDir, fileName), b, 0o644); err != nil {
		return err
	}
	// 2) 正文按 Target-File 追加沉淀到 memory/（§11.4；默认 MEMORY.md）。
	target := "MEMORY.md"
	if perr == nil && p.TargetFile != "" {
		target = normalizeTarget(p.TargetFile)
	}
	memDir := joinPath(ws, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return err
	}
	proposalID := p.ProposalID
	if perr == nil && proposalID == "" {
		proposalID = "evo-" + p.Signature
	}
	entry := fmt.Sprintf("\n## %s · %s\n\n%s\n",
		time.Now().Format("2006-01-02"), proposalID, strings.TrimSpace(content))
	if err := appendToFile(filepath.Join(memDir, target), entry); err != nil {
		return err
	}
	return os.Remove(filepath.Join(pending, fileName))
}

// RejectProposal 审批拒绝：草稿迁入 rejected/（留痕，可回溯）。
func (s *Store) RejectProposal(empID, fileName string) error {
	e, ok := s.Get(empID)
	if !ok {
		return fmt.Errorf("员工不存在: %s", empID)
	}
	ws := e.Workspace(s.dir)
	pending := joinPath(ws, "evolution-drafts", "pending")
	b, err := os.ReadFile(filepath.Join(pending, fileName))
	if err != nil {
		return fmt.Errorf("提案草稿不存在: %s", fileName)
	}
	rejectedDir := joinPath(ws, "evolution-drafts", "rejected")
	if err := os.MkdirAll(rejectedDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(rejectedDir, fileName), b, 0o644); err != nil {
		return err
	}
	return os.Remove(filepath.Join(pending, fileName))
}

// appendToFile 追加写入（记忆沉淀为追加式，§11.4；文件不存在则创建）。
func appendToFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

// normalizeTarget 记忆文件名规范化（Target-File → 固定白名单，防路径注入）。
func normalizeTarget(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "memory/")
	t = strings.TrimPrefix(t, `memory\`)
	switch strings.ToUpper(t) {
	case "BEHAVIOR.MD":
		return "BEHAVIOR.md"
	case "TOOLS.MD":
		return "TOOLS.md"
	case "WORKFLOWS.MD":
		return "WORKFLOWS.md"
	default:
		return "MEMORY.md"
	}
}
