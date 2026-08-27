package llm

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Luo-root/pulse/kernel"
)

// 双请求 scope：llm.generate 事件只进入发出方的 scope。
func TestLLMEventsStayInRequestScope(t *testing.T) {
	root := kernel.New()
	defer root.Dispose()

	// Registry 挂在 root；两个请求 scope 是兄弟。
	reg := NewRegistry(root)
	_, _ = reg.RegisterProvider(root, "mock", func(Config) (ChatModel, error) {
		return NewScripted(Resp("ok")), nil
	})
	if err := reg.Declare("main", Config{Provider: "mock", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	model, err := reg.Open("main")
	if err != nil {
		t.Fatal(err)
	}

	scopeA, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer scopeA.Dispose()
	scopeB, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer scopeB.Dispose()

	var aN, bN atomic.Int32
	if _, err := kernel.On(scopeA, EventAfterResponse, func(*Response) { aN.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if _, err := kernel.On(scopeB, EventAfterResponse, func(*Response) { bN.Add(1) }); err != nil {
		t.Fatal(err)
	}

	if _, err := model.Generate(WithEventScope(context.Background(), scopeA), NewRequest(UserText("q"))); err != nil {
		t.Fatal(err)
	}
	if aN.Load() != 1 || bN.Load() != 0 {
		t.Fatalf("llm event leaked: A=%d B=%d", aN.Load(), bN.Load())
	}
}
