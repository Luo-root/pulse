package war

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/observability"
)

// ---- Pulse 参赛方 ----

// nopSink 是黑洞观测出口：装配 observability.Bootstrap 需要一个 Sink，
// benchmark 只测框架路径开销，不让 MemorySink 的无限增长污染 allocs 口径。
type nopSink struct{}

func (nopSink) Write(observability.Record) {}

// pulseToolHits 是 Pulse 侧工具执行计数（副作用痕迹，断言用）。
var pulseToolHits int

// runPulseTextRound 跑一轮 Pulse **全家桶生产装配**的单步文本回合：kernel
// 宿主 + observability.Bootstrap（nopSink 黑洞）+ llm.Plugin（Registry 装载
// + observed 包装）+ 命名实例 Declare/Open + 请求 scope——即 #102 分层基线
// 的 **L2a 口径**。模型与 Agent 的重建每轮进行（脚本耗尽语义，构造计入 =
// 上界口径，与 Eino 侧对齐）；kernel/Registry/sink 是装配期一次性的。
func runPulseTextRound(ctx context.Context) error {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, observability.Bootstrap("war", nopSink{})); err != nil {
		return err
	}
	model, scope, err := assemblePulseRegistry(host, llm.Resp("done"))
	if err != nil {
		return err
	}
	agent, err := loop.NewAgent(model, loop.WithEventScope(scope))
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

// assemblePulseRegistry 装配 llm.Registry（Plugin + scripted provider +
// Declare/Open），返回打开的模型与请求 scope。对应 #102 的 L1 装配层。
// steps 是该 provider 的响应脚本（按任务区分：文本回合给纯文本，工具
// 回合给 [tool_calls, done]——每轮重建 host，脚本首次 Run 完整消费）。
func assemblePulseRegistry(host *kernel.Context, steps ...*llm.Response) (llm.ChatModel, *kernel.Context, error) {
	if _, err := kernel.Use(host, llm.Plugin()); err != nil {
		return nil, nil, err
	}
	reg, ok := kernel.Get(host, llm.ServiceKey)
	if !ok {
		return nil, nil, fmt.Errorf("war: pulse registry missing")
	}
	if _, err := reg.RegisterProvider(host, "scripted", func(llm.Config) (llm.ChatModel, error) {
		return llm.NewScripted(steps...), nil
	}); err != nil {
		return nil, nil, err
	}
	if err := reg.Declare("main", llm.Config{Provider: "scripted", Model: "scripted"}); err != nil {
		return nil, nil, err
	}
	model, err := reg.Open("main")
	if err != nil {
		return nil, nil, err
	}
	scope, err := host.Derive()
	if err != nil {
		return nil, nil, err
	}
	return model, scope, nil
}

