package war

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

// BenchmarkWar_PulseTextRound T1 单步文本回合：Pulse 全生产装配，每轮重建
// 模型与 Agent（上界口径，构造计入）。
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

// BenchmarkWar_PulseTextRoundReused T1 复用版：装配一次（模型 + Agent），
// 循环只跑回合——把「构造成本」从运行成本中剥离（单条脚本耗尽后停在末
// 条，每轮返回同一文本，复用语义成立）。与 #102 L2a 同口径。
func BenchmarkWar_PulseTextRoundReused(b *testing.B) {
	ctx := context.Background()
	model := llm.NewScripted(llm.Resp("done"))
	agent, err := loop.NewAgent(model)
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
		einoToolHits = 0
		if err := runEinoToolRound(ctx); err != nil {
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
}
