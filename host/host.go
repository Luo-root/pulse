// Package host 是两层装配的第二层：只接跨包的缝——模型供应商注册、
// 工具来源注册、会话栈 ↔ loop 的三向接线（surface 注入 history / 请求级
// 事件 scope / 按 loop 事件同步落盘）、agent 构造与生命周期。
//
// 各包自己的基础装配不在本包：memory 会话栈/条目栈见 memory 包根级
// 门面（memory.NewSessionStack / memory.NewItemStack），llm.Registry /
// observability.Bootstrap / toolset/builtins.Register 各自是一站式入口。
// host 只收敛「把各包串起来」的知识。
//
// 零新抽象：供应商与工具来源都是函数类型，签名对齐各包既有 Register，
// openai.Register / 闭包版 builtins.Register 直接转换。
//
// 落盘契约（model-visible means logged）：会话持久化按 loop 事件同步
// Append——assistant 消息在工具执行与 HITL 审批**之前**已在日志里，
// 进程死在任意执行点，日志都停在真实现场（#158 冷恢复的官方来源）。
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/memory"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/observability"
	"github.com/Luo-root/pulse/toolset"
)

// Provider 把模型供应商适配器的注册函数装进 host。签名对齐
// llm/openai.Register 与 llm/anthropic.Register，直接转换即可：
//
//	host.Provider(openai.Register)
type Provider func(c *kernel.Context, reg *llm.Registry) error

// ToolSource 把工具来源装进 host 的 toolset.Registry。签名对齐
// toolset/builtins.Register 减去其 Options（用闭包携带）：
//
//	func(c *kernel.Context, reg *toolset.Registry) error {
//	    _, err := builtins.Register(c, reg, builtins.Options{Root: root})
//	    return err
//	}
type ToolSource func(c *kernel.Context, reg *toolset.Registry) error

// ToolGate 是工具执行前的宿主闸门（loop.EventBeforeToolCall waterfall
// 的挂载点）：返回 approved=false 即拒绝本次执行——模型收到 IsError
// 结果（含 reason），工具不会运行。审批 UI / 策略引擎经此接入
// DefaultAgent，这是 HITL 的最小官方挂点。
type ToolGate func(call llm.ToolCall) (approved bool, reason string)

// ObserveConfig 观测装配。Sink 为 nil = 不装观测（零开销）。
type ObserveConfig struct {
	// HostID 是观测记录的宿主标识（trace 归属）；空则用 "pulse-host"。
	HostID string
	Sink   observability.Sink
}

// ModelDecl 是一个模型声明：名字 + 配置。用切片不用 map——声明顺序
// 稳定可复现，Registry 的 Declare 顺序即宿主书写顺序。
type ModelDecl struct {
	Name   string
	Config llm.Config
}

// Options 是宿主装配参数：静态装配收敛常规场景，进阶场景经 Kernel()
// 用 kernel 原生语义。所有外部副作用（模型网络、工具执行、落盘）都是
// 显式 opt-in——不传就没有。
type Options struct {
	// Kernel 是挂载宿主组件的 kernel 根。**必填**，host 不私建内核——
	// 应用的其他插件（UI、审批、任务队列…）Use 到同一个 kernel 上即可
	// 与 host 组件共享服务仓库与事件总线。生命周期归所有者：Dispose
	// 由调用方负责。
	Kernel *kernel.Context
	// Providers 注册模型供应商适配器（先于 Models 声明执行）。
	Providers []Provider
	// Models 声明模型路由：Provider 字段须已在 Providers 里注册。
	Models []ModelDecl
	// Tools 注册工具来源（本地 builtins / MCP Source / 自定义）。
	Tools []ToolSource
	// Session 接会话栈（memory.NewSessionStack 的产物）；nil = 纯无状态
	// 回合（agent.Run 的 history 由调用方自管）。
	Session *memory.SessionStack
	// Observe 装观测双基座；Sink 为 nil = 不装。
	Observe ObserveConfig
}

// Host 是装配好的宿主：模型 Registry + 工具 Registry + 可选会话栈，全部
// 挂在**调用方注入的 kernel** 上——应用的其他插件（UI、审批、任务队列…）
// 与 host 组件共享同一个服务仓库与事件总线，互相可见、可订阅。kernel 的
// 生命周期归其所有者（Dispose 归调用方）；Host 自身无状态可复用。
type Host struct {
	ctx     *kernel.Context
	models  *llm.Registry
	tools   *toolset.Registry
	session *memory.SessionStack
	sink    observability.Sink
	hostID  string
}

