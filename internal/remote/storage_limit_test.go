package remote

// storage_limit_test.go P-Storage 存储上限清理测试：
//   - 超限时按日期从旧到新清理（图片整天删 → 会话整天删 → 去重表压缩）；
//   - 活动会话（running）绝不强删；
//   - 未超限不清理；limit=0 关闭。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedImage 写一张指定 ModTime 与体量的图片（存储清理的日期锚点）。
func seedImage(t *testing.T, dir, name string, mod time.Time, size int) {
	t.Helper()
	p := filepath.Join(dir, "images")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(p, name)
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// TestStoragePruneImagesOldestDayFirst 超限时优先删最旧日期图片（不碰会话）。
func TestStoragePruneImagesOldestDayFirst(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, nil, "", nil, nil, nil)
	defer hub.Stop()
	hub.SetStoreLimit(1 << 20) // 1MB：两张 600KB 图共 1.2MB 超限
	now := time.Now()
	seedImage(t, dir, "old.png", now.AddDate(0, 0, -3), 600<<10)
	seedImage(t, dir, "new.png", now, 600<<10)
	hub.tasks["t-live"] = &Task{ID: "t-live", SessionID: "s-live", Status: TaskStatusRunning, CreatedAt: now.UnixMilli()}

	freed := hub.PruneStorage()
	if freed <= 0 {
		t.Fatalf("应释放空间: %d", freed)
	}
	if _, err := os.Stat(filepath.Join(dir, "images", "old.png")); !os.IsNotExist(err) {
		t.Fatal("最旧日期图片应被删除")
	}
	if _, err := os.Stat(filepath.Join(dir, "images", "new.png")); err != nil {
		t.Fatal("新日期图片不应被误删")
	}
	if _, ok := hub.GetTask("t-live"); !ok {
		t.Fatal("图片清理不应触碰任务记录")
	}
}

// TestStoragePruneSessionsByDay 图片不存在时删最旧日期的终态会话；
// running 会话（即便最旧）绝不强删；最新日期会话保留。
func TestStoragePruneSessionsByDay(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, nil, "", nil, nil, nil)
	defer hub.Stop()
	hub.SetStoreLimit(100 << 10) // 100KB：三个 ~40KB 镜像会话超限
	now := time.Now().UnixMilli()
	dayMs := int64(24 * 3600 * 1000)

	mk := func(id, sid string, status string, created int64) {
		hub.tasks[id] = &Task{ID: id, SessionID: sid, NodeID: "node-p", Text: "任务",
			Status: status, CreatedAt: created, FinishedAt: created + 100}
		msgs := make([]interface{}, 0, 40)
		for i := 0; i < 40; i++ {
			msgs = append(msgs, map[string]interface{}{"say": "text", "text": strings.Repeat("x", 1024)})
		}
		hub.SaveSessionMessages(sid, msgs)
	}
	mk("t-old-run", "remote-s-oldrun", TaskStatusRunning, now-3*dayMs) // 最旧但运行中
	mk("t-old", "remote-s-old", TaskStatusDone, now-3*dayMs+1000)
	mk("t-mid", "remote-s-mid", TaskStatusDone, now-2*dayMs)
	mk("t-new", "remote-s-new", TaskStatusDone, now-dayMs)
	// 镜像走异步持久化：先 flush 落盘（约 160KB > 100KB 上限），统计才真实。
	hub.FlushPersistence()

	freed := hub.PruneStorage()
	if freed <= 0 {
		t.Fatalf("应释放空间: %d", freed)
	}
	if _, ok := hub.GetTask("t-old-run"); !ok {
		t.Fatal("运行中会话不得被强删")
	}
	if _, ok := hub.GetTask("t-new"); !ok {
		t.Fatal("最新日期会话不应被清理（日期从旧到新）")
	}
	if _, ok := hub.GetTask("t-old"); ok {
		t.Fatal("最旧日期终态会话应被清理")
	}
	if msgs := hub.SessionMessages("remote-s-old"); len(msgs) != 0 {
		t.Fatalf("被清理会话镜像应同步删除: %d", len(msgs))
	}
	if msgs := hub.SessionMessages("remote-s-new"); len(msgs) == 0 {
		t.Fatal("保留会话镜像不应被误删")
	}
}

// TestStoragePruneCompactMsgIDs 无图片/会话可删时压缩去重表（保留最新一半）。
func TestStoragePruneCompactMsgIDs(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, nil, "", nil, nil, nil)
	defer hub.Stop()
	hub.SetStoreLimit(1 << 20) // 1MB：4096 行 × ~315B ≈ 1.29MB 超限
	var content []byte
	line := "\"" + strings.Repeat("a", 300) + "\""
	for i := 0; i < 4096; i++ {
		content = append(content, []byte(line)...)
		content = append(content, '\n')
	}
	path := hub.msgIDsFile()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	if freed := hub.PruneStorage(); freed <= 0 {
		t.Fatal("压缩去重表应释放空间")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(strings.TrimSpace(string(b)), "\n") + 1
	if lines == 0 || lines > 2048 {
		t.Fatalf("压缩后应保留约一半（≤2048）行: %d", lines)
	}
}

// TestStoragePruneDisabledAndNoOver 未超限不清理；limit=0 关闭。
func TestStoragePruneDisabledAndNoOver(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, nil, "", nil, nil, nil)
	defer hub.Stop()
	now := time.Now().UnixMilli()
	hub.tasks["t-1"] = &Task{ID: "t-1", SessionID: "s-1", Status: TaskStatusDone, CreatedAt: now}
	hub.SaveSessionMessages("s-1", []interface{}{map[string]interface{}{"say": "text", "text": "在用"}})

	hub.SetStoreLimit(1 << 30) // 1GB 远未超限
	if freed := hub.PruneStorage(); freed != 0 {
		t.Fatalf("未超限不应清理: %d", freed)
	}
	if _, ok := hub.GetTask("t-1"); !ok {
		t.Fatal("未超限不应删除任务")
	}

	hub.SetStoreLimit(0) // 关闭
	seedImage(t, dir, "old.png", time.Now().AddDate(0, 0, -30), 1024)
	if freed := hub.PruneStorage(); freed != 0 {
		t.Fatalf("limit=0 应跳过清理: %d", freed)
	}
	if _, err := os.Stat(filepath.Join(dir, "images", "old.png")); err != nil {
		t.Fatal("关闭上限时图片不应被删")
	}
}
