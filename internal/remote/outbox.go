package remote

// outbox.go PC 节点上行可靠投递（P-Reliable）：消息不丢失核心实现。
//
// 模型（对齐需求「PC 端开线程一直向服务端发，服务端开线程一直收，
// 看的时候直接读服务端的消息，保证不丢失」）：
//
//	PC 端                                 服务端
//	  │ Send()/SendTaskEvent()/...          │
//	  ▼                                      │
//	outbox 内存队列 + journal 落盘            │
//	  │                                      │
//	  ▼ sender 线程（独立 goroutine）          │
//	队首 → 帧带 MsgID 发送 ─────────────────▶ 读线程（只收不处理）
//	  ▲                                      ▼
//	  │            server.ack(msgId) ◀──── worker 线程（处理+落盘 → 回执）
//	  │ ack：出队删除 + journal 记账           │ 失败 → server.send_failed（丢弃+告警）
//	  │ 断线：队列保留                        ▼
//	  │ 重连：sender 重新补发未确认帧          服务端按 MsgID 去重（幂等）
//	  ▼
//	journal：~/.kaelvoris/agent_outbox_<nodeID>.jsonl
//	（进程重启回放，未确认消息继续投递；写入即持久，崩溃不丢）
//
// 语义要点：
//   - Send 从「未连接静默丢弃」改为「入队等待连接」—— 断线期间产生的事件/终态
//     全部保留，重连后按序补发（修复「打着打着不打了」的根因：消息随连接丢失）；
//   - 串行发送 + 逐条等 ack（滑动窗口简化为停等）：事件流帧率远低于 RTT 上限，
//     吞吐足够；严格 FIFO 保证服务端镜像顺序与 PC 端产生顺序一致；
//   - 心跳/注册不走 outbox（连接级控制帧，随连接生灭，无补发语义）。
//
// journal 格式：JSONL，每行 {"op":"add","msgId":..,"env":{..}} 或 {"op":"ack","msgId":..}。
// 回放规则：按序应用 add/ack，未被 ack 的 add 即待发队列；回放后原子压缩重写。

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"
)

// outboxMsg 上行未确认消息（内存队列节点）。
type outboxMsg struct {
	Env   Envelope `json:"env"`             // 完整信封（含 MsgID）
	At    int64    `json:"at"`              // 首次入队毫秒时间戳（诊断）
	Tries int      `json:"tries,omitempty"` // 已尝试发送次数（诊断）
}

// journalOp journal 操作记录。
type journalOp struct {
	Op    string    `json:"op"` // "add" | "ack"
	MsgID string    `json:"msgId,omitempty"`
	Env   *Envelope `json:"env,omitempty"`
}

// initOutbox 初始化 outbox：回放 journal 恢复未确认消息，启动 sender 线程。
func (c *Client) initOutbox() {
	c.outboxMu.Lock()
	c.senderWake = make(chan struct{}, 1)
	c.ackNotify = make(chan struct{}, 1)
	stop := make(chan struct{})
	c.flushStop = stop
	path := c.journalPath
	c.outboxMu.Unlock()
	if path != "" {
		c.replayJournal(path)
	}
	go c.senderLoop(stop)
}

// closeOutbox 停止发送线程（Stop 调用；journal 保留，下次启动回放续发）。
func (c *Client) closeOutbox() {
	c.outboxMu.Lock()
	stop := c.flushStop
	c.flushStop = nil
	c.outboxMu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// enqueue 上行消息入队（分配 MsgID + journal 先落盘 + 唤醒发送线程）。
func (c *Client) enqueue(typ string, payload interface{}) {
	env := NewEnvelope("", typ, payload)
	env.MsgID = NewMsgID()
	m := &outboxMsg{Env: env, At: time.Now().UnixMilli()}
	c.outboxMu.Lock()
	c.outbox = append(c.outbox, m)
	c.outboxMu.Unlock()
	c.journalAppend(journalOp{Op: "add", MsgID: env.MsgID, Env: &env})
	c.wakeSender()
}

// wakeSender 唤醒发送线程（容量 1 合并多次唤醒；nil 安全：未初始化时忽略）。
func (c *Client) wakeSender() {
	c.outboxMu.Lock()
	wake := c.senderWake
	c.outboxMu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// senderLoop 发送线程主循环：醒来后尽力清空队列；stop 关闭退出。
func (c *Client) senderLoop(stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-c.senderWake:
		}
		c.drainOutbox(stop)
	}
}

// outboxPeek 队首快照（不移除；空返回 nil）。
func (c *Client) outboxHead() *outboxMsg {
	c.outboxMu.Lock()
	defer c.outboxMu.Unlock()
	if len(c.outbox) == 0 {
		return nil
	}
	return c.outbox[0]
}

// outboxPopHead 队首已确认（ack 到达）后由 removeOutbox 删除；此处仅读取。

// drainOutbox 停等式发送循环：发送队首 → 等 ack（8s 超时重发，服务端去重幂等）→
// 确认后取下一条。连接不可用即返回（等重连唤醒）；stop 关闭即退出。
func (c *Client) drainOutbox(stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		m := c.outboxHead()
		if m == nil {
			return // 队列空
		}
		c.mu.Lock()
		ws := c.ws
		online := c.online
		c.mu.Unlock()
		if ws == nil || !online {
			return // 未连接：保留队列，等重连唤醒补发
		}
		if err := c.writeWS(ws, m.Env); err != nil {
			// 写失败：连接大概率已断。短暂退避后重查在线态；连接恢复由
			// register_ok 唤醒（connectOnce）。
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		// 已发出，等待确认（ack → removeOutbox；send_failed → NACK 丢弃）。
		if c.waitAck(m.Env.MsgID, 8*time.Second, stop) {
			continue
		}
		// 超时未确认：重发同一帧（服务端按 MsgID 幂等去重，不会重复入账）。
	}
}

