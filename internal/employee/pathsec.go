package employee

// pathsec.go 路径安全辅助：目录白名单前缀匹配（§7.2 目录路径规则）。
// Windows 文件系统大小写不敏感，全部比较统一转小写。

import (
	"path/filepath"
	"strings"
)

// hasDirPrefix 判断 abs 是否位于 dir 之内（含相等时的调用方自查）。
// 统一转小写 + 补分隔符，防 "D:\data-evil" 误匹配 "D:\data" 前缀。
func hasDirPrefix(abs, dir string) bool {
	sep := string(filepath.Separator)
	dir = filepath.Clean(dir)
	if !strings.HasSuffix(dir, sep) {
		dir += sep
	}
	a := strings.ToLower(abs)
	d := strings.ToLower(dir)
	return strings.HasPrefix(a+sep, d)
}

// sameDir 大小写不敏感的目录相等判断。
func sameDir(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// normalizeDir 规范化用户配置的目录项（§7.2：正斜杠/反斜杠统一、去尾部分隔符）。
func normalizeDir(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return ""
	}
	return filepath.Clean(d)
}

// normalizeWorkspaceRoot 规范化自定义工作空间根（§3）：去首尾空白与引号
// （Windows 资源管理器「复制文件地址」产生 "D:\dir" 带引号形式），并 Clean。
func normalizeWorkspaceRoot(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "\"'")
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}
