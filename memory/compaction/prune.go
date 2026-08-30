package compaction

import (
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/llm"
)

// PruneOptions 是 §9.2 tool result deterministic pruning 的参数：超
// MaxRunes 的结果按 head + marker + tail 裁剪（rune 安全，不做字节级
// 截断）。零值取默认：Max 4000 / Head 2400 / Tail 800。
type PruneOptions struct {
	MaxRunes  int
	HeadRunes int
	TailRunes int
}

func (o PruneOptions) normalized() PruneOptions {
	if o.MaxRunes <= 0 {
		o.MaxRunes = 4000
	}
	if o.HeadRunes <= 0 || o.HeadRunes >= o.MaxRunes {
		o.HeadRunes = o.MaxRunes * 3 / 5
	}
	if o.TailRunes <= 0 || o.HeadRunes+o.TailRunes >= o.MaxRunes {
		o.TailRunes = (o.MaxRunes - o.HeadRunes) / 2
	}
	return o
}

const pruneMarkerFmt = "\n…[pruned %d runes; full text kept in raw log]…\n"

// pruneText 对单条文本做 head + marker + tail 裁剪，返回裁剪结果与是否
// 发生裁剪。rune 切片操作保证多字节字符不被劈开（§9.2：不能只按字符
// 截断 JSON / UTF-8）。
func pruneText(s string, opts PruneOptions) (string, bool) {
	runes := []rune(s)
	if len(runes) <= opts.MaxRunes {
		return s, false
	}
	head := string(runes[:opts.HeadRunes])
	tail := string(runes[len(runes)-opts.TailRunes:])
	marker := fmt.Sprintf(pruneMarkerFmt, len(runes)-opts.HeadRunes-opts.TailRunes)
	return head + marker + tail, true
}

// PruneResult 裁剪一个 tool 消息：返回替代节点（同 RoleTool，文本为
// head+marker+tail）与是否发生裁剪。结构化字段（IsError、ToolCallID）
// 保留；原文完整保存在 raw log，UI 可展开（§9.2）。
func PruneResult(m *llm.Message, opts PruneOptions) (*llm.Message, bool) {
	opts = opts.normalized()
	if m == nil || m.Role != llm.RoleTool {
		return m, false
	}
	out := *m
	changed := false
	out.Parts = make([]llm.Part, len(m.Parts))
	for i, p := range m.Parts {
		if p.Kind != llm.PartToolResult || p.ToolResultValue == nil {
			out.Parts[i] = p
			continue
		}
		part := *p.ToolResultValue
		part.Content = make([]llm.Part, len(p.ToolResultValue.Content))
		for j, c := range p.ToolResultValue.Content {
			if text, pruned := pruneText(c.Text, opts); pruned {
				c.Text = text
				changed = true
			}
			part.Content[j] = c
		}
		p.ToolResultValue = &part
		out.Parts[i] = p
	}
	if !changed {
		return m, false
	}
	return &out, true
}

// OversizedToolNodes 返回 surface 中超预算的 tool 节点下标（§9.2 的
// 选区判定：只对 RoleTool 且文本超 MaxRunes 的节点做 pruning）。
func OversizedToolNodes(msgs []*llm.Message, opts PruneOptions) []int {
	opts = opts.normalized()
	var out []int
	for i, m := range msgs {
		if m == nil || m.Role != llm.RoleTool {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind == llm.PartToolResult && p.ToolResultValue != nil {
				for _, c := range p.ToolResultValue.Content {
					if len([]rune(c.Text)) > opts.MaxRunes {
						out = append(out, i)
						break
					}
				}
			}
		}
	}
	return out
}

// pruneMarker 供测试断言形态。
func pruneMarker(pruned int) string {
	return strings.TrimSpace(fmt.Sprintf(pruneMarkerFmt, pruned))
}
