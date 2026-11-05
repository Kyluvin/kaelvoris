package desktop

// channel_im_test.go 通用 IM 通道运行时单测：
//   - pubtoken 签名（imPubToken）确定性 + 排序 + 格式
//   - pubtoken 验签（回调侧：PubAccId/FromUserName/CreateTime/MsgId/pubkey）
//   - 回调解析（parseCallback：JSON 载荷字段提取）
//   - handleCallback POST 全链路（验签 → 应答 → 异步触发，httptest）
//   - MsgId 去重（seenMsg）
//   - sendPubMessage 请求体组装（httptest 假服务：pubtoken 签名 + from/to/type/msg
//     + 文档成功响应形态 {"pubId","sourceMsgId"} + pubsendV2 形态兼容）
//   - sendWebhook 请求体组装（httptest 假服务：APIBase 回退形态 + WebhookURL 直配形态 + 出参模板）
//   - 群消息 HTTP 接收（parseGroupHooks / parseGroupPayload / InMap 映射 / handleGroupMsg / onGroupMsg）
//
// 说明：签名函数 imPubToken(pubkey, args...) 的第一个参数为 pubkey（尤其
// 回调侧 pubtoken=sha1(sort(PubAccId,FromUserName,CreateTime,MsgId,pubkey))，
// 与发送侧 pubtoken=sha1(sort(no,pub,pubkey,nonce,time)) 均为「同一集合排序后
// 拼接 sha1」，区别仅在参与元素——测试分别覆盖两个方向。

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func imTestChannel() *IMChannel {
	return &IMChannel{
		cfg: ChannelConfig{
			Type:      "im",
			Name:      "测试 IM",
			PublicID:  "XT-test-pub-001",
			PubSecret: "test-pubkey",
			EID:       "10603457",
		},
		running: map[string]bool{},
		pending: map[string]*pendingAsk{},
	}
}

// TestIMPubToken_Send 发送侧签名：pubtoken=sha1(sort(no,pub,pubkey,nonce,time))。
func TestIMPubToken_Send(t *testing.T) {
	// 文档 PHP 示例参数：no=19901, pub=XT-87e1732f-xxxx, pubkey=12345678123456781234567812345678,
	// nonce=123456, time=1395460000 → sha1(sort(...))。
	got := imPubToken("12345678123456781234567812345678", "19901", "XT-87e1732f-xxxx", "123456", "1395460000")
	if len(got) != 40 {
		t.Fatalf("pubtoken 长度应为 40（sha1 hex），got %d", len(got))
	}
	// 乱序输入 → 相同结果（函数内部排序）。
	got2 := imPubToken("12345678123456781234567812345678", "1395460000", "XT-87e1732f-xxxx", "19901", "123456")
	if got != got2 {
		t.Fatalf("乱序输入应得相同签名: %s vs %s", got, got2)
	}
}

// TestIMCallbackToken 回调侧验签：pubtoken=sha1(sort(PubAccId,FromUserName,CreateTime,MsgId,pubkey))。
func TestIMCallbackToken(t *testing.T) {
	pubkey := "test-pubkey"
	cb := map[string]string{
		"PubAccId":     "XT-a833d285-xxx",
		"FromUserName": "c51b52e8-xxx",
		"CreateTime":   "1396593594951",
		"MsgId":        "XT-baa6c6fd-xxx",
	}
	// 平台侧计算方式（排序拼接 sha1）——独立实现验证一致性。
	want := imPubToken(pubkey, cb["PubAccId"], cb["FromUserName"], cb["CreateTime"], cb["MsgId"])
	// 传参乱序也应一致（函数内部排序，按字符串序）。
	got := imPubToken(pubkey, cb["MsgId"], cb["CreateTime"], cb["FromUserName"], cb["PubAccId"])
	if want != got {
		t.Fatalf("回调验签不一致: %s vs %s", want, got)
	}
	// 明文拼接校验（手动复现 sha1(sort(...))）：
	parts := []string{pubkey, cb["PubAccId"], cb["FromUserName"], cb["CreateTime"], cb["MsgId"]}
	sortStrings(parts)
	sum := sha1Hex(strings.Join(parts, ""))
	if sum != want {
		t.Fatalf("sha1 明文不一致: got %s want %s", sum, want)
	}
}

