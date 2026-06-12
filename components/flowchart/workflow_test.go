package flowchart

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/flow"
	"github.com/Luo-root/pulse/components/flowchart/node"
)

// ============================================================================
// 基础 Workflow 测试
// ============================================================================

func TestWorkflow_BasicRun(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	// 单节点：输入 "a", "b"，输出 "sum"
	n := node.NewNode("add", []string{"a", "b"}, []string{"sum"},
		func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			a := inputs["a"].(int)
			b := inputs["b"].(int)
			return map[string]any{"sum": a + b}, nil
		},
	)

	if err := wf.AddNode(n); err != nil {
		t.Fatal(err)
	}

	err = wf.Run(map[string]any{"a": 3, "b": 7})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	val, err := wf.Get("sum")
	if err != nil {
		t.Fatal(err)
	}
	if val.(int) != 10 {
		t.Fatalf("expected 10, got %v", val)
	}
}

func TestWorkflow_DataDrivenChain(t *testing.T) {
	// A → B → C 链式依赖，全部同时提交，数据驱动执行顺序
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	var order []string
	var mu sync.Mutex
	record := func(id string) {
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
	}

	nA := node.NewNode("A", nil, []string{"x"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		record("A")
		return map[string]any{"x": 1}, nil
	})

	nB := node.NewNode("B", []string{"x"}, []string{"y"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		record("B")
		x := inputs["x"].(int)
		return map[string]any{"y": x * 10}, nil
	})

	nC := node.NewNode("C", []string{"y"}, []string{"z"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		record("C")
		y := inputs["y"].(int)
		return map[string]any{"z": y + 100}, nil
	})

	wf.AddNode(nA)
	wf.AddNode(nB)
	wf.AddNode(nC)

	err = wf.Run(nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// 验证执行顺序 A → B → C
	mu.Lock()
	if len(order) != 3 {
		t.Fatalf("expected 3 executions, got %d: %v", len(order), order)
	}
	if order[0] != "A" || order[1] != "B" || order[2] != "C" {
		t.Fatalf("expected order [A B C], got %v", order)
	}
	mu.Unlock()

	// 验证最终值
	val, _ := wf.Get("z")
	if val.(int) != 110 {
		t.Fatalf("expected 110, got %v", val)
	}
}

func TestWorkflow_ParallelExecution(t *testing.T) {
	// 两个独立节点并行执行，无依赖关系
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	var started sync.WaitGroup
	started.Add(2)
	barrier := make(chan struct{})

	n1 := node.NewNode("P1", nil, []string{"r1"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		started.Done()
		<-barrier // 等另一个节点也启动
		return map[string]any{"r1": "done1"}, nil
	})

	n2 := node.NewNode("P2", nil, []string{"r2"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		started.Done()
		<-barrier
		return map[string]any{"r2": "done2"}, nil
	})

	wf.AddNode(n1)
	wf.AddNode(n2)

	done := make(chan error, 1)
	go func() {
		done <- wf.Run(nil)
	}()

	// 等两个节点都启动了才放行 barrier，证明是并行的
	waitCh := make(chan struct{})
	go func() {
		started.Wait()
		close(barrier)
		close(waitCh)
	}()

	select {
	case <-waitCh:
		// 两个节点都启动了，确认并行
	case <-time.After(2 * time.Second):
		t.Fatal("nodes did not start in parallel")
	}

	if err := <-done; err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	v1, _ := wf.Get("r1")
	v2, _ := wf.Get("r2")
	if v1.(string) != "done1" || v2.(string) != "done2" {
		t.Fatalf("unexpected results: r1=%v, r2=%v", v1, v2)
	}
}

func TestWorkflow_DiamondDependency(t *testing.T) {
	// 菱形依赖: root → (left, right) → merge
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	root := node.NewNode("root", nil, []string{"data"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"data": 42}, nil
	})

	left := node.NewNode("left", []string{"data"}, []string{"left_result"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		d := inputs["data"].(int)
		return map[string]any{"left_result": d + 1}, nil
	})

	right := node.NewNode("right", []string{"data"}, []string{"right_result"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		d := inputs["data"].(int)
		return map[string]any{"right_result": d * 2}, nil
	})

	merge := node.NewNode("merge", []string{"left_result", "right_result"}, []string{"final"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		l := inputs["left_result"].(int)
		r := inputs["right_result"].(int)
		return map[string]any{"final": l + r}, nil
	})

	wf.AddNode(root)
	wf.AddNode(left)
	wf.AddNode(right)
	wf.AddNode(merge)

	if err := wf.Run(nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	val, _ := wf.Get("final")
	// left: 42+1=43, right: 42*2=84, merge: 43+84=127
	if val.(int) != 127 {
		t.Fatalf("expected 127, got %v", val)
	}
}

func TestWorkflow_MultipleInputs(t *testing.T) {
	// 多输入汇聚
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	n1 := node.NewNode("source_a", nil, []string{"a"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"a": 10}, nil
	})
	n2 := node.NewNode("source_b", nil, []string{"b"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"b": 20}, nil
	})
	n3 := node.NewNode("source_c", nil, []string{"c"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"c": 30}, nil
	})

	sumer := node.NewNode("sum", []string{"a", "b", "c"}, []string{"total"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		total := inputs["a"].(int) + inputs["b"].(int) + inputs["c"].(int)
		return map[string]any{"total": total}, nil
	})

	wf.AddNode(n1)
	wf.AddNode(n2)
	wf.AddNode(n3)
	wf.AddNode(sumer)

	if err := wf.Run(nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	val, _ := wf.Get("total")
	if val.(int) != 60 {
		t.Fatalf("expected 60, got %v", val)
	}
}