// runPulseToolRound 跑一轮 Pulse **全家桶生产装配**的工具往返：L1 装配 +
// MemToolSet 注册 + Agent 挂请求 scope——即 #102 分层基线的 **L2b 口径**
// （tool_calls → 执行 → 结果回填 → 最终回答）。每轮重建模型与 Agent（上界
// 口径，与 Eino 侧对齐）。
func runPulseToolRound(ctx context.Context) error {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, observability.Bootstrap("war", nopSink{})); err != nil {
		return err
	}
	model, scope, err := assemblePulseRegistry(host,
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("done"),
	)
	if err != nil {
		return err
	}
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
	agent, err := loop.NewAgent(model, loop.WithToolSet(tools), loop.WithEventScope(scope))
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
			return event.Err
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
	if wt.hits == 0 {
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

// ---- 编排执行器对比（T3 线性链 / T4 DAG 分支汇聚）----
//
// 任务集：全部节点为零计算透传（测量对象 = 图执行机本身：调度、数据
// 传递、汇聚同步；不含模型与工具）。两边都是冷启动口径（每轮重建 +
// Compile/Run，构造计入），与 T1/T2 冷启动一致。
//
// 等价性声明：
//   - T3：3 个透传节点串联，输入字符串原样流到输出；
//   - T4：1 源 → 2 并行分支 → AND 汇聚（join 等两路输入齐才跑）→ 拼接
//     输出。Pulse 用 AND 槽位语义（Requires 两个 key）；Eino 用
//     AllPredecessor 触发模式（多入边 fan-in 成 []string）——语义对齐点。
//   - 生产入口：Pulse 用 kernel/flow 的 Graph（Add/Seed/Run）；Eino 用
//     compose.Chain（T3）与 compose.Workflow（T4，字段映射 AND 汇聚）。

// runPulseFlowChain T3：Pulse kernel/flow 三节点线性链。
func runPulseFlowChain(ctx context.Context) error {
	g := flow.New(ctx)
	kIn := flow.NewKey[string]("in")
	k1 := flow.NewKey[string]("m1")
	k2 := flow.NewKey[string]("m2")
	kOut := flow.NewKey[string]("out")
	pass := func(in, out flow.Key[string]) func(*flow.RunCtx) error {
		return func(rc *flow.RunCtx) error {
			v, err := flow.Get(rc, in)
			if err != nil {
				return err
			}
			return flow.Set(rc, out, v)
		}
	}
	for _, n := range []*flow.Node{
		flow.NewNode("n1", flow.Requires[string](kIn), flow.Provides[string](k1), pass(kIn, k1)),
		flow.NewNode("n2", flow.Requires[string](k1), flow.Provides[string](k2), pass(k1, k2)),
		flow.NewNode("n3", flow.Requires[string](k2), flow.Provides[string](kOut), pass(k2, kOut)),
	} {
		if err := g.Add(n); err != nil {
			return err
		}
	}
	if err := flow.Seed(g, kIn, "war payload"); err != nil {
		return err
	}
	return g.Run()
}

// runEinoChain T3：Eino compose.Chain 三节点线性链。
func runEinoChain(ctx context.Context) error {
	pass := compose.InvokableLambda(func(_ context.Context, s string) (string, error) {
		return s, nil
	})
	c := compose.NewChain[string, string]()
	c.AppendLambda(pass)
	c.AppendLambda(pass)
	c.AppendLambda(pass)
	r, err := c.Compile(ctx)
	if err != nil {
		return err
	}
	_, err = r.Invoke(ctx, "war payload")
	return err
}

// runPulseFlowDAG T4：Pulse kernel/flow 分支汇聚（1 源 Seed → 2 并行分支
// → AND 汇聚节点 → 拼接输出）。终端结果经 join 闭包写出——Graph 没有
// Run 后的公开读槽，这是 flow README 声明的契约权宜（非输出惯例）。
func runPulseFlowDAG(ctx context.Context) (string, error) {
	g := flow.New(ctx)
	kIn := flow.NewKey[string]("in")
	kA := flow.NewKey[string]("a")
	kB := flow.NewKey[string]("b")
	pass := func(in, out flow.Key[string]) func(*flow.RunCtx) error {
		return func(rc *flow.RunCtx) error {
			v, err := flow.Get(rc, in)
			if err != nil {
				return err
			}
			return flow.Set(rc, out, v)
		}
	}
	var out string
	for _, n := range []*flow.Node{
		flow.NewNode("a", flow.Requires[string](kIn), flow.Provides[string](kA), pass(kIn, kA)),
		flow.NewNode("b", flow.Requires[string](kIn), flow.Provides[string](kB), pass(kIn, kB)),
		flow.NewNode("join", flow.Requires[string](kA, kB), nil, func(rc *flow.RunCtx) error {
			a, err := flow.Get(rc, kA)
			if err != nil {
				return err
			}
			bVal, err := flow.Get(rc, kB)
			if err != nil {
				return err
			}
			out = a + bVal
			return nil
		}),
	} {
		if err := g.Add(n); err != nil {
			return "", err
		}
	}
	if err := flow.Seed(g, kIn, "war payload"); err != nil {
		return "", err
	}
	if err := g.Run(); err != nil {
		return "", err
	}
	return out, nil
}

// runEinoDAG T4：Eino compose.Workflow 分支汇聚——Workflow 是 Eino 的显式
// 依赖 + 字段映射 DAG（底层 AllPredecessor 触发）：START 整输出喂给 a/b 两
// 个透传节点；join 的输入 map[string]string 由 a/b 两路输出经 ToField 字段
// 映射填充（= AND 汇聚语义）。注：compose.Graph 的裸多入边 fan-in 对
// string 需内部 MergeFunc 注册（外部 module 不可达），Workflow 字段映射是
// 官方开箱姿势。
func runEinoDAG(ctx context.Context) (string, error) {
	pass := compose.InvokableLambda(func(_ context.Context, s string) (string, error) {
		return s, nil
	})
	join := compose.InvokableLambda(func(_ context.Context, m map[string]string) (string, error) {
		return m["a"] + m["b"], nil
	})
	wf := compose.NewWorkflow[string, string]()
	wf.AddLambdaNode("a", pass).AddInput(compose.START)
	wf.AddLambdaNode("b", pass).AddInput(compose.START)
	wf.AddLambdaNode("join", join).
		AddInput("a", compose.ToField("a")).
		AddInput("b", compose.ToField("b"))
	wf.AddEnd("join")
	r, err := wf.Compile(ctx)
	if err != nil {
		return "", err
	}
	out, err := r.Invoke(ctx, "war payload")
	if err != nil {
		return "", err
	}
	return out, nil
}
