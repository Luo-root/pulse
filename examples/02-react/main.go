// 02-react：ReAct 循环、工具调用，以及**官方观测适配的接入**。
//
// 运行：go run ./examples/02-react
// 三件事：① toolset.Registry 注册工具 → AsToolSet 交给 loop；② RunStream
// 流式输出；③ 本课主角——观测接入：ObserveConfig + AttachCollector +
// llm.Observe / loop.Observe 把一次请求的运行期事实折进同一个 Sink，
// 业务自定义事实（react.summary）经 Collector 直写（03 课起复用封装版
// demoapp.Host.NewObserve）。审批（HITL）是 03 课主题。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
		// 每轮独立 reqScope + 观测接入 + Agent：
		// Local 派发要求监听与 Agent 同 scope，请求结束随手销毁。
		reqScope, err := host.Ctx.Derive()
		if err != nil {
			return nil, err
		}
		defer reqScope.Dispose()

		// 本课手写观测接入（03 课起复用 demoapp.Host.NewObserve 封装版）。
		// cfg 生命周期 = 请求：同一请求多适配复用同一值即 D3 请求级关联；
		// TraceID 由官方默认生成器生成（每请求一次；宿主也可自带方案）。
		cfg := observability.ObserveConfig{Sink: host.Sink, HostID: host.HostID(), TraceID: observability.NewTraceID()}
		// 装配层示范默认值：Anthropic 线格式 MaxTokens 必填（nil →
		// ErrBadRequest），loop 组请求不填——请求 scope 上兜底注入
		// （demoapp 封装，非库 API；与 llm.Observe 同挂 reqScope）。
		if err := demoapp.InstallAnthropicMaxTokensDefault(reqScope); err != nil {
			return nil, err
		}
		// AttachCollector：业务直写面（Collector 服务随 reqScope 销毁撤除）。
		collector, err := observability.AttachCollector(reqScope, cfg)
		if err != nil {
			return nil, err
		}
		// 官方适配：llm 折 generate_finished（模型名与 token 进 Attrs），
		// loop 折 tool_finished（三态 completed/rejected/failed）与
		// turn_finished。监听与 Agent 同 scope——挂错 scope 什么也听不到。
		if err := llm.Observe(reqScope, cfg); err != nil {
			return nil, err
		}
		if err := loop.Observe(reqScope, cfg); err != nil {
			return nil, err
		}

		agent, err := loop.NewAgent(host.Model, "react",
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
		// 自定义事实经 Collector 直写同一出口：自动携带 HostID/TraceID，
		// 事件名遵守 <组件>.<事实> 点分约定（官方 Record 不扩字段）。
		collector.Write("react.summary", fmt.Sprintf("steps=%d history=%d", res.Steps, len(history)))
		fmt.Fprintf(os.Stderr, "stopped_by=%s steps=%d history=%d trace=%s\n",
			res.StoppedBy, res.Steps, len(history), cfg.TraceID)
		return res.Messages, nil
	}, func() int { return len(history) })
}
