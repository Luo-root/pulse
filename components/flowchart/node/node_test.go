package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/flow"
)

// ============================================================
// SimpleNode 测试
// ============================================================

func TestSimpleNode_BasicExecution(t *testing.T) {
	n := NewNode("test", []string{"input"}, []string{"output"},
		func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			return map[string]any{"output": "result"}, nil
		},
	)

	if n.ID() != "test" {
		t.Fatalf("expected ID 'test', got %q", n.ID())
	}
	if len(n.Inputs()) != 1 || n.Inputs()[0] != "input" {
		t.Fatalf("unexpected inputs: %v", n.Inputs())
	}
	if len(n.Outputs()) != 1 || n.Outputs()[0] != "output" {
		t.Fatalf("unexpected outputs: %v", n.Outputs())
	}
}

func TestSimpleNode_AddAspect(t *testing.T) {
	n := NewNode("test", nil, nil,
		func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			return nil, nil
		},
	)

	aspect := &AroundAspect{
		BeforeFn: func(ctx *flow.FlowContext, node Node) {},
		AfterFn:  func(ctx *flow.FlowContext, node Node, err error) {},
	}
	n.AddAspect(aspect)

	if len(n.Aspects()) != 1 {
		t.Fatalf("expected 1 aspect, got %d", len(n.Aspects()))
	}
}

// ============================================================
// Aspect 测试
// ============================================================

func TestBeforeAspect(t *testing.T) {
	called := false
	aspect := &BeforeAspect{
		Fn: func(ctx *flow.FlowContext, node Node) {
			called = true
		},
	}

	aspect.Before(nil, nil)
	if !called {
		t.Fatal("BeforeFn was not called")
	}

	// After 应该是 no-op
	aspect.After(nil, nil, nil)
}

func TestAfterAspect(t *testing.T) {
	called := false
	var receivedErr error
	aspect := &AfterAspect{
		Fn: func(ctx *flow.FlowContext, node Node, err error) {
			called = true
			receivedErr = err
		},
	}

	// Before 应该是 no-op
	aspect.Before(nil, nil)

	testErr := errors.New("test")
	aspect.After(nil, nil, testErr)
	if !called {
		t.Fatal("AfterFn was not called")
	}
	if receivedErr != testErr {
		t.Fatalf("expected test error, got %v", receivedErr)
	}
}

func TestAroundAspect(t *testing.T) {
	var beforeCalled, afterCalled bool
	aspect := &AroundAspect{
		BeforeFn: func(ctx *flow.FlowContext, node Node) { beforeCalled = true },
		AfterFn:  func(ctx *flow.FlowContext, node Node, err error) { afterCalled = true },
	}

	aspect.Before(nil, nil)
	aspect.After(nil, nil, nil)

	if !beforeCalled || !afterCalled {
		t.Fatalf("before=%v, after=%v", beforeCalled, afterCalled)
	}
}

func TestAroundAspect_NilFuncs(t *testing.T) {
	aspect := &AroundAspect{}
	// 不应该 panic
	aspect.Before(nil, nil)
	aspect.After(nil, nil, nil)
}

// ============================================================
// Interceptor 测试
// ============================================================

func TestRetryInterceptor_Success(t *testing.T) {
	retry := NewRetryInterceptor(3, 10*time.Millisecond)
	callCount := 0

	invoker := func() (map[string]any, error) {
		callCount++
		if callCount < 2 {
			return nil, errors.New("temp error")
		}
		return map[string]any{"result": "ok"}, nil
	}

	result, err := retry.Around(nil, nil, invoker)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["result"] != "ok" {
		t.Fatalf("unexpected result: %v", result)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 calls, got %d", callCount)
	}
}

func TestRetryInterceptor_Exhausted(t *testing.T) {
	retry := NewRetryInterceptor(3, 5*time.Millisecond)
	callCount := 0

	invoker := func() (map[string]any, error) {
		callCount++
		return nil, errors.New("persistent error")
	}

	_, err := retry.Around(nil, nil, invoker)
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if callCount != 3 {
		t.Fatalf("expected 3 calls, got %d", callCount)
	}
}

