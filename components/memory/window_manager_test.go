package memory

import (
	"context"
	"testing"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// defaultEstimator 测试
// ============================================================================

func TestDefaultEstimator_UserMessage(t *testing.T) {
	e := &defaultEstimator{}
	msg := schema.UserMessage("hello world")
	tokens := e.Estimate(msg)

	if tokens <= 0 {
		t.Fatalf("expected positive tokens, got %d", tokens)
	}
	// 11 runes / 1.8 ≈ 6
	if tokens < 3 || tokens > 15 {
		t.Fatalf("expected ~6 tokens, got %d", tokens)
	}
}

func TestDefaultEstimator_EmptyMessage(t *testing.T) {
	e := &defaultEstimator{}
	msg := &schema.Message{Role: schema.UserRole, Content: ""}
	tokens := e.Estimate(msg)

	if tokens != 0 {
		t.Fatalf("expected 0 tokens for empty message, got %d", tokens)
	}
}

func TestDefaultEstimator_WithReasoningContent(t *testing.T) {
	e := &defaultEstimator{}
	msg := &schema.Message{
		Role:             schema.AssistantRole,
		Content:          "answer",
		ReasoningContent: "let me think about this carefully",
	}
	tokens := e.Estimate(msg)

	// content + reasoning 都应该被统计
	msgNoReasoning := schema.AssistantMessage("answer", "")
	tokensNoReasoning := e.Estimate(msgNoReasoning)

	if tokens <= tokensNoReasoning {
		t.Fatalf("expected more tokens with reasoning: %d vs %d", tokens, tokensNoReasoning)
	}
}

func TestDefaultEstimator_WithToolCalls(t *testing.T) {
	e := &defaultEstimator{}
	msg := &schema.Message{
		Role:    schema.AssistantRole,
		Content: "",
		ToolCalls: []schema.ToolCall{
			{
				ID: "call_1",
				Function: schema.FunctionCall{
					Name:      "file_read",
					Arguments: `{"path":"/very/long/path/to/some/file.txt"}`,
				},
			},
		},
	}
	tokens := e.Estimate(msg)

	if tokens <= 0 {
		t.Fatalf("expected positive tokens for tool call message, got %d", tokens)
	}
}

func TestDefaultEstimator_ChineseText(t *testing.T) {
	e := &defaultEstimator{}

	msgEn := schema.UserMessage("hello")
	msgZh := schema.UserMessage("你好世界")

	tokensEn := e.Estimate(msgEn)
	tokensZh := e.Estimate(msgZh)

	// 中文 4 rune，英文 5 rune，中文应该更少
	if tokensZh >= tokensEn {
		// 这取决于具体文本，这里只是验证不会 panic
	}
	if tokensZh <= 0 {
		t.Fatalf("expected positive tokens for Chinese text, got %d", tokensZh)
	}
}

// ============================================================================
// WindowManager 测试
// ============================================================================

func TestWindowManager_NilPassthrough(t *testing.T) {
	var wm *WindowManager
	msgs := []*schema.Message{
		schema.SystemMessage("system"),
		schema.UserMessage("hello"),
	}

	result := wm.Truncate(msgs)

	if len(result) != len(msgs) {
		t.Fatalf("nil WindowManager should pass through, got %d messages", len(result))
	}
}

func TestWindowManager_EmptyMessages(t *testing.T) {
	wm := NewWindowManager(WindowConfig{MaxHistoryMessages: 10}, nil, nil)

	result := wm.Truncate(nil)
	if len(result) != 0 {
		t.Fatalf("expected 0, got %d", len(result))
	}

	result = wm.Truncate([]*schema.Message{})
	if len(result) != 0 {
		t.Fatalf("expected 0, got %d", len(result))
	}
}

func TestWindowManager_PreservesSystemMessages(t *testing.T) {
	wm := NewWindowManager(WindowConfig{MaxHistoryMessages: 2}, nil, nil)

	msgs := []*schema.Message{
		schema.SystemMessage("system prompt"),
		schema.UserMessage("msg1"),
		schema.AssistantMessage("msg2", ""),
		schema.UserMessage("msg3"),
		schema.AssistantMessage("msg4", ""),
	}

	result := wm.Truncate(msgs)

	// System 消息应该始终保留
	if result[0].Role != schema.SystemRole {
		t.Fatalf("expected first message to be system, got %s", result[0].Role)
	}

	// 总数 = 1 system + 2 history = 3
	if len(result) != 3 {
		t.Fatalf("expected 3 messages (1 system + 2 history), got %d", len(result))
	}
}

func TestWindowManager_MultipleSystemMessages(t *testing.T) {
	wm := NewWindowManager(WindowConfig{MaxHistoryMessages: 2}, nil, nil)

	msgs := []*schema.Message{
		schema.SystemMessage("system 1"),
		schema.SystemMessage("system 2"),
		schema.UserMessage("msg1"),
		schema.AssistantMessage("msg2", ""),
		schema.UserMessage("msg3"),
		schema.AssistantMessage("msg4", ""),
	}

	result := wm.Truncate(msgs)

	// 2 个 system 消息都应该保留
	systemCount := 0
	for _, m := range result {
		if m.Role == schema.SystemRole {
			systemCount++
		}
	}
	if systemCount != 2 {
		t.Fatalf("expected 2 system messages, got %d", systemCount)
	}
}

func TestWindowManager_MaxHistoryMessages(t *testing.T) {
	wm := NewWindowManager(WindowConfig{MaxHistoryMessages: 3}, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("msg1"),
		schema.AssistantMessage("msg2", ""),
		schema.UserMessage("msg3"),
		schema.AssistantMessage("msg4", ""),
		schema.UserMessage("msg5"),
	}

	result := wm.Truncate(msgs)

	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}

	// 应该保留最后 3 条
	if result[0].Content != "msg3" {
		t.Fatalf("expected msg3, got %s", result[0].Content)
	}
	if result[2].Content != "msg5" {
		t.Fatalf("expected msg5, got %s", result[2].Content)
	}
}

