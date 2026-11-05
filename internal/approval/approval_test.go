package approval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDecideUnsetFallsBack(t *testing.T) {
	var m Matrix // nil 矩阵 = 旧逻辑
	if allow, forced := m.Decide("shell", true); !allow || forced {
		t.Fatalf("nil matrix autoApprove=true: want (true,false), got (%v,%v)", allow, forced)
	}
	if allow, forced := m.Decide("shell", false); allow || forced {
		t.Fatalf("nil matrix autoApprove=false: want (false,false), got (%v,%v)", allow, forced)
	}
	// 空 map 同义
	m = Matrix{}
	if allow, _ := m.Decide("shell", true); !allow {
		t.Fatal("empty matrix should fall back to autoApprove=true")
	}
}

func TestDecideThreeStates(t *testing.T) {
	m := Matrix{"shell": ActionDeny, "fs_write": ActionAllow, "browser": ActionAsk}
	// deny：无论 AutoApprove 与否都拒绝
	if allow, forced := m.Decide("shell", true); allow || forced {
		t.Fatalf("deny: want (false,false), got (%v,%v)", allow, forced)
	}
	// allow：无论 AutoApprove 与否都放行
	if allow, forced := m.Decide("fs_write", false); !allow || forced {
		t.Fatalf("allow: want (true,false), got (%v,%v)", allow, forced)
	}
	// ask：强制弹卡（绕过 AutoApprove）
	if allow, forced := m.Decide("browser", true); allow || !forced {
		t.Fatalf("ask: want (false,true), got (%v,%v)", allow, forced)
	}
	// 未列入的工具走旧逻辑
	if allow, forced := m.Decide("fs_read", false); allow || forced {
		t.Fatalf("unset tool: want (false,false), got (%v,%v)", allow, forced)
	}
}

func TestLoadMissingAndBroken(t *testing.T) {
	dir := t.TempDir()
	if m := Load(filepath.Join(dir, "absent.json")); m != nil {
		t.Fatalf("missing file: want nil, got %v", m)
	}
	p := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := Load(p); m != nil {
		t.Fatalf("broken file: want nil, got %v", m)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "approvals.json")
	if err := Save(p, Matrix{"shell": ActionAsk, "fs_write": ActionAllow}); err != nil {
		t.Fatal(err)
	}
	m := Load(p)
	if m["shell"] != ActionAsk || m["fs_write"] != ActionAllow || len(m) != 2 {
		t.Fatalf("roundtrip: got %v", m)
	}
}

func TestSaveMergePreservesOtherEntries(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "approvals.json")
	if err := Save(p, Matrix{"shell": ActionAsk}); err != nil {
		t.Fatal(err)
	}
	// 第二次只写 fs_write=allow，shell 规则必须保留（防覆盖）。
	if err := Save(p, Matrix{"fs_write": ActionAllow}); err != nil {
		t.Fatal(err)
	}
	m := Load(p)
	if m["shell"] != ActionAsk || m["fs_write"] != ActionAllow {
		t.Fatalf("merge: want shell=ask + fs_write=allow, got %v", m)
	}
	// 删除动作：置空删除该键
	if err := Save(p, Matrix{"shell": ""}); err != nil {
		t.Fatal(err)
	}
	m = Load(p)
	if _, ok := m["shell"]; ok {
		t.Fatalf("delete action: shell should be removed, got %v", m)
	}
}

func TestSaveEmptyRemovesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "approvals.json")
	if err := Save(p, Matrix{"shell": ActionDeny}); err != nil {
		t.Fatal(err)
	}
	if err := Save(p, Matrix{"shell": ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("empty matrix should remove file, stat err=%v", err)
	}
	if m := Load(p); m != nil {
		t.Fatalf("after removal Load should be nil, got %v", m)
	}
}

func TestValidAndString(t *testing.T) {
	for _, s := range []string{"ask", "allow", "deny", " ALLOW "} {
		if !Valid(s) {
			t.Fatalf("Valid(%q)=false", s)
		}
	}
	if Valid("nope") {
		t.Fatal(`Valid("nope")=true`)
	}
	if a, err := String("Deny"); err != nil || a != ActionDeny {
		t.Fatalf("String(Deny): (%v,%v)", a, err)
	}
	if _, err := String("nope"); err == nil {
		t.Fatal("String(nope) should error")
	}
}

// ---- D2 参数级规则（第三批对标 Gemini CLI policy engine）----

