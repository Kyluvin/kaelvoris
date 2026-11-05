package remote

// 服务端生成图片托管（绘画链路服务端侧）：
//
//	PC 节点 generate_image 产出图片后经 POST /api/images（multipart 或裸二进制）
//	上传到服务端 → 服务端落 images 目录并返回相对 URL（/img/<file>）→
//	节点发 agent.image 事件 → 服务端回填 Task.Images（AttachTaskImages）并广播
//	task.event(kind=image) → 移动端聊天流/绘画 tab 直接用 URL 展示。
//
// 鉴权：
//   - 上传：节点令牌（X-Node-Token，与 /ws/agent 注册同一张 agentTokens 表）或
//     REST 凭据（restAuth 语义）——上传方只有本系统节点/账号，防公网匿名写。
//   - 读取：/img/ 公开只读（移动端 H5 <img> 直挂 URL，与 /m/ 静态页同信任级别；
//     文件名由服务端生成（时间戳+随机+白名单扩展名），无路径穿越面）。

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 图片上传上限（OpenAI 兼容生图单图通常 1~8MB；放宽到 32MB 兜底 4 图请求）。
const maxImageUploadBytes = 32 << 20

// 媒体上传上限（generate_video 产出的 MP4 常见 5~50MB；256MB 兜底长片段）。
const maxMediaUploadBytes = 256 << 20

// allowedImageExt 上传文件白名单扩展名（generate_image 落盘只会是这几种）。
var allowedImageExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true,
}

// allowedMediaExt 媒体上传白名单扩展名（generate_video / generate_speech 落盘）。
var allowedMediaExt = map[string]bool{
	".mp4": true, ".webm": true, ".mov": true, ".mkv": true, ".avi": true, ".ogv": true,
	".mp3": true, ".wav": true, ".ogg": true, ".opus": true, ".flac": true, ".aac": true, ".m4a": true,
}

// mediaMimeByExt 媒体扩展名 → MIME（/img/ 托管下发 Content-Type 用）。
var mediaMimeByExt = map[string]string{
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	".mkv": "video/x-matroska", ".avi": "video/x-msvideo", ".ogv": "video/ogg",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".opus": "audio/ogg",
	".flac": "audio/flac", ".aac": "audio/aac", ".m4a": "audio/mp4",
}

// SetImageDir 注入图片存储目录（服务端部署目录下 images/；NewServer 后、Start 前调用）。
// 空串 = 关闭上传（/api/images 返回 404）。
func (s *Server) SetImageDir(dir string) {
	s.mu.Lock()
	s.imageDir = dir
	s.mu.Unlock()
}

// SetAgentTokens 注入节点令牌表（agentToken → nodeID；上传鉴权用）。
// 与 hub 的 tokens 同源，服务端 main.go 装配时传入。
func (s *Server) SetAgentTokens(tokens map[string]string) {
	s.mu.Lock()
	s.agentTokens = tokens
	s.mu.Unlock()
}

// imageDir 图片存储目录（空 = /api/images 404）。
// agentTokens 节点令牌快照（每请求动态读取，accounts.json 变更后生效）。

// imageAuth 图片上传鉴权：节点令牌（X-Node-Token 头，匹配任一 agentToken）或
// 回落 restAuth（Basic 统一账号 / clientToken）。任一通过即放行。
func (s *Server) imageAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		tokens := s.agentTokens
		s.mu.Unlock()
		// agentTokens 形态为 token → nodeID（与 /ws/agent 注册同源），鉴权比对键（令牌）。
		tok := r.Header.Get("X-Node-Token")
		if tok != "" {
			for t := range tokens {
				if t != "" && tok == t {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		s.restAuth(next).ServeHTTP(w, r) // 回落 REST 凭据（本地部署/账号直传场景）
	})
}

// registerImageRoutes 挂载图片/媒体上传与托管路由（NewServer 内调用；统一经 s.route）。
func (s *Server) registerImageRoutes(mux *http.ServeMux, hub *Hub) {
	s.route(mux, "/api/images", s.handleImageUpload, s.imageAuth)
	// /api/media 节点上传视频/音频（generate_video / generate_speech 链路）：
	// 落盘同一 images 目录、/img/<file> URL 托管（同白名单安全文件名策略）。
	s.route(mux, "/api/media", s.handleMediaUpload, s.imageAuth)
	// /img/<file> 公开只读托管（服务端生成的安全文件名，无目录穿越面；图片 + 音视频）。
	s.route(mux, "/img/", func(w http.ResponseWriter, r *http.Request) {
		s.serveImage(w, r, hub)
	})
	// /api/file-url 换取短期签名 URL（鉴权 Basic/clientToken；见 handleFileURL）。
	s.route(mux, "/api/file-url", s.handleFileURL)

	// /api/attachments DELETE 清理移动端已取消/移除的附件暂存（防孤儿附件）：
	// 上传会话未发送即取消（XHR abort 后服务端可能已落盘），客户端逆向清理。
	s.route(mux, "/api/attachments", s.handleAttachmentDelete, s.imageAuth)
}

