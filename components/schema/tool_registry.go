package schema

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// ToolHandler 工具执行函数签名
// ctx: 上下文
// args: 解析后的参数（map[string]any）
// 返回: 任意结果 + error
type ToolHandler func(ctx context.Context, args map[string]any) (any, error)

// ToolPermission 工具权限级别
type ToolPermission int

const (
	// PermReadOnly 只读权限（安全，无副作用）
	PermReadOnly ToolPermission = iota
	// PermReadWrite 读写权限（可能修改状态）
	PermReadWrite
	// PermDangerous 危险权限（可能破坏数据/系统）
	PermDangerous
)

func (p ToolPermission) String() string {
	switch p {
	case PermReadOnly:
		return "readonly"
	case PermReadWrite:
		return "readwrite"
	case PermDangerous:
		return "dangerous"
	default:
		return "unknown"
	}
}

// ToolMetadata 工具元数据（Harness 风格声明式配置）
type ToolMetadata struct {
	// Name 工具名称
	Name string `json:"name"`
	// Description 工具描述
	Description string `json:"description"`
	// Parameters JSON Schema 参数定义
	Parameters any `json:"parameters"`
	// Permission 权限级别
	Permission ToolPermission `json:"permission"`
	// Category 工具分类（如 "file", "network", "system"）
	Category string `json:"category"`
	// Version 工具版本
	Version string `json:"version"`
	// Author 工具作者
	Author string `json:"author"`
	// Tags 标签（用于搜索和过滤）
	Tags []string `json:"tags"`
	// Requires 依赖的其他工具名称
	Requires []string `json:"requires,omitempty"`
	// Timeout 默认超时时间
	Timeout time.Duration `json:"timeout"`
	// Enabled 是否启用
	Enabled bool `json:"enabled"`
	// Config 工具特定配置
	Config map[string]any `json:"config,omitempty"`
}

// RegisteredToolV2 注册后的工具（Harness 风格）
type RegisteredToolV2 struct {
	Metadata ToolMetadata
	Handler  ToolHandler
	// RegisteredAt 注册时间
	RegisteredAt time.Time
	// UpdatedAt 最后更新时间
	UpdatedAt time.Time
	// UseCount 使用次数统计
	UseCount int64
}

// ToolRegistry Harness 风格的动态工具注册中心
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]*RegisteredToolV2
	// hooks 生命周期钩子
	hooks struct {
		beforeExecute []func(ctx context.Context, toolName string, args map[string]any) error
		afterExecute  []func(ctx context.Context, toolName string, result ToolResult, duration time.Duration)
		onRegister    []func(tool *RegisteredToolV2)
		onUnregister  []func(toolName string)
	}
}

// NewToolRegistry 创建工具注册中心
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]*RegisteredToolV2),
	}
}

// Register 动态注册工具
func (r *ToolRegistry) Register(meta ToolMetadata, handler ToolHandler) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if meta.Name == "" {
		return fmt.Errorf("tool name is required")
	}

	if _, exists := r.tools[meta.Name]; exists {
		return fmt.Errorf("tool %s already registered", meta.Name)
	}

	// 设置默认值
	if meta.Version == "" {
		meta.Version = "1.0.0"
	}
	if meta.Timeout == 0 {
		meta.Timeout = 30 * time.Second
	}
	meta.Enabled = true

	tool := &RegisteredToolV2{
		Metadata:     meta,
		Handler:      handler,
		RegisteredAt: time.Now(),
		UpdatedAt:    time.Now(),
	}

	r.tools[meta.Name] = tool

	// 触发注册钩子
	for _, hook := range r.hooks.onRegister {
		hook(tool)
	}

	return nil
}

// MustRegister 注册工具，失败 panic
func (r *ToolRegistry) MustRegister(meta ToolMetadata, handler ToolHandler) {
	if err := r.Register(meta, handler); err != nil {
		panic(err)
	}
}

// Unregister 注销工具
func (r *ToolRegistry) Unregister(toolName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	tool, exists := r.tools[toolName]
	if !exists {
		return fmt.Errorf("tool %s not found", toolName)
	}

	delete(r.tools, toolName)

	// 触发注销钩子
	for _, hook := range r.hooks.onUnregister {
		hook(toolName)
	}

	// 记录日志
	_ = tool
	return nil
}

// Update 更新工具（热更新）
func (r *ToolRegistry) Update(toolName string, meta ToolMetadata, handler ToolHandler) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	tool, exists := r.tools[toolName]
	if !exists {
		return fmt.Errorf("tool %s not found", toolName)
	}

	// 保留原有统计
	tool.Metadata = meta
	if handler != nil {
		tool.Handler = handler
	}
	tool.UpdatedAt = time.Now()

	return nil
}

// Get 获取工具
func (r *ToolRegistry) Get(toolName string) (*RegisteredToolV2, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, ok := r.tools[toolName]
	return tool, ok
}

// GetEnabledTools 获取所有启用的工具 Schema（发给模型用）
func (r *ToolRegistry) GetEnabledTools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []Tool
	for _, t := range r.tools {
		if t.Metadata.Enabled {
			result = append(result, Tool{
				Name:        t.Metadata.Name,
				Description: t.Metadata.Description,
				Parameters:  t.Metadata.Parameters,
			})
		}
	}
	return result
}

// GetAllTools 获取所有工具（包括禁用的）
func (r *ToolRegistry) GetAllTools() []*RegisteredToolV2 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*RegisteredToolV2, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

