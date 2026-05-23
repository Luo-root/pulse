package chatmodel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// MockResponse
// ============================================================================

// MockResponse 定义 Mock 模型的单次响应行为
type MockResponse struct {
	Content          string               // 文本内容
	ReasoningContent string               // 推理内容
	ContentParts     []schema.ContentPart // 多模态内容（优先于 Content）
	ToolCalls        []schema.ToolCall    // 工具调用
	Delay            time.Duration        // 模拟延迟
	Error            error                // 是否返回错误
}

// ============================================================================
// MockModel
// ============================================================================

// MockModel 用于测试的模拟模型
// 不需要真实 API Key 和网络请求，可精确控制响应行为
type MockModel struct {
	mu sync.RWMutex

	// 预设响应队列，按顺序取出
	responses []MockResponse
	// 当前索引
	currentIdx int
	// 循环使用
	loop bool

	// 记录所有输入消息（用于断言）
	recordedInputs [][]*schema.Message

	// 自定义函数（优先级最高）
	generateFunc func(ctx context.Context, input []*schema.Message) (*schema.Message, error)
	streamFunc   func(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)

	// 模型名称
	modelName string
}

// NewMockModel 创建空的 Mock 模型
func NewMockModel() *MockModel {
	return &MockModel{
		responses:      make([]MockResponse, 0),
		recordedInputs: make([][]*schema.Message, 0),
		modelName:      "mock-model",
	}
}

// NewMockModelWithResponses 创建带预设响应的 Mock 模型
func NewMockModelWithResponses(responses ...MockResponse) *MockModel {
	m := NewMockModel()
	m.responses = responses
	return m
}

// ============================================================================
// 配置方法
// ============================================================================

// SetLoop 设置是否循环使用响应队列
func (m *MockModel) SetLoop(loop bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loop = loop
}

// AddResponse 追加一个预设响应
func (m *MockModel) AddResponse(resp MockResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, resp)
}

// SetGenerateFunc 设置自定义 Generate 函数（优先于响应队列）
func (m *MockModel) SetGenerateFunc(fn func(ctx context.Context, input []*schema.Message) (*schema.Message, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generateFunc = fn
}

// SetStreamFunc 设置自定义 Stream 函数（优先于响应队列）
func (m *MockModel) SetStreamFunc(fn func(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamFunc = fn
}

// SetModelName 设置模型名称
func (m *MockModel) SetModelName(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modelName = name
}

// ============================================================================
// 断言辅助方法 — 输入记录
// ============================================================================

// GetRecordedInputs 获取所有记录的输入消息（深拷贝）
func (m *MockModel) GetRecordedInputs() [][]*schema.Message {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([][]*schema.Message, len(m.recordedInputs))
	for i, msgs := range m.recordedInputs {
		copied := make([]*schema.Message, len(msgs))
		for j, msg := range msgs {
			cloned := msg.Clone()
			copied[j] = &cloned
		}
		result[i] = copied
	}
	return result
}

// GetLastInput 获取最后一次输入消息
func (m *MockModel) GetLastInput() []*schema.Message {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.recordedInputs) == 0 {
		return nil
	}
	last := m.recordedInputs[len(m.recordedInputs)-1]
	result := make([]*schema.Message, len(last))
	for i, msg := range last {
		cloned := msg.Clone()
		result[i] = &cloned
	}
	return result
}

// GetCallCount 获取调用次数
func (m *MockModel) GetCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.recordedInputs)
}

// Reset 重置状态
func (m *MockModel) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentIdx = 0
	m.recordedInputs = make([][]*schema.Message, 0)
}

// GetModelName 返回模型名称
func (m *MockModel) GetModelName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.modelName
}

// ============================================================================
// 断言辅助方法 — 多模态输入
// ============================================================================

// HasMultimodalInput 检查最后一次输入是否包含多模态内容
func (m *MockModel) HasMultimodalInput() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.recordedInputs) == 0 {
		return false
	}
	for _, msg := range m.recordedInputs[len(m.recordedInputs)-1] {
		if msg.IsMultimodal() {
			return true
		}
	}
	return false
}

