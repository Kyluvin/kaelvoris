package desktop

// channel_wecom_test.go 企微通道运行时单测：
//   - WXBizMsgCrypt 加解密 round-trip（wecomEncrypt → wecomDecrypt）
//   - 回调签名 wecomSignature 确定性 + hex 长度
//   - handleCallback GET 验证 URL 全链路（echostr 解密回显，httptest）
//   - decryptCallback POST 载荷验签 + 解密 + XML 解析
//   - MsgId 去重（seenMsg）
//
// 说明：合法 EncodingAESKey 为 43 位字符串（Base64 无填充，补 "=" 后解码为 32 字节），
// 测试用 43 个 'A'（解码后恰为 32 字节）。

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const wecomTestAESKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 43 位 → 32 字节密钥

func wecomTestChannel() *WeComChannel {
	return &WeComChannel{
		cfg: ChannelConfig{
			Type:           "wecom",
			Name:           "测试企微",
			CorpID:         "ww1234567890abcdef",
			Token:          "test-token",
			EncodingAESKey: wecomTestAESKey,
		},
		running: map[string]bool{},
		pending: map[string]*pendingAsk{},
	}
}

// TestWeComCrypt_RoundTrip 加解密对称性：明文 + receiveid 经加密后可原样解出。
func TestWeComCrypt_RoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		msg       string
		receiveID string
	}{
		{"纯英文短消息", "hello wecom", "corp-id-1"},
		{"中文与符号", "你好，企微！任务完成 ✅", "ww1234567890abcdef"},
		{"空 receiveid 跳过校验", "msg", ""},
		{"长消息（多块填充）", strings.Repeat("内容", 500), "receive-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b64, err := wecomEncrypt(wecomTestAESKey, tc.msg, tc.receiveID)
			if err != nil {
				t.Fatalf("加密失败: %v", err)
			}
			got, err := wecomDecrypt(wecomTestAESKey, b64, tc.receiveID)
			if err != nil {
				t.Fatalf("解密失败: %v", err)
			}
			if got != tc.msg {
				t.Fatalf("round-trip 不一致: got %q want %q", got, tc.msg)
			}
		})
	}
}

// TestWeComDecrypt_ReceiveIDMismatch receiveid 校验（防跨企业重放）。
func TestWeComDecrypt_ReceiveIDMismatch(t *testing.T) {
	b64, err := wecomEncrypt(wecomTestAESKey, "hello", "corp-A")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if _, err := wecomDecrypt(wecomTestAESKey, b64, "corp-B"); err == nil {
		t.Fatal("receiveid 不匹配应报错，实际通过")
	}
}

// TestWeComDecrypt_BadKey 非法 EncodingAESKey（非 43 位）应拒绝。
func TestWeComDecrypt_BadKey(t *testing.T) {
	if _, err := wecomDecrypt("short-key", "AAAA", "corp"); err == nil {
		t.Fatal("非法 EncodingAESKey 应报错")
	}
}

// TestWeComSignature 签名确定性 + 格式（40 位 hex），参数乱序不影响结果。
func TestWeComSignature(t *testing.T) {
	sig1 := wecomSignature("token", "1409659813", "nonce-1", "encrypt-data")
	sig2 := wecomSignature("encrypt-data", "nonce-1", "token", "1409659813") // 乱序
	if len(sig1) != 40 {
		t.Fatalf("签名长度应为 40（sha1 hex），got %d", len(sig1))
	}
	if sig1 != sig2 {
		t.Fatalf("相同参数集签名应一致: %s vs %s", sig1, sig2)
	}
}

// TestWeComCallbackGETVerify URL 有效性验证：GET echostr → 解密原样回显明文。
func TestWeComCallbackGETVerify(t *testing.T) {
	ch := wecomTestChannel()
	plain := "1755878866123456"
	enc, err := wecomEncrypt(wecomTestAESKey, plain, ch.cfg.CorpID)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	sig := wecomSignature(ch.cfg.Token, "1409659866", "nonce-abc", enc)

	// echostr 为 Base64 密文（含 +/=），必须经 query 编码（真实企微回调同样如此）。
	q := url.Values{}
	q.Set("msg_signature", sig)
	q.Set("timestamp", "1409659866")
	q.Set("nonce", "nonce-abc")
	q.Set("echostr", enc)
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9880/wecom/callback?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	ch.handleCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body = %q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != plain {
		t.Fatalf("echostr 回显 = %q，want %q", got, plain)
	}
}

// TestWeComDecryptCallbackPOST POST 消息推送：验签 → 解密 → XML 字段解析。
func TestWeComDecryptCallbackPOST(t *testing.T) {
	ch := wecomTestChannel()
	xmlMsg := "<xml><ToUserName><![CDATA[CorpID]]></ToUserName><FromUserName><![CDATA[zhangsan]]></FromUserName><CreateTime>1755878866</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[运行冒烟测试]]></Content><MsgId>100000001</MsgId><AgentID>1000002</AgentID></xml>"
	enc, err := wecomEncrypt(wecomTestAESKey, xmlMsg, ch.cfg.CorpID)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	encXML := "<xml><Encrypt><![CDATA[" + enc + "]]></Encrypt><ToUserName><![CDATA[CorpID]]></ToUserName></xml>"
	sig := wecomSignature(ch.cfg.Token, "1409659866", "nonce-abc", enc)

	m, err := ch.decryptCallback(sig, "1409659866", "nonce-abc", []byte(encXML))
	if err != nil {
		t.Fatalf("解密回调失败: %v", err)
	}
	if m.MsgType != "text" {
		t.Fatalf("MsgType = %q，want text", m.MsgType)
	}
	if m.Content != "运行冒烟测试" {
		t.Fatalf("Content = %q，want 运行冒烟测试", m.Content)
	}
	if m.FromUserName != "zhangsan" {
		t.Fatalf("FromUserName = %q，want zhangsan", m.FromUserName)
	}
	if m.MsgID != 100000001 {
		t.Fatalf("MsgId = %d，want 100000001", m.MsgID)
	}

	// 签名错误应拒绝。
	if _, err := ch.decryptCallback("bad-signature", "1409659866", "nonce-abc", []byte(encXML)); err == nil {
		t.Fatal("签名校验失败时应报错")
	}
}

// TestWeComSeenMsg MsgId 去重：首次 false，10 分钟内重复 true。
func TestWeComSeenMsg(t *testing.T) {
	ch := wecomTestChannel()
	if ch.seenMsg(42) {
		t.Fatal("首次消息不应判重")
	}
	if !ch.seenMsg(42) {
		t.Fatal("10 分钟内重复消息应判重")
	}
	if ch.seenMsg(43) {
		t.Fatal("不同 MsgId 不应误判重")
	}
}
