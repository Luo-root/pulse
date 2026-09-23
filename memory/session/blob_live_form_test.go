package session

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Luo-root/pulse/llm"
)

// bigImageBytes 造一份超过内联上限的图片字节（内容可复现）。
func bigImageBytes(n int) []byte {
	big := make([]byte, n)
	for i := range big {
		big[i] = byte(i % 253)
	}
	return big
}

// TestJSONLLiveFormMatchesReopen：带大 blob 的会话在**未重开**的当回合里，
// Surface 与 Events 拿到的 payload 与重开后逐字节一致——内存态恒为还原
// 形态，引用形态只存在于磁盘行。
//
// 此前内存态持引用形态：宿主 append 大图后当回合折 history 会拿到
// `blob:<sha>` 的 URL-only 图片块（供应商可拒的坏请求），而重启后症状消失。
func TestJSONLLiveFormMatchesReopen(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	sess, err := store.Create(ctx, SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	big := bigImageBytes(blobInlineLimit + 2048)
	if _, err := sess.Append(ctx, EventDraft{
		Type:    EventMessageUser,
		Data:    mustJSONPayload(t, MessagePayload{Parts: []llm.Part{llm.ImageData("application/octet-stream", big)}}),
		Surface: &SurfaceIntent{Op: SurfaceAppend},
	}); err != nil {
		t.Fatal(err)
	}

	// 未重开：当回合的 Surface 必须已是完整内联字节。
	live, err := sess.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := live[0].Parts[0].Image; got == nil || !bytes.Equal(got.Data, big) || got.URL != "" {
		t.Fatalf("live image = %+v, want inline bytes（blob 引用不得进模型请求）", live[0].Parts[0].Image)
	}
	liveEnvs, err := sess.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(liveEnvs) != 1 {
		t.Fatalf("events = %d, want 1", len(liveEnvs))
	}
	liveData := append([]byte(nil), liveEnvs[0].Data...)

	id := sess.Header().SessionID
	closeJSONL(t, sess)
	reopened, err := store.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer closeJSONL(t, reopened)
	after, err := reopened.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	afterEnvs, err := reopened.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	liveJSON, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(liveJSON, afterJSON) {
		t.Fatalf("live surface != reopened surface\n live=%s\n after=%s", liveJSON, afterJSON)
	}
	if !bytes.Equal(liveData, afterEnvs[0].Data) {
		t.Fatal("live envelope payload != reopened payload（内存态必须与重开同形态）")
	}
}

// TestJSONLForkSeedsChildBlobs：带大 blob 的父会话 Fork 出的子会话，字节
// 物化进子会话自己的 blobs 目录，**新 store**（新进程视角）能 Open 并还原。
//
// 此前 seed 传的是引用形态 → 子目录零 blob 文件 → 子会话日志引用缺失，
// 新 store Open 报 ErrCorruptLog（「写入成功但永远打不开」）。
func TestJSONLForkSeedsChildBlobs(t *testing.T) {
	store := newJSONLStore(t)
	ctx := t.Context()
	parent, err := store.Create(ctx, SessionHeader{SessionID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	big := bigImageBytes(blobInlineLimit + 1024)
	if _, err := parent.Append(ctx, EventDraft{
		Type:    EventMessageUser,
		Data:    mustJSONPayload(t, MessagePayload{Parts: []llm.Part{llm.ImageData("application/octet-stream", big)}}),
		Surface: &SurfaceIntent{Op: SurfaceAppend},
	}); err != nil {
		t.Fatal(err)
	}
	child, err := parent.Fork(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	childID := child.Header().SessionID
	childDir := filepath.Join(store.root, childID)
	if n := countFiles(t, filepath.Join(childDir, "blobs")); n != 1 {
		t.Fatalf("child blobs = %d, want 1（seed 字节必须在写子会话时物化）", n)
	}
	closeJSONL(t, child)
	closeJSONL(t, parent)

	fresh, err := NewJSONLStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := fresh.Open(ctx, childID)
	if err != nil {
		t.Fatalf("forked child must open in a fresh store: %v", err)
	}
	defer closeJSONL(t, reopened)
	surface, err := reopened.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := surface[0].Parts[0].Image; got == nil || !bytes.Equal(got.Data, big) {
		t.Fatal("forked child must restore the same bytes")
	}
	// 父会话自己的 blob 不受影响（子会话复制字节，不是搬走）。
	if n := countFiles(t, filepath.Join(store.root, "parent", "blobs")); n != 1 {
		t.Fatalf("parent blobs = %d, want 1", n)
	}
}
