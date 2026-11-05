package remote

// 绘画链路（移动端发布绘画 → PC 节点生图 → 上传服务端 → 移动端看到图片）核心环节测试：
//   - 图片上传：multipart（X-Node-Token 节点令牌鉴权）→ /img/ URL 回包 + 落盘；
//   - 图片事件：上传关联 taskID 时 Task.Images 回填 + task.event(kind=image) 广播；
//   - /img/<file> 公开只读回读（Content-Type 正确、无 token 也 200）；
//   - 非图片内容拒绝；无效令牌 401；
//   - paint 任务全链路字段（kind/spot/生图指令注入/无节点暂存）。

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pngBytes 构造 1x1 PNG（最小合法图片，过内容嗅探）。
func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return b.Bytes()
}

// uploadImage 以节点令牌 multipart 上传图片，返回响应结构。
func uploadImage(t *testing.T, ts *httptest.Server, token, fileName string, data []byte, taskID, sessionID string) (int, map[string]interface{}) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(data)); err != nil {
		t.Fatalf("copy data: %v", err)
	}
	mw.WriteField("taskId", taskID)
	mw.WriteField("sessionID", sessionID)
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/images", buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("X-Node-Token", token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestImageUploadHandler 上传 → 落盘 → URL 回包 → /img/ 公开回读 → 任务回填 → 事件广播。
func TestImageUploadHandler(t *testing.T) {
	imgDir := t.TempDir()
	hub := NewHub(t.TempDir(), map[string]string{"tok-img": "node-img"}, "", nil, nil, nil)
	defer hub.Stop() // 停 persistLoop/巡检：防落盘与 TempDir 清理竞态（Windows unlinkat flake，下同）
	srv := NewServer(hub, "")
	srv.SetImageDir(imgDir)
	srv.SetAgentTokens(map[string]string{"tok-img": "node-img"})
	// REST 凭据非空：imageAuth 回落 restAuth 时错误令牌无法直通（验证节点令牌鉴权实效）。
	srv.SetRESTCredentials("restuser", "restpass", "")
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	// 预注册离线节点（无真实连接 → CreateTaskKinded 暂存 pending）：
	// 直接构造 nodeState + 空 agentConn（writeJSON 安全返回 errStopped，
	// dispatchLocked 写失败回退暂存，与节点离线语义一致）。
	hub.mu.Lock()
	ns := &nodeState{conn: &agentConn{nodeID: "node-img"}}
	ns.info.ID = "node-img"
	ns.info.Name = "图片节点"
	ns.info.FirstSeen = time.Now().UnixMilli()
	hub.nodes["node-img"] = ns
	hub.mu.Unlock()

	// paint 任务受理（无连接节点 → 暂存 pending，不影响图片回填）。
	task, err := hub.CreateTaskKinded("node-img", "雪后断桥水墨画", "", "mobile:test", TaskKindPaint, "杭州西湖")
	if err != nil {
		t.Fatalf("create paint task: %v", err)
	}
	if task.Kind != TaskKindPaint || task.Spot != "杭州西湖" {
		t.Fatalf("task kind/spot 不符: %+v", task)
	}
	if task.Status != TaskStatusPending {
		t.Fatalf("无在线节点应暂存 pending，got %s", task.Status)
	}
	if !strings.HasPrefix(task.Text, "【绘画任务】") {
		t.Fatalf("paint 任务提示词未注入生图指令: %q", task.Text)
	}

	// 无效令牌 → 401。
	if code, _ := uploadImage(t, ts, "tok-wrong", "a.png", pngBytes(t), task.ID, task.SessionID); code != http.StatusUnauthorized {
		t.Fatalf("错误令牌应 401，got %d", code)
	}
	// 正确令牌 → 200 + url。
	code, out := uploadImage(t, ts, "tok-img", "a.png", pngBytes(t), task.ID, task.SessionID)
	if code != http.StatusOK {
		t.Fatalf("上传应 200，got %d: %v", code, out)
	}
	url, _ := out["url"].(string)
	if !strings.HasPrefix(url, "/img/") {
		t.Fatalf("url 应为 /img/ 前缀，got %q", url)
	}
	fileName, _ := out["fileName"].(string)
	if _, err := os.Stat(filepath.Join(imgDir, fileName)); err != nil {
		t.Fatalf("图片未落盘: %v", err)
	}

	// /img/ 公开只读（无鉴权头）。
	resp, err := http.Get(ts.URL + url)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/img/ 应 200，got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		t.Fatalf("Content-Type 应 image/*，got %s", ct)
	}
	// 防穿越/白名单。
	for _, p := range []string{"/img/..%2Ftasks.json", "/img/evil.txt", "/img/"} {
		if r2, err := http.Get(ts.URL + p); err == nil {
			r2.Body.Close()
			if r2.StatusCode == http.StatusOK && p == "/img/..%2Ftasks.json" {
				t.Fatalf("路径穿越未拦截: %s", p)
			}
		}
	}

	// 任务回填：Task.Images 已含 URL（持久化随 saveTasks）。
	got, ok := hub.GetTask(task.ID)
	if !ok {
		t.Fatalf("任务丢失")
	}
	if len(got.Images) != 1 || got.Images[0] != url {
		t.Fatalf("Task.Images 回填不符: %v", got.Images)
	}

	// 事件广播：kind=image 的 TaskEvent 已进事件历史。
	evs := hub.TaskEvents(task.ID)
	found := false
	for _, e := range evs {
		if e.Kind == "image" && strings.Contains(e.Text, url) {
			found = true
		}
	}
	if !found {
		t.Fatalf("未找到 image 事件（事件数 %d）", len(evs))
	}

	// 非图片内容（伪装扩展名）→ 400。
	if code, _ := uploadImage(t, ts, "tok-img", "fake.png", []byte("this is not an image"), task.ID, ""); code != http.StatusBadRequest {
		t.Fatalf("非图片应 400，got %d", code)
	}

	// 会话镜像：image 事件应落 get.messages 可见（say=image 帧）。
	if msgs := hub.SessionMessages(task.SessionID); len(msgs) == 0 {
		t.Fatalf("image 事件未落会话镜像")
	}
}
