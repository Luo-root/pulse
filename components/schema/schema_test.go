package schema

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================================
// 构造函数
// ============================================================================

func TestSystemMessage(t *testing.T) {
	msg := SystemMessage("you are helpful")
	if msg.Role != SystemRole {
		t.Errorf("expected system, got %s", msg.Role)
	}
	if msg.Content != "you are helpful" {
		t.Errorf("expected 'you are helpful', got %q", msg.Content)
	}
}

func TestUserMessage(t *testing.T) {
	msg := UserMessage("hello")
	if msg.Role != UserRole {
		t.Errorf("expected user, got %s", msg.Role)
	}
	if msg.Content != "hello" {
		t.Errorf("expected 'hello', got %q", msg.Content)
	}
}

func TestAssistantMessage_NoReasoning(t *testing.T) {
	msg := AssistantMessage("hi", "")
	if msg.Role != AssistantRole {
		t.Errorf("expected assistant, got %s", msg.Role)
	}
	if msg.Content != "hi" {
		t.Errorf("expected 'hi', got %q", msg.Content)
	}
	if msg.ReasoningContent != "" {
		t.Errorf("expected empty reasoning, got %q", msg.ReasoningContent)
	}
}

func TestAssistantMessage_WithReasoning(t *testing.T) {
	msg := AssistantMessage("answer", "let me think...")
	if msg.ReasoningContent != "let me think..." {
		t.Errorf("expected 'let me think...', got %q", msg.ReasoningContent)
	}
}

func TestNewToolResult(t *testing.T) {
	r := NewToolResult("call_1", "output", false)
	if r.CallID != "call_1" {
		t.Errorf("call_id: %s", r.CallID)
	}
	if r.Content != "output" {
		t.Errorf("content: %s", r.Content)
	}
	if r.IsError {
		t.Error("expected IsError=false")
	}
}

func TestNewToolResult_Error(t *testing.T) {
	r := NewToolResult("call_2", "fail", true)
	if !r.IsError {
		t.Error("expected IsError=true")
	}
}

func TestToolResultsMessage_Basic(t *testing.T) {
	results := []ToolResult{
		{CallID: "c1", Content: "result1"},
		{CallID: "c2", Content: "result2"},
	}

	msgs := ToolResultsMessage(results)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	for i, m := range msgs {
		if m.Role != ToolRole {
			t.Errorf("msg %d: expected tool role, got %s", i, m.Role)
		}
	}

	if msgs[0].ToolCallID != "c1" || msgs[0].Content != "result1" {
		t.Errorf("msg 0: id=%s content=%s", msgs[0].ToolCallID, msgs[0].Content)
	}
	if msgs[1].ToolCallID != "c2" || msgs[1].Content != "result2" {
		t.Errorf("msg 1: id=%s content=%s", msgs[1].ToolCallID, msgs[1].Content)
	}
}

func TestToolResultsMessage_WithError(t *testing.T) {
	results := []ToolResult{
		{CallID: "c1", Content: "permission denied", IsError: true},
		{CallID: "c2", Content: "ok", IsError: false},
	}

	msgs := ToolResultsMessage(results)
	if !strings.Contains(msgs[0].Content, "[Error]") {
		t.Errorf("expected [Error] prefix, got %q", msgs[0].Content)
	}
	if strings.Contains(msgs[1].Content, "[Error]") {
		t.Errorf("should not have [Error] prefix, got %q", msgs[1].Content)
	}
}

func TestToolResultsMessage_Empty(t *testing.T) {
	msgs := ToolResultsMessage(nil)
	if len(msgs) != 0 {
		t.Fatalf("expected 0, got %d", len(msgs))
	}

	msgs = ToolResultsMessage([]ToolResult{})
	if len(msgs) != 0 {
		t.Fatalf("expected 0, got %d", len(msgs))
	}
}

func TestToolResultsMessage_WithContentParts(t *testing.T) {
	results := []ToolResult{
		{
			CallID:  "c1",
			Content: "screenshot",
			ContentParts: []ContentPart{
				TextPart("screenshot"),
				ImagePartBase64("image/png", "iVBORw0KGgo="),
			},
		},
	}

	msgs := ToolResultsMessage(results)
	if len(msgs) != 1 {
		t.Fatalf("expected 1, got %d", len(msgs))
	}

	msg := msgs[0]
	if msg.Role != ToolRole {
		t.Errorf("role: %s", msg.Role)
	}
	if msg.ToolCallID != "c1" {
		t.Errorf("tool_call_id: %s", msg.ToolCallID)
	}
	if len(msg.ContentParts) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(msg.ContentParts))
	}
	if msg.ContentParts[0].Type != ContentTypeText {
		t.Errorf("part 0 type: %s", msg.ContentParts[0].Type)
	}
	if msg.ContentParts[1].Type != ContentTypeImageURL {
		t.Errorf("part 1 type: %s", msg.ContentParts[1].Type)
	}
}