// ============================================================================
// 错误处理测试
// ============================================================================

func TestWorkflow_NodeErrorCancelsWorkflow(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	expectedErr := errors.New("node failed")

	// 节点 A 输出 "x"，节点 B 依赖 "x" 但会失败
	nA := node.NewNode("A", nil, []string{"x"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"x": 1}, nil
	})

	nB := node.NewNode("B", []string{"x"}, []string{"y"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return nil, expectedErr
	})

	// C 也依赖 "x"，应该被取消或能正常执行（因为 x 已经就绪）
	nC := node.NewNode("C", []string{"x"}, []string{"z"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"z": 99}, nil
	})

	wf.AddNode(nA)
	wf.AddNode(nB)
	wf.AddNode(nC)

	runErr := wf.Run(nil)
	if runErr == nil {
		t.Fatal("expected error from workflow, got nil")
	}
}

func TestWorkflow_NodePanicRecovered(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	recovery := node.NewRecoveryInterceptor(func(ctx *flow.FlowContext, n node.Node, recoverVal any) (map[string]any, error) {
		return map[string]any{"result": true}, nil
	})
	wf.AddAspect(recovery)

	panicNode := node.NewNode("panicker", nil, []string{"result"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		panic("boom")
	})

	wf.AddNode(panicNode)

	runErr := wf.Run(nil)
	if runErr != nil {
		t.Fatalf("expected recovery, got error: %v", runErr)
	}

	val, _ := wf.Get("result")
	if val == nil {
		t.Fatal("expected recovered result")
	}
}

// ============================================================================
// Aspect 测试
// ============================================================================

