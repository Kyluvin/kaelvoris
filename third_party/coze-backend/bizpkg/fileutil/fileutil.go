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
	"encoding/json"
	"os"

	"github.com/cloudwego/eino/components/prompt"
	"github.com/cloudwego/eino/schema"

	"github.com/coze-dev/coze-studio/backend/pkg/logs"
)

// GetWorkingDirectory 返回 coze 静态资源（resources/conf/...）的根目录。
//
// Kaelvoris 融合改造（2026-10）：原实现为 os.Getwd()（Docker 里工作目录固定 /app），
// 融合形态下 Kaelvoris 桌面进程 CWD 不可控且 shell 工具依赖原 CWD，故改为：
// 优先读环境变量 COZE_RESOURCE_DIR（由 internal/cozeflow bootstrap 启动时释放
// 嵌入资源并注入），未设置时回落原 os.Getwd() 行为（保持 coze 独立部署可用）。
func GetWorkingDirectory() string {
	if dir := os.Getenv("COZE_RESOURCE_DIR"); dir != "" {
		return dir
	}
	root, err := os.Getwd()
	if err != nil {
		logs.Warnf("[InitConfig] Failed to get current working directory: %v", err)
		root = os.Getenv("PWD")
	}
	return root
}

func ReadJinja2PromptTemplate(jsonFilePath string) (prompt.ChatTemplate, error) {
	b, err := os.ReadFile(jsonFilePath)
	if err != nil {
		return nil, err
	}
	var m2qMessages []*schema.Message
	if err = json.Unmarshal(b, &m2qMessages); err != nil {
		return nil, err
	}
	tpl := make([]schema.MessagesTemplate, len(m2qMessages))
	for i := range m2qMessages {
		tpl[i] = m2qMessages[i]
	}
	return prompt.FromMessages(schema.Jinja2, tpl...), nil
}