// New 装配宿主：把观测、供应商、模型声明、工具来源挂到**注入的 kernel**
// 上。Kernel 必填——host 不私建内核，否则外部插件与 host 组件互相不可见。
//
// 失败语义：host 不拥有 kernel，任一步失败只返回 error、**不做** Dispose
// 兜底；已成功挂载的组件留在 kernel 上，随调用方的 kernel.Dispose() 统一
// 逆序回收（可逆效应语义由 kernel 保证）。失败通常是配置错误——丢弃本次
// 装配修正后重来即可。
func New(opt Options) (*Host, error) {
	c := opt.Kernel
	if c == nil {
		return nil, fmt.Errorf("host: kernel is required (inject the shared kernel root; host does not own it)")
	}
	h := &Host{ctx: c}
	if opt.Observe.Sink != nil {
		id := opt.Observe.HostID
		if id == "" {
			id = "pulse-host"
		}
		if _, err := kernel.Use(c, observability.Bootstrap(id, opt.Observe.Sink)); err != nil {
			return nil, fmt.Errorf("host: observability: %w", err)
		}
		h.sink, h.hostID = opt.Observe.Sink, id
	}
	reg := llm.NewRegistry(c)
	for i, p := range opt.Providers {
		if p == nil {
			continue
		}
		if err := p(c, reg); err != nil {
			return nil, fmt.Errorf("host: provider[%d]: %w", i, err)
		}
	}
	for _, m := range opt.Models {
		if err := reg.Declare(m.Name, m.Config); err != nil {
			return nil, fmt.Errorf("host: declare %q: %w", m.Name, err)
		}
	}
	h.models = reg

	tr := toolset.NewRegistry()
	for i, src := range opt.Tools {
		if src == nil {
			continue
		}
		if err := src(c, tr); err != nil {
			return nil, fmt.Errorf("host: tool source[%d]: %w", i, err)
		}
	}
	h.tools = tr
	h.session = opt.Session
	return h, nil
}

// AgentOptions 是 agent 的**全参数注入**形态（NewAgent 用）。
type AgentOptions struct {
	// Name 是 agent 标识（必填：随 loop 事件与观测记录发出，供同 scope
	// 多 Agent 归因）。
	Name string
	// Model 是任意 llm.ChatModel 来源（Registry 产出 / stub / 宿主自定义）。
	Model llm.ChatModel
	// ModelName 是模型的显示名（request.header 审计的 Model 字段；便捷
	// 方法填声明名）。接会话时**必填**——request.header codec 要求 Model
	// 非空，构造期校验，不留到回合中段才失败。
	ModelName string
	// ToolSet 是本 agent 的工具集；nil = 无工具（纯对话回合）。
	ToolSet loop.ToolSet
	// Session 是本 agent 的会话（三向接线目标）；nil = 无会话持久化。
	Session session.Session
	// System 是系统提示词；空 = 无。
	System string
	// ToolGate 是工具执行闸门（nil = 不设防，所有调用直接执行）。
	ToolGate ToolGate
	// ScopeHook 是请求级 scope 的进阶挂点：每次 Run 派生请求 scope 后、
	// 回合开始前调用——应用经 kernel.On / kernel.OnWaterfall 在请求
	// scope 上订阅 loop / llm 事件（loop/llm 是 EmitLocal 派发，只本
	// scope 可见，挂宿主根收不到）。返回 error 中止本次回合。
	ScopeHook func(scope *kernel.Context) error
}

// DefaultAgentOptions 是便捷实例化参数：模型按声明名从宿主 Registry 解析，
// 工具与会话取宿主默认装配（Options.Tools / Options.Session 的产物）。
type DefaultAgentOptions struct {
	// Name 是 agent 标识（观测里进 agent 属性）。
	Name string
	// Model 是 Options.Models 里声明过的模型名。
	Model string
	// System 是系统提示词；空 = 无。
	System string
	// SessionID 非空 = 打开既有会话续跑（冷恢复语义随会话栈的 Store：
	// JSONL 默认档合成闭环，RecoverExposePending 档未决挂 Recoverable
	// 由宿主裁决）；空 = 新建会话。宿主未接 Options.Session 时非空报错。
	SessionID string
	// ToolGate 是工具执行闸门（nil = 不设防）。
	ToolGate ToolGate
}