func TestWorkflow_BeforeAfterAspect(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	var events []string
	var mu sync.Mutex
	record := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}

	// 全局切面
	wf.AddAspect(&node.AroundAspect{
		BeforeFn: func(ctx *flow.FlowContext, n node.Node) {
			record("global_before:" + n.ID())
		},
		AfterFn: func(ctx *flow.FlowContext, n node.Node, err error) {
			record("global_after:" + n.ID())
		},
	})

	n1 := node.NewNode("task1", nil, []string{"x"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		record("run:task1")
		return map[string]any{"x": 1}, nil
	})

	// 节点级切面
	n1.AddAspect(&node.AroundAspect{
		BeforeFn: func(ctx *flow.FlowContext, n node.Node) {
			record("node_before:" + n.ID())
		},
		AfterFn: func(ctx *flow.FlowContext, n node.Node, err error) {
			record("node_after:" + n.ID())
		},
	})

	wf.AddNode(n1)

	if err := wf.Run(nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Before 应该在 Run 之前，After 应该在 Run 之后
	// 全局切面先于节点切面
	expected := []string{
		"global_before:task1",
		"node_before:task1",
		"run:task1",
		"node_after:task1",
		"global_after:task1",
	}

	if len(events) != len(expected) {
		t.Fatalf("expected %d events, got %d: %v", len(expected), len(events), events)
	}
	for i, e := range expected {
		if events[i] != e {
			t.Fatalf("event[%d]: expected %q, got %q", i, e, events[i])
		}
	}
}

func TestWorkflow_AspectOnError(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	var gotErr atomic.Value

	wf.AddAspect(&node.AfterAspect{
		Fn: func(ctx *flow.FlowContext, n node.Node, err error) {
			if err != nil {
				gotErr.Store(err.Error())
			}
		},
	})

	n := node.NewNode("fail", nil, nil, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return nil, errors.New("node failed")
	})

	wf.AddNode(n)
	wf.Run(nil)

	if gotErr.Load() == nil || gotErr.Load().(string) != "node failed" {
		t.Fatalf("expected aspect to capture error, got %v", gotErr.Load())
	}
}

func TestWorkflow_GlobalAndNodeAspectOrder(t *testing.T) {
	// 验证全局切面和节点切面都执行，且全局在前、节点在后
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	var mu sync.Mutex
	var order []string

	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}

	wf.AddAspect(&node.BeforeAspect{
		Fn: func(ctx *flow.FlowContext, n node.Node) {
			record("global:" + n.ID())
		},
	})

	n := node.NewNode("mytask", nil, nil, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		record("exec")
		return nil, nil
	})

	n.AddAspect(&node.BeforeAspect{
		Fn: func(ctx *flow.FlowContext, n node.Node) {
			record("local:" + n.ID())
		},
	})

	wf.AddNode(n)
	wf.Run(nil)

	mu.Lock()
	defer mu.Unlock()

	if len(order) != 3 {
		t.Fatalf("expected 3 events, got %d: %v", len(order), order)
	}
	if order[0] != "global:mytask" || order[1] != "local:mytask" || order[2] != "exec" {
		t.Fatalf("expected [global:mytask local:mytask exec], got %v", order)
	}
}

// ============================================================================
// Interceptor 测试
// ============================================================================

