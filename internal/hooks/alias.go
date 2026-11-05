package hooks

import (
	"path/filepath"
	"regexp"
)

// regexpT 别名（测试文件引用；保持 hooks.go 不额外暴露标准库类型细节）。
type regexpT = regexp.Regexp

// filepathJoin 路径拼接薄封装。
func filepathJoin(elems ...string) string { return filepath.Join(elems...) }
