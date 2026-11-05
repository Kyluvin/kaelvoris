// node_debug_test.go：单节点试运行验证（对标 Dify 的节点级测试）。
//
// 走内核原生 NodeDebug 链路：AsyncExecuteNode(ExecuteModeNodeDebug, FromDraft)
// → 轮询 GetExecution(includeNodes) → 断言被测节点的输入/输出。
//
// 同时验证前端 CozeCanvas 两类节点模板的正确性：
//   - Code 节点：inputs.code(Python) + inputs.language=3，整图执行与单节点执行均验证
//   - LLM 节点：inputs.llmParam 数组（已由 workflow_llm_test.go 验证，此处复用单节点链路）
package cozeflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	model "github.com/coze-dev/coze-studio/backend/crossdomain/workflow/model"
	wfentity "github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
)

// TestNodeDebugCode 对代码节点做单节点试运行：先整图跑通，再单节点执行。
func TestNodeDebugCode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	c, err := Bootstrap(ctx, dir)
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	// ---- 画布：start(query) → code(透传 query) → end(result=code.result) ----
	const canvas = `{
		"nodes": [
			{
				"id": "100001", "type": "1",
				"data": {
					"nodeMeta": {"title": "开始"},
					"outputs": [{"name": "query", "type": "string", "required": true}]
				}
			},
			{
				"id": "100002", "type": "5",
				"data": {
					"nodeMeta": {"title": "代码"},
					"inputs": {
						"code": "async def main(args: Args) -> Output:\n    params = args.params\n    ret: Output = {\"result\": \"code:\" + params['query']}\n    return ret",
						"language": 3,
						"inputParameters": [
							{"name": "query", "input": {"type": "string", "value": {"type": "ref", "content": {"blockID": "100001", "name": "query", "source": "block-output"}}}}
						]
					},
					"outputs": [{"name": "result", "type": "string"}]
				}
			},
			{
				"id": "900001", "type": "2",
				"data": {
					"nodeMeta": {"title": "结束"},
					"inputs": {
						"terminatePlan": "returnVariables",
						"inputParameters": [
							{"name": "result", "input": {"type": "string", "value": {"type": "ref", "content": {"blockID": "100002", "name": "result", "source": "block-output"}}}}
						]
					},
					"outputs": [{"name": "result", "type": "string"}]
				}
			}
		],
		"edges": [
			{"sourceNodeID": "100001", "targetNodeID": "100002"},
			{"sourceNodeID": "100002", "targetNodeID": "900001"}
		]
	}`

	wfID, err := c.Workflow.DomainSVC.Create(ctx, &vo.MetaCreate{
		Name: "node-debug-code", SpaceID: 1, CreatorID: 1,
		InitCanvasSchema: canvas,
	})
	if err != nil {
		t.Fatalf("创建工作流失败: %v", err)
	}
	if err := c.Workflow.DomainSVC.Save(ctx, wfID, canvas); err != nil {
		t.Fatalf("保存画布失败（Code 节点模板与 adaptor 不匹配）: %v", err)
	}

	// ---- 整图执行（验证 code 节点在链路中可用）----
	exe, _, err := c.Workflow.DomainSVC.SyncExecute(ctx, model.ExecuteConfig{
		ID: wfID, From: model.FromDraft, Operator: 1,
		Mode: model.ExecuteModeRelease, SyncPattern: model.SyncPatternSync, BizType: model.BizTypeWorkflow,
	}, map[string]any{"query": "hello"})
	if err != nil {
		t.Fatalf("整图执行失败: %v", err)
	}
	if exe.Status != wfentity.WorkflowSuccess {
		t.Fatalf("整图执行未成功: %v fail=%v", exe.Status, exe.FailReason)
	}
	var whole map[string]any
	_ = json.Unmarshal([]byte(*exe.Output), &whole)
	t.Logf("整图执行输出: %v", whole)
	if whole["result"] != "code:hello" {
		t.Fatalf("期望 result=code:hello，实际 %v", whole["result"])
	}

	// ---- 单节点试运行（NodeDebug：直接对 code 节点执行，输入手工给定）----
	exeID, err := c.Workflow.DomainSVC.AsyncExecuteNode(ctx, "100002", model.ExecuteConfig{
		ID: wfID, From: model.FromDraft, Operator: 1,
		Mode: model.ExecuteModeNodeDebug, SyncPattern: model.SyncPatternSync, BizType: model.BizTypeWorkflow,
	}, map[string]any{"query": "solo"})
	if err != nil {
		t.Fatalf("单节点执行发起失败: %v", err)
	}

	var nodeExe *wfentity.NodeExecution
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		// 轮询被测节点自身的执行记录（不走 GetExecution：其内部查 interrupt
		// event 表，NodeDebug 短执行下存在空串记录导致 EOF）。
		ne, _, err := c.Workflow.DomainSVC.GetNodeExecution(ctx, exeID, "100002")
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		t.Logf("  node=%s type=%s status=%v output=%v", ne.NodeID, ne.NodeType, ne.Status, deref(ne.Output))
		if ne.Status == wfentity.NodeSuccess {
			nodeExe = ne
			break
		}
		if ne.Status == wfentity.NodeFailed {
			t.Fatalf("单节点执行失败: output=%v err=%v", deref(ne.Output), deref(ne.ErrorInfo))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if nodeExe == nil {
		t.Fatal("未找到被测节点(100002)的执行记录")
	}
	t.Logf("单节点执行: status=%v input=%v output=%v", nodeExe.Status, deref(nodeExe.Input), deref(nodeExe.Output))
	if nodeExe.Output == nil || !strings.Contains(*nodeExe.Output, "code:solo") {
		t.Fatalf("期望单节点输出含 code:solo，实际 %v", deref(nodeExe.Output))
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
