package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// 接口合规
// ============================================================================

func TestMockAgent_InterfaceCompliance(t *testing.T) {
	var _ Interface = NewMockAgent()
	var _ Interface = (*MockAgent)(nil)
}

// ============================================================================
// 基本收发
// ============================================================================

func TestMockAgent_BasicSend(t *testing.T) {
	mock := NewMockAgent().WithFallback("你好！")

	resp, err := mock.SendMessage(context.Background(), schema.UserMessage("随便"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "你好！" {
		t.Errorf("got %q, want %q", resp.Content, "你好！")
	}
	if resp.Role != schema.AssistantRole {
		t.Errorf("role = %q, want %q", resp.Role, schema.AssistantRole)
	}
}

func TestMockAgent_NilMessage(t *testing.T) {
	mock := NewMockAgent().WithFallback("fallback")

	resp, err := mock.SendMessage(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "fallback" {
		t.Errorf("nil message should use fallback, got %q", resp.Content)
	}
}

// ============================================================================
// 响应匹配
// ============================================================================

func TestMockAgent_WithResponse_Exact(t *testing.T) {
	mock := NewMockAgent().
		WithResponse("天气", "今天晴天").
		WithFallback("不知道")

	resp, _ := mock.SendMessage(context.Background(), schema.UserMessage("天气"))
	if resp.Content != "今天晴天" {
		t.Errorf("exact match failed: got %q", resp.Content)
	}
}

func TestMockAgent_WithResponse_Contains(t *testing.T) {
	mock := NewMockAgent().
		WithResponse("你好", "你好！有什么可以帮你的？").
		WithFallback("不知道")

	// "你好吗" 包含 "你好"
	resp, _ := mock.SendMessage(context.Background(), schema.UserMessage("你好吗"))
	if resp.Content != "你好！有什么可以帮你的？" {
		t.Errorf("contains match failed: got %q", resp.Content)
	}
}

func TestMockAgent_WithResponse_NoMatch_Fallback(t *testing.T) {
	mock := NewMockAgent().
		WithResponse("天气", "晴天").
		WithFallback("默认回复")

	resp, _ := mock.SendMessage(context.Background(), schema.UserMessage("今天吃什么"))
	if resp.Content != "默认回复" {
		t.Errorf("expected fallback, got %q", resp.Content)
	}
}

func TestMockAgent_WithResponse_NoMatch_NoFallback(t *testing.T) {
	mock := NewMockAgent().
		WithResponse("天气", "晴天")

	resp, _ := mock.SendMessage(context.Background(), schema.UserMessage("随便"))
	if resp.Content != "[MockAgent] no matching response" {
		t.Errorf("expected default message, got %q", resp.Content)
	}
}

func TestMockAgent_WithResponse_Priority(t *testing.T) {
	// 精确匹配优先于 fallback
	mock := NewMockAgent().
		WithResponse("你好", "精确匹配").
		WithFallback("fallback")

	resp, _ := mock.SendMessage(context.Background(), schema.UserMessage("你好"))
	if resp.Content != "精确匹配" {
		t.Errorf("expected exact match, got %q", resp.Content)
	}
}

func TestMockAgent_WithResponseMsg(t *testing.T) {
	msg := &schema.Message{
		Role:    schema.AssistantRole,
		Content: "自定义消息",
	}
	mock := NewMockAgent().WithResponseMsg("test", msg)

	resp, _ := mock.SendMessage(context.Background(), schema.UserMessage("test"))
	if resp.Content != "自定义消息" {
		t.Errorf("got %q", resp.Content)
	}
}

// ============================================================================
// 错误处理
// ============================================================================

func TestMockAgent_WithError(t *testing.T) {
	mock := NewMockAgent().WithError(fmt.Errorf("API 限流"))

	_, err := mock.SendMessage(context.Background(), schema.UserMessage("test"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "API 限流" {
		t.Errorf("got error %q", err.Error())
	}
}

func TestMockAgent_ErrorOverFallback(t *testing.T) {
	// Error 优先于 fallback
	mock := NewMockAgent().
		WithFallback("fallback").
		WithError(fmt.Errorf("boom"))

	_, err := mock.SendMessage(context.Background(), schema.UserMessage("test"))
	if err == nil {
		t.Fatal("error should take priority over fallback")
	}
}

// ============================================================================
// 调用追踪
// ============================================================================

func TestMockAgent_CallCount(t *testing.T) {
	mock := NewMockAgent().WithFallback("ok")

	if mock.CallCount() != 0 {
		t.Errorf("initial count = %d, want 0", mock.CallCount())
	}

	mock.SendMessage(context.Background(), schema.UserMessage("a"))
	mock.SendMessage(context.Background(), schema.UserMessage("b"))
	mock.SendMessage(context.Background(), schema.UserMessage("c"))

	if mock.CallCount() != 3 {
		t.Errorf("count = %d, want 3", mock.CallCount())
	}
}

func TestMockAgent_LastCall(t *testing.T) {
	mock := NewMockAgent().WithFallback("ok")

	if mock.LastCall() != nil {
		t.Error("LastCall should be nil before any call")
	}

	mock.SendMessage(context.Background(), schema.UserMessage("第一条"))
	mock.SendMessage(context.Background(), schema.UserMessage("第二条"))

	last := mock.LastCall()
	if last == nil {
		t.Fatal("LastCall should not be nil")
	}
	if last.TextContent() != "第二条" {
		t.Errorf("LastCall = %q, want %q", last.TextContent(), "第二条")
	}
}

func TestMockAgent_LastCallContent(t *testing.T) {
	mock := NewMockAgent().WithFallback("ok")

	if mock.LastCallContent() != "" {
		t.Error("LastCallContent should be empty before any call")
	}

	mock.SendMessage(context.Background(), schema.UserMessage("hello"))
	if mock.LastCallContent() != "hello" {
		t.Errorf("got %q, want %q", mock.LastCallContent(), "hello")
	}
}

func TestMockAgent_HasCallWith(t *testing.T) {
	mock := NewMockAgent().WithFallback("ok")

	mock.SendMessage(context.Background(), schema.UserMessage("请帮我查询天气"))

	if !mock.HasCallWith("天气") {
		t.Error("should find '天气' in call history")
	}
	if mock.HasCallWith("不存在的内容") {
		t.Error("should not find non-existent content")
	}
}

func TestMockAgent_Calls(t *testing.T) {
	mock := NewMockAgent().WithFallback("ok")

	mock.SendMessage(context.Background(), schema.UserMessage("a"))
	mock.SendMessage(context.Background(), schema.UserMessage("b"))

	calls := mock.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Msg.TextContent() != "a" {
		t.Errorf("first call = %q, want %q", calls[0].Msg.TextContent(), "a")
	}
	if calls[1].Msg.TextContent() != "b" {
		t.Errorf("second call = %q, want %q", calls[1].Msg.TextContent(), "b")
	}

	// 修改返回的副本不应影响内部状态
	calls[0].Msg.Content = "modified"
	if mock.Calls()[0].Msg.TextContent() == "modified" {
		t.Error("Calls() should return a copy")
	}
}

// ============================================================================
// Reset
// ============================================================================

func TestMockAgent_Reset(t *testing.T) {
	mock := NewMockAgent().
		WithResponse("a", "b").
		WithFallback("c").
		WithError(fmt.Errorf("d")).
		WithDelay(time.Second)

	mock.SendMessage(context.Background(), schema.UserMessage("a"))

	mock.Reset()

	// Reset 后所有状态清空
	if mock.CallCount() != 0 {
		t.Errorf("call count = %d after reset", mock.CallCount())
	}

	resp, err := mock.SendMessage(context.Background(), schema.UserMessage("a"))
	if err != nil {
		t.Fatalf("error should be cleared: %v", err)
	}
	if resp.Content != "[MockAgent] no matching response" {
		t.Errorf("responses should be cleared, got %q", resp.Content)
	}
}

// ============================================================================
// Stream
// ============================================================================

func TestMockAgent_Stream(t *testing.T) {
	mock := NewMockAgent().WithResponse("test", "Hello World Stream")

	var chunks []string
	final, err := mock.SendMessageStream(
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

	// 验证 chunks 拼接等于完整内容
	full := strings.Join(chunks, "")
	if full != "Hello World Stream" {
		t.Errorf("streamed content = %q, want %q", full, "Hello World Stream")
	}

	// 验证 final message
	if final.Content != "Hello World Stream" {
		t.Errorf("final content = %q, want %q", final.Content, "Hello World Stream")
	}

	// 验证 Usage 被设置
	if final.Usage == nil {
		t.Fatal("Usage should not be nil")
	}
	if final.Usage.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100", final.Usage.PromptTokens)
	}
}

func TestMockAgent_Stream_EmptyResponse(t *testing.T) {
	mock := NewMockAgent().WithResponse("test", "")

	var gotContent bool
	_, err := mock.SendMessageStream(
		context.Background(),
		schema.UserMessage("test"),
		func(msg *schema.Message, isToolCall bool) bool {
			if msg.Content != "" {
				gotContent = true
			}
			return true
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContent {
		t.Error("empty response should produce no content chunks")
	}
}

func TestMockAgent_Stream_CancelMidway(t *testing.T) {
	mock := NewMockAgent().WithResponse("test", "A very long message that should be cancelled midway")

	var chunks []string
	_, err := mock.SendMessageStream(
		context.Background(),
		schema.UserMessage("test"),
		func(msg *schema.Message, isToolCall bool) bool {
			chunks = append(chunks, msg.Content)
			return len(chunks) < 2 // 发送 2 个 chunk 后取消
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
// Context
// ============================================================================

func TestMockAgent_ContextCancel(t *testing.T) {
	mock := NewMockAgent().
		WithFallback("ok").
		WithDelay(5 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := mock.SendMessage(ctx, schema.UserMessage("test"))
	if err == nil {
		t.Fatal("expected context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

// ============================================================================
// 延迟
// ============================================================================

func TestMockAgent_Delay(t *testing.T) {
	mock := NewMockAgent().
		WithFallback("ok").
		WithDelay(100 * time.Millisecond)

	start := time.Now()
	mock.SendMessage(context.Background(), schema.UserMessage("test"))
	elapsed := time.Since(start)

	if elapsed < 90*time.Millisecond {
		t.Errorf("expected ~100ms delay, got %v", elapsed)
	}
}

// ============================================================================
// 回调钩子
// ============================================================================

func TestMockAgent_OnSend(t *testing.T) {
	var received []*schema.Message

	mock := NewMockAgent().
		WithFallback("ok").
		WithOnSend(func(msg *schema.Message) {
			received = append(received, msg)
		})

	mock.SendMessage(context.Background(), schema.UserMessage("第一条"))
	mock.SendMessage(context.Background(), schema.UserMessage("第二条"))

	if len(received) != 2 {
		t.Fatalf("onSend called %d times, want 2", len(received))
	}
	if received[0].TextContent() != "第一条" {
		t.Errorf("first hook: got %q", received[0].TextContent())
	}
	if received[1].TextContent() != "第二条" {
		t.Errorf("second hook: got %q", received[1].TextContent())
	}
}

// ============================================================================
// 多模态消息
// ============================================================================

func TestMockAgent_Multimodal(t *testing.T) {
	mock := NewMockAgent().
		WithResponse("图片", "我看到了一张图片").
		WithFallback("默认")

	msg := schema.UserMultimodalMessage(
		schema.TextPart("请描述这张图片"),
		schema.ImagePartBase64("image/png", "iVBOR..."),
	)

	resp, err := mock.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "我看到了一张图片" {
		t.Errorf("multimodal match failed: got %q", resp.Content)
	}

	// 验证调用记录保留了多模态内容
	last := mock.LastCall()
	if !last.IsMultimodal() {
		t.Error("last call should be multimodal")
	}
	if last.ImageCount() != 1 {
		t.Errorf("image count = %d, want 1", last.ImageCount())
	}
}

// ============================================================================
// 链式配置
// ============================================================================

func TestMockAgent_ChainConfig(t *testing.T) {
	mock := NewMockAgent().
		WithResponse("a", "A").
		WithResponse("b", "B").
		WithResponse("c", "C").
		WithFallback("default").
		WithDelay(10 * time.Millisecond)

	// 验证多个 Response 都能匹配
	tests := []struct {
		input, want string
	}{
		{"a", "A"},
		{"b", "B"},
		{"c", "C"},
		{"other", "default"},
	}

	for _, tt := range tests {
		resp, _ := mock.SendMessage(context.Background(), schema.UserMessage(tt.input))
		if resp.Content != tt.want {
			t.Errorf("input %q: got %q, want %q", tt.input, resp.Content, tt.want)
		}
	}
}

// ============================================================================
// 并发安全
// ============================================================================

func TestMockAgent_Concurrent(t *testing.T) {
	mock := NewMockAgent().
		WithResponse("ping", "pong").
		WithFallback("ok")

	var wg sync.WaitGroup
	n := 100

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			msg := fmt.Sprintf("msg_%d", idx)
			_, err := mock.SendMessage(context.Background(), schema.UserMessage(msg))
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()

	if mock.CallCount() != n {
		t.Errorf("call count = %d, want %d", mock.CallCount(), n)
	}
}
