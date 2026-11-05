package main

import (
	"os"
	"testing"
)

// TestValidateStartup 覆盖集中校验的各类缺失/非法组合（规划板 rm_94698f59e404）。
func TestValidateStartup(t *testing.T) {
	cases := []struct {
		name      string
		data      string
		addr      string
		apiUser   string
		apiPass   string
		noAPIAuth bool
		grace     int
		limit     int
		wantN     int // 期望错误条数（0 = 通过）
	}{
		{"合法", "", ":8900", "", "", false, 150, 512, 0},
		{"addr非法-缺冒号", "", "8900", "", "", false, 150, 512, 1},
		{"addr端口超界", "", ":99999", "", "", false, 150, 512, 1},
		{"api-user缺pass", "", ":8900", "admin", "", false, 150, 512, 1},
		{"api-pass缺user", "", ":8900", "", "secret", false, 150, 512, 1},
		{"no-api-auth时单侧OK", "", ":8900", "admin", "", true, 150, 512, 0},
		{"node-grace负", "", ":8900", "", "", false, -1, 512, 1},
		{"store-limit负", "", ":8900", "", "", false, 150, -1, 1},
		{"多重缺失聚合", "", "bad", "admin", "", false, -5, -1, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := validateStartup(c.data, c.addr, c.apiUser, c.apiPass, c.noAPIAuth, c.grace, c.limit)
			if len(errs) != c.wantN {
				t.Fatalf("errs = %d, want %d: %v", len(errs), c.wantN, errs)
			}
		})
	}
}

// TestHomeEnvOK 主目录环境变量判定（Windows 仅认 USERPROFILE/HOME，与
// os.UserHomeDir 实测行为 go1.27.1 一致；HOMEDRIVE+HOMEPATH 组合不生效）。
func TestHomeEnvOK(t *testing.T) {
	oldU, oldH, oldD, oldP := os.Getenv("USERPROFILE"), os.Getenv("HOME"), os.Getenv("HOMEDRIVE"), os.Getenv("HOMEPATH")
	os.Unsetenv("USERPROFILE")
	os.Unsetenv("HOME")
	os.Unsetenv("HOMEDRIVE")
	os.Unsetenv("HOMEPATH")
	defer func() {
		os.Setenv("USERPROFILE", oldU)
		os.Setenv("HOME", oldH)
		os.Setenv("HOMEDRIVE", oldD)
		os.Setenv("HOMEPATH", oldP)
	}()

	if homeEnvOK() {
		t.Fatal("无 USERPROFILE/HOME 时不应通过")
	}
	os.Setenv("USERPROFILE", "C:\\Users\\test")
	if !homeEnvOK() {
		t.Fatal("USERPROFILE 存在时应通过")
	}
	// HOMEDRIVE+HOMEPATH 存在但 USERPROFILE/HOME 缺失：os.UserHomeDir 会 panic
	//（%userprofile% is not defined），校验必须拦截。
	os.Unsetenv("USERPROFILE")
	os.Setenv("HOMEDRIVE", "C:")
	os.Setenv("HOMEPATH", "\\Users\\test")
	if homeEnvOK() {
		t.Fatal("HOMEDRIVE+HOMEPATH 不应放行（UserHomeDir 实测 panic）")
	}
}
