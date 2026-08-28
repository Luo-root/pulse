package builtins

import (
	"sync"
	"time"
)

// readTracker 记录某路径最近一次成功 read 的时间（进程内、按 Options 实例）。
// edit/write 用它做 stale-read：未读过或磁盘 mtime 更新则拒绝。
type readTracker struct {
	mu    sync.Mutex
	times map[string]time.Time
}

func newReadTracker() *readTracker {
	return &readTracker{times: make(map[string]time.Time)}
}

func (t *readTracker) mark(path string, when time.Time) {
	t.mu.Lock()
	t.times[path] = when
	t.mu.Unlock()
}

func (t *readTracker) last(path string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tm, ok := t.times[path]
	return tm, ok
}
