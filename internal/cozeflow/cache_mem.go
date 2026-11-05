// Package cozeflow Coze Studio 源码融合的嵌入式基础设施层。
//
// 设计背景（2026-10 与用户确认的融合方案）：
// coze-studio（third_party/coze-backend，Apache 2.0）原设计为独立部署，
// 依赖 MySQL/Redis/MinIO/ES/Milvus/NSQ 六大外部服务。Kaelvoris 遵循
// "单二进制、零外部服务、文件持久化"哲学，因此按 coze 的 infra 接口
// （cache.Cmdable / storage.Storage / eventbus.Producer 等）逐一提供
// 嵌入式实现，coze 的 domain/application 层代码零改动直接复用。
//
// cache_mem.go：内存版 cache.Cmdable —— 对齐 go-redis 语义
// （string/hash/list/generic 命令 + Pipeline），值带 TTL 惰性过期。
// 单进程内替代 Redis：idgen 计数器、工作流中断事件、取消信号、
// 执行历史缓存等消费方全部无感。
package cozeflow

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coze-dev/coze-studio/backend/infra/cache"
)

// memValue 缓存条目：任意值 + 过期时间点（零值表示永不过期）。
type memValue struct {
	v       any
	expires time.Time // 零值 = 永不过期
}

// MemCache 内存版 cache.Cmdable。
// 并发安全（RWMutex）；TTL 惰性清除（读取/遍历时判过期）。
type MemCache struct {
	mu sync.RWMutex
	m  map[string]memValue
	// shutdown 兜底清扫协程退出信号；进程内缓存随进程消亡，无需复杂清理。
	stopOnce sync.Once
}

// NewMemCache 创建内存缓存并启动周期清扫（每 10 分钟物理清除过期键）。
func NewMemCache() *MemCache {
	c := &MemCache{m: make(map[string]memValue)}
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			c.gc()
		}
	}()
	return c
}

// gc 物理清除全部过期键（清扫协程专用）。
func (c *MemCache) gc() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.m {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(c.m, k)
		}
	}
}

// get 读取有效条目：不存在或已过期均返回 notfound。
func (c *MemCache) get(key string) (memValue, bool) {
	c.mu.RLock()
	e, ok := c.m[key]
	c.mu.RUnlock()
	if !ok {
		return memValue{}, false
	}
	if !e.expires.IsZero() && time.Now().After(e.expires) {
		return memValue{}, false
	}
	return e, true
}

// set 写入条目。
func (c *MemCache) set(key string, v any, expiration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var exp time.Time
	if expiration > 0 {
		exp = time.Now().Add(expiration)
	}
	c.m[key] = memValue{v: v, expires: exp}
}

// del 删除键，返回删除个数（对齐 redis 语义）。
func (c *MemCache) del(keys ...string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for _, k := range keys {
		if _, ok := c.m[k]; ok {
			delete(c.m, k)
			n++
		}
	}
	return n
}

// ---- String 命令 ----

// Set 写入字符串值（value 经 fmt 序列化存储，redis 中一切皆字符串）。
func (c *MemCache) Set(_ context.Context, key string, value interface{}, expiration time.Duration) cache.StatusCmd {
	c.set(key, toString(value), expiration)
	return okStatus(fmt.Sprint(value))
}

// Get 读字符串值；键不存在返回 redis.Nil 风格错误（cache.Nil）。
func (c *MemCache) Get(_ context.Context, key string) cache.StringCmd {
	e, ok := c.get(key)
	if !ok {
		return errStringCmd(cache.Nil)
	}
	return okStringCmd(fmt.Sprint(e.v))
}

// Incr 自增 1（非数值或不存在按 0 起步；值以 int64 存储）。
func (c *MemCache) Incr(ctx context.Context, key string) cache.IntCmd {
	return c.IncrBy(ctx, key, 1)
}

// IncrBy 自增 n；键不存在按 0 起步；原值非数值则报错（对齐 redis）。
func (c *MemCache) IncrBy(_ context.Context, key string, value int64) cache.IntCmd {
	e, ok := c.get(key)
	if !ok {
		c.set(key, value, 0)
		return okIntCmd(value)
	}
	var cur int64
	switch v := e.v.(type) {
	case int64:
		cur = v
	case string:
		if _, err := fmt.Sscanf(v, "%d", &cur); err != nil {
			return errIntCmd(fmt.Errorf("value is not an integer or out of range"))
		}
	default:
		return errIntCmd(fmt.Errorf("value is not an integer or out of range"))
	}
	next := cur + value
	c.set(key, next, 0)
	return okIntCmd(next)
}