func TestWindowManager_MaxHistoryTokens(t *testing.T) {
	// 设置很小的 token 限制，应该截断
	wm := NewWindowManager(WindowConfig{MaxHistoryTokens: 10}, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("this is a moderately long message with many words"),
		schema.AssistantMessage("this is another fairly long response from the assistant", ""),
		schema.UserMessage("short"),
	}

	result := wm.Truncate(msgs)

	// 应该截断掉一些消息
	if len(result) >= len(msgs) {
		t.Fatalf("expected truncation, got %d messages (original %d)", len(result), len(msgs))
	}
}

func TestWindowManager_ToolMessageChainRepair(t *testing.T) {
	wm := NewWindowManager(WindowConfig{MaxHistoryMessages: 3}, nil, nil)

	// 截断后第一条是 tool 消息（孤 orphan），应该被移除
	msgs := []*schema.Message{
		schema.SystemMessage("system"),
		{
			Role:       schema.ToolRole,
			Content:    "tool result",
			ToolCallID: "call_1",
		},
		schema.AssistantMessage("after tool", ""),
		schema.UserMessage("user msg"),
	}

	result := wm.Truncate(msgs)

	// 第一条应该是 system，然后是 assistant 和 user
	// orphan tool 消息应该被移除
	for _, m := range result {
		if m.Role == schema.ToolRole {
			t.Fatal("orphan tool message should be removed")
		}
	}
}

func TestWindowManager_NoTruncation(t *testing.T) {
	wm := NewWindowManager(WindowConfig{MaxHistoryMessages: 100}, nil, nil)

	msgs := []*schema.Message{
		schema.SystemMessage("system"),
		schema.UserMessage("hello"),
		schema.AssistantMessage("hi", ""),
	}

	result := wm.Truncate(msgs)

	if len(result) != 3 {
		t.Fatalf("expected 3 (no truncation), got %d", len(result))
	}
}

