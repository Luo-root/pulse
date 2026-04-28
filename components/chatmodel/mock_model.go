package chatmodel

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// MockResponse 定义 Mock 模型的响应行为
type MockResponse struct {
	// 响应消息内容
	Content string
	// 推理内容（reasoning）
	ReasoningContent string
	// 工具调用列表
	ToolCalls []schema.ToolCall
	// 模拟延迟
	Delay time.Duration
	// 是否返回错误
	Error error
}

// MockModel 是一个用于测试的模拟模型实现
// 它不需要真实的 API Key 和网络请求，可以精确控制响应行为
type MockModel struct {
	mu sync.RWMutex

	// 预设的响应队列，每次调用按顺序取出一个
	responses []MockResponse
	// 当前响应索引
	currentIdx int
	// 是否循环使用响应（到达末尾后从头开始）
	loop bool

	// 记录所有接收到的输入消息（用于断言验证）
	recordedInputs [][]*schema.Message

	// 自定义响应生成函数（优先级高于 responses 队列）
	// 如果设置了这个函数，每次调用都会使用此函数生成响应
	generateFunc func(ctx context.Context, input []*schema.Message) (*schema.Message, error)
	streamFunc   func(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)

	// 模型名称（用于 UsageTracker）
	modelName string
}

// NewMockModel 创建一个新的 Mock 模型
func NewMockModel() *MockModel {
	return &MockModel{
		responses:      make([]MockResponse, 0),
		recordedInputs: make([][]*schema.Message, 0),
		modelName:      "mock-model",
	}
}

// NewMockModelWithResponses 创建带有预设响应的 Mock 模型
func NewMockModelWithResponses(responses ...MockResponse) *MockModel {
	m := NewMockModel()
	m.responses = responses
	return m
}

// SetLoop 设置是否循环使用响应队列
func (m *MockModel) SetLoop(loop bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loop = loop
}

// AddResponse 向响应队列追加一个预设响应
func (m *MockModel) AddResponse(resp MockResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, resp)
}

// SetGenerateFunc 设置自定义 Generate 函数
func (m *MockModel) SetGenerateFunc(fn func(ctx context.Context, input []*schema.Message) (*schema.Message, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generateFunc = fn
}

// SetStreamFunc 设置自定义 Stream 函数
func (m *MockModel) SetStreamFunc(fn func(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamFunc = fn
}

// SetModelName 设置模型名称（用于 UsageTracker）
func (m *MockModel) SetModelName(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modelName = name
}

// GetRecordedInputs 获取所有记录到的输入消息（用于测试断言）
func (m *MockModel) GetRecordedInputs() [][]*schema.Message {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([][]*schema.Message, len(m.recordedInputs))
	for i, inputs := range m.recordedInputs {
		copied := make([]*schema.Message, len(inputs))
		for j, msg := range inputs {
			cloned := msg.Clone()
			copied[j] = &cloned
		}
		result[i] = copied
	}
	return result
}

// GetLastInput 获取最后一次接收到的输入消息
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

// GetCallCount 获取被调用的次数
func (m *MockModel) GetCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.recordedInputs)
}

// Reset 重置 Mock 状态（清空记录和索引）
func (m *MockModel) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentIdx = 0
	m.recordedInputs = make([][]*schema.Message, 0)
}

// nextResponse 获取下一个响应
func (m *MockModel) nextResponse() MockResponse {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.responses) == 0 {
		return MockResponse{
			Content: "mock default response",
		}
	}

	resp := m.responses[m.currentIdx]
	m.currentIdx++

	if m.currentIdx >= len(m.responses) {
		if m.loop {
			m.currentIdx = 0
		} else {
			m.currentIdx = len(m.responses) // 保持在末尾，后续调用使用最后一个响应
		}
	}

	return resp
}

// recordInput 记录输入消息
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

