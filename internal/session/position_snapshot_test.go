package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Phase 1-C（doc/09-设计方案/岗位分配与双区权限设计方案.md §3/§6.1）：
// SessionData/Session/Summary 增加岗位分配快照字段 PositionID + AssignRule，
// 快照防漂移（员工调岗后会话岗位不跟随）；旧会话 JSON（无新字段）加载兼容。

// TestSessionPositionSnapshotJSONRoundTrip 新字段序列化/反序列化往返：
// SessionData 含 positionId/assignRule 时 JSON 编解码无损。
func TestSessionPositionSnapshotJSONRoundTrip(t *testing.T) {
	d := SessionData{
		ID:         "s1",
		ChannelID:  "lark",
		EmployeeID: "emp-zhang",
		PositionID: "pos-dev-lead",
		AssignRule: "round_robin",
	}
	b, err := json.Marshal(&d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back SessionData
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.PositionID != "pos-dev-lead" || back.AssignRule != "round_robin" || back.EmployeeID != "emp-zhang" {
		t.Fatalf("round trip lost fields: %+v", back)
	}
}

// TestSessionPositionSnapshotLegacyJSONCompatible 旧会话 JSON 兼容加载：
// 存量会话文件无 positionId/assignRule 字段（零值 = 无岗位快照，语义即普通会话），
// Unmarshal 不得报错，其他字段原样保留。
func TestSessionPositionSnapshotLegacyJSONCompatible(t *testing.T) {
	legacy := `{
		"id": "old-1",
		"channelId": "desktop",
		"employeeId": "emp-li",
		"messages": [],
		"createdAt": "2026-01-01T00:00:00Z",
		"updatedAt": "2026-01-01T00:00:00Z"
	}`
	var d SessionData
	if err := json.Unmarshal([]byte(legacy), &d); err != nil {
		t.Fatalf("legacy json should unmarshal without error, got: %v", err)
	}
	if d.PositionID != "" || d.AssignRule != "" {
		t.Fatalf("legacy session must have empty snapshot, got pos=%q rule=%q", d.PositionID, d.AssignRule)
	}
	if d.ID != "old-1" || d.EmployeeID != "emp-li" {
		t.Fatalf("legacy fields lost: %+v", d)
	}
}

// TestSessionPositionSnapshotPersistsThroughStore 快照字段随会话持久化往返：
// Session → Save → flush → Store.Get（从盘加载）→ 快照字段不丢（§2.1：解析
// 结果固化到会话，会话生命周期内不重选——依赖持久化往返保真）。
func TestSessionPositionSnapshotPersistsThroughStore(t *testing.T) {
	// 同一目录新开 Store（空缓存），Get 必走磁盘加载，真正验证持久化往返。
	dir := filepath.Join(t.TempDir(), "sessions")
	st := NewStore(dir)
	s := &Session{ID: "pos-1", ChannelID: "desktop"}
	s.SetEmployeeID("emp-wang")
	s.SetPositionSnapshot("pos-qa", "random")
	st.Save(s)
	st.FlushAll()
	st.Close()

	st2 := NewStore(dir)
	defer st2.Close()
	got, err := st2.Get("pos-1")
	if err != nil {
		t.Fatalf("reload from disk: %v", err)
	}
	if got.EmployeeID != "emp-wang" || got.PositionID != "pos-qa" || got.AssignRule != "random" {
		t.Fatalf("snapshot not persisted: emp=%q pos=%q rule=%q", got.EmployeeID, got.PositionID, got.AssignRule)
	}

	// 落盘文件直读校验：JSON 键名按设计方案协议（positionId/assignRule）。
	b, err := os.ReadFile(filepath.Join(dir, "pos-1.json"))
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("session file invalid json: %v", err)
	}
	if raw["positionId"] != "pos-qa" || raw["assignRule"] != "random" {
		t.Fatalf("file keys wrong: positionId=%v assignRule=%v", raw["positionId"], raw["assignRule"])
	}
}

// TestSummaryExportsPositionSnapshot Summary 列表条目导出快照字段
// （设计方案 §5.2 资源管理面板会话 Tab「岗位 / 员工」列数据源）。
func TestSummaryExportsPositionSnapshot(t *testing.T) {
	s := &Session{ID: "s2", PositionID: "pos-ops", AssignRule: "least_loaded", EmployeeID: "emp-li"}
	sum := s.Summary()
	if sum.PositionID != "pos-ops" || sum.AssignRule != "least_loaded" {
		t.Fatalf("summary lost snapshot: %+v", sum)
	}
	// Summary JSON 协议键名（前端消费）。
	sb, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var sm map[string]any
	if err := json.Unmarshal(sb, &sm); err != nil {
		t.Fatalf("summary json: %v", err)
	}
	if sm["positionId"] != "pos-ops" || sm["assignRule"] != "least_loaded" {
		t.Fatalf("summary json keys wrong: %+v", sm)
	}
}

// TestSetPositionSnapshotSetAndClear SetPositionSnapshot 写入/清空（空对 =
// 解除岗位快照，会话退回普通会话语义）。
func TestSetPositionSnapshotSetAndClear(t *testing.T) {
	s := &Session{ID: "s3"}
	s.SetPositionSnapshot("pos-dev", "round_robin")
	if s.PositionID != "pos-dev" || s.AssignRule != "round_robin" {
		t.Fatalf("set failed: %+v", s)
	}
	s.SetPositionSnapshot("", "")
	if s.PositionID != "" || s.AssignRule != "" {
		t.Fatalf("clear failed: %+v", s)
	}
}