// handleAttachmentDelete DELETE /api/attachments?f=<file>&sid=<sessionID>：删除未发送的附件暂存。
// 移动端上传成功但用户在发送前取消/移除 chip 时调用（防孤儿附件）；
// 仅允许删除有归属记录且 sid 匹配的文件（无归属不删除，防误删他端文件）。
func (s *Server) handleAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 DELETE")
		return
	}
	s.mu.Lock()
	dir := s.imageDir
	s.mu.Unlock()
	if dir == "" {
		writeJSONError(w, http.StatusNotFound, "附件存储未启用")
		return
	}
	f := r.URL.Query().Get("f")
	sid := r.URL.Query().Get("sid")
	// 安全校验：文件名仅白名单扩展名 + 无路径分隔（防穿越）；sid 必填（归属锚点）。
	if f == "" || strings.ContainsAny(f, `/\\`) || strings.Contains(f, "..") {
		writeJSONError(w, http.StatusBadRequest, "参数无效")
		return
	}
	ext := strings.ToLower(filepath.Ext(f))
	if !allowedImageExt[ext] && !allowedMediaExt[ext] {
		writeJSONError(w, http.StatusBadRequest, "文件类型不允许")
		return
	}
	if sid == "" {
		writeJSONError(w, http.StatusBadRequest, "缺少会话标识")
		return
	}
	// 归属校验：有归属记录时 sid 必须匹配（禁止跨会话删除）；无归属记录（旧文件）拒绝删除。
	if m, ok := s.hub.LookupFileMeta(f); ok {
		if m.SessionID != "" && m.SessionID != sid {
			writeJSONError(w, http.StatusForbidden, "无权删除该附件")
			return
		}
	} else {
		writeJSONError(w, http.StatusNotFound, "附件不存在")
		return
	}
	if err := os.Remove(filepath.Join(dir, f)); err != nil && !os.IsNotExist(err) {
		writeJSONError(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	s.hub.DropFileMeta(f)
	slog.Info("附件暂存已删除（取消/移除清理）", "file", f, "session", sid)
	writeJSONOK(w, map[string]interface{}{"ok": true})
}

// handleFileURL GET /api/file-url?f=<file>&sid=<sessionID>：换取短期签名下载 URL。
// 鉴权（ImageDocAuth 语义，与知识库读一致）：Basic 任意有效账号 / clientToken
// （Bearer/X-Access-Token/?token=）。签名绑定换取者账号 + 归属会话，/img/ 侧校验。
func (s *Server) handleFileURL(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	token := s.restToken
	s.mu.Unlock()
	// 鉴权：clientToken 或统一账号 Basic（复用 docAuth 的账号表校验语义）。
	if tok := docToken(r); tok != "" && token != "" &&
		subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1 {
		// clientToken 换取：共享令牌即完整凭据（docAuth 同语义）。
	} else if u, p, ok := r.BasicAuth(); ok && u != "" && p != "" {
		if _, _, ok2 := s.hub.AuthClient(u, p, ""); !ok2 {
			writeJSONError(w, http.StatusUnauthorized, "账号或密码错误")
			return
		}
	} else {
		writeJSONError(w, http.StatusUnauthorized, "需要登录凭据（Basic 认证）或访问令牌")
		return
	}
	f := r.URL.Query().Get("f")
	sid := r.URL.Query().Get("sid")
	if f == "" || strings.ContainsAny(f, `/\\`) || strings.Contains(f, "..") {
		writeJSONError(w, http.StatusBadRequest, "参数无效")
		return
	}
	url := s.hub.SignFileURL(f, sid)
	if url == "" {
		writeJSONError(w, http.StatusBadRequest, "参数无效")
		return
	}
	writeJSONOK(w, map[string]interface{}{"url": url, "exp": time.Now().Add(fileURLTTL).UnixMilli()})
}

// nodeIDForToken 上传请求反查节点 ID（归属记录用）：X-Node-Token 命中 agentTokens
// 表返回 nodeID；未命中（REST 直传/校验失败路径）返回空。
func (s *Server) nodeIDForToken(r *http.Request) string {
	s.mu.Lock()
	tokens := s.agentTokens
	s.mu.Unlock()
	tok := r.Header.Get("X-Node-Token")
	if tok == "" {
		return ""
	}
	for t, nid := range tokens {
		if t == tok {
			return nid
		}
	}
	return ""
}

// handleMediaUpload POST /api/media（multipart 或裸二进制，同 /api/images 形态）：
// 节点生成视频/音频上传服务端镜像，供移动端聊天流 <video>/<audio> 播放。
// 上传成功即广播 task.event（kind=image 复用 Images 通道 + task.Images 回填）。
func (s *Server) handleMediaUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	s.mu.Lock()
	dir := s.imageDir
	s.mu.Unlock()
	if dir == "" {
		s.jsonError(w, http.StatusNotFound, "媒体存储未启用")
		return
	}

	var (
		raw       []byte
		fileName  string
		taskID    string
		sessionID string
		err       error
	)
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxMediaUploadBytes + (8 << 20)); err != nil {
			s.jsonError(w, http.StatusBadRequest, "multipart 解析失败: "+err.Error())
			return
		}
		var fh *multipart.FileHeader
		if fhs := r.MultipartForm.File["file"]; len(fhs) > 0 {
			fh = fhs[0]
		}
		if fh == nil {
			s.jsonError(w, http.StatusBadRequest, "缺少 file 字段")
			return
		}
		if fh.Size > maxMediaUploadBytes {
			s.jsonError(w, http.StatusRequestEntityTooLarge, "媒体过大（>256MB）")
			return
		}
		f, err := fh.Open()
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "读取上传文件失败: "+err.Error())
			return
		}
		defer f.Close()
		raw, err = io.ReadAll(io.LimitReader(f, maxMediaUploadBytes+1))
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "读取上传文件失败: "+err.Error())
			return
		}
		fileName = fh.Filename
		taskID = r.FormValue("taskId")
		sessionID = r.FormValue("sessionID")
		if sessionID == "" {
			sessionID = r.FormValue("sessionId")
		}
	} else {
		raw, err = io.ReadAll(io.LimitReader(r.Body, maxMediaUploadBytes+1))
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
			return
		}
		fileName = r.Header.Get("X-Media-Name")
		taskID = r.Header.Get("X-Task-Id")
		sessionID = r.Header.Get("X-Session-Id")
	}
	if len(raw) == 0 || len(raw) > maxMediaUploadBytes {
		s.jsonError(w, http.StatusBadRequest, "媒体为空或超过 256MB 上限")
		return
	}

	// 内容嗅探：非音视频直接拒绝（防伪装上传）。
	mime := sniffMediaMIME(raw)
	if !strings.HasPrefix(mime, "video/") && !strings.HasPrefix(mime, "audio/") {
		s.jsonError(w, http.StatusBadRequest, "内容不是音视频（嗅探: "+mime+"）")
		return
	}

	// 安全文件名：时间戳 + 随机 + 白名单扩展名（客户端名仅扩展名提示）。
	ext := mediaExt(mime)
	if e := strings.ToLower(strings.TrimSpace(filepath.Ext(fileName))); allowedMediaExt[e] {
		ext = strings.TrimPrefix(e, ".")
	}
	name := time.Now().Format("20060102-150405") + "-" + randHex(4) + "." + ext
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.jsonError(w, http.StatusInternalServerError, "创建媒体目录失败: "+err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
		s.jsonError(w, http.StatusInternalServerError, "写盘失败: "+err.Error())
		return
	}
	url := "/img/" + name
	slog.Info("节点媒体上传", "file", name, "size", len(raw), "task", taskID, "session", sessionID)

	// 归属记录（2026-10 签名下载）：媒体文件 → 所属会话/任务/节点，供 /img/ 校验
	// 与旧直链兼容窗口判定；nodeID 从上传令牌反查（REST 直传无令牌 → 空）。
	s.hub.RecordFileMeta(name, sessionID, taskID, s.nodeIDForToken(r))

	// 存储上限巡检（音视频体积更大，巡检更必要）。
	go s.hub.PruneStorage()

	// 回填任务媒体记录（同图片链路：Task.Images 追加 /img/ URL + task.event 广播；
	// 移动端按扩展名分发 <video>/<audio> 渲染）。
	if taskID != "" {
		payload, _ := json.Marshal(ImageEventPayload{Images: []string{url}})
		ev := TaskEvent{TaskID: taskID, SessionID: sessionID, Kind: "image", Text: string(payload), Images: []string{url}, TS: time.Now().UnixMilli()}
		s.hub.AddTaskEvent(ev)
		s.hub.AttachTaskImages(taskID, []string{url})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "url": url, "fileName": name})
}

