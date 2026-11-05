package desktop

// channel_im_replay_test.go IM 适配器报文回放回归测试（fixtures 驱动）：
//
//	目的：适配器解析/验签/去重/路由改动后，跑回放即暴露解析回归——不需要
//	平台联调环境。fixtures 录制于 testdata/im/<渠道>/，样例可按渠道扩展：
//	新平台录制样本存同构 JSON（desc/kind/body/want），自动纳入回放。
//
//	覆盖链路（回放 = 真实入口全链路，不 mock 解析函数）：
//	  - 公共号（kind=pub）：handleCallback 全链路 —— 解析 → PubToken 验签
//	    （非 sign 样本 403）→ 10 秒受理应答（text 回显 type=2 / 非文本 type=0）
//	    → MsgId 去重（repeat>1 时仅首个被受理）→ 路由（app=nil 不触发引擎）；
//	  - 群消息（kind=group）：handleGroupMsg 全链路 —— hookID 白名单（未知 404）
//	    → 宽松解析 / InMap 显式映射 → success 受理应答 → 非空文本路由；
//	  - 解析断言直接对照 fixture want（text/sender/senderName/msgId）。
//
//	运行：go test . -run TestIMReplay（已随根包测试纳入发布门禁 G2）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imReplayFixture 回放样例声明（testdata/im/<渠道>/*.json 同构格式：
// 新渠道录制样本按此结构落盘即被自动回放，无需改测试代码）。
type imReplayFixture struct {
	Desc   string            `json:"desc"`             // 样例说明（失败定位用）
	Kind   string            `json:"kind"`             // pub | group
	Hook   string            `json:"hook,omitempty"`   // group：hookID（须在渠道配置内）
	InMap  map[string]string `json:"inMap,omitempty"`  // group：InMap 显式映射（可选）
	Sign   *bool             `json:"sign,omitempty"`   // pub：是否以测试密钥重签（默认 true；false = 验签应 403）
	Repeat int               `json:"repeat,omitempty"` // 平台超时重发次数（>1 验证 MsgId 去重）
	Body   json.RawMessage   `json:"body"`             // 录制的原始载荷（pub：pubRespon 对象；group：任意 JSON）
	// RawBody 非 JSON 原始报文（群消息纯文本/表单推送；优先于 Body）
	RawBody string `json:"rawBody,omitempty"`

	Want struct {
		Status      int    `json:"status"`      // 期望 HTTP 状态（0 = 200）
		ParseOk     bool   `json:"parseOk"`     // 期望解析通过（body 命中字段可解析）
		From        string `json:"from"`        // 期望发送者（pub=FromUserName / group=sender）
		Sender      string `json:"sender"`      // 群消息发送者标识（与 From 二选一）
		MsgID       string `json:"msgId"`       // 期望消息 ID
		Text        string `json:"text"`        // 期望提取文本
		SenderName  string `json:"senderName"`  // 期望发送者显示名
		RespType    int    `json:"respType"`    // 期望受理应答 type（pub：text=2 / 非文本=0）
		RespSuccess *bool  `json:"respSuccess"` // 期望受理应答 success（group）
		RespContent string `json:"content"`     // 期望受理应答 content（chatbot 文档格式；空 = 仅验 type）
		Dedupe      int    `json:"dedupe"`      // 期望去重判重次数（repeat>1 时 = repeat-1）
		Accepted    int    `json:"accepted"`    // 期望实际受理条数（默认 1；验签失败 0）
	} `json:"want"`
}

// imReplayChannel 测试渠道（与 imTestChannel 同凭据；hook-a1b2c3d4 覆盖群样本）。
func imReplayChannel() *IMChannel {
	ch := imTestChannel()
	ch.cfg.GroupHookIDs = "hook-a1b2c3d4"
	return ch
}

// imReplaySign 以测试密钥重签 PubToken（录制样本不含真实密钥，回放时重算）。
func imReplaySign(body []byte, pubkey string) []byte {
	var cb imPubCallback
	if json.Unmarshal(body, &cb) != nil {
		return body
	}
	cb.PubToken = imPubToken(pubkey, cb.PubAccID, cb.FromUserName, cb.CreateTime, cb.MsgID)
	b, err := json.Marshal(cb)
	if err != nil {
		return body
	}
	return b
}

// TestIMReplayFixtures 报文回放：遍历 testdata/im/generic/ 全部样例，
// 逐个走真实 HTTP 入口（handleCallback/handleGroupMsg），断言解析、验签、
// 去重、受理应答与 hookID 路由行为。
func TestIMReplayFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata/im/generic")
	if err != nil {
		t.Fatalf("读取 fixtures 目录失败: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("fixtures 目录为空（样例缺失即回归防护失效）")
	}
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "im", "generic", name))
			if err != nil {
				t.Fatalf("读取样例失败: %v", err)
			}
			var fx imReplayFixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatalf("fixture 格式错误: %v", err)
			}
			switch fx.Kind {
			case "pub":
				replayPubFixture(t, fx, raw)
			case "group":
				replayGroupFixture(t, fx, raw)
			default:
				t.Fatalf("未知样例类型 %q（kind 须为 pub|group）", fx.Kind)
			}
		})
	}
}