func TestDecideExRules(t *testing.T) {
	s := &Store{
		Matrix: Matrix{"shell": ActionAllow},
		Rules: []Rule{
			{ID: "r1", Tool: "shell", Pattern: "git status", Action: ActionAllow, Priority: 0},
			{ID: "r2", Tool: "shell", Pattern: "rm ", Action: ActionDeny, Priority: 5},
			{ID: "r3", Tool: "fs_write", Action: ActionAsk}, // 不限参数
		},
	}
	// pattern 命中 allow 直通（无规则时矩阵也是 allow，但走 rules 分支 decided=true）
	if allow, forced, decided := s.DecideEx("shell", `{"command":"git status --porcelain"}`, false); !allow || forced || !decided {
		t.Fatalf("pattern allow: want (true,false,true), got (%v,%v,%v)", allow, forced, decided)
	}
	// pattern 命中 deny（priority 5 > 0）
	if allow, forced, decided := s.DecideEx("shell", `{"command":"rm -rf x"}`, true); allow || forced || !decided {
		t.Fatalf("pattern deny: want (false,false,true), got (%v,%v,%v)", allow, forced, decided)
	}
	// pattern 未命中 → 回退矩阵（shell=allow）
	if allow, forced, decided := s.DecideEx("shell", `{"command":"ls -la"}`, false); !allow || forced || !decided {
		t.Fatalf("fallback matrix: want (true,false,true), got (%v,%v,%v)", allow, forced, decided)
	}
	// 无 pattern 规则命中任意参数 → ask 强制弹卡
	if allow, forced, decided := s.DecideEx("fs_write", `{"path":"a.txt","content":"x"}`, true); allow || !forced || !decided {
		t.Fatalf("no-pattern ask: want (false,true,true), got (%v,%v,%v)", allow, forced, decided)
	}
	// 全部未命中 → decided=false 走 AutoApprove
	if _, _, decided := s.DecideEx("fs_read", `{"path":"a.txt"}`, true); decided {
		t.Fatal("no hit: decided should be false")
	}
	// 同分取数组靠后：两条同 priority 命中，后一条生效
	s2 := &Store{Rules: []Rule{
		{Tool: "shell", Action: ActionAllow},
		{Tool: "shell", Action: ActionDeny},
	}}
	if allow, _, decided := s2.DecideEx("shell", `{}`, false); allow || !decided {
		t.Fatalf("tie last wins: want deny (false,_,true), got (%v,_,%v)", allow, decided)
	}
	// nil store 全部未命中
	var nilStore *Store
	if _, _, decided := nilStore.DecideEx("shell", `{}`, true); decided {
		t.Fatal("nil store: decided should be false")
	}
}

func TestLoadStoreCompat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "approvals.json")
	// 旧形态照读
	if err := os.WriteFile(p, []byte(`{"shell":"ask","fs_write":"allow"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := LoadStore(p)
	if s == nil || s.Matrix["shell"] != ActionAsk || s.Matrix["fs_write"] != ActionAllow || len(s.Rules) != 0 {
		t.Fatalf("old form: got %+v", s)
	}
	// 新形态：rules + 矩阵键并存
	if err := os.WriteFile(p, []byte(`{"rules":[{"id":"r1","tool":"shell","pattern":"git ","action":"allow","priority":3}],"browser":"deny"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s = LoadStore(p)
	if s == nil || len(s.Rules) != 1 || s.Rules[0].Priority != 3 || s.Matrix["browser"] != ActionDeny {
		t.Fatalf("new form: got %+v", s)
	}
	// 非法正则/非法 action/空 tool 条目丢弃
	if err := os.WriteFile(p, []byte(`{"rules":[{"tool":"shell","pattern":"([","action":"allow"},{"tool":"fs_write","action":"nope"},{"tool":"","action":"allow"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if s = LoadStore(p); s != nil {
		t.Fatalf("invalid entries should all drop: got %+v", s)
	}
}

func TestSaveStoreMerge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "approvals.json")
	// 先写旧形态矩阵
	if err := Save(p, Matrix{"shell": ActionAsk}); err != nil {
		t.Fatal(err)
	}
	// SaveStore 写规则：旧矩阵键必须保留（防覆盖丢失）
	if err := SaveStore(p, &Store{Rules: []Rule{{ID: "r1", Tool: "shell", Pattern: "git ", Action: ActionAllow}}}); err != nil {
		t.Fatal(err)
	}
	s := LoadStore(p)
	if s == nil || s.Matrix["shell"] != ActionAsk || len(s.Rules) != 1 || s.Rules[0].ID != "r1" {
		t.Fatalf("merge old matrix: got %+v", s)
	}
	// 同 ID 替换（pattern 更新）
	if err := SaveStore(p, &Store{Rules: []Rule{{ID: "r1", Tool: "shell", Pattern: "git push", Action: ActionDeny}}}); err != nil {
		t.Fatal(err)
	}
	s = LoadStore(p)
	if len(s.Rules) != 1 || s.Rules[0].Pattern != "git push" || s.Rules[0].Action != ActionDeny {
		t.Fatalf("same-id replace: got %+v", s.Rules)
	}
	// 旧 Save 写矩阵：规则键原样保留
	if err := Save(p, Matrix{"fs_write": ActionAllow}); err != nil {
		t.Fatal(err)
	}
	s = LoadStore(p)
	if s == nil || s.Matrix["fs_write"] != ActionAllow || len(s.Rules) != 1 {
		t.Fatalf("Save preserves rules: got %+v", s)
	}
	// 删除规则（ID + 空 action = 删除语义）+ 清空矩阵（全部键）→ 全空 → 删除文件
	if err := SaveStore(p, &Store{
		Rules:  []Rule{{ID: "r1", Action: ""}},
		Matrix: Matrix{"shell": "", "fs_write": ""},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("all cleared should remove file, stat err=%v", err)
	}
}

func TestLoadLegacyStillWorks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "approvals.json")
	if err := Save(p, Matrix{"shell": ActionAsk, "fs_write": ActionAllow}); err != nil {
		t.Fatal(err)
	}
	m := Load(p)
	if m["shell"] != ActionAsk || m["fs_write"] != ActionAllow || len(m) != 2 {
		t.Fatalf("legacy Load roundtrip: got %v", m)
	}
}
