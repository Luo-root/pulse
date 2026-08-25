package loop

import (
	"context"
	"fmt"
	"runtime"
	"time"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
)

// loop 侧压力测试：长回合、并发 Run 风暴、事件洪峰下的轨迹完整性。

// L1：500 步长回合——msgs 切片线性增长、事件高频派发、工具反复执行，
// 全程 -race 且结果一致。
func TestStressLongRun500Steps(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c", Name: "tick", Arguments: []byte(`{}`)}),
	)
	var ticks atomic.Int64
	tools := &fakeTools{fn: func(call llm.ToolCall) (string, error) {
		ticks.Add(1)
		return "t", nil
	}}
	a, err := NewAgent(model, WithToolSet(tools), WithEventScope(scope), WithMaxSteps(500))
	if err != nil {
		t.Fatal(err)
	}

	res, err := a.Run(context.Background(), nil, llm.UserText("go long"))
	if err != nil {
		t.Fatal(err)
	}
	if res.StoppedBy != StopMaxSteps || res.Steps != 500 {
		t.Fatalf("stoppedBy=%s steps=%d", res.StoppedBy, res.Steps)
	}
	if got := ticks.Load(); got != 500 {
		t.Fatalf("tool executions = %d, want 500", got)
	}
	want := 500 * 2 // ?? assistant(toolcall)+tool ????? system/input
	if len(res.Messages) != want {
		t.Fatalf("messages = %d, want %d (only this turn's output)", len(res.Messages), want)
	}
}

// L2：32 并发 Run 共享 Agent + 共享 ToolSet + 同一事件作用域。
func TestStressConcurrentRuns(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(llm.Resp("ok"))
	tools := &fakeTools{fn: func(call llm.ToolCall) (string, error) { return "r", nil }}
	a, err := NewAgent(model, WithToolSet(tools), WithEventScope(scope))
	if err != nil {
		t.Fatal(err)
	}

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := a.Run(context.Background(), nil, llm.UserText(fmt.Sprintf("q%d", i)))
			if err != nil {
				errs[i] = err
				return
			}
			if res.Final == nil || res.Final.Text() != "ok" {
				errs[i] = fmt.Errorf("run %d: final=%v", i, res.Final)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

// L3：事件洪峰——监听器全量记录轨迹的同时跑大量回合，
// 轨迹条数必须与理论值严格一致（事件不丢不重）。
func TestStressEventFloodTraceCount(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(llm.Resp("done"))
	a, err := NewAgent(model, WithEventScope(scope), WithSystemPrompt("s"))
	if err != nil {
		t.Fatal(err)
	}

	tr := &trace{}
	for _, reg := range []func() error{
		func() error { _, e := kernel.On(scope, EventTurnStart, func(*TurnStart) { tr.record("ts") }); return e },
		func() error { _, e := kernel.On(scope, EventStepStart, func(*StepStart) { tr.record("ss") }); return e },
		func() error { _, e := kernel.On(scope, EventAfterModel, func(*AfterModel) { tr.record("am") }); return e },
		func() error { _, e := kernel.On(scope, EventTurnEnd, func(*TurnEnd) { tr.record("te") }); return e },
	} {
		if err := reg(); err != nil {
			t.Fatal(err)
		}
	}

	const turns = 100
	for i := 0; i < turns; i++ {
		if _, err := a.Run(context.Background(), nil, llm.UserText("q")); err != nil {
			t.Fatal(err)
		}
	}

	want := turns * 4 // ts+ss+am+te 每回合各一条
	if got := strings.Count(tr.joined(), "|") + 1; got != want {
		t.Fatalf("trace lines = %d, want %d (events lost or duplicated)", got, want)
	}
}

// L4：goroutine 泄漏——大量短回合后协程数回落（settleLoop 不存在于
// loop，但 kernel 的变更订阅链路会被 Provide 触发）。
func TestStressLoopGoroutineLeak(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(llm.Resp("x"))
	tools := &fakeTools{fn: func(call llm.ToolCall) (string, error) { return "", nil }}
	a, err := NewAgent(model, WithToolSet(tools), WithEventScope(scope), WithMaxSteps(2))
	if err != nil {
		t.Fatal(err)
	}

	for warm := 0; warm < 5; warm++ {
		if _, err := a.Run(context.Background(), nil, llm.UserText("warm")); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 200; i++ {
		if _, err := a.Run(context.Background(), nil, llm.UserText("q")); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		now := runtime.NumGoroutine()
		if now <= base+2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: base=%d now=%d", base, now)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// L5：拒绝策略风暴——waterfall 监听器按规则拒绝一半的工具调用，
// 断言被拒与被执行的计数严格互补（协议无歧义）。
func TestStressRejectHalfPolicy(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c", Name: "echo", Arguments: []byte(`{}`)}),
	)
	tools := &fakeTools{fn: func(call llm.ToolCall) (string, error) { return "ran", nil }}
	a, err := NewAgent(model, WithToolSet(tools), WithEventScope(scope), WithMaxSteps(40))
	if err != nil {
		t.Fatal(err)
	}

	unsub, err := scopeEffectRejectOdd(scope)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	res, rerr := a.Run(context.Background(), nil, llm.UserText("q"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	// 40 步全部是 tool_calls：第奇数次调用被拒。执行次数 == 被拒次数 ± 1
	// （从第 1 次开始计数：1 拒、2 执、3 拒……40 步中执行 20 次）。
	if got := tools.callCount(); got != 20 {
		t.Fatalf("executed = %d, want 20", got)
	}
	var rejected int
	for _, m := range res.Messages {
		for _, part := range m.Parts {
			if p := part.ToolResultValue; p != nil && p.IsError {
				rejected++
			}
		}
	}
	if rejected != 20 {
		t.Fatalf("rejected results = %d, want 20", rejected)
	}
}

// scopeEffectRejectOdd 在 scope 上挂一个按调用序号拒绝奇数次的 waterfall 监听。
func scopeEffectRejectOdd(scope *kernel.Context) (func(), error) {
	var counter atomic.Int64
	d, err := kernel.OnWaterfall(scope, EventBeforeToolCall, func(p *BeforeToolCall, next func(*BeforeToolCall) *BeforeToolCall) *BeforeToolCall {
		if counter.Add(1)%2 == 1 {
			p.Rejected = true
			p.RejectReason = "odd"
			return p
		}
		return next(p)
	})
	return d, err
}
