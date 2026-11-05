// model_profiles_test.go —— 模型配置条目化（同一类型多模型 + 启停开关）测试：
// 覆盖投影 applyModelProfiles（每类第一个未停用条目 → 平铺字段）、旧配置迁移
// migrateFlatToProfiles（平铺字段收敛为单条目）与 SaveConfig 的条目裁剪校验。
package desktop

import (
	"strings"
	"testing"

	"kaelvoris/internal/task"
)

// TestApplyModelProfiles 投影规则：每类取第一个未停用条目；条目全停用回落平铺原值。
func TestApplyModelProfiles(t *testing.T) {
	c := Config{
		BaseURL: "http://flat", APIKey: "k-flat", Model: "flat-model",
		ImageGenBaseURL: "http://iflat", ImageGenModel: "iflat-model", ImageGenAPIKey: "ik",
	}
	c.ModelProfiles = []ModelProfile{
		{Kind: "chat", BaseURL: "http://a", Model: "a-model", APIKey: "ka", Disabled: true},
		{Kind: "chat", BaseURL: "http://b", Model: "b-model", APIKey: "kb"},
		{Kind: "image", BaseURL: "http://img", Model: "img-model", APIKey: "ki"},
	}
	applyModelProfiles(&c)
	if c.BaseURL != "http://b" || c.Model != "b-model" || c.APIKey != "kb" {
		t.Fatalf("chat should project first enabled profile, got %s/%s", c.BaseURL, c.Model)
	}
	if c.ImageGenBaseURL != "http://img" || c.ImageGenModel != "img-model" || c.ImageGenAPIKey != "ki" {
		t.Fatalf("image should project enabled profile, got %s/%s", c.ImageGenBaseURL, c.ImageGenModel)
	}
	if c.VideoGenBaseURL != "" || c.SpeechBaseURL != "" {
		t.Fatal("kinds without profiles must keep flat fields untouched")
	}

	// 全停用：回落平铺原值（零破坏）
	c2 := Config{BaseURL: "http://flat", Model: "flat-model"}
	c2.ModelProfiles = []ModelProfile{{Kind: "chat", BaseURL: "http://x", Model: "x", Disabled: true}}
	applyModelProfiles(&c2)
	if c2.BaseURL != "http://flat" || c2.Model != "flat-model" {
		t.Fatalf("all-disabled profiles must fall back to flat fields, got %s/%s", c2.BaseURL, c2.Model)
	}
}

// TestMigrateFlatToProfiles 旧配置一次性收敛：平铺字段 → 单条目；已有 profiles 不动。
func TestMigrateFlatToProfiles(t *testing.T) {
	c := Config{BaseURL: "http://m", APIKey: "km", Model: "m-model", ImageGenBaseURL: "http://i", ImageGenModel: "i-model"}
	migrateFlatToProfiles(&c)
	if len(c.ModelProfiles) != 2 {
		t.Fatalf("want 2 migrated profiles (chat+image), got %d", len(c.ModelProfiles))
	}
	if c.ModelProfiles[0].Kind != "chat" || c.ModelProfiles[0].BaseURL != "http://m" || c.ModelProfiles[0].Model != "m-model" || c.ModelProfiles[0].APIKey != "km" {
		t.Fatalf("chat migration mismatch: %+v", c.ModelProfiles[0])
	}
	if c.ModelProfiles[1].Kind != "image" || c.ModelProfiles[1].BaseURL != "http://i" || c.ModelProfiles[1].APIKey != "" {
		t.Fatalf("image migration mismatch: %+v", c.ModelProfiles[1])
	}

	// 已有条目：不迁移（幂等）
	c2 := Config{BaseURL: "http://m", ModelProfiles: []ModelProfile{{Kind: "chat", BaseURL: "http://x", Model: "x"}}}
	migrateFlatToProfiles(&c2)
	if len(c2.ModelProfiles) != 1 || c2.ModelProfiles[0].BaseURL != "http://x" {
		t.Fatal("existing profiles must not be re-migrated")
	}
}

// TestSaveConfigProfileTrimming SaveConfig 条目裁剪：非法 kind 丢弃、字段 TrimSpace、
// 投影到平铺字段；空 baseURL 校验仍生效。
func TestSaveConfigProfileTrimming(t *testing.T) {
	// SaveConfig 落盘真实 ConfigPath()（~/.kaelvoris/desktop.json），必须把
	// UserHomeDir 重定向到临时目录，否则测试假值会覆盖用户真实配置
	//（2026-10-25 事故：http://trim 假值写入 desktop.json 致 LLM 链路 dial tcp: lookup trim 失败）。
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)                // Windows UserHomeDir 来源
	t.Setenv("HOME", dir)                       // Unix 兜底
	a := &App{scheduler: task.NewScheduler(10)} // SaveConfig 内 SetMax 需非 nil 调度器
	c := Config{
		BaseURL: "http://keep", MaxParallel: 10,
		ModelProfiles: []ModelProfile{
			{Kind: "bogus", BaseURL: "http://bad", Model: "bad"},
			{Kind: "chat", BaseURL: " http://trim ", Model: " trim-model ", APIKey: " k "},
			{Kind: "speech", BaseURL: "http://s", Model: "s-model"},
		},
	}
	if err := a.SaveConfig(c); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got := a.GetConfig()
	if len(got.ModelProfiles) != 2 {
		t.Fatalf("bogus kind must be dropped, got %d profiles", len(got.ModelProfiles))
	}
	p := got.ModelProfiles[0]
	if p.BaseURL != "http://trim" || p.Model != "trim-model" || p.APIKey != "k" {
		t.Fatalf("fields must be trimmed: %+v", p)
	}
	if got.BaseURL != "http://trim" || got.Model != "trim-model" {
		t.Fatalf("projection mismatch: %s/%s", got.BaseURL, got.Model)
	}
	if got.SpeechBaseURL != "http://s" || got.SpeechModel != "s-model" {
		t.Fatalf("speech projection mismatch: %s/%s", got.SpeechBaseURL, got.SpeechModel)
	}

	// 空 chat 端点（投影后平铺字段为空）→ 校验报错
	empty := Config{BaseURL: "http://flat", ModelProfiles: []ModelProfile{{Kind: "chat", BaseURL: " ", Model: "m"}}}
	if err := a.SaveConfig(empty); err == nil || !strings.Contains(err.Error(), "Base URL") {
		t.Fatalf("empty projected baseURL must be rejected, got %v", err)
	}
}