// TestIMParseCallback 回调 JSON 解析：字段提取 + 非法载荷拒绝。
func TestIMParseCallback(t *testing.T) {
	ch := imTestChannel()
	body := `{"ServiceType":"xt","PubAccId":"XT-a833d285-xxx","FromUserName":"c51b52e8-xxx","ToUserName":"XT-a833d285-xxx","MsgType":"text","MsgId":"XT-baa6c6fd-xxx","CreateTime":"1396593594951","PubToken":"abc","Payload":{"Content":"TEST 内容"}}`
	cb, err := ch.parseCallback([]byte(body))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cb.MsgType != "text" || cb.Payload.Content != "TEST 内容" || cb.FromUserName != "c51b52e8-xxx" {
		t.Fatalf("字段提取错误: %+v", cb)
	}
	if _, err := ch.parseCallback([]byte(`{"MsgType":"text"}`)); err == nil {
		t.Fatal("缺少 FromUserName 应报错")
	}
}

// TestIMHandleCallbackPOST 回调全链路（httptest）：验签 → 应答 JSON → 异步触发。
func TestIMHandleCallbackPOST(t *testing.T) {
	ch := imTestChannel()
	pubkey := ch.cfg.PubSecret
	from, mid, createTime := "c51b52e8-xxx", "XT-baa6c6fd-xxx", "1396593594951"
	token := imPubToken(pubkey, ch.cfg.PublicID, from, createTime, mid)
	body := `{"ServiceType":"xt","PubAccId":"` + ch.cfg.PublicID + `","FromUserName":"` + from +
		`","ToUserName":"XT-a833d285-xxx","MsgType":"text","MsgId":"` + mid +
		`","CreateTime":"` + createTime + `","PubToken":"` + token +
		`","Payload":{"Content":"你好"}}`
	req := httptest.NewRequest(http.MethodPost, "http://localhost:9881/im/callback", strings.NewReader(body))
	rec := httptest.NewRecorder()
	ch.handleCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body = %q", rec.Code, rec.Body.String())
	}
	var resp imPubResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应 JSON 解析失败: %v", err)
	}
	if !resp.Success || resp.Data.Type != 2 || resp.ErrorCode != 0 {
		t.Fatalf("响应结构错误: %+v", resp)
	}

	// 验签失败 → 403。
	badBody := strings.Replace(body, `"PubToken":"`+token+`"`, `"PubToken":"bad"`, 1)
	req2 := httptest.NewRequest(http.MethodPost, "http://localhost:9881/im/callback", strings.NewReader(badBody))
	rec2 := httptest.NewRecorder()
	ch.handleCallback(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("验签失败应返回 403，got %d", rec2.Code)
	}
}

// TestIMSeenMsg MsgId 去重：首次 false，10 分钟内重复 true。
func TestIMSeenMsg(t *testing.T) {
	ch := imTestChannel()
	if ch.seenMsg("msg-1") {
		t.Fatal("首次消息不应判重")
	}
	if !ch.seenMsg("msg-1") {
		t.Fatal("10 分钟内重复消息应判重")
	}
	if ch.seenMsg("msg-2") {
		t.Fatal("不同 MsgId 不应误判重")
	}
}

