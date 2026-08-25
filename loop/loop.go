package loop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
)

// StopReason 解释回合结束的原因。
type StopReason string

const (
	// StopCompleted：模型不再发起工具调用，回合自然结束。
	StopCompleted StopReason = "completed"
	// StopMaxSteps：达到 MaxSteps 上限被安全阀打断（err 为 nil，
	// Result 如实返回最后状态）。
	StopMaxSteps StopReason = "max_steps"
	// StopCanceled：ctx 取消（Run 同时返回 ctx.Err()）。
	StopCanceled StopReason = "canceled"
	// StopError：基础设施失败（模型调用失败、流异常终止）；
	// Run 同时返回包装后的错误。
	StopError StopReason = "error"
)

// Result 是一个回合的完整产出。
type Result struct {
	// Messages 是**本回合新产生的消息**：assistant 与 tool 结果交替，
	// 以最终 assistant 收尾；不含 system、调用方传入的 history 与 input。
	// 多轮对话 = 调用方自行 append(input, res.Messages...) 到自己的历史。
	Messages []*llm.Message
	// Final 是最后的 assistant 消息；MaxSteps 打断时可能是带未执行
	// 工具调用的中间态——以 StoppedBy 区分。
	Final *llm.Message
	// Usage 是本回合全部模型调用的用量累计。
	Usage llm.TokenUsage
	// Steps 是实际执行的推理-行动步数。
	Steps int
	// StoppedBy 解释终止原因。
	StoppedBy StopReason
}

// Agent 是无状态的回合执行器：实例只是配置与依赖引用（不可变，
// 无锁），并发 Run 共享同一实例是安全的——下游依赖（ChatModel、
// ToolSet、事件作用域）需自行保证并发安全。
type Agent struct {
	model    llm.ChatModel
	tools    ToolSet
	system   string
	maxSteps int // <=0 表示不限制
	scope    *kernel.Context
}

// Option 配置 Agent。
type Option func(*Agent)

// WithToolSet 注入工具集合。nil 表示纯对话（不暴露任何工具）。
func WithToolSet(ts ToolSet) Option { return func(a *Agent) { a.tools = ts } }

// WithSystemPrompt 设置系统提示词（每次回合组装为首条消息）。
func WithSystemPrompt(s string) Option { return func(a *Agent) { a.system = s } }

// WithMaxSteps 设置单回合推理-行动步数上限。
//
// 默认 0 = 不限制：几十上百次工具调用的长任务不应被打断。需要
// 安全阀的场景（不可信模型、成本敏感、嵌入常驻进程）再显式设置；
// 触发上限不是错误——Result 以 StoppedBy=max_steps 如实返回。
func WithMaxSteps(n int) Option {
	return func(a *Agent) {
		if n > 0 {
			a.maxSteps = n
		} else {
			a.maxSteps = 0 // 显式归一：负值同样表示不限制
		}
	}
}

// WithEventScope 指定事件派发作用域。缺省为 nil——不向任何作用域
// 派发（纯库用法零内核足迹）；要让轨迹进入宿主作用域树（例如与
// 限流插件同树、被轨迹监听器记录），传入宿主或其子作用域。
func WithEventScope(scope *kernel.Context) Option {
	return func(a *Agent) { a.scope = scope }
}

