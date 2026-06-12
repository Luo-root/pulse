package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/tools"
)

// ============================================================================
// 辅助函数
// ============================================================================

// newTestAgent 创建用于测试的 Agent
func newTestAgent(model *chatmodel.MockModel, registry *tools.ToolRegistry) *Agent {
	if registry == nil {
		registry = tools.NewToolRegistry()
	}
	return NewAgent(model, registry,
		WithSessionID(fmt.Sprintf("test_%d", time.Now().UnixNano())),
	)
}

// newTestAgentWithTracker 创建带 UsageTracker 的 Agent
func newTestAgentWithTracker(model *chatmodel.MockModel, registry *tools.ToolRegistry) (*Agent, *UsageTracker) {
	tracker := NewUsageTracker()
	ag := newTestAgent(model, registry)
	ag.usageTracker = tracker
	return ag, tracker
}

// ============================================================================
// 基本收发
// ============================================================================

func TestAgent_Send_BasicResponse(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("你好！有什么可以帮助你的？"),
	)

	agent := newTestAgent(model, nil)

	resp, err := agent.SendMessage(context.Background(), schema.UserMessage("你好"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "你好！有什么可以帮助你的？" {
		t.Errorf("got %q, want %q", resp.Content, "你好！有什么可以帮助你的？")
	}
	if model.GetCallCount() != 1 {
		t.Errorf("model called %d times, want 1", model.GetCallCount())
	}
}

func TestAgent_Send_MultipleRounds(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("第一轮回复"),
		chatmodel.MockTextResponse("第二轮回复"),
	)

	agent := newTestAgent(model, nil)

	resp1, err := agent.SendMessage(context.Background(), schema.UserMessage("问题一"))
	if err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if resp1.Content != "第一轮回复" {
		t.Errorf("round 1: got %q", resp1.Content)
	}

	resp2, err := agent.SendMessage(context.Background(), schema.UserMessage("问题二"))
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if resp2.Content != "第二轮回复" {
		t.Errorf("round 2: got %q", resp2.Content)
	}
}

// ============================================================================
// 工具调用
// ============================================================================

