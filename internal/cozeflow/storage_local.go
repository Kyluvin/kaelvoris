// storage_local.go：本地文件系统版 storage.Storage（替代 MinIO/TOS/S3）。
//
// 对象键 → 本地文件映射：root/<objectKey>（自动建父目录）。
// GetObjectUrl 返回 file:// URL（融合形态下前端经 Kaelvoris 静态路由读取，
// 不依赖公网签名 URL 语义；expire 参数仅保留接口兼容）。
package cozeflow

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coze-dev/coze-studio/backend/infra/storage"
)

// LocalStorage 嵌入式对象存储：单进程读写锁保护 + 目录树持久化。
type LocalStorage struct {
	root string
	mu   sync.RWMutex
}

// NewLocalStorage 创建本地文件存储（root 不存在时自动创建）。
func NewLocalStorage(root string) (*LocalStorage, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("创建存储根目录失败: %w", err)
	}
	return &LocalStorage{root: root}, nil
}

// pathOf 对象键 → 本地路径（防目录穿越：拒绝 .. 与盘符）。
func (s *LocalStorage) pathOf(objectKey string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean("/" + objectKey))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || strings.Contains(clean, "../") {
		return "", fmt.Errorf("非法对象键: %s", objectKey)
	}
	return filepath.Join(s.root, filepath.FromSlash(clean)), nil
}

// PutObject 写入对象（自动建父目录）。
func (s *LocalStorage) PutObject(_ context.Context, objectKey string, content []byte, _ ...storage.PutOptFn) error {
	p, err := s.pathOf(objectKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("创建对象父目录失败: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.WriteFile(p, content, 0o644)
}

// PutObjectWithReader 流式写入对象。
func (s *LocalStorage) PutObjectWithReader(_ context.Context, objectKey string, content io.Reader, _ ...storage.PutOptFn) error {
	p, err := s.pathOf(objectKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("创建对象父目录失败: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, content)
	return err
}

// GetObject 读取对象全文。
func (s *LocalStorage) GetObject(_ context.Context, objectKey string) ([]byte, error) {
	p, err := s.pathOf(objectKey)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return os.ReadFile(p)
}

// DeleteObject 删除对象（不存在不报错，对齐 S3 语义）。
func (s *LocalStorage) DeleteObject(_ context.Context, objectKey string) error {
	p, err := s.pathOf(objectKey)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// GetObjectUrl 返回 file:// 本地 URL（expire 仅为接口兼容保留）。
func (s *LocalStorage) GetObjectUrl(_ context.Context, objectKey string, _ ...storage.GetOptFn) (string, error) {
	p, err := s.pathOf(objectKey)
	if err != nil {
		return "", err
	}
	return "file:///" + filepath.ToSlash(p), nil
}

// HeadObject 返回对象元数据；不存在返回错误。
func (s *LocalStorage) HeadObject(_ context.Context, objectKey string, _ ...storage.GetOptFn) (*storage.FileInfo, error) {
	p, err := s.pathOf(objectKey)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	return toFileInfo(objectKey, p, st), nil
}

// ListAllObjects 前缀遍历全部对象（按 key 排序，保证稳定顺序）。
func (s *LocalStorage) ListAllObjects(_ context.Context, prefix string, _ ...storage.GetOptFn) ([]*storage.FileInfo, error) {
	prefixPath, err := s.pathOf(prefix)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []*storage.FileInfo
	_ = filepath.Walk(s.root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil // 跳过错误与目录
		}
		key := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(path, s.root), string(os.PathSeparator)))
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		_ = prefixPath // 前缀已按 key 判定
		out = append(out, toFileInfo(key, path, info))
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// ListObjectsPaginated 分页遍历（游标 = 上一页最后一条 key）。
func (s *LocalStorage) ListObjectsPaginated(ctx context.Context, input *storage.ListObjectsPaginatedInput, opts ...storage.GetOptFn) (*storage.ListObjectsPaginatedOutput, error) {
	all, err := s.ListAllObjects(ctx, input.Prefix, opts...)
	if err != nil {
		return nil, err
	}
	pageSize := int64(input.PageSize)
	if pageSize <= 0 {
		pageSize = 100
	}
	// 定位游标之后的位置
	start := int64(0)
	if input.Cursor != "" {
		for i, f := range all {
			if f.Key > input.Cursor {
				start = int64(i)
				break
			}
			start = int64(i + 1)
		}
	}
	end := start + pageSize
	truncated := false
	if end < int64(len(all)) {
		truncated = true
	} else {
		end = int64(len(all))
	}
	page := all[start:end]
	var cursor string
	if len(page) > 0 {
		cursor = page[len(page)-1].Key
	}
	return &storage.ListObjectsPaginatedOutput{Files: page, Cursor: cursor, IsTruncated: truncated}, nil
}

// toFileInfo 组装元数据（ETag 用内容 md5 近似，融合场景仅用于展示/去重参考）。
func toFileInfo(key, path string, st os.FileInfo) *storage.FileInfo {
	etag := ""
	if st.Size() > 0 && st.Size() < 32*1024*1024 {
		if b, err := os.ReadFile(path); err == nil {
			sum := md5.Sum(b)
			etag = hex.EncodeToString(sum[:])
		}
	}
	return &storage.FileInfo{
		Key:          key,
		LastModified: st.ModTime().UTC(),
		ETag:         etag,
		Size:         st.Size(),
		URL:          "file:///" + filepath.ToSlash(path),
	}
}

// 编译期接口断言：LocalStorage 必须完整实现 coze storage.Storage。
var _ storage.Storage = (*LocalStorage)(nil)

// 保留 time 引用（FileInfo.LastModified 类型所需，防误删 import）。
var _ = time.Now
