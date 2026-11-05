// eventbus_mem.go：内存版事件总线（替代 NSQ/Kafka/NATS/Pulsar/RocketMQ）。
//
// 实现 coze 的 infra/eventbus.Producer（Send/BatchSend，topic 在构造时绑定，
// 对齐 coze NewProducer(nameServer, topic, group, retries) 语义）与
// ConsumerService.RegisterConsumer（nameServer/topic/group + handler）：
// 单进程内 topic → 消费者组直投递，每条消息起 goroutine 异步消费，
// 消费失败仅记日志（与 MQ 的 at-most-once 宽松语义一致，进程内可接受）。
package cozeflow

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/coze-dev/coze-studio/backend/infra/eventbus"
)

// MemEventBus 内存事件总线：同时实现 Producer 与 ConsumerService。
type MemEventBus struct {
	mu sync.RWMutex
	// consumers: topic → group → handler（同名 group 覆盖，语义上竞争消费）。
	consumers map[string]map[string]eventbus.ConsumerHandler
}

// NewMemEventBus 创建内存事件总线。
func NewMemEventBus() *MemEventBus {
	return &MemEventBus{consumers: make(map[string]map[string]eventbus.ConsumerHandler)}
}

// NewProducer 构造绑定 topic 的 Producer（topic 在 coze 侧由常量传入）。
func (b *MemEventBus) NewProducer(topic string) eventbus.Producer {
	return &memProducer{bus: b, topic: topic}
}

// deliver 投递消息到指定 topic 的全部消费者组（memProducer 专用入口）。
func (b *MemEventBus) deliver(ctx context.Context, topic string, bodyArr [][]byte) {
	b.mu.RLock()
	var handlers []eventbus.ConsumerHandler
	for _, h := range b.consumers[topic] {
		handlers = append(handlers, h)
	}
	b.mu.RUnlock()
	for _, body := range bodyArr {
		for _, h := range handlers {
			h := h
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("cozeflow: 事件消费 panic", "topic", topic, "err", r)
					}
				}()
				if err := h.HandleMessage(ctx, &eventbus.Message{Topic: topic, Body: body}); err != nil {
					slog.Error("cozeflow: 事件消费失败", "topic", topic, "err", err)
				}
			}()
		}
	}
}

// RegisterConsumer 注册消费者组（同 topic+group 幂等覆盖）。
func (b *MemEventBus) RegisterConsumer(_, topic, group string, handler eventbus.ConsumerHandler, _ ...eventbus.ConsumerOpt) error {
	if topic == "" || group == "" || handler == nil {
		return fmt.Errorf("eventbus: topic/group/handler 均不能为空")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.consumers[topic] == nil {
		b.consumers[topic] = make(map[string]eventbus.ConsumerHandler)
	}
	b.consumers[topic][group] = handler
	return nil
}

// memProducer 绑定 topic 的生产者视图。
type memProducer struct {
	bus   *MemEventBus
	topic string
}

// Send 投递单条消息。
func (p *memProducer) Send(ctx context.Context, body []byte, _ ...eventbus.SendOpt) error {
	p.bus.deliver(ctx, p.topic, [][]byte{body})
	return nil
}

// BatchSend 批量投递。
func (p *memProducer) BatchSend(ctx context.Context, bodyArr [][]byte, _ ...eventbus.SendOpt) error {
	p.bus.deliver(ctx, p.topic, bodyArr)
	return nil
}

// 编译期接口断言：内存实现必须完整实现 coze 的 Producer 与 ConsumerService。
var (
	_ eventbus.Producer        = (*memProducer)(nil)
	_ eventbus.ConsumerService = (*MemEventBus)(nil)
)