func TestAgent_Send_WithToolCall(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.MustRegister(tools.ToolMetadata{
		Name:       "calculator",
		Parameters: map[string]any{"type": "object"},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"result": 42}, nil
	})

	model := chatmodel.NewMockModelWithResponses(
		// 第 1 轮：请求调用 calculator
		chatmodel.MockToolCallResponse("calculator", map[string]any{"expression": "6*7"}),
		// 第 2 轮：基于工具结果返回最终答案
		chatmodel.MockTextResponse("计算结果是 42"),
	)

	agent := newTestAgent(model, registry)

	resp, err := agent.SendMessage(context.Background(), schema.UserMessage("6乘7等于多少"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "计算结果是 42" {
		t.Errorf("got %q, want %q", resp.Content, "计算结果是 42")
	}
	if model.GetCallCount() != 2 {
		t.Errorf("model called %d times, want 2", model.GetCallCount())
	}
}

func TestAgent_Send_MultipleToolCalls(t *testing.T) {
	registry := tools.NewToolRegistry()

	registry.MustRegister(tools.ToolMetadata{
		Name:       "search",
		Parameters: map[string]any{"type": "object"},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"results": []string{"a", "b"}}, nil
	})

	registry.MustRegister(tools.ToolMetadata{
		Name:       "fetch",
		Parameters: map[string]any{"type": "object"},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"data": "fetched"}, nil
	})

	// 用自定义函数，一次返回两个工具调用
	model := chatmodel.NewMockModel()
	callIdx := 0
	model.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		callIdx++
		if callIdx == 1 {
			return &schema.Message{
				Role: schema.AssistantRole,
				ToolCalls: []schema.ToolCall{
					{
						ID:       "call_search",
						Type:     "function",
						Function: schema.FunctionCall{Name: "search", Arguments: `{"q":"test"}`},
					},
					{
						ID:       "call_fetch",
						Type:     "function",
						Function: schema.FunctionCall{Name: "fetch", Arguments: `{"url":"http://example.com"}`},
					},
				},
			}, nil
		}
		return &schema.Message{
			Role:    schema.AssistantRole,
			Content: "搜索和抓取都完成了",
		}, nil
	})

	agent := newTestAgent(model, registry)

	resp, err := agent.SendMessage(context.Background(), schema.UserMessage("搜索并抓取"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "搜索和抓取都完成了" {
		t.Errorf("got %q", resp.Content)
	}
	if model.GetCallCount() != 2 {
		t.Errorf("model called %d times, want 2", model.GetCallCount())
	}
}

// ============================================================================
// Stream
// ============================================================================

func TestAgent_SendStream_Basic(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("流式回复内容测试"),
	)

	agent := newTestAgent(model, nil)

	var chunks []string
	resp, err := agent.SendMessageStream(
		context.Background(),
		schema.UserMessage("test"),
		func(msg *schema.Message, isToolCall bool) bool {
			chunks = append(chunks, msg.Content)
			return true
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fullContent := strings.Join(chunks, "")
	if fullContent != "流式回复内容测试" {
		t.Errorf("streamed = %q, want %q", fullContent, "流式回复内容测试")
	}
	if resp.Content != "流式回复内容测试" {
		t.Errorf("final content = %q", resp.Content)
	}
}

func TestAgent_SendStream_WithToolCalls(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.MustRegister(tools.ToolMetadata{
		Name:       "lookup",
		Parameters: map[string]any{"type": "object"},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return "found", nil
	})

	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockToolCallResponse("lookup", nil),
		chatmodel.MockTextResponse("查找完成"),
	)

	agent := newTestAgent(model, registry)

	var toolCallSeen bool
	resp, err := agent.SendMessageStream(
		context.Background(),
		schema.UserMessage("查找"),
		func(msg *schema.Message, isToolCall bool) bool {
			if isToolCall {
				toolCallSeen = true
			}
			return true
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !toolCallSeen {
		t.Error("should have seen a tool call chunk")
	}
	if resp.Content != "查找完成" {
		t.Errorf("final content = %q", resp.Content)
	}
}

func TestAgent_SendStream_CancelByUser(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("这是一段很长的回复内容用于测试取消功能"),
	)

	agent := newTestAgent(model, nil)

	var count int
	_, err := agent.SendMessageStream(
		context.Background(),
		schema.UserMessage("test"),
		func(msg *schema.Message, isToolCall bool) bool {
			count++
			return count < 2 // 第 2 个 chunk 后取消
		},
	)
	if err == nil {
		t.Fatal("expected error on cancel")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("expected cancel error, got %v", err)
	}
}

// ============================================================================
// 错误处理
// ============================================================================

func TestAgent_Send_ModelError(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockErrorResponse(fmt.Errorf("model overloaded")),
	)

	agent := newTestAgent(model, nil)

	_, err := agent.SendMessage(context.Background(), schema.UserMessage("test"))
	if err == nil {
		t.Fatal("expected model error")
	}
	if !strings.Contains(err.Error(), "model overloaded") {
		t.Errorf("got error %v", err)
	}
}

// ============================================================================
// 选项
// ============================================================================

func TestAgent_WithSessionID(t *testing.T) {
	model := chatmodel.NewMockModel()
	agent := NewAgent(model, nil, WithSessionID("my_session"))

	if agent.sessionID != "my_session" {
		t.Errorf("sessionID = %q, want %q", agent.sessionID, "my_session")
	}
}

func TestAgent_WithMaxToolRounds(t *testing.T) {
	model := chatmodel.NewMockModel()
	agent := NewAgent(model, nil, WithMaxToolRounds(5))

	if agent.maxToolRounds != 5 {
		t.Errorf("maxToolRounds = %d, want 5", agent.maxToolRounds)
	}
}

func TestAgent_WithMaxToolRounds_ZeroIgnored(t *testing.T) {
	model := chatmodel.NewMockModel()
	agent := NewAgent(model, nil, WithMaxToolRounds(0))

	if agent.maxToolRounds != DefaultMaxToolRounds {
		t.Errorf("maxToolRounds = %d, want %d (default)", agent.maxToolRounds, DefaultMaxToolRounds)
	}
}

// ============================================================================
// 消息管理
// ============================================================================

func TestAgent_GetHistory(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("回复一"),
		chatmodel.MockTextResponse("回复二"),
	)

	agent := newTestAgent(model, nil)

	agent.SendMessage(context.Background(), schema.UserMessage("问题一"))
	agent.SendMessage(context.Background(), schema.UserMessage("问题二"))

	history, err := agent.GetHistory(context.Background())
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}

	hasUser := false
	hasAssistant := false
	for _, msg := range history {
		switch msg.Role {
		case schema.UserRole:
			hasUser = true
		case schema.AssistantRole:
			hasAssistant = true
		}
	}

	if !hasUser {
		t.Error("history should contain user messages")
	}
	if !hasAssistant {
		t.Error("history should contain assistant messages")
	}
}

