package desktop

// 回归测试：effectiveNodeIdentity 的 mutex 双重解锁（2026-10-01 打包秒退事故）。
// 事故版本在该函数内同时使用 defer Unlock 与显式 Unlock，return 触发
// "fatal error: sync: unlock of unlocked mutex" 直接杀死进程——且仅当
// ~/.kaelvoris/remote.json 存在（远程连接器启动路径）才会命中，隔离 HOME
// 的冒烟测试测不出来。本测试以零值 App 调用一次：修复前此测试会 fatal，
// 修复后应正常返回降级身份。

import "testing"

func TestEffectiveNodeIdentityNoDoubleUnlock(t *testing.T) {
	a := &App{}
	id, name, slot := a.effectiveNodeIdentity()
	if id != "" || name != "" || slot != 0 {
		t.Fatalf("零值 App 应返回空身份/槽位0，got id=%q name=%q slot=%d", id, name, slot)
	}
	// 二次调用：确认锁状态未被破坏（此前双解锁会让后续调用行为未定义）。
	id2, _, slot2 := a.effectiveNodeIdentity()
	if id2 != id || slot2 != slot {
		t.Fatalf("两次调用结果应一致，got (%q,%d) vs (%q,%d)", id2, slot2, id, slot)
	}
}

// TestRnOwnTaskFilter 执行端归属过滤（2026-11 用户需求）：
// PC 端仅渲染本机执行（广播 nodeId == 本机身份）或本机曾发起（会话映射命中）的任务；
// 他机执行/他端发布的任务须被过滤（移动端/管理端不受影响，仍是广播全量）。
func TestRnOwnTaskFilter(t *testing.T) {
	a := &App{}
	// 1) 无本机身份 + 无会话映射：他机任务被过滤。
	if a.rnOwnTask("remote-s-other", "node-other") {
		t.Fatal("他机任务不应放行")
	}
	// 2) 会话映射命中（本机曾发起/曾参与）：放行。
	a.ensureRn().sessTask = map[string]string{"remote-s-mine": "t-1"}
	if !a.rnOwnTask("remote-s-mine", "") {
		t.Fatal("本机发起会话应放行（映射命中）")
	}
	// 3) 广播 nodeId == 本机身份：放行（本机执行）。
	a.remoteCfg = RemoteConfig{NodeID: "node-self"}
	if !a.rnOwnTask("remote-s-exec", "node-self") {
		t.Fatal("本机执行任务应放行（nodeId 匹配）")
	}
	// 4) 广播 nodeId 为他人：过滤。
	if a.rnOwnTask("remote-s-other2", "node-other") {
		t.Fatal("他机执行任务应过滤")
	}
	// 5) 空会话：过滤。
	if a.rnOwnTask("", "node-self") {
		t.Fatal("空会话应过滤")
	}
}
