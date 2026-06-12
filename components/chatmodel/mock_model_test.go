package chatmodel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// 基本构造
// ============================================================================

func TestMockModel_New(t *testing.T) {
	m := NewMockModel()
	if m.GetCallCount() != 0 {
		t.Errorf("initial call count = %d", m.GetCallCount())
	}
	if m.GetModelName() != "mock-model" {
		t.Errorf("model name = %q", m.GetModelName())
	}
}

func TestMockModel_NewWithResponses(t *testing.T) {
	m := NewMockModelWithResponses(
		MockTextResponse("a"),
		MockTextResponse("b"),
	)
	if m.GetCallCount() != 0 {
		t.Error("call count should be 0 before any call")
	}

	resp, _ := m.Generate(context.Background(), nil)
	if resp.Content != "a" {
		t.Errorf("first call: got %q, want %q", resp.Content, "a")
	}

	resp, _ = m.Generate(context.Background(), nil)
	if resp.Content != "b" {
		t.Errorf("second call: got %q, want %q", resp.Content, "b")
	}
}

// ============================================================================
// 响应队列
// ============================================================================

func TestMockModel_ResponseQueue_Exhausted(t *testing.T) {
	m := NewMockModelWithResponses(MockTextResponse("only"))

	m.Generate(context.Background(), nil)
	m.Generate(context.Background(), nil) // 超出队列

	// 超出后始终返回最后一个
	resp, _ := m.Generate(context.Background(), nil)
	if resp.Content != "only" {
		t.Errorf("got %q, want 'only'", resp.Content)
	}
}

func TestMockModel_ResponseQueue_Loop(t *testing.T) {
	m := NewMockModelWithResponses(
		MockTextResponse("a"),
		MockTextResponse("b"),
	)
	m.SetLoop(true)

	resp1, _ := m.Generate(context.Background(), nil)
	resp2, _ := m.Generate(context.Background(), nil)
	resp3, _ := m.Generate(context.Background(), nil) // 循环回 "a"

	if resp1.Content != "a" || resp2.Content != "b" || resp3.Content != "a" {
		t.Errorf("loop failed: got %q, %q, %q", resp1.Content, resp2.Content, resp3.Content)
	}
}

func TestMockModel_EmptyQueue_DefaultResponse(t *testing.T) {
	m := NewMockModel()
	resp, _ := m.Generate(context.Background(), nil)

	if resp.Content != "mock default response" {
		t.Errorf("got %q", resp.Content)
	}
}

func TestMockModel_AddResponse(t *testing.T) {
	m := NewMockModel()
	m.AddResponse(MockTextResponse("added"))

	resp, _ := m.Generate(context.Background(), nil)
	if resp.Content != "added" {
		t.Errorf("got %q", resp.Content)
	}
}

// ============================================================================
// 自定义函数
// ============================================================================

func TestMockModel_SetGenerateFunc(t *testing.T) {
	m := NewMockModel()
	m.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		return &schema.Message{
			Role:    schema.AssistantRole,
			Content: "custom",
		}, nil
	})

	resp, _ := m.Generate(context.Background(), nil)
	if resp.Content != "custom" {
		t.Errorf("got %q", resp.Content)
	}
}

func TestMockModel_SetStreamFunc(t *testing.T) {
	m := NewMockModel()
	m.SetStreamFunc(func(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
		reader := schema.NewStreamReader()
		go func() {
			defer reader.Close()
			reader.Send(schema.Message{Role: schema.AssistantRole, Content: "custom_stream"})
		}()
		return reader, nil
	})

	reader, _ := m.Stream(context.Background(), nil)
	msg, _ := reader.Recv()
	if msg.Content != "custom_stream" {
		t.Errorf("got %q", msg.Content)
	}
}

func TestMockModel_CustomFunc_PriorityOverQueue(t *testing.T) {
	m := NewMockModelWithResponses(MockTextResponse("queue"))
	m.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		return &schema.Message{Role: schema.AssistantRole, Content: "func"}, nil
	})

	resp, _ := m.Generate(context.Background(), nil)
	if resp.Content != "func" {
		t.Errorf("custom func should take priority, got %q", resp.Content)
	}
}

// ============================================================================
// 输入记录
// ============================================================================

func TestMockModel_RecordedInputs(t *testing.T) {
	m := NewMockModelWithResponses(MockTextResponse("ok"))

	m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("hello"),
	})
	m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("world"),
	})

	inputs := m.GetRecordedInputs()
	if len(inputs) != 2 {
		t.Fatalf("recorded %d inputs, want 2", len(inputs))
	}
	if inputs[0][0].TextContent() != "hello" {
		t.Errorf("first input = %q", inputs[0][0].TextContent())
	}
	if inputs[1][0].TextContent() != "world" {
		t.Errorf("second input = %q", inputs[1][0].TextContent())
	}
}

