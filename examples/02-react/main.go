// 02-react：ReAct 循环与工具调用。
//
// 运行：go run ./examples/02-react
// toolset.Registry 注册工具 → AsToolSet 交给 loop.Agent → RunStream 流式
// 输出。本课只有只读工具、不装审批——审批（HITL）是 03 课的独立主题。
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

	stdin := demoapp.NewLineSource(os.Stdin)
	var history []*llm.Message
	fmt.Printf("02-react provider=%s model=%s scripted=%v host=%s\n",
		flags.Provider, flags.Model, flags.Scripted, host.HostID())
	return demoapp.Loop(stdin, os.Stdout, func(msg *llm.Message) ([]*llm.Message, error) {
		// 每轮独立 reqScope + Bridge + Agent：Local 派发要求监听与
		// Agent 同 scope，请求结束随手销毁。
		reqScope, err := host.Ctx.Derive()
		if err != nil {
			return nil, err
		}
		defer reqScope.Dispose()
		bridge, err := host.NewBridge(reqScope)
		if err != nil {
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
		fmt.Printf("stopped_by=%s steps=%d history=%d trace=%s\n",
			res.StoppedBy, res.Steps, len(history), bridge.TraceID)
		return res.Messages, nil
	}, func() int { return len(history) })
}
