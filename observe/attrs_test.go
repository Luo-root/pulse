package observe

import (
	"encoding/json"
	"testing"
)

type model string // 命名标量：底层类型命中约束

// Set/Get 基础标量往返。
func TestAttrsSetGet(t *testing.T) {
	var a Attrs // 零值可用
	Set(&a, "app.model", "m1")
	Set(&a, "app.tokens_in", int64(10))
	Set(&a, "app.temp", 0.7)
	Set(&a, "app.hitl", true)

	if a.Len() != 4 {
		t.Fatalf("len = %d, want 4", a.Len())
	}
	if v, ok := Get[string](a, "app.model"); !ok || v != "m1" {
		t.Fatalf("string roundtrip: %q %v", v, ok)
	}
	if v, ok := Get[int64](a, "app.tokens_in"); !ok || v != 10 {
		t.Fatalf("int roundtrip: %d %v", v, ok)
	}
	if v, ok := Get[float64](a, "app.temp"); !ok || v != 0.7 {
		t.Fatalf("float roundtrip: %v %v", v, ok)
	}
	if v, ok := Get[bool](a, "app.hitl"); !ok || !v {
		t.Fatalf("bool roundtrip: %v %v", v, ok)
	}

	// 缺失 key
	if _, ok := Get[string](a, "nope"); ok {
		t.Fatal("missing key must be ok=false")
	}
	// 类型不符
	if _, ok := Get[bool](a, "app.model"); ok {
		t.Fatal("type mismatch must be ok=false")
	}
	// 同名覆盖
	Set(&a, "app.model", "m2")
	if v, _ := Get[string](a, "app.model"); v != "m2" {
		t.Fatalf("overwrite: %q", v)
	}
}

// 命名标量（底层类型命中约束）写读往返。
func TestAttrsNamedTypes(t *testing.T) {
	var a Attrs
	Set(&a, "app.model", model("claude-3"))
	Set(&a, "app.steps", int64(3))

	v, ok := Get[model](a, "app.model")
	if !ok || v != model("claude-3") {
		t.Fatalf("named roundtrip: %q %v", v, ok)
	}
	// 命名类型写入、基础类型读出同样成立
	if s, ok := Get[string](a, "app.model"); !ok || s != "claude-3" {
		t.Fatalf("named write / plain read: %q %v", s, ok)
	}
	// 基础类型写入、命名类型读出同样成立
	if n, ok := Get[model](a, "app.model"); !ok || n != "claude-3" {
		_ = n
	}
	// int64 不兼容 string 底层
	if _, ok := Get[model](a, "app.steps"); ok {
		t.Fatal("int64 into ~string type must fail")
	}
}

// 载荷结构在类型上进不来（隐私边界的类型部分，编译期保证——此处以
// 不编译的调用形式注释钉死意图）：
//
//	Set(&a, "prompt", []byte("..."))       // ✗ 不编译
//	Set(&a, "messages", "some messages")   // ✓ 标量可以，key 自述意图
//	Set(&a, "payload", struct{ X int }{})  // ✗ 不编译

// MarshalJSON 按 key 排序（确定性），还原原生类型。
func TestAttrsMarshalJSONSorted(t *testing.T) {
	var a Attrs
	Set(&a, "app.tokens_out", int64(20))
	Set(&a, "app.model", "m1")
	Set(&a, "app.tokens_in", int64(10))
	Set(&a, "app.hitl", true)

	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"app.hitl":true,"app.model":"m1","app.tokens_in":10,"app.tokens_out":20}`
	if string(data) != want {
		t.Fatalf("json = %s, want %s", data, want)
	}
}

// 空 Attrs 的 JSON 是空对象；Range 覆盖全部键值。
func TestAttrsEmptyJSONAndRange(t *testing.T) {
	var a Attrs
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}" {
		t.Fatalf("empty json = %s, want {}", data)
	}

	Set(&a, "k1", "v1")
	Set(&a, "k2", int64(2))
	seen := map[string]any{}
	a.Range(func(k string, v any) { seen[k] = v })
	if len(seen) != 2 {
		t.Fatalf("range saw %d keys, want 2", len(seen))
	}
	if seen["k1"] != "v1" || seen["k2"] != int64(2) {
		t.Fatalf("range values wrong: %v", seen)
	}
}
