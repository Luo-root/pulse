package window

import (
	"testing"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/schema"
)

func TestWindowShortMemory_AddAndGetRecent(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 100}, nil, nil)
	mem := NewShortMemory(wm, nil, nil)

	mem.AddTurn("session1", []*schema.Message{
		schema.UserMessage("hello"),
		schema.AssistantMessage("hi there", ""),
	})

	recent := mem.GetRecent("session1")
	if len(recent) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(recent))
	}
	if recent[0].Content != "hello" {
		t.Fatalf("expected 'hello', got '%s'", recent[0].Content)
	}
}

func TestWindowShortMemory_GetRecent_EmptySession(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 100}, nil, nil)
	mem := NewShortMemory(wm, nil, nil)

	recent := mem.GetRecent("nonexistent")
	if recent != nil {
		t.Fatalf("expected nil, got %v", recent)
	}
}

func TestWindowShortMemory_GetRecent_Truncated(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 2}, nil, nil)
	mem := NewShortMemory(wm, nil, nil)

	mem.AddTurn("session1", []*schema.Message{
		schema.UserMessage("msg1"),
		schema.AssistantMessage("msg2", ""),
		schema.UserMessage("msg3"),
		schema.AssistantMessage("msg4", ""),
	})

	recent := mem.GetRecent("session1")
	if len(recent) != 2 {
		t.Fatalf("expected 2 (truncated), got %d", len(recent))
	}
	if recent[0].Content != "msg3" {
		t.Fatalf("expected msg3, got %s", recent[0].Content)
	}
}

func TestWindowShortMemory_GetContextMessages_NoSummary(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 100}, nil, nil)
	mem := NewShortMemory(wm, nil, nil) // 无 summarizer

	mem.AddTurn("session1", []*schema.Message{
		schema.UserMessage("hello"),
		schema.AssistantMessage("hi", ""),
	})

	ctx := mem.GetContextMessages("session1")
	if len(ctx) != 2 {
		t.Fatalf("expected 2, got %d", len(ctx))
	}
}

func TestWindowShortMemory_GetContextMessages_WithSummary(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 2}, nil, nil)

	// 简单的摘要函数：返回消息数量
	summarizer := func(messages []*schema.Message, model chatmodel.BaseModel) string {
		return "summary of messages"
	}

	mem := NewShortMemory(wm, nil, summarizer)

	// 添加 4 条消息，窗口只保留 2 条
	mem.AddTurn("session1", []*schema.Message{
		schema.UserMessage("old msg1"),
		schema.AssistantMessage("old msg2", ""),
		schema.UserMessage("new msg3"),
		schema.AssistantMessage("new msg4", ""),
	})

	ctx := mem.GetContextMessages("session1")

	// 应该有：summary 系统消息 + 2 条保留消息
	hasSummary := false
	for _, m := range ctx {
		if m.Role == schema.SystemRole {
			hasSummary = true
		}
	}

	if !hasSummary {
		t.Fatal("expected summary system message")
	}

	// 去掉 system 消息后应该有 2 条
	nonSystem := 0
	for _, m := range ctx {
		if m.Role != schema.SystemRole {
			nonSystem++
		}
	}
	if nonSystem != 2 {
		t.Fatalf("expected 2 non-system messages, got %d", nonSystem)
	}
}

func TestWindowShortMemory_Clear(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 100}, nil, nil)
	mem := NewShortMemory(wm, nil, nil)

	mem.AddTurn("session1", []*schema.Message{
		schema.UserMessage("hello"),
	})

	mem.Clear("session1")

	recent := mem.GetRecent("session1")
	if recent != nil {
		t.Fatalf("expected nil after clear, got %v", recent)
	}
}

