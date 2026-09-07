package observability

import (
	"strings"
	"sync"
	"testing"
)

func TestNewTraceIDNonEmpty(t *testing.T) {
	id := NewTraceID()
	if id == "" {
		t.Fatal("NewTraceID 返回空串")
	}
	if strings.ContainsAny(id, " \t\n") {
		t.Fatalf("TraceID 不应包含空白字符：%q", id)
	}
}

// 并发唯一性：TraceID 的唯一契约是进程内不重复（跨进程靠随机段），
// 用多 goroutine 高频调用做唯一性断言。
func TestNewTraceIDConcurrentUnique(t *testing.T) {
	const workers = 8
	const perWorker = 250

	var mu sync.Mutex
	seen := make(map[string]struct{}, workers*perWorker)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, perWorker)
			for j := 0; j < perWorker; j++ {
				local = append(local, NewTraceID())
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range local {
				if _, dup := seen[id]; dup {
					t.Errorf("重复 TraceID：%s", id)
				}
				seen[id] = struct{}{}
			}
		}()
	}
	wg.Wait()

	if len(seen) != workers*perWorker {
		t.Fatalf("唯一 TraceID 数 %d != 期望 %d", len(seen), workers*perWorker)
	}
}
