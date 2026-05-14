package flow

import "sync"

// SafeMap 泛型并发安全 Map
// K: 键类型（必须可比较）
// V: 值类型（任意）
type SafeMap[K comparable, V any] struct {
	m sync.Map
}

// Set 存值
func (s *SafeMap[K, V]) Set(key K, value V) {
	s.m.Store(key, value)
}

// Get 取值
func (s *SafeMap[K, V]) Get(key K) (V, bool) {
	val, ok := s.m.Load(key)
	if !ok {
		var zero V
		return zero, false
	}
	// 类型断言（泛型安全）
	return val.(V), true
}

// GetOrSet 原子的「获取或创建」
// 已存在则返回已有值，不存在则调用 createFn 创建后存入
func (s *SafeMap[K, V]) GetOrSet(key K, createFn func() V) V {
	if val, ok := s.m.Load(key); ok {
		return val.(V)
	}
	newVal := createFn()
	actual, _ := s.m.LoadOrStore(key, newVal)
	return actual.(V)
}

// MustGet 没有就panic
func (s *SafeMap[K, V]) MustGet(key K) V {
	val, ok := s.m.Load(key)
	if !ok {
		panic("SafeMap: key not found: " + any(key).(string))
	}
	return val.(V)
}

// Delete 删除值
func (s *SafeMap[K, V]) Delete(key K) {
	s.m.Delete(key)
}

// Exist 判断 key 是否存在
func (s *SafeMap[K, V]) Exist(key K) bool {
	_, ok := s.m.Load(key)
	return ok
}

// Range 遍历所有键值对
func (s *SafeMap[K, V]) Range(fn func(key K, value V) bool) {
	s.m.Range(func(k, v any) bool {
		return fn(k.(K), v.(V))
	})
}

// Clear 清空所有数据
func (s *SafeMap[K, V]) Clear() {
	s.m.Range(func(k, _ any) bool {
		s.m.Delete(k)
		return true
	})
}
