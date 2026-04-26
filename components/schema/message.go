package schema

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type RoleType string

const (
	AssistantRole RoleType = "assistant"
	UserRole      RoleType = "user"
	SystemRole    RoleType = "system"
	ToolRole      RoleType = "tool"
)

type Message struct {
	Role             RoleType `json:"role"`
	Content          string   `json:"content"`
	ReasoningContent string   `json:"reasoning_content,omitempty"`
	// 消息发送者的名称（可选）
	Name string `json:"name,omitempty"`
	// 当设置为 true 时，表示这条消息是未完成的，模型需要继续生成这条消息的剩余内容。（可选）
	Partial   bool       `json:"partial,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	ToolResults []ToolResult `json:"tool_results,omitempty"`
	Usage       *Usage       `json:"-"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Index    int          `json:"index,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolResult 工具执行结果
type ToolResult struct {
	CallID  string `json:"call_id"`  // 对应 ToolCall.ID
	Content string `json:"content"`  // 结果内容（JSON字符串或纯文本）
	IsError bool   `json:"is_error"` // 是否错误（Claude支持）
}

// Clone 深拷贝
func (m *Message) Clone() Message {
	cloned := Message{
		Role:             m.Role,
		Content:          m.Content,
		ReasoningContent: m.ReasoningContent,
		Name:             m.Name,
		Partial:          m.Partial,
	}

	// 深拷贝切片
	if m.ToolCalls != nil {
		cloned.ToolCalls = make([]ToolCall, len(m.ToolCalls))
		copy(cloned.ToolCalls, m.ToolCalls)
	}

	// 深拷贝切片
	if m.ToolResults != nil {
		cloned.ToolResults = make([]ToolResult, len(m.ToolResults))
		copy(cloned.ToolResults, m.ToolResults)
	}

	// 深拷贝指针
	if m.Usage != nil {
		cloned.Usage = &Usage{
			PromptTokens: m.Usage.PromptTokens,
			Completion:   m.Usage.Completion,
			TotalTokens:  m.Usage.TotalTokens,
		}
	}

	return cloned
}

// SystemMessage 返回一个role为system的信息
func SystemMessage(content, reasoningContent string) *Message {
	return &Message{
		Role:             SystemRole,
		Content:          content,
		ReasoningContent: reasoningContent,
	}
}

// UserMessage 返回一个role为user的信息
func UserMessage(content string) *Message {
	return &Message{
		Role:    UserRole,
		Content: content,
	}
}

// AssistantMessage 返回一个role为user的信息
func AssistantMessage(content string) *Message {
	return &Message{
		Role:    AssistantRole,
		Content: content,
	}
}

// ToolMessage 返回一个role为tool的信息
func ToolMessage(content string) *Message {
	return &Message{
		Role:    ToolRole,
		Content: content,
	}
}

// StreamReader 流式消息读取器
type StreamReader struct {
	streamChan chan Message
	closeOnce  sync.Once
	err        error      // 存储流错误
	errMu      sync.Mutex // 错误保护
	Usage      Usage
}

// NewStreamReader 创建默认带缓冲的流读取器
func NewStreamReader() *StreamReader {
	return NewStreamReaderWithBuffer(16)
}

// NewStreamReaderWithBuffer 带缓冲大小
func NewStreamReaderWithBuffer(bufSize int) *StreamReader {
	return &StreamReader{
		streamChan: make(chan Message, bufSize),
	}
}

// setError 内部设置错误
func (sr *StreamReader) setError(err error) {
	if err == nil || err == io.EOF {
		return
	}
	sr.errMu.Lock()
	defer sr.errMu.Unlock()
	if sr.err == nil {
		sr.err = err
	}
}

// Close 安全关闭
func (sr *StreamReader) Close() {
	sr.closeOnce.Do(func() {
		close(sr.streamChan)
	})
}

// Recv 从stream流中接收一个值。
//
//	for {
//		msg, err := reader.Recv()
//		if errors.Is(err, io.EOF){
//			break
//		}
//		print(msg.Content)
//	}
//
// Recv 从流中接收一条消息，符合 Go 标准流式读取风格
func (sr *StreamReader) Recv() (*Message, error) {
	sr.errMu.Lock()
	err := sr.err
	sr.errMu.Unlock()

	if err != nil {
		return nil, err
	}

	msg, ok := <-sr.streamChan
	if !ok {
		return nil, io.EOF
	}
	return &msg, nil
}

// StreamResponse 流式响应最外层
type StreamResponse struct {
	Choices []Choice `json:"choices"`
}

type Choice struct {
	Index        int    `json:"index"`
	Delta        Delta  `json:"delta"`
	FinishReason string `json:"finish_reason"`
	Usage        *Usage `json:"usage,omitempty"`
}

type Delta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type Usage struct {
	PromptTokens uint64 `json:"prompt_tokens"`
	Completion   uint64 `json:"completion"`
	TotalTokens  uint64 `json:"total_tokens"`
}

// StreamReception 流式接收并返回一个 StreamReader 用于读取流式数据
func StreamReception(resp *http.Response) (*StreamReader, error) {
	reader := NewStreamReader()

	go func() {
		defer func() {
			_ = resp.Body.Close()
			reader.Close()
		}()

		const maxBufferSize = 1 << 20
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, maxBufferSize), maxBufferSize)

		var msg Message
		var streamResp StreamResponse

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				return
			}

			streamResp = StreamResponse{}
			// 解析JSON
			if err := json.Unmarshal([]byte(data), &streamResp); err != nil {
				continue
			}

			if len(streamResp.Choices) == 0 {
				continue
			}
			choice := streamResp.Choices[0]

			// 设置角色（第一条有效）
			if choice.Delta.Role != "" {
				msg.Role = RoleType(choice.Delta.Role)

			}

			if choice.Delta.Content != "" {
				msg.Content = choice.Delta.Content
			}

			if choice.Delta.ReasoningContent != "" {
				msg.ReasoningContent = choice.Delta.ReasoningContent
			}

			if len(choice.Delta.ToolCalls) > 0 {
				for _, tc := range choice.Delta.ToolCalls {
					idx := tc.Index
					for len(msg.ToolCalls) <= idx {
						msg.ToolCalls = append(msg.ToolCalls, ToolCall{})
					}
					if tc.Function.Arguments != "" {
						msg.ToolCalls[idx].Function.Arguments += tc.Function.Arguments
					}
					if tc.ID != "" {
						msg.ToolCalls[idx].ID = tc.ID
					}
					if tc.Type != "" {
						msg.ToolCalls[idx].Type = tc.Type
					}
					if tc.Function.Name != "" {
						msg.ToolCalls[idx].Function.Name = tc.Function.Name
					}
				}
			}

			// 安全赋值 usage
			if streamResp.Choices[0].Usage != nil {
				reader.Usage = *streamResp.Choices[0].Usage
			}

			// 发送到通道
			reader.streamChan <- msg.Clone()
		}
	}()

	return reader, nil
}

// Multicast 多播：1个源流 → N个独立流
// 生产级实现：不丢数据、错误传播、并发安全、带超时背压
func (sr *StreamReader) Multicast(n uint) []*StreamReader {
	if n <= 0 {
		return nil
	}

	// 1. 创建 N 个消费者
	readers := make([]*StreamReader, n)
	for i := range readers {
		readers[i] = NewStreamReaderWithBuffer(16)
	}

	// 2. 后台转发协程（核心）
	go func() {
		// 退出时统一关闭所有子流
		defer func() {
			for _, r := range readers {
				r.Usage = sr.Usage
				r.Close()
			}
		}()

		for {
			// 3. 从源流读
			msg, err := sr.Recv()
			if err != nil {
				// 错误传播给所有子节点
				for _, r := range readers {
					r.setError(err)
				}
				return
			}

			// 4. 深拷贝 N 份
			msgs := make([]Message, n)
			for i := range msgs {
				msgs[i] = msg.Clone()
			}

			// 5. 同步发送：全部送达 or 超时失败
			ok := sendToAll(readers, msgs, 5*time.Second)
			if !ok {
				return
			}
		}
	}()

	return readers
}

// sendToAll 向所有子流发送消息（带超时，不丢数据）
func sendToAll(readers []*StreamReader, msgs []Message, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	n := len(readers)
	results := make(chan struct {
		idx int
		ok  bool
	}, n)

	var wg sync.WaitGroup
	wg.Add(n)

	for i := range readers {
		go func(idx int) {
			defer wg.Done()
			select {
			case readers[idx].streamChan <- msgs[idx]:
				results <- struct {
					idx int
					ok  bool
				}{idx, true}
			case <-ctx.Done():
				results <- struct {
					idx int
					ok  bool
				}{idx, false}
			}
		}(i)
	}

	// 安全关闭 result channel
	go func() {
		wg.Wait()
		close(results)
	}()

	success := make([]bool, n)
	for res := range results {
		success[res.idx] = res.ok
	}

	for _, ok := range success {
		if !ok {
			return false
		}
	}
	return true
}

// FormatMessages 标准化格式化 []*Message 为可读字符串
// 返回格式清晰的结构化文本，包含所有字段的详细展示
func FormatMessages(messages []*Message) string {
	if len(messages) == 0 {
		return "📭 无消息"
	}

	var builder strings.Builder
	separator := "────────────────────────────────────────────────────────────────"

	for i, msg := range messages {
		if msg == nil {
			continue
		}

		// 消息头部
		builder.WriteString(fmt.Sprintf("%s\n", separator))
		builder.WriteString(fmt.Sprintf("📨 消息 #%d\n", i+1))
		builder.WriteString(fmt.Sprintf("%s\n", separator))

		// 基础信息（角色、名称、是否未完成）
		builder.WriteString(fmt.Sprintf("🎭 角色: %s", msg.Role))
		if msg.Name != "" {
			builder.WriteString(fmt.Sprintf(" | 🏷️ 名称: %s", msg.Name))
		}
		if msg.Partial {
			builder.WriteString(" | ⏳ [未完成]")
		}
		builder.WriteString("\n")

		// 消息内容
		content := msg.Content
		if content == "" {
			content = "(空)"
		}
		builder.WriteString(fmt.Sprintf("📝 内容:\n%s\n", indentString(content, "  ")))

		// 思考内容
		if msg.ReasoningContent != "" {
			builder.WriteString(fmt.Sprintf("💭 思考内容:\n%s\n", indentString(msg.ReasoningContent, "  ")))
		}

		// 工具调用
		if len(msg.ToolCalls) > 0 {
			builder.WriteString("🔧 工具调用:\n")
			for j, tc := range msg.ToolCalls {
				builder.WriteString(fmt.Sprintf("  #%d\n", j+1))
				builder.WriteString(fmt.Sprintf("    🆔 ID: %s\n", tc.ID))
				builder.WriteString(fmt.Sprintf("    📌 类型: %s\n", tc.Type))
				builder.WriteString(fmt.Sprintf("    📦 函数: %s\n", tc.Function.Name))
				args := tc.Function.Arguments
				if args == "" {
					args = "(空)"
				}
				builder.WriteString(fmt.Sprintf("    📋 参数:\n%s\n", indentString(args, "      ")))
			}
		}

		// 工具结果
		if len(msg.ToolResults) > 0 {
			builder.WriteString("📊 工具结果:\n")
			for j, tr := range msg.ToolResults {
				builder.WriteString(fmt.Sprintf("  #%d\n", j+1))
				builder.WriteString(fmt.Sprintf("    🔗 调用ID: %s\n", tr.CallID))
				errStatus := "❌ 是"
				if !tr.IsError {
					errStatus = "✅ 否"
				}
				builder.WriteString(fmt.Sprintf("    ⚠️ 错误: %s\n", errStatus))
				content := tr.Content
				if content == "" {
					content = "(空)"
				}
				builder.WriteString(fmt.Sprintf("    📄 内容:\n%s\n", indentString(content, "      ")))
			}
		}

		// Token 使用情况
		if msg.Usage != nil {
			builder.WriteString(fmt.Sprintf("💰 Token 使用: 提示=%d, 完成=%d, 总计=%d\n",
				msg.Usage.PromptTokens, msg.Usage.Completion, msg.Usage.TotalTokens))
		}

		builder.WriteString("\n")
	}

	builder.WriteString(separator)
	return builder.String()
}

// PrintMessages 标准化打印 []*Message
// 直接将格式化后的消息打印到控制台
func PrintMessages(messages []*Message) {
	fmt.Println(FormatMessages(messages))
}

// indentString 为多行字符串的每一行添加指定前缀（用于缩进）
func indentString(s, prefix string) string {
	if s == "" {
		return prefix + "(空)"
	}
	lines := strings.Split(s, "\n")
	var indented strings.Builder
	for _, line := range lines {
		indented.WriteString(fmt.Sprintf("%s%s\n", prefix, line))
	}
	return strings.TrimSuffix(indented.String(), "\n")
}