// handleImageUpload POST /api/images（multipart：file 字段 + 可选 sessionID/taskID 表单值；
// 或裸二进制 body + X-Image-Name 头）。落盘后返回 {ok, url, fileName}。
func (s *Server) handleImageUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	s.mu.Lock()
	dir := s.imageDir
	s.mu.Unlock()
	if dir == "" {
		s.jsonError(w, http.StatusNotFound, "图片存储未启用")
		return
	}

	var (
		raw       []byte
		fileName  string
		taskID    string
		sessionID string
		err       error
	)
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			s.jsonError(w, http.StatusBadRequest, "multipart 解析失败: "+err.Error())
			return
		}
		var fh *multipart.FileHeader
		if fhs := r.MultipartForm.File["file"]; len(fhs) > 0 {
			fh = fhs[0]
		}
		if fh == nil {
			s.jsonError(w, http.StatusBadRequest, "缺少 file 字段")
			return
		}
		if fh.Size > maxImageUploadBytes {
			s.jsonError(w, http.StatusRequestEntityTooLarge, "图片过大（>32MB）")
			return
		}
		f, err := fh.Open()
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "读取上传文件失败: "+err.Error())
			return
		}
		defer f.Close()
		raw, err = io.ReadAll(io.LimitReader(f, maxImageUploadBytes+1))
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "读取上传文件失败: "+err.Error())
			return
		}
		fileName = fh.Filename
		taskID = r.FormValue("taskId")
		sessionID = r.FormValue("sessionID")
		if sessionID == "" {
			sessionID = r.FormValue("sessionId")
		}
	} else {
		raw, err = io.ReadAll(io.LimitReader(r.Body, maxImageUploadBytes+1))
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
			return
		}
		fileName = r.Header.Get("X-Image-Name")
		taskID = r.Header.Get("X-Task-Id")
		sessionID = r.Header.Get("X-Session-Id")
	}
	if len(raw) == 0 || len(raw) > maxImageUploadBytes {
		s.jsonError(w, http.StatusBadRequest, "图片为空或超过 32MB 上限")
		return
	}

	// 内容嗅探：非图片直接拒绝（防伪装上传）。
	mime := sniffImageMIME(raw)
	if !strings.HasPrefix(mime, "image/") {
		s.jsonError(w, http.StatusBadRequest, "内容不是图片（嗅探: "+mime+"）")
		return
	}

	// 安全文件名：服务端生成（时间戳 + 4 字节随机 + 白名单扩展名），客户端名仅取扩展名提示。
	ext := imageExt(mime)
	if e := strings.ToLower(strings.TrimSpace(filepath.Ext(fileName))); allowedImageExt[e] {
		ext = strings.TrimPrefix(e, ".")
	}
	name := time.Now().Format("20060102-150405") + "-" + randHex(4) + "." + ext
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.jsonError(w, http.StatusInternalServerError, "创建图片目录失败: "+err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
		s.jsonError(w, http.StatusInternalServerError, "写盘失败: "+err.Error())
		return
	}
	url := "/img/" + name
	slog.Info("节点图片上传", "file", name, "size", len(raw), "task", taskID, "session", sessionID)

	// 归属记录（2026-10 签名下载）：图片文件 → 所属会话/任务/节点，供 /img/ 校验
	// 与旧直链兼容窗口判定；nodeID 从上传令牌反查（REST 直传无令牌 → 空）。
	s.hub.RecordFileMeta(name, sessionID, taskID, s.nodeIDForToken(r))

	// 存储上限巡检（P-Storage）：图片是存储体积大头，上传成功后异步触发一次
	// 超限检查（PruneStorage 内部互斥，与定时巡检并发安全；无超限即快速返回）。
	go s.hub.PruneStorage()

	// 回填任务图片记录（taskID 可关联时）：Task.Images 去重追加 + 持久化 + task.event 广播。
	if taskID != "" {
		payload, _ := json.Marshal(ImageEventPayload{Images: []string{url}})
		ev := TaskEvent{TaskID: taskID, SessionID: sessionID, Kind: "image", Text: string(payload), Images: []string{url}, TS: time.Now().UnixMilli()}
		s.hub.AddTaskEvent(ev) // 事件记录 + 镜像 + 广播
		s.hub.AttachTaskImages(taskID, []string{url})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "url": url, "fileName": name})
}

