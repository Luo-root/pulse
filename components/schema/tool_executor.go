package schema

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ToolHandler 工具执行函数签名
// ctx: 上下文
// args: 解析后的参数（map[string]any）
// 返回: 任意结果 + error
type ToolHandler func(ctx context.Context, args map[string]any) (any, error)

// RegisteredTool 注册后的工具（内部用）
type RegisteredTool struct {
	Schema  Tool
	Handler ToolHandler
}

// ToolExecutor 工具执行器
type ToolExecutor struct {
	mu    sync.RWMutex
	tools map[string]RegisteredTool
}

// NewToolExecutor 创建执行器
func NewToolExecutor() *ToolExecutor {
	return &ToolExecutor{
		tools: make(map[string]RegisteredTool),
	}
}

// Register 注册工具
func (e *ToolExecutor) Register(tool Tool, handler ToolHandler) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.tools[tool.Name]; exists {
		return fmt.Errorf("tool %s already registered", tool.Name)
	}

	e.tools[tool.Name] = RegisteredTool{
		Schema:  tool,
		Handler: handler,
	}
	return nil
}

// MustRegister 注册工具，失败 panic（初始化时用）
func (e *ToolExecutor) MustRegister(tool Tool, handler ToolHandler) {
	if err := e.Register(tool, handler); err != nil {
		panic(err)
	}
}

// GetToolsSchema 获取所有工具的 schema（发给模型时用）
func (e *ToolExecutor) GetToolsSchema() []Tool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make([]Tool, 0, len(e.tools))
	for _, t := range e.tools {
		result = append(result, t.Schema)
	}
	return result
}

// Execute 执行单个工具调用
func (e *ToolExecutor) Execute(ctx context.Context, call ToolCall) ToolResult {
	e.mu.RLock()
	tool, ok := e.tools[call.Function.Name]
	e.mu.RUnlock()

	if !ok {
		return ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf(`{"error": "tool %s not found"}`, call.Function.Name),
			IsError: true,
		}
	}

	// 解析参数
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf(`{"error": "parse arguments failed: %s"}`, err.Error()),
			IsError: true,
		}
	}

	// 执行
	output, err := tool.Handler(ctx, args)
	if err != nil {
		return ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf(`{"error": "%s"}`, err.Error()),
			IsError: true,
		}
	}

	// 序列化结果
	content, err := json.Marshal(output)
	if err != nil {
		return ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf(`{"error": "serialize output failed: %s"}`, err.Error()),
			IsError: true,
		}
	}

	return ToolResult{
		CallID:  call.ID,
		Content: string(content),
		IsError: false,
	}
}

// ExecuteBatch 批量执行（支持并发）
func (e *ToolExecutor) ExecuteBatch(ctx context.Context, calls []ToolCall) []ToolResult {
	if len(calls) == 0 {
		return nil
	}

	// 单个直接执行，避免 goroutine 开销
	if len(calls) == 1 {
		return []ToolResult{e.Execute(ctx, calls[0])}
	}

	// 多个并发执行
	var wg sync.WaitGroup
	results := make([]ToolResult, len(calls))

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c ToolCall) {
			defer wg.Done()
			results[idx] = e.Execute(ctx, c)
		}(i, call)
	}

	wg.Wait()
	return results
}

// ToToolMessages 将 ToolResult 转成 schema.Message（回传给模型用）
func (e *ToolExecutor) ToToolMessages(results []ToolResult) []*Message {
	msgs := make([]*Message, len(results))
	for i, r := range results {
		msgs[i] = &Message{
			Role:    ToolRole,
			Name:    r.CallID, // OpenAI 用 Name 存 tool_call_id
			Content: r.Content,
			// ToolResults 供 Claude/Gemini 使用，OpenAI 忽略
			ToolResults: []ToolResult{r},
		}
	}
	return msgs
}
