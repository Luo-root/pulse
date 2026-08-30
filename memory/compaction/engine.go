package compaction

import (
	"context"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/llm"
)

// SummarizeInput 是一次摘要请求：选区消息（fold 后 surface 的窗口切片）
// 与目标预算（提示词参考值，不构成硬保证——摘要长度由 summarizer 决定）。
type SummarizeInput struct {
	Messages     []*llm.Message
	BudgetTokens int
}

// SummarizeResult 是摘要产物：RoleUser 稳定前缀消息（fold 语义由 session
// 的 codec 钉死——checkpoint 载荷里 Role 非 user/assistant/tool 会被拒）
// + 溯源字段（审计写进 compaction.summarized）。
type SummarizeResult struct {
	Summary llm.Message
	Model   string
	Usage   llm.TokenUsage
}

// Engine 是 CompactionEngine seam（§7.2）：把选区压成一条摘要。
// basic backend 两个：LLMSummarizer（真实模型）与
// DeterministicSummarizer（无模型 fallback）。
type Engine interface {
	Summarize(ctx context.Context, in SummarizeInput) (SummarizeResult, error)
}

// LLMSummarizer 用 llm.ChatModel 出摘要。Model 为 nil 时 Summarize 报错
// 不静默（没有摘要来源就不能假装压缩成功）。
type LLMSummarizer struct {
	Model     llm.ChatModel
	ModelName string
}

// Summarize 实现 Engine：把选区序列成带角色标记的文本，请模型总结。
// 指令与内容都走 user 消息（system 归宿主/Assembler，本包不注入）。
func (s *LLMSummarizer) Summarize(ctx context.Context, in SummarizeInput) (SummarizeResult, error) {
	if s.Model == nil {
		return SummarizeResult{}, fmt.Errorf("compaction: llm summarizer has no model")
	}
	var b strings.Builder
	b.WriteString("将以下对话历史压缩成一段摘要，保留未决事项、关键决定与工具结果要点：\n\n")
	for i, m := range in.Messages {
		if m == nil {
			continue
		}
		fmt.Fprintf(&b, "[%d] %s: %s\n", i, m.Role, m.Text())
	}
	req := &llm.GenerateRequest{Messages: []*llm.Message{llm.UserText(b.String())}}
	if in.BudgetTokens > 0 {
		max := in.BudgetTokens
		req.MaxTokens = &max
	}
	resp, err := s.Model.Generate(ctx, req)
	if err != nil {
		return SummarizeResult{}, fmt.Errorf("compaction: summarize: %w", err)
	}
	if resp == nil || resp.Message == nil {
		return SummarizeResult{}, fmt.Errorf("compaction: summarize returned empty response")
	}
	return SummarizeResult{
		Summary: llm.Message{Role: llm.RoleUser, Parts: []llm.Part{llm.Text(resp.Message.Text())}},
		Model:   s.ModelName,
		Usage:   resp.Usage,
	}, nil
}

// DeterministicSummarizer 是无模型 fallback：按序拼接选区文本的确定性
// 摘要（不调模型、结果可复现）。适合测试与「模型不可用时的降级压缩」。
type DeterministicSummarizer struct{}

// Summarize 实现 Engine。
func (DeterministicSummarizer) Summarize(ctx context.Context, in SummarizeInput) (SummarizeResult, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "[compacted %d messages]", len(in.Messages))
	for i, m := range in.Messages {
		if m == nil {
			continue
		}
		fmt.Fprintf(&b, "\n[%d] %s: %s", i, m.Role, m.Text())
	}
	return SummarizeResult{
		Summary: llm.Message{Role: llm.RoleUser, Parts: []llm.Part{llm.Text(b.String())}},
		Model:   "deterministic",
	}, nil
}
