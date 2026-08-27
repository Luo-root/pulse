package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

// capturingModel 截获发给模型的完整消息列表，供断言 prompt 组装契约。
type capturingModel struct {
	mu      sync.Mutex
	request []*llm.Message
}

func (c *capturingModel) Generate(_ context.Context, req *llm.GenerateRequest) (*llm.Response, error) {
	c.mu.Lock()
	c.request = req.Messages
	c.mu.Unlock()
	return llm.Resp("answered"), nil
}

func (c *capturingModel) Stream(ctx context.Context, req *llm.GenerateRequest) (<-chan llm.StreamEvent, error) {
	resp, err := c.Generate(ctx, req)
	out := make(chan llm.StreamEvent, 1)
	if err != nil {
		out <- llm.StreamEvent{Kind: llm.EventError, Err: err}
	} else {
		out <- llm.StreamEvent{Kind: llm.EventDone, Response: resp}
	}
	close(out)
	return out, nil
}

func (c *capturingModel) lastText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, m := range c.request {
		b.WriteString(m.Text())
		b.WriteString("\n")
	}
	return b.String()
}

func newTestHost(t *testing.T) *demoapp.Host {
	t.Helper()
	h, err := demoapp.Open(demoapp.Flags{Scripted: true}, llm.Resp("ok"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func newTestAgent(t *testing.T, h *demoapp.Host) *loop.Agent {
	t.Helper()
	return mustAgent(t, h.Model)
}

func newCapturingAgent(t *testing.T, m *capturingModel) (*loop.Agent, *capturingModel) {
	t.Helper()
	return mustAgent(t, m), m
}

func mustAgent(t *testing.T, model llm.ChatModel) *loop.Agent {
	t.Helper()
	a, err := loop.NewAgent(model)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

type errRetriever struct{}

func (errRetriever) Search(context.Context, string, int) ([]Document, error) {
	return nil, errors.New("search backend down")
}

// 空命中是合法数据：图成功跑完，且 answer 的 prompt 必须带上查询原文与「无命中」标注。
func TestRunGraphEmptyHitIsData(t *testing.T) {
	h := newTestHost(t)
	r := memoryRetriever{docs: []Document{{ID: "k1", Title: "kernel", Content: "卸载即还原"}}}
	agent, cap := newCapturingAgent(t, &capturingModel{})
	res, dur, err := runGraph(h, agent, r, nil, llm.UserText("晚饭吃什么"), "t")
	if err != nil {
		t.Fatalf("empty hit must not fail graph: %v", err)
	}
	if res == nil || res.Final == nil {
		t.Fatal("missing result")
	}
	_ = dur // 流程时序不为负即可；精确阈值受调度影响不做硬断言
	prompt := cap.lastText()
	if !strings.Contains(prompt, "检索查询：晚饭吃什么") {
		t.Fatalf("prompt missing query line (QueryText not consumed): %q", prompt)
	}
	if !strings.Contains(prompt, "无命中") {
		t.Fatalf("prompt missing empty-hitker: %q", prompt)
	}
	if strings.Contains(prompt, "卸载即还原") {
		t.Fatalf("empty hit leaked docs into prompt: %q", prompt)
	}
}

// 检索失败 → 节点 error → 取消整图，runGraph 返回该错误且无结果。
func TestRunGraphRetrievalErrorCancels(t *testing.T) {
	h := newTestHost(t)
	res, _, err := runGraph(h, newTestAgent(t, h), errRetriever{}, nil, llm.UserText("任意"), "t")
	if err == nil {
		t.Fatal("expected retrieval error to cancel graph")
	}
	if !strings.Contains(err.Error(), "search backend down") {
		t.Fatalf("want root cause surfaced, got %v", err)
	}
	if res != nil {
		t.Fatal("no result should be produced after cancellation")
	}
}

// 命中路径：QueryText 与文档标题/内容都进入 answer 的 prompt（AND 三键真实消费）。
func TestRunGraphHitConsumesQueryAndDocs(t *testing.T) {
	h := newTestHost(t)
	r := memoryRetriever{docs: []Document{
		{ID: "k1", Title: "kernel", Content: "卸载即还原，依赖响应式装载。"},
	}}
	agent, cap := newCapturingAgent(t, &capturingModel{})
	if _, _, err := runGraph(h, agent, r, nil, llm.UserText("kernel 卸载"), "t"); err != nil {
		t.Fatal(err)
	}
	prompt := cap.lastText()
	for _, want := range []string{"检索查询：kernel 卸载", "kernel", "卸载即还原"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q; got %q", want, prompt)
		}
	}
}