// TestIMSendPubMessage 公共号定向回传（httptest 假服务）：
// APIBase 可配置（平台地址不写死）+ 请求体结构 + pubtoken 验签。
func TestIMSendPubMessage(t *testing.T) {
	var gotBody map[string]any
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pubacc/pubsendV2" {
			t.Errorf("路径 = %q，want /pubacc/pubsendV2", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("请求体解析失败: %v", err)
		}
		gotBody = body
		from := body["from"].(map[string]any)
		gotToken = from["pubtoken"].(string)
		// 文档 pubSend 成功响应形态：{"pubId":"...","sourceMsgId":"..."}
		//（无 success 字段）。
		_, _ = w.Write([]byte(`{"pubId":"XT-d4dfc04c-9416-4b8c-86ca-dc57dc949006","sourceMsgId":"XT-5b3dc0f3e4b0ad2483c0701a"}`))
	}))
	defer srv.Close()

	ch := imTestChannel()
	ch.cfg.APIBase = srv.URL // 平台地址面板可配
	ch.sendPubMessage("5a41b292e4b058cf3d0cf314", "你好，IM！")

	if gotBody == nil {
		t.Fatal("服务端未收到请求体")
	}
	from := gotBody["from"].(map[string]any)
	if from["no"] != ch.cfg.EID || from["pub"] != ch.cfg.PublicID {
		t.Fatalf("from 字段错误: %+v", from)
	}
	to := gotBody["to"].([]any)[0].(map[string]any)
	if to["no"] != ch.cfg.EID {
		t.Fatalf("to.no 错误: %v", to["no"])
	}
	users := to["user"].([]any)
	if len(users) != 1 || users[0] != "5a41b292e4b058cf3d0cf314" {
		t.Fatalf("to.user 错误: %v", users)
	}
	if gotBody["type"].(float64) != 2 {
		t.Fatalf("type = %v，want 2", gotBody["type"])
	}
	msg := gotBody["msg"].(map[string]any)
	if msg["text"] != "你好，IM！" {
		t.Fatalf("msg.text = %v", msg["text"])
	}
	// 验签：token=sha1(sort(eid,pub,pubkey,nonce,time))。
	nonce := from["nonce"].(string)
	ts := from["time"].(string)
	want := imPubToken(ch.cfg.PubSecret, ch.cfg.EID, ch.cfg.PublicID, nonce, ts)
	if gotToken != want {
		t.Fatalf("pubtoken 验签失败: got %s want %s", gotToken, want)
	}
}

// TestIMSendPubMessage_CrossEid 跨企业回复定向（2026-10 线上缺陷修复）：
// 回调报文携带发送者来源企业 Eid（与渠道配置 EID 不同），回复 pubsend 的
// to.no 必须投发送者来源企业——固定投配置 EID 被网关拒「无权限发送给公司
// to.no=...」。from.no 恒用渠道配置（公共号自身身份不变）。
func TestIMSendPubMessage_CrossEid(t *testing.T) {
	var gotToNo, gotFromNo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if to, ok := body["to"].([]any); ok && len(to) > 0 {
			gotToNo = to[0].(map[string]any)["no"].(string)
		}
		if from, ok := body["from"].(map[string]any); ok {
			gotFromNo = from["no"].(string)
		}
		_, _ = w.Write([]byte(`{"code":0,"success":true}`))
	}))
	defer srv.Close()

	ch := imTestChannel()
	ch.cfg.APIBase = srv.URL
	// 模拟跨企业用户回调：来源企业 Eid=500000042 ≠ 渠道配置 EID=10603457。
	ch.onCallback(&imPubCallback{
		FromUserName: "openid-cross-eid", MsgType: "text", Eid: "500000042",
		Payload: struct {
			Content string `json:"Content"`
		}{Content: "跨企业消息"},
	})
	ch.sendPubMessage("openid-cross-eid", "跨企业回复")

	if gotToNo != "500000042" {
		t.Fatalf("跨企业用户回复 to.no = %q，want 500000042（发送者来源企业）", gotToNo)
	}
	if gotFromNo != ch.cfg.EID {
		t.Fatalf("from.no = %q，want 渠道配置 EID %q（公共号自身身份不变）", gotFromNo, ch.cfg.EID)
	}

	// 未簿记用户（无 Eid 载荷/进程重启）：回退渠道配置 EID。
	ch.sendPubMessage("openid-unknown-eid", "回退")
	if gotToNo != ch.cfg.EID {
		t.Fatalf("未簿记用户回复 to.no = %q，want 渠道配置 EID %q", gotToNo, ch.cfg.EID)
	}
}