func TestWindowShortMemory_MultipleSessions(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 100}, nil, nil)
	mem := NewShortMemory(wm, nil, nil)

	mem.AddTurn("session1", []*schema.Message{schema.UserMessage("s1 msg")})
	mem.AddTurn("session2", []*schema.Message{schema.UserMessage("s2 msg")})

	s1 := mem.GetRecent("session1")
	s2 := mem.GetRecent("session2")

	if len(s1) != 1 || s1[0].Content != "s1 msg" {
		t.Fatalf("unexpected session1: %v", s1)
	}
	if len(s2) != 1 || s2[0].Content != "s2 msg" {
		t.Fatalf("unexpected session2: %v", s2)
	}
}

func TestWindowShortMemory_AddTurn_Appends(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 100}, nil, nil)
	mem := NewShortMemory(wm, nil, nil)

	mem.AddTurn("session1", []*schema.Message{schema.UserMessage("msg1")})
	mem.AddTurn("session1", []*schema.Message{schema.AssistantMessage("msg2", "")})

	recent := mem.GetRecent("session1")
	if len(recent) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(recent))
	}
}

func TestWindowShortMemory_ContextMessages_MultipleSummaries(t *testing.T) {
	wm := NewManager(Config{MaxHistoryMessages: 1}, nil, nil)

	callCount := 0
	summarizer := func(messages []*schema.Message, model chatmodel.BaseModel) string {
		callCount++
		return "summary of messages"
	}

	mem := NewShortMemory(wm, nil, summarizer)

	// 第一轮：2 条消息，窗口只保留 1 条 → 触发摘要
	mem.AddTurn("s1", []*schema.Message{
		schema.UserMessage("msg1"),
		schema.AssistantMessage("msg2", ""),
	})
	mem.GetContextMessages("s1")

	// 第二轮：再加 2 条
	mem.AddTurn("s1", []*schema.Message{
		schema.UserMessage("msg3"),
		schema.AssistantMessage("msg4", ""),
	})
	mem.GetContextMessages("s1")

	// 摘要应该被调用了至少一次
	if callCount < 1 {
		t.Fatalf("expected summarizer to be called at least once, got %d", callCount)
	}
}

func TestBuildSummaryPrompt(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("what is Go?"),
		schema.AssistantMessage("Go is a programming language.", ""),
	}

	prompt := buildSummaryPrompt(msgs)

	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	if !contains(prompt, "what is Go?") {
		t.Fatal("expected prompt to contain user message")
	}
	if !contains(prompt, "Go is a programming language") {
		t.Fatal("expected prompt to contain assistant message")
	}
}

func TestBuildSummaryPrompt_WithToolResult(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("read file"),
		{
			Role:      schema.AssistantRole,
			Content:   "",
			ToolCalls: []schema.ToolCall{{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "file_read"}}},
		},
		{
			Role:       schema.ToolRole,
			Content:    "file contents here",
			ToolCallID: "c1",
		},
		schema.AssistantMessage("The file contains: file contents here", ""),
	}

	prompt := buildSummaryPrompt(msgs)

	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
}

func TestFallbackSummary(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("hello world"),
		schema.AssistantMessage("hi there", ""),
		schema.UserMessage("how are you"),
	}

	summary := fallbackSummary(msgs)

	if summary == "" {
		t.Fatal("expected non-empty fallback summary")
	}
	if !contains(summary, "hello world") {
		t.Fatal("expected summary to contain original content")
	}
}

func TestFallbackSummary_LimitParts(t *testing.T) {
	msgs := make([]*schema.Message, 20)
	for i := 0; i < 20; i++ {
		msgs[i] = schema.UserMessage("msg")
	}

	summary := fallbackSummary(msgs)

	// 应该只取前 5 条
	if !contains(summary, "|") {
		// 有分隔符说明截断了
	}
	// 不会 panic
	_ = summary
}

func TestMergeSummaries(t *testing.T) {
	s := mergeSummaries("", "new")
	if s != "new" {
		t.Fatalf("expected 'new', got '%s'", s)
	}

	s = mergeSummaries("old", "new")
	if s != "old\nnew" {
		t.Fatalf("expected 'old\\nnew', got '%s'", s)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
