// 02-react：ReAct 循环、工具调用，以及**手写一个运行期观测桥**。
//
// 运行：go run ./examples/02-react
// 三件事：① toolset.Registry 注册工具 → AsToolSet 交给 loop；② RunStream
// 流式输出；③ 本课主角——reqBridge：订阅 llm/loop 的公开事件，把一次
// 请求的运行期事实聚合进同一个 Sink（demoapp.Bridge 的教学展开，03 课
// 起复用封装版）。审批（HITL）是 03 课主题。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/observability"
	"github.com/Luo-root/pulse/toolset"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "02-react: %v\n", err)
		os.Exit(1)
	}
}

// reqBridge 是本课手写的运行期观测桥：聚合一次请求的观测状态。
//
// 设计要点（对应 demoapp.Bridge 的实现）：
//   - **生命周期 = 该请求**：监听挂在 reqScope 上，Dispose 自动摘除——
//     桥对象不需要 Close；
//   - **两层标识**：HostID 宿主稳定（装配期），TraceID 每请求独立
//     （单一生成源，桥不自己另造序号）；
//   - **Waterfall vs On**：BeforeGenerate 是 Waterfall（链上可改写请求，
//     礼仪是拿到参数后调用 next(req) 放行）；AfterResponse/AfterToolCall
//     /TurnEnd 是普通事件（只观察，不修改）；
//   - **官方 Record 不扩字段**：token 数等装不进信封的指标走 slog 附加键；
//     桥事件名保持 `<组件>.<事实>` 点分约定便于聚合分组。
type reqBridge struct {
	sink    observability.Sink
	hostID  string
	traceID string

	mu       sync.Mutex
	genStart time.Time
}

func newReqBridge(sink observability.Sink, hostID, traceID string) *reqBridge {
	return &reqBridge{sink: sink, hostID: hostID, traceID: traceID}
}

// install 把本请求的事件监听挂到 scope。scope 必须与 Agent 的
// WithEventScope 相同——Local 派发下，挂错 scope 什么也听不到。
func (b *reqBridge) install(scope *kernel.Context) error {
	if scope == nil {
		return fmt.Errorf("02-react: bridge requires a request scope")
	}

	// ⓪ 装配层示范默认值：Anthropic 线格式 MaxTokens 必填（nil →
	//    ErrBadRequest），loop 组请求不填——桥在请求 scope 上兜底注入。
	//    与 ⓶ 同事件两个监听：Waterfall 链按注册顺序串联。
	if _, err := kernel.OnWaterfall(scope, llm.EventBeforeGenerate,
		func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
			if req != nil && req.MaxTokens == nil {
				v := 4096
				req.MaxTokens = &v
			}
			return next(req)
		}); err != nil {
		return err
	}

	// ① BeforeGenerate：记请求起点（Duration 的锚点）。
	if _, err := kernel.OnWaterfall(scope, llm.EventBeforeGenerate,
		func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
			b.mu.Lock()
			b.genStart = time.Now()
			b.mu.Unlock()
			return next(req)
		}); err != nil {
		return err
	}

	// ② AfterResponse：模型调用完成——延迟、finish reason 进 Sink；
	//    token usage 走 slog 附加键（不扩官方 Record）。
	if _, err := kernel.On(scope, llm.EventAfterResponse, func(resp *llm.Response) {
		b.mu.Lock()
		started := b.genStart
		b.mu.Unlock()
		b.sink.Write(observability.Record{
			HostID:   b.hostID,
			TraceID:  b.traceID,
			Source:   observability.SourceBridge,
			Event:    "llm.generate_finished",
			Duration: time.Since(started),
			Status:   string(resp.FinishReason),
		})
		slog.Debug("token usage",
			"trace_id", b.traceID,
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
		)
	}); err != nil {
		return err
	}

	// ③ AfterToolCall：工具结果三态（completed / rejected / failed）。
	//    rejected 是 HITL 的拒绝——被拒不算 crash，是独立状态。
	if _, err := kernel.On(scope, loop.EventAfterToolCall, func(after *loop.AfterToolCall) {
		status := "completed"
		switch {
		case after.Rejected:
			status = "rejected"
		case after.Err != nil:
			status = "failed"
		}
		b.sink.Write(observability.Record{
			HostID:   b.hostID,
			TraceID:  b.traceID,
			Source:   observability.SourceBridge,
			Event:    "loop.tool_finished",
			Status:   status,
			Duration: after.Duration,
			Err:      after.Err,
		})
	}); err != nil {
		return err
	}

	// ④ TurnEnd：回合摘要（steps / stopped_by / token）走 slog。
	if _, err := kernel.On(scope, loop.EventTurnEnd, func(end *loop.TurnEnd) {
		slog.Info("turn summary",
			"host_id", b.hostID,
			"trace_id", b.traceID,
			"steps", end.Steps,
			"stopped_by", end.StoppedBy,
			"input_tokens", end.Usage.InputTokens,
			"output_tokens", end.Usage.OutputTokens,
		)
	}); err != nil {
		return err
	}
	return nil
}

