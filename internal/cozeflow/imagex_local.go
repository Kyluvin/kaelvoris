// imagex_local.go：本地对象存储版 imagex.ImageX（替代 veimagex 火山引擎镜像服务）。
//
// 对齐 coze infra/storage/impl/minio/minio_imagex.go 的语义：
// coze 官方在 MinIO 部署形态下也是拿 Storage 顶替 ImageX（GetResourceURL 转发
// GetObjectUrl、Upload 为空实现），融合形态照搬该模式，把底层换成 LocalStorage。
// 上传走 coze 的 /api/common/upload HTTP 面（UploadService.UploadFileCommon），
// 桌面形态下上传由前端直连 Wails 绑定写 LocalStorage，本实现仅兜底协议面。
package cozeflow

import (
	"context"
	"time"

	"github.com/coze-dev/coze-studio/backend/infra/imagex"
	"github.com/coze-dev/coze-studio/backend/infra/storage"
)

// LocalImageX 基于 LocalStorage 的 ImageX 实现。
type LocalImageX struct {
	storage storage.Storage
}

// NewLocalImageX 构造（storage 传入 bootstrap 装配好的 LocalStorage）。
func NewLocalImageX(st storage.Storage) *LocalImageX {
	return &LocalImageX{storage: st}
}

// GetUploadAuth 返回空凭证（本地上传无需鉴权，仅对齐协议字段）。
func (m *LocalImageX) GetUploadAuth(_ context.Context, _ ...imagex.UploadAuthOpt) (*imagex.SecurityToken, error) {
	now := time.Now().Format("2006-01-02 15:04:05")
	return &imagex.SecurityToken{
		ExpiredTime: time.Now().Add(time.Hour).Format("2006-01-02 15:04:05"),
		CurrentTime: now,
		HostScheme:  "http",
	}, nil
}

// GetUploadAuthWithExpire 同上（expire 参数仅保留接口兼容）。
func (m *LocalImageX) GetUploadAuthWithExpire(_ context.Context, _ time.Duration, _ ...imagex.UploadAuthOpt) (*imagex.SecurityToken, error) {
	return m.GetUploadAuth(context.Background())
}

// GetResourceURL 对象 URI → 本地 file:// URL。
func (m *LocalImageX) GetResourceURL(ctx context.Context, uri string, _ ...imagex.GetResourceOpt) (*imagex.ResourceURL, error) {
	url, err := m.storage.GetObjectUrl(ctx, uri)
	if err != nil {
		return nil, err
	}
	return &imagex.ResourceURL{URL: url}, nil
}

// Upload 直传（桌面形态走 Wails 绑定/HTTP 上传面，此处返回空实现对齐 minio_imagex.go）。
func (m *LocalImageX) Upload(_ context.Context, _ []byte, _ ...imagex.UploadAuthOpt) (*imagex.UploadResult, error) {
	return nil, nil
}

// GetServerID 火山引擎 ImageX 服务 ID，本地形态无此概念，返回空。
func (m *LocalImageX) GetServerID() string { return "" }

// GetUploadHost 上传主机（coze 原生从 ctx 取 Host 拼 /api/common/upload/apply_upload_action；
// 桌面形态上传不经该协议面，返回空字符串，消费方对空值已容错）。
func (m *LocalImageX) GetUploadHost(_ context.Context) string { return "" }

// 编译期接口断言：LocalImageX 必须完整实现 coze imagex.ImageX。
var _ imagex.ImageX = (*LocalImageX)(nil)