func TestTimeoutInterceptor_Success(t *testing.T) {
	timeout := NewTimeoutInterceptor(time.Second)

	// 创建一个简单的 mock node
	node := NewNode("fast", nil, nil, nil)

	invoker := func() (map[string]any, error) {
		return map[string]any{"done": true}, nil
	}

	result, err := timeout.Around(nil, node, invoker)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["done"] != true {
		t.Fatalf("unexpected result: %v", result)
	}
}

func TestTimeoutInterceptor_Timeout(t *testing.T) {
	timeout := NewTimeoutInterceptor(50 * time.Millisecond)
	node := NewNode("slow", nil, nil, nil)

	invoker := func() (map[string]any, error) {
		time.Sleep(200 * time.Millisecond)
		return map[string]any{"done": true}, nil
	}

	_, err := timeout.Around(nil, node, invoker)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestCircuitBreakerInterceptor_Closed_AllowsThrough(t *testing.T) {
	cb := NewCircuitBreakerInterceptor(3, time.Second)
	node := NewNode("test", nil, nil, nil)

	invoker := func() (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}

	result, err := cb.Around(nil, node, invoker)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["ok"] != true {
		t.Fatalf("unexpected result: %v", result)
	}
}

func TestCircuitBreakerInterceptor_OpensAfterThreshold(t *testing.T) {
	cb := NewCircuitBreakerInterceptor(3, time.Second)
	node := NewNode("test", nil, nil, nil)

	failInvoker := func() (map[string]any, error) {
		return nil, errors.New("fail")
	}

	// 连续失败 3 次触发熔断
	for i := 0; i < 3; i++ {
		cb.Around(nil, node, failInvoker)
	}

	// 熔断后，应该直接拒绝（不调用 invoker）
	called := false
	_, err := cb.Around(nil, node, func() (map[string]any, error) {
		called = true
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected circuit breaker error")
	}
	if called {
		t.Fatal("invoker should not be called when circuit is open")
	}
}

func TestCircuitBreakerInterceptor_Fallback(t *testing.T) {
	cb := NewCircuitBreakerInterceptor(1, time.Second)
	cb.FallbackFunc = func(ctx *flow.FlowContext, node Node) (map[string]any, error) {
		return map[string]any{"fallback": true}, nil
	}
	node := NewNode("test", nil, nil, nil)

	// 触发熔断
	cb.Around(nil, node, func() (map[string]any, error) {
		return nil, errors.New("fail")
	})

	// 下一次调用应走 fallback
	result, err := cb.Around(nil, node, func() (map[string]any, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected fallback result, got error: %v", err)
	}
	if result["fallback"] != true {
		t.Fatalf("expected fallback result, got %v", result)
	}
}

func TestCircuitBreakerInterceptor_HalfOpen_Recovery(t *testing.T) {
	cb := NewCircuitBreakerInterceptor(2, 50*time.Millisecond)
	cb.HalfOpenMaxCalls = 2
	node := NewNode("test", nil, nil, nil)

	failInvoker := func() (map[string]any, error) {
		return nil, errors.New("fail")
	}

	// 触发熔断
	cb.Around(nil, node, failInvoker)
	cb.Around(nil, node, failInvoker)

	// 等待超时进入半开
	time.Sleep(100 * time.Millisecond)

	// 半开状态下连续成功 2 次 → 恢复
	successInvoker := func() (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}

	for i := 0; i < 2; i++ {
		result, err := cb.Around(nil, node, successInvoker)
		if err != nil {
			t.Fatalf("half-open call %d failed: %v", i, err)
		}
		if result["ok"] != true {
			t.Fatalf("unexpected result: %v", result)
		}
	}

	// 熔断已恢复，正常调用应该通过
	result, err := cb.Around(nil, node, successInvoker)
	if err != nil {
		t.Fatalf("expected success after recovery: %v", err)
	}
	if result["ok"] != true {
		t.Fatalf("unexpected result: %v", result)
	}
}

func TestRecoveryInterceptor_Fallback(t *testing.T) {
	recovery := NewRecoveryInterceptor(func(ctx *flow.FlowContext, n Node, rec any) (map[string]any, error) {
		return map[string]any{"recovered": fmt.Sprintf("%v", rec)}, nil
	})
	node := NewNode("test", nil, nil, nil)

	invoker := func() (map[string]any, error) {
		panic("boom")
	}

	result, err := recovery.Around(nil, node, invoker)
	if err != nil {
		t.Fatalf("expected fallback result, got error: %v", err)
	}
	if result["recovered"] != "boom" {
		t.Fatalf("expected 'boom', got %v", result["recovered"])
	}
}

// ============================================================
// Interceptor 链式调用测试（洋葱模型）
// ============================================================

func TestInterceptorChain_OnionModel(t *testing.T) {
	var order []string

	outer := &testInterceptor{
		name: "outer",
		aroundFn: func(ctx *flow.FlowContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
			order = append(order, "outer-before")
			result, err := next()
			order = append(order, "outer-after")
			return result, err
		},
	}

	inner := &testInterceptor{
		name: "inner",
		aroundFn: func(ctx *flow.FlowContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
			order = append(order, "inner-before")
			result, err := next()
			order = append(order, "inner-after")
			return result, err
		},
	}

	// 模拟 Workflow.RunNode 中的调用链构建
	interceptors := []Interceptor{outer, inner}
	invoker := func() (map[string]any, error) {
		order = append(order, "actual-run")
		return map[string]any{"result": "ok"}, nil
	}

	for i := len(interceptors) - 1; i >= 0; i-- {
		ic := interceptors[i]
		next := invoker
		invoker = func(ic Interceptor, next func() (map[string]any, error)) func() (map[string]any, error) {
			return func() (map[string]any, error) {
				return ic.Around(nil, nil, next)
			}
		}(ic, next)
	}

	invoker()

	expected := []string{"outer-before", "inner-before", "actual-run", "inner-after", "outer-after"}
	if len(order) != len(expected) {
		t.Fatalf("expected %d steps, got %d: %v", len(expected), len(order), order)
	}
	for i, v := range expected {
		if order[i] != v {
			t.Fatalf("step %d: expected %q, got %q", i, v, order[i])
		}
	}
}

// testInterceptor 用于测试的拦截器
type testInterceptor struct {
	name     string
	aroundFn func(ctx *flow.FlowContext, node Node, next func() (map[string]any, error)) (map[string]any, error)
}

func (t *testInterceptor) Before(ctx *flow.FlowContext, node Node)           {}
func (t *testInterceptor) After(ctx *flow.FlowContext, node Node, err error) {}
func (t *testInterceptor) Around(ctx *flow.FlowContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
	return t.aroundFn(ctx, node, next)
}

// ============================================================
// Plan 测试
// ============================================================

func TestPlan_FindFailedTask(t *testing.T) {
	plan := NewPlan("test goal")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskSuccess},
		{ID: "task_2", State: TaskFailed, Error: "something went wrong"},
		{ID: "task_3", State: TaskPending},
	}

	failed := plan.FindFailedTask()
	if failed == nil {
		t.Fatal("expected to find failed task")
	}
	if failed.ID != "task_2" {
		t.Fatalf("expected task_2, got %s", failed.ID)
	}
}

func TestPlan_FindFailedTask_None(t *testing.T) {
	plan := NewPlan("test goal")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskSuccess},
		{ID: "task_2", State: TaskRunning},
	}

	if plan.FindFailedTask() != nil {
		t.Fatal("expected no failed task")
	}
}

