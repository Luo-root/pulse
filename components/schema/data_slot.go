package schema

import (
	"context"
	"sync"
)

// DataSlot 数据槽
// 作用：存储一个值，支持多协程等待、多订阅、重复读取、广播唤醒
// 一个值生成后，所有节点都能获取
type DataSlot struct {
	value any
	cond  *sync.Cond
	ready bool
}

func NewDataSlot() *DataSlot {
	return &DataSlot{
		cond: sync.NewCond(&sync.Mutex{}),
	}
}

// Set 写入值（唤醒所有等待者）
func (s *DataSlot) Set(value any) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	if s.ready {
		return
	}

	s.value = value
	s.ready = true
	s.cond.Broadcast()
}

// Get 等待值就绪
func (s *DataSlot) Get(ctx context.Context) (any, error) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	// 已经有值，直接返回
	if s.ready {
		return s.value, nil
	}

	stop := context.AfterFunc(ctx, func() {
		// ctx取消时，唤醒所有等待的cond
		s.cond.Broadcast()
	})
	// 函数返回时停止监听，彻底销毁goroutine，杜绝泄漏
	defer stop()

	// 循环等待：每次唤醒先检查ctx，再检查数据
	for !s.ready {
		// 检查ctx是否已经取消/超时
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// 阻塞等待唤醒
		s.cond.Wait()
	}

	return s.value, nil
}