// TestIMSendPubMessage_V2Compat pubsendV2 形态兼容：平台新旧版本共存，
// HTTP 200 + {"code":0,"success":true} 同样判成功。
func TestIMSendPubMessage_V2Compat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"message":"发送成功","success":true}`))
	}))
	defer srv.Close()

	ch := imTestChannel()
	ch.cfg.APIBase = srv.URL
	ch.sendPubMessage("openid-v2", "v2 形态兼容")
}

// TestIMParseMalformedStatus 畸形状态码提取：pubsend 失败响应为非标准 4 位
// HTTP 状态码（HTTP/1.1 5000 ...），Go net/http 报 malformed HTTP status code
// 断连——须从错误文本提取业务码供频控重试判定。
func TestIMParseMalformedStatus(t *testing.T) {
	got, ok := imParseMalformedStatus(`Post "https://im.example.com/pubacc/pubsend": net/http: HTTP/1.x transport connection broken: malformed HTTP status code "5005"`)
	if !ok || got != 5005 {
		t.Fatalf("提取失败: code=%d ok=%v", got, ok)
	}
	if _, ok := imParseMalformedStatus("network timeout"); ok {
		t.Fatal("普通网络错误不应提取出状态码")
	}
}

// TestIMSendPubMessage_Retry5005 频控重试：首响 5005（同内容重复）→ 加空格变内容重发
// → 第二响成功；验证重试后到达服务端的 msg.text 与首次不同。
func TestIMSendPubMessage_Retry5005(t *testing.T) {
	var texts []string
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		texts = append(texts, body["msg"].(map[string]any)["text"].(string))
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"code":5005,"message":"发送太快啦，稍后再试","success":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"发送成功","success":true}`))
	}))
	defer srv.Close()

	ch := imTestChannel()
	ch.cfg.APIBase = srv.URL
	ch.sendPubMessage("openid-x", "重复内容")

	if calls != 2 {
		t.Fatalf("请求次数 = %d，want 2（5005 退避重试一次）", calls)
	}
	if texts[1] == texts[0] {
		t.Fatal("重试时 msg.text 未变化，无法规避 5005 同内容判定")
	}
}

// TestIMSendPubMessage_MalformedStatus5005 文档 pubsend 失败形态端到端：
// 平台失败响应为非标准 4 位 HTTP 状态码（HTTP/1.1 5005 ...，Go net/http
// 解析失败直接断连）→ 须从错误文本提取 5005 触发频控退避重试。
func TestIMSendPubMessage_MalformedStatus5005(t *testing.T) {
	var calls int32
	var texts []string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srvDone := make(chan struct{})
	go func() { // httptest 不支持自定义状态行，裸 TCP 模拟平台响应
		defer close(srvDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			calls++
			buf := make([]byte, 4096)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, _ := conn.Read(buf)
			var body map[string]any
			_ = json.Unmarshal(buf[bytes.LastIndexByte(buf[:n], '{'):n], &body)
			if m, ok := body["msg"].(map[string]any); ok {
				texts = append(texts, m["text"].(string))
			}
			if calls == 1 {
				fmt.Fprint(conn, "HTTP/1.1 5005 Unknown Status (5005)\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 2\r\n\r\n发送太快啦")
			} else {
				fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 60\r\n\r\n{\"pubId\":\"XT-p1\",\"sourceMsgId\":\"XT-m1\"}")
			}
			conn.Close()
		}
	}()

	base := "http://" + ln.Addr().String()

	ch := imTestChannel()
	ch.cfg.APIBase = base
	ch.sendPubMessage("openid-z", "畸形状态码重试")

	if calls != 2 {
		t.Fatalf("请求次数 = %d，want 2（畸形状态码 5005 也应触发频控退避重试）", calls)
	}
	if len(texts) == 2 && texts[1] == texts[0] {
		t.Fatal("重试时 msg.text 未变化")
	}
}

// TestIMSendPubMessage_HttpError 非 JSON 响应体（平台异常返回 HTML）+ HTTP 500：
// 旧实现此处静默通过（无日志、无失败标记），修复后须按失败落日志。
func TestIMSendPubMessage_HttpError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer srv.Close()

	ch := imTestChannel()
	ch.cfg.APIBase = srv.URL
	ch.sendPubMessage("openid-y", "探活")

	if calls != 1 {
		t.Fatalf("请求次数 = %d，want 1（HTTP 500 不可重试，避免重复发送风险）", calls)
	}
}

