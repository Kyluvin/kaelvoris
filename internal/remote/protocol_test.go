package remote

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	e := NewEnvelope("req-1", TypeCreateTask, CreateTask{NodeID: "n1", Text: "修复编译错误"})
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Envelope
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != TypeCreateTask || got.ID != "req-1" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	var ct CreateTask
	if err := got.Decode(&ct); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ct.Text != "修复编译错误" || ct.NodeID != "n1" {
		t.Fatalf("payload mismatch: %+v", ct)
	}
}

func TestEnvelopeEmptyPayload(t *testing.T) {
	e := NewEnvelope("", TypeListNodes, nil)
	if len(e.Payload) != 0 {
		t.Fatalf("empty payload should stay empty, got %s", e.Payload)
	}
	var out map[string]string
	if err := e.Decode(&out); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
}

func TestVersionConstants(t *testing.T) {
	// 消息类型常量不冲突且格式稳定（<方向>.<动作> 或单段动作）。
	seen := map[string]bool{}
	types := []string{
		TypeAgentRegister, TypeAgentHeartbeat, TypeAgentTaskEvent, TypeAgentTaskDone, TypeAgentMessages,
		TypeServerRegisterOK, TypeServerTaskDispatch, TypeServerTaskCancel, TypeServerChat,
		TypeRoadmapList, TypeRoadmapPlan, TypeRoadmapDev, TypeRoadmapDelete, TypeRoadmap,
		TypeServerRoadmapList, TypeServerRoadmapPlan, TypeServerRoadmapDev, TypeServerRoadmapDelete,
		TypeAgentRoadmapData,
		TypeServerAskReply, TypeServerGetMessages,
		TypeAuth, TypeListNodes, TypeListTasks, TypeCreateTask, TypeChatSend, TypeTaskCancel,
		TypeGetMessages, TypeAskReply, TypeAuthOK, TypeAuthErr, TypeNodes, TypeTasks,
		TypeTaskCreated, TypeTaskEvent, TypeTaskResult, TypeMessages, TypeOpResult, TypeServerError,
		// A2A 群聊（group.*）
		TypeGroupCreate, TypeGroupList, TypeGroupMsg, TypeGroupTask, TypeGroupJoin, TypeGroupLeave, TypeGroupSync,
		TypeGroups, TypeGroupCreated, TypeGroupMessage, TypeGroupState, TypeGroupTaskDone,
		TypeServerGroupCreate, TypeServerGroupMsg, TypeServerGroupTask, TypeServerGroupJoin, TypeServerGroupLeave, TypeServerGroupSync,
		TypeAgentGroupState, TypeAgentGroupMsg, TypeAgentGroupDone,
	}
	for _, typ := range types {
		if seen[typ] {
			t.Fatalf("duplicate type: %s", typ)
		}
		seen[typ] = true
	}
}

func TestTaskStatusConstants(t *testing.T) {
	statuses := []string{TaskStatusPending, TaskStatusRunning, TaskStatusDone, TaskStatusFailed, TaskStatusCanceled}
	seen := map[string]bool{}
	for _, s := range statuses {
		if seen[s] {
			t.Fatalf("duplicate status: %s", s)
		}
		seen[s] = true
	}
}

func TestNewNodeID(t *testing.T) {
	a, b := NewNodeID(), NewNodeID()
	if a == b {
		t.Fatalf("node IDs should differ: %s", a)
	}
	if len(a) < 8 {
		t.Fatalf("node ID too short: %s", a)
	}
}

// TestNewIDUniqueSameTick 回归：Windows 墙钟 tick 粒度可达 ~15ms，同一 tick 内
// 连续调用 newID 曾生成相同 ID（仅时间戳+pid），导致并发创建的任务/会话互相覆盖
// （TestE2E_DeleteTaskRecord 偶现两任务同 ID）。现 newID 含进程内原子计数器 +
// 随机后缀，进程内必须严格唯一。
func TestNewIDUniqueSameTick(t *testing.T) {
	const n = 1000
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ { // 并发生成，覆盖同 tick 多 goroutine 场景
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n/8; j++ {
				ids <- newID("t")
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("newID 重复: %s", id)
		}
		seen[id] = true
	}
}

// TestGroupTaskResultsRoundTrip GroupTask 扩展字段（Results + AssignedTo 逗号
// 语义）序列化往返；旧 JSON（无 results）解析应为空切片（向后兼容）。
func TestGroupTaskResultsRoundTrip(t *testing.T) {
	task := GroupTask{
		ID: "gt-1", GroupID: "g1", Instruction: "写介绍", AssignedTo: "emp-writer,emp-analyst",
		Status: "done", Result: "汇总", TS: 1,
		Results: []GroupStepResult{
			{EmployeeID: "emp-writer", Name: "写作专员", Result: "产出A"},
			{EmployeeID: "emp-analyst", Name: "数据分析师", Result: "产出B"},
		},
	}
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got GroupTask
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AssignedTo != "emp-writer,emp-analyst" || len(got.Results) != 2 {
		t.Fatalf("roundtrip 丢失: %+v", got)
	}
	if got.Results[0].EmployeeID != "emp-writer" || got.Results[1].Result != "产出B" {
		t.Fatalf("Results 内容异常: %+v", got.Results)
	}
	// 向后兼容：旧版载荷无 results 字段 → 解析为空切片（不报错）。
	legacy := `{"id":"gt-2","groupId":"g1","instruction":"x","assignedTo":"emp-writer","status":"done","result":"r","ts":2}`
	var old GroupTask
	if err := json.Unmarshal([]byte(legacy), &old); err != nil {
		t.Fatalf("旧载荷解析失败: %v", err)
	}
	if old.AssignedTo != "emp-writer" || len(old.Results) != 0 {
		t.Fatalf("旧载荷语义异常: %+v", old)
	}
}
