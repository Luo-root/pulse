package schema

import "context"

// FlowContext 工作流上下文（高级名称）
// 支持：
// 1. 数据多订阅共享
// 2. 自动等待依赖
// 3. 并发安全
// 4. 数据驱动执行
type FlowContext struct {
	slots *SafeMap[string, *DataSlot]
}

type ReadOnlyFlowContext interface {
	Get(key string) (any, bool)
}

func NewFlowContext() *FlowContext {
	return &FlowContext{
		slots: new(SafeMap[string, *DataSlot]),
	}
}

func (c *FlowContext) Get(key string) (any, bool) {
	slot, ok := c.slots.Get(key)
	return slot, ok
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

// Set 往上下文放入数据
func (c *FlowContext) Set(key string, value any) {
	c.slot(key).Set(value)
}

// Wait 等待数据（多节点可同时等待同一个key）
func (c *FlowContext) Wait(ctx context.Context, key string) (any, error) {
	return c.slot(key).Get(ctx)
}

// WaitAll 等待多个数据
func (c *FlowContext) WaitAll(ctx context.Context, keys ...string) (map[string]any, error) {
	result := make(map[string]any, len(keys))
	for _, k := range keys {
		val, err := c.Wait(ctx, k)
		if err != nil {
			return nil, err // 任意一个等待取消，直接返回错误
		}
		result[k] = val
	}
	return result, nil
}
