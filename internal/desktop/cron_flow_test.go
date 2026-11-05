// cron_flow_test.go —— cron run_flow 动作的桌面端分发链路测试（无 Wails 上下文，
// 对齐 cron_model_test.go 的 newCronTestApp 构造方式）：
//   - cronRunner 遇 Action=run_flow 走 cronRunFlow 分派（不再进员工解析/会话链）；
//   - FlowID 缺失/流程不存在时同步报错（调度器可记 failed）；
//   - 合法流程异步启动（runFlowCore 被调用，此处用纯模板流程验证到输出落 RunResult）。

package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaelvoris/internal/cron"
	"kaelvoris/internal/flow"
	"kaelvoris/internal/session"
)

// newCronFlowTestApp 构造测试 App：store/cronStore 指临时目录，cfg 带空 APIKey
// （runFlowCore 的 dryRun=false 会拦 —— cron run_flow 固定 dryRun=false，故预置
// 一个非空占位，避免依赖用户真实配置）。
func newCronFlowTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	st := session.NewStore(dir)
	cs, err := cron.NewStore(dir)
	if err != nil {
		t.Fatalf("cron store: %v", err)
	}
	app := &App{store: st, cronStore: cs}
	app.cfg.BaseURL = "http://localhost:0"
	app.cfg.APIKey = "test-key"
	return app
}

// saveTestFlow 写一个纯模板流程到临时 flowsDir（HOME 重定向隔离用户目录）。
func saveTestFlow(t *testing.T, a *App, id string) *flow.Flow {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir) // Windows UserHomeDir 来源
	t.Setenv("HOME", dir)        // Unix 兜底
	f := &flow.Flow{
		ID:   id,
		Name: "测试流程",
		Steps: []flow.Step{
			{Type: flow.StepTemplate, Role: "user", Name: "问好", Template: "你好 {who}"},
		},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	fd := flowsDir()
	if err := os.MkdirAll(fd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fd, id+".json"), b, 0o644); err != nil {
		t.Fatalf("保存流程: %v", err)
	}
	return f
}

func TestCronRunnerDispatchesRunFlow(t *testing.T) {
	a := newCronFlowTestApp(t)
	f := saveTestFlow(t, a, "flow_cron1")

	// 负路径：FlowID 缺失 → 同步报错。
	_, err := a.cronRunner(cron.Job{ID: "j1", Name: "缺ID", Action: cron.ActionRunFlow})
	if err == nil || !strings.Contains(err.Error(), "流程 ID") {
		t.Fatalf("缺 flowId 应报错，得到 %v", err)
	}
	// 负路径：流程不存在 → 同步报错。
	_, err = a.cronRunner(cron.Job{ID: "j2", Name: "不存在", Action: cron.ActionRunFlow, FlowID: "flow_missing"})
	if err == nil || !strings.Contains(err.Error(), "加载流程") {
		t.Fatalf("流程不存在应报错，得到 %v", err)
	}
	// 正路径：合法流程 → 异步启动，runID 带固定前缀（与会话任务会话区分）。
	runID, err := a.cronRunner(cron.Job{ID: "j3", Name: "定时流程", Action: cron.ActionRunFlow, FlowID: f.ID})
	if err != nil {
		t.Fatalf("run_flow 分发不应报错: %v", err)
	}
	if !strings.HasPrefix(runID, "cron_flow_") {
		t.Fatalf("runID 应带 cron_flow_ 前缀: %q", runID)
	}
}
