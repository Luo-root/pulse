package reflection

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/candidate"
	"github.com/Luo-root/pulse/memory/store"
)

// ReflectorKey 是 memory/reflection 的 kernel 服务键（memory/* 先例）。
var ReflectorKey = kernel.NewServiceKey[*Reflector]("memory.reflection")

// Options 是 Reflector 装配项。
type Options struct {
	// Pipeline 是候选管线（必填——反思输出只到候选：Reflect = Extract，
	// 不自动 Approve/Reject，审批人盖章；模型路由由 Pipeline 的
	// Extractor seam 承担，本包不重复注入）。
	Pipeline *candidate.Pipeline
	// MaxInputChars 是单次反射的输入字符预算（rune 计，口径对齐
	// compaction.CharMeter：Text/Reasoning + ToolCall Name/Arguments +
	// ToolResult Content Text）；0 = 不限。超限从头部丢弃整条消息（尾部
	// 保留——提取看近期内容；整条为粒度：不截半条消息，tool pairing
	// 结构完整、rune 安全自动满足）。
	MaxInputChars int
}

// ReflectionResult 是一次反射的审计结果（§10.3「反思进程本身有审计」：
// 本包不 import observability——宿主可把此值桥进观测面，request.usage
// 同先例）。
type ReflectionResult struct {
	// Items 是本轮入库的 Pending 候选（透传 Extract——宿主审批面可直接
	// 展示）。
	Items []store.MemoryItem
	// Report 是本轮提炼计数（透传 Extract：Extracted/Stored/Duplicates/
	// Invalid——Pipeline.Metrics 另有累计面）。
	Report candidate.Report
	// InputChars 是本轮实际喂入 Extract 的字符数（截断后）。
	InputChars int
	// TruncatedChars 是预算截断丢弃的字符数（0 = 未截断）。
	TruncatedChars int
}

// Metrics 是 Reflector 的累计计数快照（token 成本 v1 口径：Runs + 字符
// 数；真实 LLM usage 不进 seam——由宿主从自己的 client 拿、装配层桥写，
// compaction.request.usage 同口径）。
type Metrics struct {
	// Runs 是累计成功反射轮数（错误轮不计数——计数只反映完整成功轮）。
	Runs uint64
	// TotalInputChars 是累计喂入 Extract 的字符数。
	TotalInputChars uint64
	// TruncatedChars 是累计截断丢弃字符数。
	TruncatedChars uint64
}

// Reflector 是 background reflection 编排组件（§10.3）：输入截断（预算
// 门）→ Pipeline.Extract（候选入库）→ 计数 → 审计结果。
//
// 同步单次执行：**无后台循环、无计时器**——触发时机归宿主（会话末/
// 每 N 轮/空闲钩子，candidate「调用时机归宿主」的兑现）；§10.3 的并发
// 上限由调用方控制（包内 -race 安全）；不 import session（surface 由宿
// 主从 session 取出喂入——compaction 依赖 session 是因为要 fold/写回，
// 本包只读输入，零依赖更薄）。
type Reflector struct {
	opt             Options
	runs            atomic.Uint64
	totalInputChars atomic.Uint64
	truncatedChars  atomic.Uint64
}

// New 创建 Reflector（显式装配；默认关——不 New 不运行、零成本）。
func New(opt Options) (*Reflector, error) {
	if opt.Pipeline == nil {
		return nil, fmt.Errorf("reflection: pipeline is required")
	}
	if opt.MaxInputChars < 0 {
		return nil, fmt.Errorf("reflection: negative max input chars")
	}
	return &Reflector{opt: opt}, nil
}

// Reflect 执行一次反思：截断 → Extract → 计数。错误透传不静默（错误轮
// 不计数；候选对检索天然不可见——store 默认只 Active）。
func (r *Reflector) Reflect(ctx context.Context, surface []*llm.Message) (ReflectionResult, error) {
	input, truncated := truncateTail(surface, r.opt.MaxInputChars)
	res := ReflectionResult{InputChars: charsOf(input), TruncatedChars: truncated}
	items, report, err := r.opt.Pipeline.Extract(ctx, input)
	if err != nil {
		return res, err
	}
	res.Items = items
	res.Report = report
	r.runs.Add(1)
	r.totalInputChars.Add(uint64(res.InputChars))
	r.truncatedChars.Add(uint64(truncated))
	return res, nil
}

// Metrics 返回累计计数快照（atomic 读，-race 安全）。
func (r *Reflector) Metrics() Metrics {
	return Metrics{
		Runs:            r.runs.Load(),
		TotalInputChars: r.totalInputChars.Load(),
		TruncatedChars:  r.truncatedChars.Load(),
	}
}

// msgChars 计单条消息的字符数（rune；口径对齐 compaction.CharMeter 的
// 计数集合——Text/Reasoning + ToolCall Name/Arguments + ToolResult
// Content Text；nil 消息计 0）。
func msgChars(msg *llm.Message) int {
	if msg == nil {
		return 0
	}
	total := 0
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
	return total
}

// charsOf 计消息集总字符数（rune；口径同 msgChars）。
func charsOf(msgs []*llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += msgChars(m)
	}
	return total
}

// truncateTail 预算截断：从头部丢弃整条消息直到总字符 ≤ max（尾部保留
// ——提取看近期内容）。整条消息为丢弃粒度：不截半条消息（tool pairing
// 结构完整、多字节字符不截半——rune 安全由「不劈消息」免费保证）。
// max<=0 原样返回（不限）；至少保留最后一条消息（空 surface 送提取无
// 意义；末条自身超预算时整条保留）。返回保留消息与丢弃字符数。
func truncateTail(msgs []*llm.Message, max int) ([]*llm.Message, int) {
	if max <= 0 || len(msgs) == 0 {
		return msgs, 0
	}
	kept := 0 // 从尾部起已保留的消息数
	budget := max
	for i := len(msgs) - 1; i >= 0; i-- {
		c := msgChars(msgs[i])
		if c > budget && kept > 0 {
			break // 预算用尽：这条（及更早的）丢弃
		}
		// kept == 0 时即使超预算也保留（至少保最后一条，不截半条）
		budget -= c
		kept++
	}
	if kept == len(msgs) {
		return msgs, 0
	}
	return msgs[len(msgs)-kept:], charsOf(msgs[:len(msgs)-kept])
}
