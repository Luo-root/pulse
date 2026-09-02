package eval

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/observability"
)

// 基建 benchmark（评测三步走·第一步，Issue #101）：同一任务在不同装配
// 深度下的框架开销。全部用 ScriptedModel（零网络、零计算占位）——所有
// 差值可归因框架层，不被模型/网络方差污染。
//
// 分层与差值归因：
//
//	L0 裸调用            —— 基线：直接 ChatModel.Generate。
//	L1 Registry 装配      —— L1−L0 = llm.Plugin + observed 事件包装开销
//	                         （before_generate waterfall + after_response emit）。
//	L2a ReAct 单步文本回合 —— L2a−L1 = loop.Agent 的回合循环与结果聚合
//	                         （无工具路径；Agent 复用，构造不摊进每轮）。
//	L2b ReAct 完整工具往返 —— L2b−L2a = 工具调用往返 + Agent/模型每轮重建
//	                         （构造计入：Scripted 耗尽后停在末条，完整
//	                         两步脚本必须每轮新配；差值是上界）。
//	L3 会话记账           —— L3−L2a = event-sourced 记账的真实代价
//	                         （每轮 user/assistant 两条事件 Append）。
//
// 运行：go test -bench . -benchmem -run '^$' ./eval/
// 数字是单机参考值：比的是同机分层差值，不是绝对值，不作通用结论。

// nopSink 是黑洞 Sink：benchmark 只测框架路径开销，不让 MemorySink 的
// 无限增长污染 allocs 口径。
type nopSink struct{}

func (nopSink) Write(observability.Record) {}

// benchReq 构造固定单消息请求（所有层同一输入）。
func benchReq() *llm.GenerateRequest {
	return llm.NewRequest(llm.UserText("benchmark payload for layered overhead measurement"))
}

// BenchmarkL0BareGenerate 基线：不经任何框架装配，直接模型调用。
func BenchmarkL0BareGenerate(b *testing.B) {
	model := llm.NewScripted(llm.Resp("ok"))
	req := benchReq()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := model.Generate(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkL1RegistryGenerate L1：llm.Plugin 装配（Registry + observed
// 包装 + kernel 事件派发）后经命名实例调用。装配只做一次，循环内是纯请求路径。
func BenchmarkL1RegistryGenerate(b *testing.B) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, observability.Bootstrap("bench", nopSink{})); err != nil {
		b.Fatal(err)
	}
	if _, err := kernel.Use(host, llm.Plugin()); err != nil {
		b.Fatal(err)
	}
	reg, ok := kernel.Get(host, llm.ServiceKey)
	if !ok {
		b.Fatal("registry missing")
	}
	if _, err := reg.RegisterProvider(host, "scripted", func(llm.Config) (llm.ChatModel, error) {
		return llm.NewScripted(llm.Resp("ok")), nil
	}); err != nil {
		b.Fatal(err)
	}
	if err := reg.Declare("main", llm.Config{Provider: "scripted", Model: "scripted"}); err != nil {
		b.Fatal(err)
	}
	model, err := reg.Open("main")
	if err != nil {
		b.Fatal(err)
	}
	req := benchReq()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := model.Generate(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkL2AgentTextTurn L2a：loop.Agent 单步文本回合（脚本只含最终回
// 答，零工具调用）。Agent 复用——构造与装配不摊进每轮，测的是回合循环
// 本身（事件派发、消息聚合、结果组装）。
func BenchmarkL2AgentTextTurn(b *testing.B) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, observability.Bootstrap("bench", nopSink{})); err != nil {
		b.Fatal(err)
	}
	if _, err := kernel.Use(host, llm.Plugin()); err != nil {
		b.Fatal(err)
	}
	reg, ok := kernel.Get(host, llm.ServiceKey)
	if !ok {
		b.Fatal("registry missing")
	}
	if _, err := reg.RegisterProvider(host, "scripted", func(llm.Config) (llm.ChatModel, error) {
		return llm.NewScripted(llm.Resp("ok")), nil
	}); err != nil {
		b.Fatal(err)
	}
	if err := reg.Declare("main", llm.Config{Provider: "scripted", Model: "scripted"}); err != nil {
		b.Fatal(err)
	}
	model, err := reg.Open("main")
	if err != nil {
		b.Fatal(err)
	}
	scope, err := host.Derive()
	if err != nil {
		b.Fatal(err)
	}
	defer scope.Dispose()
	agent, err := loop.NewAgent(model, loop.WithEventScope(scope))
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := agent.Run(ctx, nil, llm.UserText("benchmark payload"))
		if err != nil {
			b.Fatal(err)
		}
		if res.StoppedBy != loop.StopCompleted {
			b.Fatalf("stopped_by = %s", res.StoppedBy)
		}
	}
}