// serveImage GET /img/<file>：图片/媒体只读托管。2026-10 安全加固：签名 URL 校验 +
// 旧直链兼容窗口（见 file_meta.go）。
//   - 带签名（?e=&s=&sid=）→ VerifyFileURL（过期/无效/归属不符 → 403）；
//   - 无签名 → LegacyAllowed（创建 7 天内放行，超窗 403）→ 过期即失效（验收标准）；
//   - 校验通过后 http.ServeFile（自动 Content-Type + Range，视频拖进度）。
func (s *Server) serveImage(w http.ResponseWriter, r *http.Request, hub *Hub) {
	s.mu.Lock()
	dir := s.imageDir
	s.mu.Unlock()
	if dir == "" {
		dir = defaultImageDir(hub.Dir())
	}
	name := strings.TrimPrefix(r.URL.Path, "/img/")
	if name == "" || strings.ContainsAny(name, `/\\`) || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	if !allowedImageExt[ext] && !allowedMediaExt[ext] {
		http.NotFound(w, r)
		return
	}
	// 签名/兼容窗口准入校验：用户身份（Basic 凭证）与附件归属均在本步核对。
	q := r.URL.Query()
	sig, exp, sid := q.Get("s"), q.Get("e"), q.Get("sid")
	if sig != "" || exp != "" || sid != "" {
		// 带签名参数：必须全部有效（防半参数探测，部分缺失按无效处理）。
		if err := hub.VerifyFileURL(name, sid, sig, exp); err != nil {
			s.jsonError(w, http.StatusForbidden, errFileForbidden.Error())
			return
		}
	} else if !hub.LegacyAllowed(name, dir) {
		// 无签名直链：兼容窗口（归属 createdAt + 7 天）外一律 403（未授权访问）。
		s.jsonError(w, http.StatusForbidden, errFileForbidden.Error())
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // 内容不可变（时间戳文件名）
	http.ServeFile(w, r, filepath.Join(dir, name))
}

// jsonError JSON 错误响应（统一形态）。
func (s *Server) jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": msg})
}

