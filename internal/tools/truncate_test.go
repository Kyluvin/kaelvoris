package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// truncateMiddle 单测（mini-swe-agent observation_template 对标）：
// 短文本原样返回；超长文本头尾保留 + 中段省略说明 + 可操作指令；
// UTF-8 多字节字符截点不产生残字节。
func TestTruncateMiddleShort(t *testing.T) {
	s := "hello world"
	if got := truncateMiddle(s, 100); got != s {
		t.Fatalf("short text must pass through: got %q", got)
	}
}

func TestTruncateMiddleHeadTail(t *testing.T) {
	// 30 字节 ASCII，limit=20 → 头 10 + 尾 10，中段省略 10。
	s := "0123456789ABCDEFGHIJKLMN" // 24 bytes
	got := truncateMiddle(s, 20)
	if len(s) <= 20 {
		t.Fatalf("test setup: input should exceed limit")
	}
	if !strings.Contains(got, "012345") || !strings.Contains(got, "KLMN") {
		t.Fatalf("head and tail must be preserved: %q", got)
	}
	if !strings.Contains(got, "characters elided") {
		t.Fatalf("elided marker missing: %q", got)
	}
	if !strings.Contains(got, "more selective command") {
		t.Fatalf("actionable instruction missing: %q", got)
	}
	// 头尾不得重叠（limit 20 < len 24 时 head+tail=20，elided=4）。
	if !strings.Contains(got, "[4 characters elided]") {
		t.Fatalf("elided count wrong: %q", got)
	}
}

func TestTruncateMiddleUTF8Boundary(t *testing.T) {
	// 中文每字 3 字节；limit 落在字符中间时不得出现乱码残字节。
	s := strings.Repeat("汉", 100) // 300 bytes
	got := truncateMiddle(s, 50)
	if !utf8.ValidString(got) {
		t.Fatalf("broken UTF-8 in truncated output: %q", got)
	}
	if !strings.Contains(got, "characters elided") {
		t.Fatalf("elided marker missing: %q", got)
	}
}
