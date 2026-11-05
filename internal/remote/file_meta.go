package remote

// file_meta.go 附件/生成文件归属元数据 + 短期签名下载（2026-10 安全加固）：
//
// 背景：/img/<file> 公开只读托管，任意知晓文件名者（历史 URL 泄露/枚举）均可
// 下载全部图片/媒体，无归属校验、无过期约束。
//
// 方案（签名 URL + 归属校验 + 旧直链兼容窗口）：
//   - 上传（/api/images /api/media）成功时记录 file → {sessionID, taskID, nodeID,
//     createdAt} 归属（file_meta.json 持久化，同步写——上传/删除低频，量级小）；
//   - GET /api/file-url?f=<file>&sid=<sessionID>（Basic 任意账号 / clientToken 鉴权）
//     换取短期签名 URL：/img/<file>?e=<exp>&s=<HMAC-SHA256(secret, file|exp|user|sid)>；
//   - /img/ 下载校验：签名有效 + 未过期 + URL sid 与归属 sessionID 一致 → 200；
//     无签名直链（旧客户端/历史持久化 URL）在归属 createdAt + legacyWindow 内放行，
//     超窗 403；过期/无效/归属不符 → 403。
//
// 安全边界：签名 secret（file_token.key，32 字节随机）首次使用生成于 hub 数据目录，
// 不落代码；签名绑定换取者 user 与归属 sessionID，防跨会话拼接与跨用户扩散
//（持 URL 者 = 换取时已通过账号/令牌鉴权）。

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileMeta 附件归属元数据（file → 所属会话/任务/节点 + 创建时间）。
type FileMeta struct {
	File      string `json:"file"`      // 文件名（/img/ 后段）
	SessionID string `json:"sessionID"` // 所属会话（空 = 未关联，如节点无 session 上传）
	TaskID    string `json:"taskID"`    // 所属任务（空 = 未关联）
	NodeID    string `json:"nodeID"`    // 上传节点（X-Node-Token 反查；空 = REST 直传/未知）
	CreatedAt int64  `json:"createdAt"` // 落盘时间（毫秒；兼容窗口锚点）
}

const (
	// legacyWindowDays 旧直链（无签名 /img/<file>）兼容窗口天数：窗口内视为旧
	// 行为放行，超窗 403（验收「未授权访问返回 403」）。给存量 URL/旧客户端收敛期。
	legacyWindowDays = 7

	// fileURLTTL 签名 URL 有效期（短期，移动端渲染/下载窗口；超期需重新换取）。
	fileURLTTL = 2 * time.Hour
)

// errFileForbidden 统一 403 错误（不区分签名无效/过期/归属不符，防探测）。
var errFileForbidden = errors.New("forbidden: 访问未授权或链接已过期")

// fileMetaMu 归属元数据读写锁（独立于 h.mu：上传/下载热路径不阻塞 hub 主锁）。
var fileMetaMu sync.RWMutex

// fileMetaByFile file → 归属（进程内存权威态；加载自 file_meta.json）。
// Hub 为单例（服务端单进程），全局 map 与 hub 生命周期同步，避免把锁/持久化
// 塞进 Hub 结构体（hub.go 核心面零改动）。
var fileMetaByFile = map[string]FileMeta{}

// fileSignSecret 签名密钥（进程内缓存；首次使用时从 file_token.key 加载/生成）。
var fileSignSecret []byte

// fileSignSecretMu 保护 fileSignSecret 并发初始化。
var fileSignSecretMu sync.Mutex

// fileMetaKeyFile file_token.key 路径（hub 数据目录）。
func (h *Hub) fileMetaKeyFile() string { return filepath.Join(h.dir, "file_token.key") }

// fileMetaFile file_meta.json 路径（hub 数据目录）。
func (h *Hub) fileMetaFile() string { return filepath.Join(h.dir, "file_meta.json") }

// loadFileMeta 加载归属元数据（损坏/缺失静默跳过，对齐 hub 持久化容错语义）。
func (h *Hub) loadFileMeta() {
	b, err := os.ReadFile(h.fileMetaFile())
	if err != nil {
		return
	}
	var recs map[string]FileMeta
	if json.Unmarshal(b, &recs) != nil {
		return
	}
	fileMetaMu.Lock()
	for k, v := range recs {
		if k != "" && v.File != "" {
			fileMetaByFile[k] = v
		}
	}
	fileMetaMu.Unlock()
}

// saveFileMeta 归属元数据原子落盘（tmp + rename；hub 目录不存在时跳过——纯内存模式）。
func (h *Hub) saveFileMeta() {
	if h.dir == "" {
		return
	}
	fileMetaMu.RLock()
	snap := make(map[string]FileMeta, len(fileMetaByFile))
	for k, v := range fileMetaByFile {
		snap[k] = v
	}
	fileMetaMu.RUnlock()
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	tmp := h.fileMetaFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, h.fileMetaFile())
}

// RecordFileMeta 上传成功时记录归属（覆盖；file 为空跳过）。
func (h *Hub) RecordFileMeta(file, sessionID, taskID, nodeID string) {
	if file == "" {
		return
	}
	m := FileMeta{
		File:      file,
		SessionID: sessionID,
		TaskID:    taskID,
		NodeID:    nodeID,
		CreatedAt: time.Now().UnixMilli(),
	}
	fileMetaMu.Lock()
	fileMetaByFile[file] = m
	fileMetaMu.Unlock()
	h.saveFileMeta()
}

// LookupFileMeta 查询文件归属（不存在返回零值 + false）。
func (h *Hub) LookupFileMeta(file string) (FileMeta, bool) {
	fileMetaMu.RLock()
	defer fileMetaMu.RUnlock()
	m, ok := fileMetaByFile[file]
	return m, ok
}

