package loop

// #253-1 的回归用例：after_tool_call 的 Duration 只计工具本体。

import (
	"context"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
)

// TestAfterToolCallDurationExcludesApprovalWait：Duration 不含 before_tool_call
// 水位上的审批等待（真实场景是人批，可能等几分钟）。修复前 start 取在水位之前，
// 一个几乎不耗时的工具会被记成「耗时 = 审批等待」。
func TestAfterToolCallDurationExcludesApprovalWait(t *testing.T) {
	scope := kernel.New()
	defer scope.Dispose()
	const approvalWait = 200 * time.Millisecond

	if _, err := kernel.OnWaterfall(scope, EventBeforeToolCall,
		func(p *BeforeToolCall, next func(*BeforeToolCall) *BeforeToolCall) *BeforeToolCall {
			time.Sleep(approvalWait) // 模拟人批：等待与工具本体无关
			return next(p)
		}); err != nil {
		t.Fatal(err)
	}
	var got time.Duration
	fired := 0
	if _, err := kernel.On(scope, EventAfterToolCall, func(p *AfterToolCall) {
		fired++
		got = p.Duration
	}); err != nil {
		t.Fatal(err)
	}

	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: []byte(`{}`)}),
		respWith(llm.TokenUsage{InputTokens: 1, OutputTokens: 1}, llm.Text("done")),
	)
	tools := &fakeTools{fn: func(llm.ToolCall) (string, error) { return "pong", nil }}
	a := newTestAgent(t, model, tools, scope)

	if _, err := a.Run(context.Background(), nil, llm.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("after_tool_call 事件 = %d 次，want 1", fired)
	}
	// 工具本体是空实现：修复后这里量到的是「几乎为 0」（0 也是合法值，本机
	// 时钟粒度下实测就是 0s），修复前则是审批等待那段。
	if got >= approvalWait/2 {
		t.Fatalf("Duration = %v：审批等待（%v）被算进了工具耗时", got, approvalWait)
	}
}
