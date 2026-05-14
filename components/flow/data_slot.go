package flow

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
	// 首次 SetOnce 时 close
	done chan struct{}
}

func NewDataSlot() *DataSlot {
	return &DataSlot{
		cond: sync.NewCond(&sync.Mutex{}),
		done: make(chan struct{}),
	}
}

// Done 返回通知 channel，值被设置时会被 close
func (s *DataSlot) Done() <-chan struct{} {
	return s.done
}

// SetOnce 首次写入值（幂等：如果已设置则忽略，不报错）
// 适用于：节点输出写入上下文，确保只写入一次
func (s *DataSlot) SetOnce(value any) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	if s.ready {
		return
	}

	s.value = value
	s.ready = true
	s.cond.Broadcast()
	close(s.done)
}

// SetOrUpdate 写入或更新值（始终覆盖，唤醒等待者）
// 适用于：ReAct 重规划时更新计划、状态变更等场景
// 注意：更新时会再次 Broadcast，确保新等待者能获取最新值
func (s *DataSlot) SetOrUpdate(value any) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	s.value = value

	if !s.ready {
		s.ready = true
		close(s.done)
	}

	s.cond.Broadcast()
}

// Set 写入值（唤醒所有等待者）
// 行为同 SetOnce，保持向后兼容
func (s *DataSlot) Set(value any) {
	s.SetOnce(value)
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

// TryGet 非阻塞获取值
// 返回值和 true 表示获取成功
// 返回 nil 和 false 表示值尚未就绪
func (s *DataSlot) TryGet() (any, bool) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	if !s.ready {
		return nil, false
	}
	return s.value, true
}

// IsReady 检查值是否已就绪
func (s *DataSlot) IsReady() bool {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	return s.ready
}
