package llm

import (
	"context"
	"sync"
)

// ScriptedModel 是按脚本顺序回放的测试模型：Generate 依次返回
// 预置的响应（或错误），Stream 把同一响应拆为文本增量 + done。
//
// 用法：
//
//	m := llm.NewScripted(llm.Resp("你好"), llm.Resp("再见"))
type ScriptedModel struct {
	mu   sync.Mutex
	seq  []scriptedStep
	idx  int
}

type scriptedStep struct {
	resp *Response
	err  error
}

// Resp 构造一个纯文本助手响应。
func Resp(text string) *Response {
	return &Response{
		Message:      AssistantText(text),
		FinishReason: FinishStop,
	}
}

// RespToolCalls 构造一个工具调用响应。
func RespToolCalls(calls ...ToolCall) *Response {
	parts := make([]Part, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, Call(c))
	}
	return &Response{Message: Assistant(parts...), FinishReason: FinishToolCalls}
}

// NewScripted 创建按序回放的模型。脚本耗尽后重复最后一个条目。
func NewScripted(steps ...*Response) *ScriptedModel {
	m := &ScriptedModel{}
	for _, r := range steps {
		m.seq = append(m.seq, scriptedStep{resp: r})
	}
	return m
}

// NewFailing 创建始终返回指定错误的模型。
func NewFailing(err error) *ScriptedModel {
	return &ScriptedModel{seq: []scriptedStep{{err: err}}}
}

// Next 取出当前脚本条目（耗尽则停在最后）。
func (m *ScriptedModel) next() scriptedStep {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.seq) == 0 {
		return scriptedStep{}
	}
	if m.idx >= len(m.seq) {
		m.idx = len(m.seq) - 1
	}
	s := m.seq[m.idx]
	m.idx++
	return s
}

// Generate 实现 ChatModel。
func (m *ScriptedModel) Generate(_ context.Context, _ *GenerateRequest) (*Response, error) {
	s := m.next()
	if s.err != nil {
		return nil, s.err
	}
	cp := *s.resp
	return &cp, nil
}

// Stream 实现 ChatModel：把脚本文本拆为若干增量事件后 done；
// 错误脚本以 error 事件收尾。
func (m *ScriptedModel) Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamEvent, error) {
	s := m.next()
	out := make(chan StreamEvent, 4)
	go func() {
		defer close(out)
		if s.err != nil {
			out <- StreamEvent{Kind: EventError, Err: s.err}
			return
		}
		resp := *s.resp
		text := ""
		var calls []ToolCall
		if resp.Message != nil {
			text = resp.Message.Text()
			calls = resp.Message.ToolCalls()
		}
		if text != "" {
			select {
			case out <- StreamEvent{Kind: EventTextDelta, Text: text}:
			case <-ctx.Done():
				out <- StreamEvent{Kind: EventError, Err: ctx.Err()}
				return
			}
		}
		for i, c := range calls {
			out <- StreamEvent{Kind: EventToolCallBegin, Index: i, CallID: c.ID, ToolName: c.Name}
			out <- StreamEvent{Kind: EventToolCallDelta, Index: i, Text: string(c.Arguments)}
		}
		out <- StreamEvent{Kind: EventDone, Response: &resp}
	}()
	return out, nil
}

// 编译期断言。
var _ ChatModel = (*ScriptedModel)(nil)