func TestWorkflow_NodeError_FailFast(t *testing.T) {
	// 无拦截器 → ctx.Cancel → 工作流 fail-fast
	wf, err := NewWorkflow(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	wf.AddNode(node.NewNode("fail", nil, []string{"out"},
		func(_ *flow.FlowContext, _ map[string]any) (map[string]any, error) {
			return nil, errors.New("boom")
		}))
	if err := wf.Run(nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestWorkflow_ErrorSwallow_Continues(t *testing.T) {
	// ErrorSwallowInterceptor 吞掉错误 → 下游正常运行
	wf, err := NewWorkflow(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}

	wf.AddAspect(node.NewErrorSwallowInterceptor(
		func(_ *flow.FlowContext, _ node.Node, _ error) (map[string]any, error) {
			return map[string]any{"x": 0}, nil // 降级输出
		},
	))

	wf.AddNode(node.NewNode("a", nil, []string{"x"},
		func(_ *flow.FlowContext, _ map[string]any) (map[string]any, error) {
			return nil, errors.New("a failed")
		}))
	wf.AddNode(node.NewNode("b", []string{"x"}, []string{"y"},
		func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			return map[string]any{"y": in["x"].(int) + 1}, nil
		}))

	if err := wf.Run(nil); err != nil {
		t.Fatalf("Run should succeed with ErrorSwallow: %v", err)
	}
	val, _ := wf.Get("y")
	if val != 1 {
		t.Fatalf("want 1, got %v", val)
	}
}

func TestWorkflow_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wf, _ := NewWorkflow(ctx, 10)
	defer wf.Close()

	wf.AddNode(node.NewNode("waiter", []string{"trigger"}, nil,
		func(_ *flow.FlowContext, _ map[string]any) (map[string]any, error) {
			return nil, nil
		}))
	cancel()
	if err := wf.Run(nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestWorkflow_RetryInterceptor(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	var attempts atomic.Int32

	retry := node.NewRetryInterceptor(3, 10*time.Millisecond)
	wf.AddAspect(retry)

	n := node.NewNode("flaky", nil, []string{"result"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		count := attempts.Add(1)
		if count < 3 {
			return nil, errors.New("temporary failure")
		}
		return map[string]any{"result": "success"}, nil
	})

	wf.AddNode(n)

	if err := wf.Run(nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}

	val, _ := wf.Get("result")
	if val.(string) != "success" {
		t.Fatalf("expected 'success', got %v", val)
	}
}

func TestWorkflow_RetryInterceptor_Exhausted(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	var attempts atomic.Int32

	retry := node.NewRetryInterceptor(3, 5*time.Millisecond)
	wf.AddAspect(retry)

	n := node.NewNode("always_fail", nil, nil, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		attempts.Add(1)
		return nil, errors.New("permanent failure")
	})

	wf.AddNode(n)

	runErr := wf.Run(nil)
	if runErr == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestWorkflow_TimeoutInterceptor(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	timeout := node.NewTimeoutInterceptor(100 * time.Millisecond)
	wf.AddAspect(timeout)

	n := node.NewNode("slow", nil, []string{"result"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		time.Sleep(1 * time.Second)
		return map[string]any{"result": "done"}, nil
	})

	wf.AddNode(n)

	runErr := wf.Run(nil)
	if runErr == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWorkflow_CircuitBreakerInterceptor(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	var execCount atomic.Int32

	cb := node.NewCircuitBreakerInterceptor(2, 5*time.Second)
	cb.FallbackFunc = func(ctx *flow.FlowContext, n node.Node) (map[string]any, error) {
		return map[string]any{"result": "cb_fallback"}, nil
	}

	// 用 ErrorSwallowInterceptor 包裹 CircuitBreaker，吞掉 error
	errorSwallow := node.NewErrorSwallowInterceptor(func(ctx *flow.FlowContext, n node.Node, err error) (map[string]any, error) {
		// 调用 CircuitBreaker 的 Fallback
		return cb.FallbackFunc(ctx, n)
	})

	wf.AddAspect(errorSwallow)
	wf.AddAspect(cb)

	n := node.NewNode("breaker_test", nil, []string{"result"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		execCount.Add(1)
		return nil, errors.New("fail")
	})

	wf.AddNode(n)

	err = wf.Run(nil)
	if err != nil {
		t.Fatalf("workflow should not fail: %v", err)
	}

	count := execCount.Load()
	t.Logf("Execution count: %d", count)

	val, _ := wf.Get("result")
	t.Logf("Final result: %v", val)
}

func TestWorkflow_TimeoutThenRetry(t *testing.T) {
	// 超时 + 重试组合：超时触发，重试再试
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	var attempts atomic.Int32

	// 拦截器链：先重试（外层），再超时（内层）
	retry := node.NewRetryInterceptor(2, 10*time.Millisecond)
	timeout := node.NewTimeoutInterceptor(50 * time.Millisecond)
	wf.AddAspect(retry)
	wf.AddAspect(timeout)

	n := node.NewNode("combo", nil, []string{"result"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		count := attempts.Add(1)
		if count == 1 {
			time.Sleep(200 * time.Millisecond) // 第一次超时
		}
		return map[string]any{"result": "ok"}, nil
	})

	wf.AddNode(n)

	if err := wf.Run(nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}
}

// ============================================================================
// 拦截器组合：ErrorSwallow + CB + Retry + Recovery
// ============================================================================

func TestWorkflow_AllInterceptorsCombined(t *testing.T) {
	// 最外层: ErrorSwallow — 最终安全网
	// 次外层: CB — 熔断降级
	// 内层:   Retry — 重试
	// 最内层: Recovery — panic 恢复
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	var execCount atomic.Int32

	errorSwallow := node.NewErrorSwallowInterceptor(
		func(_ *flow.FlowContext, _ node.Node, err error) (map[string]any, error) {
			return map[string]any{"result": "final_fallback"}, nil
		})

	n := node.NewNode("combo", nil, []string{"result"},
		func(_ *flow.FlowContext, _ map[string]any) (map[string]any, error) {
			execCount.Add(1)
			return nil, errors.New("always fail")
		})

	n.AddAspect(errorSwallow)                                      // 最外
	n.AddAspect(node.NewCircuitBreakerInterceptor(3, time.Second)) // 次外
	n.AddAspect(node.NewRetryInterceptor(2, 10*time.Millisecond))  // 内
	n.AddAspect(node.NewRecoveryInterceptor(nil))                  // 最内

	wf.AddNode(n)

	if err := wf.Run(nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	val, _ := wf.Get("result")
	if val != "final_fallback" {
		t.Fatalf("want final_fallback, got %v", val)
	}
}

// ============================================================================
// 生命周期测试
// ============================================================================

func TestWorkflow_NoNodesError(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	runErr := wf.Run(nil)
	if runErr == nil {
		t.Fatal("expected error for workflow with no nodes")
	}
}

func TestWorkflow_DoubleRunRejected(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	block := make(chan struct{})

	n := node.NewNode("blocker", nil, nil, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		<-block
		return nil, nil
	})
	wf.AddNode(n)

	go wf.Run(nil)
	time.Sleep(50 * time.Millisecond) // 等 Run 启动

	// 第二次 Run 应该被拒绝
	runErr := wf.Run(nil)
	if runErr != flow.ErrWorkflowRunning {
		t.Fatalf("expected ErrWorkflowRunning, got %v", runErr)
	}

	close(block)
}

func TestWorkflow_CloseRejectsAddNode(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}

	wf.Close()

	n := node.NewNode("late", nil, nil, nil)
	if err := wf.AddNode(n); err != flow.ErrWorkflowClosed {
		t.Fatalf("expected ErrWorkflowClosed, got %v", err)
	}
}

func TestWorkflow_CloseRejectsRun(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}

	wf.Close()

	runErr := wf.Run(nil)
	if runErr != flow.ErrWorkflowClosed {
		t.Fatalf("expected ErrWorkflowClosed, got %v", runErr)
	}
}

func TestWorkflow_Reset(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	n := node.NewNode("inc", []string{"x"}, []string{"y"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"y": inputs["x"].(int) + 1}, nil
	})
	wf.AddNode(n)

	// 第一次 Run
	wf.Run(map[string]any{"x": 1})
	v1, _ := wf.Get("y")
	if v1.(int) != 2 {
		t.Fatalf("first run: expected 2, got %v", v1)
	}

	// Reset 后用新的 context 再次 Run
	wf.Reset(context.Background())
	wf.Run(map[string]any{"x": 10})
	v2, _ := wf.Get("y")
	if v2.(int) != 11 {
		t.Fatalf("second run: expected 11, got %v", v2)
	}
}

func TestWorkflow_GetStats(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	wf.AddNode(node.NewNode("a", nil, nil, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return nil, nil
	}))
	wf.AddNode(node.NewNode("b", nil, nil, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return nil, nil
	}))

	stats := wf.GetStats()
	if stats["total_nodes"].(int) != 2 {
		t.Fatalf("expected 2 nodes, got %v", stats["total_nodes"])
	}
	if stats["is_running"].(bool) != false {
		t.Fatal("expected not running")
	}
	if stats["max_workers"].(int) != 2 {
		t.Fatalf("expected 2 workers, got %v", stats["max_workers"])
	}
}