// NewAgent 是 agent 的**最泛化构造**：全参数注入——model 可以是任意
// llm.ChatModel 来源（Registry 产出、stub、宿主自定义），ToolSet / Session
// 显式传入（nil = 无工具 / 无会话持久化），不依赖宿主的默认装配。宿主
// 在这里只提供生命周期容器与三向接线。
func (h *Host) NewAgent(opt AgentOptions) (*Agent, error) {
	if opt.Model == nil {
		return nil, fmt.Errorf("host: model is required (inject any llm.ChatModel)")
	}
	if opt.Name == "" {
		return nil, fmt.Errorf("host: agent name is required (loop instance identity)")
	}
	if opt.Session != nil && opt.ModelName == "" {
		return nil, fmt.Errorf("host: model name is required with a session (request.header audit records it)")
	}
	return &Agent{
		kernel:    h.ctx,
		sink:      h.sink,
		hostID:    h.hostID,
		name:      opt.Name,
		model:     opt.Model,
		toolSet:   opt.ToolSet,
		modelName: opt.ModelName,
		system:    opt.System,
		sess:      opt.Session,
		gate:      opt.ToolGate,
		scopeHook: opt.ScopeHook,
	}, nil
}

// DefaultAgent 是基于 NewAgent 的**便捷封装**：模型经宿主 Registry 按名
// 解析、工具集取宿主 Tools 的聚合视图、会话在宿主 SessionStack 上新建
// （SessionID 非空则打开既有会话续跑）。需要非默认来源（stub 模型、
// 专用工具集、外部会话）时直接用 NewAgent。
func (h *Host) DefaultAgent(ctx context.Context, opt DefaultAgentOptions) (*Agent, error) {
	model, err := h.models.Open(opt.Model)
	if err != nil {
		return nil, fmt.Errorf("host: open model %q: %w", opt.Model, err)
	}
	var toolSet loop.ToolSet
	if h.tools != nil {
		toolSet = h.tools.AsToolSet()
	}
	var sess session.Session
	if opt.SessionID != "" && h.session == nil {
		return nil, fmt.Errorf("host: session id %q requires Options.Session (none configured)", opt.SessionID)
	}
	if h.session != nil {
		if opt.SessionID == "" {
			sess, err = h.session.Create(ctx, session.SessionHeader{})
		} else {
			sess, err = h.session.Open(ctx, opt.SessionID)
		}
		if err != nil {
			return nil, fmt.Errorf("host: open session: %w", err)
		}
	}
	return h.NewAgent(AgentOptions{
		Name:      opt.Name,
		Model:     model,
		ToolSet:   toolSet,
		Session:   sess,
		System:    opt.System,
		ModelName: opt.Model,
		ToolGate:  opt.ToolGate,
	})
}

// Kernel 暴露 kernel 宿主——进阶装配（自定义服务、宿主级插件）经此用
// kernel 原生语义；host 不藏内核，但也不逼你先学内核。
func (h *Host) Kernel() *kernel.Context { return h.ctx }

// Models 暴露模型 Registry（进阶：流式、多模态、逐请求覆盖）。
func (h *Host) Models() *llm.Registry { return h.models }

// Tools 暴露工具 Registry（进阶：撤销来源、逐项预览）。
func (h *Host) Tools() *toolset.Registry { return h.tools }

// SessionStack 返回宿主接的会话栈；未接返回 nil。
func (h *Host) SessionStack() *memory.SessionStack { return h.session }

// Host 没有 Close：kernel 归调用方所有，生命周期由其 Dispose 负责
// （已装载插件的 Effect 届时逆序还原）。

