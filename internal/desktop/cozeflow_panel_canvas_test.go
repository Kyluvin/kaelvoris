package desktop

// cozeflow_panel_canvas_test.go：CozeCreateWorkflow 画布模板防回归测试。
// 背景（2026-10-28）：模板在结束节点 inputs 收尾多写一个 '}'，coze 后端 Save
// 阶段 sonic 反序列化报 720701011 "invalid character '}' after array element"，
// 前端表现为「创建工作流失败」。此处断言模板 JSON 合法 + 结构正确。

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCozeDefaultCanvasJSON 模板必须是合法 JSON，且 outputs 挂在 data 内、3 节点 2 边。
func TestCozeDefaultCanvasJSON(t *testing.T) {
	canvas := cozeDefaultCanvas()
	var v struct {
		Nodes []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Data struct {
				NodeMeta struct {
					Title string `json:"title"`
				} `json:"nodeMeta"`
				Inputs   json.RawMessage `json:"inputs"`
				Outputs  json.RawMessage `json:"outputs"`
				MetaName string          `json:"meta"` // 若 outputs 错挂到 node 层会读不到
			} `json:"data"`
		} `json:"nodes"`
		Edges []struct {
			Source string `json:"sourceNodeID"`
			Target string `json:"targetNodeID"`
		} `json:"edges"`
	}
	if err := json.Unmarshal([]byte(canvas), &v); err != nil {
		t.Fatalf("canvas JSON 非法: %v\ncanvas: %s", err, canvas)
	}
	if len(v.Nodes) != 3 {
		t.Fatalf("want 3 nodes, got %d", len(v.Nodes))
	}
	if len(v.Edges) != 2 {
		t.Fatalf("want 2 edges, got %d", len(v.Edges))
	}
	for _, n := range v.Nodes {
		if !strings.Contains(string(n.Data.Outputs), "\"outputs\"") && len(n.Data.Outputs) == 0 {
			t.Fatalf("node %s data.outputs 缺失（括号错位会把 outputs 挤到 node 层）", n.ID)
		}
		if len(n.Data.Inputs) > 0 {
			// inputs/outputs 自身也必须是合法 JSON 片段
			var probe interface{}
			if err := json.Unmarshal(n.Data.Inputs, &probe); err != nil {
				t.Fatalf("node %s inputs 片段非法: %v", n.ID, err)
			}
			if err := json.Unmarshal(n.Data.Outputs, &probe); err != nil {
				t.Fatalf("node %s outputs 片段非法: %v", n.ID, err)
			}
		}
	}
	// 结束节点必须有 terminatePlan + inputParameters（曾因括号错位丢字段）
	var endNode struct {
		Inputs struct {
			TerminatePlan   string          `json:"terminatePlan"`
			InputParameters json.RawMessage `json:"inputParameters"`
		} `json:"inputs"`
	}
	found := false
	for _, n := range v.Nodes {
		if n.ID == "900001" {
			found = true
			if err := json.Unmarshal([]byte(fmtNodeData(t, canvas, "900001")), &endNode); err != nil {
				t.Fatalf("解析结束节点: %v", err)
			}
			if endNode.Inputs.TerminatePlan != "returnVariables" {
				t.Fatalf("结束节点 terminatePlan = %q, want returnVariables", endNode.Inputs.TerminatePlan)
			}
			if len(endNode.Inputs.InputParameters) == 0 {
				t.Fatal("结束节点 inputParameters 缺失（括号错位特征）")
			}
		}
	}
	if !found {
		t.Fatal("缺少 900001 结束节点")
	}
}

// fmtNodeData 从 canvas 提取指定节点 data 原始 JSON（测试辅助）。
func fmtNodeData(t *testing.T, canvas, id string) []byte {
	t.Helper()
	var raw struct {
		Nodes []struct {
			ID   string          `json:"id"`
			Data json.RawMessage `json:"data"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(canvas), &raw); err != nil {
		t.Fatalf("解析 canvas: %v", err)
	}
	for _, n := range raw.Nodes {
		if n.ID == id {
			return n.Data
		}
	}
	return nil
}
