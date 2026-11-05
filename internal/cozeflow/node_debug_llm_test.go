// node_debug_llm_test.go：复现并验证 LLM 节点的单节点试运行（NodeDebug 链路）。
package cozeflow

import (
	"context"
	"strings"
	"testing"
	"time"

	model "github.com/coze-dev/coze-studio/backend/crossdomain/workflow/model"
	wfentity "github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
)

// TestNodeDebugLLM 对大模型节点做单节点试运行（NodeDebug）。
func TestNodeDebugLLM(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	c, err := Bootstrap(ctx, dir)
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 外部 agnes API 不可用/限流（ModelConfigured=false）时跳过，避免发布门禁被外部限流阻塞
	if !c.ModelConfigured {
		t.Skip("cozeflow: 外部 agnes API 不可用/限流，跳过 LLM 节点测试")
	}

	// 画布与 workflow_llm_test.go 相同（start → llm → end）
	const canvas = `{
		"nodes": [
			{"id": "100001", "type": "1", "data": {"nodeMeta": {"title": "开始"}, "outputs": [{"name": "query", "type": "string", "required": true}]}},
			{"id": "100002", "type": "3", "data": {
				"nodeMeta": {"title": "大模型"},
				"inputs": {"llmParam": [
					{"name": "modelType", "input": {"type": "literal", "value": {"type": "literal", "content": "69010"}}},
					{"name": "modleName", "input": {"type": "literal", "value": {"type": "literal", "content": "agnes-2.5-flash"}}},
					{"name": "systemPrompt", "input": {"type": "literal", "value": {"type": "literal", "content": "Answer briefly."}}},
					{"name": "prompt", "input": {"type": "literal", "value": {"type": "literal", "content": "What is 3+4? Reply with just the number."}}},
					{"name": "responseFormat", "input": {"type": "literal", "value": {"type": "literal", "content": "0"}}},
					{"name": "temperature", "input": {"type": "literal", "value": {"type": "literal", "content": "0.5"}}}
				]},
				"outputs": [{"name": "output", "type": "string"}]
			}},
			{"id": "900001", "type": "2", "data": {"nodeMeta": {"title": "结束"}, "inputs": {"terminatePlan": "returnVariables", "inputParameters": [
				{"name": "answer", "input": {"type": "string", "value": {"type": "ref", "content": {"blockID": "100002", "name": "output", "source": "block-output"}}}}
			]}, "outputs": [{"name": "answer", "type": "string"}]}}
		],
		"edges": [
			{"sourceNodeID": "100001", "targetNodeID": "100002"},
			{"sourceNodeID": "100002", "targetNodeID": "900001"}
		]
	}`

	wfID, err := c.Workflow.DomainSVC.Create(ctx, &vo.MetaCreate{
		Name: "node-debug-llm", SpaceID: 1, CreatorID: 1, InitCanvasSchema: canvas,
	})
	if err != nil {
		t.Fatalf("创建工作流失败: %v", err)
	}
	if err := c.Workflow.DomainSVC.Save(ctx, wfID, canvas); err != nil {
		t.Fatalf("保存画布失败: %v", err)
	}

	// 单节点试运行 LLM 节点（输入 query 由 NodeDebug 直接提供）
	exeID, err := c.Workflow.DomainSVC.AsyncExecuteNode(ctx, "100002", model.ExecuteConfig{
		ID: wfID, From: model.FromDraft, Operator: 1,
		Mode: model.ExecuteModeNodeDebug, SyncPattern: model.SyncPatternSync, BizType: model.BizTypeWorkflow,
	}, map[string]any{"query": "hello"})
	if err != nil {
		t.Fatalf("单节点执行发起失败: %v", err)
	}

	var nodeExe *wfentity.NodeExecution
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		ne, _, err := c.Workflow.DomainSVC.GetNodeExecution(ctx, exeID, "100002")
		if err == nil {
			t.Logf("  node=%s status=%v output=%v err=%v", ne.NodeID, ne.Status, deref(ne.Output), deref(ne.ErrorInfo))
			if ne.Status == wfentity.NodeSuccess {
				nodeExe = ne
				break
			}
			if ne.Status == wfentity.NodeFailed {
				t.Fatalf("单节点执行失败: output=%v err=%v", deref(ne.Output), deref(ne.ErrorInfo))
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if nodeExe == nil {
		t.Fatal("超时未拿到节点执行结果")
	}
	// 正常断言：节点成功且输出包含 3+4 的答案（此前遗留诊断 t.Fatal 已移除）
	if nodeExe.Status != wfentity.NodeSuccess {
		t.Fatalf("节点状态非成功: status=%v err=%v", nodeExe.Status, deref(nodeExe.ErrorInfo))
	}
	out := deref(nodeExe.Output)
	if !strings.Contains(out, "7") {
		t.Fatalf("输出未包含预期答案 7: output=%s", out)
	}
}
