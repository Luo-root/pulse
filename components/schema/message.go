package schema

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

type RoleType string

const (
	AssistantRole RoleType = "assistant"
	UserRole      RoleType = "user"
	SystemRole    RoleType = "system"
	ToolRole      RoleType = "tool"
)

// ============================================================================
// 多模态内容类型
// ============================================================================

// ContentPart 多模态内容片段
type ContentPart struct {
	Type     string    `json:"type"`                // "text" 或 "image_url"
	Text     string    `json:"text,omitempty"`      // type="text"
	ImageURL *ImageURL `json:"image_url,omitempty"` // type="image_url"
}

// ImageURL 图片信息
type ImageURL struct {
	URL    string `json:"url"`              // http(s)://... 或 data:image/png;base64,...
	Detail string `json:"detail,omitempty"` // "low", "high", "auto"
}

// TextPart 创建文本片段
func TextPart(text string) ContentPart {
	return ContentPart{Type: "text", Text: text}
}

// ImagePart 创建图片片段（通过 URL）
func ImagePart(url string) ContentPart {
	return ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: url}}
}

// ImagePartBase64 创建图片片段（通过 base64 数据）
func ImagePartBase64(mediaType, base64Data string) ContentPart {
	return ContentPart{
		Type: "image_url",
		ImageURL: &ImageURL{
			URL: fmt.Sprintf("data:%s;base64,%s", mediaType, base64Data),
		},
	}
}