// Generate 实现 BaseModel 接口
func (m *MockModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	m.recordInput(input)

	// 如果设置了自定义函数，优先使用
	m.mu.RLock()
	genFunc := m.generateFunc
	m.mu.RUnlock()
	if genFunc != nil {
		return genFunc(ctx, input)
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

	return &schema.Message{
		Role:             schema.AssistantRole,
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		ToolCalls:        resp.ToolCalls,
	}, nil
}

// Stream 实现 BaseModel 接口
func (m *MockModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
	m.recordInput(input)

	// 如果设置了自定义函数，优先使用
	m.mu.RLock()
	streamFn := m.streamFunc
	m.mu.RUnlock()
	if streamFn != nil {
		return streamFn(ctx, input)
	}

	resp := m.nextResponse()

	if resp.Error != nil {
		return nil, resp.Error
	}

	// 创建流式读取器，模拟逐字输出
	reader, writer := schema.PipeStreamReader()

	go func() {
		defer writer.Close()

		if resp.Delay > 0 {
			select {
			case <-time.After(resp.Delay):
			case <-ctx.Done():
				return
			}
		}

		// 如果有 reasoning content，先发送
		if resp.ReasoningContent != "" {
			writer.Send(&schema.Message{
				Role:             schema.AssistantRole,
				ReasoningContent: resp.ReasoningContent,
			})
		}

		// 分块发送内容（模拟流式）
		content := resp.Content
		chunkSize := 4 // 每块4个字符
		for i := 0; i < len(content); i += chunkSize {
			end := i + chunkSize
			if end > len(content) {
				end = len(content)
			}
			chunk := content[i:end]
			writer.Send(&schema.Message{
				Role:    schema.AssistantRole,
				Content: chunk,
			})
		}

		// 如果有工具调用，最后发送
		if len(resp.ToolCalls) > 0 {
			writer.Send(&schema.Message{
				Role:      schema.AssistantRole,
				ToolCalls: resp.ToolCalls,
			})
		}
	}()

	return reader, nil
}

// GetModelName 返回模型名称（用于 UsageTracker）
func (m *MockModel) GetModelName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.modelName
}

// ============================================================================
// 预设响应构建器（方便测试使用）
// ============================================================================

// MockTextResponse 创建纯文本响应
func MockTextResponse(content string) MockResponse {
	return MockResponse{Content: content}
}

// MockReasoningResponse 创建带推理内容的响应
func MockReasoningResponse(content, reasoning string) MockResponse {
	return MockResponse{
		Content:          content,
		ReasoningContent: reasoning,
	}
}

// MockToolCallResponse 创建工具调用响应
func MockToolCallResponse(toolName string, args map[string]any) MockResponse {
	argsJSON := "{}"
	if args != nil {
		// 简单 JSON 序列化
		pairs := make([]string, 0, len(args))
		for k, v := range args {
			pairs = append(pairs, fmt.Sprintf(`"%s":"%v"`, k, v))
		}
		if len(pairs) > 0 {
			argsJSON = "{"
			for i, p := range pairs {
				if i > 0 {
					argsJSON += ","
				}
				argsJSON += p
			}
			argsJSON += "}"
		}
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

// MockErrorResponse 创建错误响应
func MockErrorResponse(err error) MockResponse {
	return MockResponse{Error: err}
}

// MockDelayedResponse 创建带延迟的响应
func MockDelayedResponse(content string, delay time.Duration) MockResponse {
	return MockResponse{
		Content: content,
		Delay:   delay,
	}
}

// ============================================================================
// 常见测试场景辅助函数
// ============================================================================

// NewWeatherMockModel 创建一个模拟天气助手的 Mock 模型
// 第一次调用返回工具调用，第二次调用返回天气结果
func NewWeatherMockModel() *MockModel {
	return NewMockModelWithResponses(
		MockToolCallResponse("get_weather", map[string]any{"city": "北京"}),
		MockTextResponse("北京今天天气晴，气温25°C"),
	)
}

// NewEchoMockModel 创建一个回声 Mock 模型（返回用户输入的内容）
func NewEchoMockModel() *MockModel {
	m := NewMockModel()
	m.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		// 找到最后一条用户消息并回显
		var userContent string
		for i := len(input) - 1; i >= 0; i-- {
			if input[i].Role == schema.UserRole {
				userContent = input[i].Content
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

// NewStreamingMockModel 创建一个模拟流式输出的 Mock 模型
func NewStreamingMockModel(content string) *MockModel {
	m := NewMockModel()
	m.AddResponse(MockTextResponse(content))
	return m
}
