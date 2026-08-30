package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/llm"
)

func newJSONLStore(t *testing.T, opts ...jsonlOption) *JSONLStore {
	t.Helper()
	store, err := NewJSONLStore(t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestJSONLRoundTrip：全类型事件 Append → Close → Open 重放，Events 与
// Surface 语义一致；带 ImageData 的 user 消息 fold 出同样 Part（≤32KiB
// base64 内联路径）。
func TestJSONLRoundTrip(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{Workspace: "w"})
	if err != nil {
		t.Fatal(err)
	}
	smallPNG := bytes.Repeat([]byte{0x89, 0x50}, 100) // 200B：内联
	appends := []EventDraft{
		lifecycleDraft(t, EventTurnStarted, "t1", ""),
		userDraft(t, "hello"),
		{Type: EventMessageUser, Data: mustJSONPayload(t, MessagePayload{Parts: []llm.Part{
			llm.Text("see attachment"), llm.ImageData("image/png", smallPNG),
		}}), Surface: &SurfaceIntent{Op: SurfaceAppend}},
		assistantDraft(t,
			llm.Call(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}),
			llm.Text("checking"),
		),
		toolResultDraft(t, "c1", "found", false),
		assistantDraft(t, llm.Text("done")),
		lifecycleDraft(t, EventTurnEnded, "t1", ReasonCompleted),
	}
	var wantEnvs []EventEnvelope
	for _, d := range appends {
		env, err := sess.Append(ctx, d)
		if err != nil {
			t.Fatalf("append %s: %v", d.Type, err)
		}
		wantEnvs = append(wantEnvs, env)
	}
	if err := sess.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	id := sess.Header().SessionID
	closeJSONL(t, sess)

	reopened, err := store.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	gotEnvs, err := reopened.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotEnvs) != len(wantEnvs) {
		t.Fatalf("events = %d, want %d", len(gotEnvs), len(wantEnvs))
	}
	for i, want := range wantEnvs {
		got := gotEnvs[i]
		if got.Seq != want.Seq || got.Type != want.Type || !got.Time.Equal(want.Time) {
			t.Fatalf("env[%d] = {seq %d time %v type %s}, want {seq %d time %v type %s}",
				i, got.Seq, got.Time, got.Type, want.Seq, want.Time, want.Type)
		}
	}
	// Surface：7 条事件折出 user/user/assistant/tool/assistant 五个节点。
	surface, err := reopened.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) != 5 {
		t.Fatalf("surface = %d nodes, want 5", len(surface))
	}
	img := surface[1].Parts[1]
	if img.Kind != llm.PartImage || img.Image == nil || !bytes.Equal(img.Image.Data, smallPNG) || img.Image.MediaType != "image/png" {
		t.Fatalf("roundtrip image part = %+v（roundtrip 后必须 fold 出同样 Part）", img)
	}
	if img.Image.URL != "" {
		t.Errorf("inline image must not carry blob URL: %q", img.Image.URL)
	}
	closeJSONL(t, reopened)
}

