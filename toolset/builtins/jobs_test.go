package builtins

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestJobRingBufferDropsHead(t *testing.T) {
	j := &job{waitCh: make(chan struct{})}
	j.append([]byte("abcdefgh"), 8)
	if string(j.buf) != "abcdefgh" || j.dropped != 0 {
		t.Fatalf("buf=%q dropped=%d", j.buf, j.dropped)
	}
	j.append([]byte("1234"), 8)
	if j.dropped != 4 {
		t.Fatalf("dropped=%d", j.dropped)
	}
	_, clipped, total := j.read(0, 100)
	if !clipped {
		t.Fatal("offset 0 should be inside the dropped window")
	}
	if total != 12 {
		t.Fatalf("total=%d", total)
	}
	data, clipped2, _ := j.read(4, 100)
	if clipped2 || string(data) != "efgh1234" {
		t.Fatalf("data=%q clipped=%v", data, clipped2)
	}
}

func TestJobRingBufferRuneBoundary(t *testing.T) {
	j := &job{waitCh: make(chan struct{})}
	// "中" 是 3 字节；把上限切在中间时切点应后移到 rune 起点。
	j.append([]byte("中中中中"), 9)
	if !utf8.Valid(j.buf) {
		t.Fatalf("buf not rune-aligned: %q", j.buf)
	}
	if j.dropped != 3 {
		t.Fatalf("dropped=%d", j.dropped)
	}
}

func TestJobTableMaxRunning(t *testing.T) {
	tb := newJobTable(1, 100)
	fake := &job{id: "j0", waitCh: make(chan struct{})}
	tb.jobs[fake.id] = fake
	_, err := tb.launch(nil, "echo hi", ".")
	if err == nil || !strings.Contains(err.Error(), "too many running background jobs") {
		t.Fatalf("err=%v", err)
	}
}

func TestJobTableReapsOldestDone(t *testing.T) {
	tb := newJobTable(1, 100)
	mk := func(id string, done bool) *job {
		j := &job{id: id, waitCh: make(chan struct{}), done: done}
		tb.jobs[id] = j
		tb.order = append(tb.order, id)
		return j
	}
	// 上限 max=1 → done 上限 2。已有 3 个 done：reap 应删最旧的 j1。
	mk("j1", true)
	mk("j2", true)
	mk("j3", true)
	tb.mu.Lock()
	tb.reapDoneLocked()
	tb.mu.Unlock()
	if _, ok := tb.jobs["j1"]; ok {
		t.Fatal("oldest done job j1 should be reaped")
	}
	if _, ok := tb.jobs["j2"]; !ok {
		t.Fatal("j2 should survive")
	}
	if _, ok := tb.jobs["j3"]; !ok {
		t.Fatal("j3 should survive")
	}
	// running job 不参与淘汰：再塞 1 个 done（j4）→ done=2 已达上限；j2 保留。
	tb.mu.Lock()
	tb.reapDoneLocked()
	tb.mu.Unlock()
	if _, ok := tb.jobs["j2"]; !ok {
		t.Fatal("j2 should survive at limit")
	}
}