// Agent 拥有会话 ↔ loop 的三向接线。Run 每次执行一个无状态 ReAct 回合：
//
//  1. 回合前：session.Surface() 折影为 history 传给 loop（未决会话在此
//     拒绝——ErrPendingEvents，不把 unpaired tool call 喂给模型）；
//  2. 回合中：按 loop 事件**同步**落盘——turn.started → request.header →
//     输入消息 → step.started → assistant（先于工具执行与 HITL）→
//     tool.result → step.ended → turn.ended；模型可见的每一步在发生时
//     即已入日志，进程死在任意执行点日志都停在真实现场；
//  3. 回合级 scope：每回合从宿主 kernel 派生独立请求 scope（观测桥 /
//     ToolGate / ScopeHook 都挂它），用毕即毁——同宿主多 Agent 互不串扰。
//
// 无会话注入的 Agent 退化为纯透传（history 由调用方经 RunHistory）。
// 同一 Agent 的并发 Run 未定义（会话括号会交错）；并发场景为每个并发
// 单元构造独立 Agent（Host 无状态可复用）。
type Agent struct {
	kernel    *kernel.Context
	sink      observability.Sink
	hostID    string
	name      string
	model     llm.ChatModel
	toolSet   loop.ToolSet
	modelName string
	system    string
	sess      session.Session
	gate      ToolGate
	scopeHook func(scope *kernel.Context) error
}

// Run 执行一个回合。input 是本回合的用户输入（user 消息；多条时按序）。
func (a *Agent) Run(ctx context.Context, input ...*llm.Message) (*loop.Result, error) {
	return a.run(ctx, nil, input)
}

// RunHistory 显式传 history 执行回合——供无会话 Agent 或旁路注入使用；
// 有会话时 history 仍以 session.Surface() 为准（参数被忽略）。
func (a *Agent) RunHistory(ctx context.Context, history []*llm.Message, input ...*llm.Message) (*loop.Result, error) {
	return a.run(ctx, history, input)
}

func (a *Agent) run(ctx context.Context, explicitHistory []*llm.Message, input []*llm.Message) (res *loop.Result, err error) {
	// 输入校验前置：回合输入只接受 user 消息（assistant/tool 由回合自身
	// 产出并落盘，不接受注入）——拒绝而不是静默归档错乱。
	for i, m := range input {
		if m == nil {
			return nil, fmt.Errorf("host: input[%d] is nil", i)
		}
		if m.Role != llm.RoleUser {
			return nil, fmt.Errorf("host: input[%d] role %q: turn input must be user messages", i, m.Role)
		}
	}

	var history []*llm.Message
	if a.sess != nil {
		h, err := a.sess.Surface(ctx)
		if err != nil {
			// 未决会话（ExposePending 档）在此拒绝投影——裁决经
			// Recoverable 完成，见 memory/session 冷恢复文档。
			return nil, fmt.Errorf("host: session surface: %w", err)
		}
		history = h
	} else {
		history = explicitHistory
	}

	// 请求级 scope：每回合独立派生、用毕即毁。loop/llm 都是 Local 派发
	// （只本 scope 可见），观测桥、闸门、落盘监听必须挂在这里。
	reqScope, err := a.kernel.Derive()
	if err != nil {
		return nil, fmt.Errorf("host: derive request scope: %w", err)
	}
	defer reqScope.Dispose()

	// 观测桥（宿主装配了 Sink 时）：每请求独立 TraceID，HostID 承接宿主。
	if a.sink != nil {
		cfg := observability.ObserveConfig{Sink: a.sink, HostID: a.hostID, TraceID: observability.NewTraceID()}
		if err := llm.Observe(reqScope, cfg); err != nil {
			return nil, fmt.Errorf("host: llm observe: %w", err)
		}
		if err := loop.Observe(reqScope, cfg); err != nil {
			return nil, fmt.Errorf("host: loop observe: %w", err)
		}
	}

	// 会话落盘监听：按 loop 事件同步 Append（见 turnRecorder）。
	if a.sess != nil {
		rec := &turnRecorder{a: a, sess: a.sess, ctx: context.Background()}
		if err := rec.mount(reqScope); err != nil {
			return nil, fmt.Errorf("host: session recorder: %w", err)
		}
	}

	// 工具闸门（HITL 最小挂点）：注册在请求 scope 的 before_tool_call
	// waterfall 首环——拒绝即短路，模型收到带 reason 的 IsError 结果。
	if a.gate != nil {
		if _, err := kernel.OnWaterfall(reqScope, loop.EventBeforeToolCall,
			func(p *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
				if ok, reason := a.gate(p.Call); !ok {
					if reason == "" {
						reason = "rejected by tool gate"
					}
					return &loop.BeforeToolCall{Call: p.Call, Rejected: true, RejectReason: reason}
				}
				return next(p)
			}); err != nil {
			return nil, fmt.Errorf("host: tool gate: %w", err)
		}
	}

	// 进阶挂点：应用在请求 scope 上自行订阅 loop/llm 事件。
	if a.scopeHook != nil {
		if err := a.scopeHook(reqScope); err != nil {
			return nil, fmt.Errorf("host: scope hook: %w", err)
		}
	}

	loopOpts := make([]loop.Option, 0, 4)
	if a.system != "" {
		loopOpts = append(loopOpts, loop.WithSystemPrompt(a.system))
	}
	if a.toolSet != nil {
		loopOpts = append(loopOpts, loop.WithToolSet(a.toolSet))
	}
	loopOpts = append(loopOpts, loop.WithEventScope(reqScope))
	la, err := loop.NewAgent(a.model, a.name, loopOpts...)
	if err != nil {
		return nil, fmt.Errorf("host: new agent: %w", err)
	}

	// 落盘监听以 panic 上报失败（fail closed：中断回合，日志停在与真实
	// 一致处），这里只把落盘失败转回 error——其余 panic（模型适配器、
	// onDelta、其他监听器）原样重抛，不吞不标。
	func() {
		defer func() {
			if r := recover(); r != nil {
				af, ok := r.(appendFail)
				if !ok {
					panic(r)
				}
				res, err = nil, fmt.Errorf("host: session append: %w", af.err)
			}
		}()
		res, err = la.RunStream(ctx, nil, history, input...)
	}()
	return res, err // error 路径 res 可能非 nil（canceled/error 的部分产出；日志已由 turn_end 监听闭合）
}

