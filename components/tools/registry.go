package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ToolHandler 工具执行函数签名
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

// ToolMetadata 工具元数据
type ToolMetadata struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  any            `json:"parameters"`
	Permission  ToolPermission `json:"permission"`
	Category    string         `json:"category"`
	Version     string         `json:"version"`
	Author      string         `json:"author"`
	Tags        []string       `json:"tags"`
	Requires    []string       `json:"requires,omitempty"`
	Timeout     time.Duration  `json:"timeout"`
	Enabled     bool           `json:"enabled"`
	Config      map[string]any `json:"config,omitempty"`
}

// RegisteredTool 注册后的工具
type RegisteredTool struct {
	Metadata     ToolMetadata
	Handler      ToolHandler
	RegisteredAt time.Time
	UpdatedAt    time.Time
	UseCount     int64
}

// HookID 钩子标识
type HookID int

// ToolRegistry 动态工具注册中心
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]*RegisteredTool

	nextHookID HookID

	beforeExecuteID   []HookID
	beforeExecuteFunc []func(ctx context.Context, toolName string, args map[string]any) error

	afterExecuteID   []HookID
	afterExecuteFunc []func(ctx context.Context, toolName string, result schema.ToolResult, duration time.Duration)

	onRegisterID   []HookID
	onRegisterFunc []func(tool *RegisteredTool)

	onUnregisterID   []HookID
	onUnregisterFunc []func(toolName string)
}

// NewToolRegistry 创建工具注册中心
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]*RegisteredTool),
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

	if meta.Version == "" {
		meta.Version = "1.0.0"
	}
	if meta.Timeout == 0 {
		meta.Timeout = 30 * time.Second
	}
	meta.Enabled = true

	tool := &RegisteredTool{
		Metadata:     meta,
		Handler:      handler,
		RegisteredAt: time.Now(),
		UpdatedAt:    time.Now(),
	}

	r.tools[meta.Name] = tool

	for _, hook := range r.onRegisterFunc {
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

	_, exists := r.tools[toolName]
	if !exists {
		return fmt.Errorf("tool %s not found", toolName)
	}

	delete(r.tools, toolName)

	for _, hook := range r.onUnregisterFunc {
		hook(toolName)
	}

	return nil
}

// Update 更新工具
func (r *ToolRegistry) Update(toolName string, meta ToolMetadata, handler ToolHandler) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	tool, exists := r.tools[toolName]
	if !exists {
		return fmt.Errorf("tool %s not found", toolName)
	}

	tool.Metadata = meta
	if handler != nil {
		tool.Handler = handler
	}
	tool.UpdatedAt = time.Now()

	return nil
}

// Get 获取工具
func (r *ToolRegistry) Get(toolName string) (*RegisteredTool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, ok := r.tools[toolName]
	return tool, ok
}

// GetEnabledTools 获取所有启用的工具 Schema（发给模型用）
func (r *ToolRegistry) GetEnabledTools() []schema.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []schema.Tool
	for _, t := range r.tools {
		if t.Metadata.Enabled {
			result = append(result, schema.Tool{
				Name:        t.Metadata.Name,
				Description: t.Metadata.Description,
				Parameters:  t.Metadata.Parameters,
			})
		}
	}
	return result
}

// GetAllTools 获取所有工具
func (r *ToolRegistry) GetAllTools() []*RegisteredTool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*RegisteredTool, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

// GetByCategory 按分类获取
func (r *ToolRegistry) GetByCategory(category string) []*RegisteredTool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*RegisteredTool
	for _, t := range r.tools {
		if t.Metadata.Category == category {
			result = append(result, t)
		}
	}
	return result
}

// GetByPermission 按权限级别获取
func (r *ToolRegistry) GetByPermission(perm ToolPermission) []*RegisteredTool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*RegisteredTool
	for _, t := range r.tools {
		if t.Metadata.Permission == perm {
			result = append(result, t)
		}
	}
	return result
}

// GetByTag 按标签搜索
func (r *ToolRegistry) GetByTag(tag string) []*RegisteredTool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*RegisteredTool
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

// Enable 启用工具
func (r *ToolRegistry) Enable(toolName string) error {
	return r.setEnabled(toolName, true)
}

// Disable 禁用工具
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
func (r *ToolRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	start := time.Now()

	r.mu.RLock()
	tool, ok := r.tools[call.Function.Name]
	r.mu.RUnlock()

	if !ok {
		return schema.NewToolResult(call.ID, fmt.Sprintf(`{"error": "tool %s not found"}`, call.Function.Name), true)
	}

	if !tool.Metadata.Enabled {
		return schema.NewToolResult(call.ID, fmt.Sprintf(`{"error": "tool %s is disabled"}`, call.Function.Name), true)
	}

	args, err := parseToolArgs(call)
	if err != nil {
		return schema.NewToolResult(call.ID, fmt.Sprintf(`{"error": "%s"}`, err.Error()), true)
	}

	for _, hook := range r.beforeExecuteFunc {
		if err := hook(ctx, call.Function.Name, args); err != nil {
			return schema.NewToolResult(call.ID, fmt.Sprintf(`{"error": "before execute hook failed: %s"}`, err.Error()), true)
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, tool.Metadata.Timeout)
	defer cancel()

	output, err := tool.Handler(execCtx, args)
	duration := time.Since(start)

	r.mu.Lock()
	tool.UseCount++
	r.mu.Unlock()

	var result schema.ToolResult
	if err != nil {
		result = schema.NewToolResult(call.ID, fmt.Sprintf(`{"error": "%s"}`, err.Error()), true)
	} else {
		content := marshalOutput(output)
		result = schema.NewToolResult(call.ID, content, false)
	}

	for _, hook := range r.afterExecuteFunc {
		hook(ctx, call.Function.Name, result, duration)
	}

	return result
}

// ExecuteBatch 批量执行
func (r *ToolRegistry) ExecuteBatch(ctx context.Context, calls []schema.ToolCall) []schema.ToolResult {
	if len(calls) == 0 {
		return nil
	}

	if len(calls) == 1 {
		return []schema.ToolResult{r.Execute(ctx, calls[0])}
	}

	var wg sync.WaitGroup
	results := make([]schema.ToolResult, len(calls))

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c schema.ToolCall) {
			defer wg.Done()
			results[idx] = r.Execute(ctx, c)
		}(i, call)
	}

	wg.Wait()
	return results
}