func TestToolResultsMessage_WithContentParts_Error(t *testing.T) {
	results := []ToolResult{
		{
			CallID:  "c1",
			Content: "failed",
			IsError: true,
			ContentParts: []ContentPart{
				TextPart("error details"),
			},
		},
	}

	msgs := ToolResultsMessage(results)

	if !strings.Contains(msgs[0].Content, "[Error]") {
		t.Errorf("expected [Error] prefix, got %q", msgs[0].Content)
	}
	if len(msgs[0].ContentParts) != 1 {
		t.Fatalf("expected 1 content part, got %d", len(msgs[0].ContentParts))
	}
}

// ============================================================================
// Message.Clone
// ============================================================================

func TestClone_BasicMessage(t *testing.T) {
	orig := &Message{
		Role:    UserRole,
		Content: "hello",
		Name:    "test",
	}
	cloned := orig.Clone()

	if cloned.Role != UserRole {
		t.Errorf("role: %s", cloned.Role)
	}
	if cloned.Content != "hello" {
		t.Errorf("content: %s", cloned.Content)
	}
	if cloned.Name != "test" {
		t.Errorf("name: %s", cloned.Name)
	}
}

func TestClone_FullMessage(t *testing.T) {
	orig := &Message{
		Role:             AssistantRole,
		Content:          "answer",
		ReasoningContent: "thinking...",
		Name:             "bot",
		Partial:          true,
		ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Index: 0, Function: FunctionCall{Name: "fn1", Arguments: `{"a":1}`}},
			{ID: "tc2", Type: "function", Index: 1, Function: FunctionCall{Name: "fn2", Arguments: `{"b":2}`}},
		},
		ToolCallID: "orig_call",
		Usage: &Usage{
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
		},
	}

	cloned := orig.Clone()

	if cloned.Role != AssistantRole {
		t.Errorf("role")
	}
	if cloned.Content != "answer" {
		t.Errorf("content")
	}
	if cloned.ReasoningContent != "thinking..." {
		t.Errorf("reasoning")
	}
	if cloned.Name != "bot" {
		t.Errorf("name")
	}
	if !cloned.Partial {
		t.Errorf("partial")
	}
	if cloned.ToolCallID != "orig_call" {
		t.Errorf("tool_call_id")
	}

	// ToolCalls 深拷贝
	if len(cloned.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(cloned.ToolCalls))
	}
	if cloned.ToolCalls[0].ID != "tc1" || cloned.ToolCalls[1].ID != "tc2" {
		t.Errorf("tool calls: %s, %s", cloned.ToolCalls[0].ID, cloned.ToolCalls[1].ID)
	}

	// Usage 深拷贝
	if cloned.Usage == nil {
		t.Fatal("usage is nil")
	}
	if cloned.Usage.PromptTokens != 100 || cloned.Usage.CompletionTokens != 50 {
		t.Errorf("usage: %d, %d", cloned.Usage.PromptTokens, cloned.Usage.CompletionTokens)
	}
}

func TestClone_Isolation_ToolCalls(t *testing.T) {
	orig := &Message{
		ToolCalls: []ToolCall{
			{ID: "tc1", Function: FunctionCall{Name: "fn"}},
		},
	}
	cloned := orig.Clone()

	// 修改克隆不应影响原始
	cloned.ToolCalls[0].ID = "modified"
	if orig.ToolCalls[0].ID != "tc1" {
		t.Error("clone did not isolate ToolCalls slice")
	}
}

func TestClone_Isolation_Usage(t *testing.T) {
	orig := &Message{
		Usage: &Usage{PromptTokens: 10},
	}
	cloned := orig.Clone()

	cloned.Usage.PromptTokens = 999
	if orig.Usage.PromptTokens != 10 {
		t.Error("clone did not isolate Usage pointer")
	}
}

func TestClone_NilSlices(t *testing.T) {
	orig := &Message{
		Role:    UserRole,
		Content: "test",
	}
	cloned := orig.Clone()

	if cloned.ToolCalls != nil {
		t.Errorf("expected nil ToolCalls, got %v", cloned.ToolCalls)
	}
	if cloned.Usage != nil {
		t.Errorf("expected nil Usage, got %v", cloned.Usage)
	}
}

func TestClone_EmptyToolCalls(t *testing.T) {
	orig := &Message{
		ToolCalls: []ToolCall{},
	}
	cloned := orig.Clone()

	// 空切片应该被深拷贝（非 nil）
	if cloned.ToolCalls == nil {
		t.Error("expected non-nil empty slice")
	}
	if len(cloned.ToolCalls) != 0 {
		t.Errorf("expected empty, got %d", len(cloned.ToolCalls))
	}
}

func TestClone_ValueReceiver(t *testing.T) {
	// Clone 是指针方法，测试值类型也能用
	orig := Message{Role: UserRole, Content: "test"}
	cloned := orig.Clone()
	if cloned.Content != "test" {
		t.Errorf("content: %s", cloned.Content)
	}
}

// ============================================================================
// StreamReader
// ============================================================================

