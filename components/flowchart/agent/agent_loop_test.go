package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel/mock"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/tools"
)

// ============================================================================
// AgentLoop 测试
// ============================================================================

func TestAgentLoop_BasicAnswer(t *testing.T) {
	model := mock.NewMockModelWithResponses(
		mock.MockTextResponse("最终回答"),
	)

	loop := NewAgentLoop(model, tools.NewToolRegistry())
	result, err := loop.Run(context.Background(), "你好")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Answer != "最终回答" {
		t.Errorf("expected '最终回答', got %q", result.Answer)
	}
	if result.Rounds != 1 {
		t.Errorf("expected 1 round, got %d", result.Rounds)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(result.Steps))
	}
	if !result.Steps[0].IsFinal {
		t.Error("step should be final")
	}
}

func TestAgentLoop_ToolCallThenAnswer(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.Register(tools.ToolMetadata{
		Name:       "get_time",
		Parameters: map[string]any{},
		Timeout:    5 * time.Second,
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"time": "2026-06-13"}, nil
	})

	model := mock.NewMockModelWithResponses(
		mock.MockToolCallResponse("get_time", nil),
		mock.MockTextResponse("现在是 2026-06-13"),
	)

	loop := NewAgentLoop(model, registry)
	result, err := loop.Run(context.Background(), "现在几点")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Answer != "现在是 2026-06-13" {
		t.Errorf("answer: %s", result.Answer)
	}
	if result.Rounds != 2 {
		t.Errorf("expected 2 rounds, got %d", result.Rounds)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(result.Steps))
	}
	if result.Steps[0].IsFinal {
		t.Error("step 0 should not be final")
	}
	if len(result.Steps[0].ToolCalls) != 1 {
		t.Errorf("step 0 tool calls: %d", len(result.Steps[0].ToolCalls))
	}
	if !result.Steps[1].IsFinal {
		t.Error("step 1 should be final")
	}
}

func TestAgentLoop_MultipleToolRounds(t *testing.T) {
	registry := tools.NewToolRegistry()
	var callCount atomic.Int32
	registry.Register(tools.ToolMetadata{
		Name:       "step_tool",
		Parameters: map[string]any{},
		Timeout:    5 * time.Second,
	}, func(ctx context.Context, args map[string]any) (any, error) {
		n := callCount.Add(1)
		return map[string]any{"step": n}, nil
	})

	model := mock.NewMockModelWithResponses(
		mock.MockToolCallResponse("step_tool", nil),
		mock.MockToolCallResponse("step_tool", nil),
		mock.MockTextResponse("两步都完成了"),
	)

	loop := NewAgentLoop(model, registry)
	result, err := loop.Run(context.Background(), "做两步")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Rounds != 3 {
		t.Errorf("expected 3 rounds, got %d", result.Rounds)
	}
	if callCount.Load() != 2 {
		t.Errorf("expected 2 tool calls, got %d", callCount.Load())
	}
}

func TestAgentLoop_MaxRoundsExceeded(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.Register(tools.ToolMetadata{
		Name:       "loop_tool",
		Parameters: map[string]any{},
		Timeout:    5 * time.Second,
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"ok": true}, nil
	})

	// 模型总是返回工具调用，永不给出最终回答
	model := mock.NewMockModel()
	model.SetLoop(true)
	model.AddResponse(mock.MockToolCallResponse("loop_tool", nil))

	loop := NewAgentLoop(model, registry, WithMaxRounds(3))
	_, err := loop.Run(context.Background(), "无限循环")
	if err == nil {
		t.Fatal("expected max rounds error")
	}
}

func TestAgentLoop_WithSystemPrompt(t *testing.T) {
	model := mock.NewMockModelWithResponses(
		mock.MockTextResponse("ok"),
	)

	loop := NewAgentLoop(model, tools.NewToolRegistry(),
		WithSystemPrompt("你是一个测试助手"),
	)
	_, err := loop.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 验证系统提示词被传入
	inputs := model.GetRecordedInputs()
	if len(inputs) == 0 {
		t.Fatal("no recorded inputs")
	}
	if inputs[0][0].Role != schema.SystemRole {
		t.Errorf("first message should be system, got %s", inputs[0][0].Role)
	}
	if inputs[0][0].Content != "你是一个测试助手" {
		t.Errorf("system prompt: %s", inputs[0][0].Content)
	}
}

func TestAgentLoop_StepCallback(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.Register(tools.ToolMetadata{
		Name:       "test_tool",
		Parameters: map[string]any{},
		Timeout:    5 * time.Second,
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return "result", nil
	})

	model := mock.NewMockModelWithResponses(
		mock.MockToolCallResponse("test_tool", nil),
		mock.MockTextResponse("done"),
	)

	var steps []*Step
	loop := NewAgentLoop(model, registry,
		WithStepCallback(func(step *Step) {
			steps = append(steps, step)
		}),
	)
	result, err := loop.Run(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if len(result.Steps) != 2 {
		t.Fatalf("expected 2 result steps, got %d", len(result.Steps))
	}
}

func TestAgentLoop_ContextCancelled(t *testing.T) {
	model := mock.NewMockModel()
	model.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return schema.AssistantMessage("never", ""), nil
		}
	})

	loop := NewAgentLoop(model, tools.NewToolRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := loop.Run(ctx, "hello")
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestAgentLoop_EmptyResponse(t *testing.T) {
	model := mock.NewMockModelWithResponses(
		mock.MockTextResponse(""),
	)

	loop := NewAgentLoop(model, tools.NewToolRegistry())
	result, err := loop.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Answer != "" {
		t.Errorf("expected empty answer, got %q", result.Answer)
	}
}

// ============================================================================
// PlanExecutor 测试
// ============================================================================

func TestPlanExecutor_BasicPlanAndExecute(t *testing.T) {
	// 模型第一次调用返回计划 JSON，后续调用返回步骤结果
	model := mock.NewMockModel()
	callCount := 0
	model.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		callCount++
		// 第一次调用：规划
		if callCount == 1 {
			planJSON := `{"steps": [
				{"id": "step_1", "description": "获取数据", "depends_on": []},
				{"id": "step_2", "description": "分析数据", "depends_on": ["step_1"]}
			]}`
			return schema.AssistantMessage(planJSON, ""), nil
		}
		// 后续调用：步骤执行结果
		return schema.AssistantMessage("步骤执行完成", ""), nil
	})

	executor := NewPlanExecutor(model, tools.NewToolRegistry())
	result, err := executor.Run(context.Background(), "分析销售数据")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Answer == "" {
		t.Error("expected non-empty answer")
	}
	if len(result.PlanSteps) != 2 {
		t.Errorf("expected 2 plan steps, got %d", len(result.PlanSteps))
	}
}

