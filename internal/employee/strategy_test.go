package employee

// strategy_test.go 分配策略注册表单测：least_loaded 选取/提前收敛/Load nil、
// 未知名兜底 random、round_robin 负游标取模。

import "testing"

func fixturePool(n int) []Employee {
	pool := make([]Employee, n)
	for i := range pool {
		pool[i] = Employee{ID: string(rune('A' + i)), PositionID: "pos_x", Status: StatusEnabled}
	}
	return pool
}

// TestLeastLoadedStrategy 选运行数最少者；并列取池中靠前者。
func TestLeastLoadedStrategy(t *testing.T) {
	pool := fixturePool(3)
	loads := map[string]int{"A": 2, "B": 0, "C": 1}
	strat := strategyFor(AssignLeastLoaded)
	if got := strat(pool, assignCtx{Load: func(id string) int { return loads[id] }}); got != 1 {
		t.Fatalf("least_loaded 应选下标 1（B，运行数 0），got %d", got)
	}
	// 并列最小（A/B 均 0）：取靠前者 A（下标 0）。
	loads["B"] = 0
	loads["A"] = 0
	if got := strat(pool, assignCtx{Load: func(id string) int { return loads[id] }}); got != 0 {
		t.Fatalf("并列最小应取池首，got %d", got)
	}
}

// TestLeastLoadedNilLoad Load 缺省 = 全 0 → 恒选首位（不 panic）。
func TestLeastLoadedNilLoad(t *testing.T) {
	pool := fixturePool(2)
	if got := strategyFor(AssignLeastLoaded)(pool, assignCtx{}); got != 0 {
		t.Fatalf("Load nil 应选首位，got %d", got)
	}
}

// TestStrategyFallback 未知规则名/空串 → random 兜底（返回合法下标）。
func TestStrategyFallback(t *testing.T) {
	pool := fixturePool(2)
	for _, rule := range []string{"", "unknown_rule", "  RANDOM "} {
		got := strategyFor(rule)(pool, assignCtx{})
		if got < 0 || got >= len(pool) {
			t.Fatalf("rule=%q 下标越界: %d", rule, got)
		}
	}
}

// TestRoundRobinNegativeCursor 负游标（配置手改）取模自适应。
func TestRoundRobinNegativeCursor(t *testing.T) {
	pool := fixturePool(3)
	if got := strategyFor(AssignRoundRobin)(pool, assignCtx{LastIndex: -1}); got != 2 {
		t.Fatalf("LastIndex=-1 池长 3 应取下标 2，got %d", got)
	}
}

// TestResolveLeastLoaded 端到端：岗位池经 least_loaded 选人 + 游标不推进。
func TestResolveLeastLoaded(t *testing.T) {
	emps := []Employee{
		{ID: "e1", PositionID: "p", Status: StatusEnabled},
		{ID: "e2", PositionID: "p", Status: StatusEnabled},
	}
	loads := map[string]int{"e1": 3, "e2": 0}
	r, next, err := ResolveAssignment(emps, Assignment{
		PositionID: "p", AssignRule: AssignLeastLoaded,
		Load: func(id string) int { return loads[id] },
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.EmployeeID != "e2" {
		t.Fatalf("应选 e2（运行数 0），got %s", r.EmployeeID)
	}
	if next != 0 {
		t.Fatalf("least_loaded 游标不推进，got %d", next)
	}
}
