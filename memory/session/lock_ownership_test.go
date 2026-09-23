package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLockReleaseAfterPreemption：stale 抢占之后，旧持有者的 Close 不得删掉
// 新持有者的锁——否则第三个写者随即可以打开同一 events.jsonl，单写者失效。
//
// 触发面：长 HITL 等待 / 宿主挂起超过 stale 阈值就会被另一进程接管，旧持有者
// 稍后 Close 时此前会无条件 os.Remove 掉别人的锁。
func TestLockReleaseAfterPreemption(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	lockPath := filepath.Join(root, "s1", "lock")

	first, err := NewJSONLStore(root)
	if err != nil {
		t.Fatal(err)
	}
	sessA, err := first.Create(ctx, SessionHeader{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	rawA, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var holderA lockInfo
	if err := json.Unmarshal(rawA, &holderA); err != nil || holderA.Token == "" {
		t.Fatalf("lock file must carry a holder token: %v (%s)", err, rawA)
	}

	// 把锁 mtime 推老 = 「持有者已崩溃」的唯一判定依据（不 sleep 撞时序）。
	old := time.Now().Add(-2 * defaultLockStale)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	// 第二个 store 实例代表另一进程：抢占成功，锁内容换成新 token。
	second, err := NewJSONLStore(root)
	if err != nil {
		t.Fatal(err)
	}
	sessB, err := second.Open(ctx, "s1")
	if err != nil {
		t.Fatalf("stale lock must be preemptible: %v", err)
	}
	defer closeJSONL(t, sessB)
	rawB, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var holderB lockInfo
	if err := json.Unmarshal(rawB, &holderB); err != nil || holderB.Token == holderA.Token {
		t.Fatalf("preemption must rewrite the lock with a fresh token: %v (%s)", err, rawB)
	}

	// 旧持有者 Close：锁必须还在（现在属于 B）。
	if err := sessA.(*jsonlSession).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("preempted holder removed the new owner's lock: %v", err)
	}

	// 第三个写者仍被拒：单写者保证成立。
	third, err := NewJSONLStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := third.Open(ctx, "s1"); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("third writer err = %v, want ErrWriterBusy", err)
	}
}
