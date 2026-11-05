// workflow_roundtrip_test.go：复刻前端 loadCanvas/exportCanvas 往返后的画布形态
//（meta.position 坐标、edge.sourcePortID 空串、顶层字段透传），验证整图运行链路。
package cozeflow

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	model "github.com/coze-dev/coze-studio/backend/crossdomain/workflow/model"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
)

// runCfg 与面板 CozeRunWorkflow（fromDraft）一致的整图执行配置。
func runCfg(wfID int64) model.ExecuteConfig {
	return model.ExecuteConfig{
		ID: wfID, From: model.FromDraft, Operator: 1,
		Mode: model.ExecuteModeRelease, SyncPattern: model.SyncPatternSync,
		BizType: model.BizTypeWorkflow, WorkflowMode: 0,
	}
}

// TestWorkflowRoundtripRun 前端画布 JSON 往返 → 保存 → 整图运行（start→llm→end）。
func TestWorkflowRoundtripRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	c, err := Bootstrap(ctx, dir)
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 外部 agnes API 不可用/限流（ModelConfigured=false）时跳过，避免发布门禁被外部限流阻塞
	if !c.ModelConfigured {
		t.Skip("cozeflow: 外部 agnes API 不可用/限流，跳过 LLM 整图测试")
	}

	// 前端 exportCanvas 的形态：节点带 meta.position、顶层透传字段、
	// edge 带 sourcePortID 空串（普通连线）、节点字段顺序/嵌套与前端模板一致。
	const canvas = `{"nodes":[` +
		`{"id":"100001","type":"1","meta":{"position":{"x":60,"y":60}},"data":{"nodeMeta":{"title":"开始"},"outputs":[{"name":"query","type":"string","description":"用户输入","required":true}]}},` +
		`{"id":"100002","type":"3","meta":{"position":{"x":340,"y":60}},"data":{"nodeMeta":{"title":"大模型"},"inputs":{"llmParam":[{"name":"modelType","input":{"type":"literal","value":{"type":"literal","content":"69010"}}},{"name":"modleName","input":{"type":"literal","value":{"type":"literal","content":"agnes-2.5-flash"}}},{"name":"systemPrompt","input":{"type":"literal","value":{"type":"literal","content":"你是一个乐于助人的助手。"}}},{"name":"prompt","input":{"type":"literal","value":{"type":"literal","content":"回答这个问题：{{query}}。直接给出答案。"}}},{"name":"responseFormat","input":{"type":"literal","value":{"type":"literal","content":"0"}}},{"name":"temperature","input":{"type":"literal","value":{"type":"literal","content":"0.7"}}}],"inputParameters":[{"name":"query","input":{"type":"string","value":{"type":"ref","content":{"blockID":"100001","name":"query","source":"block-output"}}}}]},"outputs":[{"name":"output","type":"string","description":"模型回答"}]}},` +
		`{"id":"900001","type":"2","meta":{"position":{"x":620,"y":60}},"data":{"nodeMeta":{"title":"结束"},"inputs":{"terminatePlan":"returnVariables","inputParameters":[{"name":"answer","input":{"type":"string","value":{"type":"ref","content":{"blockID":"100002","name":"output","source":"block-output"}}}}]},"outputs":[{"name":"answer","type":"string"}]}}` +
		`],"edges":[{"sourceNodeID":"100001","targetNodeID":"100002","sourcePortID":""},{"sourceNodeID":"100002","targetNodeID":"900001","sourcePortID":""}]}`

	wfID, err := c.Workflow.DomainSVC.Create(ctx, &vo.MetaCreate{
		Name: "roundtrip-run", SpaceID: 1, CreatorID: 1, InitCanvasSchema: canvas,
	})
	if err != nil {
		t.Fatalf("创建工作流失败: %v", err)
	}
	if err := c.Workflow.DomainSVC.Save(ctx, wfID, canvas); err != nil {
		t.Fatalf("保存画布失败: %v", err)
	}

	// 整图运行（同面板 CozeRunWorkflow：SyncExecute + FromDraft）
	exe, _, err := c.Workflow.DomainSVC.SyncExecute(ctx, runCfg(wfID), map[string]any{"query": "2+3等于几？直接给答案"})
	if err != nil {
		t.Fatalf("整图运行失败: %v", err)
	}
	// 轮询至终态（GetExecution 签名：传 WorkflowExecution 对象、返回 (exec, err)）
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := c.Workflow.DomainSVC.GetExecution(ctx, exe, false)
		if err == nil && cur.Status != entity.WorkflowRunning {
			exe = cur
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if exe.Status != entity.WorkflowSuccess {
		t.Fatalf("整图运行未成功: status=%v failReason=%v output=%v", exe.Status, deref(exe.FailReason), deref(exe.Output))
	}
	// 诊断落盘：workflow 输出 + end/llm 节点执行记录（终端重定向会吞 go test 日志）
	var diag string
	diag += fmt.Sprintf("workflow output=%s\n", deref(exe.Output))
	for _, nid := range []string{"100002", "900001"} {
		ne, _, err := c.Workflow.DomainSVC.GetNodeExecution(ctx, exe.ID, nid)
		if err != nil {
			diag += fmt.Sprintf("node=%s err=%v\n", nid, err)
			continue
		}
		diag += fmt.Sprintf("node=%s status=%v input=%s output=%s raw=%s\n",
			nid, ne.Status, deref(ne.Input), deref(ne.Output), deref(ne.RawOutput))
	}
	_ = os.WriteFile("../../_diag_rt.txt", []byte(diag), 0o644)
	if !strings.Contains(deref(exe.Output), "answer") {
		t.Fatalf("整图输出缺少 answer 字段: %s\n%s", deref(exe.Output), diag)
	}
	t.Logf("整图运行成功: output=%s", deref(exe.Output))
}

