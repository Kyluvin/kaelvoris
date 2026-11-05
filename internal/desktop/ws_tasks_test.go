// ws_tasks_test.go —— 工作区面板「执行」链路单测（用户需求 2026-12）。
package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaelvoris/internal/session"
)

// TestWSReadReadme 一级目录 README.md 读取（存在/不存在）。
func TestWSReadReadme(t *testing.T) {
	dir := t.TempDir()
	if got := wsReadReadme(dir); got != "" {
		t.Fatalf("无 README 应返回空串, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# 项目说明\n\n约定：Go 1.25，模块 kaelvoris。"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := wsReadReadme(dir)
	if got == "" || !contains(got, "# 项目说明") {
		t.Fatalf("应读到 README 内容, got %q", got)
	}
}

// TestWSSessionWorkspace 复用会话判定：工作区匹配（大小写/分隔符归一）、
// cron/roadmap 自动会话跳过、子会话跳过。
func TestWSSessionWorkspace(t *testing.T) {
	dir := t.TempDir()
	a := &App{store: session.NewStore(filepath.Join(t.TempDir(), "sessions"))}
	now := time.UnixMilli(time.Now().UnixMilli())

	// 命中：工作区路径大小写差异归一（Windows）。
	a.store.Save(&session.Session{ID: "1001", Workspace: dir + "\\", UpdatedAt: now.Add(-time.Hour)})
	if got := a.wsSessionWorkspace(dir); got != "1001" {
		t.Fatalf("应复用会话 1001, got %q", got)
	}
	// cron 会话不劫持。
	a.store.Save(&session.Session{ID: "cron_x", Workspace: dir, UpdatedAt: now})
	if got := a.wsSessionWorkspace(dir); got != "1001" {
		t.Fatalf("cron 会话应跳过, got %q", got)
	}
	// 无匹配 → 空串（新建）。
	if got := a.wsSessionWorkspace(filepath.Join(t.TempDir(), "other")); got != "" {
		t.Fatalf("无匹配应返回空串, got %q", got)
	}
}

// TestMaybeWSProjectLog 回写链路：README 已存在时区块插入 + 滚动上限；
// 不存在时自动创建。
func TestMaybeWSProjectLog(t *testing.T) {
	dir := t.TempDir()
	a := &App{store: session.NewStore(filepath.Join(t.TempDir(), "sessions"))}

	// 1) README 不存在 → 自动创建 + 首条日志。
	wsRegisterLog("s1", dir)
	a.maybeWSProjectLog("s1", "修复登录崩溃", "定位到 nil 指针并补齐防护", nil)
	b, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	if !contains(content, wsProjectLogBegin) || !contains(content, "修复登录崩溃") {
		t.Fatalf("README 应含日志区块与任务记录:\n%s", content)
	}

	// 2) 第二条 → 区块内新条目置前。
	wsRegisterLog("s2", dir)
	a.maybeWSProjectLog("s2", "重构日志模块", "拆分 writer 接口", os.ErrPermission)
	b2, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	content2 := string(b2)
	if !contains(content2, "重构日志模块") || !contains(content2, "修复登录崩溃") {
		t.Fatalf("第二条应追加且保留第一条:\n%s", content2)
	}
	if !contains(content2, "❌") {
		t.Fatalf("失败任务应标记 ❌:\n%s", content2)
	}
		iNew := strings.Index(content2, "重构日志模块")
		iOld := strings.Index(content2, "修复登录崩溃")
	if iNew > iOld {
		t.Fatalf("新条目应在旧条目之前 (new=%d old=%d)", iNew, iOld)
	}

	// 3) 未登记会话不回写（回到未创建 README 的目录应保持无 README）。
	other := t.TempDir()
	a.maybeWSProjectLog("s3-unknown", "无关任务", "结果", nil)
	if _, err := os.Stat(filepath.Join(other, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("未登记会话不应在任何位置回写")
	}
}

// TestMaybeWSProjectLog_RollUp 日志条目超过上限滚动丢弃（最多保留 10 条）。
func TestMaybeWSProjectLog_RollUp(t *testing.T) {
	dir := t.TempDir()
	a := &App{store: session.NewStore(filepath.Join(t.TempDir(), "sessions"))}
	for i := 0; i < 13; i++ {
		tid := "roll-" + string(rune('a'+i))
		wsRegisterLog(tid, dir)
		a.maybeWSProjectLog(tid, "任务"+string(rune('A'+i)), "结果", nil)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	c := string(b)
	if countEntries(c) > wsProjectLogMaxRows {
		t.Fatalf("日志条目应滚动上限 %d, got %d:\n%s", wsProjectLogMaxRows, countEntries(c), c)
	}
	// 最早的任务已被滚动丢弃。
	if contains(c, "任务A") {
		t.Fatal("最早条目应被滚动丢弃")
	}
}

// countEntries 统计区块内条目首行数（"- " 开头行）。
func countEntries(content string) int {
	begin := strings.Index(content, wsProjectLogBegin)
	end := strings.Index(content, wsProjectLogEnd)
	if begin < 0 || end <= begin {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(content[begin+len(wsProjectLogBegin):end], "\n") {
		// 条目首行无缩进（"- ✅ …"）；从属行带缩进（"  - 任务：…"）不计数。
		if strings.HasPrefix(ln, "- ") {
			n++
		}
	}
	return n
}

// wsTestApp 搭建仅带面板目录的 App 零值（emitEvent 对 nil ctx 静默，适合绑定层单测）。
func wsTestApp(t *testing.T, dir string) *App {
	t.Helper()
	return &App{cfg: Config{WSPanelDir: dir}}
}

// TestWSFileCRUD 文件编辑链路端到端：创建→写入→读回→重命名→删除（用户需求：
// 右键新建/重命名/删除的后端链路自检；前端弹窗行为由浏览器 mock 验证）。
func TestWSFileCRUD(t *testing.T) {
	dir := t.TempDir()
	a := wsTestApp(t, dir)

	// 新建带子目录的文件（右键目录新建 → 自动建父目录）。
	if err := a.CreateWSFile(WSCreateRequest{Path: "sub/dir/a.txt"}); err != nil {
		t.Fatalf("新建文件: %v", err)
	}
	// 写入（Ctrl+S 保存链路的后端）。
	if err := a.WriteWSFile(WSSaveRequest{Path: "sub/dir/a.txt", Content: "hello 世界"}); err != nil {
		t.Fatalf("保存: %v", err)
	}
	// 读回（含 UTF-8 与中文）。
	c, err := a.ReadWSFile("sub/dir/a.txt")
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if c.Content != "hello 世界" || c.Truncated {
		t.Fatalf("读回应完整且未截断, got %q truncated=%v", c.Content, c.Truncated)
	}
	// 重命名（含移动到子目录）。
	if err := a.RenameWSFile(WSRenameRequest{From: "sub/dir/a.txt", To: "sub/dir/b.txt"}); err != nil {
		t.Fatalf("重命名: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "dir", "b.txt")); err != nil {
		t.Fatalf("重命名后文件应存在: %v", err)
	}
	// 删除（文件级）。
	if err := a.DeleteWSFile("sub/dir/b.txt"); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "dir", "b.txt")); !os.IsNotExist(err) {
		t.Fatal("删除后文件应不存在")
	}
	// 目录递归删除。
	if err := a.CreateWSFile(WSCreateRequest{Path: "sub/dir/c.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteWSFile("sub"); err != nil {
		t.Fatalf("递归删除目录: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
		t.Fatal("目录应整体删除")
	}
}

// TestWSFileReadTruncated 超 2MB 文件读取：内容截断且 truncated=true（前端据此
// 禁用「编辑」，防止把截断内容写回覆盖原文件——数据丢失防护）。
func TestWSFileReadTruncated(t *testing.T) {
	dir := t.TempDir()
	a := wsTestApp(t, dir)
	big := strings.Repeat("x", wsReadLimit+1024)
	if err := os.WriteFile(filepath.Join(dir, "big.log"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := a.ReadWSFile("big.log")
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if !c.Truncated {
		t.Fatal("超大文件应标记 truncated=true")
	}
	if len(c.Content) != wsReadLimit {
		t.Fatalf("内容应截到上限 %d, got %d", wsReadLimit, len(c.Content))
	}
	// 常规文件不误标。
	small := filepath.Join(dir, "small.txt")
	_ = os.WriteFile(small, []byte("ok"), 0o644)
	c2, _ := a.ReadWSFile("small.txt")
	if c2.Truncated {
		t.Fatal("常规文件不应标记截断")
	}
}

// TestWSPathGuard 路径校验回归：绝对路径/盘符/..逃逸/重名均拒绝。
func TestWSPathGuard(t *testing.T) {
	dir := t.TempDir()
	a := wsTestApp(t, dir)
	for _, bad := range []string{"C:/evil.txt", "/abs.txt", "../out.txt"} {
		if err := a.CreateWSFile(WSCreateRequest{Path: bad}); err == nil {
			t.Fatalf("路径 %q 应被拒绝", bad)
		}
	}
	// a/../b 实际合法（Clean 后为 b）——验证不误杀。
	if err := a.CreateWSFile(WSCreateRequest{Path: "a/../b"}); err != nil {
		t.Fatalf("a/../b 应归一为 b 合法: %v", err)
	}
	// 重名拒绝。
	if err := a.CreateWSFile(WSCreateRequest{Path: "b"}); err == nil {
		t.Fatal("已存在同名应拒绝")
	}
}
