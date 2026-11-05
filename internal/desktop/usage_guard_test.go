// usage_guard_test.go —— 全局日额护栏单测（E1）：
// 覆盖 0 值不限 / 未超限放行 / 超限拒绝（含重置方法提示）/ 累计与跨日滚动
// / 损坏文件视为零重建。状态文件路径经 HOME 重定向到临时目录（并行安全）。
package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withUsageGuardHome 把 UserHomeDir 重定向到临时目录（usageGlobalPath 依赖）。
func withUsageGuardHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir) // Windows UserHomeDir 来源
	t.Setenv("HOME", dir)        // Unix 兜底
	return filepath.Join(dir, ".kaelvoris", "usage_global.json")
}

func TestUsageGuardZeroLimitAlwaysAllows(t *testing.T) {
	withUsageGuardHome(t)
	if err := usageGuardCheck(0); err != nil {
		t.Fatalf("limit 0 must allow, got: %v", err)
	}
	if err := usageGuardCheck(-1); err != nil {
		t.Fatalf("negative limit must allow, got: %v", err)
	}
}

func TestUsageGuardCheckAndRecord(t *testing.T) {
	path := withUsageGuardHome(t)

	// 未超限放行。
	if err := usageGuardCheck(10.0); err != nil {
		t.Fatalf("fresh state must allow: %v", err)
	}
	// 累计 $9.5。
	usageGuardRecord(9.5, 12)
	// 未达上限（>= 判定：9.5 < 10）仍放行。
	if err := usageGuardCheck(10.0); err != nil {
		t.Fatalf("9.5 < 10 must allow: %v", err)
	}
	// 再累计 $0.6 → 10.1 超限拒绝。
	usageGuardRecord(0.6, 3)
	err := usageGuardCheck(10.0)
	if err == nil {
		t.Fatal("10.1 >= 10 must reject")
	}
	if !strings.Contains(err.Error(), "重置") {
		t.Errorf("reject error should mention reset method: %v", err)
	}
	// 落盘内容校验。
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("state file missing: %v", rerr)
	}
	var g usageGlobal
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("state file corrupt: %v", err)
	}
	if g.CostUSD < 10.0 || g.Calls != 15 {
		t.Errorf("state = %+v, want cost>=10 calls=15", g)
	}
}

func TestUsageGuardCorruptFileTreatedAsZero(t *testing.T) {
	path := withUsageGuardHome(t)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("{not json"), 0o644)
	if err := usageGuardCheck(10.0); err != nil {
		t.Fatalf("corrupt file must be treated as zero: %v", err)
	}
}

func TestUsageGuardCrossDayReset(t *testing.T) {
	path := withUsageGuardHome(t)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	// 昨日已超限的状态 → 今日应滚动重置放行。
	_ = os.WriteFile(path, []byte(`{"date":"2000-01-01","costUSD":999,"calls":100}`), 0o644)
	if err := usageGuardCheck(10.0); err != nil {
		t.Fatalf("stale date must reset: %v", err)
	}
}

func TestUsageGuardIgnoreNonPositiveDelta(t *testing.T) {
	withUsageGuardHome(t)
	// 零成本任务（未配单价）不累计不落盘。
	usageGuardRecord(0, 5)
	if _, err := os.Stat(usageGlobalPath()); !os.IsNotExist(err) {
		t.Errorf("zero delta must not create state file, err=%v", err)
	}
}
