package flow

import (
	"context"
	"fmt"
	"reflect"
	"sync"
)

type FlowContext struct {
	slots  *SafeMap[string, *DataSlot]
	ctx    context.Context
	cancel context.CancelFunc
	err    error
	mu     sync.Mutex
}

func NewFlowContext(ctx context.Context) *FlowContext {
	c, cancel := context.WithCancel(ctx)
	return &FlowContext{
		slots:  new(SafeMap[string, *DataSlot]),
		ctx:    c,
		cancel: cancel,
	}
}

func (c *FlowContext) GetContext() context.Context {
	return c.ctx
}

// slot 原子获取或创建，无竞态
func (c *FlowContext) slot(key string) *DataSlot {
	return c.slots.GetOrSet(key, func() *DataSlot {
		return NewDataSlot()
	})
}

// Set 放入数据（首次设置，已存在则忽略）
func (c *FlowContext) Set(key string, value any) {
	c.slot(key).SetOnce(value)
}

// SetOnce 同 Set
func (c *FlowContext) SetOnce(key string, value any) {
	c.slot(key).SetOnce(value)
}

// SetOrUpdate 放入或更新数据（始终覆盖）
func (c *FlowContext) SetOrUpdate(key string, value any) {
	c.slot(key).SetOrUpdate(value)
}

// Get 等待数据就绪并返回
func (c *FlowContext) Get(key string) (any, error) {
	return c.Wait(key)
}

// Wait 等待数据就绪
func (c *FlowContext) Wait(key string) (any, error) {
	return c.slot(key).Get(c.ctx)
}

// WaitWithContext 使用指定 context 等待数据就绪
// 用于切面级超时控制：传入切面的 context 而非工作流的 context
func (c *FlowContext) WaitWithContext(ctx context.Context, key string) (any, error) {
	return c.slot(key).Get(ctx)
}

// WaitAll 等待多个数据全部就绪
func (c *FlowContext) WaitAll(keys ...string) (map[string]any, error) {
	result := make(map[string]any, len(keys))
	for _, k := range keys {
		val, err := c.Wait(k)
		if err != nil {
			return nil, err
		}
		result[k] = val
	}
	return result, nil
}

// WaitAllWithContext 使用指定 context 等待多个数据全部就绪
func (c *FlowContext) WaitAllWithContext(ctx context.Context, keys ...string) (map[string]any, error) {
	result := make(map[string]any, len(keys))
	for _, k := range keys {
		val, err := c.WaitWithContext(ctx, k)
		if err != nil {
			return nil, err
		}
		result[k] = val
	}
	return result, nil
}

// TryGet 非阻塞获取
func (c *FlowContext) TryGet(key string) (any, bool) {
	slot, ok := c.slots.Get(key)
	if !ok {
		return nil, false
	}
	return slot.TryGet()
}

// IsReady 检查是否已就绪
func (c *FlowContext) IsReady(key string) bool {
	slot, ok := c.slots.Get(key)
	if !ok {
		return false
	}
	return slot.IsReady()
}

// WaitForAny 等待多个 key 中任意一个就绪
// 基于 done channel + reflect.Select，零轮询
func (c *FlowContext) WaitForAny(keys ...string) (string, any, error) {
	if len(keys) == 0 {
		return "", nil, fmt.Errorf("no keys provided")
	}

	// 快速路径：已有就绪的
	for _, k := range keys {
		if val, ok := c.TryGet(k); ok {
			return k, val, nil
		}
	}

	// 慢路径：channel 多路复用
	cases := make([]reflect.SelectCase, 0, len(keys)+1)
	cases = append(cases, reflect.SelectCase{
		Dir:  reflect.SelectRecv,
		Chan: reflect.ValueOf(c.ctx.Done()),
	})

	keyOrder := make([]string, len(keys))
	for i, k := range keys {
		keyOrder[i] = k
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(c.slot(k).Done()),
		})
	}

	chosen, _, _ := reflect.Select(cases)

	if chosen == 0 {
		return "", nil, c.ctx.Err()
	}

	key := keyOrder[chosen-1]
	val, _ := c.TryGet(key)
	return key, val, nil
}

// Cancel 取消工作流并记录首个错误
func (c *FlowContext) Cancel(err error) {
	c.mu.Lock()
	if c.err == nil && err != nil {
		c.err = err
	}
	c.mu.Unlock()
	c.cancel()
}

// Err 返回工作流级错误
func (c *FlowContext) Err() error {
	c.mu.Lock()
	e := c.err
	c.mu.Unlock()
	if e != nil {
		return e
	}
	return c.ctx.Err()
}

// Done 返回取消信号 channel
func (c *FlowContext) Done() <-chan struct{} {
	return c.ctx.Done()
}

// SetError 设置错误并触发取消
func (c *FlowContext) SetError(err error) error {
	if err == nil {
		return nil
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.cancel()
	return err
}

// GetError 获取已记录的首个错误（不触发取消）
func (c *FlowContext) GetError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
