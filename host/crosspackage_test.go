package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/toolset"
)

// captureModel 包住内层模型并记录最后一次请求——用于字面断言「续跑轮里
// 模型实际收到的 history 带着裁决文本」，而不是只从 Surface 间接推断。
type captureModel struct {
	inner llm.ChatModel
	last  *llm.GenerateRequest
}

func (m *captureModel) Generate(ctx context.Context, req *llm.GenerateRequest) (*llm.Response, error) {
	m.last = req
	return m.inner.Generate(ctx, req)
}

func (m *captureModel) Stream(ctx context.Context, req *llm.GenerateRequest) (<-chan llm.StreamEvent, error) {
	m.last = req
	return m.inner.Stream(ctx, req)
}

// TestHostCrossPackageHITLRecovery 跨包验收（host + memory/session + loop +
// kernel 四包拼起来）：Agent 跑到 HITL 等待点 → 进程猝死（不写 tool.result、
// 不闭合 turn/step）→ 以 ExposePending 冷恢复打开 → 宿主裁决（补**真实**
// 结果，回到等待点而非作废）→ 同一会话续跑。
//
// 各包内测试各自绿，这条钉的是「拼起来仍然成立」：崩溃现场可裁决、裁决
// 结果真实进入后续 history、未裁决时宿主不把 unpaired tool_call 喂给模型。
func TestHostCrossPackageHITLRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 模型脚本：第一轮请求工具（审批后才有结果），续跑轮给最终答复。
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "deploy", Arguments: json.RawMessage(`{"env":"prod"}`)}),
		llm.Resp("resumed and done"),
	)
	deploy := func(c *kernel.Context, reg *toolset.Registry) error {
		_, err := reg.Register(c, toolset.Registration{
			Def: llm.ToolDef{
				Name:        "deploy",
				Description: "deploys the app",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"env":{"type":"string"}}}`),
			},
			Fn:     func(ctx context.Context, args json.RawMessage) (string, error) { return "deployed:prod", nil },
			Source: "test.deploy",
			Risk:   toolset.RiskReadWrite,
		})
		return err
	}

	// ---- 第一段生命周期：装配 host，跑到 HITL 等待点后「猝死」 ----

	stack1, err := memory.NewJSONLSessionStack(dir)
	if err != nil {
		t.Fatal(err)
	}
	h1 := newTestHost(t, model, func(o *Options) {
		o.Session = stack1
		o.Tools = []ToolSource{deploy}
	})

	// ToolGate 阻塞在等待点：进入即报到，放行才继续（模拟审批人坐在对面）。
	gateEntered := make(chan struct{}, 1)
	gateRelease := make(chan struct{})
	gate := func(call llm.ToolCall) (bool, string) {
		gateEntered <- struct{}{}
		<-gateRelease
		return true, ""
	}
	a1, err := h1.DefaultAgent(ctx, DefaultAgentOptions{Name: "hitl", Model: "stub", ToolGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	sess1 := a1.Session()
	id := sess1.Header().SessionID

	roundErr := make(chan error, 1)
	go func() {
		_, err := a1.Run(ctx, llm.User(llm.Text("please deploy to prod")))
		roundErr <- err
	}()

	<-gateEntered // 已到等待点：assistant(tool_call) 落盘并经 HITL 检查点 Flush

	// 模拟进程猝死：直接关底层句柄——此后任何落盘都失败（崩溃后活着的
	// 写者写不进去的语义），日志停在等待点，没有 tool.result / turn.ended。
	if c, ok := sess1.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	close(gateRelease) // 放行闸门，让回合走完（预期因落盘失败而报错）
	if err := <-roundErr; err == nil {
		t.Fatal("round must fail after the persistence sink is gone (fail closed)")
	} else if !errors.Is(err, session.ErrSessionClosed) {
		// 锚语义不锚文案：Host 用 %w 包住 session 哨兵，句柄已关可直接判定。
		t.Fatalf("crash-path error = %v, want ErrSessionClosed (handle closed)", err)
	}

	// ---- 第二段生命周期：ExposePending 冷恢复 → 裁决 → 续跑 ----

	st2, err := session.NewJSONLStore(dir, session.WithRecoverPolicy(session.RecoverExposePending))
	if err != nil {
		t.Fatal(err)
	}
	stack2 := memory.NewSessionStack(st2)
	sess2, err := stack2.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := sess2.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})

	// 现场可裁决：c1 未决（带调用载荷）+ step/turn 悬空。
	rec, ok := sess2.(session.Recoverable)
	if !ok {
		t.Fatal("jsonl session must expose session.Recoverable")
	}
	p := rec.Pending()
	if len(p.Calls) != 1 || p.Calls[0].ToolCallID != "c1" || p.Calls[0].Name != "deploy" {
		t.Fatalf("pending calls = %+v, want c1/deploy", p.Calls)
	}
	if !p.HasOpenStep || !p.HasOpenTurn {
		t.Fatalf("pending lifecycle = %+v, want open step+turn", p)
	}

	capModel := &captureModel{inner: model}
	h2 := newTestHost(t, capModel, func(o *Options) {
		o.Session = stack2
		o.Tools = []ToolSource{deploy}
	})
	a2, err := h2.DefaultAgent(ctx, DefaultAgentOptions{Name: "resumed", Model: "stub", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}

	// 未裁决直接续跑：宿主在 Surface 处拒绝（unpaired tool_call 不喂模型）。
	if _, err := a2.Run(ctx, llm.User(llm.Text("continue"))); !errors.Is(err, session.ErrPendingEvents) {
		t.Fatalf("run without adjudication = %v, want ErrPendingEvents", err)
	}

	// 裁决：补真实结果（回到等待点）+ 依次闭合 step/turn；完成后自行 Flush。
	if err := rec.ResolvePending(ctx, session.ResolvePendingOption{
		Result: &session.ToolResultPayload{ToolCallID: "c1", Text: "approved: deployed to prod"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rec.ResolvePending(ctx, session.ResolvePendingOption{Interrupted: true}); err != nil {
		t.Fatal(err)
	}
	if err := rec.ResolvePending(ctx, session.ResolvePendingOption{Interrupted: true}); err != nil {
		t.Fatal(err)
	}
	if err := sess2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if p := rec.Pending(); len(p.Calls) != 0 || p.HasOpenStep || p.HasOpenTurn {
		t.Fatalf("pending after adjudication = %+v", p)
	}

	// 续跑：裁决结果真实进入 history，新一轮正常完成。
	res, err := a2.Run(ctx, llm.User(llm.Text("continue")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final == nil || res.Final.Text() != "resumed and done" {
		t.Fatalf("resumed final = %+v", res.Final)
	}

	surface, err := sess2.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range surface {
		roles = append(roles, string(m.Role))
	}
	want := "user,assistant,tool,user,assistant"
	if strings.Join(roles, ",") != want {
		t.Fatalf("resumed surface roles = %v, want %s", roles, want)
	}
	tr := surface[2].Parts[0].ToolResultValue
	if tr == nil || tr.ToolCallID != "c1" || len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, "approved: deployed to prod") {
		t.Fatalf("adjudicated tool result = %+v, want the real adjudicated text", tr)
	}

	// 字面闭环：续跑轮里模型**实际收到的请求**就带着裁决文本（不是从
	// Surface 反推——scripted 模型不读输入，这里直接查 captured request）。
	if capModel.last == nil {
		t.Fatal("resumed round never reached the model")
	}
	fed := false
	for _, m := range capModel.last.Messages {
		for _, part := range m.Parts {
			if part.Kind != llm.PartToolResult || part.ToolResultValue == nil {
				continue
			}
			for _, c := range part.ToolResultValue.Content {
				if strings.Contains(c.Text, "approved: deployed to prod") {
					fed = true
				}
			}
		}
	}
	if !fed {
		t.Fatal("resumed round did not feed the adjudicated tool result to the model")
	}
}