func TestPlan_IsAllCompleted(t *testing.T) {
	plan := NewPlan("test goal")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskSuccess},
		{ID: "task_2", State: TaskSuccess},
	}

	if !plan.IsAllCompleted() {
		t.Fatal("expected all completed")
	}

	plan.Tasks[1].State = TaskRunning
	if plan.IsAllCompleted() {
		t.Fatal("expected not all completed")
	}
}

func TestPlan_IsAnyFailed(t *testing.T) {
	plan := NewPlan("test goal")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskSuccess},
	}

	if plan.IsAnyFailed() {
		t.Fatal("expected no failure")
	}

	plan.Tasks[0].State = TaskFailed
	if !plan.IsAnyFailed() {
		t.Fatal("expected failure detected")
	}
}

func TestPlan_Snapshot_ThreadSafe(t *testing.T) {
	plan := NewPlan("test goal")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskPending},
	}

	snapshot := plan.Snapshot()
	// 修改原始 plan 不影响快照
	plan.mu.Lock()
	plan.Tasks[0].State = TaskSuccess
	plan.mu.Unlock()

	if snapshot.Tasks[0].State != TaskPending {
		t.Fatalf("snapshot should be immutable, got %v", snapshot.Tasks[0].State)
	}
}

func TestPlan_StateChangeNotification(t *testing.T) {
	plan := NewPlan("test goal")

	// 在另一个 goroutine 等待状态变更
	done := make(chan struct{})
	go func() {
		plan.WaitForStateChange()
		close(done)
	}()

	// 短暂等待确保 goroutine 已进入等待
	time.Sleep(50 * time.Millisecond)

	// 触发状态变更
	plan.mu.Lock()
	plan.Tasks = append(plan.Tasks, Task{ID: "task_1", State: TaskRunning})
	plan.notifyStateChanged()
	plan.mu.Unlock()

	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Fatal("WaitForStateChange did not return after notification")
	}
}