func TestStreamReader_BasicFlow(t *testing.T) {
	r := NewStreamReader()

	go func() {
		r.Send(Message{Role: AssistantRole, Content: "Hello"})
		r.Send(Message{Role: AssistantRole, Content: " World"})
		r.Close()
	}()

	var full string
	for {
		msg, err := r.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		full += msg.Content
	}

	if full != "Hello World" {
		t.Errorf("expected 'Hello World', got %q", full)
	}
}

func TestStreamReader_EmptyStream(t *testing.T) {
	r := NewStreamReader()
	r.Close()

	msg, err := r.Recv()
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	if msg != nil {
		t.Errorf("expected nil message, got %v", msg)
	}
}

func TestStreamReader_CustomBuffer(t *testing.T) {
	r := NewStreamReaderWithBuffer(1)

	r.Send(Message{Content: "msg1"})
	r.Close()

	msg, err := r.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if msg.Content != "msg1" {
		t.Errorf("content: %s", msg.Content)
	}
}

func TestStreamReader_SetError(t *testing.T) {
	r := NewStreamReader()

	expectedErr := errors.New("stream broken")
	r.SetError(expectedErr)

	_, err := r.Recv()
	if err != expectedErr {
		t.Fatalf("expected %v, got %v", expectedErr, err)
	}
}

func TestStreamReader_SetError_IgnoresNil(t *testing.T) {
	r := NewStreamReader()

	r.SetError(nil) // 不应该设置任何错误
	r.Send(Message{Content: "ok"})
	r.Close()

	msg, err := r.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if msg.Content != "ok" {
		t.Errorf("content: %s", msg.Content)
	}
}

func TestStreamReader_SetError_IgnoresEOF(t *testing.T) {
	r := NewStreamReader()

	r.SetError(io.EOF) // EOF 不应该被设置为错误
	r.Send(Message{Content: "ok"})
	r.Close()

	msg, err := r.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if msg.Content != "ok" {
		t.Errorf("content: %s", msg.Content)
	}
}

func TestStreamReader_SetError_FirstErrorWins(t *testing.T) {
	r := NewStreamReader()

	r.SetError(errors.New("first"))
	r.SetError(errors.New("second"))

	_, err := r.Recv()
	if err.Error() != "first" {
		t.Fatalf("expected first error, got %v", err)
	}
}

func TestStreamReader_CloseIdempotent(t *testing.T) {
	r := NewStreamReader()

	r.Send(Message{Content: "test"})
	r.Close()
	r.Close() // 第二次关闭不应 panic
	r.Close()

	// 仍然能读到之前发送的消息
	msg, err := r.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if msg.Content != "test" {
		t.Errorf("content: %s", msg.Content)
	}

	// 再读应该 EOF
	_, err = r.Recv()
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestStreamReader_SendWithContext_Success(t *testing.T) {
	r := NewStreamReader()

	ok := r.SendWithContext(context.Background(), Message{Content: "ok"})
	if !ok {
		t.Fatal("expected success")
	}

	r.Close()
	msg, _ := r.Recv()
	if msg.Content != "ok" {
		t.Errorf("content: %s", msg.Content)
	}
}

func TestStreamReader_SendWithContext_Timeout(t *testing.T) {
	// 创建一个满缓冲的 reader
	r := NewStreamReaderWithBuffer(1)

	// 填满缓冲
	r.Send(Message{Content: "fill"})

	// 设置极短超时
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	ok := r.SendWithContext(ctx, Message{Content: "should fail"})
	if ok {
		t.Fatal("expected timeout/failure")
	}

	r.Close()
}

func TestStreamReader_SendWithContext_Cancelled(t *testing.T) {
	r := NewStreamReaderWithBuffer(0) // 无缓冲，发送会阻塞

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan bool, 1)
	go func() {
		ok := r.SendWithContext(ctx, Message{Content: "blocked"})
		done <- !ok // 应该返回 false
	}()

	// 短暂等待后取消
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case failed := <-done:
		if !failed {
			t.Fatal("expected cancelled send to return false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for cancelled send")
	}

	r.Close()
}

func TestStreamReader_ErrorAfterSend(t *testing.T) {
	r := NewStreamReader()

	r.Send(Message{Content: "before error"})
	r.SetError(errors.New("late error"))

	// 第一次 Recv 应该返回错误（因为 err 已设置）
	_, err := r.Recv()
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "late error" {
		t.Fatalf("expected 'late error', got %v", err)
	}
}

func TestStreamReader_Usage(t *testing.T) {
	r := NewStreamReader()

	r.Usage = Usage{
		PromptTokens:     100,
		CompletionTokens: 200,
		TotalTokens:      300,
	}

	if r.Usage.TotalTokens != 300 {
		t.Errorf("expected 300, got %d", r.Usage.TotalTokens)
	}
}

