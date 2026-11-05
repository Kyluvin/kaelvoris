package tools

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
)

// 媒体载荷协议：generate_image / generate_video / generate_speech / 浏览器截图把媒体
// 以「磁盘路径 + data URL（可空）+ MIME」包在 <<<KAELVORIS_MEDIA:...:KAELVORIS_MEDIA>>>
// 标记里追加到工具结果尾部（kind: image | video | audio 统一通道）。
//
// 引擎 execTool 在工具返回后立即 ExtractMediaPayload：
//   - 剥离标记后的纯文本回填 LLM 上下文（base64 永不进模型窗口，防撑爆）；
//   - 媒体转成 ClineMessage{Say: image|video|audio} 推送聊天流：
//     实时推送前端用 DataURL 直接展示；会话历史持久化只留 Path（详情见
//     app.go makeSayBridge），前端回放时按路径经 /kaelvoris-image/、/kaelvoris-media/
//     路由回挂。
//
// 历史注：图片曾走独立的 KAELVORIS_IMAGES 通道，2026-12 统一并入本通道（kind=image），
// 旧会话历史里的 text 载荷 [{path,fileName}] 前端解析向后兼容。

// MustDataURL 从原始图片字节构造 data URL（嗅探 MIME）。
func MustDataURL(raw []byte) string {
	return "data:" + DetectImageMIME(raw) + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// MarshalMediaMsgText 把媒体载荷列表（含 kind=image）编码为 say 消息的 text 载荷
// （仅磁盘路径 + 文件名 + MIME + kind；dataUrl 不重复存，实时推送走消息 Images 字段，
// 历史落盘时剥离，见 app.go makeSayBridge）。
func MarshalMediaMsgText(medias []MediaPayload) string {
	type item struct {
		Kind     string `json:"kind,omitempty"`
		Path     string `json:"path"`
		FileName string `json:"fileName,omitempty"`
		MIME     string `json:"mime,omitempty"`
	}
	items := make([]item, 0, len(medias))
	for _, md := range medias {
		items = append(items, item{Kind: md.Kind, Path: md.Path, FileName: md.FileName, MIME: md.MIME})
	}
	b, err := json.Marshal(items)
	if err != nil {
		return ""
	}
	return string(b)
}

// DetectImageMIME 嗅探图片 MIME（默认 png）。
func DetectImageMIME(raw []byte) string {
	n := len(raw)
	if n > 512 {
		n = 512
	}
	switch http.DetectContentType(raw[:n]) {
	case "image/jpeg":
		return "image/jpeg"
	case "image/webp":
		return "image/webp"
	case "image/gif":
		return "image/gif"
	default:
		return "image/png"
	}
}

// ImageExt 由 MIME 推断文件扩展名（默认 png）。
func ImageExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return "png"
	}
}

// ── 媒体载荷协议（视频/音频，generate_video / generate_speech 共用）──────
//
// 与图片 KAELVORIS_IMAGES 同构：工具把媒体以「磁盘路径 + data URL（可空）+ MIME」
// 包在 <<<KAELVORIS_MEDIA:...:KAELVORIS_MEDIA>>> 标记里追加到工具结果尾部。
// 引擎 execTool 剥离标记：纯文本回填 LLM，媒体转 SayVideo / SayAudio 消息
// 推送聊天流（dataUrl 实时直播；历史只留 path，回放经 /kaelvoris-media/ 回挂）。
const (
	MediaPayloadOpen  = "\n<<<KAELVORIS_MEDIA:"
	MediaPayloadClose = ":KAELVORIS_MEDIA>>>"
)

// MediaPayload 单个生成媒体载荷（kind: image | video | audio）。
type MediaPayload struct {
	Kind     string `json:"kind"`               // video | audio（image 走 KAELVORIS_IMAGES 旧标记）
	Path     string `json:"path"`               // 磁盘绝对路径（会话历史/回放回挂用）
	DataURL  string `json:"dataUrl,omitempty"`  // data:...;base64,...（实时展示用；视频为空）
	FileName string `json:"fileName,omitempty"` // 文件名（卡片展示）
	MIME     string `json:"mime,omitempty"`     // video/mp4 / audio/mpeg …（卡片播放器用）
}

// BuildMediaPayload 构造内嵌媒体载荷段（工具侧；引擎侧只读）。
func BuildMediaPayload(medias []MediaPayload) string {
	if len(medias) == 0 {
		return ""
	}
	b, err := json.Marshal(medias)
	if err != nil {
		return ""
	}
	return MediaPayloadOpen + string(b) + MediaPayloadClose
}

