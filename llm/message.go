package llm

import (
	"encoding/json"
	"strings"
)

// Role 是消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool 承载工具执行结果。采用独立角色以贴近 OpenAI 线格式；
	// Anthropic 风格的 adapter 将其转换为 user 消息中的 tool_result block。
	RoleTool Role = "tool"
)

// PartKind 区分内容块类型。
type PartKind string

const (
	PartText       PartKind = "text"
	PartImage      PartKind = "image"
	PartToolCall   PartKind = "tool_call"   // 仅出现在 assistant 消息
	PartToolResult PartKind = "tool_result" // 仅出现在 tool/user 消息
	PartReasoning  PartKind = "reasoning"   // 思维链（推理模型）
)

// ImageSource 描述图像内容：内联字节或 URL 二选一。
type ImageSource struct {
	Data     []byte // 内联字节（优先使用）
	URL      string // http(s):// 或 data: URI
	MIMEType string
}

// ToolCall 是一次工具调用请求（由模型发起）。
type ToolCall struct {
	ID        string          // 调用标识，回传结果时引用
	Name      string          // 工具名
	Arguments json.RawMessage // 参数 JSON
}

// ToolResult 是一次工具调用的执行结果（回传给模型）。
type ToolResult struct {
	ToolCallID string
	Content    []Part // 结果内容，通常为一个文本块
	IsError    bool   // true 表示工具执行失败（模型可据此自我修正）
}

// Part 是消息的内容块。Kind 决定哪个字段有效：
//
//	PartText / PartReasoning => Text
//	PartImage                => Image
//	PartToolCall             => ToolCallValue
//	PartToolResult           => ToolResultValue
type Part struct {
	Kind            PartKind
	Text            string
	Image           *ImageSource
	ToolCallValue   *ToolCall
	ToolResultValue *ToolResult
}

// ---- Part 构造器 ----

// Text 构造文本块。
func Text(s string) Part { return Part{Kind: PartText, Text: s} }

// Reasoning 构造思维链块。
func Reasoning(s string) Part { return Part{Kind: PartReasoning, Text: s} }

// ImageURL 用 URL 构造图像块。
func ImageURL(url, mimeType string) Part {
	return Part{Kind: PartImage, Image: &ImageSource{URL: url, MIMEType: mimeType}}
}

// ImageData 用内联字节构造图像块。
func ImageData(mimeType string, data []byte) Part {
	return Part{Kind: PartImage, Image: &ImageSource{Data: data, MIMEType: mimeType}}
}

// Call 构造工具调用块。
func Call(tc ToolCall) Part { return Part{Kind: PartToolCall, ToolCallValue: &tc} }

// Result 构造工具结果块（快捷：单条文本结果）。
func Result(toolCallID, text string) Part {
	return Part{Kind: PartToolResult, ToolResultValue: &ToolResult{
		ToolCallID: toolCallID,
		Content:    []Part{Text(text)},
	}}
}

// ResultParts 构造多块内容的工具结果。
func ResultParts(toolCallID string, isError bool, parts ...Part) Part {
	return Part{Kind: PartToolResult, ToolResultValue: &ToolResult{
		ToolCallID: toolCallID,
		Content:    parts,
		IsError:    isError,
	}}
}

// Message 是一条对话消息：角色 + 内容块序列。
type Message struct {
	Role  Role
	Parts []Part
	// Name 可选的参与者名（多角色场景），provider 不支持则忽略。
	Name string
}

// ---- Message 构造器 ----

// System 构造系统消息。
func System(text string) *Message {
	return &Message{Role: RoleSystem, Parts: []Part{Text(text)}}
}

// User 构造用户消息。
func User(parts ...Part) *Message {
	return &Message{Role: RoleUser, Parts: parts}
}

// UserText 构造纯文本用户消息。
func UserText(text string) *Message { return User(Text(text)) }

// Assistant 构造助手消息。
func Assistant(parts ...Part) *Message {
	return &Message{Role: RoleAssistant, Parts: parts}
}

// AssistantText 构造纯文本助手消息。
func AssistantText(text string) *Message { return Assistant(Text(text)) }

// ToolMessage 构造工具结果消息（快捷：单调用单文本）。
func ToolMessage(toolCallID, text string) *Message {
	return &Message{Role: RoleTool, Parts: []Part{Result(toolCallID, text)}}
}

// ---- Message 读取辅助 ----

// Text 拼接全部文本块（含思维链之外的正文），以换行连接。
func (m *Message) Text() string {
	var b strings.Builder
	for i, p := range m.Parts {
		if p.Kind == PartText {
			if i > 0 && b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ReasoningText 拼接全部思维链块；无则返回空串。
func (m *Message) ReasoningText() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Kind == PartReasoning {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ToolCalls 返回本消息携带的全部工具调用。
func (m *Message) ToolCalls() []ToolCall {
	var out []ToolCall
	for _, p := range m.Parts {
		if p.Kind == PartToolCall && p.ToolCallValue != nil {
			out = append(out, *p.ToolCallValue)
		}
	}
	return out
}

// Clone 深拷贝顶层结构（Part 内的指针共享——消息按约定不可变，
// 需要改写时调用方自行复制对应块）。
func (m *Message) Clone() *Message {
	cp := *m
	cp.Parts = append([]Part{}, m.Parts...)
	return &cp
}
