// workflow_llm_test.go：LLM 节点画布端到端验证（可视化画布拖出的节点模板是否可跑）。
//
// 画布形态（对照 nodes/llm/llm.go 的 Config.Adapt 期望结构）：
//
//	start(100001, type=1, 输出变量 query)
//	    → llm(100002, type=3, llmParam 数组: systemPrompt/prompt/responseFormat/temperature/modelType/modleName)
//	    → end(900001, type=2, terminatePlan=returnVariables, answer=ref(llm.output))
//
// 关键约定（源码移植校准点）：
//   - llmParam 是 []*Param，Param.Name 分发字段；"modleName" 为 coze 源码固有拼写
//   - Param.Input 是 BlockInput：{"type":"literal","value":{"type":"literal","content":"..."}}
//     —— llmParamsToLLMParam 里 Content.(string) 强转，数值也必须传 JSON 字符串
//   - prompt 模板 {{query}} 引用上游 start 节点输出（block-output 全局可达）
//   - modelType=69010（Bootstrap 探测到的 agnes-2.5-flash 模型 ID，日志可见
//     "sync old model id 69010 - agnes-2.5-flash"）；ModelType 实为模型表主键，
//     传 0 会报 "model 0 not found"
package cozeflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	model "github.com/coze-dev/coze-studio/backend/crossdomain/workflow/model"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
)

// TestWorkflowLLMNodeExecute start → 大模型 → end 全链路（真实调用 LLM）。
func TestWorkflowLLMNodeExecute(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	c, err := Bootstrap(ctx, dir)
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 外部 agnes API 不可用/限流（ModelConfigured=false）时跳过，避免发布门禁被外部限流阻塞
	if !c.ModelConfigured {
		t.Skip("cozeflow: 外部 agnes API 不可用/限流，跳过 LLM 链路测试")
	}

	// ---- 画布：start(query) → llm({{query}} 问答) → end(answer=llm.output) ----
	const canvas = `{
		"nodes": [
			{
				"id": "100001", "type": "1",
				"data": {
					"nodeMeta": {"title": "开始"},
					"outputs": [
						{"name": "query", "type": "string", "description": "用户问题", "required": true}
					]
				}
			},
			{
				"id": "100002", "type": "3",
				"data": {
					"nodeMeta": {"title": "大模型"},
					"inputs": {
						"llmParam": [
							{"name": "modelType",    "input": {"type": "literal", "value": {"type": "literal", "content": "69010"}}},
							{"name": "modleName",    "input": {"type": "literal", "value": {"type": "literal", "content": "agnes-2.5-flash"}}},
							{"name": "systemPrompt", "input": {"type": "literal", "value": {"type": "literal", "content": "You are a helpful assistant. Answer briefly."}}},
							{"name": "prompt",       "input": {"type": "literal", "value": {"type": "literal", "content": "What is 2+3? Reply with just the number."}}},
							{"name": "responseFormat", "input": {"type": "literal", "value": {"type": "literal", "content": "0"}}},
							{"name": "temperature",  "input": {"type": "literal", "value": {"type": "literal", "content": "0.5"}}}
						]
					},
					"outputs": [
						{"name": "output", "type": "string", "description": "模型回答"}
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
								"name": "answer",
								"input": {
									"type": "string",
									"value": {"type": "ref", "content": {"blockID": "100002", "name": "output", "source": "block-output"}}
								}
							}
						]
					},
					"outputs": [
						{"name": "answer", "type": "string", "description": "最终回答"}
					]
				}
			}
		],
		"edges": [
			{"sourceNodeID": "100001", "targetNodeID": "100002"},
			{"sourceNodeID": "100002", "targetNodeID": "900001"}
		]
	}`

	// ---- 创建 + 保存草稿（Save 会做画布编译校验，模板字段错误在此暴露）----
	wfID, err := c.Workflow.DomainSVC.Create(ctx, &vo.MetaCreate{
		Name:             "llm-node-smoke",
		Desc:             "LLM 节点画布模板验证",
		SpaceID:          1,
		CreatorID:        1,
		InitCanvasSchema: canvas,
	})
	if err != nil {
		t.Fatalf("创建工作流失败: %v", err)
	}
	if err := c.Workflow.DomainSVC.Save(ctx, wfID, canvas); err != nil {
		t.Fatalf("保存画布失败（LLM 节点模板与 adaptor 不匹配）: %v", err)
	}

	// ---- 发布 + 同步执行（真实调用 LLM；模型端点 Bootstrap 已探测可用）----
	if err := c.Workflow.DomainSVC.Publish(ctx, &vo.PublishPolicy{
		ID:                 wfID,
		Version:            "0.0.1",
		VersionDescription: "LLM 节点验证",
		CreatorID:          1,
		Force:              true,
	}); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

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
		t.Fatalf("执行未成功: status=%v failReason=%v output=%v", exe.Status, exe.FailReason, exe.Output)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(*exe.Output), &out); err != nil {
		t.Fatalf("解析输出失败: %v raw=%s", err, *exe.Output)
	}
	t.Logf("LLM 节点执行成功: output=%v", out)

	// 2+3 的答案应包含 "5"（LLM 输出宽松断言：数字 5 出现在回答里）
	answer, _ := out["answer"].(string)
	if !strings.Contains(answer, "5") {
		t.Fatalf("期望回答包含 5，实际 %q", answer)
	}
}