// TestIMSendWebhook 群机器人推送（httptest 假服务）：APIBase+WebhookToken 回退形态。
func TestIMSendWebhook(t *testing.T) {
	var gotContent string
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway/robot/webhook/send" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		gotToken = r.URL.Query().Get("yzjtoken")
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(b, &body)
		gotContent = body.Content
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := &IMChannel{cfg: ChannelConfig{Type: "im", Name: "测试", APIBase: srv.URL, WebhookToken: "tok-123"}}
	ch.sendWebhook("大家好,@ALL")

	if gotToken != "tok-123" {
		t.Fatalf("yzjtoken = %q, want tok-123", gotToken)
	}
	if gotContent != "大家好,@ALL" {
		t.Fatalf("content = %q", gotContent)
	}
}

// TestIMSendWebhookURLTemplate 完整 Webhook URL 直配 + 出参模板渲染：
// 任意平台 Webhook（WebhookURL）+ OutTemplate {{text}} 占位符替换。
func TestIMSendWebhookURLTemplate(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := &IMChannel{cfg: ChannelConfig{
		Type:        "im",
		Name:        "通用平台",
		WebhookURL:  srv.URL + "/hook?token=abc",
		OutTemplate: `{"msgtype":"text","text":{"content":"{{text}}"}}`,
	}}
	ch.sendWebhook("跨平台回复\n第二行")

	var parsed struct {
		MsgType string `json:"msgtype"`
		Text    struct {
			Content string `json:"content"`
		} `json:"text"`
	}
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("模板渲染结果非合法 JSON: %v (%s)", err, gotBody)
	}
	if parsed.MsgType != "text" || parsed.Text.Content != "跨平台回复\n第二行" {
		t.Fatalf("模板渲染错误: %+v", parsed)
	}
}

// TestIMRenderTemplate 出参模板渲染单元：占位符替换 + 无占位符原样 + 空模板默认。
func TestIMRenderTemplate(t *testing.T) {
	if got := imRenderTemplate(`{"content":"{{text}}"}`, "hi"); got != `{"content":"hi"}` {
		t.Fatalf("占位符替换失败: %q", got)
	}
	// JSON 字符串安全转义：引号/反斜杠/换行（多行文本不得破坏 JSON 结构）。
	if got := imRenderTemplate(`{"content":"{{text}}"}`, "a\"b\\c"); got != `{"content":"a\"b\\c"}` {
		t.Fatalf("引号/反斜杠转义失败: %q", got)
	}
	if got := imRenderTemplate(`{"content":"{{text}}"}`, "L1\nL2"); got != "{\"content\":\"L1\\nL2\"}" {
		t.Fatalf("换行转义失败: %q", got)
	}
	// 多行文本 + 出参模板端到端：渲染结果必须是合法 JSON。
	body := imRenderTemplate(`{"msgtype":"text","text":{"content":"{{text}}"}}`, "L1\nL2\"q\"")
	var probe map[string]any
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		t.Fatalf("多行+引号文本渲染结果非合法 JSON: %v (%s)", err, body)
	}
	if got := imRenderTemplate(`fixed`, "hi"); got != "fixed" {
		t.Fatalf("无占位符应原样返回: %q", got)
	}
}

// 小工具：sortStrings + sha1Hex（测试仅用，避免与生产代码耦合）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
func sha1Hex(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// TestIMParseGroupHooks hookID 列表解析：逗号分隔 + 去空 + 去重 + 保序。
func TestIMParseGroupHooks(t *testing.T) {
	ch := imTestChannel()
	ch.cfg.GroupHookIDs = "group-a, group-b ,,group-a,group-c"
	got := ch.parseGroupHooks()
	want := []string{"group-a", "group-b", "group-c"}
	if len(got) != len(want) {
		t.Fatalf("hook 数量 = %v，want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hook[%d] = %q，want %q", i, got[i], want[i])
		}
	}
}