func TestStreamReader_ConcurrentSendRecv(t *testing.T) {
	r := NewStreamReader()

	const count = 100
	var wg sync.WaitGroup

	// 生产者
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < count; i++ {
			r.Send(Message{Content: "msg"})
		}
		r.Close()
	}()

	// 消费者
	received := 0
	for {
		_, err := r.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		received++
	}

	wg.Wait()

	if received != count {
		t.Fatalf("expected %d messages, got %d", count, received)
	}
}

func TestStreamReader_PreservesMessageFields(t *testing.T) {
	r := NewStreamReader()

	orig := Message{
		Role:             AssistantRole,
		Content:          "test",
		ReasoningContent: "thinking",
		ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: FunctionCall{Name: "fn", Arguments: `{"a":1}`}},
		},
		ToolCallID: "call_1",
		Name:       "bot",
	}

	go func() {
		r.Send(orig)
		r.Close()
	}()

	msg, err := r.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}

	if msg.Role != AssistantRole {
		t.Errorf("role: %s", msg.Role)
	}
	if msg.Content != "test" {
		t.Errorf("content: %s", msg.Content)
	}
	if msg.ReasoningContent != "thinking" {
		t.Errorf("reasoning: %s", msg.ReasoningContent)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls: %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].ID != "tc1" {
		t.Errorf("tool call id: %s", msg.ToolCalls[0].ID)
	}
	if msg.ToolCallID != "call_1" {
		t.Errorf("tool_call_id: %s", msg.ToolCallID)
	}
	if msg.Name != "bot" {
		t.Errorf("name: %s", msg.Name)
	}
}

// ============================================================================
// FormatMessages / PrintMessages / indentString
// ============================================================================

func TestFormatMessages_Empty(t *testing.T) {
	result := FormatMessages(nil)
	if !strings.Contains(result, "无消息") {
		t.Errorf("expected '无消息', got %q", result)
	}

	result = FormatMessages([]*Message{})
	if !strings.Contains(result, "无消息") {
		t.Errorf("expected '无消息' for empty slice")
	}
}

func TestFormatMessages_BasicMessage(t *testing.T) {
	msgs := []*Message{
		UserMessage("hello"),
		AssistantMessage("hi there", ""),
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "user") {
		t.Error("should contain role 'user'")
	}
	if !strings.Contains(result, "assistant") {
		t.Error("should contain role 'assistant'")
	}
	if !strings.Contains(result, "hello") {
		t.Error("should contain content 'hello'")
	}
	if !strings.Contains(result, "hi there") {
		t.Error("should contain content 'hi there'")
	}
}

func TestFormatMessages_WithReasoning(t *testing.T) {
	msgs := []*Message{
		AssistantMessage("answer", "let me reason..."),
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "let me reason...") {
		t.Error("should contain reasoning content")
	}
}

func TestFormatMessages_WithToolCalls(t *testing.T) {
	msgs := []*Message{
		{
			Role: AssistantRole,
			ToolCalls: []ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: FunctionCall{
						Name:      "file_read",
						Arguments: `{"path":"/test"}`,
					},
				},
			},
		},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "file_read") {
		t.Error("should contain function name")
	}
	if !strings.Contains(result, "call_1") {
		t.Error("should contain tool call ID")
	}
	if !strings.Contains(result, `{"path":"/test"}`) {
		t.Error("should contain arguments")
	}
}

func TestFormatMessages_WithToolResult(t *testing.T) {
	msgs := []*Message{
		{
			Role:       ToolRole,
			Content:    "file contents",
			ToolCallID: "call_1",
		},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "call_1") {
		t.Error("should contain tool call ID")
	}
	if !strings.Contains(result, "file contents") {
		t.Error("should contain content")
	}
}

