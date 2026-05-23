package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// MockAgent 用于测试的模拟 Agent
// ============================================================================

// MockAgent 可配置的模拟 Agent，用于测试调用方逻辑
//
// 用法：
//
//	mock := NewMockAgent().
//	    WithResponse("你好", "你好！我是助手").
//	    WithResponse("天气", "今天晴天").
//	    WithFallback("我不确定，请重试")
//
//	resp, err := mock.Send(ctx, "你好")  // → "你好！我是助手"
//	resp, err := mock.Send(ctx, "天气")  // → "今天晴天"
//	resp, err := mock.Send(ctx, "随便")  // → "我不确定，请重试"
type MockAgent struct {
	mu sync.Mutex

	// 响应规则
	responses map[string]*schema.Message // 精确匹配
	fallback  *schema.Message            // 无匹配时的默认响应

	// 调用记录
	calls []MockCall

	// 可配置行为
	err    error                 // 直接返回错误
	delay  time.Duration         // 模拟延迟
	onSend func(*schema.Message) // 发送时的回调钩子
}

// MockCall 单次调用记录
type MockCall struct {
	Msg       *schema.Message `json:"msg"`
	Timestamp time.Time       `json:"timestamp"`
}

// NewMockAgent 创建 MockAgent
func NewMockAgent() *MockAgent {
	return &MockAgent{
		responses: make(map[string]*schema.Message),
		calls:     make([]MockCall, 0),
	}
}

// ============================================================================
// 配置方法（链式调用）
// ============================================================================

// WithResponse 注册精确匹配响应：当用户消息的纯文本内容包含 key 时返回
func (m *MockAgent) WithResponse(key string, content string) *MockAgent {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[key] = &schema.Message{
		Role:    schema.AssistantRole,
		Content: content,
	}
	return m
}

// WithResponseMsg 注册精确匹配响应（完整 Message）
func (m *MockAgent) WithResponseMsg(key string, msg *schema.Message) *MockAgent {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[key] = msg
	return m
}

// WithToolResponse 注册带工具调用的响应（模拟一轮工具调用后返回结果）
//
//	mock.WithToolResponse("搜索",
//	    // 第一轮：模型要求调用工具
//	    &schema.Message{
//	        Role: schema.AssistantRole,
//	        ToolCalls: []schema.ToolCall{{ID: "call_1", ...}},
//	    },
//	    // 第二轮：工具执行后模型返回最终回答
//	    &schema.Message{
//	        Role:    schema.AssistantRole,
//	        Content: "搜索结果是...",
//	    },
//	)
func (m *MockAgent) WithToolResponse(key string, rounds ...*schema.Message) *MockAgent {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 存储为切片，SendMessage 时按轮次返回
	// 用特殊 key 前缀区分
	toolKey := "__tool__" + key
	toolResponses[toolKey] = rounds
	m.responses[toolKey] = rounds[0] // 第一轮

	return m
}

// 全局工具响应存储（简化实现）
var toolResponses = make(map[string][]*schema.Message)

// WithFallback 设置无匹配时的默认响应
func (m *MockAgent) WithFallback(content string) *MockAgent {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallback = &schema.Message{
		Role:    schema.AssistantRole,
		Content: content,
	}
	return m
}

// WithFallbackMsg 设置无匹配时的默认响应（完整 Message）
func (m *MockAgent) WithFallbackMsg(msg *schema.Message) *MockAgent {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallback = msg
	return m
}

// WithError 配置所有调用直接返回错误
func (m *MockAgent) WithError(err error) *MockAgent {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
	return m
}

// WithDelay 配置模拟延迟（模拟真实 API 调用耗时）
func (m *MockAgent) WithDelay(d time.Duration) *MockAgent {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delay = d
	return m
}

// WithOnSend 配置发送时的回调钩子（用于断言或副作用）
func (m *MockAgent) WithOnSend(fn func(*schema.Message)) *MockAgent {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onSend = fn
	return m
}

// ============================================================================
// AgentInterface 实现
// ============================================================================

// SendMessage 非流式发送
func (m *MockAgent) SendMessage(ctx context.Context, msg *schema.Message) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 记录调用
	m.calls = append(m.calls, MockCall{Msg: msg, Timestamp: time.Now()})

	// 回调钩子
	if m.onSend != nil {
		m.onSend(msg)
	}

	// 模拟延迟
	if m.delay > 0 {
		time.Sleep(m.delay)
	}

	// 检查 context
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 直接返回错误
	if m.err != nil {
		return nil, m.err
	}

	// 匹配响应
	resp := m.matchResponse(msg)
	return resp, nil
}

