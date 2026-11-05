package desktop

import (
	"path/filepath"
	"strings"
	"testing"

	"kaelvoris/internal/promptconfig"
)

// TestReviewExtraPrompt 规划板审核补充判据兜底链：用户显式指定优先，
// 空时回退 promptconfig roadmapReview 段，再空 = 空。
func TestReviewExtraPrompt(t *testing.T) {
	a := &App{promptStore: promptconfig.New(filepath.Join(t.TempDir(), "prompts.json"))}
	// 均空 → 空
	if got := a.reviewExtraPrompt(""); got != "" {
		t.Errorf("empty fallback = %q, want empty", got)
	}
	// 配置段非空 → 回退配置
	a.promptStore.Save(promptconfig.Config{RoadmapReview: "严控粒度"})
	if got := a.reviewExtraPrompt(""); !strings.Contains(got, "严控粒度") {
		t.Errorf("config fallback = %q, want roadmapReview segment", got)
	}
	// 用户显式指定优先
	if got := a.reviewExtraPrompt(" 人工指定 "); got != "人工指定" {
		t.Errorf("explicit = %q, want trimmed user prompt", got)
	}
}