func TestAgent_ClearHistory(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("ok"),
		chatmodel.MockTextResponse("ok"),
	)

	agent := newTestAgent(model, nil)

	agent.SendMessage(context.Background(), schema.UserMessage("test"))
	agent.ClearAgentHistory(context.Background())

	history, _ := agent.GetHistory(context.Background())
	if len(history) != 0 {
		t.Errorf("history length = %d after clear, want 0", len(history))
	}
}

func TestAgent_ChangeModel(t *testing.T) {
	model1 := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("model1"),
	)
	model2 := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("model2"),
	)

	agent := newTestAgent(model1, nil)

	resp1, _ := agent.SendMessage(context.Background(), schema.UserMessage("test"))
	if resp1.Content != "model1" {
		t.Errorf("got %q from model1", resp1.Content)
	}

	agent.ChangeModel(model2)
	agent.ClearAgentHistory(context.Background())

	resp2, _ := agent.SendMessage(context.Background(), schema.UserMessage("test"))
	if resp2.Content != "model2" {
		t.Errorf("got %q from model2", resp2.Content)
	}
}

// ============================================================================
// 并发安全
// ============================================================================

func TestAgent_ConcurrentSend(t *testing.T) {
	// 循环模式，5 个 goroutine 各发一条
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("r1"),
		chatmodel.MockTextResponse("r2"),
		chatmodel.MockTextResponse("r3"),
		chatmodel.MockTextResponse("r4"),
		chatmodel.MockTextResponse("r5"),
	)

	agent := newTestAgent(model, nil)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := agent.SendMessage(context.Background(),
				schema.UserMessage(fmt.Sprintf("msg_%d", idx)))
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	if len(errs) > 0 {
		t.Errorf("got %d errors: %v", len(errs), errs)
	}
}

// ============================================================================
// Usage 追踪
// ============================================================================

func TestAgent_UsageTracking(t *testing.T) {
	model := chatmodel.NewMockModel()
	model.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		return &schema.Message{
			Role:    schema.AssistantRole,
			Content: "test",
			Usage: &schema.Usage{
				PromptTokens:     50,
				CompletionTokens: 10,
				TotalTokens:      60,
			},
		}, nil
	})

	agent, tracker := newTestAgentWithTracker(model, nil)

	agent.SendMessage(context.Background(), schema.UserMessage("test"))

	stats := tracker.GetStats()
	if stats.TotalCalls != 1 {
		t.Errorf("total calls = %d, want 1", stats.TotalCalls)
	}
	if stats.TotalPrompt != 50 {
		t.Errorf("prompt tokens = %d, want 50", stats.TotalPrompt)
	}
	if stats.TotalCompletion != 10 {
		t.Errorf("completion tokens = %d, want 10", stats.TotalCompletion)
	}
}

// ============================================================================
// Model 输入记录断言
// ============================================================================

func TestAgent_ModelRecordsInputs(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("ok"),
	)

	agent := newTestAgent(model, nil)
	agent.SendMessage(context.Background(), schema.UserMessage("你好"))

	inputs := model.GetRecordedInputs()
	if len(inputs) == 0 {
		t.Fatal("model should have recorded inputs")
	}

	// 最后一次调用的输入应该包含用户消息
	lastInput := model.GetLastInput()
	if lastInput == nil {
		t.Fatal("GetLastInput should not be nil")
	}

	foundUserMsg := false
	for _, msg := range lastInput {
		if msg.Role == schema.UserRole && strings.Contains(msg.TextContent(), "你好") {
			foundUserMsg = true
		}
	}
	if !foundUserMsg {
		t.Error("last input should contain user message '你好'")
	}
}

