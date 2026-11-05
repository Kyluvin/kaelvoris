// cron_model_test.go 定时任务级模型（需求：定时任务可设置大模型）：
//   - cron_job 工具 model 参数（add 写入 / update 更新 / 未提供保持不变）；
//   - syncCronSession 触发时把任务级 model/mode 同步到 cron_<jobID> 会话
//     （runTask 经「会话自有模型/模式」覆盖链生效）。
//
// 不依赖 Wails 上下文与异步任务链（App 仅构造 store/cronStore）。
package desktop

import (
	"context"
	"strings"
	"testing"

	"kaelvoris/internal/cron"
	"kaelvoris/internal/session"
)

// newCronTestApp 构造无 Wails 上下文的测试 App（store/cronStore 指临时目录）。
func newCronTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	st := session.NewStore(dir)
	cs, err := cron.NewStore(dir)
	if err != nil {
		t.Fatalf("cron store: %v", err)
	}
	return &App{store: st, cronStore: cs}
}

// TestCronToolModelParam cron_job 工具 model 参数：add 写入 / update 更新 /
// 留空不覆盖既有值（对齐 name/expr/mode 的「未提供保持不变」语义）。
func TestCronToolModelParam(t *testing.T) {
	app := newCronTestApp(t)
	tool := &CronTool{Store: app.cronStore}

	// add 带 model。
	out, err := tool.Execute(context.Background(), `{"action":"add","name":"日报","expr":"0 9 * * *","task":"生成日报","model":"deepseek-chat"}`)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out, "模型: deepseek-chat") {
		t.Fatalf("add 输出缺模型信息:\n%s", out)
	}
	jobs := app.cronStore.List()
	if len(jobs) != 1 || jobs[0].Model != "deepseek-chat" {
		t.Fatalf("model 未落库: %+v", jobs)
	}
	id := jobs[0].ID

	// update 换模型。
	if _, err := tool.Execute(context.Background(), `{"action":"update","id":"`+id+`","model":"gpt-4o"}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if j, _ := app.cronStore.Get(id); j.Model != "gpt-4o" {
		t.Fatalf("update model 未生效: %+v", j)
	}

	// update 不带 model：模型与其它未提供字段保持不变。
	if _, err := tool.Execute(context.Background(), `{"action":"update","id":"`+id+`","name":"日报2"}`); err != nil {
		t.Fatalf("update 无 model: %v", err)
	}
	if j, _ := app.cronStore.Get(id); j.Model != "gpt-4o" || j.Name != "日报2" {
		t.Fatalf("未提供的字段应保持不变: %+v", j)
	}
}

// TestSyncCronSession 任务级配置 → cron 会话：model/mode 同步写入会话记录
// （runTask 按会话自有值构建引擎）；model 清空 = 恢复跟随全局（空串）。
func TestSyncCronSession(t *testing.T) {
	app := newCronTestApp(t)
	// 预置 cron 会话（模拟历史触发已建会话；首次触发前会话不存在时
	// syncCronSession 应静默跳过，不报错）。
	sessID := "cron_job123"
	app.store.Save(&session.Session{ID: sessID, ChannelID: "desktop"})

	app.syncCronSession(sessID, cron.Job{Model: "deepseek-chat", Mode: "code"}, "")
	s, err := app.store.Get(sessID)
	if err != nil || s == nil {
		t.Fatalf("会话应存在: %v", err)
	}
	if snap := s.Snapshot(); snap.Model != "deepseek-chat" || snap.Mode != "code" {
		t.Fatalf("model/mode 应同步到会话: model=%q mode=%q", snap.Model, snap.Mode)
	}

	// 清空任务级模型 → 会话恢复跟随全局（空）。
	app.syncCronSession(sessID, cron.Job{}, "")
	if snap := s.Snapshot(); snap.Model != "" {
		t.Fatalf("model 清空后会话应跟随全局（空），got %q", snap.Model)
	}

	// 会话不存在：不 panic 不报错（幂等）。
	app.syncCronSession("cron_notexist", cron.Job{Model: "x"}, "")
}