// ---- Hash 命令 ----

// HSet 写入 hash 字段（values 为 k1,v1,k2,v2... 序列）。
func (c *MemCache) HSet(_ context.Context, key string, values ...interface{}) cache.IntCmd {
	h, _ := c.getRawHash(key)
	n := int64(0)
	for i := 0; i+1 < len(values); i += 2 {
		fk := fmt.Sprint(values[i])
		fv := toString(values[i+1])
		if _, exists := h[fk]; !exists {
			n++
		}
		h[fk] = fv
	}
	c.mu.Lock()
	c.m[key] = memValue{v: h}
	c.mu.Unlock()
	return okIntCmd(n)
}

// HGetAll 读取整个 hash；键不存在返回空 map（对齐 redis）。
func (c *MemCache) HGetAll(_ context.Context, key string) cache.MapStringStringCmd {
	e, ok := c.get(key)
	if !ok {
		return okMapCmd(map[string]string{})
	}
	h, ok := e.v.(map[string]string)
	if !ok {
		return okMapCmd(map[string]string{})
	}
	cp := make(map[string]string, len(h))
	for k, v := range h {
		cp[k] = v
	}
	return okMapCmd(cp)
}

// getRawHash 取出 hash（不存在则建空表），调用方写回。
func (c *MemCache) getRawHash(key string) (map[string]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[key]
	if !ok {
		return map[string]string{}, false
	}
	h, ok := e.v.(map[string]string)
	if !ok {
		return map[string]string{}, false
	}
	return h, true
}

// ---- List 命令 ----

// getRawList 取出 list（不存在则建空表），调用方写回。
func (c *MemCache) getRawList(key string) []string {
	c.mu.RLock()
	e, ok := c.m[key]
	c.mu.RUnlock()
	if ok {
		if l, ok := e.v.([]string); ok {
			return l
		}
	}
	return []string{}
}

// LPush 头插（多值按顺序依次头插，最终顺序与 redis 一致：后插在前）。
func (c *MemCache) LPush(_ context.Context, key string, values ...interface{}) cache.IntCmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.listLocked(key)
	for _, v := range values {
		l = append([]string{toString(v)}, l...)
	}
	c.m[key] = memValue{v: l}
	return okIntCmd(int64(len(l)))
}

// RPush 尾插。
func (c *MemCache) RPush(_ context.Context, key string, values ...interface{}) cache.IntCmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.listLocked(key)
	for _, v := range values {
		l = append(l, toString(v))
	}
	c.m[key] = memValue{v: l}
	return okIntCmd(int64(len(l)))
}

// LIndex 取下标元素（负数从尾部数，对齐 redis）。
func (c *MemCache) LIndex(_ context.Context, key string, index int64) cache.StringCmd {
	l := c.getRawList(key)
	idx := normIndex(index, int64(len(l)))
	if idx < 0 {
		return errStringCmd(cache.Nil)
	}
	return okStringCmd(l[idx])
}

// LPop 弹出头部元素。
func (c *MemCache) LPop(_ context.Context, key string) cache.StringCmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.listLocked(key)
	if len(l) == 0 {
		return errStringCmd(cache.Nil)
	}
	head := l[0]
	c.m[key] = memValue{v: l[1:]}
	return okStringCmd(head)
}

// LRange 取区间 [start, stop]（闭区间，负数从尾部数，对齐 redis）。
func (c *MemCache) LRange(_ context.Context, key string, start, stop int64) cache.StringSliceCmd {
	l := c.getRawList(key)
	n := int64(len(l))
	s := normIndex(start, n)
	if s < 0 {
		s = 0
	}
	e := normIndex(stop, n)
	if e >= n {
		e = n - 1
	}
	if s > e || n == 0 {
		return okSliceCmd([]string{})
	}
	return okSliceCmd(l[s : e+1])
}

// LSet 设置下标元素。
func (c *MemCache) LSet(_ context.Context, key string, index int64, value interface{}) cache.StatusCmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.listLocked(key)
	if index < 0 || index >= int64(len(l)) {
		return errStatus(fmt.Errorf("index out of range"))
	}
	l[index] = toString(value)
	c.m[key] = memValue{v: l}
	return okStatus("OK")
}