func TestMockModel_RecordedInputs_DeepCopy(t *testing.T) {
	m := NewMockModelWithResponses(MockTextResponse("ok"))

	msg := schema.UserMessage("original")
	m.Generate(context.Background(), []*schema.Message{msg})

	// 修改原始消息不应影响记录
	msg.Content = "modified"

	inputs := m.GetRecordedInputs()
	if inputs[0][0].TextContent() == "modified" {
		t.Error("GetRecordedInputs should return deep copies")
	}
}

func TestMockModel_GetLastInput(t *testing.T) {
	m := NewMockModelWithResponses(MockTextResponse("ok"))

	if m.GetLastInput() != nil {
		t.Error("GetLastInput should be nil before any call")
	}

	m.Generate(context.Background(), []*schema.Message{schema.UserMessage("first")})
	m.Generate(context.Background(), []*schema.Message{schema.UserMessage("second")})

	last := m.GetLastInput()
	if last[0].TextContent() != "second" {
		t.Errorf("got %q, want 'second'", last[0].TextContent())
	}
}

func TestMockModel_Reset(t *testing.T) {
	m := NewMockModelWithResponses(MockTextResponse("ok"))
	m.Generate(context.Background(), nil)

	m.Reset()

	if m.GetCallCount() != 0 {
		t.Errorf("call count = %d after reset", m.GetCallCount())
	}
}

// ============================================================================
// 模型名称
// ============================================================================

func TestMockModel_SetModelName(t *testing.T) {
	m := NewMockModel()
	m.SetModelName("gpt-4")

	if m.GetModelName() != "gpt-4" {
		t.Errorf("got %q", m.GetModelName())
	}
}

// ============================================================================
// 延迟
// ============================================================================

func TestMockModel_Delay(t *testing.T) {
	m := NewMockModelWithResponses(
		MockDelayedResponse("ok", 100*time.Millisecond),
	)

	start := time.Now()
	m.Generate(context.Background(), nil)
	elapsed := time.Since(start)

	if elapsed < 80*time.Millisecond {
		t.Errorf("expected ~100ms delay, got %v", elapsed)
	}
}

