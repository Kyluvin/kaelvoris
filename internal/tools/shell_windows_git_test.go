// shell_windows_git_test.go —— Windows 纯 git 命令 argv 化修复的单测。
//
// 背景（2026-09-27 复盘）：cmd /C 转发整行命令时引号被 cmd.exe 剥离，
// git commit -m "中文 信息" 报 pathspec 错误。修复后纯 git 命令走
// argv 参数化直跑；本文件验证切分与判定逻辑的边界。
package tools

import (
	"reflect"
	"testing"
)

func TestSplitGitTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"简单", `git status`, []string{"git", "status"}},
		{"带C路径", `git -C D:\repo commit -m "fix: 修复登录"`,
			[]string{"git", "-C", `D:\repo`, "commit", "-m", "fix: 修复登录"}},
		{"中文含空格", `git commit -m "feat(remote): 远程中心增强（任务过滤）"`,
			[]string{"git", "commit", "-m", "feat(remote): 远程中心增强（任务过滤）"}},
		{"单引号字面量", `git commit -m 'hello world'`,
			[]string{"git", "commit", "-m", "hello world"}},
		{"单引号相邻拼接", `git commit -m 'it''s ok'`,
			[]string{"git", "commit", "-m", "its ok"}},
		{"双写引号转义", `git commit -m "say ""hi"" now"`,
			[]string{"git", "commit", "-m", `say "hi" now`}},
		{"token 内反斜杠转义引号", `git commit -m "a\"b"`,
			[]string{"git", "commit", "-m", `a"b`}},
		{"token 外转义引号产出字面量", `git commit -m \"abc\"`,
			[]string{"git", "commit", "-m", `"abc"`}},
		{"空参数保留", `git commit -m ""`,
			[]string{"git", "commit", "-m", ""}},
		{"多空格", `git    log   -1`, []string{"git", "log", "-1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitGitTokens(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("splitGitTokens(%q) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}

func TestSplitGitTokensUnclosed(t *testing.T) {
	// 未闭合引号必须返回 nil（走原 cmd /C 路径，不误吞参数）。
	for _, in := range []string{`git commit -m "abc`, `git commit -m 'abc`} {
		if got := splitGitTokens(in); got != nil {
			t.Fatalf("splitGitTokens(%q) = %#v, want nil", in, got)
		}
	}
}

func TestIsPureGitCommand(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"纯git提交", `git commit -m "fix: x"`,
			[]string{"git", "commit", "-m", "fix: x"}},
		{"带路径exe", `git.exe status`, []string{"git.exe", "status"}},
		{"大小写", `GIT log -1`, []string{"GIT", "log", "-1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isPureGitCommand(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("isPureGitCommand(%q) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}

	// 这些必须走原 cmd /C 路径（含 shell 语法 / 非 git / 边界）。
	nonGit := []string{
		`git commit -m "x" && echo done`, // 命令拼接
		`git log | head`,                 // 管道
		`echo hi > f.txt && git add .`,   // 重定向
		`cd repo && git status`,          // cd 前缀
		`go test ./...`,                  // 非 git
		`git`,                            // 光杆 git（无子命令）
		`git help commit`,                // help 走原路径
		`powershell -Command "git st"`,   // 非 git 首词
		`git -C D:\repo commit -m "a" ^   // 转义符`, // 含 ^ 转义字符
		"git commit -m \"x\"\nnext",               // 含换行
	}
	for _, in := range nonGit {
		if got := isPureGitCommand(in); got != nil {
			t.Fatalf("isPureGitCommand(%q) = %#v, want nil（应走原 cmd /C 路径）", in, got)
		}
	}
}
