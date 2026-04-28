package schema

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// FlowContext 工作流上下文（高级名称）
// 支持：
// 1. 数据多订阅共享
// 2. 自动等待依赖
// 3. 并发安全
// 4. 数据驱动执行
// 5. 级联取消与首错传播
type FlowContext struct {
	slots  *SafeMap[string, *DataSlot]
	ctx    context.Context
	cancel context.CancelFunc
	err    error
	mu     sync.Mutex // 保护 err
}

func (c *FlowContext) GetContext() *context.Context {
	return &c.ctx
}

type ReadOnlyFlowContext interface {
	Get(key string) (any, error)
}

func NewFlowContext(ctx context.Context) *FlowContext {
	c, cancel := context.WithCancel(ctx)
	return &FlowContext{
		slots:  new(SafeMap[string, *DataSlot]),
		ctx:    c,
		cancel: cancel,
	}
}

func (c *FlowContext) Get(key string) (any, error) {
	return c.Wait(key)
}

// 获取或创建数据槽
func (c *FlowContext) slot(key string) *DataSlot {
	slot, ok := c.slots.Get(key)
	if !ok {
		slot = NewDataSlot()
		c.slots.Set(key, slot)
	}
	return slot
}

// Set 往上下文放入数据（首次设置，已存在则忽略）
// 行为同 SetOnce，保持向后兼容
func (c *FlowContext) Set(key string, value any) {
	c.slot(key).SetOnce(value)
}

// SetOnce 往上下文放入数据（首次设置，已存在则忽略）
// 适用于：普通节点输出，确保每个 key 只被设置一次
func (c *FlowContext) SetOnce(key string, value any) {
	c.slot(key).SetOnce(value)
}

// SetOrUpdate 往上下文放入或更新数据（始终覆盖）
// 适用于：ReAct 重规划更新计划、状态变更等需要覆盖的场景
func (c *FlowContext) SetOrUpdate(key string, value any) {
	c.slot(key).SetOrUpdate(value)
}

// Wait 等待数据（多节点可同时等待同一个key）
func (c *FlowContext) Wait(key string) (any, error) {
	return c.slot(key).Get(c.ctx)
}

// WaitAll 等待多个数据
func (c *FlowContext) WaitAll(keys ...string) (map[string]any, error) {
	result := make(map[string]any, len(keys))
	for _, k := range keys {
		val, err := c.Wait(k)
		if err != nil {
			return nil, err // 任意一个等待取消，直接返回错误
		}
		result[k] = val
	}
	return result, nil
}

// TryGet 非阻塞获取数据
// 返回值和 true 表示获取成功
// 返回 nil 和 false 表示数据尚未就绪
func (c *FlowContext) TryGet(key string) (any, bool) {
	slot, ok := c.slots.Get(key)
	if !ok {
		return nil, false
	}
	return slot.TryGet()
}

// IsReady 检查 key 是否已就绪
func (c *FlowContext) IsReady(key string) bool {
	slot, ok := c.slots.Get(key)
	if !ok {
		return false
	}
	return slot.IsReady()
}

// Cancel 取消工作流上下文，并记录首个错误（仅首次调用时记录）
// 当任意节点失败时，应调用此方法触发级联取消，通知所有等待中的节点立即退出
func (c *FlowContext) Cancel(err error) {
	c.mu.Lock()
	if c.err == nil && err != nil {
		c.err = err
	}
	c.mu.Unlock()
	c.cancel()
}

// Err 返回工作流级错误（如果有的话）
// 若已记录具体错误则返回该错误；否则返回底层 context 的错误（如 context.Canceled）
func (c *FlowContext) Err() error {
	c.mu.Lock()
	e := c.err
	c.mu.Unlock()
	if e != nil {
		return e
	}
	return c.ctx.Err()
}

// Done 返回上下文的 Done channel，可用于 select 监听取消信号
func (c *FlowContext) Done() <-chan struct{} {
	return c.ctx.Done()
}

// SetError 设置工作流级错误（仅首次调用时记录）并触发取消
// 这是 Cancel 的便捷包装，同时设置错误和取消上下文
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

// WaitForAny 等待多个 key 中任意一个就绪
// 返回第一个就绪的 key 及其值
// 注意：如果有多个 key 同时就绪，返回哪个是不确定的
func (c *FlowContext) WaitForAny(keys ...string) (string, any, error) {
	if len(keys) == 0 {
		return "", nil, fmt.Errorf("no keys provided")
	}

	// 先检查是否已有就绪的
	for _, k := range keys {
		if val, ok := c.TryGet(k); ok {
			return k, val, nil
		}
	}

	// 使用轮询等待（带退避）
	for {
		select {
		case <-c.ctx.Done():
			return "", nil, c.ctx.Err()
		default:
		}

		for _, k := range keys {
			if val, ok := c.TryGet(k); ok {
				return k, val, nil
			}
		}

		// 短暂休眠避免忙等待
		select {
		case <-c.ctx.Done():
			return "", nil, c.ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