// ============================================================================
// Echo 模型集成测试
// ============================================================================

func TestAgent_WithEchoModel(t *testing.T) {
	model := chatmodel.NewEchoMockModel()

	agent := newTestAgent(model, nil)

	resp, err := agent.SendMessage(context.Background(), schema.UserMessage("Hello World"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Echo: Hello World" {
		t.Errorf("got %q, want %q", resp.Content, "Echo: Hello World")
	}
}

// ============================================================================
// 延迟
// ============================================================================

func TestAgent_Send_WithDelay(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockDelayedResponse("ok", 100*time.Millisecond),
	)

	agent := newTestAgent(model, nil)

	start := time.Now()
	resp, err := agent.SendMessage(context.Background(), schema.UserMessage("test"))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("got %q", resp.Content)
	}
	if elapsed < 80*time.Millisecond {
		t.Errorf("expected ~100ms delay, got %v", elapsed)
	}
}

func TestAgent_Send_ContextTimeout(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockDelayedResponse("ok", 5*time.Second),
	)

	agent := newTestAgent(model, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := agent.SendMessage(ctx, schema.UserMessage("test"))
	if err == nil {
		t.Fatal("expected context error")
	}
}

// ============================================================================
// 多模态消息
// ============================================================================

func TestAgent_Send_Multimodal(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("我看到了一张截图，页面上有一个标题和一段文字"),
	)

	agent := newTestAgent(model, nil)

	msg := schema.UserMultimodalMessage(
		schema.TextPart("请描述这张截图"),
		schema.ImagePartBase64("image/png", "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJ"),
	)

	resp, err := agent.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Content != "我看到了一张截图，页面上有一个标题和一段文字" {
		t.Errorf("got %q", resp.Content)
	}

	if !model.HasMultimodalInput() {
		t.Error("model should have received multimodal input")
	}

	images := model.GetLastInputImages()
	if len(images) == 0 {
		t.Fatal("model should have received image data")
	}
	if !strings.HasPrefix(images[0], "data:image/png;base64,") {
		t.Errorf("image URL format = %q", images[0][:30])
	}
}

func TestAgent_Send_MultipleImages(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("两张图片都收到了"),
	)

	agent := newTestAgent(model, nil)

	msg := schema.UserMultimodalMessage(
		schema.TextPart("对比这两张图"),
		schema.ImagePartBase64("image/png", "iVBORw0KGgoAAAA..."),
		schema.ImagePartBase64("image/jpeg", "/9j/4AAQSkZJRg..."),
		schema.ImagePart("https://example.com/diagram.png"),
	)

	resp, err := agent.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Content != "两张图片都收到了" {
		t.Errorf("got %q", resp.Content)
	}

	images := model.GetLastInputImages()
	if len(images) != 3 {
		t.Fatalf("image count = %d, want 3", len(images))
	}

	if images[2] != "https://example.com/diagram.png" {
		t.Errorf("third image = %q", images[2])
	}
}

func TestAgent_SendStream_Multimodal(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("流式多模态回复"),
	)

	agent := newTestAgent(model, nil)

	msg := schema.UserMultimodalMessage(
		schema.TextPart("分析这个页面"),
		schema.ImagePartBase64("image/png", "iVBORw0KGgo..."),
	)

	var fullContent string
	_, err := agent.SendMessageStream(context.Background(), msg,
		func(chunk *schema.Message, isToolCall bool) bool {
			fullContent += chunk.Content
			return true
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fullContent != "流式多模态回复" {
		t.Errorf("streamed = %q", fullContent)
	}

	if !model.HasMultimodalInput() {
		t.Error("model should have received multimodal input")
	}
}

func TestAgent_Send_MultimodalWithToolCall(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.MustRegister(tools.ToolMetadata{
		Name:       "analyze_image",
		Parameters: map[string]any{"type": "object"},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"objects": []string{"text", "button"}}, nil
	})

	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockToolCallResponse("analyze_image", map[string]any{"image": "screenshot"}),
		chatmodel.MockTextResponse("页面包含文本和按钮"),
	)

	agent := newTestAgent(model, registry)

	msg := schema.UserMultimodalMessage(
		schema.TextPart("分析这个截图"),
		schema.ImagePartBase64("image/png", "iVBORw0KGgo..."),
	)

	resp, err := agent.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Content != "页面包含文本和按钮" {
		t.Errorf("got %q", resp.Content)
	}

	if model.GetCallCount() != 2 {
		t.Errorf("model called %d times, want 2", model.GetCallCount())
	}
}

