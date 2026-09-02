package war

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

// ---- Pulse 参赛方 ----

// pulseToolHits 是 Pulse 侧工具执行计数（副作用痕迹，断言用）。
var pulseToolHits int

// runPulseTextRound 跑一轮 Pulse 完整生产装配的单步文本回合：每轮重建
// ScriptedModel + Agent（脚本耗尽语义，构造计入 = 上界口径，与 Eino 侧
// 对齐）。装配链 = #102 分层基线的 L2a 口径。
func runPulseTextRound(ctx context.Context) error {
	model := llm.NewScripted(llm.Resp("done"))
	agent, err := loop.NewAgent(model)
	if err != nil {
		return err
	}
	res, err := agent.Run(ctx, nil, llm.UserText("war payload"))
	if err != nil {
		return err
	}
	if res.StoppedBy != loop.StopCompleted {
		return fmt.Errorf("pulse text round: stopped_by=%s", res.StoppedBy)
	}
	return nil
}

// runPulseToolRound 跑一轮 Pulse 完整生产装配的工具往返（含工具声明进
// 请求、执行、结果回填）：每轮重建 ScriptedModel + Agent + 工具计数复位。
// 装配链 = #102 分层基线的 L2b 口径。
func runPulseToolRound(ctx context.Context) error {
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("done"),
	)
	tools := loop.NewMemToolSet()
	if err := tools.Register(llm.ToolDef{
		Name:        "lookup",
		Description: "war benchmark tool",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}, func(_ context.Context, _ json.RawMessage) (string, error) {
		pulseToolHits++
		return `{"ok":true}`, nil
	}); err != nil {
		return err
	}
	agent, err := loop.NewAgent(model, loop.WithToolSet(tools))
	if err != nil {
		return err
	}
	res, err := agent.Run(ctx, nil, llm.UserText("war payload"))
	if err != nil {
		return err
	}
	if res.StoppedBy != loop.StopCompleted || res.Steps != 2 {
		return fmt.Errorf("pulse tool round: stopped_by=%s steps=%d", res.StoppedBy, res.Steps)
	}
	return nil
}

// ---- Eino 参赛方 ----

// einoToolHits 是 Eino 侧工具执行计数。
var einoToolHits int

// runEinoTextRound 跑一轮 Eino 完整生产装配的单步文本回合：ChatModelAgent
// + Runner（ADK 官方生产入口）——每轮重建 stub 模型与 agent（与 Pulse 侧
// 同口径的上界）。instruction 对齐 Pulse 的 system prompt 语义（本轮均空
// 指令——Pulse 侧未设 system，Eino 侧 Instruction 留默认空）。
func runEinoTextRound(ctx context.Context) error {
	cm := newEinoStub(assistantText("done"))
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:  "war",
		Model: cm,
	})
	if err != nil {
		return err
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent})
	iter := runner.Run(ctx, []*schema.Message{schema.UserMessage("war payload")})
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			return err
		}
	}
	return nil
}

// runEinoToolRound 跑一轮 Eino 完整生产装配的工具往返：stub 第一响应带
// tool_calls，第二响应给最终回答；工具经 ToolsNode 执行后回填。
func runEinoToolRound(ctx context.Context) error {
	cm := newEinoStub(
		assistantToolCall("c1", "lookup", "{}"),
		assistantText("done"),
	)
	wt := &warTool{}
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:  "war",
		Model: cm,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: []tool.BaseTool{wt},
			},
		},
	})
	if err != nil {
		return err
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent})
	iter := runner.Run(ctx, []*schema.Message{schema.UserMessage("war payload")})
	sawTool := false
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			return event.Err
		}
		if event.Output != nil && event.Output.MessageOutput != nil {
			if msg, merr := event.Output.MessageOutput.GetMessage(); merr == nil && msg != nil && len(msg.ToolCalls) > 0 {
				sawTool = true
			}
		}
	}
	if !sawTool {
		return fmt.Errorf("eino tool round: no tool call observed")
	}
	if einoToolHits == 0 && wt.hits == 0 {
		return fmt.Errorf("eino tool round: tool never executed")
	}
	return nil
}

// assistantText 构造纯文本 assistant 消息。
func assistantText(text string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, Content: text}
}

// assistantToolCall 构造带单个工具调用的 assistant 消息。
func assistantToolCall(id, name, args string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{ID: id, Function: schema.FunctionCall{Name: name, Arguments: args}},
		},
	}
}
