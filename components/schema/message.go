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
	Type       string      `json:"type"`                  // 内容类型，见 ContentType* 常量
	Text       string      `json:"text,omitempty"`        // type="text"
	ImageURL   *ImageURL   `json:"image_url,omitempty"`   // type="image_url"
	InputAudio *InputAudio `json:"input_audio,omitempty"` // type="input_audio"
	VideoURL   *MediaURL   `json:"video_url,omitempty"`   // type="video_url"
	FileURL    *MediaURL   `json:"file_url,omitempty"`    // type="file_url"
	InlineData *InlineData `json:"inline_data,omitempty"` // type="inline_data"
}

// 内容类型常量
const (
	ContentTypeText       = "text"
	ContentTypeImageURL   = "image_url"
	ContentTypeInputAudio = "input_audio"
	ContentTypeVideoURL   = "video_url"
	ContentTypeFileURL    = "file_url"
	ContentTypeInlineData = "inline_data"
)

// ImageURL 图片信息
type ImageURL struct {
	URL    string `json:"url"`              // http(s)://... 或 data:image/png;base64,...
	Detail string `json:"detail,omitempty"` // "low", "high", "auto"
}

// InputAudio 用户输入的音频数据
type InputAudio struct {
	Data   string `json:"data"`   // base64 编码的音频数据
	Format string `json:"format"` // 音频格式："wav", "mp3"
}

// MediaURL 通用媒体资源引用（视频、文件等）
type MediaURL struct {
	URL string `json:"url"` // http(s)://... 或 data:xxx;base64,...
}

// InlineData 内联二进制数据（通用 base64 编码）
type InlineData struct {
	MediaType string `json:"media_type"` // MIME 类型："image/png", "audio/mp3", "video/mp4", "application/pdf"
	Data      string `json:"data"`       // base64 编码的数据
}

// OutputImage 模型输出的图片
type OutputImage struct {
	URL           string `json:"url,omitempty"`            // 图片 URL
	Base64        string `json:"base64,omitempty"`         // base64 编码的图片数据
	RevisedPrompt string `json:"revised_prompt,omitempty"` // 模型修订后的 prompt（如 DALL·E）
}

