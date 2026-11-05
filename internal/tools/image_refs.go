package tools

import (
	"strings"
)

// IsDataURL 判断字符串是否为 data URL。
func IsDataURL(s string) bool {
	return strings.HasPrefix(s, "data:")
}