func TestWorkflow_ResizePool(t *testing.T) {
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	if err := wf.ResizePool(8); err != nil {
		t.Fatal(err)
	}

	stats := wf.GetStats()
	if stats["max_workers"].(int) != 8 {
		t.Fatalf("expected 8 workers after resize, got %v", stats["max_workers"])
	}
}

func TestWorkflow_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wf, err := NewWorkflow(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	started := make(chan struct{})
	n := node.NewNode("waiter", nil, []string{"x"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		close(started)
		<-ctx.Done() // 等待取消
		return nil, ctx.Err()
	})
	wf.AddNode(n)

	go func() {
		<-started
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	runErr := wf.Run(nil)
	if runErr == nil {
		t.Fatal("expected cancellation error")
	}
}

// ============================================================================
// 综合集成测试
// ============================================================================

func TestWorkflow_FullIntegration(t *testing.T) {
	// 场景：带切面 + 重试 + 数据链 + 并行 的完整工作流
	wf, err := NewWorkflow(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	// 追踪执行顺序
	var mu sync.Mutex
	var executionLog []string
	logEvent := func(e string) {
		mu.Lock()
		executionLog = append(executionLog, e)
		mu.Unlock()
	}

	// 全局切面：记录每个节点的生命周期
	wf.AddAspect(&node.AroundAspect{
		BeforeFn: func(ctx *flow.FlowContext, n node.Node) {
			logEvent("before:" + n.ID())
		},
		AfterFn: func(ctx *flow.FlowContext, n node.Node, err error) {
			if err != nil {
				logEvent("after_error:" + n.ID())
			} else {
				logEvent("after_ok:" + n.ID())
			}
		},
	})

	// 节点 1：产生初始数据
	source := node.NewNode("source", nil, []string{"raw_data"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		logEvent("exec:source")
		return map[string]any{"raw_data": "hello world"}, nil
	})

	// 节点 2 & 3：并行处理
	upper := node.NewNode("upper", []string{"raw_data"}, []string{"upper_data"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		logEvent("exec:upper")
		raw := inputs["raw_data"].(string)
		return map[string]any{"upper_data": raw + "_UPPER"}, nil
	})

	length := node.NewNode("length", []string{"raw_data"}, []string{"data_len"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		logEvent("exec:length")
		raw := inputs["raw_data"].(string)
		return map[string]any{"data_len": len(raw)}, nil
	})

	// 节点 4：汇聚结果
	merge := node.NewNode("merge", []string{"upper_data", "data_len"}, []string{"final"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		logEvent("exec:merge")
		u := inputs["upper_data"].(string)
		l := inputs["data_len"].(int)
		return map[string]any{"final": u + "_len" + string(rune('0'+l))}, nil
	})

	wf.AddNode(source)
	wf.AddNode(upper)
	wf.AddNode(length)
	wf.AddNode(merge)

	if err := wf.Run(nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// 验证结果
	val, _ := wf.Get("final")
	if val == nil {
		t.Fatal("expected final result")
	}

	// 验证切面触发了
	mu.Lock()
	defer mu.Unlock()

	// source 应该最先执行
	sourceIdx := -1
	for i, e := range executionLog {
		if e == "exec:source" {
			sourceIdx = i
			break
		}
	}
	if sourceIdx < 0 {
		t.Fatal("source node did not execute")
	}

	// merge 应该最后执行
	mergeIdx := -1
	for i, e := range executionLog {
		if e == "exec:merge" {
			mergeIdx = i
			break
		}
	}
	if mergeIdx < 0 {
		t.Fatal("merge node did not execute")
	}
	if mergeIdx <= sourceIdx {
		t.Fatal("merge should execute after source")
	}

	// 验证 before/after 切面对每个节点都触发了
	for _, id := range []string{"source", "upper", "length", "merge"} {
		hasBefore := false
		hasAfter := false
		for _, e := range executionLog {
			if e == "before:"+id {
				hasBefore = true
			}
			if e == "after_ok:"+id {
				hasAfter = true
			}
		}
		if !hasBefore {
			t.Errorf("missing before aspect for %s", id)
		}
		if !hasAfter {
			t.Errorf("missing after aspect for %s", id)
		}
	}
}

func TestWorkflow_TopologicalNodeAsOneNode(t *testing.T) {
	// TopologicalNode 作为 Workflow 中的一个节点
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	// 内部子节点
	n1 := node.NewNode("step1", []string{"input"}, []string{"mid"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"mid": inputs["input"].(int) * 2}, nil
	})
	n2 := node.NewNode("step2", []string{"mid"}, []string{"output"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"output": inputs["mid"].(int) + 10}, nil
	})

	topoNode, err := node.NewTopologicalNode("pipeline", []node.Node{n1, n2}, []string{"output"})
	if err != nil {
		t.Fatal(err)
	}

	wf.AddNode(topoNode)

	if err := wf.Run(map[string]any{"input": 5}); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	val, _ := wf.Get("output")
	// step1: 5*2=10, step2: 10+10=20
	if val.(int) != 20 {
		t.Fatalf("expected 20, got %v", val)
	}
}

func TestWorkflow_InputKeyAsInitialData(t *testing.T) {
	// 验证通过 Input() 预设的数据可以被节点 WaitAll 获取
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	wf.Input("preset", 999)

	n := node.NewNode("reader", []string{"preset"}, []string{"read"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"read": inputs["preset"].(int)}, nil
	})
	wf.AddNode(n)

	if err := wf.Run(nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	val, _ := wf.Get("read")
	if val.(int) != 999 {
		t.Fatalf("expected 999, got %v", val)
	}
}

func TestWorkflow_StaticSort(t *testing.T) {
	// 验证节点列表顺序不影响执行（数据驱动，不依赖注册顺序）
	wf, err := NewWorkflow(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()

	var order []string
	var mu sync.Mutex

	nC := node.NewNode("C", []string{"b_val"}, []string{"c_val"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		mu.Lock()
		order = append(order, "C")
		mu.Unlock()
		return map[string]any{"c_val": 3}, nil
	})
	nA := node.NewNode("A", nil, []string{"a_val"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		mu.Lock()
		order = append(order, "A")
		mu.Unlock()
		return map[string]any{"a_val": 1}, nil
	})
	nB := node.NewNode("B", []string{"a_val"}, []string{"b_val"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		mu.Lock()
		order = append(order, "B")
		mu.Unlock()
		return map[string]any{"b_val": 2}, nil
	})

	// 故意逆序添加：C, A, B
	wf.AddNode(nC)
	wf.AddNode(nA)
	wf.AddNode(nB)

	if err := wf.Run(nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// 数据驱动：A 必须在 B 前，B 必须在 C 前
	if len(order) != 3 {
		t.Fatalf("expected 3 nodes, got %d: %v", len(order), order)
	}

	aIdx := sort.SearchStrings(order, "A")
	_ = aIdx
	// 线性查找索引
	find := func(s string) int {
		for i, v := range order {
			if v == s {
				return i
			}
		}
		return -1
	}

	if find("A") > find("B") {
		t.Fatalf("A should execute before B, got %v", order)
	}
	if find("B") > find("C") {
		t.Fatalf("B should execute before C, got %v", order)
	}
}