func TestAgent_Send_MultimodalRecording(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("图1"),
		chatmodel.MockTextResponse("普通"),
		chatmodel.MockTextResponse("图2"),
	)

	// 使用独立的 session ID，确保每次测试都是全新的记忆
	registry := tools.NewToolRegistry()

	// 第1次调用：多模态
	agent1 := NewAgent(model, registry, WithSessionID("test_multimodal_1"))
	agent1.SendMessage(context.Background(), schema.UserMultimodalMessage(
		schema.TextPart("图1"),
		schema.ImagePartBase64("image/png", "aaa"),
	))

	// 第2次调用：普通文本（使用新的 agent 实例避免历史污染）
	agent2 := NewAgent(model, registry, WithSessionID("test_text_2"))
	agent2.SendMessage(context.Background(), schema.UserMessage("普通问题"))

	// 第3次调用：多模态
	agent3 := NewAgent(model, registry, WithSessionID("test_multimodal_3"))
	agent3.SendMessage(context.Background(), schema.UserMultimodalMessage(
		schema.TextPart("图2"),
		schema.ImagePartBase64("image/jpeg", "bbb"),
	))

	// 验证：只有第1次和第3次是多模态调用
	if model.GetMultimodalCallCount() != 2 {
		t.Errorf("multimodal call count = %d, want 2", model.GetMultimodalCallCount())
	}

	if model.GetCallCount() != 3 {
		t.Errorf("total call count = %d, want 3", model.GetCallCount())
	}

	// 最后一次输入应该包含 'bbb'
	images := model.GetLastInputImages()
	if len(images) != 1 {
		t.Fatalf("last input image count = %d, want 1", len(images))
	}
	if !strings.Contains(images[0], "bbb") {
		t.Error("last image should contain 'bbb'")
	}
}

func TestAgent_Send_PureTextNotMultimodal(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("ok"),
	)

	agent := newTestAgent(model, nil)

	agent.SendMessage(context.Background(), schema.UserMessage("纯文本"))

	if model.HasMultimodalInput() {
		t.Error("pure text should not be detected as multimodal")
	}
	if model.GetMultimodalCallCount() != 0 {
		t.Errorf("multimodal call count = %d, want 0", model.GetMultimodalCallCount())
	}
}

