package window

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

	if tokensZh >= tokensEn {
		// depends on specific text, just verify no panic
	}
	if tokensZh <= 0 {
		t.Fatalf("expected positive tokens for Chinese text, got %d", tokensZh)
	}
}

// ============================================================================
// Manager 测试
// ============================================================================

func TestManager_NilPassthrough(t *testing.T) {
	var wm *Manager
	msgs := []*schema.Message{
		schema.SystemMessage("system"),
		schema.UserMessage("hello"),
	}

	result := wm.Truncate(msgs)

	if len(result) != len(msgs) {
		t.Fatalf("nil Manager should pass through, got %d messages", len(result))
	}
}

func TestManager_EmptyMessages(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 10}, nil, nil)

	result := wm.Truncate(nil)
	if len(result) != 0 {
		t.Fatalf("expected 0, got %d", len(result))
	}

	result = wm.Truncate([]*schema.Message{})
	if len(result) != 0 {
		t.Fatalf("expected 0, got %d", len(result))
	}
}

func TestManager_PreservesSystemMessages(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 2}, nil, nil)

	msgs := []*schema.Message{
		schema.SystemMessage("system prompt"),
		schema.UserMessage("msg1"),
		schema.AssistantMessage("msg2", ""),
		schema.UserMessage("msg3"),
		schema.AssistantMessage("msg4", ""),
	}

	result := wm.Truncate(msgs)

	if result[0].Role != schema.SystemRole {
		t.Fatalf("expected first message to be system, got %s", result[0].Role)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 messages (1 system + 2 history), got %d", len(result))
	}
}

func TestManager_MultipleSystemMessages(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 2}, nil, nil)

	msgs := []*schema.Message{
		schema.SystemMessage("system 1"),
		schema.SystemMessage("system 2"),
		schema.UserMessage("msg1"),
		schema.AssistantMessage("msg2", ""),
		schema.UserMessage("msg3"),
		schema.AssistantMessage("msg4", ""),
	}

	result := wm.Truncate(msgs)

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

func TestManager_MaxHistoryMessages(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 3}, nil, nil)

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
	if result[0].Content != "msg3" {
		t.Fatalf("expected msg3, got %s", result[0].Content)
	}
	if result[2].Content != "msg5" {
		t.Fatalf("expected msg5, got %s", result[2].Content)
	}
}

func TestManager_MaxHistoryTokens(t *testing.T) {
	wm := NewManager(Config{MaxHistoryTokens: 10}, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("this is a moderately long message with many words"),
		schema.AssistantMessage("this is another fairly long response from the assistant", ""),
		schema.UserMessage("short"),
	}

	result := wm.Truncate(msgs)

	if len(result) >= len(msgs) {
		t.Fatalf("expected truncation, got %d messages (original %d)", len(result), len(msgs))
	}
}

func TestManager_ToolMessageChainRepair(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 3}, nil, nil)

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

	for _, m := range result {
		if m.Role == schema.ToolRole {
			t.Fatal("orphan tool message should be removed")
		}
	}
}

func TestManager_NoTruncation(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 100}, nil, nil)

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

func TestManager_BothLimitsApplied(t *testing.T) {
	wm := NewManager(Config{
		MaxHistoryMessages: 5,
		MaxHistoryTokens:   5,
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

	if len(result) > 5 {
		t.Fatalf("expected <= 5 messages, got %d", len(result))
	}
}

func TestManager_GetConfig(t *testing.T) {
	config := Config{
		MaxHistoryMessages: 50,
		MaxHistoryTokens:   1000,
		ReserveTokens:      8000,
	}
	wm := NewManager(config, nil, nil)

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
	wm := NewManager(Config{}, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("hello"),
		schema.AssistantMessage("world", ""),
	}

	result := wm.truncateByTokens(msgs)

	if len(result) != 2 {
		t.Fatalf("expected 2, got %d", len(result))
	}
}

func TestTruncateByTokens_KeepsTail(t *testing.T) {
	wm := NewManager(Config{MaxHistoryTokens: 20}, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("this is a very long old message that takes many many tokens to represent"),
		schema.AssistantMessage("this is a very long old response that also takes many tokens", ""),
		schema.UserMessage("recent short"),
	}

	result := wm.truncateByTokens(msgs)

	last := result[len(result)-1]
	if last.Content != "recent short" {
		t.Fatalf("expected 'recent short' as last message, got '%s'", last.Content)
	}
}

func TestTruncateByTokens_ExtremeLimit(t *testing.T) {
	wm := NewManager(Config{MaxHistoryTokens: 1}, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("a"),
		schema.AssistantMessage("b", ""),
	}

	result := wm.truncateByTokens(msgs)

	if len(result) < 1 {
		t.Fatal("expected at least 1 message")
	}
}

// ============================================================================
// NewManager 自动计算测试
// ============================================================================

func TestNewManager_AutoCalcTokens(t *testing.T) {
	model := &mockContextWindowModel{contextWindow: 128000}

	wm := NewManager(Config{
		ReserveTokens: 8000,
	}, model, nil)

	config := wm.GetConfig()
	if config.MaxHistoryTokens != 120000 {
		t.Fatalf("expected 120000, got %d", config.MaxHistoryTokens)
	}
}

func TestNewManager_ManualOverrideAutoCalc(t *testing.T) {
	model := &mockContextWindowModel{contextWindow: 128000}

	wm := NewManager(Config{
		MaxHistoryTokens: 50000,
		ReserveTokens:    8000,
	}, model, nil)

	config := wm.GetConfig()
	if config.MaxHistoryTokens != 50000 {
		t.Fatalf("expected 50000 (manual), got %d", config.MaxHistoryTokens)
	}
}

// mockContextWindowModel implements ModelContextWindow interface
type mockContextWindowModel struct {
	contextWindow int
}

func (m *mockContextWindowModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	panic("implement me")
}

func (m *mockContextWindowModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
	panic("implement me")
}

func (m *mockContextWindowModel) ContextWindow() int {
	return m.contextWindow
}