// waitAck 等待 msgID 被确认（从队列消失即 ack/nack 到达）。通知驱动
// （ackNotify 信号，避免轮询粒度的每消息延迟），stop 关闭返回 false。
func (c *Client) waitAck(msgID string, timeout time.Duration, stop chan struct{}) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if !c.outboxContains(msgID) {
			return true
		}
		notify := c.ackNotifySnapshot()
		if notify == nil {
			return false // sender 已停止
		}
		select {
		case <-deadline.C:
			return false
		case <-stop:
			return false
		case <-notify: // 任一 ack/nack 到达 → 重查队列
		}
	}
}

// pushAckNotify 通知信号（ackMsg/nackMsg 推送；容量 1 合并）。
func (c *Client) pushAckNotify() {
	c.outboxMu.Lock()
	notify := c.ackNotify
	c.outboxMu.Unlock()
	if notify == nil {
		return
	}
	select {
	case notify <- struct{}{}:
	default:
	}
}

// ackNotifySnapshot 获取通知通道快照（nil = sender 未启动/已停止）。
func (c *Client) ackNotifySnapshot() chan struct{} {
	c.outboxMu.Lock()
	defer c.outboxMu.Unlock()
	return c.ackNotify
}

// outboxContains msgID 是否仍在未确认队列。
func (c *Client) outboxContains(msgID string) bool {
	c.outboxMu.Lock()
	defer c.outboxMu.Unlock()
	for _, m := range c.outbox {
		if m.Env.MsgID == msgID {
			return true
		}
	}
	return false
}

// ackMsg 服务端确认（TypeServerAck）：从队列删除 + journal 记账。
func (c *Client) ackMsg(msgID string) {
	if msgID == "" {
		return
	}
	c.outboxMu.Lock()
	for i, m := range c.outbox {
		if m.Env.MsgID == msgID {
			c.outbox = append(c.outbox[:i], c.outbox[i+1:]...)
			break
		}
	}
	c.outboxMu.Unlock()
	c.journalAppend(journalOp{Op: "ack", MsgID: msgID})
	c.pushAckNotify()
}

// nackMsg 服务端明确拒绝（TypeServerSendFailed）：丢弃并告警。
// 重发无意义（处理失败是确定性的：未知类型/处理器 panic），保留只会无限重试；
// 真正的网络丢帧表现为「无 ack 也无 nack」，由 drainOutbox 超时重发兜底。
// nackMsg 服务端明确拒绝（TypeServerSendFailed）：丢弃并告警。
// 重发无意义（处理失败是确定性的：未知类型/处理器 panic），保留只会无限重试；
// 真正的网络丢帧表现为「无 ack 也无 nack」，由 drainOutbox 超时重发兑底。
func (c *Client) nackMsg(msgID, reason string) {
	if msgID == "" {
		return
	}
	c.ackMsg(msgID) // 同样出队 + journal 记账（终止重发）
	slog.Warn("服务端拒绝上行消息（已丢弃，不重发）", "msgId", msgID, "reason", reason)
}

// ---------- journal 持久化 ----------

// journalAppend 追加一条操作记录（同步写：发送线程崩溃前消息已可恢复）。
func (c *Client) journalAppend(op journalOp) {
	c.journalMu.Lock()
	path := c.journalPath
	c.journalMu.Unlock()
	if path == "" {
		return
	}
	b, err := json.Marshal(op)
	if err != nil {
		return
	}
	c.journalMu.Lock()
	defer c.journalMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// replayJournal 启动回放：按序应用 add/ack 还原未确认队列，随后压缩重写。
func (c *Client) replayJournal(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return // 无文件/不可读：空队列起步
	}
	type slot struct {
		msg  *outboxMsg
		done bool
	}
	var list []slot
	byID := map[string]int{} // msgID → list 下标
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var op journalOp
		if json.Unmarshal([]byte(line), &op) != nil {
			continue // 损坏行（进程崩溃半写）：跳过
		}
		switch op.Op {
		case "add":
			if op.Env == nil || op.Env.MsgID == "" {
				continue
			}
			if _, ok := byID[op.Env.MsgID]; ok {
				continue
			}
			byID[op.Env.MsgID] = len(list)
			list = append(list, slot{msg: &outboxMsg{Env: *op.Env}})
		case "ack":
			if idx, ok := byID[op.MsgID]; ok && !list[idx].done {
				list[idx].done = true
			}
		}
	}
	c.outboxMu.Lock()
	c.outbox = c.outbox[:0]
	for _, s := range list {
		if !s.done && s.msg != nil {
			c.outbox = append(c.outbox, s.msg)
		}
	}
	n := len(c.outbox)
	c.outboxMu.Unlock()
	c.compactJournal(path)
	if n > 0 {
		slog.Info("outbox 回放：恢复上次未确认的上行消息，重连后补发", "count", n)
	}
}

// compactJournal 原子重写 journal 为当前未确认队列快照（tmp + rename 防半写）。
func (c *Client) compactJournal(path string) {
	c.journalMu.Lock()
	defer c.journalMu.Unlock()
	c.outboxMu.Lock()
	snap := make([]Envelope, 0, len(c.outbox))
	for _, m := range c.outbox {
		snap = append(snap, m.Env)
	}
	c.outboxMu.Unlock()
	var buf strings.Builder
	for i := range snap {
		op := journalOp{Op: "add", MsgID: snap[i].MsgID, Env: &snap[i]}
		b, err := json.Marshal(op)
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// outboxLen 当前未确认消息数（状态页/诊断用）。
func (c *Client) outboxLen() int {
	c.outboxMu.Lock()
	defer c.outboxMu.Unlock()
	return len(c.outbox)
}