// 新增：测试模型返回多模态响应
func TestAgent_Send_MultimodalResponse(t *testing.T) {
	// 模型返回文本 + 图片（如图片生成场景）
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockMultimodalResponse(
			"这是生成的图片",
			"https://example.com/generated.png",
		),
	)

	agent := newTestAgent(model, nil)

	resp, err := agent.SendMessage(context.Background(), schema.UserMessage("生成一张图片"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 验证文本内容
	if resp.Content != "这是生成的图片" {
		t.Errorf("got content %q", resp.Content)
	}

	// 验证多模态内容（图片 URL）
	if !resp.IsMultimodal() {
		t.Error("response should be multimodal")
	}
	if resp.ImageCount() != 1 {
		t.Errorf("image count = %d, want 1", resp.ImageCount())
	}

	found := false
	for _, p := range resp.ContentParts {
		if p.Type == "image_url" && p.ImageURL != nil && strings.Contains(p.ImageURL.URL, "generated.png") {
			found = true
		}
	}
	if !found {
		t.Error("response should contain image URL with 'generated.png'")
	}
}

// 新增：测试流式返回多模态响应
func TestAgent_SendStream_MultimodalResponse(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockMultimodalResponse(
			"生成的图表",
			"https://example.com/chart.png",
		),
	)

	agent := newTestAgent(model, nil)

	var lastChunk *schema.Message
	_, err := agent.SendMessageStream(
		context.Background(),
		schema.UserMessage("画个图表"),
		func(msg *schema.Message, isToolCall bool) bool {
			lastChunk = msg
			return true
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 流式模式下，多模态内容在一个 chunk 中一次性发送
	if lastChunk == nil {
		t.Fatal("should have received at least one chunk")
	}
	if !lastChunk.IsMultimodal() {
		t.Error("last chunk should be multimodal")
	}
	if lastChunk.ImageCount() != 1 {
		t.Errorf("image count = %d, want 1", lastChunk.ImageCount())
	}
}

// ============================================================================
// 多模态输入
// ============================================================================

func TestAgent_Send_MultimodalInput_AudioVideo(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("已分析媒体"),
	)

	agent := newTestAgent(model, nil)

	msg := schema.UserMultimodalMessage(
		schema.TextPart("分析这段音频"),
		schema.AudioPart("mp3", "SUQzAwAA"),
		schema.VideoPart("https://example.com/video.mp4"),
		schema.FilePart("https://example.com/doc.pdf"),
	)

	resp, err := agent.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "已分析媒体" {
		t.Errorf("content: %s", resp.Content)
	}

	// 验证模型收到了多模态输入
	if !model.HasMultimodalInput() {
		t.Error("model should receive multimodal input")
	}
}

func TestAgent_Send_MultimodalInput_InlineData(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("已处理文档"),
	)

	agent := newTestAgent(model, nil)

	msg := schema.UserMultimodalMessage(
		schema.TextPart("总结这个PDF"),
		schema.InlineDataPart("application/pdf", "JVBERi0xLjQ="),
	)

	resp, err := agent.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "已处理文档" {
		t.Errorf("content: %s", resp.Content)
	}
}

// ============================================================================
// 多模态输出
// ============================================================================

func TestAgent_Send_OutputImages(t *testing.T) {
	model := chatmodel.NewMockModel()
	model.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		return &schema.Message{
			Role:    schema.AssistantRole,
			Content: "图片已生成",
			OutputImages: []schema.OutputImage{
				{URL: "https://example.com/gen.png", RevisedPrompt: "a beautiful sunset"},
			},
		}, nil
	})

	agent := newTestAgent(model, nil)

	resp, err := agent.SendMessage(context.Background(), schema.UserMessage("画一幅日落"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.HasOutputImages() {
		t.Error("should have output images")
	}
	if len(resp.OutputImages) != 1 {
		t.Fatalf("expected 1 output image, got %d", len(resp.OutputImages))
	}
	if resp.OutputImages[0].URL != "https://example.com/gen.png" {
		t.Errorf("url: %s", resp.OutputImages[0].URL)
	}
}

func TestAgent_Send_OutputAudio(t *testing.T) {
	model := chatmodel.NewMockModel()
	model.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		return &schema.Message{
			Role:        schema.AssistantRole,
			Content:     "音频已生成",
			OutputAudio: &schema.OutputAudio{Data: "base64audiodata", Format: "mp3"},
		}, nil
	})

	agent := newTestAgent(model, nil)

	resp, err := agent.SendMessage(context.Background(), schema.UserMessage("读这段话"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.HasOutputAudio() {
		t.Error("should have output audio")
	}
	if resp.OutputAudio.Format != "mp3" {
		t.Errorf("format: %s", resp.OutputAudio.Format)
	}
}

func TestAgent_SendStream_OutputImages(t *testing.T) {
	model := chatmodel.NewMockModelWithResponses(
		chatmodel.MockResponse{
			Content: "图片已生成",
			OutputImages: []schema.OutputImage{
				{URL: "https://example.com/gen.png"},
			},
		},
	)

	agent := newTestAgent(model, nil)

	resp, err := agent.SendMessageStream(
		context.Background(),
		schema.UserMessage("画一幅画"),
		func(msg *schema.Message, isToolCall bool) bool { return true },
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.HasOutputImages() {
		t.Error("stream response should have output images")
	}
}
