// aux_model_test.go —— E2/E3 模型分流单测：auxClient 优先级三态（辅助配置
// 齐 → 辅助模型 / 缺任一 → 主模型 / primary nil → 全局配置构造）、
// fallbackClient 启用条件（三项齐备且与主模型不同；缺一/同配置 → nil）。
// App 依赖最小化：GetConfig 直读结构体（cfgMu 锁），无 Wails runtime 依赖。
package desktop

import (
	"testing"

	"kaelvoris/internal/llm"
)

func TestAuxClientFallsBackToPrimary(t *testing.T) {
	a := &App{} // 零配置：aux 三项全空
	primary := llm.NewClient("http://primary", "k1", "main-model")
	got := a.auxClient(primary)
	if got != primary {
		t.Fatalf("empty aux config must return primary, got %+v", got)
	}
}

func TestAuxClientUsesAuxWhenComplete(t *testing.T) {
	a := &App{cfg: Config{
		BaseURL: "http://primary", APIKey: "k1", Model: "main-model",
		AuxBaseURL: "http://aux", AuxAPIKey: "k2", AuxModel: "aux-model",
	}}
	got := a.auxClient(nil)
	if got.BaseURL != "http://aux" || got.Model != "aux-model" {
		t.Fatalf("complete aux config must use aux, got %+v", got)
	}
}

func TestAuxClientPartialAuxFallsBack(t *testing.T) {
	a := &App{cfg: Config{
		BaseURL: "http://primary", APIKey: "k1", Model: "main-model",
		AuxBaseURL: "http://aux", AuxModel: "aux-model", // 缺 AuxAPIKey
	}}
	primary := llm.NewClient("http://primary", "k1", "main-model")
	got := a.auxClient(primary)
	if got != primary {
		t.Fatalf("partial aux config must fall back to primary, got %+v", got)
	}
}

func TestFallbackClientDisabled(t *testing.T) {
	a := &App{cfg: Config{BaseURL: "http://p", APIKey: "k", Model: "m"}}
	if fb := a.fallbackClient(nil); fb != nil {
		t.Fatal("empty fallback config must return nil")
	}
	// 缺一项。
	a.cfg = Config{FallbackBaseURL: "http://f", FallbackModel: "fm"} // 缺 key
	if fb := a.fallbackClient(nil); fb != nil {
		t.Fatal("partial fallback config must return nil")
	}
}

func TestFallbackClientSameAsPrimary(t *testing.T) {
	a := &App{cfg: Config{
		BaseURL: "http://same", APIKey: "k", Model: "m",
		FallbackBaseURL: "http://same", FallbackAPIKey: "k", FallbackModel: "m",
	}}
	primary := llm.NewClient("http://same", "k", "m")
	if fb := a.fallbackClient(primary); fb != nil {
		t.Fatal("fallback identical to primary must return nil (degradation meaningless)")
	}
}

func TestFallbackClientEnabled(t *testing.T) {
	a := &App{cfg: Config{
		BaseURL: "http://p", APIKey: "k", Model: "m",
		FallbackBaseURL: "http://f", FallbackAPIKey: "kf", FallbackModel: "fm",
	}}
	fb := a.fallbackClient(nil)
	if fb == nil {
		t.Fatal("complete distinct fallback config must be enabled")
	}
	if fb.BaseURL != "http://f" || fb.Model != "fm" {
		t.Fatalf("fallback client wrong: %+v", fb)
	}
}
