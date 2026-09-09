// Package host 是两层装配的第二层：只接跨包的缝——模型供应商注册、
// 工具来源注册、会话栈 ↔ loop 的三向接线（surface 注入 history / 本回合
// 落盘 / request.header 审计）、agent 构造与生命周期。
//
// 各包自己的基础装配不在本包：memory 会话栈/条目栈见 memory 包根级
// 门面（memory.NewSessionStack / memory.NewItemStack），llm.Registry /
// observability.Bootstrap / toolset/builtins.Register 各自是一站式入口。
// host 只收敛「把各包串起来」的知识。
//
// 零新抽象：供应商与工具来源都是函数类型，签名对齐各包既有 Register，
// openai.Register / 闭包版 builtins.Register 直接转换。
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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

// ObserveConfig 观测装配。Sink 为 nil = 不装观测（零开销）。
type ObserveConfig struct {
	// HostID 是观测记录的宿主标识（trace 归属）；空则用 "pulse-host"。
	HostID string
	Sink   observability.Sink
}

// Options 是宿主装配参数：静态装配收敛常规场景，进阶场景经 Kernel()
// 用 kernel 原生语义。所有外部副作用（模型网络、工具执行、落盘）都是
// 显式 opt-in——不传就没有。
type Options struct {
	// Providers 注册模型供应商适配器（先于 Models 声明执行）。
	Providers []Provider
	// Models 声明模型路由：名字 → 配置（Provider 字段须已在 Providers 里注册）。
	Models map[string]llm.Config
	// Tools 注册工具来源（本地 builtins / MCP Source / 自定义）。
	Tools []ToolSource
	// Session 接会话栈（memory.NewSessionStack 的产物）；nil = 纯无状态
	// 回合（agent.Run 的 history 由调用方自管）。
	Session *memory.SessionStack
	// Observe 装观测双基座；Sink 为 nil = 不装。
	Observe ObserveConfig
}

// Host 是装配好的宿主：kernel 宿主 + 模型 Registry + 工具 Registry +
// 可选会话栈。并发安全由各组件自有锁保证；Host 本身无状态可复用。
type Host struct {
	ctx     *kernel.Context
	models  *llm.Registry
	tools   *toolset.Registry
	session *memory.SessionStack
}

// New 装配宿主。执行顺序：观测 → 供应商 → 模型声明 → 工具来源；
// 任一步失败即整体失败（已注册部分随 kernel Dispose 逆序撤除——
// 可逆效应语义）。
func New(opt Options) (*Host, error) {
	c := kernel.New()
	h := &Host{ctx: c}
	if opt.Observe.Sink != nil {
		id := opt.Observe.HostID
		if id == "" {
			id = "pulse-host"
		}
		if _, err := kernel.Use(c, observability.Bootstrap(id, opt.Observe.Sink)); err != nil {
			c.Dispose()
			return nil, fmt.Errorf("host: observability: %w", err)
		}
	}
	reg := llm.NewRegistry(c)
	for i, p := range opt.Providers {
		if p == nil {
			continue
		}
		if err := p(c, reg); err != nil {
			c.Dispose()
			return nil, fmt.Errorf("host: provider[%d]: %w", i, err)
		}
	}
	for name, cfg := range opt.Models {
		if err := reg.Declare(name, cfg); err != nil {
			c.Dispose()
			return nil, fmt.Errorf("host: declare %q: %w", name, err)
		}
	}
	h.models = reg

	tr := toolset.NewRegistry()
	for i, src := range opt.Tools {
		if src == nil {
			continue
		}
		if err := src(c, tr); err != nil {
			c.Dispose()
			return nil, fmt.Errorf("host: tool source[%d]: %w", i, err)
		}
	}
	h.tools = tr
	h.session = opt.Session
	return h, nil
}

// Agent 构造一个接入会话栈的 agent。每个 Agent 拥有独立会话；Session
// 为 nil 的宿主构造的 agent 也是无会话回合。
func (h *Host) Agent(ctx context.Context, opt AgentOptions) (*Agent, error) {
	model, err := h.models.Open(opt.Model)
	if err != nil {
		return nil, fmt.Errorf("host: open model %q: %w", opt.Model, err)
	}
	var loopOpts []loop.Option
	if opt.System != "" {
		loopOpts = append(loopOpts, loop.WithSystemPrompt(opt.System))
	}
	if h.tools != nil {
		loopOpts = append(loopOpts, loop.WithToolSet(h.tools.AsToolSet()))
	}
	agent, err := loop.NewAgent(model, opt.Name, loopOpts...)
	if err != nil {
		return nil, fmt.Errorf("host: new agent: %w", err)
	}
	a := &Agent{agent: agent, model: opt.Model, system: opt.System, tools: h.tools}
	if h.session != nil {
		sess, err := h.session.Create(ctx, session.SessionHeader{})
		if err != nil {
			return nil, fmt.Errorf("host: create session: %w", err)
		}
		a.sess = sess
	}
	return a, nil
}

// Kernel 暴露 kernel 宿主——进阶装配（请求级 scope、事件订阅、自定义
// 服务）经此用 kernel 原生语义；host 不藏内核，但也不逼你先学内核。
func (h *Host) Kernel() *kernel.Context { return h.ctx }