// appendFail 是落盘失败的私有 panic 载荷：recorder 的 append / Flush 失败
// 以 panic(appendFail) 上报（fail closed），run 的 recover 只认这个类型；
// 其他 panic 不是落盘问题，原样上抛。
type appendFail struct{ err error }

// turnRecorder 把 loop 回合事件同步落盘成 session 事件——官方路径上的
// model-visible means logged 接线。监听挂在请求 scope 上，随 scope 销毁
// 摘除；事件由 loop 在 RunStream 单 goroutine 串行发出，无需加锁。
//
// 落盘映射（与 session §6.3 fold 表对齐；turn/step/request.* 是 log-only，
// message.* 与 tool.result 进 surface）：
//
//	loop.turn_start      → turn.started + request.header + 输入消息(user)
//	loop.step_start      → step.started（上一步未闭合则先补 step.ended——
//	                       loop 无显式 step_end：一步的终点即下一步起点）
//	loop.after_model     → message.assistant（**先于**工具执行与 HITL 落盘）
//	loop.after_tool_call → tool.result（含被闸门拒绝的调用，IsError）
//	loop.turn_end        → step.ended + turn.ended（completed / max_steps 记
//	                       completed；canceled / error 记 interrupted）
//
// 失败语义（fail closed）：任何 append 失败 = panic 中断回合——日志停在
// 与真实一致的状态（重开由冷恢复合成闭合），Run 经 recover 转回 error。
// 落盘 ctx 用 Background：日志闭合不受请求取消影响（取消的回合同样要
// 在日志里留下闭合的痕迹）。
type turnRecorder struct {
	a    *Agent
	sess session.Session
	ctx  context.Context // 落盘用 Background

	turnID string // 当前回合的会话事件 ID（"turn-<unixnano>"）
	stepID string // 当前未闭合 step 的 ID；空 = 无未闭合 step
}