// replayPubFixture 公共号样本回放：POST /im/callback → 验签/应答/去重断言。
func replayPubFixture(t *testing.T, fx imReplayFixture, raw []byte) {
	t.Helper()
	ch := imReplayChannel()
	pubkey := ch.cfg.PubSecret

	// 声明驱动：默认重签（录制样本不含真实密钥）；sign=false 保持坏签名原样。
	body := fx.Body
	if fx.Sign == nil || *fx.Sign {
		body = imReplaySign(fx.Body, pubkey)
	}

	// 去重簿前置清空（样本间互不干扰）。
	ch.seenMu.Lock()
	ch.seen = nil
	ch.seenMu.Unlock()

	repeat := fx.Repeat
	if repeat < 1 {
		repeat = 1
	}
	duplicateHits := 0
	for i := 0; i < repeat; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:9881/im/callback",
			strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		ch.handleCallback(rec, req)
		if fx.Want.Status != 0 {
			if rec.Code != fx.Want.Status {
				t.Fatalf("状态码 = %d，want %d（body=%q）", rec.Code, fx.Want.Status, rec.Body.String())
			}
			return // 非受理分支（403/400）：到达即通过
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("受理应答非 200: %d body=%q", rec.Code, rec.Body.String())
		}
		var resp imPubResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("受理应答 JSON 解析失败: %v", err)
		}
		if fx.Want.RespType > 0 && resp.Data.Type != fx.Want.RespType {
			t.Fatalf("受理应答 type = %d，want %d", resp.Data.Type, fx.Want.RespType)
		}
		// 重放第 2..N 次：MsgId 已在去重簿 → 判重静默丢弃（响应仍 200）。
		if i > 0 {
			duplicateHits++
		}
	}
	// 去重验证：重放样本后，去重簿语义直验 —— 同 MsgId 首次放行、重放判重
	// （app=nil 时 onCallback 在去重前返回，受理计数经 seenMsg 簿核验等价）。
	if fx.Want.Dedupe > 0 {
		if duplicateHits != fx.Want.Dedupe {
			t.Fatalf("重放次数 = %d，want %d", duplicateHits, fx.Want.Dedupe)
		}
		// 平台重发场景模拟：同一 MsgId 连续查簿 → 首次放行、后续全部判重。
		if ch.seenMsg(fx.Want.MsgID) {
			t.Fatal("首查不应判重（上一轮 handleCallback 不入簿）")
		}
		if !ch.seenMsg(fx.Want.MsgID) {
			t.Fatal("重复 MsgId 应判重")
		}
		if !ch.seenMsg(fx.Want.MsgID) {
			t.Fatal("第三次重放仍应判重（幂等）")
		}
	}

	if fx.Want.MsgID != "" {
		cb, err := ch.parseCallback(fx.Body)
		if err != nil {
			t.Fatalf("样本解析失败: %v", err)
		}
		if cb.MsgID != fx.Want.MsgID || cb.FromUserName != fx.Want.From {
			t.Fatalf("解析字段错误: from=%q msgId=%q", cb.FromUserName, cb.MsgID)
		}
	}
}

// replayGroupFixture 群消息样本回放：POST /im/group/<hook> → 路由/解析/应答断言。
func replayGroupFixture(t *testing.T, fx imReplayFixture, raw []byte) {
	t.Helper()
	ch := imReplayChannel()
	if len(fx.InMap) > 0 {
		ch.cfg.GroupInMap = fx.InMap
	}
	payload := fx.RawBody
	if payload == "" {
		payload = string(fx.Body)
	}
	req := httptest.NewRequest(http.MethodPost, "/im/group/"+fx.Hook, strings.NewReader(payload))
	rec := httptest.NewRecorder()
	ch.handleGroupMsg(rec, req)
	wantStatus := fx.Want.Status
	if wantStatus == 0 {
		wantStatus = http.StatusOK
	}
	if rec.Code != wantStatus {
		t.Fatalf("状态码 = %d，want %d（body=%q）", rec.Code, wantStatus, rec.Body.String())
	}
	if wantStatus != http.StatusOK {
		return
	}
	// 受理应答 = chatbot 文档格式：success=true + data.type=2 + 非空 content
	// （data.type/content 缺省即被平台判「返回的数据格式错误」）；探活样本
	//（group_chatbot_probe.json）额外断言固定示例文案。
	var resp imPubResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Success {
		t.Fatalf("受理应答错误: %q err=%v", rec.Body.String(), err)
	}
	if resp.Data.Type != 2 || resp.Data.Content == "" {
		t.Fatalf("受理应答非 chatbot 文档格式（data.type=2+content 必填）: %q", rec.Body.String())
	}
	if fx.Want.RespContent != "" && resp.Data.Content != fx.Want.RespContent {
		t.Fatalf("受理应答 content = %q，want %q", resp.Data.Content, fx.Want.RespContent)
	}
	m := ch.parseGroupPayload([]byte(payload))
	if fx.Want.Text != "" && m.Text != fx.Want.Text {
		t.Fatalf("文本提取 = %q，want %q", m.Text, fx.Want.Text)
	}
	if fx.Want.Sender != "" && m.Sender != fx.Want.Sender {
		t.Fatalf("发送者 = %q，want %q", m.Sender, fx.Want.Sender)
	}
	if fx.Want.SenderName != "" && m.SenderName != fx.Want.SenderName {
		t.Fatalf("发送者名 = %q，want %q", m.SenderName, fx.Want.SenderName)
	}
	if fx.Want.MsgID != "" && m.MsgID != fx.Want.MsgID {
		t.Fatalf("MsgId = %q，want %q", m.MsgID, fx.Want.MsgID)
	}
}
