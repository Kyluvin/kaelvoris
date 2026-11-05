// usage_guard.go —— 全局日额护栏（E1，第四批，对标 SWE-agent
// MSWEA_GLOBAL_COST_LIMIT 全局成本上限）。
//
// 单任务护栏（usage.Counter.OverLimit）防单任务失控；本文件补进程级护栏：
// 多会话/定时任务/IM 通道并行叠加时的「当日总成本」熔断——超日额后新任务
// 直接拒绝启动（错误说明用量/上限/重置方法），已运行任务不受影响。
//
// 状态落 ~/.kaelvoris/usage_global.json（date/costUSD/calls），跨日（本地
// 时区）自动滚动重置；文件损坏按「跳过不致命」约定视为零重建。
// 累计口径：任务收尾时按引擎 Usage 快照与任务前基线的差值累加（任务粒度
// 精度足够防失控；ponytail：需要每轮实时落盘时改为 say usage 事件挂点）。
package desktop

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// usageGuardMu 串行化「读-改-写」（并行会话任务收尾同时累计）。
var usageGuardMu sync.Mutex

// usageGlobal 全局日额状态（usage_global.json 结构）。
type usageGlobal struct {
	Date    string  `json:"date"`    // YYYY-MM-DD（本地时区，跨日滚动重置）
	CostUSD float64 `json:"costUSD"` // 当日累计成本（美元）
	Calls   int     `json:"calls"`   // 当日累计 LLM 调用次数
}

// usageGlobalPath 状态文件路径（~/.kaelvoris/usage_global.json）。
func usageGlobalPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kaelvoris", "usage_global.json")
}

// today 本地时区日期串（YYYY-MM-DD）。
func today() string { return time.Now().Format("2006-01-02") }

// usageGuardLoad 读取状态；缺失/损坏/跨日一律返回今日零值（损坏重建，
// 跨日滚动重置——不回写，写入时统一落盘）。
func usageGuardLoad(path string) usageGlobal {
	g := usageGlobal{Date: today()}
	if path == "" {
		return g
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return g
	}
	var st usageGlobal
	if err := json.Unmarshal(b, &st); err != nil {
		return g // 损坏视为零（对齐「损坏跳过不致命」约定）
	}
	if st.Date != today() {
		return g // 跨日滚动重置
	}
	return st
}

// usageGuardSave 原子写盘（temp + rename，对齐仓库持久化约定）。
func usageGuardSave(path string, g usageGlobal) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// usageGuardCheck 新任务启动前的全局日额校验（limitUSD<=0 = 不限，恒放行）。
// 超限返回带重置方法的拒绝错误（不阻断已运行任务）。
func usageGuardCheck(limitUSD float64) error {
	if limitUSD <= 0 {
		return nil
	}
	usageGuardMu.Lock()
	defer usageGuardMu.Unlock()
	g := usageGuardLoad(usageGlobalPath())
	if g.CostUSD >= limitUSD {
		return fmt.Errorf("全局日成本护栏触发：今日已用 $%.2f，达上限 $%.2f，新任务已拒绝。可调高设置中的 globalCostLimitUSD，或删除 %s 重置", g.CostUSD, limitUSD, usageGlobalPath())
	}
	return nil
}

// usageGuardRecord 任务收尾时累计本任务成本增量（deltaUSD<=0 跳过）。
// 失败仅日志（护栏统计不致命，不影响任务收尾链路）。
func usageGuardRecord(deltaUSD float64, calls int) {
	if deltaUSD <= 0 {
		return
	}
	usageGuardMu.Lock()
	defer usageGuardMu.Unlock()
	path := usageGlobalPath()
	g := usageGuardLoad(path)
	g.CostUSD += deltaUSD
	g.Calls += calls
	if err := usageGuardSave(path, g); err != nil {
		slog.Warn("usage_guard: 累计落盘失败（不致命）", "err", err)
	}
}