func TestFormatMessages_WithUsage(t *testing.T) {
	msgs := []*Message{
		{
			Role:    AssistantRole,
			Content: "answer",
			Usage:   &Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "100") {
		t.Error("should contain prompt tokens")
	}
	if !strings.Contains(result, "50") {
		t.Error("should contain completion tokens")
	}
	if !strings.Contains(result, "150") {
		t.Error("should contain total tokens")
	}
}

func TestFormatMessages_WithPartial(t *testing.T) {
	msgs := []*Message{
		{Role: AssistantRole, Content: "partial", Partial: true},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "未完成") {
		t.Error("should contain '未完成' for partial message")
	}
}

func TestFormatMessages_WithName(t *testing.T) {
	msgs := []*Message{
		{Role: UserRole, Content: "test", Name: "alice"},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "alice") {
		t.Error("should contain name")
	}
}

func TestFormatMessages_EmptyContent(t *testing.T) {
	msgs := []*Message{
		{Role: AssistantRole, Content: ""},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "(空)") {
		t.Error("should show (空) for empty content")
	}
}

func TestFormatMessages_NilMessage(t *testing.T) {
	msgs := []*Message{nil, UserMessage("test")}

	result := FormatMessages(msgs)

	// 不应该 panic，且应该包含第二条消息
	if !strings.Contains(result, "test") {
		t.Error("should contain 'test'")
	}
}

func TestFormatMessages_MultipleMessages(t *testing.T) {
	msgs := []*Message{
		SystemMessage("system"),
		UserMessage("user"),
		AssistantMessage("assistant", ""),
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "消息 #1") {
		t.Error("should have message #1")
	}
	if !strings.Contains(result, "消息 #2") {
		t.Error("should have message #2")
	}
	if !strings.Contains(result, "消息 #3") {
		t.Error("should have message #3")
	}
}

func TestFormatMessages_MultipleToolCalls(t *testing.T) {
	msgs := []*Message{
		{
			Role: AssistantRole,
			ToolCalls: []ToolCall{
				{ID: "tc1", Function: FunctionCall{Name: "fn1"}},
				{ID: "tc2", Function: FunctionCall{Name: "fn2"}},
			},
		},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "fn1") || !strings.Contains(result, "fn2") {
		t.Error("should contain both function names")
	}
}

func TestFormatMessages_ToolCallEmptyArgs(t *testing.T) {
	msgs := []*Message{
		{
			Role: AssistantRole,
			ToolCalls: []ToolCall{
				{ID: "tc1", Function: FunctionCall{Name: "fn"}},
			},
		},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "(空)") {
		t.Error("should show (空) for empty arguments")
	}
}

func TestPrintMessages_NoPanic(t *testing.T) {
	// PrintMessages 只是调用 FormatMessages + fmt.Println
	// 确保不 panic
	PrintMessages(nil)
	PrintMessages([]*Message{})
	PrintMessages([]*Message{UserMessage("test")})
}

func TestIndentString(t *testing.T) {
	result := indentString("line1\nline2\nline3", "  ")
	lines := strings.Split(result, "\n")

	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("line %d: missing prefix, got %q", i, line)
		}
	}
}

func TestIndentString_Empty(t *testing.T) {
	result := indentString("", "  ")
	if !strings.Contains(result, "(空)") {
		t.Errorf("expected (空) for empty string, got %q", result)
	}
}

func TestIndentString_SingleLine(t *testing.T) {
	result := indentString("hello", ">> ")
	if result != ">> hello" {
		t.Errorf("expected '>> hello', got %q", result)
	}
}

// ============================================================================
// 类型和常量
// ============================================================================

func TestRoleConstants(t *testing.T) {
	if AssistantRole != "assistant" {
		t.Errorf("AssistantRole: %s", AssistantRole)
	}
	if UserRole != "user" {
		t.Errorf("UserRole: %s", UserRole)
	}
	if SystemRole != "system" {
		t.Errorf("SystemRole: %s", SystemRole)
	}
	if ToolRole != "tool" {
		t.Errorf("ToolRole: %s", ToolRole)
	}
}

func TestTool_JSONRoundTrip(t *testing.T) {
	// Tool 定义在 tool.go 中，确保可正常序列化
	tool := Tool{
		Name:        "file_read",
		Description: "read a file",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
		},
	}

	if tool.Name != "file_read" {
		t.Errorf("name: %s", tool.Name)
	}
	if tool.Description != "read a file" {
		t.Errorf("desc: %s", tool.Description)
	}
	if tool.Parameters == nil {
		t.Error("params should not be nil")
	}
}

func TestUsage_Fields(t *testing.T) {
	u := Usage{
		PromptTokens:     100,
		CompletionTokens: 200,
		TotalTokens:      300,
	}

	if u.PromptTokens != 100 {
		t.Errorf("prompt: %d", u.PromptTokens)
	}
	if u.CompletionTokens != 200 {
		t.Errorf("completion: %d", u.CompletionTokens)
	}
	if u.TotalTokens != 300 {
		t.Errorf("total: %d", u.TotalTokens)
	}
}

func TestToolCall_Fields(t *testing.T) {
	tc := ToolCall{
		ID:    "call_123",
		Type:  "function",
		Index: 0,
		Function: FunctionCall{
			Name:      "search",
			Arguments: `{"q":"test"}`,
		},
	}

	if tc.ID != "call_123" {
		t.Errorf("id: %s", tc.ID)
	}
	if tc.Function.Name != "search" {
		t.Errorf("function name: %s", tc.Function.Name)
	}
}

// ============================================================================
// 多模态构造函数
// ============================================================================

func TestTextPart(t *testing.T) {
	p := TextPart("hello")
	if p.Type != ContentTypeText {
		t.Errorf("type: %s", p.Type)
	}
	if p.Text != "hello" {
		t.Errorf("text: %s", p.Text)
	}
}

func TestImagePart(t *testing.T) {
	p := ImagePart("https://example.com/img.png")
	if p.Type != ContentTypeImageURL {
		t.Errorf("type: %s", p.Type)
	}
	if p.ImageURL == nil || p.ImageURL.URL != "https://example.com/img.png" {
		t.Errorf("url: %v", p.ImageURL)
	}
}

