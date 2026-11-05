/*
 * Copyright 2025 coze-dev Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package fileutil

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/coze-dev/coze-studio/backend/pkg/logs"
)

func GetPythonFilePath(fileName string) string {
	cwd, err := os.Getwd()
	if err != nil {
		logs.Warnf("[GetPythonFilePath] Failed to get current working directory: %v", err)
		return fileName
	}

	return filepath.Join(cwd, fileName)
}

// GetPython3Path 返回 Python3 解释器路径。
//
// Kaelvoris 融合改造（2026-10）：
//   - 优先读环境变量 COZE_PYTHON（Kaelvoris 侧可显式指定解释器）；
//   - venv 路径仅在存在时使用（Windows 下无 .venv/bin 结构，检查后回落）；
//   - Windows 系统解释器命令名为 "python"（无 "python3"），按运行时系统分流。
func GetPython3Path() string {
	if p := os.Getenv("COZE_PYTHON"); p != "" {
		return p
	}
	cwd, err := os.Getwd()
	if err != nil {
		logs.Warnf("[GetPythonFilePath] Failed to get current working directory: %v", err)
		return pythonFallbackName()
	}

	filePath := filepath.Join(cwd, ".venv/bin/python3")
	if _, err := os.Stat(filePath); err == nil {
		return filePath
	}

	return pythonFallbackName() // use system python
}

// pythonFallbackName 返回系统 Python 命令名：Windows 为 python，其余为 python3。
func pythonFallbackName() string {
	if runtime.GOOS == "windows" {
		return "python"
	}
	return "python3"
}
