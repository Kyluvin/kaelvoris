package usage

import (
	"encoding/json"
	"testing"

	"kaelvoris/internal/llm"
)

func TestCounterRecordRealUsage(t *testing.T) {
	c := New(0.30, 1.20, 65536) // 示例单价：$0.30/M in, $1.20/M out
	c.Record(&llm.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}, "", "")
	c.Record(&llm.Usage{PromptTokens: 2000, CompletionTokens: 1000, TotalTokens: 3000}, "", "")
	got := c.Snapshot()
	if got.Requests != 2 || got.PromptTokens != 3000 || got.CompletionTokens != 1500 || got.TotalTokens != 4500 {
		t.Fatalf("bad totals: %+v", got)
	}
	if got.EstimatedTokens != 0 {
		t.Fatalf("real usage must not count as estimated: %+v", got)
	}
	// 成本：3000/1e6*0.30 + 1500/1e6*1.20 = 0.0009 + 0.0018 = 0.0027
	if got.CostUSD < 0.00269 || got.CostUSD > 0.00271 {
		t.Fatalf("bad cost: %v", got.CostUSD)
	}
	if c.ContextWindow() != 65536 {
		t.Fatal("context window lost")
	}
}

func TestCounterEstimateFallback(t *testing.T) {
	c := New(0, 0, 0)
	c.Record(nil, "12345678", "abcd") // 8/4=2 in, 4/4=1 out
	got := c.Snapshot()
	if got.Requests != 1 || got.PromptTokens != 2 || got.CompletionTokens != 1 {
		t.Fatalf("bad estimate: %+v", got)
	}
	if got.EstimatedTokens != 3 || got.TotalTokens != 3 {
		t.Fatalf("estimated tokens wrong: %+v", got)
	}
}

func TestTotalsJSON(t *testing.T) {
	c := New(1, 2, 0)
	c.Record(&llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, "", "")
	b, err := json.Marshal(c.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var back Totals
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.TotalTokens != 15 {
		t.Fatalf("roundtrip failed: %s", b)
	}
}

func TestNilSafety(t *testing.T) {
	var c *Counter
	c.Record(&llm.Usage{}, "", "") // 不得 panic
	s := c.Snapshot()
	if s.Requests != 0 {
		t.Fatal("nil counter must return zero totals")
	}
	if _, _, over := c.OverLimit(); over {
		t.Fatal("nil counter must never over-limit")
	}
}

// ---- E1 成本/调用护栏（第四批，对标 SWE-agent cost_limit / call_limit）----

func TestOverLimitCost(t *testing.T) {
	// 单价 $1/M：一次 3000 prompt + 1000 completion = $0.004。
	c := NewWithLimits(1.0, 1.0, 0, 0.005, 0)
	if _, _, over := c.OverLimit(); over {
		t.Fatal("fresh counter must not over-limit")
	}
	c.Record(&llm.Usage{PromptTokens: 3000, CompletionTokens: 1000, TotalTokens: 4000}, "", "")
	// 未超（0.004 < 0.005，超过才停对齐 mini "after exceeding"）。
	if _, _, over := c.OverLimit(); over {
		t.Fatal("cost 0.004 < limit 0.005 must not over-limit")
	}
	c.Record(&llm.Usage{PromptTokens: 2000, CompletionTokens: 0, TotalTokens: 2000}, "", "")
	kind, limit, over := c.OverLimit()
	if !over || kind != "cost" || limit != 0.005 {
		t.Fatalf("expected cost over-limit, got kind=%q limit=%v over=%v", kind, limit, over)
	}
}

func TestOverLimitCalls(t *testing.T) {
	// 调用次数轨：2 次达限（>=，第 N 次完成即达）；成本轨关闭（0）。
	c := NewWithLimits(0, 0, 0, 0, 2)
	c.Record(nil, "x", "y") // 1
	if _, _, over := c.OverLimit(); over {
		t.Fatal("1 call < limit 2 must not over-limit")
	}
	c.Record(nil, "x", "y") // 2
	kind, limit, over := c.OverLimit()
	if !over || kind != "calls" || limit != 2 {
		t.Fatalf("expected calls over-limit, got kind=%q limit=%v over=%v", kind, limit, over)
	}
}

func TestOverLimitUnset(t *testing.T) {
	// 限额未配置（0）恒不超限——向后兼容旧行为。
	c := New(1.0, 1.0, 0)
	c.Record(&llm.Usage{PromptTokens: 9e9, CompletionTokens: 9e9, TotalTokens: 18e9}, "", "")
	if _, _, over := c.OverLimit(); over {
		t.Fatal("unset limits must never over-limit")
	}
	// New 委托 NewWithLimits（0 限额），Limits 快照一致。
	costLimit, callLimit := c.Limits()
	if costLimit != 0 || callLimit != 0 {
		t.Fatalf("New defaults must be unset, got %v/%v", costLimit, callLimit)
	}
}