// GetByCategory 按分类获取工具
func (r *ToolRegistry) GetByCategory(category string) []*RegisteredToolV2 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*RegisteredToolV2
	for _, t := range r.tools {
		if t.Metadata.Category == category {
			result = append(result, t)
		}
	}
	return result
}

// GetByPermission 按权限级别获取工具
func (r *ToolRegistry) GetByPermission(perm ToolPermission) []*RegisteredToolV2 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*RegisteredToolV2
	for _, t := range r.tools {
		if t.Metadata.Permission == perm {
			result = append(result, t)
		}
	}
	return result
}

// GetByTag 按标签搜索工具
func (r *ToolRegistry) GetByTag(tag string) []*RegisteredToolV2 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*RegisteredToolV2
	for _, t := range r.tools {
		for _, tTag := range t.Metadata.Tags {
			if tTag == tag {
				result = append(result, t)
				break
			}
		}
	}
	return result
}

// Enable/Disable 启用/禁用工具

func (r *ToolRegistry) Enable(toolName string) error {
	return r.setEnabled(toolName, true)
}

func (r *ToolRegistry) Disable(toolName string) error {
	return r.setEnabled(toolName, false)
}

func (r *ToolRegistry) setEnabled(toolName string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	tool, ok := r.tools[toolName]
	if !ok {
		return fmt.Errorf("tool %s not found", toolName)
	}

	tool.Metadata.Enabled = enabled
	tool.UpdatedAt = time.Now()
	return nil
}

// Execute 执行工具（带生命周期钩子）
func (r *ToolRegistry) Execute(ctx context.Context, call ToolCall) ToolResult {
	start := time.Now()

	// 查找工具
	r.mu.RLock()
	tool, ok := r.tools[call.Function.Name]
	r.mu.RUnlock()

	if !ok {
		return ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf(`{"error": "tool %s not found"}`, call.Function.Name),
			IsError: true,
		}
	}

	if !tool.Metadata.Enabled {
		return ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf(`{"error": "tool %s is disabled"}`, call.Function.Name),
			IsError: true,
		}
	}

	// 解析参数
	args, err := parseToolArgs(call)
	if err != nil {
		return ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf(`{"error": "%s"}`, err.Error()),
			IsError: true,
		}
	}

	// 执行前置钩子
	for _, hook := range r.hooks.beforeExecute {
		if err := hook(ctx, call.Function.Name, args); err != nil {
			return ToolResult{
				CallID:  call.ID,
				Content: fmt.Sprintf(`{"error": "before execute hook failed: %s"}`, err.Error()),
				IsError: true,
			}
		}
	}

	// 执行工具（带超时）
	execCtx, cancel := context.WithTimeout(ctx, tool.Metadata.Timeout)
	defer cancel()

	output, err := tool.Handler(execCtx, args)
	duration := time.Since(start)

	// 更新使用统计
	r.mu.Lock()
	tool.UseCount++
	r.mu.Unlock()

	var result ToolResult
	if err != nil {
		result = ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf(`{"error": "%s"}`, err.Error()),
			IsError: true,
		}
	} else {
		content, _ := marshalOutput(output)
		result = ToolResult{
			CallID:  call.ID,
			Content: content,
			IsError: false,
		}
	}

	// 执行后置钩子
	for _, hook := range r.hooks.afterExecute {
		hook(ctx, call.Function.Name, result, duration)
	}

	return result
}

// ExecuteBatch 批量执行
func (r *ToolRegistry) ExecuteBatch(ctx context.Context, calls []ToolCall) []ToolResult {
	if len(calls) == 0 {
		return nil
	}

	if len(calls) == 1 {
		return []ToolResult{r.Execute(ctx, calls[0])}
	}

	var wg sync.WaitGroup
	results := make([]ToolResult, len(calls))

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c ToolCall) {
			defer wg.Done()
			results[idx] = r.Execute(ctx, c)
		}(i, call)
	}

	wg.Wait()
	return results
}

// ToToolMessages 将 ToolResult 转成 schema.Message
func (r *ToolRegistry) ToToolMessages(results []ToolResult) []*Message {
	msgs := make([]*Message, len(results))
	for i, res := range results {
		msgs[i] = &Message{
			Role:        ToolRole,
			Name:        res.CallID,
			Content:     res.Content,
			ToolResults: []ToolResult{res},
		}
	}
	return msgs
}

// ============================================================================
// 生命周期钩子
// ============================================================================

// AddBeforeExecuteHook 添加执行前钩子
func (r *ToolRegistry) AddBeforeExecuteHook(hook func(ctx context.Context, toolName string, args map[string]any) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks.beforeExecute = append(r.hooks.beforeExecute, hook)
}

// AddAfterExecuteHook 添加执行后钩子
func (r *ToolRegistry) AddAfterExecuteHook(hook func(ctx context.Context, toolName string, result ToolResult, duration time.Duration)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks.afterExecute = append(r.hooks.afterExecute, hook)
}

// AddOnRegisterHook 添加注册钩子
func (r *ToolRegistry) AddOnRegisterHook(hook func(tool *RegisteredToolV2)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks.onRegister = append(r.hooks.onRegister, hook)
}

// ============================================================================
// 辅助函数
// ============================================================================

func parseToolArgs(call ToolCall) (map[string]any, error) {
	var args map[string]any
	if call.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, fmt.Errorf("parse arguments failed: %w", err)
		}
	}
	return args, nil
}

func marshalOutput(output any) (string, error) {
	switch v := output.(type) {
	case string:
		return v, nil // 直接返回字符串
	case []byte:
		return string(v), nil
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}
