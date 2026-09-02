package war

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// einoStubModel 是 Eino 参赛方的等薄 stub：按序返回脚本消息，耗尽后停在
// 末条——与 Pulse llm.NewScripted 的语义逐点对齐（互斥锁 + 下标推进 +
// 浅拷贝），保证两边 stub 薄度一致。
type einoStubModel struct {
	mu  sync.Mutex
	seq []*schema.Message
	idx int
}

func newEinoStub(seq ...*schema.Message) *einoStubModel {
	return &einoStubModel{seq: seq}
}

// Generate 实现 model.BaseChatModel。忽略输入与 options——stub 不解析
// tools 注入（真实模型解析 tool 声明属于模型行为，不属于框架开销）。
func (m *einoStubModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.seq) {
		m.idx = len(m.seq) - 1
	}
	s := m.seq[m.idx]
	m.idx++
	cp := *s
	return &cp, nil
}

// Stream 实现 model.BaseChatModel：把单条响应包成单元素流（schema.Pipe
// 模式，与 Eino 官方测试 stub 同构）。非流式任务集只走 Generate。
func (m *einoStubModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	r, w := schema.Pipe[*schema.Message](1)
	go func() {
		defer w.Close()
		w.Send(msg, nil)
	}()
	return r, nil
}

// warTool 是 Eino 参赛方的等薄工具：实现 tool.InvokableTool（Info +
// InvokableRun），执行体 = 一次计数 + 固定 JSON 返回。
type warTool struct {
	mu   sync.Mutex
	hits int
}

func (t *warTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "lookup",
		Desc: "war benchmark tool",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"topic": {Type: schema.DataType("string"), Required: true},
		}),
	}, nil
}

func (t *warTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	t.mu.Lock()
	t.hits++
	t.mu.Unlock()
	return `{"ok":true}`, nil
}