// mount 在请求 scope 上注册五个事件监听。
func (r *turnRecorder) mount(scope *kernel.Context) error {
	if _, err := kernel.On(scope, loop.EventTurnStart, func(p *loop.TurnStart) {
		r.turnID = fmt.Sprintf("turn-%d", time.Now().UnixNano())
		r.stepID = ""
		// turn.started 开括号：此后任何失败都留下可冷恢复的未决现场。
		r.append(session.EventTurnStarted, session.LifecyclePayload{ID: r.turnID}, nil)
		// request.header 审计：system / 工具声明快照 / model（重放与续跑锚点）。
		var sysPtr *string
		if r.a.system != "" {
			sys := r.a.system
			sysPtr = &sys
		}
		var defs []llm.ToolDef
		if r.a.toolSet != nil {
			defs = r.a.toolSet.Definitions()
		}
		r.append(session.EventRequestHeader, session.RequestHeaderPayload{
			System: sysPtr, ToolDefs: defs, Model: r.a.modelName,
		}, nil)
		for _, m := range p.Input {
			r.appendMessage(session.EventMessageUser, m)
		}
	}); err != nil {
		return err
	}

	if _, err := kernel.On(scope, loop.EventStepStart, func(p *loop.StepStart) {
		if r.stepID != "" {
			r.append(session.EventStepEnded, session.LifecyclePayload{ID: r.stepID, Reason: session.ReasonCompleted}, nil)
		}
		r.stepID = fmt.Sprintf("%s-step-%d", r.turnID, p.Step)
		r.append(session.EventStepStarted, session.LifecyclePayload{ID: r.stepID}, nil)
	}); err != nil {
		return err
	}

	if _, err := kernel.On(scope, loop.EventAfterModel, func(p *loop.AfterModel) {
		// 先于工具执行与 HITL 审批落盘：进程死在等待批准时，日志已含
		// tool_call——ExposePending 裁决的官方来源。
		r.appendMessage(session.EventMessageAssistant, p.Response.Message)
		// HITL 检查点：assistant（含 tool_call）落盘后立即 Flush——
		// JSONL 的 Append 只 write 不 fsync，崩溃只保证 Flush 点之前；
		// 掉电/强杀时裁决现场必须在磁盘上。只在这一点刷，不逐条刷。
		if err := r.sess.Flush(r.ctx); err != nil {
			panic(appendFail{err: err})
		}
	}); err != nil {
		return err
	}

	if _, err := kernel.On(scope, loop.EventAfterToolCall, func(p *loop.AfterToolCall) {
		// Result 即回传给模型的文本（拒绝/错误时 loop 已合成说明文字）；
		// IsError 与之一致（被拒绝的调用对模型同样是错误结果）。
		r.append(session.EventToolResult, session.ToolResultPayload{
			ToolCallID: p.Call.ID, Text: p.Result, IsError: p.Err != nil || p.Rejected,
		}, &session.SurfaceIntent{Op: session.SurfaceAppend})
	}); err != nil {
		return err
	}

	if _, err := kernel.On(scope, loop.EventTurnEnd, func(p *loop.TurnEnd) {
		// 无论何种方式结束（完成 / MaxSteps / 取消 / 错误）都闭合日志：
		// completed / max_steps 是策略性收尾记 completed；canceled /
		// error 记 interrupted（未完成的回合在日志里如实可见）。
		reason := session.ReasonCompleted
		if p.StoppedBy != loop.StopCompleted && p.StoppedBy != loop.StopMaxSteps {
			reason = session.ReasonInterrupted
		}
		if r.stepID != "" {
			r.append(session.EventStepEnded, session.LifecyclePayload{ID: r.stepID, Reason: reason}, nil)
			r.stepID = ""
		}
		r.append(session.EventTurnEnded, session.LifecyclePayload{ID: r.turnID, Reason: reason}, nil)
	}); err != nil {
		return err
	}
	return nil
}

// append 落盘一条事件。失败即 panic(appendFail)（fail closed，见类型注释）。
func (r *turnRecorder) append(t session.EventType, payload any, surface *session.SurfaceIntent) {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(appendFail{err: fmt.Errorf("marshal %s payload: %w", t, err)})
	}
	if _, err := r.sess.Append(r.ctx, session.EventDraft{Type: t, Data: data, Surface: surface}); err != nil {
		panic(appendFail{err: err})
	}
}

// appendMessage 落盘一条消息（user / assistant，surface append）。
func (r *turnRecorder) appendMessage(t session.EventType, m *llm.Message) {
	parts := m.Parts
	if parts == nil {
		parts = []llm.Part{}
	}
	r.append(t, session.MessagePayload{Parts: parts}, &session.SurfaceIntent{Op: session.SurfaceAppend})
}

// Session 返回本 agent 的会话句柄（导出/导入、Recoverable 裁决、直读
// 事件经此）；无会话返回 nil。
func (a *Agent) Session() session.Session { return a.sess }
