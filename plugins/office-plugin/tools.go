// 工具定义与 JSON Schema 辅助。
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Tool 单个 MCP 工具定义。
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
	Execute     func(args json.RawMessage) (string, error)
}

// bindArgs 解析参数到结构体（宽容模式：未知字段忽略，缺省字段零值）。
func bindArgs(raw json.RawMessage, v interface{}) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// ---- 领域错误码（对齐 dsh-univer-office 的稳定 code 恢复语义） ----

// errKind 领域错误分类（稳定 code，模型可见）。
type errKind string

const (
	errInvalidRequest  errKind = "INVALID_REQUEST"
	errFileNotFound    errKind = "FILE_NOT_FOUND"
	errFileExists      errKind = "FILE_EXISTS"
	errScopeDenied     errKind = "SESSION_SCOPE_DENIED"
	errWorktreeState   errKind = "WORKTREE_STATE"
	errUnitNotFound    errKind = "UNIT_NOT_FOUND"
	errUnitTypeInvalid errKind = "UNIT_TYPE_INVALID"
	errGatewayNo       errKind = "GATEWAY_UNAVAILABLE"
	errContentBad      errKind = "CONTENT_INVALID"
)

// domainError 携带稳定 code 的领域错误。
type domainError struct {
	Code errKind
	Msg  string
}

func (e *domainError) Error() string { return e.Msg }

func derr(code errKind, format string, args ...interface{}) error {
	return &domainError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// isDomainError 判断是否为领域错误。
func isDomainError(err error) (*domainError, bool) {
	de, ok := err.(*domainError)
	return de, ok
}

// errCode 提取错误 code（非领域错误统一 CONTRACT_ERROR）。
func errCode(err error) string {
	if de, ok := isDomainError(err); ok {
		return string(de.Code)
	}
	return "CONTRACT_ERROR"
}

// ---- JSON Schema 辅助 ----

// objectSchema 构造 object 类型 schema（properties + required）。
func objectSchema(props map[string]interface{}, required []string) map[string]interface{} {
	s := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// strProp 字符串参数描述。
func strProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

// strEnum 枚举字符串参数。
func strEnum(desc string, values ...string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc, "enum": values}
}

// strPropReq 必填字符串参数。
func strPropReq(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

// joinLines 多行文本拼接（工具结果呈现）。
func joinLines(parts ...string) string {
	return strings.TrimSpace(strings.Join(parts, "\n"))
}