type Message struct {
	Role             RoleType `json:"role"`
	Content          string   `json:"content,omitempty"`
	ReasoningContent string   `json:"reasoning_content,omitempty"`
	// 消息发送者的名称（可选）
	Name string `json:"name,omitempty"`
	// 当设置为 true 时，表示这条消息是未完成的，模型需要继续生成这条消息的剩余内容。（可选）
	Partial   bool       `json:"partial,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// 多模态内容：当非空时，序列化由 adapter 处理，Content 字段被忽略
	// JSON 序列化标记为 "-"，各 provider adapter 自行转换为对应格式
	ContentParts []ContentPart `json:"-"`

	// tool 消息专用：关联到哪个 ToolCall
	ToolCallID string `json:"tool_call_id,omitempty"`

	Usage *Usage `json:"-"`
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

// ToolResult 工具执行结果（仅用于工具执行层内部传递，不作为 Message 字段）
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

type Usage struct {
	PromptTokens        uint64 `json:"prompt_tokens"`
	CompletionTokens    uint64 `json:"completion_tokens"`
	TotalTokens         uint64 `json:"total_tokens"`
	CachedTokens        uint64 `json:"cached_tokens"`
	PromptTokensDetails struct {
		CachedTokens uint64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// IsMultimodal 是否包含多模态内容
func (m *Message) IsMultimodal() bool {
	return len(m.ContentParts) > 0
}

// TextContent 统一获取纯文本内容
// 优先从 ContentParts 中提取所有文本片段拼接，否则返回 Content
func (m *Message) TextContent() string {
	if len(m.ContentParts) > 0 {
		var parts []string
		for _, p := range m.ContentParts {
			if p.Type == "text" && p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return m.Content
}

// ImageCount 返回消息中的图片数量
func (m *Message) ImageCount() int {
	count := 0
	for _, p := range m.ContentParts {
		if p.Type == "image_url" {
			count++
		}
	}
	return count
}

// Clone 深拷贝
// Clone 深拷贝
func (m *Message) Clone() Message {
	cloned := Message{
		Role:             m.Role,
		Content:          m.Content,
		ReasoningContent: m.ReasoningContent,
		Name:             m.Name,
		Partial:          m.Partial,
		ToolCallID:       m.ToolCallID,
	}

	if m.ToolCalls != nil {
		cloned.ToolCalls = make([]ToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			cloned.ToolCalls[i] = ToolCall{
				ID:    tc.ID,
				Type:  tc.Type,
				Index: tc.Index,
				Function: FunctionCall{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			}
		}
	}

	if m.ContentParts != nil {
		cloned.ContentParts = make([]ContentPart, len(m.ContentParts))
		for i, p := range m.ContentParts {
			cp := ContentPart{Type: p.Type, Text: p.Text}
			if p.ImageURL != nil {
				cp.ImageURL = &ImageURL{
					URL:    p.ImageURL.URL,
					Detail: p.ImageURL.Detail,
				}
			}
			cloned.ContentParts[i] = cp
		}
	}

	if m.Usage != nil {
		cloned.Usage = &Usage{
			PromptTokens:        m.Usage.PromptTokens,
			CompletionTokens:    m.Usage.CompletionTokens,
			TotalTokens:         m.Usage.TotalTokens,
			CachedTokens:        m.Usage.CachedTokens,
			PromptTokensDetails: m.Usage.PromptTokensDetails,
		}
	}

	return cloned
}

// SystemMessage 返回一个role为system的信息
func SystemMessage(content string) *Message {
	return &Message{
		Role:    SystemRole,
		Content: content,
	}
}

// UserMessage 返回一个role为user的信息
func UserMessage(content string) *Message {
	return &Message{
		Role:    UserRole,
		Content: content,
	}
}

// UserMultimodalMessage 创建包含图片/文本混合内容的用户消息
//
//	msg := UserMultimodalMessage(
//	    TextPart("请描述这张图片"),
//	    ImagePartBase64("image/png", screenshotBase64),
//	)
func UserMultimodalMessage(parts ...ContentPart) *Message {
	return &Message{Role: UserRole, ContentParts: parts}
}

// AssistantMessage 返回一个role为assistant的信息
func AssistantMessage(content, reasoningContent string) *Message {
	return &Message{
		Role:             AssistantRole,
		Content:          content,
		ReasoningContent: reasoningContent,
	}
}

// ToolResultsMessage 创建一组 tool 角色的消息，每个 ToolResult 对应一条独立消息
func ToolResultsMessage(results []ToolResult) []*Message {
	msgs := make([]*Message, 0, len(results))
	for _, r := range results {
		content := r.Content
		if r.IsError {
			content = "[Error] " + content
		}
		msgs = append(msgs, &Message{
			Role:       ToolRole,
			Content:    content,
			ToolCallID: r.CallID,
		})
	}
	return msgs
}

// NewToolResult 便捷构造一个结果条目
func NewToolResult(callID, content string, isError bool) ToolResult {
	return ToolResult{
		CallID:  callID,
		Content: content,
		IsError: isError,
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

func (sr *StreamReader) Send(msg Message) {
	sr.streamChan <- msg
}

// SendWithContext 带上下文的发送，可被取消或超时实现超时控制
// 返回 true 表示发送成功，false 表示被取消
func (sr *StreamReader) SendWithContext(ctx context.Context, msg Message) bool {
	select {
	case sr.streamChan <- msg:
		return true
	case <-ctx.Done():
		return false
	}
}

// SetError 内部设置错误
func (sr *StreamReader) SetError(err error) {
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

// FormatMessages 标准化格式化 []*Message 为可读字符串
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

		builder.WriteString(fmt.Sprintf("%s\n", separator))
		builder.WriteString(fmt.Sprintf("📨 消息 #%d\n", i+1))
		builder.WriteString(fmt.Sprintf("%s\n", separator))

		builder.WriteString(fmt.Sprintf("🎭 角色: %s", msg.Role))
		if msg.Name != "" {
			builder.WriteString(fmt.Sprintf(" | 🏷️ 名称: %s", msg.Name))
		}
		if msg.Partial {
			builder.WriteString(" | ⏳ [未完成]")
		}
		builder.WriteString("\n")

		// 多模态内容
		if msg.IsMultimodal() {
			builder.WriteString("📎 多模态内容:\n")
			for j, part := range msg.ContentParts {
				switch part.Type {
				case "text":
					builder.WriteString(fmt.Sprintf("  [%d] 文本: %s\n", j, indentString(part.Text, "      ")))
				case "image_url":
					if part.ImageURL != nil {
						url := part.ImageURL.URL
						if len(url) > 80 {
							url = url[:80] + "..."
						}
						builder.WriteString(fmt.Sprintf("  [%d] 图片: %s", j, url))
						if part.ImageURL.Detail != "" {
							builder.WriteString(fmt.Sprintf(" (detail=%s)", part.ImageURL.Detail))
						}
						builder.WriteString("\n")
					}
				default:
					builder.WriteString(fmt.Sprintf("  [%d] %s\n", j, part.Type))
				}
			}
		} else {
			content := msg.Content
			if content == "" {
				content = "(空)"
			}
			builder.WriteString(fmt.Sprintf("📝 内容:\n%s\n", indentString(content, "  ")))
		}

		if msg.ReasoningContent != "" {
			builder.WriteString(fmt.Sprintf("💭 思考内容:\n%s\n", indentString(msg.ReasoningContent, "  ")))
		}

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

		if msg.ToolCallID != "" {
			builder.WriteString(fmt.Sprintf("🔗 工具调用ID: %s\n", msg.ToolCallID))
		}

		if msg.Usage != nil {
			builder.WriteString(fmt.Sprintf("💰 Token 使用: 提示=%d, 完成=%d, 总计=%d\n",
				msg.Usage.PromptTokens, msg.Usage.CompletionTokens, msg.Usage.TotalTokens))
		}

		builder.WriteString("\n")
	}

	builder.WriteString(separator)
	return builder.String()
}

func PrintMessages(messages []*Message) {
	fmt.Println(FormatMessages(messages))
}

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
