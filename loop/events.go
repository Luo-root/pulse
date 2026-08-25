package loop

import (
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
)

// loop 层事件：agent 决策级的扩展点（审批、审计、轨迹还原）。
// 与 llm 层事件分工——token 级关注点（路由/限流/计量）挂
// llm.before_generate / llm.after_response，两层不重复。
var (
	EventTurnStart      = kernel.NewEventKey[TurnStart]("pulse.loop.turn_start")
	EventStepStart      = kernel.NewEventKey[StepStart]("pulse.loop.step_start")
	EventAfterModel     = kernel.NewEventKey[AfterModel]("pulse.loop.after_model")
	EventBeforeToolCall = kernel.NewEventKey[*BeforeToolCall]("pulse.loop.before_tool_call")
	EventAfterToolCall  = kernel.NewEventKey[AfterToolCall]("pulse.loop.after_tool_call")
	EventTurnEnd        = kernel.NewEventKey[TurnEnd]("pulse.loop.turn_end")
)

// TurnStart 是回合开始事件载荷。
type TurnStart struct {
	Input   []*llm.Message // 本轮新增输入
	History []*llm.Message // 之前的对话历史
}

// StepStart 是单个推理-行动步开始的事件载荷。
type StepStart struct {
	Step int
}

// AfterModel 是模型响应就绪的事件载荷。
type AfterModel struct {
	Response *llm.Response
	Step     int
}

// BeforeToolCall 是工具执行前的 waterfall 载荷。
//
// 监听器契约（around 语义）：
//   - 可就地改写 Call.Arguments / Call.Name；
//   - 置 Rejected = true 并直接返回（不再委托 next）即拒绝本次执行，
//     后续监听器与真实执行都被跳过，模型将收到一条 IsError 结果；
//   - 只观察则修改后务必调用 next 委托。
type BeforeToolCall struct {
	Call         llm.ToolCall
	Rejected     bool
	RejectReason string
}

// AfterToolCall 是工具执行完成的事件载荷。
type AfterToolCall struct {
	Call     llm.ToolCall
	Result   string        // 回传给模型的文本（失败时为错误说明）
	Duration time.Duration
	Err      error         // 工具自身的错误；nil 表示成功
	Rejected bool          // true 表示被 before_tool_call 拒绝，未真实执行
}

// TurnEnd 是回合结束的事件载荷。
type TurnEnd struct {
	Final     *llm.Message    // 最后一条 assistant 消息
	Usage     llm.TokenUsage  // 全回合用量累计
	Steps     int             // 实际执行的步数
	StoppedBy StopReason      // 终止原因
}
