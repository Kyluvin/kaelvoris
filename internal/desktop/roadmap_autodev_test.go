package desktop

// roadmap_autodev_test.go 完全自主模式补货与配置归一单测（用户需求 2026-10）：
//  1. SetAutoDevConfig 归一化：minDeveloping 钳位 / maxPlanItems 硬顶 /
//     完全自主开启时强制 autoReview；
//  2. intervalOf 间隔归一（0 = 默认 2h；低于补货重试间隔抬到重试间隔）。

import "testing"

func TestIntervalOf(t *testing.T) {
	if got := intervalOf(0); got != autoDevDefaultIntervalMin*60_000_000_000 {
		t.Fatalf("0 应回落默认 2h: %v", got)
	}
	if got := intervalOf(120); got != 120*60_000_000_000 {
		t.Fatalf("120min 应原样: %v", got)
	}
	if got := intervalOf(1); got != autoDevRetryInterval {
		t.Fatalf("低于补货重试间隔应抬到重试间隔: %v", got)
	}
}

func TestAutoDevMinDevelopingDefault(t *testing.T) {
	a := &App{}
	if got := a.autoDevMinDeveloping(); got != 1 {
		t.Fatalf("未配置时最低并行数应为 1: %d", got)
	}
}
