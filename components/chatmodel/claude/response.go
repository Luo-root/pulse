package claude

// ClaudeMessageResponse Anthropic Messages API 非流式响应
type ClaudeMessageResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"` // "message"
	Role       string         `json:"role"` // "assistant"
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`     // ✅ 核心：混合内容块数组
	StopReason string         `json:"stop_reason"` // end_turn | max_tokens | tool_use | stop_sequence
	StopSeq    string         `json:"stop_sequence,omitempty"`
	Usage      ClaudeUsage    `json:"usage"`
}

// ClaudeUsage Anthropic token 用量
type ClaudeUsage struct {
	InputTokens              uint64 `json:"input_tokens"`
	OutputTokens             uint64 `json:"output_tokens"`
	CacheCreationInputTokens uint64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     uint64 `json:"cache_read_input_tokens,omitempty"`
}

// ContentBlock Anthropic 内容块（请求/响应共用）
type ContentBlock struct {
	Type      string      `json:"type"` // text | tool_use | tool_result | thinking
	Text      string      `json:"text,omitempty"`
	ID        string      `json:"id,omitempty"`          // tool_use 块的 ID
	Name      string      `json:"name,omitempty"`        // tool_use 块的工具名
	Input     interface{} `json:"input,omitempty"`       // tool_use 块参数（JSON object）
	ToolUseID string      `json:"tool_use_id,omitempty"` // tool_result 块关联的 tool_use ID
	Content   interface{} `json:"content,omitempty"`     // tool_result / thinking 的内容
	IsError   bool        `json:"is_error,omitempty"`    // tool_result 错误标记
}

// ClaudeTool Anthropic 工具定义
type ClaudeTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"input_schema"`
}

// InputSchema JSON Schema 参数定义
type InputSchema struct {
	Type       string                 `json:"type"` // 固定 "object"
	Properties map[string]interface{} `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
}

// ClaudeToolChoice 工具选择策略
type ClaudeToolChoice struct {
	Type                string `json:"type"`                                // auto | any | tool | none
	Name                string `json:"name,omitempty"`                      // type=tool 时指定工具名
	DisableParallelTool bool   `json:"disable_parallel_tool_use,omitempty"` // 禁用并行工具调用
}

// ClaudeThinkingConfig 思考配置（对应 Anthropic thinking 字段）
type ClaudeThinkingConfig struct {
	Type         string `json:"type"`                    // "enabled" | "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // 思考预算（DeepSeek 忽略）
}

// ClaudeOutputConfig 输出配置
type ClaudeOutputConfig struct {
	Effort string `json:"effort,omitempty"` // low | medium | high（仅 DeepSeek 支持 effort）
}