// TestIMParseGroupPayload 宽松解析 + InMap 显式映射：
//   - 无 InMap：多候选字段 + 嵌套提取 + 非 JSON 退纯文本（兜底不变）。
//   - 有 InMap：显式映射优先，命中显式源；宽松解析不覆盖。
func TestIMParseGroupPayload(t *testing.T) {
	// 平台典型 JSON：嵌套 payload/msg 携带文本与发送者（大小写/嵌套不敏感解析）。
	ch := imTestChannel()
	body := []byte(`{"EventKey":"group-a","fromUserId":"u-123","Payload":{"fromUserName":"张三","Content":"帮我看下构建日志"}}`)
	m := ch.parseGroupPayload(body)
	if m.MsgID != "" {
		t.Fatalf("无 MsgId 载荷不应误取 MsgID: %+v", m)
	}
	if m.Sender != "u-123" || m.SenderName != "张三" {
		t.Fatalf("字段提取错误: %+v", m)
	}
	if !strings.Contains(m.Text, "帮我看下构建日志") {
		t.Fatalf("文本提取错误: %q", m.Text)
	}

	// 非 JSON：退纯文本。
	m2 := ch.parseGroupPayload([]byte("纯文本推送"))
	if m2.Text != "纯文本推送" {
		t.Fatalf("非 JSON 应退纯文本: %q", m2.Text)
	}

	// InMap 显式映射：text=Payload.Content 命中显式源。
	ch2 := imTestChannel()
	ch2.cfg.GroupInMap = map[string]string{
		"text":   "Payload.Content",
		"sender": "fromUserId",
		"msgId":  "MsgId",
	}
	body2 := []byte(`{"MsgId":"XT-m1","fromUserId":"u-9","Payload":{"Content":"显式映射文本"}}`)
	m3 := ch2.parseGroupPayload(body2)
	if m3.Text != "显式映射文本" || m3.Sender != "u-9" || m3.MsgID != "XT-m1" {
		t.Fatalf("InMap 映射错误: %+v", m3)
	}

	// InMap 源未命中 → 回退宽松解析（该字段仍能提取）。
	ch3 := imTestChannel()
	ch3.cfg.GroupInMap = map[string]string{"text": "不存在的路径.Content"}
	m4 := ch3.parseGroupPayload([]byte(`{"Content":"兜底提取"}`))
	if m4.Text != "兜底提取" {
		t.Fatalf("InMap 未命中应回退宽松解析: %+v", m4)
	}
}

// TestIMLookupPath InMap 源路径解析：点分嵌套 + 大小写不敏感 + 数组下标。
func TestIMLookupPath(t *testing.T) {
	obj := map[string]any{
		"Payload": map[string]any{"Content": "hello"},
		"msgList": []any{map[string]any{"Text": "arr-text"}},
	}
	flat := map[string]string{}
	imFlattenJSON("", obj, flat)

	if v := imLookupPath(flat, "Payload.Content"); v != "hello" {
		t.Fatalf("点分路径失败: %q", v)
	}
	if v := imLookupPath(flat, "payload.content"); v != "hello" {
		t.Fatalf("大小写不敏感失败: %q", v)
	}
	if v := imLookupPath(flat, "msgList[0].Text"); v != "arr-text" {
		t.Fatalf("数组下标失败: %q", v)
	}
	if v := imLookupPath(flat, ""); v != "" {
		t.Fatalf("空路径应返回空: %q", v)
	}
	if v := imLookupPath(flat, "no.such.path"); v != "" {
		t.Fatalf("未命中应返回空: %q", v)
	}
}

