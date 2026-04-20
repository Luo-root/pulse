package schema

import (
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

// Get 等待值就绪（可多次调用、多协程安全）
func (s *DataSlot) Get() any {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	// 自动等待
	for !s.ready {
		s.cond.Wait()
	}

	return s.value
}