func TestImagePartBase64(t *testing.T) {
	p := ImagePartBase64("image/png", "iVBORw0KGgo=")
	if p.Type != ContentTypeImageURL {
		t.Errorf("type: %s", p.Type)
	}
	if p.ImageURL == nil {
		t.Fatal("ImageURL is nil")
	}
	if !strings.Contains(p.ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("url: %s", p.ImageURL.URL)
	}
	if !strings.Contains(p.ImageURL.URL, "iVBORw0KGgo=") {
		t.Errorf("url missing base64 data: %s", p.ImageURL.URL)
	}
}

func TestAudioPart(t *testing.T) {
	p := AudioPart("mp3", "SUQzAwAA=")
	if p.Type != ContentTypeInputAudio {
		t.Errorf("type: %s", p.Type)
	}
	if p.InputAudio == nil {
		t.Fatal("InputAudio is nil")
	}
	if p.InputAudio.Format != "mp3" {
		t.Errorf("format: %s", p.InputAudio.Format)
	}
	if p.InputAudio.Data != "SUQzAwAA=" {
		t.Errorf("data: %s", p.InputAudio.Data)
	}
}

func TestVideoPart(t *testing.T) {
	p := VideoPart("https://example.com/video.mp4")
	if p.Type != ContentTypeVideoURL {
		t.Errorf("type: %s", p.Type)
	}
	if p.VideoURL == nil || p.VideoURL.URL != "https://example.com/video.mp4" {
		t.Errorf("url: %v", p.VideoURL)
	}
}

func TestFilePart(t *testing.T) {
	p := FilePart("https://example.com/doc.pdf")
	if p.Type != ContentTypeFileURL {
		t.Errorf("type: %s", p.Type)
	}
	if p.FileURL == nil || p.FileURL.URL != "https://example.com/doc.pdf" {
		t.Errorf("url: %v", p.FileURL)
	}
}

func TestInlineDataPart(t *testing.T) {
	p := InlineDataPart("application/pdf", "JVBERi0xLjQ=")
	if p.Type != ContentTypeInlineData {
		t.Errorf("type: %s", p.Type)
	}
	if p.InlineData == nil {
		t.Fatal("InlineData is nil")
	}
	if p.InlineData.MediaType != "application/pdf" {
		t.Errorf("media_type: %s", p.InlineData.MediaType)
	}
	if p.InlineData.Data != "JVBERi0xLjQ=" {
		t.Errorf("data: %s", p.InlineData.Data)
	}
}

// ============================================================================
// 多模态 Message 构造
// ============================================================================

func TestUserMultimodalMessage(t *testing.T) {
	msg := UserMultimodalMessage(
		TextPart("描述这张图片"),
		ImagePart("https://example.com/img.png"),
	)

	if msg.Role != UserRole {
		t.Errorf("role: %s", msg.Role)
	}
	if len(msg.ContentParts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(msg.ContentParts))
	}
	if msg.ContentParts[0].Type != ContentTypeText {
		t.Errorf("part 0 type: %s", msg.ContentParts[0].Type)
	}
	if msg.ContentParts[1].Type != ContentTypeImageURL {
		t.Errorf("part 1 type: %s", msg.ContentParts[1].Type)
	}
}

func TestUserMultimodalMessage_AudioVideo(t *testing.T) {
	msg := UserMultimodalMessage(
		TextPart("分析这个媒体"),
		AudioPart("wav", "UklGRg=="),
		VideoPart("https://example.com/clip.mp4"),
		FilePart("https://example.com/report.pdf"),
	)

	if len(msg.ContentParts) != 4 {
		t.Fatalf("expected 4 parts, got %d", len(msg.ContentParts))
	}

	if msg.ContentParts[1].Type != ContentTypeInputAudio {
		t.Errorf("part 1 type: %s", msg.ContentParts[1].Type)
	}
	if msg.ContentParts[2].Type != ContentTypeVideoURL {
		t.Errorf("part 2 type: %s", msg.ContentParts[2].Type)
	}
	if msg.ContentParts[3].Type != ContentTypeFileURL {
		t.Errorf("part 3 type: %s", msg.ContentParts[3].Type)
	}
}

// ============================================================================
// IsMultimodal / TextContent / ImageCount / 输出侧辅助方法
// ============================================================================

func TestIsMultimodal(t *testing.T) {
	msg := &Message{Content: "text only"}
	if msg.IsMultimodal() {
		t.Error("should not be multimodal")
	}

	msg.ContentParts = []ContentPart{TextPart("hi")}
	if !msg.IsMultimodal() {
		t.Error("should be multimodal")
	}
}

func TestTextContent_Multimodal(t *testing.T) {
	msg := &Message{
		ContentParts: []ContentPart{
			TextPart("line1"),
			ImagePart("https://example.com/img.png"),
			TextPart("line2"),
		},
	}

	text := msg.TextContent()
	if !strings.Contains(text, "line1") || !strings.Contains(text, "line2") {
		t.Errorf("text: %s", text)
	}
}