// TestJSONLBlobSpill：>32KiB 的 ImageData 溢出为内容寻址 blob，文件行持
// 引用形态，重开后字节等价还原；同一字节不重复落盘。
func TestJSONLBlobSpill(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, blobInlineLimit+1024)
	for i := range big {
		big[i] = byte(i % 251)
	}
	env, err := sess.Append(ctx, EventDraft{
		Type:    EventMessageUser,
		Data:    mustJSONPayload(t, MessagePayload{Parts: []llm.Part{llm.ImageData("application/octet-stream", big)}}),
		Surface: &SurfaceIntent{Op: SurfaceAppend},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := sess.Header().SessionID
	closeJSONL(t, sess)
	// 文件行持引用形态：URL = blob:<sha>，无 base64 字节。
	raw, err := os.ReadFile(filepath.Join(store.root, id, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), blobURLPrefix) {
		t.Fatal("oversized inline bytes must be replaced by a blob reference on disk")
	}
	if !strings.Contains(string(raw), `"URL":"blob:`) {
		t.Fatalf("reference form unexpected: %s", raw[:min(200, len(raw))])
	}
	var fileEnv EventEnvelope
	if err := json.Unmarshal(extractLine(raw, env.Seq), &fileEnv); err != nil {
		t.Fatal(err)
	}
	var p MessagePayload
	if err := json.Unmarshal(fileEnv.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Parts[0].Image == nil || p.Parts[0].Image.Data != nil || !isBlobRef(p.Parts[0].Image.URL) {
		t.Fatalf("on-disk part = %+v, want URL-only blob reference", p.Parts[0].Image)
	}
	// 重开：还原为等价字节。
	reopened, err := store.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	surface, err := reopened.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := surface[0].Parts[0]
	if got.Image == nil || !bytes.Equal(got.Image.Data, big) {
		t.Fatal("blob roundtrip bytes differ（禁止静默丢字节）")
	}
	if got.Image.URL != "" {
		t.Fatalf("restored part must not keep blob URL: %q", got.Image.URL)
	}
	// 内容寻址去重：同字节重复 append 不新增 blob 文件。
	blobsBefore := countFiles(t, filepath.Join(store.root, id, "blobs"))
	if _, err := reopened.Append(ctx, EventDraft{
		Type:    EventMessageUser,
		Data:    mustJSONPayload(t, MessagePayload{Parts: []llm.Part{llm.ImageData("application/octet-stream", big)}}),
		Surface: &SurfaceIntent{Op: SurfaceAppend},
	}); err != nil {
		t.Fatal(err)
	}
	if blobsAfter := countFiles(t, filepath.Join(store.root, id, "blobs")); blobsAfter != blobsBefore {
		t.Fatalf("blob files %d → %d, want dedup", blobsBefore, blobsAfter)
	}
	closeJSONL(t, reopened)
}

// TestJSONLBlobMissing：引用指向的 blob 缺失 = 加载错误（fail closed）。
func TestJSONLBlobMissing(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, blobInlineLimit+1)
	if _, err := sess.Append(ctx, EventDraft{
		Type:    EventMessageUser,
		Data:    mustJSONPayload(t, MessagePayload{Parts: []llm.Part{llm.ImageData("application/octet-stream", big)}}),
		Surface: &SurfaceIntent{Op: SurfaceAppend},
	}); err != nil {
		t.Fatal(err)
	}
	id := sess.Header().SessionID
	closeJSONL(t, sess)
	blobs := filepath.Join(store.root, id, "blobs")
	entries, _ := os.ReadDir(blobs)
	if err := os.Remove(filepath.Join(blobs, entries[0].Name())); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, id); err == nil {
		t.Fatal("open must fail when a referenced blob is missing")
	}
}

