package evolve

import (
	"strings"
	"testing"
	"time"
)

func TestStoreAddListDelete(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Card{Title: "", Body: "x"}); err == nil {
		t.Fatal("空标题应报错")
	}
	c1, err := s.Add(Card{Title: "先读后写", Body: "改文件前必须 fs_read 最新内容", Keywords: []string{"fs_write", "写文件"}})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if c1.ID == "" {
		t.Fatal("应生成 ID")
	}
	// 同标题合并（去重），保留命中数。
	if _, err := s.Add(Card{Title: "先读后写", Body: "更新后的正文", Keywords: []string{"diff"}}); err != nil {
		t.Fatalf("Add merge: %v", err)
	}
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("同标题应合并为 1 张，得到 %d", len(list))
	}
	if list[0].Body != "更新后的正文" {
		t.Fatalf("正文应更新: %q", list[0].Body)
	}
	// 关键词合并去重。
	found := map[string]bool{}
	for _, k := range list[0].Keywords {
		found[k] = true
	}
	if !found["fs_write"] || !found["diff"] {
		t.Fatalf("关键词应合并: %v", list[0].Keywords)
	}
	if err := s.Delete(c1.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("删除后应为空")
	}
}

func TestMatchAndSnippet(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	_, _ = s.Add(Card{Title: "Windows 路径", Body: "PowerShell 下用 Select-String 代替 grep", Keywords: []string{"windows", "powershell"}})
	_, _ = s.Add(Card{Title: "全局守则", Body: "改动前先跑 go build ./..."}) // 无关键词 = 常驻卡

	// 命中关键词 + 常驻卡。
	hits := s.Match("修复 windows 下的构建脚本")
	if len(hits) != 2 {
		t.Fatalf("应命中 2 张（关键词+常驻），得到 %d", len(hits))
	}
	// 仅常驻卡。
	hits = s.Match("重构登录模块")
	if len(hits) != 1 || hits[0].Title != "全局守则" {
		t.Fatalf("仅常驻卡应命中: %+v", hits)
	}
	// 计数累加。
	if s.List()[0].Hits == 0 {
		t.Fatal("命中应累加计数")
	}
	// ContextSnippet 片段。
	snippet := s.ContextSnippet("重构登录模块")
	if !strings.Contains(snippet, "<self_evolution>") || !strings.Contains(snippet, "全局守则") {
		t.Fatalf("片段应包含命中卡: %q", snippet)
	}
	if snippet != "" && strings.Contains(snippet, "Windows 路径") {
		t.Fatal("未命中关键词的卡不应注入")
	}
	// 无命中（无常驻卡时）。
	s2, _ := NewStore(t.TempDir())
	_, _ = s2.Add(Card{Title: "特例", Body: "x", Keywords: []string{"mysql"}})
	if got := s2.ContextSnippet("前端样式调整"); got != "" {
		t.Fatalf("无命中应返回空，得到 %q", got)
	}
}