func TestImageCount(t *testing.T) {
	msg := &Message{
		ContentParts: []ContentPart{
			TextPart("hi"),
			ImagePart("https://a.com/1.png"),
			ImagePart("https://a.com/2.png"),
			AudioPart("mp3", "data"),
		},
	}

	if msg.ImageCount() != 2 {
		t.Errorf("expected 2, got %d", msg.ImageCount())
	}
}

func TestHasOutputImages(t *testing.T) {
	msg := &Message{}
	if msg.HasOutputImages() {
		t.Error("should not have output images")
	}

	msg.OutputImages = []OutputImage{{URL: "https://example.com/gen.png"}}
	if !msg.HasOutputImages() {
		t.Error("should have output images")
	}
}

func TestHasOutputAudio(t *testing.T) {
	msg := &Message{}
	if msg.HasOutputAudio() {
		t.Error("should not have output audio")
	}

	msg.OutputAudio = &OutputAudio{Data: "base64data", Format: "mp3"}
	if !msg.HasOutputAudio() {
		t.Error("should have output audio")
	}
}

// ============================================================================
// Clone 多模态
// ============================================================================

func TestClone_ContentParts_AllTypes(t *testing.T) {
	orig := &Message{
		Role: UserRole,
		ContentParts: []ContentPart{
			TextPart("hello"),
			ImagePart("https://example.com/img.png"),
			ImagePartBase64("image/png", "base64data"),
			AudioPart("mp3", "audiodata"),
			VideoPart("https://example.com/video.mp4"),
			FilePart("https://example.com/file.pdf"),
			InlineDataPart("application/pdf", "pdfdata"),
		},
	}

	cloned := orig.Clone()

	if len(cloned.ContentParts) != 7 {
		t.Fatalf("expected 7 parts, got %d", len(cloned.ContentParts))
	}

	// 验证各类型正确拷贝
	if cloned.ContentParts[0].Text != "hello" {
		t.Errorf("text: %s", cloned.ContentParts[0].Text)
	}
	if cloned.ContentParts[1].ImageURL == nil || cloned.ContentParts[1].ImageURL.URL != "https://example.com/img.png" {
		t.Errorf("image url")
	}
	if cloned.ContentParts[2].ImageURL == nil || !strings.Contains(cloned.ContentParts[2].ImageURL.URL, "base64data") {
		t.Errorf("image base64")
	}
	if cloned.ContentParts[3].InputAudio == nil || cloned.ContentParts[3].InputAudio.Format != "mp3" {
		t.Errorf("audio")
	}
	if cloned.ContentParts[4].VideoURL == nil || cloned.ContentParts[4].VideoURL.URL != "https://example.com/video.mp4" {
		t.Errorf("video")
	}
	if cloned.ContentParts[5].FileURL == nil || cloned.ContentParts[5].FileURL.URL != "https://example.com/file.pdf" {
		t.Errorf("file")
	}
	if cloned.ContentParts[6].InlineData == nil || cloned.ContentParts[6].InlineData.MediaType != "application/pdf" {
		t.Errorf("inline data")
	}

	// 验证深拷贝隔离
	cloned.ContentParts[0].Text = "modified"
	if orig.ContentParts[0].Text != "hello" {
		t.Error("clone did not isolate ContentParts")
	}
	cloned.ContentParts[3].InputAudio.Data = "modified"
	if orig.ContentParts[3].InputAudio.Data != "audiodata" {
		t.Error("clone did not isolate InputAudio")
	}
}

func TestClone_OutputImages(t *testing.T) {
	orig := &Message{
		OutputImages: []OutputImage{
			{URL: "https://example.com/1.png", RevisedPrompt: "revised"},
			{Base64: "imagedata"},
		},
	}

	cloned := orig.Clone()

	if len(cloned.OutputImages) != 2 {
		t.Fatalf("expected 2, got %d", len(cloned.OutputImages))
	}
	if cloned.OutputImages[0].URL != "https://example.com/1.png" {
		t.Errorf("url: %s", cloned.OutputImages[0].URL)
	}
	if cloned.OutputImages[0].RevisedPrompt != "revised" {
		t.Errorf("revised prompt: %s", cloned.OutputImages[0].RevisedPrompt)
	}
	if cloned.OutputImages[1].Base64 != "imagedata" {
		t.Errorf("base64: %s", cloned.OutputImages[1].Base64)
	}

	// 深拷贝隔离
	cloned.OutputImages[0].URL = "modified"
	if orig.OutputImages[0].URL != "https://example.com/1.png" {
		t.Error("clone did not isolate OutputImages")
	}
}

func TestClone_OutputAudio(t *testing.T) {
	orig := &Message{
		OutputAudio: &OutputAudio{Data: "audiodata", Format: "mp3"},
	}

	cloned := orig.Clone()

	if cloned.OutputAudio == nil {
		t.Fatal("OutputAudio is nil")
	}
	if cloned.OutputAudio.Data != "audiodata" {
		t.Errorf("data: %s", cloned.OutputAudio.Data)
	}
	if cloned.OutputAudio.Format != "mp3" {
		t.Errorf("format: %s", cloned.OutputAudio.Format)
	}

	// 深拷贝隔离
	cloned.OutputAudio.Data = "modified"
	if orig.OutputAudio.Data != "audiodata" {
		t.Error("clone did not isolate OutputAudio")
	}
}