// OutputAudio 模型输出的音频
type OutputAudio struct {
	Data   string `json:"data"`   // base64 编码的音频数据
	Format string `json:"format"` // 音频格式："mp3", "wav", "opus"
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

// AudioPart 创建音频片段（通过 base64 数据）
func AudioPart(format, base64Data string) ContentPart {
	return ContentPart{
		Type:       ContentTypeInputAudio,
		InputAudio: &InputAudio{Data: base64Data, Format: format},
	}
}

// VideoPart 创建视频片段（通过 URL）
func VideoPart(url string) ContentPart {
	return ContentPart{
		Type:     ContentTypeVideoURL,
		VideoURL: &MediaURL{URL: url},
	}
}

// FilePart 创建文件片段（通过 URL）
func FilePart(url string) ContentPart {
	return ContentPart{
		Type:    ContentTypeFileURL,
		FileURL: &MediaURL{URL: url},
	}
}

// InlineDataPart 创建内联数据片段（通用 base64）
func InlineDataPart(mediaType, base64Data string) ContentPart {
	return ContentPart{
		Type:       ContentTypeInlineData,
		InlineData: &InlineData{MediaType: mediaType, Data: base64Data},
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

	// 输出侧多模态：模型生成的非文本内容
	OutputImages []OutputImage `json:"output_images,omitempty"`
	OutputAudio  *OutputAudio  `json:"output_audio,omitempty"`

	// tool 消息专用：关联到哪个 ToolCall
	ToolCallID string `json:"tool_call_id,omitempty"`

	// tool 消息专用：标记工具执行是否出错（Anthropic API 原生支持 is_error）
	IsError bool `json:"is_error,omitempty"`

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
	CallID       string        `json:"call_id"`
	Content      string        `json:"content"`
	IsError      bool          `json:"is_error"`
	ContentParts []ContentPart `json:"content_parts,omitempty"` // 多模态工具结果
}

// ToolResultContent 工具返回多模态结果时使用的结构体
// 工具 Handler 返回此类型时，Execute() 会将其 ContentParts 填充到 ToolResult 中
//
// 用法：
//
//	func MyTool(ctx context.Context, args map[string]any) (any, error) {
//	    return &schema.ToolResultContent{
//	        Content: "截图已保存",
//	        ContentParts: []schema.ContentPart{
//	            schema.TextPart("截图已保存"),
//	            schema.ImagePartBase64("image/png", base64Data),
//	        },
//	    }, nil
//	}
type ToolResultContent struct {
	Content      string        // 文本内容
	ContentParts []ContentPart // 多模态内容片段
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
		if p.Type == ContentTypeImageURL {
			count++
		}
	}
	return count
}

// HasOutputImages 是否包含输出图片
func (m *Message) HasOutputImages() bool {
	return len(m.OutputImages) > 0
}

// HasOutputAudio 是否包含输出音频
func (m *Message) HasOutputAudio() bool {
	return m.OutputAudio != nil
}

// Clone 深拷贝
func (m *Message) Clone() Message {
	cloned := Message{
		Role:             m.Role,
		Content:          m.Content,
		ReasoningContent: m.ReasoningContent,
		Name:             m.Name,
		Partial:          m.Partial,
		ToolCallID:       m.ToolCallID,
		IsError:          m.IsError,
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
				cp.ImageURL = &ImageURL{URL: p.ImageURL.URL, Detail: p.ImageURL.Detail}
			}
			if p.InputAudio != nil {
				cp.InputAudio = &InputAudio{Data: p.InputAudio.Data, Format: p.InputAudio.Format}
			}
			if p.VideoURL != nil {
				cp.VideoURL = &MediaURL{URL: p.VideoURL.URL}
			}
			if p.FileURL != nil {
				cp.FileURL = &MediaURL{URL: p.FileURL.URL}
			}
			if p.InlineData != nil {
				cp.InlineData = &InlineData{MediaType: p.InlineData.MediaType, Data: p.InlineData.Data}
			}
			cloned.ContentParts[i] = cp
		}
	}

	if m.OutputImages != nil {
		cloned.OutputImages = make([]OutputImage, len(m.OutputImages))
		for i, img := range m.OutputImages {
			cloned.OutputImages[i] = OutputImage{
				URL:           img.URL,
				Base64:        img.Base64,
				RevisedPrompt: img.RevisedPrompt,
			}
		}
	}

	if m.OutputAudio != nil {
		cloned.OutputAudio = &OutputAudio{
			Data:   m.OutputAudio.Data,
			Format: m.OutputAudio.Format,
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
		msg := &Message{
			Role:       ToolRole,
			ToolCallID: r.CallID,
			IsError:    r.IsError,
		}

		if len(r.ContentParts) > 0 {
			msg.ContentParts = r.ContentParts
			if r.Content != "" {
				msg.Content = r.Content
			}
			if r.IsError {
				msg.Content = "[Error] " + msg.Content
			}
		} else {
			msg.Content = r.Content
			if r.IsError {
				msg.Content = "[Error] " + msg.Content
			}
		}

		msgs = append(msgs, msg)
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
				case ContentTypeText:
					builder.WriteString(fmt.Sprintf("  [%d] 文本: %s\n", j, indentString(part.Text, "      ")))
				case ContentTypeImageURL:
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
				case ContentTypeInputAudio:
					if part.InputAudio != nil {
						builder.WriteString(fmt.Sprintf("  [%d] 音频: format=%s, data=%d bytes\n",
							j, part.InputAudio.Format, len(part.InputAudio.Data)))
					}
				case ContentTypeVideoURL:
					if part.VideoURL != nil {
						url := part.VideoURL.URL
						if len(url) > 80 {
							url = url[:80] + "..."
						}
						builder.WriteString(fmt.Sprintf("  [%d] 视频: %s\n", j, url))
					}
				case ContentTypeFileURL:
					if part.FileURL != nil {
						url := part.FileURL.URL
						if len(url) > 80 {
							url = url[:80] + "..."
						}
						builder.WriteString(fmt.Sprintf("  [%d] 文件: %s\n", j, url))
					}
				case ContentTypeInlineData:
					if part.InlineData != nil {
						builder.WriteString(fmt.Sprintf("  [%d] 内联数据: type=%s, data=%d bytes\n",
							j, part.InlineData.MediaType, len(part.InlineData.Data)))
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

		if len(msg.OutputImages) > 0 {
			builder.WriteString("🖼️ 输出图片:\n")
			for j, img := range msg.OutputImages {
				if img.URL != "" {
					url := img.URL
					if len(url) > 80 {
						url = url[:80] + "..."
					}
					builder.WriteString(fmt.Sprintf("  [%d] URL: %s\n", j, url))
				} else if img.Base64 != "" {
					builder.WriteString(fmt.Sprintf("  [%d] Base64: %d bytes\n", j, len(img.Base64)))
				}
				if img.RevisedPrompt != "" {
					builder.WriteString(fmt.Sprintf("      修订Prompt: %s\n", img.RevisedPrompt))
				}
			}
		}

		if msg.OutputAudio != nil {
			builder.WriteString(fmt.Sprintf("🔊 输出音频: format=%s, data=%d bytes\n",
				msg.OutputAudio.Format, len(msg.OutputAudio.Data)))
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

		if msg.IsError {
			builder.WriteString("❌ 工具执行出错\n")
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