func TestMockModel_Delay_ContextCancel(t *testing.T) {
	m := NewMockModelWithResponses(
		MockDelayedResponse("ok", 5*time.Second),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := m.Generate(ctx, nil)
	if err == nil {
		t.Fatal("expected context error")
	}
}

// ============================================================================
// 错误
// ============================================================================

func TestMockModel_Error(t *testing.T) {
	m := NewMockModelWithResponses(
		MockErrorResponse(fmt.Errorf("boom")),
	)

	_, err := m.Generate(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "boom" {
		t.Errorf("got error %q", err.Error())
	}
}

// ============================================================================
// Stream
// ============================================================================

func TestMockModel_Stream_Basic(t *testing.T) {
	m := NewMockModelWithResponses(MockTextResponse("Hello World"))

	reader, err := m.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var full string
	for {
		msg, err := reader.Recv()
		if err != nil {
			break
		}
		full += msg.Content
	}

	if full != "Hello World" {
		t.Errorf("streamed = %q, want %q", full, "Hello World")
	}
}

func TestMockModel_Stream_WithReasoning(t *testing.T) {
	m := NewMockModelWithResponses(
		MockReasoningResponse("答案是42", "让我想想..."),
	)

	reader, _ := m.Stream(context.Background(), nil)

	var content, reasoning string
	for {
		msg, err := reader.Recv()
		if err != nil {
			break
		}
		content += msg.Content
		reasoning += msg.ReasoningContent
	}

	if reasoning != "让我想想..." {
		t.Errorf("reasoning = %q", reasoning)
	}
	if content != "答案是42" {
		t.Errorf("content = %q", content)
	}
}

func TestMockModel_Stream_WithToolCalls(t *testing.T) {
	m := NewMockModelWithResponses(
		MockToolCallResponse("test_tool", map[string]any{"key": "value"}),
	)

	reader, _ := m.Stream(context.Background(), nil)

	var toolCalls []schema.ToolCall
	for {
		msg, err := reader.Recv()
		if err != nil {
			break
		}
		toolCalls = append(toolCalls, msg.ToolCalls...)
	}

	if len(toolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "test_tool" {
		t.Errorf("tool name = %q", toolCalls[0].Function.Name)
	}
}

func TestMockModel_Stream_Error(t *testing.T) {
	m := NewMockModelWithResponses(
		MockErrorResponse(fmt.Errorf("stream error")),
	)

	_, err := m.Stream(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

// ============================================================================
// 多模态
// ============================================================================

func TestMockModel_MultimodalInput(t *testing.T) {
	m := NewMockModelWithResponses(MockTextResponse("ok"))

	msg := schema.UserMultimodalMessage(
		schema.TextPart("描述图片"),
		schema.ImagePartBase64("image/png", "abc123"),
	)

	m.Generate(context.Background(), []*schema.Message{msg})

	if !m.HasMultimodalInput() {
		t.Error("should detect multimodal input")
	}
	if m.GetMultimodalCallCount() != 1 {
		t.Errorf("multimodal count = %d, want 1", m.GetMultimodalCallCount())
	}

	images := m.GetLastInputImages()
	if len(images) != 1 {
		t.Fatalf("image count = %d, want 1", len(images))
	}
	if !strings.HasPrefix(images[0], "data:image/png;base64,") {
		t.Errorf("image format = %q", images[0][:30])
	}
}

func TestMockModel_MultimodalResponse(t *testing.T) {
	m := NewMockModelWithResponses(
		MockMultimodalResponse("生成的图片", "https://example.com/img.png"),
	)

	resp, _ := m.Generate(context.Background(), nil)

	if !resp.IsMultimodal() {
		t.Error("response should be multimodal")
	}
	if resp.ImageCount() != 1 {
		t.Errorf("image count = %d, want 1", resp.ImageCount())
	}
	if resp.Content != "生成的图片" {
		t.Errorf("content = %q", resp.Content)
	}
}

func TestMockModel_MultimodalResponse_Stream(t *testing.T) {
	m := NewMockModelWithResponses(
		MockMultimodalResponse("图表", "https://example.com/chart.png"),
	)

	reader, _ := m.Stream(context.Background(), nil)

	var lastMsg *schema.Message
	for {
		msg, err := reader.Recv()
		if err != nil {
			break
		}
		lastMsg = msg
	}

	if lastMsg == nil {
		t.Fatal("should have received a message")
	}
	if !lastMsg.IsMultimodal() {
		t.Error("streamed message should be multimodal")
	}
	if lastMsg.ImageCount() != 1 {
		t.Errorf("image count = %d, want 1", lastMsg.ImageCount())
	}
}

func TestMockModel_GetLastInputImages_Empty(t *testing.T) {
	m := NewMockModel()
	if images := m.GetLastInputImages(); images != nil {
		t.Errorf("expected nil, got %v", images)
	}
}

func TestMockModel_HasMultimodalInput_Empty(t *testing.T) {
	m := NewMockModel()
	if m.HasMultimodalInput() {
		t.Error("empty model should not have multimodal input")
	}
}

func TestMockModel_PureTextNotMultimodal(t *testing.T) {
	m := NewMockModelWithResponses(MockTextResponse("ok"))

	m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("纯文本"),
	})

	if m.HasMultimodalInput() {
		t.Error("pure text should not be multimodal")
	}
	if m.GetMultimodalCallCount() != 0 {
		t.Errorf("multimodal count = %d, want 0", m.GetMultimodalCallCount())
	}
}

func TestMockModel_GetLastInputTextContent_Multimodal(t *testing.T) {
	m := NewMockModelWithResponses(MockTextResponse("ok"))

	m.Generate(context.Background(), []*schema.Message{
		schema.UserMultimodalMessage(
			schema.TextPart("这是文本"),
			schema.ImagePartBase64("image/png", "data"),
		),
	})

	text := m.GetLastInputTextContent()
	if text != "这是文本" {
		t.Errorf("got %q, want '这是文本'", text)
	}
}

// ============================================================================
// 并发安全
// ============================================================================

func TestMockModel_Concurrent(t *testing.T) {
	m := NewMockModelWithResponses(
		MockTextResponse("a"),
		MockTextResponse("b"),
		MockTextResponse("c"),
		MockTextResponse("d"),
		MockTextResponse("e"),
	)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Generate(context.Background(), []*schema.Message{
				schema.UserMessage("test"),
			})
		}()
	}
	wg.Wait()

	if m.GetCallCount() != 5 {
		t.Errorf("call count = %d, want 5", m.GetCallCount())
	}
}

// ============================================================================
// 预置场景
// ============================================================================

func TestNewWeatherMockModel(t *testing.T) {
	m := NewWeatherMockModel()

	// 第 1 轮：工具调用
	resp1, _ := m.Generate(context.Background(), nil)
	if len(resp1.ToolCalls) != 1 {
		t.Fatalf("first call: expected 1 tool call, got %d", len(resp1.ToolCalls))
	}
	if resp1.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool name = %q", resp1.ToolCalls[0].Function.Name)
	}

	// 第 2 轮：文本结果
	resp2, _ := m.Generate(context.Background(), nil)
	if !strings.Contains(resp2.Content, "北京") {
		t.Errorf("got %q", resp2.Content)
	}
}

func TestNewEchoMockModel(t *testing.T) {
	m := NewEchoMockModel()

	resp, _ := m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("Hello"),
	})
	if resp.Content != "Echo: Hello" {
		t.Errorf("got %q", resp.Content)
	}
}

func TestNewEchoMockModel_Multimodal(t *testing.T) {
	m := NewEchoMockModel()

	msg := schema.UserMultimodalMessage(
		schema.TextPart("带图的文本"),
		schema.ImagePartBase64("image/png", "data"),
	)

	resp, _ := m.Generate(context.Background(), []*schema.Message{msg})
	if resp.Content != "Echo: 带图的文本" {
		t.Errorf("got %q, want 'Echo: 带图的文本'", resp.Content)
	}
}
