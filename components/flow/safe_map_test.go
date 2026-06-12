package flow

import (
	"sync"
	"testing"
)

func TestSafeMap_SetGet(t *testing.T) {
	m := new(SafeMap[string, int])
	m.Set("a", 1)

	val, ok := m.Get("a")
	if !ok || val != 1 {
		t.Fatalf("expected (1, true), got (%v, %v)", val, ok)
	}
}

func TestSafeMap_Get_NotExist(t *testing.T) {
	m := new(SafeMap[string, int])

	val, ok := m.Get("missing")
	if ok {
		t.Fatal("expected false")
	}
	if val != 0 {
		t.Fatalf("expected zero value, got %v", val)
	}
}

func TestSafeMap_GetOrSet_Concurrent(t *testing.T) {
	m := new(SafeMap[string, *int])
	const goroutines = 100

	var wg sync.WaitGroup
	created := make(chan *int, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val := m.GetOrSet("key", func() *int {
				v := 42
				return &v
			})
			created <- val
		}()
	}

	wg.Wait()
	close(created)

	// 所有 goroutine 应该拿到同一个指针
	var first *int
	for ptr := range created {
		if first == nil {
			first = ptr
		}
		if ptr != first {
			t.Fatal("GetOrSet returned different pointers — not atomic")
		}
	}
	if *first != 42 {
		t.Fatalf("expected 42, got %d", *first)
	}
}

func TestSafeMap_GetOrSet_AlreadyExists(t *testing.T) {
	m := new(SafeMap[string, string])
	m.Set("existing", "original")

	val := m.GetOrSet("existing", func() string {
		return "should not be created"
	})
	if val != "original" {
		t.Fatalf("expected 'original', got %v", val)
	}
}

func TestSafeMap_Delete(t *testing.T) {
	m := new(SafeMap[string, int])
	m.Set("a", 1)
	m.Delete("a")

	_, ok := m.Get("a")
	if ok {
		t.Fatal("expected key to be deleted")
	}
}

func TestSafeMap_Exist(t *testing.T) {
	m := new(SafeMap[string, int])
	m.Set("a", 1)

	if !m.Exist("a") {
		t.Fatal("expected key to exist")
	}
	if m.Exist("b") {
		t.Fatal("expected key to not exist")
	}
}

func TestSafeMap_Range(t *testing.T) {
	m := new(SafeMap[string, int])
	m.Set("a", 1)
	m.Set("b", 2)
	m.Set("c", 3)

	collected := make(map[string]int)
	m.Range(func(key string, value int) bool {
		collected[key] = value
		return true
	})

	if len(collected) != 3 {
		t.Fatalf("expected 3 items, got %d", len(collected))
	}
}

func TestSafeMap_Clear(t *testing.T) {
	m := new(SafeMap[string, int])
	m.Set("a", 1)
	m.Set("b", 2)
	m.Clear()

	if m.Exist("a") || m.Exist("b") {
		t.Fatal("expected map to be empty after Clear")
	}
}
