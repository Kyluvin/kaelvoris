package version

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.2.0", "1.2.1", -1},
		{"1.10.0", "1.9.0", 1},
		{"2.0", "1.9.9", 1},
		{"1.2", "1.2.0", 0}, // 段数不同短侧补 0
		{"v1.2.0", "1.2.0", 0},
		{"V2.0.0", "1.9", 1},
		{"1.0.0-rc1", "1.0.0", 0}, // 预发布元数据忽略（简化语义）
		{"0.6.0", "0.5.9", 1},
		{"", "0.0.1", -1},
		{"abc", "0.0.1", -1}, // 非数字段取 0
	}
	for _, c := range cases {
		got := Compare(c.a, c.b)
		if got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	if !IsNewer("0.6.0", "0.7.0") {
		t.Error("0.7.0 should be newer than 0.6.0")
	}
	if IsNewer("1.0.0", "1.0.0") {
		t.Error("same version should not be newer")
	}
	if IsNewer("1.1", "1.0.9") {
		t.Error("1.0.9 should not be newer than 1.1")
	}
}

func TestParseManifestAndCheck(t *testing.T) {
	dir := t.TempDir()
	manifest := `{
		"version": "9.9.9",
		"url": "kaelvoris-desktop.exe",
		"notes": ["new feature A", "fix B"],
		"date": "2025-06-01"
	}`
	mp := filepath.Join(dir, "update.json")
	if err := os.WriteFile(mp, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Check(mp)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Update {
		t.Error("expected update available (9.9.9 > current)")
	}
	if res.Latest != "9.9.9" {
		t.Errorf("Latest = %q", res.Latest)
	}
	if len(res.Notes) != 2 {
		t.Errorf("Notes = %v", res.Notes)
	}
	// 相对 URL 应基于清单目录解析。
	want := filepath.ToSlash(filepath.Join(filepath.ToSlash(dir), "kaelvoris-desktop.exe"))
	if filepath.ToSlash(res.AssetURL) != want {
		t.Errorf("AssetURL = %q, want %q", res.AssetURL, want)
	}
}

func TestCheckManifestMissingVersion(t *testing.T) {
	dir := t.TempDir()
	mp := filepath.Join(dir, "update.json")
	_ = os.WriteFile(mp, []byte(`{"url":"x.exe"}`), 0o644)
	if _, err := Check(mp); err == nil {
		t.Error("expected error for manifest without version")
	}
}

func TestResolveAssetURL(t *testing.T) {
	if got := ResolveAssetURL("http://h/d/update.json", ""); got != "http://h/d/kaelvoris-desktop.exe" {
		t.Errorf("ResolveAssetURL http default = %q", got)
	}
	if got := ResolveAssetURL("http://h/d/update.json", "files/app.exe"); got != "http://h/d/files/app.exe" {
		t.Errorf("ResolveAssetURL http relative = %q", got)
	}
	if got := ResolveAssetURL("http://h/d/update.json", "http://cdn/x.exe"); got != "http://cdn/x.exe" {
		t.Errorf("ResolveAssetURL absolute passthrough = %q", got)
	}
	got := ResolveAssetURL(`D:\srv\update.json`, "")
	if filepath.ToSlash(got) != "D:/srv/kaelvoris-desktop.exe" {
		t.Errorf("ResolveAssetURL local = %q", got)
	}
}

func TestNotesForAndLatestEntry(t *testing.T) {
	if NotesFor("not-exist") != nil {
		t.Error("NotesFor unknown should be nil")
	}
	latest := LatestEntry()
	if len(NotesFor(latest.Version)) == 0 {
		t.Error("NotesFor(latest) should return notes")
	}
	if NotesFor("v"+latest.Version) == nil {
		t.Error("NotesFor should tolerate v prefix")
	}
}

func TestValidateExecutable(t *testing.T) {
	// 合法 PE（MZ 头 + 足够体积）应通过。
	ok := filepath.Join(t.TempDir(), "ok.exe")
	body := append([]byte("MZ"), make([]byte, 9<<20)...)
	if err := os.WriteFile(ok, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExecutable(ok, 0, ""); err != nil {
		t.Errorf("valid PE should pass: %v", err)
	}
	// 体积门禁：低于默认 8MB 拒绝。
	small := filepath.Join(t.TempDir(), "small.exe")
	_ = os.WriteFile(small, []byte("MZ"), 0o644)
	if err := ValidateExecutable(small, 0, ""); err == nil {
		t.Error("undersized file should be rejected")
	}
	// 非法 PE 头：文本文件冒充 exe 拒绝。
	fake := filepath.Join(t.TempDir(), "fake.exe")
	_ = os.WriteFile(fake, append([]byte("MZ"), make([]byte, 9<<20)...), 0o644)
	_ = os.WriteFile(fake, []byte("<html>502 Bad Gateway</html>"), 0o644)
	if err := ValidateExecutable(fake, 1<<20, ""); err == nil {
		// 体积过门禁（1MB）但非 PE：必须拒绝。
		bigFake := filepath.Join(t.TempDir(), "bigfake.exe")
		_ = os.WriteFile(bigFake, append([]byte("<html>not pe</html>"), make([]byte, 9<<20)...), 0o644)
		if err := ValidateExecutable(bigFake, 0, ""); err == nil {
			t.Error("non-PE file should be rejected")
		}
	}
	// SHA256 校验：哈希不匹配必须拒绝。
	sum, err := Sha256File(ok)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExecutable(ok, 0, sum); err != nil {
		t.Errorf("matching sha256 should pass: %v", err)
	}
	if err := ValidateExecutable(ok, 0, "deadbeef"); err == nil {
		t.Error("mismatched sha256 should be rejected")
	}
}

func TestValidateExecutableRejectsTextFile(t *testing.T) {
	// HTML 错误页冒充安装包（CDN 5xx 落盘场景）必须在门禁拦截。
	p := filepath.Join(t.TempDir(), "errpage.exe")
	content := []byte("<!DOCTYPE html><html><body>404 not found</body></html>")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExecutable(p, 1, ""); err == nil {
		t.Error("text file should be rejected even with minBytes=1")
	}
}

func TestPruneBackups(t *testing.T) {
	dir := t.TempDir()
	bk := filepath.Join(dir, backupDirName)
	if err := os.MkdirAll(bk, 0o755); err != nil {
		t.Fatal(err)
	}
	// 建 5 份备份，修改时间依次递增。
	for i := 0; i < 5; i++ {
		p := filepath.Join(bk, fmt.Sprintf("go_%d.exe", i))
		if err := os.WriteFile(p, []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
	}
	PruneBackups(dir, 3)
	remain, err := os.ReadDir(bk)
	if err != nil {
		t.Fatal(err)
	}
	if len(remain) != 3 {
		t.Fatalf("after prune want 3 backups, got %d", len(remain))
	}
	// 必须保留最新的 3 份（go_2/3/4），删掉最旧的 go_0/go_1。
	for _, e := range remain {
		if e.Name() == "go_0.exe" || e.Name() == "go_1.exe" {
			t.Errorf("oldest backup %s should be pruned", e.Name())
		}
	}
}

func TestFailedNoticeRoundTrip(t *testing.T) {
	if _, err := FailedMarkerPath(); err != nil {
		t.Skip("marker path unavailable:", err)
	}
	defer ClearFailedNotice()
	if msg := ReadFailedNotice(); msg != "" {
		t.Fatalf("initial failed notice should be empty, got %q", msg)
	}
	// 直接写文件模拟脚本回滚记录。
	path, _ := FailedMarkerPath()
	_ = os.WriteFile(path, []byte("2026-10-02 10:00:00 自升级失败，已自动回滚到旧版\r\n"), 0o644)
	if msg := ReadFailedNotice(); msg == "" {
		t.Error("ReadFailedNotice should return written content")
	}
	ClearFailedNotice()
	if msg := ReadFailedNotice(); msg != "" {
		t.Errorf("after clear should be empty, got %q", msg)
	}
}

func TestPendingMarkerRoundTrip(t *testing.T) {
	// MarkerPath 基于当前进程 exe 目录；测试进程在临时构建目录，可读写。
	path, err := MarkerPath()
	if err != nil {
		t.Skip("marker path unavailable:", err)
	}
	defer os.Remove(path)
	p := PendingUpgrade{Target: "1.2.3", Staged: filepath.Join(filepath.Dir(path), "update-staged.exe"), Notes: "a\nb"}
	if err := WritePending(p); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	got := ReadPending()
	if got == nil || got.Target != "1.2.3" {
		t.Fatalf("ReadPending = %+v", got)
	}
	ClearPending()
	if ReadPending() != nil {
		t.Error("ClearPending should remove marker")
	}
}