// GetLastInputImages 获取最后一次输入中的所有图片 URL（base64 data URL 或 http URL）
func (m *MockModel) GetLastInputImages() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.recordedInputs) == 0 {
		return nil
	}

	var images []string
	for _, msg := range m.recordedInputs[len(m.recordedInputs)-1] {
		for _, part := range msg.ContentParts {
			if part.Type == "image_url" && part.ImageURL != nil {
				images = append(images, part.ImageURL.URL)
			}
		}
	}
	return images
}

// GetMultimodalCallCount 获取包含多模态输入的调用次数
func (m *MockModel) GetMultimodalCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, input := range m.recordedInputs {
		for _, msg := range input {
			if msg.IsMultimodal() {
				count++
				break // 每次调用只计一次
			}
		}
	}
	return count
}

// GetLastInputTextContent 获取最后一次输入的纯文本内容（多模态消息也能正确返回）
func (m *MockModel) GetLastInputTextContent() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.recordedInputs) == 0 {
		return ""
	}

	var texts []string
	for _, msg := range m.recordedInputs[len(m.recordedInputs)-1] {
		if t := msg.TextContent(); t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}

// ============================================================================
// 内部方法
// ============================================================================

// recordInput 记录输入（深拷贝）
func (m *MockModel) recordInput(input []*schema.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := make([]*schema.Message, len(input))
	for i, msg := range input {
		cloned := msg.Clone()
		copied[i] = &cloned
	}
	m.recordedInputs = append(m.recordedInputs, copied)
}

// nextResponse 取下一个响应
func (m *MockModel) nextResponse() MockResponse {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.responses) == 0 {
		return MockResponse{Content: "mock default response"}
	}

	if m.currentIdx >= len(m.responses) {
		if m.loop {
			m.currentIdx = 0
		} else {
			return m.responses[len(m.responses)-1]
		}
	}

	resp := m.responses[m.currentIdx]
	m.currentIdx++
	return resp
}

// buildMessage 从 MockResponse 构建 schema.Message
func buildMessage(resp MockResponse) *schema.Message {
	msg := &schema.Message{
		Role:             schema.AssistantRole,
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		ToolCalls:        resp.ToolCalls,
	}

	// 多模态内容优先：设置 ContentParts 时 Content 由 adapter 决定如何处理
	if len(resp.ContentParts) > 0 {
		msg.ContentParts = resp.ContentParts
		// 如果没有显式设置 Content，从 ContentParts 中提取文本
		if msg.Content == "" {
			msg.Content = msg.TextContent()
		}
	}

	return msg
}

// ============================================================================
// Generate / Stream 实现
// ============================================================================

