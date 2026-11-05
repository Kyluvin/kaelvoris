package desktop

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// channel_push.go 通道外发消息的有界队列 + 单 worker（慢消费者背压）：
//
// 背景：请求路径（企微 WS 读循环 / 飞书事件回调 / 引擎 say 桥）不能同步等待
// 消费者（HTTP API / WS ACK），否则慢消费者会拖住主循环；本件把「发送」统一
// 降为「投递闭包」：非阻塞入队，worker 串行消费（网络路径单飞，天然串行化），
// 队列满即丢弃并记日志 + 累计计数（丢失有界、可观测，不静默）。
//
// 关闭语义：ctx 取消即退出，未消费的消息随通道停止一并丢弃（与停止一致）。
//
// ponytail: 容量为包级常量而非配置字段；若线上确需按渠道调整容量，
// 升级路径 = ChannelConfig 增加 PushQueueCap 并在 newPushQueue 处读取。

const channelPushQueueCap = 256 // 单渠道推送队列容量（满则丢弃，丢失有界）

// pushQueue 有界队列（闭包项）。
type pushQueue struct {
	name     string
	items    chan func()
	dropped  atomic.Int64 // 累计丢弃数（队列满）
	lastBusy atomic.Int64 // 上次高水位日志时间（UnixNano，防刷屏）
}

func newPushQueue(name string, capacity int) *pushQueue {
	if capacity <= 0 {
		capacity = channelPushQueueCap
	}
	return &pushQueue{name: name, items: make(chan func(), capacity)}
}

// enqueue 非阻塞投递；队列满返回 false（丢弃）并打水位日志。
func (q *pushQueue) enqueue(f func()) bool {
	select {
	case q.items <- f:
		q.logHighWater()
		return true
	default:
		n := q.dropped.Add(1)
		q.logDrop(n)
		return false
	}
}

// logDrop 丢弃日志节流：每分钟最多 1 条（丢弃风暴不淹没日志），
// 计数值随每次丢弃递增（可观测：检视日志即可见丢弃规模）。
func (q *pushQueue) logDrop(total int64) {
	now := time.Now().UnixNano()
	last := q.lastBusy.Load()
	if now-last < int64(time.Minute) {
		return
	}
	if q.lastBusy.CompareAndSwap(last, now) {
		slog.Warn("渠道推送背压：队列已满，消息丢弃（丢弃有界，可由平台重推/完成回执兜底）",
			"queue", q.name, "cap", cap(q.items), "depth", len(q.items), "dropped", total)
	}
}

// run worker 主循环：串行消费直到 ctx 取消；单条发送 panic 不拖垮 worker。
func (q *pushQueue) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-q.items:
			func() {
				defer func() { _ = recover() }()
				f()
			}()
		}
	}
}

// logHighWater 高水位日志：队列占用 ≥3/4 且距上次日志 ≥1 分钟（防刷屏）。
func (q *pushQueue) logHighWater() {
	if len(q.items)*4 < cap(q.items)*3 {
		return
	}
	now := time.Now().UnixNano()
	last := q.lastBusy.Load()
	if now-last < int64(time.Minute) {
		return
	}
	if q.lastBusy.CompareAndSwap(last, now) {
		slog.Warn("渠道推送队列高水位（慢消费者迹象）",
			"queue", q.name, "depth", len(q.items), "cap", cap(q.items), "dropped", q.dropped.Load())
	}
}