// write 让桥直接把自定义事实写进同一出口（事件名自定义，保持点分约定）。
func (b *reqBridge) write(event, status string) {
	b.sink.Write(observability.Record{
		HostID:  b.hostID,
		TraceID: b.traceID,
		Source:  observability.SourceBridge,
		Event:   event,
		Status:  status,
	})
}

func run() error {
	flags := demoapp.LoadFlagsFromEnv()
	scripted := []*llm.Response{
		// 第一轮：模型决定调用工具（ReAct 的 Act 步）。
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"topic":"pulse"}`)}),
		// 第二轮：拿到工具结果后给出最终回答（ReAct 的 Respond 步）。
		llm.Resp("演示结束：lookup 经 ReAct 循环完成调用与结果回填。"),
	}
	host, err := demoapp.Open(flags, scripted...)
	if err != nil {
		return err
	}
	defer host.Close()

	// 工具不直接塞给 loop：先注册进 toolset.Registry（pulse.tools）——
	// 注册表带来 Risk/Source 元数据与可逆注销（DisposeSource），这些
	// 元数据在 03 课的审批里就是决策依据。
	if _, err := kernel.Use(host.Ctx, toolset.Plugin()); err != nil {
		return err
	}
	reg, ok := kernel.Get(host.Ctx, toolset.ServiceKey)
	if !ok {
		return fmt.Errorf("02-react: pulse.tools not provided")
	}
	if _, err := reg.Register(host.Ctx, toolset.Registration{
		Def: llm.ToolDef{
			Name:        "lookup",
			Description: "查找本地知识",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
		},
		Fn: func(_ context.Context, args json.RawMessage) (string, error) {
			return `{"topic":"pulse","note":"plugin kernel + llm vocabulary + loop"}`, nil
		},
		Source: "local.lookup",
		Risk:   toolset.RiskReadonly,
	}); err != nil {
		return err
	}
	// AsToolSet 把 Registry 适配成 loop.ToolSet——模型看到的工具面。
	tools := reg.AsToolSet()

	var history []*llm.Message
	fmt.Printf("02-react provider=%s model=%s scripted=%v host=%s\n",
		flags.Provider, flags.Model, flags.Scripted, host.HostID())
	return demoapp.Loop(os.Stdin, os.Stdout, func(msg *llm.Message) ([]*llm.Message, error) {
		// 每轮独立 reqScope + 手写 Bridge + Agent：
		// Local 派发要求监听与 Agent 同 scope，请求结束随手销毁。
		reqScope, err := host.Ctx.Derive()
		if err != nil {
			return nil, err
		}
		defer reqScope.Dispose()
		bridge := newReqBridge(host.Sink, host.HostID(), host.NewTraceID())
		if err := bridge.install(reqScope); err != nil {
			return nil, err
		}
		agent, err := loop.NewAgent(host.Model,
			loop.WithToolSet(tools),
			loop.WithSystemPrompt("你是 Pulse 示例助手。需要事实时调用 lookup 工具。"),
			loop.WithEventScope(reqScope),
		)
		if err != nil {
			return nil, err
		}
		// RunStream：token 级流式回调 + 与 Run 相同的聚合结果。
		res, err := agent.RunStream(context.Background(), func(delta string) {
			fmt.Print(delta)
		}, history, msg)
		if err != nil {
			return nil, err
		}
		if res.Final != nil && !strings.HasSuffix(res.Final.Text(), "\n") {
			fmt.Println()
		}
		history = append(history, msg)
		history = append(history, res.Messages...)
		bridge.write("react.summary", fmt.Sprintf("steps=%d history=%d", res.Steps, len(history)))
		fmt.Fprintf(os.Stderr, "stopped_by=%s steps=%d history=%d trace=%s\n",
			res.StoppedBy, res.Steps, len(history), bridge.traceID)
		return res.Messages, nil
	}, func() int { return len(history) })
}