func TestPlan_StateChannel(t *testing.T) {
	plan := NewPlan("test goal")
	ch := plan.GetStateChannel()

	// 触发通知
	plan.mu.Lock()
	plan.notifyStateChanged()
	plan.mu.Unlock()

	select {
	case <-ch:
		// OK
	case <-time.After(time.Second):
		t.Fatal("stateChanged channel not received notification")
	}
}

// ============================================================
// TrimJSONWrapper 测试
// ============================================================

func TestTrimJSONWrapper_PlainJSON(t *testing.T) {
	input := `{"tasks": []}`
	result := TrimJSONWrapper(input)
	if result != input {
		t.Fatalf("expected %q, got %q", input, result)
	}
}

func TestTrimJSONWrapper_MarkdownCodeBlock(t *testing.T) {
	input := "```json\n{\"tasks\": []}\n```"
	result := TrimJSONWrapper(input)
	expected := `{"tasks": []}`
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestTrimJSONWrapper_WithPrefixText(t *testing.T) {
	input := "Here is the plan:\n```json\n{\"tasks\": []}\n```"
	result := TrimJSONWrapper(input)
	expected := `{"tasks": []}`
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestTrimJSONWrapper_EmptyString(t *testing.T) {
	result := TrimJSONWrapper("")
	if result != "" {
		t.Fatalf("expected empty string, got %q", result)
	}
}

func TestTrimJSONWrapper_ArrayJSON(t *testing.T) {
	input := "Result: `[1, 2, 3]`"
	result := TrimJSONWrapper(input)
	expected := `[1, 2, 3]`
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

// ============================================================
// Plan 正确使用示例测试
// ============================================================

func TestPlan_CorrectCondUsage(t *testing.T) {
	plan := NewPlan("test")

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		plan.WaitForStateChange()
	}()

	time.Sleep(50 * time.Millisecond)

	plan.mu.Lock()
	plan.Tasks = append(plan.Tasks, Task{ID: "task_1", State: TaskRunning})
	plan.notifyStateChanged()
	plan.mu.Unlock()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Log("Plan correctly uses same mutex for cond and mu")
	case <-time.After(time.Second):
		t.Fatal("WaitForStateChange did not return")
	}
}

// ============================================================
// 演示问题的测试：TaskNode 失败取消整个 Workflow
// ============================================================

func TestWorkflow_TaskFailureCancelsContext_Demonstration(t *testing.T) {
	// 这个测试演示了问题 2：
	// 当一个 task node 失败时，RunNode 调用 ctx.Cancel(err)
	// 这会取消整个工作流上下文，导致 schedule loop 也退出

	// 创建一个简单的 workflow 来演示
	// （这里我们直接测试 FlowContext 的行为来演示问题）

	ctx := flow.NewFlowContext(context.Background())

	// 模拟 task_1 失败
	taskErr := errors.New("task_1 failed")
	ctx.Cancel(taskErr)

	// 模拟 schedule loop 尝试等待
	select {
	case <-ctx.Done():
		// schedule loop 会走到这里，感知到取消
		err := ctx.Err()
		if err == nil {
			t.Fatal("expected error")
		}
		t.Logf("Schedule loop sees context cancelled: %v", err)
		t.Log("This means RePlan can never be triggered — the design has a fundamental conflict")
	default:
		t.Fatal("context should be cancelled")
	}
}
