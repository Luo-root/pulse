package kernel

import (
	"strings"
	"testing"
)

// 空名 Fiber 走 Name 的防御分支：必须领号存回——两次调用返回同一
// 稳定名。若实现退化为 Load() 只读不存，名字会随全局计数器漂移，
// 两次调用结果不同即暴露（不断言具体 N：fiberSeq 是全局计数器，
// 与其他测试共享）。
func TestFiberNameFallbackStable(t *testing.T) {
	f := &Fiber{} // 未走 Use/setName 的防御路径
	n1 := f.Name()
	n2 := f.Name()
	if n1 == "" || n2 == "" {
		t.Fatalf("name must not be empty: %q %q", n1, n2)
	}
	if n1 != n2 {
		t.Fatalf("fallback name drifted: %q then %q", n1, n2)
	}
	if !strings.HasPrefix(n1, "fiber#") {
		t.Fatalf("fallback name = %q, want fiber#N prefix", n1)
	}
}

// 正常路径：Use 创建的 Func 插件 fiber 名字 = funcPlugin#N，Name()
// 直接返回 f.name，不走兜底（名字两次调用一致由 setName 一次性
// 赋值保证）。
func TestFiberNameFromUse(t *testing.T) {
	host := New()
	defer host.Dispose()
	f, err := Use(host, Func(func(*Context) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	n1 := f.Name()
	n2 := f.Name()
	if n1 != n2 {
		t.Fatalf("name drifted: %q then %q", n1, n2)
	}
	if !strings.HasPrefix(n1, "funcPlugin#") {
		t.Fatalf("name = %q, want funcPlugin#N prefix", n1)
	}
}