// TestWorkflowLoopRoundtripRun 循环容器（count×3）+ 体内代码节点的嵌套画布整图运行：
// 验证前端导出形态（blocks/edges + 自动起点/终点边 loop-function-inline-*）→ 内核递归 adapt → inner 图逐轮执行。
func TestWorkflowLoopRoundtripRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	c, err := Bootstrap(ctx, dir)
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 前端 exportCanvas 形态：循环节点带 blocks（体内节点）/edges（体内边 + 自动起终点边），
	// 循环 outputs 为 Param 形态（input.ref 指向体内代码节点输出 x = 每轮聚合）。
	const canvas = `{"nodes":[` +
		`{"id":"100001","type":"1","meta":{"position":{"x":60,"y":60}},"data":{"nodeMeta":{"title":"开始"},"outputs":[]}},` +
		`{"id":"200002","type":"21","meta":{"position":{"x":340,"y":60}},"data":{"nodeMeta":{"title":"循环"},"inputs":{"loopType":"count","loopCount":{"type":"integer","value":{"type":"literal","content":"3"}},"inputParameters":[],"variableParameters":[]},"outputs":[{"name":"outputList","input":{"type":"list","schema":{"type":"string"},"value":{"type":"ref","content":{"blockID":"200021","name":"x","source":"block-output"}}}}]},"blocks":[{"id":"200021","type":"5","meta":{"position":{"x":80,"y":80}},"data":{"nodeMeta":{"title":"代码"},"inputs":{"code":"async def main(args):\n    return {'x': 'run'}","language":3,"inputParameters":[]},"outputs":[{"name":"x","type":"string"}]}}],"edges":[{"sourceNodeID":"200002","targetNodeID":"200021","sourcePortID":"loop-function-inline-output"},{"sourceNodeID":"200021","targetNodeID":"200002","targetPortID":"loop-function-inline-input"}]},` +
		`{"id":"900001","type":"2","meta":{"position":{"x":620,"y":60}},"data":{"nodeMeta":{"title":"结束"},"inputs":{"terminatePlan":"returnVariables","inputParameters":[{"name":"list","input":{"type":"string","value":{"type":"ref","content":{"blockID":"200002","name":"outputList","source":"block-output"}}}}]},"outputs":[{"name":"list","type":"string"}]}}` +
		`],"edges":[{"sourceNodeID":"100001","targetNodeID":"200002","sourcePortID":""},{"sourceNodeID":"200002","targetNodeID":"900001","sourcePortID":""}]}`

	wfID, err := c.Workflow.DomainSVC.Create(ctx, &vo.MetaCreate{
		Name: "loop-roundtrip-run", SpaceID: 1, CreatorID: 1, InitCanvasSchema: canvas,
	})
	if err != nil {
		t.Fatalf("创建工作流失败: %v", err)
	}
	if err := c.Workflow.DomainSVC.Save(ctx, wfID, canvas); err != nil {
		t.Fatalf("保存画布失败: %v", err)
	}

	exe, _, err := c.Workflow.DomainSVC.SyncExecute(ctx, runCfg(wfID), map[string]any{})
	if err != nil {
		t.Fatalf("整图运行失败: %v", err)
	}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := c.Workflow.DomainSVC.GetExecution(ctx, exe, false)
		if err == nil && cur.Status != entity.WorkflowRunning {
			exe = cur
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if exe.Status != entity.WorkflowSuccess {
		t.Fatalf("循环整图运行未成功: status=%v failReason=%v output=%v", exe.Status, deref(exe.FailReason), deref(exe.Output))
	}
	var diag string
	diag += fmt.Sprintf("workflow output=%s\n", deref(exe.Output))
	ne, _, err := c.Workflow.DomainSVC.GetNodeExecution(ctx, exe.ID, "200002")
	if err == nil {
		diag += fmt.Sprintf("loop node status=%v output=%s\n", ne.Status, deref(ne.Output))
	}
	_ = os.WriteFile("../../_diag_loop.txt", []byte(diag), 0o644)
	// 3 轮代码输出 x="run" → outputList 聚合为 ["run","run","run"]
	if !strings.Contains(deref(exe.Output), "run") {
		t.Fatalf("循环输出缺少聚合结果: %s\n%s", deref(exe.Output), diag)
	}
	t.Logf("循环整图运行成功: output=%s", deref(exe.Output))
}