// Generate 实现 BaseModel 接口
func (m *MockModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	m.recordInput(input)

	m.mu.RLock()
	fn := m.generateFunc
	m.mu.RUnlock()
	if fn != nil {
		return fn(ctx, input)
	}

	resp := m.nextResponse()

	if resp.Delay > 0 {
		select {
		case <-time.After(resp.Delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if resp.Error != nil {
		return nil, resp.Error
	}

	return buildMessage(resp), nil
}

// Stream 实现 BaseModel 接口
func (m *MockModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
	m.recordInput(input)

	m.mu.RLock()
	fn := m.streamFunc
	m.mu.RUnlock()
	if fn != nil {
		return fn(ctx, input)
	}

	resp := m.nextResponse()

	if resp.Error != nil {
		return nil, resp.Error
	}

	reader := schema.NewStreamReader()

	go func() {
		defer reader.Close()

		if resp.Delay > 0 {
			select {
			case <-time.After(resp.Delay):
			case <-ctx.Done():
				return
			}
		}

		// 1. 推理内容
		if resp.ReasoningContent != "" {
			reader.Send(schema.Message{
				Role:             schema.AssistantRole,
				ReasoningContent: resp.ReasoningContent,
			})
		}

		// 2. 多模态内容：一次性发送（图片不适合分块）
		if len(resp.ContentParts) > 0 {
			reader.Send(schema.Message{
				Role:         schema.AssistantRole,
				Content:      resp.Content,
				ContentParts: resp.ContentParts,
			})
			return
		}

		// 3. 纯文本内容分块发送
		if resp.Content != "" {
			chunkSize := 4
			for i := 0; i < len(resp.Content); i += chunkSize {
				end := i + chunkSize
				if end > len(resp.Content) {
					end = len(resp.Content)
				}

				select {
				case <-ctx.Done():
					return
				default:
				}

				reader.Send(schema.Message{
					Role:    schema.AssistantRole,
					Content: resp.Content[i:end],
				})
			}
		}

		// 4. 工具调用
		if len(resp.ToolCalls) > 0 {
			reader.Send(schema.Message{
				Role:      schema.AssistantRole,
				ToolCalls: resp.ToolCalls,
			})
		}
	}()

	return reader, nil
}

// ============================================================================
// MockResponse 构建器
// ============================================================================

// MockTextResponse 纯文本响应
func MockTextResponse(content string) MockResponse {
	return MockResponse{Content: content}
}

// MockReasoningResponse 带推理的响应
func MockReasoningResponse(content, reasoning string) MockResponse {
	return MockResponse{
		Content:          content,
		ReasoningContent: reasoning,
	}
}

// MockToolCallResponse 工具调用响应
func MockToolCallResponse(toolName string, args map[string]any) MockResponse {
	argsJSON := "{}"
	if args != nil {
		var pairs []string
		for k, v := range args {
			pairs = append(pairs, fmt.Sprintf(`"%s":"%v"`, k, v))
		}
		argsJSON = "{" + strings.Join(pairs, ",") + "}"
	}

	return MockResponse{
		ToolCalls: []schema.ToolCall{
			{
				ID:   fmt.Sprintf("call_%d", time.Now().UnixNano()),
				Type: "function",
				Function: schema.FunctionCall{
					Name:      toolName,
					Arguments: argsJSON,
				},
			},
		},
	}
}

// MockErrorResponse 错误响应
func MockErrorResponse(err error) MockResponse {
	return MockResponse{Error: err}
}

// MockDelayedResponse 带延迟的响应
func MockDelayedResponse(content string, delay time.Duration) MockResponse {
	return MockResponse{Content: content, Delay: delay}
}

// MockMultimodalResponse 多模态响应（模型返回文本 + 图片）
func MockMultimodalResponse(text string, imageURLs ...string) MockResponse {
	parts := []schema.ContentPart{schema.TextPart(text)}
	for _, url := range imageURLs {
		parts = append(parts, schema.ImagePart(url))
	}
	return MockResponse{
		Content:      text,
		ContentParts: parts,
	}
}

// MockMultimodalBase64Response 多模态响应（模型返回文本 + base64 图片）
func MockMultimodalBase64Response(text string, mediaType, base64Data string) MockResponse {
	return MockResponse{
		Content: text,
		ContentParts: []schema.ContentPart{
			schema.TextPart(text),
			schema.ImagePartBase64(mediaType, base64Data),
		},
	}
}

// ============================================================================
// 预置场景
// ============================================================================

// NewWeatherMockModel 天气助手：第一次返回工具调用，第二次返回结果
func NewWeatherMockModel() *MockModel {
	return NewMockModelWithResponses(
		MockToolCallResponse("get_weather", map[string]any{"city": "北京"}),
		MockTextResponse("北京今天天气晴，气温25°C"),
	)
}

// NewEchoMockModel 回声模型：返回用户最后一条文本内容
func NewEchoMockModel() *MockModel {
	m := NewMockModel()
	m.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		var userContent string
		for i := len(input) - 1; i >= 0; i-- {
			if input[i].Role == schema.UserRole {
				// 修复：用 TextContent() 而非 Content，支持多模态消息
				userContent = input[i].TextContent()
				break
			}
		}
		return &schema.Message{
			Role:    schema.AssistantRole,
			Content: "Echo: " + userContent,
		}, nil
	})
	return m
}

// NewStreamingMockModel 流式模型
func NewStreamingMockModel(content string) *MockModel {
	m := NewMockModel()
	m.AddResponse(MockTextResponse(content))
	return m
}