// ExtractMediaPayload 从工具结果中剥离内嵌媒体载荷标记。
// 返回（剥离后的纯文本, 媒体载荷列表）；无标记时 medias 为 nil。
func ExtractMediaPayload(result string) (string, []MediaPayload) {
	i := strings.Index(result, MediaPayloadOpen)
	if i < 0 {
		return result, nil
	}
	head := strings.TrimRight(result[:i], "\n")
	rest := result[i+len(MediaPayloadOpen):]
	j := strings.Index(rest, MediaPayloadClose)
	if j < 0 {
		return head, nil // 标记不完整（截断场景）：丢弃尾部载荷段
	}
	payload := rest[:j]
	var medias []MediaPayload
	if err := json.Unmarshal([]byte(payload), &medias); err != nil {
		medias = nil
	}
	return head, medias
}

// ImagePayload 图片构造侧载荷：磁盘绝对路径 + data URL（工具保存图片后先组装此结构，
// 经 imgsToMedia 转为统一 MediaPayload（kind=image）推送）。历史沿用名，保留。
type ImagePayload struct {
	Path     string `json:"path"`     // 磁盘绝对路径（会话历史/回放回挂用）
	DataURL  string `json:"dataUrl"`  // data:image/...;base64,...（实时展示用）
	FileName string `json:"fileName"` // 文件名（卡片展示）
}

// imgsToMedia ImagePayload 列表转 MediaPayload（图片复用统一媒体通道推送）。
func imgsToMedia(imgs []ImagePayload) []MediaPayload {
	out := make([]MediaPayload, 0, len(imgs))
	for _, im := range imgs {
		out = append(out, MediaPayload{
			Kind: "image", Path: im.Path, DataURL: im.DataURL, FileName: im.FileName,
		})
	}
	return out
}

// DetectAudioMIME 嗅探音频 MIME（默认 audio/mpeg）。
func DetectAudioMIME(raw []byte) string {
	n := len(raw)
	if n > 512 {
		n = 512
	}
	switch http.DetectContentType(raw[:n]) {
	case "audio/wave", "audio/x-wav":
		return "audio/wav"
	case "audio/ogg":
		return "audio/ogg"
	case "audio/mpeg":
		return "audio/mpeg"
	case "audio/flac":
		return "audio/flac"
	default:
		// ID3 / MP3 帧头嗅探（http.DetectContentType 对裸 mp3 可能落 octet-stream）。
		if n >= 3 && raw[0] == 'I' && raw[1] == 'D' && raw[2] == '3' {
			return "audio/mpeg"
		}
		if n >= 2 && raw[0] == 0xFF && raw[1]&0xE0 == 0xE0 {
			return "audio/mpeg"
		}
		return "audio/mpeg"
	}
}

// AudioExt 音频 MIME → 文件扩展名（默认 mp3）。
func AudioExt(mime string) string {
	switch mime {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/ogg", "application/ogg":
		return "ogg"
	case "audio/opus":
		return "opus"
	case "audio/flac":
		return "flac"
	case "audio/aac":
		return "aac"
	case "audio/mp4", "audio/m4a":
		return "m4a"
	default:
		return "mp3"
	}
}

// AudioMIMEFromExt 扩展名 → 音频 MIME（显式 format 下发时同步请求头用）。
func AudioMIMEFromExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "wav":
		return "audio/wav"
	case "ogg":
		return "audio/ogg"
	case "opus":
		return "audio/opus"
	case "flac":
		return "audio/flac"
	case "aac":
		return "audio/aac"
	case "m4a":
		return "audio/mp4"
	default:
		return "audio/mpeg"
	}
}

// MediaExtFromMIME 视频 Content-Type → 文件扩展名（未知 = 空，默认 mp4）。
func MediaExtFromMIME(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	switch mime {
	case "video/webm":
		return "webm"
	case "video/ogg":
		return "ogv"
	case "video/quicktime":
		return "mov"
	case "video/x-matroska":
		return "mkv"
	case "video/x-msvideo":
		return "avi"
	default:
		return "" // video/mp4 及未知类型都默认 mp4
	}
}

// MediaMIMEFromName 文件名 → MIME（前端播放器类型提示用）。
func MediaMIMEFromName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".webm":
		return "video/webm"
	case ".ogv":
		return "video/ogg"
	case ".mov":
		return "video/quicktime"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	default:
		return "video/mp4"
	}
}