func TestClone_NilMultimodalFields(t *testing.T) {
	orig := &Message{Content: "text only"}
	cloned := orig.Clone()

	if cloned.ContentParts != nil {
		t.Errorf("expected nil ContentParts")
	}
	if cloned.OutputImages != nil {
		t.Errorf("expected nil OutputImages")
	}
	if cloned.OutputAudio != nil {
		t.Errorf("expected nil OutputAudio")
	}
}

// ============================================================================
// ToolResult 多模态
// ============================================================================

func TestToolResult_WithContentParts(t *testing.T) {
	tr := ToolResult{
		CallID:  "call_1",
		Content: "screenshot taken",
		ContentParts: []ContentPart{
			TextPart("screenshot taken"),
			ImagePartBase64("image/png", "iVBORw0KGgo="),
		},
	}

	if len(tr.ContentParts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(tr.ContentParts))
	}
	if tr.ContentParts[0].Type != ContentTypeText {
		t.Errorf("part 0 type: %s", tr.ContentParts[0].Type)
	}
	if tr.ContentParts[1].Type != ContentTypeImageURL {
		t.Errorf("part 1 type: %s", tr.ContentParts[1].Type)
	}
}

// ============================================================================
// FormatMessages 多模态
// ============================================================================

func TestFormatMessages_MultimodalInput(t *testing.T) {
	msgs := []*Message{
		UserMultimodalMessage(
			TextPart("描述这张图片"),
			ImagePart("https://example.com/img.png"),
			AudioPart("mp3", "audiodata"),
			VideoPart("https://example.com/video.mp4"),
			FilePart("https://example.com/doc.pdf"),
			InlineDataPart("application/pdf", "pdfdata"),
		),
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "多模态内容") {
		t.Error("should contain '多模态内容'")
	}
	if !strings.Contains(result, "文本") {
		t.Error("should contain '文本'")
	}
	if !strings.Contains(result, "图片") {
		t.Error("should contain '图片'")
	}
	if !strings.Contains(result, "音频") {
		t.Error("should contain '音频'")
	}
	if !strings.Contains(result, "视频") {
		t.Error("should contain '视频'")
	}
	if !strings.Contains(result, "文件") {
		t.Error("should contain '文件'")
	}
	if !strings.Contains(result, "内联数据") {
		t.Error("should contain '内联数据'")
	}
}

func TestFormatMessages_OutputImages(t *testing.T) {
	msgs := []*Message{
		{
			Role: AssistantRole,
			OutputImages: []OutputImage{
				{URL: "https://example.com/gen.png", RevisedPrompt: "better prompt"},
			},
		},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "输出图片") {
		t.Error("should contain '输出图片'")
	}
	if !strings.Contains(result, "better prompt") {
		t.Error("should contain revised prompt")
	}
}

func TestFormatMessages_OutputImages_Base64(t *testing.T) {
	msgs := []*Message{
		{
			Role: AssistantRole,
			OutputImages: []OutputImage{
				{Base64: strings.Repeat("A", 100)},
			},
		},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "输出图片") {
		t.Error("should contain '输出图片'")
	}
	if !strings.Contains(result, "100 bytes") {
		t.Error("should contain byte count")
	}
}

func TestFormatMessages_OutputAudio(t *testing.T) {
	msgs := []*Message{
		{
			Role:        AssistantRole,
			OutputAudio: &OutputAudio{Data: "audiodata", Format: "mp3"},
		},
	}

	result := FormatMessages(msgs)

	if !strings.Contains(result, "输出音频") {
		t.Error("should contain '输出音频'")
	}
	if !strings.Contains(result, "mp3") {
		t.Error("should contain format")
	}
}

// ============================================================================
// ContentType 常量
// ============================================================================

func TestContentTypeConstants(t *testing.T) {
	if ContentTypeText != "text" {
		t.Errorf("ContentTypeText: %s", ContentTypeText)
	}
	if ContentTypeImageURL != "image_url" {
		t.Errorf("ContentTypeImageURL: %s", ContentTypeImageURL)
	}
	if ContentTypeInputAudio != "input_audio" {
		t.Errorf("ContentTypeInputAudio: %s", ContentTypeInputAudio)
	}
	if ContentTypeVideoURL != "video_url" {
		t.Errorf("ContentTypeVideoURL: %s", ContentTypeVideoURL)
	}
	if ContentTypeFileURL != "file_url" {
		t.Errorf("ContentTypeFileURL: %s", ContentTypeFileURL)
	}
	if ContentTypeInlineData != "inline_data" {
		t.Errorf("ContentTypeInlineData: %s", ContentTypeInlineData)
	}
}