// SendMessageStream 流式发送（将响应拆成逐字符 chunk 发送）
func (m *MockAgent) SendMessageStream(ctx context.Context, msg *schema.Message, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, MockCall{Msg: msg, Timestamp: time.Now()})

	if m.onSend != nil {
		m.onSend(msg)
	}

	if m.delay > 0 {
		time.Sleep(m.delay)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if m.err != nil {
		return nil, m.err
	}

	resp := m.matchResponse(msg)

	// 模拟流式输出：逐 chunk 发送
	fullMsg := &schema.Message{
		Role:    resp.Role,
		Content: "",
	}

	// 有工具调用时，一次性发送
	if len(resp.ToolCalls) > 0 {
		chunk := &schema.Message{
			Role:      resp.Role,
			ToolCalls: resp.ToolCalls,
		}
		if !onChunk(chunk, true) {
			return fullMsg, fmt.Errorf("user cancelled stream")
		}
		fullMsg.ToolCalls = resp.ToolCalls
		return fullMsg, nil
	}

	// 文本逐 chunk 发送
	content := resp.Content
	if content == "" {
		// 空响应也发一个 chunk
		chunk := &schema.Message{Role: resp.Role}
		onChunk(chunk, false)
		return fullMsg, nil
	}

	// 每次发 10-20 个字符，模拟真实流式
	chunkSize := 15
	for i := 0; i < len(content); i += chunkSize {
		end := i + chunkSize
		if end > len(content) {
			end = len(content)
		}
		piece := content[i:end]

		chunk := &schema.Message{
			Role:    resp.Role,
			Content: piece,
		}

		if !onChunk(chunk, false) {
			fullMsg.Content = content[:i] // 已发送部分
			return fullMsg, fmt.Errorf("user cancelled stream")
		}

		fullMsg.Content += piece
	}

	// 模拟 Usage
	fullMsg.Usage = &schema.Usage{
		PromptTokens:     100,
		CompletionTokens: uint64(len(content)),
		TotalTokens:      100 + uint64(len(content)),
	}

	return fullMsg, nil
}

// ============================================================================
// 查询方法（用于测试断言）
// ============================================================================

// CallCount 返回总调用次数
func (m *MockAgent) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// Calls 返回所有调用记录的副本
// Calls 返回所有调用记录的副本
func (m *MockAgent) Calls() []MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]MockCall, len(m.calls))
	for i, c := range m.calls {
		// 深拷贝 Message
		if c.Msg != nil {
			cloned := c.Msg.Clone()
			result[i] = MockCall{
				Msg:       &cloned,
				Timestamp: c.Timestamp,
			}
		} else {
			result[i] = c
		}
	}
	return result
}

// LastCall 返回最后一次调用的消息（没有调用则返回 nil）
func (m *MockAgent) LastCall() *schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return nil
	}
	return m.calls[len(m.calls)-1].Msg
}

// LastCallContent 返回最后一次调用的纯文本内容
func (m *MockAgent) LastCallContent() string {
	if msg := m.LastCall(); msg != nil {
		return msg.TextContent()
	}
	return ""
}

// HasCallWith 检查是否有某次调用的内容包含指定文本
func (m *MockAgent) HasCallWith(text string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.calls {
		if c.Msg != nil {
			content := c.Msg.TextContent()
			for i := 0; i <= len(content)-len(text); i++ {
				if content[i:i+len(text)] == text {
					return true
				}
			}
		}
	}
	return false
}

// Reset 重置所有状态（调用记录、响应规则等）
func (m *MockAgent) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = make([]MockCall, 0)
	m.responses = make(map[string]*schema.Message)
	m.fallback = nil
	m.err = nil
	m.delay = 0
	m.onSend = nil
}

// ============================================================================
// 内部方法
// ============================================================================

// matchResponse 根据用户消息匹配响应（调用方需持有锁）
func (m *MockAgent) matchResponse(msg *schema.Message) *schema.Message {
	if msg == nil {
		return m.defaultResponse()
	}

	text := msg.TextContent()

	// 精确匹配
	for key, resp := range m.responses {
		if key == text || contains(text, key) {
			return resp
		}
	}

	return m.defaultResponse()
}

// defaultResponse 返回默认响应（调用方需持有锁）
func (m *MockAgent) defaultResponse() *schema.Message {
	if m.fallback != nil {
		return m.fallback
	}
	return &schema.Message{
		Role:    schema.AssistantRole,
		Content: "[MockAgent] no matching response",
	}
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Ensure MockAgent implements AgentInterface
var _ AgentInterface = (*MockAgent)(nil)
