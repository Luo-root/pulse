package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

// TestPropertyToolRejectionSemantics 拒绝语义不变式（loop）：
//
//	R1 被拒工具绝不执行（副作用计数为零）；
//	R2 每个被拒调用都有 IsError 工具结果回传，文本含拒绝原因——模型可
//	   自我修正；
//	R3 拒绝使回合 completed 而非 error（裁决是回传信息，不是崩溃）；
//	R4 放行工具恰好执行一次（Waterfall 透传 next 不重复、不丢失）。
//
// 随机维度：工具数量、每工具是否被拒、拒绝原因文本、每回合调用序列。
func TestPropertyToolRejectionSemantics(t *testing.T) {
	seed := seedFor(t.Name())
	for iter := 0; iter < 8; iter++ {
		r := newRng(seed + int64(iter)*40503)

		// ① 随机工具集：handler 记录执行次数（副作用痕迹）。
		tools := loop.NewMemToolSet()
		var mu sync.Mutex
		exec := map[string]int{}
		nTools := 1 + r.IntN(3)
		names := make([]string, 0, nTools)
		for i := 0; i < nTools; i++ {
			name := "tool_" + r.randStr(6)
			names = append(names, name)
			target := name
			if err := tools.Register(llm.ToolDef{
				Name:        name,
				Description: "eval tool",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			}, func(_ context.Context, _ json.RawMessage) (string, error) {
				mu.Lock()
				exec[target]++
				mu.Unlock()
				return "ok", nil
			}); err != nil {
				t.Fatal(r.failf("iter=%d: register %s: %v", iter, name, err))
			}
		}

		// ② 随机调用序列（1~2 个调用）+ 随机拒绝策略。
		var calls []llm.ToolCall
		for i := 0; i < 1+r.IntN(2); i++ {
			calls = append(calls, llm.ToolCall{
				ID:        fmt.Sprintf("c%d", i),
				Name:      pick(r, names),
				Arguments: json.RawMessage(`{}`),
			})
		}
		deny := map[string]bool{}
		for _, n := range names {
			if r.IntN(2) == 0 {
				deny[n] = true
			}
		}
		reason := "denied:" + r.randStr(6)

		model := llm.NewScripted(llm.RespToolCalls(calls...), llm.Resp("done"))

		// ③ 装配：reqScope + HITL 监听 + Agent（同 scope 才听得到）。
		host := kernel.New()
		scope, err := host.Derive()
		if err != nil {
			t.Fatal(r.failf("iter=%d: derive: %v", iter, err))
		}
		_, err = kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
			func(btc *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
				if deny[btc.Call.Name] {
					btc.Rejected = true
					btc.RejectReason = reason
					return btc // 不调 next = 拒绝
				}
				return next(btc)
			})
		if err != nil {
			host.Dispose()
			t.Fatal(r.failf("iter=%d: install listener: %v", iter, err))
		}
		agent, err := loop.NewAgent(model,
			loop.WithToolSet(tools),
			loop.WithEventScope(scope),
		)
		if err != nil {
			host.Dispose()
			t.Fatal(r.failf("iter=%d: agent: %v", iter, err))
		}

		res, err := agent.Run(context.Background(), nil, llm.UserText("go"))
		host.Dispose()
		if err != nil {
			t.Fatal(r.failf("iter=%d: run: %v", iter, err))
		}

		// R3：拒绝使回合 completed。
		if res.StoppedBy != loop.StopCompleted {
			t.Fatal(r.failf("iter=%d: stopped_by = %s, want completed（R3：拒绝不是失败）",
				iter, res.StoppedBy))
		}

		// R1：副作用与放行次数精确一致。
		wantExec := map[string]int{}
		for _, c := range calls {
			if !deny[c.Name] {
				wantExec[c.Name]++
			}
		}
		for _, n := range names {
			mu.Lock()
			got := exec[n]
			mu.Unlock()
			if got != wantExec[n] {
				t.Fatal(r.failf("iter=%d: tool %s executed %d times, want %d（R1：被拒不得执行）",
					iter, n, got, wantExec[n]))
			}
		}

		// R2：每个被拒调用都有 IsError 结果且含原因。
		for _, c := range calls {
			if !deny[c.Name] {
				continue
			}
			found := false
			for _, m := range res.Messages {
				for _, p := range m.Parts {
					if p.Kind != llm.PartToolResult || p.ToolResultValue == nil {
						continue
					}
					if p.ToolResultValue.ToolCallID != c.ID {
						continue
					}
					found = true
					if !p.ToolResultValue.IsError {
						t.Fatal(r.failf("iter=%d: rejected call %s result not IsError", iter, c.ID))
					}
					var sb strings.Builder
					for _, cp := range p.ToolResultValue.Content {
						sb.WriteString(cp.Text)
					}
					if !strings.Contains(sb.String(), reason) {
						t.Fatal(r.failf("iter=%d: rejected call %s result %q lacks reason %q",
							iter, c.ID, sb.String(), reason))
					}
				}
			}
			if !found {
				t.Fatal(r.failf("iter=%d: rejected call %s has no tool result", iter, c.ID))
			}
		}
	}
}