// ToToolMessages 将 ToolResult 转成 Message
func (r *ToolRegistry) ToToolMessages(results []schema.ToolResult) []*schema.Message {
	return schema.ToolResultsMessage(results)
}

// ============================================================================
// 生命周期钩子
// ============================================================================

// AddBeforeExecuteHook 注册 beforeExecute 钩子，返回 ID
func (r *ToolRegistry) AddBeforeExecuteHook(hook func(ctx context.Context, toolName string, args map[string]any) error) HookID {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextHookID++
	id := r.nextHookID
	r.beforeExecuteID = append(r.beforeExecuteID, id)
	r.beforeExecuteFunc = append(r.beforeExecuteFunc, hook)
	return id
}

// RemoveBeforeExecuteHook 移除 beforeExecute 钩子
func (r *ToolRegistry) RemoveBeforeExecuteHook(id HookID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, hid := range r.beforeExecuteID {
		if hid == id {
			r.beforeExecuteID = append(r.beforeExecuteID[:i], r.beforeExecuteID[i+1:]...)
			r.beforeExecuteFunc = append(r.beforeExecuteFunc[:i], r.beforeExecuteFunc[i+1:]...)
			return
		}
	}
}

// AddAfterExecuteHook 注册 afterExecute 钩子，返回 ID
func (r *ToolRegistry) AddAfterExecuteHook(hook func(ctx context.Context, toolName string, result schema.ToolResult, duration time.Duration)) HookID {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextHookID++
	id := r.nextHookID
	r.afterExecuteID = append(r.afterExecuteID, id)
	r.afterExecuteFunc = append(r.afterExecuteFunc, hook)
	return id
}

// RemoveAfterExecuteHook 移除 afterExecute 钩子
func (r *ToolRegistry) RemoveAfterExecuteHook(id HookID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, hid := range r.afterExecuteID {
		if hid == id {
			r.afterExecuteID = append(r.afterExecuteID[:i], r.afterExecuteID[i+1:]...)
			r.afterExecuteFunc = append(r.afterExecuteFunc[:i], r.afterExecuteFunc[i+1:]...)
			return
		}
	}
}

// AddOnRegisterHook 注册 onRegister 钩子，返回 ID
func (r *ToolRegistry) AddOnRegisterHook(hook func(tool *RegisteredTool)) HookID {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextHookID++
	id := r.nextHookID
	r.onRegisterID = append(r.onRegisterID, id)
	r.onRegisterFunc = append(r.onRegisterFunc, hook)
	return id
}

// RemoveOnRegisterHook 移除 onRegister 钩子
func (r *ToolRegistry) RemoveOnRegisterHook(id HookID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, hid := range r.onRegisterID {
		if hid == id {
			r.onRegisterID = append(r.onRegisterID[:i], r.onRegisterID[i+1:]...)
			r.onRegisterFunc = append(r.onRegisterFunc[:i], r.onRegisterFunc[i+1:]...)
			return
		}
	}
}

// AddOnUnregisterHook 注册 onUnregister 钩子，返回 ID
func (r *ToolRegistry) AddOnUnregisterHook(hook func(toolName string)) HookID {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextHookID++
	id := r.nextHookID
	r.onUnregisterID = append(r.onUnregisterID, id)
	r.onUnregisterFunc = append(r.onUnregisterFunc, hook)
	return id
}

// RemoveOnUnregisterHook 移除 onUnregister 钩子
func (r *ToolRegistry) RemoveOnUnregisterHook(id HookID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, hid := range r.onUnregisterID {
		if hid == id {
			r.onUnregisterID = append(r.onUnregisterID[:i], r.onUnregisterID[i+1:]...)
			r.onUnregisterFunc = append(r.onUnregisterFunc[:i], r.onUnregisterFunc[i+1:]...)
			return
		}
	}
}

// ============================================================================
// 内部辅助函数
// ============================================================================

func parseToolArgs(call schema.ToolCall) (map[string]any, error) {
	var args map[string]any
	if call.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, fmt.Errorf("parse arguments failed: %w", err)
		}
	}
	return args, nil
}

func marshalOutput(output any) string {
	switch v := output.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf(`{"error": "marshal failed: %s"}`, err.Error())
		}
		return string(data)
	}
}