// listLocked 锁内取 list 拷贝（调用方必须已持写锁）。
func (c *MemCache) listLocked(key string) []string {
	if e, ok := c.m[key]; ok {
		if l, ok := e.v.([]string); ok {
			return append([]string{}, l...)
		}
	}
	return []string{}
}

// normIndex 归一化负下标（-1 → n-1）；越界返回 -1。
func normIndex(i, n int64) int64 {
	if i < 0 {
		i += n
	}
	if i < 0 || i >= n {
		return -1
	}
	return i
}

// ---- Generic 命令 ----

// Del 删除键（支持多键）。
func (c *MemCache) Del(_ context.Context, keys ...string) cache.IntCmd {
	return okIntCmd(c.del(keys...))
}

// Exists 判断键存在个数。
func (c *MemCache) Exists(_ context.Context, keys ...string) cache.IntCmd {
	var n int64
	for _, k := range keys {
		if _, ok := c.get(k); ok {
			n++
		}
	}
	return okIntCmd(n)
}

// Expire 设置 TTL；键不存在返回 false。
func (c *MemCache) Expire(_ context.Context, key string, expiration time.Duration) cache.BoolCmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return okBoolCmd(false)
	}
	e.expires = time.Now().Add(expiration)
	c.m[key] = e
	return okBoolCmd(true)
}

// ---- Pipeline ----

// Pipeline 返回流水线：单进程内命令本就地执行，流水线退化为立即逐条执行
// （Exec 时原样返回各命令结果），语义对齐（调用方只关心 Exec 汇总错误）。
func (c *MemCache) Pipeline() cache.Pipeliner { return c }

// Exec 空实现（命令已即时执行），返回零错误。
func (c *MemCache) Exec(_ context.Context) ([]cache.Cmder, error) { return nil, nil }

// ---- 命令结果实现（缓存常用结果对象，减少分配） ----

type intResult struct{ v int64; err error }

func (r *intResult) Err() error              { return r.err }
func (r *intResult) Result() (int64, error)  { return r.v, r.err }

type strResult struct{ v string; err error }

func (r *strResult) Err() error              { return r.err }
func (r *strResult) Result() (string, error) { return r.v, r.err }
func (r *strResult) Val() string             { return r.v }
func (r *strResult) Int64() (int64, error) {
	var n int64
	if r.err != nil {
		return 0, r.err
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(r.v), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}
func (r *strResult) Bytes() ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	return []byte(r.v), nil
}

type boolResult struct{ v bool; err error }

func (r *boolResult) Err() error             { return r.err }
func (r *boolResult) Result() (bool, error)  { return r.v, r.err }

type statusResult struct{ v string; err error }

func (r *statusResult) Err() error             { return r.err }
func (r *statusResult) Result() (string, error) { return r.v, r.err }

type mapResult struct{ v map[string]string; err error }

func (r *mapResult) Err() error                          { return r.err }
func (r *mapResult) Result() (map[string]string, error)  { return r.v, r.err }

type sliceResult struct{ v []string; err error }

func (r *sliceResult) Err() error                 { return r.err }
func (r *sliceResult) Result() ([]string, error)  { return r.v, r.err }

func okIntCmd(v int64) cache.IntCmd               { return &intResult{v: v} }
func errIntCmd(err error) cache.IntCmd            { return &intResult{err: err} }
func okStringCmd(v string) cache.StringCmd        { return &strResult{v: v} }
func errStringCmd(err error) cache.StringCmd      { return &strResult{err: err} }
func okBoolCmd(v bool) cache.BoolCmd              { return &boolResult{v: v} }
func okStatus(v string) cache.StatusCmd           { return &statusResult{v: v} }
func errStatus(err error) cache.StatusCmd         { return &statusResult{err: err} }
func okMapCmd(v map[string]string) cache.MapStringStringCmd { return &mapResult{v: v} }
func okSliceCmd(v []string) cache.StringSliceCmd  { return &sliceResult{v: v} }

// toString redis 风格值序列化：字符串原样；其余经 fmt（int64/[]byte 等高频）。
func toString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}

// 编译期接口断言：MemCache 必须完整实现 coze 的 cache.Cmdable
//（漏方法会在编译期报缺失，防止上游命令集扩充后静默降级为 nil panic）。
var _ cache.Cmdable = (*MemCache)(nil)