// DropFileMeta 删除文件归属（PruneStorage 删除图片时联动，防残留记录成为
// 兼容窗口误放行锚点；file 为空跳过）。
func (h *Hub) DropFileMeta(file string) {
	if file == "" {
		return
	}
	fileMetaMu.Lock()
	delete(fileMetaByFile, file)
	fileMetaMu.Unlock()
	h.saveFileMeta()
}

// signSecretLoad 加载（或首次生成）签名密钥 file_token.key（32 字节随机）。
// 密钥不随发布物变更；删除 key 文件 = 存量签名全部失效（运维兜底手段）。
func (h *Hub) signSecretLoad() []byte {
	fileSignSecretMu.Lock()
	defer fileSignSecretMu.Unlock()
	if len(fileSignSecret) > 0 {
		return fileSignSecret
	}
	if h.dir != "" {
		if b, err := os.ReadFile(h.fileMetaKeyFile()); err == nil && len(b) >= 16 {
			fileSignSecret = b
			return fileSignSecret
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极端兜底：时间种子（弱化但可运行；正常环境不会走到）。
		copy(b, fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	fileSignSecret = b
	if h.dir != "" {
		_ = os.MkdirAll(h.dir, 0o755)
		_ = os.WriteFile(h.fileMetaKeyFile(), b, 0o600)
	}
	return fileSignSecret
}

// signFileHMAC HMAC-SHA256(secret, file|exp|sid)。
// 说明：签名不绑定用户账号 —— <img>/<video> 标签无法携带 Authorization 头，
// 签名 URL 本身即短期凭证（换取接口已鉴权）；用户身份 + 归属由换取时校验与
// sid 绑定实现（防跨会话）。
func signFileHMAC(secret []byte, file, exp, sid string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(file))
	mac.Write([]byte("|"))
	mac.Write([]byte(exp))
	mac.Write([]byte("|"))
	mac.Write([]byte(sid))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignFileURL 为 file 生成短期签名下载 URL（sid = 归属会话）。
// exp 为毫秒时间戳（10 进制字符串）；签名不绑定用户账号（<img> 无法带 Authorization 头，
// 签名 URL 本身即短期凭证，换取接口已鉴权）。
func (h *Hub) SignFileURL(file, sid string) string {
	if file == "" {
		return ""
	}
	exp := fmt.Sprintf("%d", time.Now().Add(fileURLTTL).UnixMilli())
	s := signFileHMAC(h.signSecretLoad(), file, exp, sid)
	return fmt.Sprintf("/img/%s?e=%s&s=%s&sid=%s", file, exp, s, sid)
}

// VerifyFileURL 校验签名 URL（file + URL sid）：
//   - sig/exp/sid 任一缺失 → 403（无签名直链走兼容窗口判定，见 serveImage）；
//   - 签名无效 / 已过期 → 403；
//   - 归属存在时 URL sid 必须与归属 sessionID 一致（禁止跨会话拼接）。
func (h *Hub) VerifyFileURL(file, sid, sig, exp string) error {
	if sig == "" || exp == "" || sid == "" {
		return errFileForbidden
	}
	expMs, err := parseInt64Strict(exp)
	if err != nil || time.Now().UnixMilli() > expMs {
		return errFileForbidden // 过期或 exp 非法
	}
	want := signFileHMAC(h.signSecretLoad(), file, exp, sid)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return errFileForbidden
	}
	if m, ok := h.LookupFileMeta(file); ok && m.SessionID != "" && m.SessionID != sid {
		return errFileForbidden // 归属会话不匹配：禁止跨会话访问
	}
	return nil
}

// LegacyAllowed 无签名直链兼容窗口判定：归属存在时 createdAt+legacyWindowDays 内
// 放行；无归属记录（历史存量文件/旧版本上传）按文件 ModTime 兜底（同窗口口径）。
func (h *Hub) LegacyAllowed(file, dir string) bool {
	if m, ok := h.LookupFileMeta(file); ok {
		return time.Since(time.UnixMilli(m.CreatedAt)) < legacyWindowDays*24*time.Hour
	}
	if st, err := os.Stat(filepath.Join(dir, file)); err == nil {
		return time.Since(st.ModTime()) < legacyWindowDays*24*time.Hour
	}
	return false
}

// parseInt64Strict 严格解析非负十进制 int64（拒绝符号/空白/溢出）。
func parseInt64Strict(s string) (int64, error) {
	if s == "" || len(s) > 19 {
		return 0, fmt.Errorf("invalid int64")
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid int64")
		}
		n = n*10 + int64(r-'0')
		if n < 0 { // 溢出（> maxInt64）
			return 0, fmt.Errorf("overflow")
		}
	}
	return n, nil
}

// dropFileMetaByPrefix 删除前缀匹配的归属（PruneStorage 整天删除联动；prefix 空跳过）。
func (h *Hub) dropFileMetaByPrefix(prefix string) {
	if prefix == "" {
		return
	}
	fileMetaMu.Lock()
	for k := range fileMetaByFile {
		if strings.HasPrefix(k, prefix) {
			delete(fileMetaByFile, k)
		}
	}
	fileMetaMu.Unlock()
	h.saveFileMeta()
}

// attachmentMetaSnapshot 归属全量快照（测试/调试用）。
func (h *Hub) attachmentMetaSnapshot() map[string]FileMeta {
	fileMetaMu.RLock()
	defer fileMetaMu.RUnlock()
	out := make(map[string]FileMeta, len(fileMetaByFile))
	for k, v := range fileMetaByFile {
		out[k] = v
	}
	return out
}
