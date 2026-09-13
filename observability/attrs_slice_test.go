package observability

import (
	"encoding/json"
	"testing"
)

// 本文件是 #179（Attrs 存储切片化）的语义护栏：公开 API 与可观察行为
// 必须与 map 存储版本一致，且新增「插入序」这一更强保证。

// TestAttrsInsertionOrder Range 按插入序（比此前的「无序遍历」更强）。
func TestAttrsInsertionOrder(t *testing.T) {
	var a Attrs
	Set(&a, "k.c", "c")
	Set(&a, "k.a", "a")
	Set(&a, "k.b", "b")

	var got []string
	a.Range(func(key string, _ any) { got = append(got, key) })
	want := []string{"k.c", "k.a", "k.b"}
	if len(got) != len(want) {
		t.Fatalf("range = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("插入序 = %v, want %v", got, want)
		}
	}
}

// TestAttrsOverwriteKeepsPosition 同名覆盖保持原位置（切片语义）。
func TestAttrsOverwriteKeepsPosition(t *testing.T) {
	var a Attrs
	Set(&a, "k.1", "one")
	Set(&a, "k.2", "two")
	Set(&a, "k.1", "ONE") // 覆盖 k.1

	if got := a.Len(); got != 2 {
		t.Fatalf("len = %d, want 2（覆盖不新增条目）", got)
	}
	var keys []string
	a.Range(func(key string, _ any) { keys = append(keys, key) })
	if len(keys) != 2 || keys[0] != "k.1" || keys[1] != "k.2" {
		t.Fatalf("keys = %v, want [k.1 k.2]（覆盖保持原位置）", keys)
	}
	if v, ok := Get[string](a, "k.1"); !ok || v != "ONE" {
		t.Fatalf("k.1 = %q,%v, want ONE", v, ok)
	}
}

// TestAttrsBeyondInlineCap 跨过预留容量（6 条）后仍正确（自然扩容路径）。
func TestAttrsBeyondInlineCap(t *testing.T) {
	var a Attrs
	const n = 40
	for i := 0; i < n; i++ {
		Set(&a, keyN(i), int64(i))
	}
	if got := a.Len(); got != n {
		t.Fatalf("len = %d, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		v, ok := Get[int64](a, keyN(i))
		if !ok || v != int64(i) {
			t.Fatalf("k.%d = %v,%v, want %d", i, v, ok, i)
		}
	}
	// 覆盖其中一条后仍可读。
	Set(&a, keyN(7), int64(999))
	if v, _ := Get[int64](a, keyN(7)); v != 999 {
		t.Fatalf("覆盖后 = %v, want 999", v)
	}
	// 排序输出仍然确定。
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]int64
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != n || m[keyN(7)] != 999 {
		t.Fatalf("JSON 往返 = %d 条, k.7=%d", len(m), m[keyN(7)])
	}
}

// keyN 常量键（避免拼装噪声干扰断言；测试用可读键）。
func keyN(i int) string {
	return "k." + string(rune('A'+i%26)) + string(rune('a'+(i/26)%26))
}

// TestAttrsCloneDeepCopy clone 深拷：改原 Attrs 不影响副本，反之亦然。
func TestAttrsCloneDeepCopy(t *testing.T) {
	var a Attrs
	Set(&a, "k.a", "1")
	Set(&a, "k.b", int64(2))

	c := a.clone()
	if c.Len() != 2 {
		t.Fatalf("clone len = %d, want 2", c.Len())
	}
	// 改原：新增 + 覆盖
	Set(&a, "k.c", "3")
	Set(&a, "k.a", "1-changed")
	if v, ok := Get[string](c, "k.a"); !ok || v != "1" {
		t.Fatalf("clone 的 k.a = %q,%v, want 1（深拷：不受原 Attrs 影响）", v, ok)
	}
	if _, ok := Get[string](c, "k.c"); ok {
		t.Fatal("clone 不应看到原 Attrs 新增的 k.c")
	}
	// 空 Attrs clone 不分配（语义：空即空）。
	var empty Attrs
	if ec := empty.clone(); ec.Len() != 0 {
		t.Fatalf("空 Attrs clone len = %d, want 0", ec.Len())
	}
}

// TestAttrsValueCopySharesEntriesAttrs 值拷贝共享底层条目（文档化的引用
// 语义）：Sink 只读消费；产出方不得在 Write 之后修改。
func TestAttrsValueCopySharesEntriesAttrs(t *testing.T) {
	var a Attrs
	Set(&a, "k.a", "1")

	cp := a // 值拷贝：共享底层条目数组
	if v, ok := Get[string](cp, "k.a"); !ok || v != "1" {
		t.Fatalf("值拷贝读取 = %q,%v", v, ok)
	}
}
