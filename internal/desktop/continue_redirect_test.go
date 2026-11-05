package desktop

import (
	"path/filepath"
	"testing"
	"time"

	"kaelvoris/internal/session"
)

// resolveContinueTargetTestApp 构造带临时目录会话库的 App（复用 sessionsPath 逻辑，
// 直接手动 new store 避免依赖 App 启动链路）。
func resolveContinueTargetTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	a := &App{store: session.NewStore(filepath.Join(dir, "sessions"))}
	return a
}

// TestResolveContinueTarget_NotResume 非「继续」输入返回空串（新建会话回落）。
func TestResolveContinueTarget_NotResume(t *testing.T) {
	a := resolveContinueTargetTestApp(t)
	// 库里放一个旧会话，确保若有候选也只能是误命中
	a.store.Save(&session.Session{ID: "1001", UpdatedAt: timeNow()})
	if got := a.resolveContinueTarget("做一个登录页"); got != "" {
		t.Fatalf("非继续输入应回落新建，got %q", got)
	}
}

// TestResolveContinueTarget_PicksMostRecent 「继续」空会话 → 最近既有会话（已完结同样重开续聊）。
func TestResolveContinueTarget_PicksMostRecent(t *testing.T) {
	a := resolveContinueTargetTestApp(t)
	a.store.Save(&session.Session{ID: "1001", UpdatedAt: timeNow().Add(-2 * time.Hour)})
	a.store.Save(&session.Session{ID: "1002", UpdatedAt: timeNow()})
	if got := a.resolveContinueTarget("继续"); got != "1002" {
		t.Fatalf("应重定向到最近会话 1002，got %q", got)
	}
}

// TestResolveContinueTarget_SkipRoadmap 规划板自动迭代会话（roadmap_ 前缀）不劫持「继续」。
func TestResolveContinueTarget_SkipRoadmap(t *testing.T) {
	a := resolveContinueTargetTestApp(t)
	a.store.Save(&session.Session{ID: roadmapDevelopSessionIDPrefix + "rm_x", UpdatedAt: timeNow()})
	a.store.Save(&session.Session{ID: "2001", UpdatedAt: timeNow().Add(-time.Hour)})
	if got := a.resolveContinueTarget("继续"); got != "2001" {
		t.Fatalf("应跳过 roadmap 会话选 2001，got %q", got)
	}
	// 只有 roadmap 会话时回落新建（独立 App，避免上面 2001 干扰）
	b := resolveContinueTargetTestApp(t)
	b.store.Save(&session.Session{ID: roadmapDevelopSessionIDPrefix + "rm_y", UpdatedAt: timeNow()})
	if got := b.resolveContinueTarget("继续"); got != "" {
		t.Fatalf("仅 roadmap 会话应回落新建，got %q", got)
	}
}

// TestResolveContinueTarget_SkipChildAndRemote 远程节点会话不作为本机续接目标；子会话优先父会话。
func TestResolveContinueTarget_SkipChildAndRemote(t *testing.T) {
	a := resolveContinueTargetTestApp(t)
	a.store.Save(&session.Session{ID: "1001-s1", ParentID: "1001", UpdatedAt: timeNow()})
	a.store.Save(&session.Session{ID: "3001", NodeID: "node-a", UpdatedAt: timeNow().Add(-time.Minute)})
	a.store.Save(&session.Session{ID: "1001", UpdatedAt: timeNow().Add(-2 * time.Minute)})
	if got := a.resolveContinueTarget("继续"); got != "1001" {
		t.Fatalf("应选顶层本机会话 1001，got %q", got)
	}
}

// TestSendMessageOpts_RedirectHit 端到端：空会话「继续」走重定向（返回选中会话 ID）。
func TestSendMessageOpts_RedirectHit(t *testing.T) {
	a := resolveContinueTargetTestApp(t)
	a.store.Save(&session.Session{ID: "1001", UpdatedAt: timeNow()})
	sid, err := a.sendMessageOpts("", "继续", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sid != "1001" {
		t.Fatalf("应重定向到 1001，got %q", sid)
	}
	// 等待 runTask goroutine 收尾，避免测试竞态泄漏
	deadline := time.Now().Add(3 * time.Second)
	for a.IsSessionRunning(sid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSendMessageOpts_RedirectFallback 无任何会话时「继续」仍新建会话。
func TestSendMessageOpts_RedirectFallback(t *testing.T) {
	a := resolveContinueTargetTestApp(t)
	sid, err := a.sendMessageOpts("", "继续", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sid == "" {
		t.Fatal("应回落新建会话")
	}
	deadline := time.Now().Add(3 * time.Second)
	for a.IsSessionRunning(sid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// timeNow 测试辅助：当前时间（截断到毫秒，模拟会话 UpdatedAt 语义）。
func timeNow() time.Time {
	return time.UnixMilli(time.Now().UnixMilli())
}