// TestIMHandleGroupMsg 群接收端点全链路（httptest）：路由命中/未命中、
// GET 探活、POST 受理应答、onGroupMsg 路由（app=nil 不触发引擎）。
func TestIMHandleGroupMsg(t *testing.T) {
	ch := imTestChannel()
	ch.cfg.GroupHookIDs = "group-a,group-b"

	// GET 探活。
	req := httptest.NewRequest(http.MethodGet, "/im/group/group-a", nil)
	rec := httptest.NewRecorder()
	ch.handleGroupMsg(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "group-a") {
		t.Fatalf("GET 探活失败: %d %q", rec.Code, rec.Body.String())
	}

	// POST 命中 hookID → 200 + success JSON。
	req2 := httptest.NewRequest(http.MethodPost, "/im/group/group-a",
		strings.NewReader(`{"MsgId":"XT-m2","FromUserName":"u-1","Payload":{"Content":"你好群"}}`))
	rec2 := httptest.NewRecorder()
	ch.handleGroupMsg(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("POST 应 200，got %d body=%q", rec2.Code, rec2.Body.String())
	}
	// 受理应答 = chatbot 文档格式：success=true + data.type=2 + 非空 content
	// （data.type/content 缺省即被平台判「返回的数据格式错误」）。
	var resp imPubResp
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil || !resp.Success {
		t.Fatalf("响应错误: %s err=%v", rec2.Body.String(), err)
	}
	if resp.Data.Type != 2 || resp.Data.Content == "" {
		t.Fatalf("受理应答非 chatbot 文档格式（data.type=2+content 必填）: %s", rec2.Body.String())
	}

	// 未配置 hookID → 404；空 ID → 400。
	req3 := httptest.NewRequest(http.MethodPost, "/im/group/unknown", strings.NewReader("{}"))
	rec3 := httptest.NewRecorder()
	ch.handleGroupMsg(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("未知 hook 应 404，got %d", rec3.Code)
	}
	req4 := httptest.NewRequest(http.MethodPost, "/im/group/", strings.NewReader("{}"))
	rec4 := httptest.NewRecorder()
	ch.handleGroupMsg(rec4, req4)
	if rec4.Code != http.StatusBadRequest {
		t.Fatalf("缺 hookID 应 400，got %d", rec4.Code)
	}

	// 云之家 chatbot 平台探活测试请求（创建机器人时发起，robotId=test-robotId）：
	// 回文档示例文案、不建任务（app=nil 场景 onGroupMsg 本就不触发引擎，此处
	// 验证探活请求走专用分支且应答为文档示例文案）。
	probeBody := `{"type":2,"robotId":"test-robotId","robotName":"test-robotName","operatorOpenid":"test-userId","operatorName":"test-userName","time":1599727083000,"msgId":"test-msgId","content":"你好，你能做什么呢?"}`
	req5 := httptest.NewRequest(http.MethodPost, "/im/group/group-a", strings.NewReader(probeBody))
	rec5 := httptest.NewRecorder()
	ch.handleGroupMsg(rec5, req5)
	var probeResp imPubResp
	if err := json.Unmarshal(rec5.Body.Bytes(), &probeResp); err != nil || !probeResp.Success {
		t.Fatalf("探活应答解析失败: %d %q err=%v", rec5.Code, rec5.Body.String(), err)
	}
	if probeResp.Data.Type != 2 || probeResp.Data.Content != "好风凭借力，送我上青云" {
		t.Fatalf("探活应答非文档示例格式: %s", rec5.Body.String())
	}
}

// TestIMOnGroupMsgDedupe 群消息去重：MsgID 优先，内容哈希兑底。

// TestIMOnGroupMsgDedupe 群消息去重：MsgID 优先，内容哈希兜底。
func TestIMOnGroupMsgDedupe(t *testing.T) {
	ch := imTestChannel()
	ch.cfg.GroupHookIDs = "group-a"
	msg := imGroupMsg{MsgID: "XT-m9", Text: "重复消息"}
	if ch.seenMsg("grp|group-a|" + msg.MsgID) {
		t.Fatal("首次不应判重")
	}
	if !ch.seenMsg("grp|group-a|" + msg.MsgID) {
		t.Fatal("重复应判重")
	}
	// 内容哈希兜底路径。
	sum := sha1.Sum([]byte("group-a|重复消息"))
	key := "hash-" + hex.EncodeToString(sum[:8])
	if ch.seenMsg("grp|group-a|" + key) {
		t.Fatal("内容哈希首见不应判重")
	}
	if !ch.seenMsg("grp|group-a|" + key) {
		t.Fatal("内容哈希重复应判重")
	}
}