// defaultImageDir 默认图片目录：hub 数据目录下 images/。
func defaultImageDir(hubDir string) string {
	if hubDir == "" {
		return ""
	}
	return fmt.Sprintf("%s", hubDir) + string(os.PathSeparator) + "images"
}

// randHex n 字节随机数 hex（安全文件名用；rand.go 的 newID 语义对齐）。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()%0xffff)
	}
	return hex.EncodeToString(b)
}

// sniffImageMIME 嗅探图片 MIME（http.DetectContentType 原始结果；
// 未知内容返回非 image/ 前缀值由调用方拒绝，与 tools 包「默认 png」的宽容策略不同：
// 上传接口面向公网，必须拒绝伪装）。
func sniffImageMIME(raw []byte) string {
	n := len(raw)
	if n > 512 {
		n = 512
	}
	ct := http.DetectContentType(raw[:n])
	if strings.HasPrefix(ct, "image/") {
		return ct
	}
	return "application/octet-stream"
}

// imageExt 由 MIME 推断扩展名（默认 png，与 tools.ImageExt 同口径）。
func imageExt(mime string) string {
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

// sniffMediaMIME 嗅探媒体 MIME（视频/音频；未知内容返回非 media 前缀值由调用方拒绝）。
func sniffMediaMIME(raw []byte) string {
	n := len(raw)
	if n > 512 {
		n = 512
	}
	ct := http.DetectContentType(raw[:n])
	if strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/") {
		return ct
	}
	// mp4 box 头嗅探（http.DetectContentType 对部分 ftyp 盒变体可能落 octet-stream）。
	if n >= 12 && string(raw[4:8]) == "ftyp" {
		return "video/mp4"
	}
	if n >= 3 && raw[0] == 'I' && raw[1] == 'D' && raw[2] == '3' {
		return "audio/mpeg"
	}
	if n >= 4 && string(raw[0:4]) == "RIFF" && n >= 12 && string(raw[8:12]) == "WAVE" {
		return "audio/wav"
	}
	if n >= 4 && string(raw[0:4]) == "OggS" {
		return "audio/ogg"
	}
	if n >= 4 && string(raw[0:4]) == "fLaC" {
		return "audio/flac"
	}
	return "application/octet-stream"
}

// mediaExt 由 MIME 推断媒体扩展名（默认 mp4）。
func mediaExt(mime string) string {
	switch mime {
	case "video/webm":
		return "webm"
	case "video/ogg":
		return "ogv"
	case "video/quicktime":
		return "mov"
	case "audio/mpeg":
		return "mp3"
	case "audio/wav", "audio/x-wav":
		return "wav"
	case "audio/ogg":
		return "ogg"
	case "audio/flac":
		return "flac"
	case "audio/aac":
		return "aac"
	case "audio/mp4":
		return "m4a"
	default:
		return "mp4"
	}
}