// TestReloadMergeCrossProcess 模拟双进程共用 cards.json：先启动的进程不得用陈旧
// 快照覆盖掉其他进程新增/删除的卡（跷跷板回滚根因回归测试）。
func TestReloadMergeCrossProcess(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(dir) // "进程1"

	if _, err := s1.Add(Card{Title: "A卡", Body: "a"}); err != nil {
		t.Fatal(err)
	}
	s2, _ := NewStore(dir) // "进程2"（A 落库后启动，快照含 A）

	// 进程1 新增 B；进程2 随后新增 C —— 修复前 s2 会用 {A,C} 覆盖丢掉 B。
	if _, err := s1.Add(Card{Title: "B卡", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Add(Card{Title: "C卡", Body: "c"}); err != nil {
		t.Fatal(err)
	}
	s3, _ := NewStore(dir)
	titles := map[string]bool{}
	for _, c := range s3.List() {
		titles[c.Title] = true
	}
	if !titles["A卡"] || !titles["B卡"] || !titles["C卡"] {
		t.Fatalf("跨进程新增应全保留 A/B/C，得到 %v", titles)
	}

	// 跨进程删除：s3 删除 A 后，s1（内存仍有 A）新增 D 不得复活 A。
	for _, c := range s3.List() {
		if c.Title == "A卡" {
			if err := s3.Delete(c.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s1.Add(Card{Title: "D卡", Body: "d"}); err != nil {
		t.Fatal(err)
	}
	s4, _ := NewStore(dir)
	titles = map[string]bool{}
	for _, c := range s4.List() {
		titles[c.Title] = true
	}
	if titles["A卡"] {
		t.Fatal("他人已删的卡不得被陈旧快照复活")
	}
	if !titles["B卡"] || !titles["C卡"] || !titles["D卡"] {
		t.Fatalf("B/C/D 应在位，得到 %v", titles)
	}
}

func TestEvictAndLimits(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	for i := 0; i < maxCards+8; i++ {
		if _, err := s.Add(Card{Title: "卡" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + string(rune('0'+i%10)), Body: "b"}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	// 出生保护（evictGrace）会让宽限期内的新卡免于淘汰，此处把全部卡拨回
	// 宽限期外并落盘（下次 Add 的 reloadMerge 以磁盘为准，不落盘会被撤销），
	// 恢复「老卡可被淘汰」的原语义后再验上限收敛。
	for i := range s.cards {
		s.cards[i].UpdatedAt = time.Now().Add(-72 * time.Hour)
	}
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Card{Title: "触发卡", Body: "t"}); err != nil {
		t.Fatal(err)
	}
	if got := len(s.List()); got > maxCards {
		t.Fatalf("卡数应淘汰到 %d 以内，得到 %d", maxCards, got)
	}
	// 正文超长截断。
	c, _ := s.Add(Card{Title: "长文", Body: strings.Repeat("长", 1000)})
	if len([]rune(c.Body)) > maxBodyRunes {
		t.Fatalf("正文应截断到 %d rune", maxBodyRunes)
	}
}

// TestEvictGraceProtectsNewCards 出生保护回归（跷跷板二次丢卡形态，2026-10-10）：
// 恢复重放的卡 Hits=0、LastHitAt 零值，满库时任何 Add 都把它判为最差挤出——
// 实测并发 add 风暴三波 8 进 8 出、5 张恢复卡全灭。宽限期内的卡不得被挤。
// TestIsTransientState G2 临时状态防线：状态型断言命中、方法型静默通过。
func TestIsTransientState(t *testing.T) {
	// 命中：中英文状态型断言（含大小写不敏感）。
	for _, body := range []string{
		"功能没有实现，先跳过",
		"The repo has no game code yet",
		"尚未实现导入功能",
		"Import is Not Implemented in this module",
		"缺少测试覆盖", // 「缺失」命中
	} {
		if !IsTransientState(body) {
			t.Errorf("expected transient-state hit: %q", body)
		}
	}
	// 方法型（可执行步骤 + 判据）：不命中。
	for _, body := range []string{
		"改文件前先 fs_read 最新内容，SEARCH 未命中时按行号重读",
		"发布前跑 _smoke_test.psm1 冒烟，失败自动回滚",
		"PowerShell 下用 Select-String 代替 grep",
	} {
		if IsTransientState(body) {
			t.Errorf("method-type card should pass silently: %q", body)
		}
	}
}

func TestEvictGraceProtectsNewCards(t *testing.T) {
	old := evictGrace
	defer func() { evictGrace = old }() // 测试后还原，勿污染同包其他用例

	s, _ := NewStore(t.TempDir())
	// 造满库：老卡 UpdatedAt 拨回宽限期外（可淘汰），恢复卡（Hits=0）受保护。
	for i := 0; i < maxCards; i++ {
		if _, err := s.Add(Card{Title: "老" + string(rune('a'+i%26)) + string(rune('0'+i/26)), Body: "b"}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	for i := range s.cards {
		s.cards[i].UpdatedAt = time.Now().Add(-72 * time.Hour)
		s.cards[i].Hits = 5
		s.cards[i].LastHitAt = time.Now().Add(-time.Hour)
	}
	if err := s.saveLocked(); err != nil { // 白盒拨回后落盘，防 reloadMerge 用磁盘新时间撤销
		t.Fatal(err)
	}
	// 模拟恢复卡重放：Hits=0、LastHitAt 零值、UpdatedAt 现在（宽限期内）。
	if _, err := s.Add(Card{Title: "恢复卡", Body: "replay"}); err != nil {
		t.Fatal(err)
	}
	// 再压一张老库卡触发淘汰：被挤的应是宽限期外的老卡，而非恢复卡；
	// 总数回落到上限 64（65→淘汰 1 张老卡）。
	if _, err := s.Add(Card{Title: "压库卡", Body: "p"}); err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	oldCount := 0
	for _, c := range s.List() {
		titles[c.Title] = true
		if strings.HasPrefix(c.Title, "老") {
			oldCount++
		}
	}
	if !titles["恢复卡"] {
		t.Fatal("宽限期内恢复卡（Hits=0）不得被淘汰")
	}
	if len(s.List()) != maxCards {
		t.Fatalf("总数应回落到 %d，得到 %d", maxCards, len(s.List()))
	}
	if oldCount != maxCards-2 {
		t.Fatalf("被挤出的应是老卡（63 张老卡+恢复卡+压库卡），实际老卡 %d", oldCount)
	}

	// 宽限期归零复现旧语义：恢复卡重新参与竞争被挤（防保护被永久滥用）。
	evictGrace = 0
	if _, err := s.Add(Card{Title: "再压卡", Body: "q"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.List() {
		if c.Title == "恢复卡" {
			t.Fatal("宽限期归零后恢复卡（Hits=0）应恢复参与淘汰被挤出")
		}
	}
}