func TestPlanExecutor_PlanCallback(t *testing.T) {
	model := mock.NewMockModel()
	model.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		planJSON := `{"steps": [{"id": "s1", "description": "test", "depends_on": []}]}`
		return schema.AssistantMessage(planJSON, ""), nil
	})

	var planReceived []PlanStep
	executor := NewPlanExecutor(model, tools.NewToolRegistry(),
		WithPlanCallback(func(steps []PlanStep) {
			planReceived = steps
		}),
	)
	_, err := executor.Run(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(planReceived) != 1 {
		t.Errorf("expected 1 plan step, got %d", len(planReceived))
	}
}

// ============================================================================
// validateSteps 测试
// ============================================================================

func TestValidateSteps_ValidPlan(t *testing.T) {
	steps := []PlanStep{
		{ID: "s1", Description: "step 1", DependsOn: []string{}},
		{ID: "s2", Description: "step 2", DependsOn: []string{"s1"}},
	}
	if err := validateSteps(steps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSteps_MissingDependency(t *testing.T) {
	steps := []PlanStep{
		{ID: "s1", Description: "step 1", DependsOn: []string{"s999"}},
	}
	if err := validateSteps(steps); err == nil {
		t.Fatal("expected error for missing dependency")
	}
}

func TestValidateSteps_CycleDetection(t *testing.T) {
	steps := []PlanStep{
		{ID: "s1", Description: "step 1", DependsOn: []string{"s2"}},
		{ID: "s2", Description: "step 2", DependsOn: []string{"s1"}},
	}
	if err := validateSteps(steps); err == nil {
		t.Fatal("expected cycle detection error")
	}
}

// ============================================================================
// parsePlanSteps 测试
// ============================================================================

func TestParsePlanSteps_ValidJSON(t *testing.T) {
	input := `{"steps": [
		{"id": "s1", "description": "first step"},
		{"id": "s2", "description": "second step", "depends_on": ["s1"]}
	]}`
	steps, err := parsePlanSteps(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0].ID != "s1" {
		t.Errorf("step 0 ID: %s", steps[0].ID)
	}
	if len(steps[1].DependsOn) != 1 || steps[1].DependsOn[0] != "s1" {
		t.Errorf("step 1 depends_on: %v", steps[1].DependsOn)
	}
}

func TestParsePlanSteps_MarkdownWrapped(t *testing.T) {
	input := "```json\n{\"steps\": [{\"id\": \"s1\", \"description\": \"test\"}]}\n```"
	steps, err := parsePlanSteps(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}
}

func TestParsePlanSteps_DefaultID(t *testing.T) {
	input := `{"steps": [{"description": "unnamed step"}]}`
	steps, err := parsePlanSteps(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if steps[0].ID != "step_1" {
		t.Errorf("expected default ID 'step_1', got %s", steps[0].ID)
	}
}

func TestParsePlanSteps_EmptySteps(t *testing.T) {
	input := `{"steps": []}`
	_, err := parsePlanSteps(input)
	if err == nil {
		t.Fatal("expected error for empty steps")
	}
}

func TestParsePlanSteps_InvalidJSON(t *testing.T) {
	input := "not json at all"
	_, err := parsePlanSteps(input)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// ============================================================================
// trimJSONWrapper 测试
// ============================================================================

func TestTrimJSONWrapper_PlainJSON(t *testing.T) {
	input := `{"steps": []}`
	result := trimJSONWrapper(input)
	if result != input {
		t.Errorf("expected %q, got %q", input, result)
	}
}

func TestTrimJSONWrapper_MarkdownCodeBlock(t *testing.T) {
	input := "```json\n{\"steps\": []}\n```"
	result := trimJSONWrapper(input)
	expected := `{"steps": []}`
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestTrimJSONWrapper_WithPrefixText(t *testing.T) {
	input := "Here is the plan:\n```json\n{\"steps\": []}\n```"
	result := trimJSONWrapper(input)
	expected := `{"steps": []}`
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestTrimJSONWrapper_EmptyString(t *testing.T) {
	result := trimJSONWrapper("")
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

// ============================================================================
// FormatResult 测试
// ============================================================================

func TestFormatResult(t *testing.T) {
	result := &AgentResult{
		Answer: "test answer",
		Rounds: 3,
		Steps:  []*Step{{Round: 1, IsFinal: true}},
		Usage:  schema.Usage{PromptTokens: 100, CompletionTokens: 50},
	}

	output := FormatResult(result)
	if !strings.Contains(output, "test answer") {
		t.Error("should contain answer")
	}
	if !strings.Contains(output, "3") {
		t.Error("should contain rounds")
	}
}

// helper
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
