// Package tools 语音转文字工具：speech_to_text —— OpenAI /audio/transcriptions 兼容。
//
// 端点：POST {base}/audio/transcriptions（OpenAI 兼容），multipart/form-data：
//   - file：音频文件（mp3/wav/m4a/mp4/mpeg/mpga/oga/ogg/webm/opus 等）
//   - model：whisper-1 / gpt-4o-mini-transcribe 等
//   - language（可选）：提示语种（如 zh）
//   - response_format：json（默认，返回 {text}）
//
// 配置复用 Speech 设置页（BaseURL/Model/APIKey 与 TTS 同一组端点配置）。
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SpeechToTextTool 语音转文字工具。
type SpeechToTextTool struct {
	// Settings 提供当前 STT 配置（复用 SpeechSettings；nil 或配置不全 = 报错引导）。
	Settings func() SpeechSettings
}

func (t *SpeechToTextTool) Name() string { return "speech_to_text" }

func (t *SpeechToTextTool) Description() string {
	return "Convert an audio file to text (speech-to-text / transcription). " +
		"Use when the user provides a voice memo, meeting recording, audio clip, " +
		"or asks to transcribe 语音转文字 / 音频转写. " +
		"Supported formats: mp3, wav, m4a, mp4, mpeg, mpga, oga, ogg, webm, opus, flac, aac. " +
		"If STT is not configured, tell the user to configure 语音合成/转写 Base URL and model in 设置 → 模型."
}

func (t *SpeechToTextTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"path":     strProp("Path to the audio file (absolute or workspace-relative)"),
		"language": strProp(`Spoken language hint, e.g. "zh" / "en" (optional; ISO-639-1)`),
		"prompt":   strProp("Optional context hint to improve accuracy (e.g. domain terms)"),
	}, []string{"path"})
}

func (t *SpeechToTextTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Path     string `json:"path"`
		Language string `json:"language"`
		Prompt   string `json:"prompt"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	if strings.TrimSpace(p.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	if t.Settings == nil {
		return "", fmt.Errorf("speech-to-text is not configured")
	}
	s := t.Settings()
	if strings.TrimSpace(s.BaseURL) == "" {
		return "", fmt.Errorf("speech-to-text is not configured: set 转写 Base URL in 设置 → 模型")
	}
	model := strings.TrimSpace(s.Model)
	if model == "" {
		model = "whisper-1"
	}

	// 读取音频文件。
	audio, err := os.ReadFile(p.Path)
	if err != nil {
		return "", fmt.Errorf("read audio: %w", err)
	}
	if len(audio) > 256<<20 {
		return "", fmt.Errorf("audio too large (max 256MB)")
	}

	// multipart 表单。
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	// file 字段（文件名 + MIME 用扩展名推断）。
	fw, err := mw.CreateFormFile("file", filepath.Base(p.Path))
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(audio); err != nil {
		return "", err
	}
	_ = mw.WriteField("model", model)
	if p.Language != "" {
		_ = mw.WriteField("language", p.Language)
	}
	if p.Prompt != "" {
		_ = mw.WriteField("prompt", p.Prompt)
	}
	_ = mw.WriteField("response_format", "json")
	if err := mw.Close(); err != nil {
		return "", err
	}

	url := strings.TrimRight(s.BaseURL, "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if s.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
	}
	// 长音频转写可超 60s，独立长超时兜底。
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("STT request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("STT API %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), clipStr(string(raw), 600))
	}

	// 响应 {text: "..."}；兼容部分网关 {transcript} 或纯文本。
	var out struct {
		Text       string `json:"text"`
		Transcript string `json:"transcript"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && (out.Text != "" || out.Transcript != "") {
		if out.Text != "" {
			return out.Text, nil
		}
		return out.Transcript, nil
	}
	// 纯文本兜底。
	txt := strings.TrimSpace(string(raw))
	if txt == "" {
		return "", fmt.Errorf("STT API returned empty result")
	}
	return txt, nil
}
