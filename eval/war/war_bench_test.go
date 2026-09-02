package war

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/observability"
)

// BenchmarkWar_PulseTextRound T1 单步文本回合（冷启动口径）：Pulse 全家桶
// 生产装配每轮重建（kernel 宿主 + Registry + observed + scope + 模型 +
// Agent），度量「从零装配到跑完一个回合」的完整价格。
func BenchmarkWar_PulseTextRound(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := runPulseTextRound(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWar_PulseTextRoundReused T1 复用版：全家桶装配一次（kernel 宿主
// + observability + llm.Registry + observed 模型 + 请求 scope，Agent 复用），
// 循环只跑回合——把构造成本从运行成本中剥离。单条脚本耗尽后停在末条
// （每轮返回同一文本），复用语义成立。对齐 #102 L2a 原味口径。
func BenchmarkWar_PulseTextRoundReused(b *testing.B) {
	ctx := context.Background()
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, observability.Bootstrap("war", nopSink{})); err != nil {
		b.Fatal(err)
	}
	model, scope, err := assemblePulseRegistry(host, llm.Resp("done"))
	if err != nil {
		b.Fatal(err)
	}
	agent, err := loop.NewAgent(model, loop.WithEventScope(scope))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := agent.Run(ctx, nil, llm.UserText("war payload"))
		if err != nil {
			b.Fatal(err)
		}
		if res.StoppedBy != loop.StopCompleted {
			b.Fatalf("stopped_by = %s", res.StoppedBy)
		}
	}
}

// BenchmarkWar_EinoTextRoundReused T1 复用版（Eino 侧）：同样装配一次
// ChatModelAgent + Runner，循环只跑回合——与 Pulse 复用版对齐。
func BenchmarkWar_EinoTextRoundReused(b *testing.B) {
	ctx := context.Background()
	cm := newEinoStub(assistantText("done"))
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:  "war",
		Model: cm,
	})
	if err != nil {
		b.Fatal(err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter := runner.Run(ctx, []*schema.Message{schema.UserMessage("war payload")})
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}
			if event.Err != nil {
				b.Fatal(event.Err)
			}
		}
	}
}

// BenchmarkWar_PulseToolRound T2 工具往返：Pulse 全生产装配（含工具声明、
// 执行、回填），每轮重建（上界口径）。
func BenchmarkWar_PulseToolRound(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := runPulseToolRound(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWar_EinoTextRound T1 单步文本回合：Eino ChatModelAgent + Runner
// 全生产装配，每轮重建 stub 模型与 agent（与 Pulse 侧同口径）。
func BenchmarkWar_EinoTextRound(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := runEinoTextRound(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWar_EinoToolRound T2 工具往返：Eino ChatModelAgent（ToolsNode
// 执行工具）+ Runner，每轮重建（与 Pulse 侧同口径）。
func BenchmarkWar_EinoToolRound(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := runEinoToolRound(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// ---- 编排执行器对比（T3/T4）----

// BenchmarkWar_PulseFlowChain T3 线性链（3 透传节点）：Pulse kernel/flow
// Graph（Add + Seed + Run），每轮重建（冷启动口径）。
func BenchmarkWar_PulseFlowChain(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runPulseFlowChain(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWar_EinoChain T3 线性链：Eino compose.Chain（AppendLambda ×3 +
// Compile + Invoke），每轮重建（冷启动口径）。
func BenchmarkWar_EinoChain(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runEinoChain(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWar_PulseFlowDAG T4 分支汇聚（1→2→AND join）：Pulse kernel/flow
//（AND 槽位语义），每轮重建（冷启动口径）。
func BenchmarkWar_PulseFlowDAG(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runPulseFlowDAG(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWar_EinoDAGGraph T4 分支汇聚（Eino 变体 a）：compose.Graph +
// AllPredecessor + WithOutputKey 键化 fan-in——测「join 调度价」；与
// Workflow 变体（BenchmarkWar_EinoDAG）之差 = Workflow 字段映射税。每轮
// 重建（冷启动口径）。
func BenchmarkWar_EinoDAGGraph(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runEinoDAGGraph(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWar_EinoDAG T4 分支汇聚（Eino 变体 b）：compose.Workflow
//（AddLambdaNode + ToField 字段映射 AND 汇聚），每轮重建（冷启动口径）。
func BenchmarkWar_EinoDAG(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runEinoDAG(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// TestWarSanity 是 benchmark 的正确性哨兵：每个参赛方每条任务至少跑通
// 一次，工具确实被执行（防止 benchmark 静默退化成空转）。
func TestWarSanity(t *testing.T) {
	ctx := context.Background()
	if err := runPulseTextRound(ctx); err != nil {
		t.Fatalf("pulse text: %v", err)
	}
	if err := runPulseToolRound(ctx); err != nil {
		t.Fatalf("pulse tool: %v", err)
	}
	if pulseToolHits == 0 {
		t.Fatal("pulse tool never executed")
	}
	if err := runEinoTextRound(ctx); err != nil {
		t.Fatalf("eino text: %v", err)
	}
	if err := runEinoToolRound(ctx); err != nil {
		t.Fatalf("eino tool: %v", err)
	}
	// 编排任务：输出正确性断言（透传链保真 + DAG 拼接）。
	chainOut, err := runPulseFlowChain(ctx)
	if err != nil {
		t.Fatalf("pulse flow chain: %v", err)
	}
	if want := "war payload"; chainOut != want {
		t.Fatalf("pulse flow chain out = %q, want %q", chainOut, want)
	}
	einoChainOut, err := runEinoChain(ctx)
	if err != nil {
		t.Fatalf("eino chain: %v", err)
	}
	if want := "war payload"; einoChainOut != want {
		t.Fatalf("eino chain out = %q, want %q", einoChainOut, want)
	}
	out, err := runPulseFlowDAG(ctx)
	if err != nil {
		t.Fatalf("pulse flow dag: %v", err)
	}
	if want := "war payloadwar payload"; out != want {
		t.Fatalf("pulse flow dag out = %q, want %q", out, want)
	}
	graphOut, err := runEinoDAGGraph(ctx)
	if err != nil {
		t.Fatalf("eino graph dag: %v", err)
	}
	if want := "war payloadwar payload"; graphOut != want {
		t.Fatalf("eino graph dag out = %q, want %q", graphOut, want)
	}
	dagOut, err := runEinoDAG(ctx)
	if err != nil {
		t.Fatalf("eino dag: %v", err)
	}
	if want := "war payloadwar payload"; dagOut != want {
		t.Fatalf("eino dag out = %q, want %q", dagOut, want)
	}
}