func TestWindowManager_BothLimitsApplied(t *testing.T) {
	// 同时设置数量和 token 限制
	wm := NewWindowManager(WindowConfig{
		MaxHistoryMessages: 5,
		MaxHistoryTokens:   5, // 很小的 token 限制
	}, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("message one with some content"),
		schema.AssistantMessage("response one with some content", ""),
		schema.UserMessage("message two with some content"),
		schema.AssistantMessage("response two with some content", ""),
		schema.UserMessage("message three with some content"),
		schema.AssistantMessage("response three with some content", ""),
	}

	result := wm.Truncate(msgs)

	// 两个限制取更严格的
	if len(result) > 5 {
		t.Fatalf("expected <= 5 messages, got %d", len(result))
	}
}

func TestWindowManager_GetConfig(t *testing.T) {
	config := WindowConfig{
		MaxHistoryMessages: 50,
		MaxHistoryTokens:   1000,
		ReserveTokens:      8000,
	}
	wm := NewWindowManager(config, nil, nil)

	got := wm.GetConfig()
	if got.MaxHistoryMessages != 50 {
		t.Fatalf("expected 50, got %d", got.MaxHistoryMessages)
	}
	if got.MaxHistoryTokens != 1000 {
		t.Fatalf("expected 1000, got %d", got.MaxHistoryTokens)
	}
}

// ============================================================================
// truncateByTokens 测试
// ============================================================================

func TestTruncateByTokens_ZeroLimit(t *testing.T) {
	wm := NewWindowManager(WindowConfig{}, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("hello"),
		schema.AssistantMessage("world", ""),
	}

	result := wm.truncateByTokens(msgs)

	// 无限制时不截断
	if len(result) != 2 {
		t.Fatalf("expected 2, got %d", len(result))
	}
}

func TestTruncateByTokens_KeepsTail(t *testing.T) {
	wm := NewWindowManager(WindowConfig{MaxHistoryTokens: 20}, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("this is a very long old message that takes many many tokens to represent"),
		schema.AssistantMessage("this is a very long old response that also takes many tokens", ""),
		schema.UserMessage("recent short"),
	}

	result := wm.truncateByTokens(msgs)

	// 应该保留尾部
	last := result[len(result)-1]
	if last.Content != "recent short" {
		t.Fatalf("expected 'recent short' as last message, got '%s'", last.Content)
	}
}

func TestTruncateByTokens_ExtremeLimit(t *testing.T) {
	// 极端情况：token 限制连一条消息都放不下
	wm := NewWindowManager(WindowConfig{MaxHistoryTokens: 1}, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("a"),
		schema.AssistantMessage("b", ""),
	}

	result := wm.truncateByTokens(msgs)

	// 应该至少保留最后一条
	if len(result) < 1 {
		t.Fatal("expected at least 1 message")
	}
}

// ============================================================================
// NewWindowManager 自动计算测试
// ============================================================================

func TestNewWindowManager_AutoCalcTokens(t *testing.T) {
	model := &mockContextWindowModel{contextWindow: 128000}

	wm := NewWindowManager(WindowConfig{
		ReserveTokens: 8000,
	}, model, nil)

	config := wm.GetConfig()
	// 自动计算：128000 - 8000 = 120000
	if config.MaxHistoryTokens != 120000 {
		t.Fatalf("expected 120000, got %d", config.MaxHistoryTokens)
	}
}

func TestNewWindowManager_ManualOverrideAutoCalc(t *testing.T) {
	model := &mockContextWindowModel{contextWindow: 128000}

	wm := NewWindowManager(WindowConfig{
		MaxHistoryTokens: 50000, // 手动设置，不自动计算
		ReserveTokens:    8000,
	}, model, nil)

	config := wm.GetConfig()
	if config.MaxHistoryTokens != 50000 {
		t.Fatalf("expected 50000 (manual), got %d", config.MaxHistoryTokens)
	}
}

// mockContextWindowModel 实现 ModelContextWindow 接口
type mockContextWindowModel struct {
	contextWindow int
}

func (m *mockContextWindowModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	//TODO implement me
	panic("implement me")
}

func (m *mockContextWindowModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
	//TODO implement me
	panic("implement me")
}

func (m *mockContextWindowModel) ContextWindow() int {
	return m.contextWindow
}
