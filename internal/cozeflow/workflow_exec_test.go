// workflow_exec_test.go：工作流全链路执行验证（创建→保存→发布→同步执行）。
//
// 画布形态（canvas JSON → adaptor → eino compose → 执行）：
//
//	start(100001, type=1, 输出变量 query)
//	    → end(900001, type=2, terminatePlan=returnVariables, 返回 echo=query)
//
// 输入 {"query": "hello"}，期望输出 {"echo": "hello"}——验证变量引用
//（BlockInputValueTypeRef / block-output）在嵌入式内核下正确解析。
package cozeflow

import (
	"context"
	"encoding/json"
	"testing"

	model "github.com/coze-dev/coze-studio/backend/crossdomain/workflow/model"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
)

// TestWorkflowCreatePublishExecute 创建 → 保存 → 发布 → 同步执行 全闭环。
func TestWorkflowCreatePublishExecute(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	c, err := Bootstrap(ctx, dir)
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	// ---- 画布：start(query) → end(echo=query) ----
	const canvas = `{
		"nodes": [
			{
				"id": "100001", "type": "1",
				"data": {
					"nodeMeta": {"title": "开始"},
					"outputs": [
						{"name": "query", "type": "string", "description": "用户输入", "required": true}
					]
				}
			},
			{
				"id": "900001", "type": "2",
				"data": {
					"nodeMeta": {"title": "结束"},
					"inputs": {
						"terminatePlan": "returnVariables",
						"inputParameters": [
							{
								"name": "echo",
								"input": {
									"type": "string",
									"value": {"type": "ref", "content": {"blockID": "100001", "name": "query", "source": "block-output"}}
								}
							}
						]
					},
					"outputs": [
						{"name": "echo", "type": "string", "description": "回显输入"}
					]
				}
			}
		],
		"edges": [{"sourceNodeID": "100001", "targetNodeID": "900001"}]
	}`

	// ---- 创建 + 保存草稿 ----
	wfID, err := c.Workflow.DomainSVC.Create(ctx, &vo.MetaCreate{
		Name:             "exec-smoke",
		Desc:             "执行闭环冒烟",
		SpaceID:          1,
		CreatorID:        1,
		InitCanvasSchema: canvas,
	})
	if err != nil {
		t.Fatalf("创建工作流失败: %v", err)
	}
	t.Logf("工作流已创建: id=%d", wfID)

	if err := c.Workflow.DomainSVC.Save(ctx, wfID, canvas); err != nil {
		t.Fatalf("保存画布失败: %v", err)
	}

	// ---- 发布（Force 绕过 test-run 前置检查）----
	if err := c.Workflow.DomainSVC.Publish(ctx, &vo.PublishPolicy{
		ID:                 wfID,
		Version:            "0.0.1",
		VersionDescription: "冒烟首版",
		CreatorID:          1,
		Force:              true,
	}); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	t.Log("已发布版本 0.0.1")

	// ---- 同步执行（latest version）----
	exe, _, err := c.Workflow.DomainSVC.SyncExecute(ctx, model.ExecuteConfig{
		ID:           wfID,
		From:         model.FromLatestVersion,
		Operator:     1,
		Mode:         model.ExecuteModeRelease,
		SyncPattern:  model.SyncPatternSync,
		BizType:      model.BizTypeWorkflow,
		WorkflowMode: 0,
	}, map[string]any{"query": "hello"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if exe.Status != entity.WorkflowSuccess {
		t.Fatalf("执行未成功: status=%v err=%v failReason=%v output=%v", exe.Status, exe.ErrorCode, exe.FailReason, exe.Output)
	}
	if exe.Output == nil {
		t.Fatalf("执行输出为空: status=%v", exe.Status)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(*exe.Output), &out); err != nil {
		t.Fatalf("解析输出失败: %v raw=%s", err, *exe.Output)
	}
	t.Logf("执行成功: output=%v", out)
	if out["echo"] != "hello" {
		t.Fatalf("期望 echo=hello，实际 %v", out["echo"])
	}
}
