package remote

// file_meta_test.go 附件签名下载（2026-10 安全加固）核心行为测试：
//   - 上传记录归属（file_meta.json 落盘 + 内存查询）；
//   - /api/file-url 换取签名 URL（Basic 鉴权：合法账号 200 / 错误账号 401）；
//   - /img/ 签名访问 200；签名过期 403；伪造签名 403；归属会话不符 403；
//   - 无签名直链：兼容窗口内 200、窗口外 403；
//   - PruneStorage 删除图片联动清理归属。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fileURLTestEnv 构造测试服务（hub + server + 图片目录 + 上传一张图）。
func fileURLTestEnv(t *testing.T, users []Account) (*Server, *Hub, *httptest.Server, string) {
	t.Helper()
	imgDir := t.TempDir()
	hub := NewHub(t.TempDir(), map[string]string{"tok-img": "node-img"}, "sharetok", nil, nil, users)
	t.Cleanup(hub.Stop)  // 停 persistLoop/巡检：防落盘与 TempDir 清理竞态（Windows unlinkat flake，下同）
	hub.SetStoreLimit(0) // 测试内手动控制清理
	srv := NewServer(hub, "")
	srv.SetImageDir(imgDir)
	srv.SetAgentTokens(map[string]string{"tok-img": "node-img"})
	srv.SetRESTCredentials("", "", "sharetok")
	if users == nil {
		srv.SetRESTCredentials("", "", "sharetok")
	}
	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)
	return srv, hub, ts, imgDir
}

// fileURLUpload 上传一张测试图片（带 task/session），返回 url 与输出。
func fileURLUpload(t *testing.T, ts *httptest.Server) (string, map[string]interface{}) {
	t.Helper()
	code, out := uploadImage(t, ts, "tok-img", "s.png", pngBytes(t), "task-1", "sess-1")
	if code != http.StatusOK {
		t.Fatalf("上传应 200，got %d: %v", code, out)
	}
	url, _ := out["url"].(string)
	return url, out
}

// fileURLGet 带凭据 GET 一个 URL（Basic 或 token），返回状态码。
func fileURLGet(t *testing.T, ts *httptest.Server, path, user, pass string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestFileMetaSignFlow 签名换取 → 签名访问 → 过期/伪造/归属不符 → 直链窗口 全链路。
func TestFileMetaSignFlow(t *testing.T) {
	_, hub, ts, _ := fileURLTestEnv(t, []Account{{User: "alice", Pass: "pw", Role: "user"}})
	url, out := fileURLUpload(t, ts)
	fileName := strings.TrimPrefix(url, "/img/")
	_ = out

	// 归属已记录。
	if _, ok := hub.LookupFileMeta(fileName); !ok {
		t.Fatalf("上传后应记录归属")
	}

	// 1) 无凭据换签 → 401。
	if code, _ := fileURLGet(t, ts, "/api/file-url?f="+fileName+"&sid=sess-1", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("未认证换签应 401，got %d", code)
	}
	// 2) 合法账号换签 → 200 + 签名 URL。
	code, body := fileURLGet(t, ts, "/api/file-url?f="+fileName+"&sid=sess-1", "alice", "pw")
	if code != http.StatusOK {
		t.Fatalf("换签应 200，got %d: %s", code, body)
	}
	var ok struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ok); err != nil || !strings.Contains(ok.Data.URL, "?e=") {
		t.Fatalf("换签响应不符: %s", body)
	}
	signedURL := ok.Data.URL

	// 3) 签名 URL 带 Basic 访问 → 200。
	if code, _ := fileURLGet(t, ts, signedURL, "alice", "pw"); code != http.StatusOK {
		t.Fatalf("签名访问应 200，got %d", code)
	}

	// 4) 无签名直链（窗口内）→ 200（兼容窗口）。
	if code, _ := fileURLGet(t, ts, url, "alice", "pw"); code != http.StatusOK {
		t.Fatalf("窗口内直链应 200，got %d", code)
	}

	// 5) 篡改签名 → 403。
	tampered := strings.Replace(signedURL, "s="+signedURL[strings.Index(signedURL, "s=")+2:strings.Index(signedURL, "&sid")], "s=deadbeef", 1)
	if code, _ := fileURLGet(t, ts, tampered, "alice", "pw"); code != http.StatusForbidden {
		t.Fatalf("伪造签名应 403，got %d", code)
	}

	// 6) 归属会话不符（sid 换成他人会话）→ 403（即使签名格式有效也无法通过——此处用
	// 不同 sid 重新签发的 URL 组合：先核对正常签名 + 错误 sid 被拒）。
	wrongSidURL := strings.Replace(signedURL, "sid=sess-1", "sid=sess-2", 1)
	if code, _ := fileURLGet(t, ts, wrongSidURL, "alice", "pw"); code != http.StatusForbidden {
		t.Fatalf("错误 sid 应 403，got %d", code)
	}

	// 7) 签名过期 → 403：构造一个 exp=1（1970）的过期签名（无需密钥，服务端先拒过期）。
	expiredURL := fmt.Sprintf("/img/%s?e=%s&s=%s&sid=sess-1", fileName, "1",
		signFileHMAC(hub.signSecretLoad(), fileName, "1", "sess-1"))
	if code, _ := fileURLGet(t, ts, expiredURL, "alice", "pw"); code != http.StatusForbidden {
		t.Fatalf("过期签名应 403，got %d", code)
	}

	// 8) 兼容窗口外直链 → 403：把归属 createdAt 前移 8 天。
	fm, _ := hub.LookupFileMeta(fileName)
	fm.CreatedAt = time.Now().Add(-8 * 24 * time.Hour).UnixMilli()
	fileMetaMu.Lock()
	fileMetaByFile[fileName] = fm
	fileMetaMu.Unlock()
	if code, _ := fileURLGet(t, ts, url, "alice", "pw"); code != http.StatusForbidden {
		t.Fatalf("窗口外直链应 403，got %d", code)
	}
}

