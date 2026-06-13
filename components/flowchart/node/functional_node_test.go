package node

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel/mock"
	"github.com/Luo-root/pulse/components/flow"
	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================
// ConditionNode 测试
// ============================================================

func TestConditionNode_TrueBranch(t *testing.T) {
	node := NewConditionNode(
		"cond",
		"value",
		func(v any) bool {
			num, ok := v.(int)
			return ok && num > 10
		},
		"is_greater",
		"is_not_greater",
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"value": 42}

	result, err := node.Run(ctx, inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result["is_greater"] != true {
		t.Errorf("expected is_greater=true, got %v", result["is_greater"])
	}
	if result["is_not_greater"] != false {
		t.Errorf("expected is_not_greater=false, got %v", result["is_not_greater"])
	}
}

func TestConditionNode_FalseBranch(t *testing.T) {
	node := NewConditionNode(
		"cond",
		"value",
		func(v any) bool {
			num, ok := v.(int)
			return ok && num > 10
		},
		"is_greater",
		"is_not_greater",
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"value": 5}

	result, err := node.Run(ctx, inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result["is_greater"] != false {
		t.Errorf("expected is_greater=false, got %v", result["is_greater"])
	}
	if result["is_not_greater"] != true {
		t.Errorf("expected is_not_greater=true, got %v", result["is_not_greater"])
	}
}

func TestConditionNode_StringCondition(t *testing.T) {
	node := NewConditionNode(
		"check_status",
		"status",
		func(v any) bool {
			s, ok := v.(string)
			return ok && s == "active"
		},
		"is_active",
		"is_inactive",
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"status": "active"}

	result, err := node.Run(ctx, inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result["is_active"] != true {
		t.Errorf("expected is_active=true")
	}
	if result["is_inactive"] != false {
		t.Errorf("expected is_inactive=false")
	}
}

// ============================================================
// LoopNode 测试
// ============================================================

func TestLoopNode_CompletedNormally(t *testing.T) {
	counter := 0
	maxCount := 3

	node := NewLoopNode(
		"loop",
		"start",
		func(ctx *flow.FlowContext) bool {
			return counter < maxCount
		},
		func(ctx *flow.FlowContext) {
			counter++
		},
		"result",
		nil,
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"start": true}

	result, err := node.Run(ctx, inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if counter != maxCount {
		t.Errorf("expected counter=%d, got %d", maxCount, counter)
	}

	loopResult, ok := result["result"].(*flow.LoopResult)
	if !ok {
		t.Fatalf("expected *flow.LoopResult, got %T", result["result"])
	}
	if loopResult.Iterations != maxCount {
		t.Errorf("expected iterations=%d, got %d", maxCount, loopResult.Iterations)
	}
	if loopResult.Status != flow.LoopStatusCompleted {
		t.Errorf("expected status=completed, got %s", loopResult.Status)
	}
}

func TestLoopNode_MaxIterationsReached(t *testing.T) {
	counter := 0
	maxIter := 5

	node := NewLoopNode(
		"loop",
		"start",
		func(ctx *flow.FlowContext) bool {
			return true
		},
		func(ctx *flow.FlowContext) {
			counter++
		},
		"result",
		&LoopConfig{MaxIterations: maxIter},
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"start": true}

	result, err := node.Run(ctx, inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if counter != maxIter {
		t.Errorf("expected counter=%d, got %d", maxIter, counter)
	}

	loopResult, ok := result["result"].(*flow.LoopResult)
	if !ok {
		t.Fatalf("expected *flow.LoopResult, got %T", result["result"])
	}
	if loopResult.Status != flow.LoopStatusMaxIterations {
		t.Errorf("expected status=max_iterations, got %s", loopResult.Status)
	}
}

func TestLoopNode_ConditionExit(t *testing.T) {
	counter := 0

	node := NewLoopNode(
		"loop",
		"start",
		func(ctx *flow.FlowContext) bool {
			counter++
			return counter <= 3
		},
		func(ctx *flow.FlowContext) {
		},
		"result",
		nil,
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"start": true}

	result, err := node.Run(ctx, inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if counter != 4 {
		t.Errorf("expected counter=4, got %d", counter)
	}

	loopResult, ok := result["result"].(*flow.LoopResult)
	if !ok {
		t.Fatalf("expected *flow.LoopResult, got %T", result["result"])
	}
	if loopResult.Iterations != 3 {
		t.Errorf("expected iterations=3, got %d", loopResult.Iterations)
	}
	if loopResult.Status != flow.LoopStatusCompleted {
		t.Errorf("expected status=completed, got %s", loopResult.Status)
	}
}

func TestLoopNode_Timeout(t *testing.T) {
	counter := 0

	node := NewLoopNode(
		"loop",
		"start",
		func(ctx *flow.FlowContext) bool {
			return true // 永远为真，依赖超时限制
		},
		func(ctx *flow.FlowContext) {
			counter++
			time.Sleep(50 * time.Millisecond) // 每次循环睡 50ms
		},
		"result",
		&LoopConfig{Timeout: 150 * time.Millisecond}, // 150ms 超时
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"start": true}

	_, err := node.Run(ctx, inputs)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, flow.ErrLoopTimeout) {
		t.Errorf("expected ErrLoopTimeout, got %v", err)
	}

	// 应该执行了约 3 次（150ms / 50ms）
	if counter < 2 || counter > 4 {
		t.Errorf("expected 2-4 iterations, got %d", counter)
	}
}

func TestLoopNode_ContextCancellation(t *testing.T) {
	counter := 0
	ctx, cancel := context.WithCancel(context.Background())

	node := NewLoopNode(
		"loop",
		"start",
		func(ctx *flow.FlowContext) bool {
			return true
		},
		func(ctx *flow.FlowContext) {
			counter++
			time.Sleep(20 * time.Millisecond)
		},
		"result",
		&LoopConfig{Context: ctx},
	)

	// 在另一个 goroutine 中取消
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()

	flowCtx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"start": true}

	_, err := node.Run(flowCtx, inputs)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, flow.ErrLoopCancelled) {
		t.Errorf("expected ErrLoopCancelled, got %v", err)
	}
}

// ============================================================
// ParallelNode 测试
// ============================================================

func TestParallelNode_MergeInputs(t *testing.T) {
	node := NewParallelNode(
		"parallel",
		[]string{"input1", "input2", "input3"},
		"merged",
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{
		"input1": "value1",
		"input2": 42,
		"input3": []string{"a", "b"},
	}

	result, err := node.Run(ctx, inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	merged, ok := result["merged"].(map[string]any)
	if !ok {
		t.Fatalf("expected merged to be map[string]any, got %T", result["merged"])
	}

	if merged["input1"] != "value1" {
		t.Errorf("expected input1=value1, got %v", merged["input1"])
	}
	if merged["input2"] != 42 {
		t.Errorf("expected input2=42, got %v", merged["input2"])
	}
	if merged["__parallel_complete"] != true {
		t.Errorf("expected __parallel_complete=true")
	}
}

func TestParallelNode_EmptyInputs(t *testing.T) {
	node := NewParallelNode(
		"parallel",
		[]string{},
		"merged",
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{}

	result, err := node.Run(ctx, inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	merged, ok := result["merged"].(map[string]any)
	if !ok {
		t.Fatalf("expected merged to be map[string]any")
	}

	if merged["__parallel_complete"] != true {
		t.Errorf("expected __parallel_complete=true")
	}
}

// ============================================================
// LLMStreamNode 测试
// ============================================================

func TestLLMStreamNode_BasicStreaming(t *testing.T) {
	mockModel := mock.NewMockModelWithResponses(
		mock.MockTextResponse("Hello World"),
	)

	node := NewLLMStreamNode(
		"llm_stream",
		"prompt",
		"stream_readers",
		mockModel,
		2, // 创建 2 个副本
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"prompt": "Say hello"}

	result, err := node.Run(ctx, inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	readers, ok := result["stream_readers"].([]*schema.StreamReader)
	if !ok {
		t.Fatalf("expected []*StreamReader, got %T", result["stream_readers"])
	}

	if len(readers) != 2 {
		t.Fatalf("expected 2 readers, got %d", len(readers))
	}

	// 验证第一个 reader 能读取内容
	reader := readers[0]
	var content string
	for {
		msg, err := reader.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read error: %v", err)
		}
		content += msg.Content
	}

	if content != "Hello World" {
		t.Errorf("expected 'Hello World', got %q", content)
	}
}

func TestLLMStreamNode_InvalidPromptType(t *testing.T) {
	mockModel := mock.NewMockModelWithResponses(
		mock.MockTextResponse("response"),
	)

	node := NewLLMStreamNode(
		"llm_stream",
		"prompt",
		"stream_readers",
		mockModel,
		1,
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"prompt": 123} // 非字符串类型

	_, err := node.Run(ctx, inputs)
	if err == nil {
		t.Fatal("expected error for non-string prompt")
	}
	if !strings.Contains(err.Error(), "not a string") {
		t.Errorf("expected 'not a string' error, got %v", err)
	}
}

func TestLLMStreamNode_ModelError(t *testing.T) {
	mockModel := mock.NewMockModel()
	mockModel.SetStreamFunc(func(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
		return nil, errors.New("model stream error")
	})

	node := NewLLMStreamNode(
		"llm_stream",
		"prompt",
		"stream_readers",
		mockModel,
		1,
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"prompt": "test"}

	_, err := node.Run(ctx, inputs)
	if err == nil {
		t.Fatal("expected model error")
	}
	if err.Error() != "model stream error" {
		t.Errorf("expected 'model stream error', got %v", err)
	}
}

func TestLLMStreamNode_MultipleCopiesIndependent(t *testing.T) {
	mockModel := mock.NewMockModelWithResponses(
		mock.MockTextResponse("ABC"),
	)

	node := NewLLMStreamNode(
		"llm_stream",
		"prompt",
		"stream_readers",
		mockModel,
		3,
	)

	ctx := flow.NewFlowContext(context.Background())
	inputs := map[string]any{"prompt": "test"}

	result, err := node.Run(ctx, inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	readers := result["stream_readers"].([]*schema.StreamReader)
	if len(readers) != 3 {
		t.Fatalf("expected 3 readers, got %d", len(readers))
	}

	// 每个 reader 应该独立读取相同的内容
	for i, reader := range readers {
		var content string
		for {
			msg, err := reader.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("reader %d read error: %v", i, err)
			}
			content += msg.Content
		}

		if content != "ABC" {
			t.Errorf("reader %d: expected 'ABC', got %q", i, content)
		}
	}
}