// BenchmarkL2AgentToolRound L2b：完整 ReAct 工具往返（tool_calls → 执行 →
// 结果回填 → 最终回答）。Scripted 耗尽后停在末条，两步脚本必须每轮重建
// 模型与 Agent——构造计入本层，故对 L2a 的差值是工具往返开销的**上界**。
func BenchmarkL2AgentToolRound(b *testing.B) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, observability.Bootstrap("bench", nopSink{})); err != nil {
		b.Fatal(err)
	}
	if _, err := kernel.Use(host, llm.Plugin()); err != nil {
		b.Fatal(err)
	}
	scope, err := host.Derive()
	if err != nil {
		b.Fatal(err)
	}
	defer scope.Dispose()
	tools := loop.NewMemToolSet()
	if err := tools.Register(llm.ToolDef{
		Name:        "lookup",
		Description: "bench tool",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}, func(_ context.Context, _ json.RawMessage) (string, error) {
		return `{"ok":true}`, nil
	}); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		model := llm.NewScripted(
			llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
			llm.Resp("done"),
		)
		agent, err := loop.NewAgent(model, loop.WithToolSet(tools), loop.WithEventScope(scope))
		if err != nil {
			b.Fatal(err)
		}
		res, err := agent.Run(ctx, nil, llm.UserText("benchmark payload"))
		if err != nil {
			b.Fatal(err)
		}
		if res.StoppedBy != loop.StopCompleted || res.Steps != 2 {
			b.Fatalf("stopped_by = %s steps = %d", res.StoppedBy, res.Steps)
		}
	}
}

// BenchmarkL3SessionBookkeeping L3：L2a 的每轮 + event-sourced 记账（user /
// assistant 两条事件 Append，含 surface 意图与 codec 编码）。会话与事件存
// 储复用，测的是每轮记账的增量代价——不做全量 Surface（fold 是 O(日志长)，
// 随迭代增长会失真；折叠语义已由 property tests 钉住）。
func BenchmarkL3SessionBookkeeping(b *testing.B) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, observability.Bootstrap("bench", nopSink{})); err != nil {
		b.Fatal(err)
	}
	if _, err := kernel.Use(host, llm.Plugin()); err != nil {
		b.Fatal(err)
	}
	reg, ok := kernel.Get(host, llm.ServiceKey)
	if !ok {
		b.Fatal("registry missing")
	}
	if _, err := reg.RegisterProvider(host, "scripted", func(llm.Config) (llm.ChatModel, error) {
		return llm.NewScripted(llm.Resp("ok")), nil
	}); err != nil {
		b.Fatal(err)
	}
	if err := reg.Declare("main", llm.Config{Provider: "scripted", Model: "scripted"}); err != nil {
		b.Fatal(err)
	}
	model, err := reg.Open("main")
	if err != nil {
		b.Fatal(err)
	}
	scope, err := host.Derive()
	if err != nil {
		b.Fatal(err)
	}
	defer scope.Dispose()
	agent, err := loop.NewAgent(model, loop.WithEventScope(scope))
	if err != nil {
		b.Fatal(err)
	}
	sess, err := session.NewMemoryStore().Create(context.Background(), session.SessionHeader{})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := agent.Run(ctx, nil, llm.UserText("benchmark payload"))
		if err != nil {
			b.Fatal(err)
		}
		user := session.EventDraft{
			Type:    session.EventMessageUser,
			Data:    mustJSON(session.MessagePayload{Parts: []llm.Part{llm.Text("benchmark payload")}}),
			Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
		}
		if _, err := sess.Append(ctx, user); err != nil {
			b.Fatal(err)
		}
		assistant := session.EventDraft{
			Type:    session.EventMessageAssistant,
			Data:    mustJSON(session.MessagePayload{Parts: []llm.Part{llm.Text(res.Final.Text())}}),
			Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
		}
		if _, err := sess.Append(ctx, assistant); err != nil {
			b.Fatal(err)
		}
	}
}
