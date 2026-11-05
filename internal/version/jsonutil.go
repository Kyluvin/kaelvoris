package version

import "encoding/json"

// json 辅助（隔离 encoding/json 依赖，便于统一替换/加防御）。

func MarshalIndentCompat(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func UnmarshalCompat(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
