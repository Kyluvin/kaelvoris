// noop_eventbus.go：search.ResourceEventBus 的 no-op 实现。
//
// coze 的 search 域把资源变更事件（workflow/app/知识库的创建/更新/删除）写进
// ES 以支撑首页搜索。Phase 1 最小闭环不装配 search 域（无 ES），此实现将事件
// 丢弃：PublishResources 直接返回 nil。Phase 2 装配 bleve 全文检索时替换为
// 真实投递（事件 JSON 载荷经 MemEventBus 转 search 域消费者）。
package cozeflow

import (
	"context"
	"log/slog"

	"github.com/coze-dev/coze-studio/backend/domain/search/entity"
	searchSvc "github.com/coze-dev/coze-studio/backend/domain/search/service"
)

// NoopResourceEventBus 丢弃型资源事件总线。
type NoopResourceEventBus struct{}

// NewNoopResourceEventBus 构造。
func NewNoopResourceEventBus() *NoopResourceEventBus { return &NoopResourceEventBus{} }

// PublishResources 记录日志后丢弃事件（no-op）。
func (n *NoopResourceEventBus) PublishResources(_ context.Context, event *entity.ResourceDomainEvent) error {
	if event != nil && slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		slog.Debug("cozeflow: 资源事件已丢弃（Phase 1 未装配 search 域）",
			"op", event.OpType, "res_type", event.Resource.ResType, "res_id", event.Resource.ResID)
	}
	return nil
}

// 编译期接口断言：NoopResourceEventBus 必须实现 search.ResourceEventBus。
var _ searchSvc.ResourceEventBus = (*NoopResourceEventBus)(nil)