// TestJSONLTornTail：撕裂尾只丢无法验证的碎片，合法前缀完整，截断后可
// 接续 append（§9.3；crash tests）。
func TestJSONLTornTail(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(ctx, userDraft(t, "q1")); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(ctx, assistantDraft(t, llm.Text("a1"))); err != nil {
		t.Fatal(err)
	}
	id := sess.Header().SessionID
	closeJSONL(t, sess)
	// 模拟崩溃：追加半行（写完一半的 JSON，无换行）。
	path := filepath.Join(store.root, id, "events.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"seq":3,"time":"2026-08-30T08:`) // 半截
	f.Close()

	reopened, err := store.Open(ctx, id)
	if err != nil {
		t.Fatalf("torn tail must not fail Open: %v", err)
	}
	events, _ := reopened.Events(ctx, 0)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2（合法前缀不丢）", len(events))
	}
	// 截断后接续 append：seq 连续。
	if _, err := reopened.Append(ctx, userDraft(t, "q2")); err != nil {
		t.Fatal(err)
	}
	closeJSONL(t, reopened)
	again, err := store.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	events, _ = again.Events(ctx, 0)
	if len(events) != 3 || events[2].Seq != 3 {
		t.Fatalf("events = %d, seq[2] = %d; want 3 / 3（截断后接续写不跳 seq）", len(events), events[2].Seq)
	}
	// 「合法但无换行」的尾行同样按撕裂丢弃。
	closeJSONL(t, again)
	validNoNewline := []byte(`{"seq":4,"time":"2026-08-30T08:00:00Z","type":"turn.started"}`)
	if err := os.WriteFile(path, append(readAll(t, path), validNoNewline...), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := store.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	events, _ = third.Events(ctx, 0)
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3（无换行的尾行 = 未成功 append，丢弃）", len(events))
	}
	closeJSONL(t, third)
}

// TestJSONLCorruptMiddleLine：日志中部坏行 → 拒绝加载（fail closed）。
func TestJSONLCorruptMiddleLine(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	sess, _ := store.Create(ctx, SessionHeader{})
	if _, err := sess.Append(ctx, userDraft(t, "q")); err != nil {
		t.Fatal(err)
	}
	id := sess.Header().SessionID
	closeJSONL(t, sess)
	path := filepath.Join(store.root, id, "events.jsonl")
	if err := os.WriteFile(path, []byte("{\"seq\":1,\"type\":\"message.user\"}\n{bad line}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, id); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("err = %v, want ErrCorruptLog", err)
	}
}

// TestJSONLSeqGap：seq 断链 → 拒绝加载。
func TestJSONLSeqGap(t *testing.T) {
	store := newJSONLStore(t)
	sess, _ := store.Create(t.Context(), SessionHeader{})
	id := sess.Header().SessionID
	closeJSONL(t, sess)
	path := filepath.Join(store.root, id, "events.jsonl")
	lines := []string{
		`{"seq":1,"time":"2026-08-30T08:00:00Z","type":"turn.started"}`,
		`{"seq":5,"time":"2026-08-30T08:00:01Z","type":"turn.ended"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(t.Context(), id); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("err = %v, want ErrCorruptLog（seq 断链拒绝加载）", err)
	}
}

// TestJSONLFormatVersion：header 版本不兼容拒绝加载，不猜测迁移。
func TestJSONLFormatVersion(t *testing.T) {
	store := newJSONLStore(t)
	sess, _ := store.Create(t.Context(), SessionHeader{})
	id := sess.Header().SessionID
	closeJSONL(t, sess)
	hdrPath := filepath.Join(store.root, id, "header.json")
	var hdr SessionHeader
	if err := json.Unmarshal(readAll(t, hdrPath), &hdr); err != nil {
		t.Fatal(err)
	}
	hdr.FormatVersion = FormatVersion + 1
	if err := os.WriteFile(hdrPath, mustJSON(hdr), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(t.Context(), id); !errors.Is(err, ErrFormatVersion) {
		t.Fatalf("err = %v, want ErrFormatVersion", err)
	}
}

// TestJSONLFileLock：同会话并发打开拒绝第二写者（文件锁兜底）；Close
// 释放后可重入；stale 锁可抢占。
func TestJSONLFileLock(t *testing.T) {
	store := newJSONLStore(t, JSONLStale(50*time.Millisecond))
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{SessionID: "locked"})
	if err != nil {
		t.Fatal(err)
	}
	// 同进程重复 Open 走缓存（内存互斥接手）。
	if _, err := store.Open(ctx, "locked"); err != nil {
		t.Fatal(err)
	}
	// 模拟另一进程：直接抢锁文件 → ErrWriterBusy。
	if _, err := acquireSessionLock(filepath.Join(store.root, "locked", "lock"), time.Hour); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("err = %v, want ErrWriterBusy", err)
	}
	// Close 释放锁 → 可重新抢。
	closeJSONL(t, sess)
	releaseC, err := acquireSessionLock(filepath.Join(store.root, "locked", "lock"), time.Hour)
	if err != nil {
		t.Fatalf("lock must be released on Close: %v", err)
	}
	releaseC()
	release, err := acquireSessionLock(filepath.Join(store.root, "locked", "lock"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	release()
	// stale 锁（mtime 超过阈值）→ Open 抢占成功。
	staleLock := filepath.Join(store.root, "locked", "lock")
	if _, err := acquireSessionLock(staleLock, time.Hour); err != nil {
		t.Fatal(err)
	}
	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleLock, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, "locked")
	if err != nil {
		t.Fatalf("stale lock must be preempted: %v", err)
	}
	closeJSONL(t, reopened)
}

// TestJSONLConcurrentCreate：并发 Create 同 ID 恰好一个成功（目录 O_EXCL）。
func TestJSONLConcurrentCreate(t *testing.T) {
	store := newJSONLStore(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	var winner Session
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := store.Create(t.Context(), SessionHeader{SessionID: "race"})
			errs[i] = err
			if err == nil {
				mu.Lock()
				winner = s
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrSessionExists) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one creator must win; got %d", ok)
	}
	closeJSONL(t, winner)
}

// TestJSONLColdRecoverPersists：文件版冷恢复把合成事件真实写回日志
// （重开两次，第二次不重复补写）。
func TestJSONLColdRecoverPersists(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{SessionID: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(ctx, lifecycleDraft(t, EventTurnStarted, "t1", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(ctx, assistantDraft(t, llm.Call(llm.ToolCall{ID: "c1"}))); err != nil {
		t.Fatal(err)
	}
	closeJSONL(t, sess)
	reopened, err := store.Open(ctx, "crash")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := reopened.Events(ctx, 0)
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4（unpaired result + turn.ended 写回 log）", len(events))
	}
	closeJSONL(t, reopened)
	again, err := store.Open(ctx, "crash")
	if err != nil {
		t.Fatal(err)
	}
	events2, _ := again.Events(ctx, 0)
	if len(events2) != 4 {
		t.Fatalf("events = %d after reopen, want 4（恢复幂等，合成事件在盘上只补一次）", len(events2))
	}
	surface, err := again.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) != 2 || surface[1].Role != llm.RoleTool {
		t.Fatalf("surface = %v", surface)
	}
	closeJSONL(t, again)
}

// TestJSONLFork：子会话连同 seed 落盘，重开重放一致；父后续事件不污染子。
func TestJSONLFork(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	parent, err := store.Create(ctx, SessionHeader{SessionID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Append(ctx, userDraft(t, "q")); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Append(ctx, assistantDraft(t, llm.Text("a"))); err != nil {
		t.Fatal(err)
	}
	child, err := parent.Fork(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	childHdr := child.Header()
	if childHdr.ParentSessionID != "parent" || childHdr.SeedLength != 2 {
		t.Fatalf("child header = %+v", childHdr)
	}
	if _, err := parent.Append(ctx, userDraft(t, "after-fork")); err != nil {
		t.Fatal(err)
	}
	closeJSONL(t, child)
	reopened, err := store.Open(ctx, childHdr.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	events, _ := reopened.Events(ctx, 0)
	if len(events) != 2 {
		t.Fatalf("child events = %d, want 2", len(events))
	}
	closeJSONL(t, reopened)
	closeJSONL(t, parent)
}

// TestJSONLCrossProcessDelete：跨进程删除后，持有句柄一方的 Append 必须
// ErrDeleted——Unix/Windows 的句柄写入在目录被删后都会静默成功，stat 防护
// 堵死「报成功但数据进 void」。
func TestJSONLCrossProcessDelete(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{SessionID: "cross"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(ctx, userDraft(t, "x")); err != nil {
		t.Fatal(err)
	}
	// 模拟「会话目录已消失」：Windows 上 Go 文件句柄不带 FILE_SHARE_DELETE，
	// 进程内删不掉/改不了名打开中文件所在目录（真实跨进程删除在 Windows
	// 会直接失败由宿主协调；Unix unlink 立即生效）。白盒把 dir 指向不存
	// 在路径，直接验证防护分支：路径不在 → 拒绝写入，两平台语义一致。
	js := sess.(*jsonlSession)
	js.dir = filepath.Join(store.root, "vanished")
	if _, err := sess.Append(ctx, userDraft(t, "y")); !errors.Is(err, ErrDeleted) {
		t.Fatalf("append after cross-process delete: %v, want ErrDeleted", err)
	}
	closeJSONL(t, sess)
}

// TestJSONLLockHeartbeat：Flush 兼作锁心跳——持锁超过 stale 阈值但有心跳
// 的会话不被误抢占（对照：无心跳的同龄锁被另一 store 实例抢占）。
func TestJSONLLockHeartbeat(t *testing.T) {
	store := newJSONLStore(t, JSONLStale(50*time.Millisecond))
	// 第二个 store 实例模拟另一进程：独立 open 缓存，Open 会真正走到文件锁。
	peer, err := NewJSONLStore(store.root, JSONLStale(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{SessionID: "beat"})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(store.root, "beat", "lock")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	// 心跳：Flush touch 锁文件 mtime。
	if err := sess.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Open(ctx, "beat"); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("open after heartbeat: %v, want ErrWriterBusy（心跳后的长命会话不被误抢占）", err)
	}
	// 对照：无心跳的同龄锁被抢占（peer 侧接管成功）。
	noheart, err := store.Create(ctx, SessionHeader{SessionID: "noheart"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(store.root, "noheart", "lock"), old, old); err != nil {
		t.Fatal(err)
	}
	taken, err := peer.Open(ctx, "noheart")
	if err != nil {
		t.Fatalf("stale lock without heartbeat must be preempted: %v", err)
	}
	closeJSONL(t, taken)
	closeJSONL(t, noheart)
	closeJSONL(t, sess)
}

// TestJSONLSessionClosed：Close 后的 Append/Flush 收到显式 ErrSessionClosed
// （不依赖 os.File 的 nil-safe 巧合）。
func TestJSONLSessionClosed(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{SessionID: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	closeJSONL(t, sess)
	if _, err := sess.Append(ctx, userDraft(t, "x")); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("append after close: %v, want ErrSessionClosed", err)
	}
	if err := sess.Flush(ctx); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("flush after close: %v, want ErrSessionClosed", err)
	}
}

func TestJSONLList(t *testing.T) {
	store := newJSONLStore(t, JSONLPageSize(1))
	base := time.Now()
	for i, id := range []string{"b", "a", "c"} {
		at := base.Add(time.Duration(i) * time.Minute)
		s, err := store.Create(t.Context(), SessionHeader{SessionID: id, CreatedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		closeJSONL(t, s)
	}
	// CreatedAt 降序：c(+2min) > a(+1min) > b(+0min)。
	var got []string
	next := ""
	for {
		headers, n, err := store.List(t.Context(), SessionFilter{After: next})
		if err != nil {
			t.Fatal(err)
		}
		if len(headers) == 0 {
			break
		}
		got = append(got, headers[0].SessionID)
		if n == "" {
			break
		}
		next = n
	}
	if strings.Join(got, ",") != "c,a,b" {
		t.Fatalf("listed %v, want [c a b]", got)
	}
	if _, _, err := store.List(t.Context(), SessionFilter{After: "gone"}); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("err = %v, want ErrCursorStale", err)
	}
}

// TestJSONLDelete：目录删除 + Open NotFound + 已持实例 fail closed。
func TestJSONLDelete(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{SessionID: "doomed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(ctx, userDraft(t, "x")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "doomed")); !os.IsNotExist(err) {
		t.Fatal("session dir must be removed")
	}
	if _, err := store.Open(ctx, "doomed"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("open after delete: %v", err)
	}
	if _, err := sess.Append(ctx, userDraft(t, "late")); !errors.Is(err, ErrDeleted) {
		t.Fatalf("append after delete: %v, want ErrDeleted", err)
	}
	if err := store.Delete(ctx, "doomed"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("double delete: %v", err)
	}
}

// TestJSONLInvalidSessionID：路径穿越 ID 拒绝（fail closed）；空 ID 与
// 内存版同语义——由 store 生成。
func TestJSONLInvalidSessionID(t *testing.T) {
	store := newJSONLStore(t)
	for _, id := range []string{"../evil", "a/b", "a\\b", strings.Repeat("x", 129), "bad id", "点"} {
		if _, err := store.Create(t.Context(), SessionHeader{SessionID: id}); err == nil {
			t.Fatalf("invalid session id %q must be rejected", id)
		}
	}
	// 空 ID → 生成（与内存版 TestCreateNormalizesHeader 同语义）。
	sess, err := store.Create(t.Context(), SessionHeader{SessionID: ""})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Header().SessionID == "" {
		t.Fatal("empty SessionID must be generated")
	}
	closeJSONL(t, sess)
	if _, err := store.Open(t.Context(), "../evil"); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("open invalid id: %v, want ErrInvalidSessionID", err)
	}
}

// ---- helpers ----

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func extractLine(raw []byte, seq uint64) []byte {
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		if strings.Contains(line, `"seq":`) && strings.HasPrefix(line, `{"seq":`) {
			var probe struct {
				Seq uint64 `json:"seq"`
			}
			if json.Unmarshal([]byte(line), &probe) == nil && probe.Seq == seq {
				return []byte(line)
			}
		}
	}
	return nil
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

// closeJSONL 释放文件版会话的文件锁（Close 在实现类型上，不在 Session 接口）。
func closeJSONL(t *testing.T, s Session) {
	t.Helper()
	c, ok := s.(interface{ Close() error })
	if !ok {
		t.Fatal("jsonl session must implement Close")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