// Models 暴露模型 Registry（进阶：流式、多模态、逐请求覆盖）。
func (h *Host) Models() *llm.Registry { return h.models }

// Tools 暴露工具 Registry（进阶：撤销来源、逐项预览）。
func (h *Host) Tools() *toolset.Registry { return h.tools }

// SessionStack 返回宿主接的会话栈；未接返回 nil。
func (h *Host) SessionStack() *memory.SessionStack { return h.session }

// Close 释放宿主（kernel Dispose：已装载插件的 Effect 逆序还原）。
func (h *Host) Close() { h.ctx.Dispose() }

// AgentOptions 是 agent 构造参数。
type AgentOptions struct {
	// Name 是 agent 标识（观测里进 agent 属性）。
	Name string
	// Model 是 Options.Models 里声明过的模型名。
	Model string
	// System 是系统提示词；空 = 无。
	System string
}

// Agent 是 loop.Agent 的薄包装：拥有会话 ↔ loop 的三向接线。Run 每次
// 执行一个无状态 ReAct 回合，接线在 Host 构造的会话上完成：
//
//  1. 回合前：session.Surface() 折影为 history 传给 loop；
//  2. 回合前：request.header（system / 工具声明 / model）审计落盘；
//  3. 回合后：本回合输入与产出消息（user / assistant / tool.result）
//     逐条落盘——下一轮 Surface 即含完整历史。
//
// 无会话宿主构造的 Agent 退化为纯透传（history 由调用方经 RunHistory）。
type Agent struct {
	agent  *loop.Agent
	sess   session.Session
	model  string
	system string
	tools  *toolset.Registry
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

func (a *Agent) run(ctx context.Context, explicitHistory []*llm.Message, input []*llm.Message) (*loop.Result, error) {
	var history []*llm.Message
	if a.sess != nil {
		h, err := a.sess.Surface(ctx)
		if err != nil {
			return nil, fmt.Errorf("host: session surface: %w", err)
		}
		history = h
	} else {
		history = explicitHistory
	}
	res, err := a.agent.Run(ctx, history, input...)
	if err != nil {
		return nil, err
	}
	if a.sess != nil {
		if err := a.appendTurn(ctx, input, res); err != nil {
			return nil, fmt.Errorf("host: session append: %w", err)
		}
	}
	return res, nil
}

// appendTurn 把本回合落盘：request.header 审计 + 输入消息 + 产出消息。
func (a *Agent) appendTurn(ctx context.Context, input []*llm.Message, res *loop.Result) error {
	// request.header：system / 工具声明 / model 三样（重放与续跑的锚点）。
	var sysPtr *string
	if a.system != "" {
		sys := a.system
		sysPtr = &sys
	}
	var defs []llm.ToolDef
	if a.tools != nil {
		defs = a.tools.AsToolSet().Definitions()
	}
	if _, err := a.sess.Append(ctx, session.EventDraft{
		Type: session.EventRequestHeader,
		Data: mustJSON(session.RequestHeaderPayload{System: sysPtr, ToolDefs: defs, Model: a.model}),
	}); err != nil {
		return err
	}
	appendMsg := func(m *llm.Message) error {
		switch m.Role {
		case llm.RoleTool:
			for _, p := range m.Parts {
				if p.Kind != llm.PartToolResult || p.ToolResultValue == nil {
					continue
				}
				tr := p.ToolResultValue
				text := toolResultText(tr)
				if _, err := a.sess.Append(ctx, session.EventDraft{
					Type:    session.EventToolResult,
					Data:    mustJSON(session.ToolResultPayload{ToolCallID: tr.ToolCallID, Text: text, IsError: tr.IsError}),
					Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
				}); err != nil {
					return err
				}
			}
			return nil
		case llm.RoleAssistant:
			_, err := a.sess.Append(ctx, session.EventDraft{
				Type:    session.EventMessageAssistant,
				Data:    mustJSON(session.MessagePayload{Parts: m.Parts}),
				Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
			})
			return err
		default: // user 及其他角色按 user 事件归档
			_, err := a.sess.Append(ctx, session.EventDraft{
				Type:    session.EventMessageUser,
				Data:    mustJSON(session.MessagePayload{Parts: m.Parts}),
				Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
			})
			return err
		}
	}
	for _, m := range input {
		if m == nil {
			continue
		}
		if err := appendMsg(m); err != nil {
			return err
		}
	}
	for _, m := range res.Messages {
		if m == nil {
			continue
		}
		if err := appendMsg(m); err != nil {
			return err
		}
	}
	return nil
}

// Session 返回本 agent 的会话句柄（导出/导入、直读事件经此）；无会话
// 返回 nil。
func (a *Agent) Session() session.Session { return a.sess }

// toolResultText 把 ToolResult 的内容块序列成模型可见文本（文本块拼接；
// 非文本块不计入——tool.result 载荷契约只存 IsError + 文本）。
func toolResultText(tr *llm.ToolResult) string {
	out := make([]string, 0, len(tr.Content))
	for _, p := range tr.Content {
		if p.Kind == llm.PartText {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
