package compaction

import (
	"github.com/Luo-root/pulse/llm"
)

// Meter 估算一段模型消息的 token 量。实现可以是估算（CharMeter）也可以
// 是精确 tokenizer——接口只要求同一实现内自洽，压力阈值与它配对使用。
type Meter interface {
	Tokens(msgs []*llm.Message) int
}

// CharMeter 是零依赖的字符估算 Meter：按 rune 计数除以 CharsPerToken
// （默认 4）。不引 tokenizer 依赖（CGO-free、plan9/js 不锁死）；精确
// 计数由宿主提供自定义 Meter。
type CharMeter struct {
	// CharsPerToken 是每 token 的平均字符数；<=0 时取默认 4。
	CharsPerToken int
}

// Tokens 实现 Meter。
func (m CharMeter) Tokens(msgs []*llm.Message) int {
	per := m.CharsPerToken
	if per <= 0 {
		per = 4
	}
	total := 0
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		for _, p := range msg.Parts {
			switch p.Kind {
			case llm.PartText, llm.PartReasoning:
				total += len([]rune(p.Text))
			case llm.PartToolCall:
				if p.ToolCallValue != nil {
					total += len([]rune(p.ToolCallValue.Name)) + len([]rune(string(p.ToolCallValue.Arguments)))
				}
			case llm.PartToolResult:
				if p.ToolResultValue != nil {
					for _, c := range p.ToolResultValue.Content {
						total += len([]rune(c.Text))
					}
				}
			}
		}
	}
	return total / per
}

// Pressure 报告当前消息集是否超过 token 预算阈值——压力 compact 的触发
// 判据。overflow retry 的循环编排归装配层（§12 P2-B：本包提供 hook 点）。
func Pressure(m Meter, msgs []*llm.Message, threshold int) bool {
	return m.Tokens(msgs) > threshold
}