// NewAgent 创建回合执行器。model 必填；工具集、提示词等经 Option 注入。
func NewAgent(model llm.ChatModel, opts ...Option) (*Agent, error) {
	if model == nil {
		return nil, errors.New("loop: model is required")
	}
	a := &Agent{model: model, maxSteps: 0}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// emit 是 scope nil 安全的事件派发。
func emit[P any](scope *kernel.Context, k kernel.EventKey[P], payload P) {
	if scope == nil {
		return
	}
	kernel.Emit(scope, k, payload)
}

// waterfallOf 是 scope nil 安全的 waterfall 派发。
func waterfallOf[P any](scope *kernel.Context, k kernel.EventKey[P], payload P) P {
	if scope == nil {
		return payload
	}
	return kernel.Waterfall(scope, k, payload)
}

// Run 执行一个回合（非流式便捷入口，等价于不带 onDelta 的 RunStream）。
func (a *Agent) Run(ctx context.Context, history []*llm.Message, input ...*llm.Message) (*Result, error) {
	return a.RunStream(ctx, nil, history, input...)
}

// RunStream 执行一个回合：onDelta 非 nil 时逐段回调 assistant 文本
// 增量（流式 UI 用），结构化轨迹走内核事件总线。
//
// 无论以何种方式结束（完成 / MaxSteps / 取消 / 错误）都会发出
// turn_end 事件——轨迹保证闭合。返回的 error 仅在基础设施失败
// （模型调用失败、ctx 取消、流异常终止）时非 nil，此时 Result 仍
// 返回已发生的部分（StoppedBy=canceled/error）；MaxSteps 打断是
// 正常终止变体（见 StopMaxSteps）。
func (a *Agent) RunStream(ctx context.Context, onDelta func(text string), history []*llm.Message, input ...*llm.Message) (*Result, error) {
	emit(a.scope, EventTurnStart, TurnStart{Input: input, History: history})

	// msgs 是发往模型的完整工作缓冲；produced 只收集本回合新产生
	// 的消息（Result.Messages 的内容源）。
	msgs := make([]*llm.Message, 0, len(history)+len(input)+8) // 容量仅为扩容提示
	if a.system != "" {
		msgs = append(msgs, llm.System(a.system))
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, input...)

	var produced []*llm.Message

	var defs []llm.ToolDef
	if a.tools != nil {
		defs = a.tools.Definitions()
	}

	res := &Result{}

	// 统一收尾：任何退出路径（完成 / MaxSteps / 取消 / 错误）都
	// ① 带上已发生的部分产出（Messages 非 nil）；
	// ② 恰好发出一次 turn_end——轨迹保证闭合。
	defer func() {
		if res.Messages == nil {
			res.Messages = produced
		}
		if res.StoppedBy == "" {
			res.StoppedBy = StopError
		}
		emit(a.scope, EventTurnEnd, TurnEnd{
			Final: res.Final, Usage: res.Usage, Steps: res.Steps, StoppedBy: res.StoppedBy,
		})
	}()

	for step := 1; ; step++ {
		if err := ctx.Err(); err != nil {
			res.StoppedBy = StopCanceled
			return res, err
		}
		if a.maxSteps > 0 && step > a.maxSteps {
			res.StoppedBy = StopMaxSteps
			break
		}

		emit(a.scope, EventStepStart, StepStart{Step: step})
		req := &llm.GenerateRequest{Messages: msgs, Tools: defs}

		ch, err := a.model.Stream(ctx, req)
		if err != nil {
			res.StoppedBy = StopError
			return res, fmt.Errorf("loop: step %d: %w", step, err)
		}
		var resp *llm.Response
		for ev := range ch {
			switch ev.Kind {
			case llm.EventTextDelta:
				if onDelta != nil {
					onDelta(ev.Text)
				}
			case llm.EventError:
				res.StoppedBy = StopError
				return res, fmt.Errorf("loop: step %d: %w", step, ev.Err)
			case llm.EventDone:
				resp = ev.Response
			}
		}
		if resp == nil || resp.Message == nil {
			res.StoppedBy = StopError
			return res, fmt.Errorf("loop: step %d: stream ended without a response", step)
		}

		emit(a.scope, EventAfterModel, AfterModel{Response: resp, Step: step})
		res.Steps = step // 一步 = 一次完整的模型调用（无论后续是否调用工具）
		msgs = append(msgs, resp.Message)
		produced = append(produced, resp.Message)
		res.Usage.InputTokens += resp.Usage.InputTokens
		res.Usage.OutputTokens += resp.Usage.OutputTokens
		res.Usage.CachedInputTokens += resp.Usage.CachedInputTokens
		res.Final = resp.Message

		calls := resp.Message.ToolCalls()
		if len(calls) == 0 {
			res.StoppedBy = StopCompleted
			break
		}

		for _, call := range calls {
			start := time.Now()
			btc := &BeforeToolCall{Call: call}
			// 与 llm.before_generate 同一条 around 契约：监听器可能
			// Clone 改写后返回新载荷，必须以返回值为准——此后全程
			// 使用 btc.Call（改写后的名字与参数），原 call 不再引用。
			btc = waterfallOf(a.scope, EventBeforeToolCall, btc)
			effective := btc.Call

			if btc.Rejected {
				reason := btc.RejectReason
				if reason == "" {
					reason = "rejected by policy"
				}
				text := "tool call rejected: " + reason
				emit(a.scope, EventAfterToolCall, AfterToolCall{
					Call: effective, Result: text, Rejected: true, Duration: time.Since(start),
				})
				msgs = append(msgs, toolResultMsg(effective.ID, text, true))
				produced = append(produced, msgs[len(msgs)-1])
				continue
			}

			out, execErr := a.execute(ctx, effective)
			text := out
			if execErr != nil {
				text = "tool error: " + execErr.Error()
			}
			emit(a.scope, EventAfterToolCall, AfterToolCall{
				Call: effective, Result: text, Duration: time.Since(start), Err: execErr,
			})
			msgs = append(msgs, toolResultMsg(effective.ID, text, execErr != nil))
			produced = append(produced, msgs[len(msgs)-1])
		}
	}

	return res, nil
}

// execute 执行单个工具并统一错误形态：失败时把错误文本作为结果
// 回传给模型（可自我修正），panic 被恢复为错误。
func (a *Agent) execute(ctx context.Context, call llm.ToolCall) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out = ""
			err = fmt.Errorf("tool %q panicked: %v", call.Name, r)
		}
	}()
	if a.tools == nil {
		return "", fmt.Errorf("no tool set configured")
	}
	return a.tools.Execute(ctx, call)
}

// toolResultMsg 构造工具结果消息：isError=true 时文本应说明失败
// 原因（拒绝原因或错误详情），模型可据此自我修正。
func toolResultMsg(callID, text string, isError bool) *llm.Message {
	return &llm.Message{Role: llm.RoleTool, Parts: []llm.Part{
		llm.ResultParts(callID, isError, llm.Text(text)),
	}}
}