// TestFileMetaNoUploadedMeta 无归属记录（历史存量文件）按 ModTime 窗口兜底。
func TestFileMetaNoUploadedMeta(t *testing.T) {
	_, hub, ts, imgDir := fileURLTestEnv(t, nil)
	// 直接落盘一个旧文件（无归属记录）。
	oldName := "20200101-000000-aaaa.png"
	if err := os.WriteFile(filepath.Join(imgDir, oldName), pngBytes(t), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 文件是刚创建的 → 窗口内 → 200。
	if code, _ := fileURLGet(t, ts, "/img/"+oldName, "", ""); code != http.StatusOK {
		t.Fatalf("无归属新文件窗口内应 200，got %d", code)
	}
	// 改 ModTime 为 8 天前 → 窗口外 → 403。
	old := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(imgDir, oldName), old, old)
	if code, _ := fileURLGet(t, ts, "/img/"+oldName, "", ""); code != http.StatusForbidden {
		t.Fatalf("无归属旧文件窗口外应 403，got %d", code)
	}
	_ = hub
}

// TestFileMetaPruneCleansMeta PruneStorage 删除图片时联动清理归属记录。
func TestFileMetaPruneCleansMeta(t *testing.T) {
	dir := t.TempDir()
	hub := NewHub(dir, map[string]string{"tok": "node"}, "", nil, nil, nil)
	defer hub.Stop()
	hub.SetStoreLimit(1 << 20) // 1MB 下限
	imgDir := hub.imagesDir()
	_ = os.MkdirAll(imgDir, 0o755)
	// 写一张旧日期图片（文件名时间戳 2020）+ 归属记录。
	oldFile := "20200101-000000-aaaa.png"
	_ = os.WriteFile(filepath.Join(imgDir, oldFile), pngBytes(t), 0o644)
	old := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(imgDir, oldFile), old, old)
	hub.RecordFileMeta(oldFile, "sess-old", "task-old", "node")
	if _, ok := hub.LookupFileMeta(oldFile); !ok {
		t.Fatalf("归属记录未写入")
	}
	// 写一张大文件直接把总量顶过 1MB（图片 32MB 上限内任意大文件）。
	big := make([]byte, 2<<20)
	_ = os.WriteFile(filepath.Join(imgDir, "20260101-000000-bbbb.png"), big, 0o644)
	_ = hub.PruneStorage()
	// 最旧一天（2020）应被删 + 归属联动清理。
	if _, err := os.Stat(filepath.Join(imgDir, oldFile)); !os.IsNotExist(err) {
		t.Fatalf("旧图片应被清理")
	}
	if _, ok := hub.LookupFileMeta(oldFile); ok {
		t.Fatalf("归属记录应随文件联动清理")
	}
}
