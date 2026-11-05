// JSON 小工具（nodes_logic.go / nodes_input.go 共用；标准库封装）。
package bisheng

import "encoding/json"

// toJSONStr any → JSON 串。
func toJSONStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// marshalJSON 别名（语义注释用）。
func marshalJSON(v any) ([]byte, error) { return json.Marshal(v) }

// unmarshalJSON 别名。
func unmarshalJSON(b []byte, v any) error { return json.Unmarshal(b, v) }

// sscanfInt 简单整数解析（避免引 fmt.Sscanf 的格式串歧义）。
func sscanfInt(s string, n *int) (int, error) {
	v := 0
	ok := false
	for _, c := range s {
		if c < '0' || c > '9' {
			if ok {
				break
			}
			continue
		}
		v = v*10 + int(c-'0')
		ok = true
	}
	if !ok {
		return 0, errNotInt
	}
	*n = v
	return v, nil
}

var errNotInt = jsonErr("not int")

// jsonErr 占位错误类型（避免引 errors 包名冲突，标准 errors 即可但保持文件专注）。
type jsonErr string

func (e jsonErr) Error() string { return string(e) }
