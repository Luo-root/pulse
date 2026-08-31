package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	flowyaml "github.com/Luo-root/pulse/kernel/flow/yaml"
	"github.com/Luo-root/pulse/observability"
)

// ---- RAG 线性图测试（原 03-flow-agent）----

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

// testBridge 为测试请求创建独立桥（挂宿主私有子作用域）。
func testBridge(t *testing.T, h *demoapp.Host) *demoapp.Bridge {
	t.Helper()
	scope, err := h.Ctx.Derive()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scope.Dispose)
	b, err := h.NewBridge(scope)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 空命中是合法数据：图成功跑完，且 answer 的 prompt 必须带上查询原文与「无命中」标注。
func TestRunGraphEmptyHitIsData(t *testing.T) {
	h := newTestHost(t)
	r := memoryRetriever{docs: []Document{{Title: "kernel", Content: "卸载即还原"}}}
	agent, cap := newCapturingAgent(t, &capturingModel{})
	res, _, err := runRAGGraph(h, agent, r, nil, llm.UserText("晚饭吃什么"), testBridge(t, h))
	if err != nil {
		t.Fatalf("empty hit must not fail graph: %v", err)
	}
	if res == nil || res.Final == nil {
		t.Fatal("missing result")
	}
	prompt := cap.lastText()
	if !strings.Contains(prompt, "检索查询：晚饭吃什么") {
		t.Fatalf("prompt missing query line (QueryText not consumed): %q", prompt)
	}
	if !strings.Contains(prompt, "无命中") {
		t.Fatalf("prompt missing empty-hit marker: %q", prompt)
	}
	if strings.Contains(prompt, "卸载即还原") {
		t.Fatalf("empty hit leaked docs into prompt: %q", prompt)
	}
}

// 检索失败 → 节点 error → 取消整图，runRAGGraph 返回该错误且无结果。
func TestRunGraphRetrievalErrorCancels(t *testing.T) {
	h := newTestHost(t)
	res, _, err := runRAGGraph(h, mustAgent(t, h.Model), errRetriever{}, nil, llm.UserText("任意"), testBridge(t, h))
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
		{Title: "kernel", Content: "卸载即还原，依赖响应式装载。"},
	}}
	agent, cap := newCapturingAgent(t, &capturingModel{})
	if _, _, err := runRAGGraph(h, agent, r, nil, llm.UserText("kernel 卸载"), testBridge(t, h)); err != nil {
		t.Fatal(err)
	}
	prompt := cap.lastText()
	for _, want := range []string{"检索查询：kernel 卸载", "kernel", "卸载即还原"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q; got %q", want, prompt)
		}
	}
}

func newCapturingAgent(t *testing.T, m *capturingModel) (*loop.Agent, *capturingModel) {
	t.Helper()
	return mustAgent(t, m), m
}

// ---- DAG 分支图测试（原 04-flow-dag）----

func testDeps(local, web Retriever) (dagDeps, *observability.MemorySink, *demoapp.FlowPeak) {
	sink := &observability.MemorySink{}
	peak := &demoapp.FlowPeak{}
	return dagDeps{
		local: local,
		web:   web,
		sink:  sink,
		peak:  peak,
		host:  "host-test",
		trace: "trace-1",
	}, sink, peak
}

func TestFactPathParallelPeakAndRecords(t *testing.T) {
	local0, web0 := defaultRetrievers()
	local := memoryRetriever{source: "local", delay: 40 * time.Millisecond, docs: local0.(memoryRetriever).docs}
	web := memoryRetriever{source: "web", delay: 40 * time.Millisecond, docs: web0.(memoryRetriever).docs}
	deps, sink, peak := testDeps(local, web)

	res, _, err := runDAG("flow Requires AND", deps)
	if err != nil {
		t.Fatal(err)
	}
	if res.Intent != "fact" {
		t.Fatalf("intent=%s", res.Intent)
	}
	if !strings.Contains(res.Final, "事实回答") {
		t.Fatalf("final=%q", res.Final)
	}
	if peak.Peak() < 2 {
		t.Fatalf("alive peak=%d, want >= 2 for parallel retrieves", peak.Peak())
	}

	runs := map[string]int{}
	waits := map[string]int{}
	for _, rec := range sink.Snapshot() {
		if rec.Source != observability.SourceBridge {
			continue
		}
		switch rec.Event {
		case demoapp.EventFlowNodeWaitFinished:
			waits[rec.FiberName]++
		case demoapp.EventFlowNodeRunFinished:
			runs[rec.FiberName]++
		}
	}
	for _, id := range []string{"retrieve_local", "retrieve_web"} {
		if waits[id] != 1 || runs[id] != 1 {
			t.Fatalf("%s waits=%d runs=%d; waits=%v runs=%v", id, waits[id], runs[id], waits, runs)
		}
	}
	if runs["smalltalk"] != 0 {
		t.Fatal("fact path must not run smalltalk")
	}
}

func TestChitchatSkipsRetrieves(t *testing.T) {
	local, web := defaultRetrievers()
	deps, sink, _ := testDeps(local, web)
	res, _, err := runDAG("你好", deps)
	if err != nil {
		t.Fatal(err)
	}
	if res.Intent != "chitchat" {
		t.Fatalf("intent=%s", res.Intent)
	}
	if !strings.Contains(res.Final, "闲聊") {
		t.Fatalf("final=%q", res.Final)
	}
	var smalltalkRun bool
	waitSkip := map[string]bool{}
	for _, rec := range sink.Snapshot() {
		switch rec.Event {
		case demoapp.EventFlowNodeRunFinished:
			switch rec.FiberName {
			case "retrieve_local", "retrieve_web", "merge", "answer":
				t.Fatalf("chitchat must not run %s", rec.FiberName)
			case "smalltalk":
				smalltalkRun = true
			}
		case demoapp.EventFlowNodeWaitFinished:
			if rec.Status == string(flow.NodeSkipped) {
				waitSkip[rec.FiberName] = true
			}
		}
	}
	if !smalltalkRun {
		t.Fatal("chitchat must run smalltalk")
	}
	for _, id := range []string{"retrieve_local", "retrieve_web"} {
		if !waitSkip[id] {
			t.Fatalf("%s should have skipped wait record", id)
		}
	}
}

func TestRetrieveFailureCancelsGraph(t *testing.T) {
	local, _ := defaultRetrievers()
	web := memoryRetriever{source: "web", err: errors.New("web down")}
	deps, _, _ := testDeps(local, web)
	_, _, err := runDAG("flow observer", deps)
	if err == nil {
		t.Fatal("want retrieve failure")
	}
	if !strings.Contains(err.Error(), "web down") {
		t.Fatalf("err=%v", err)
	}
}

func TestRetrieveLocalTimeoutOnDAG(t *testing.T) {
	local0, web0 := defaultRetrievers()
	local := memoryRetriever{source: "local", delay: time.Second, docs: local0.(memoryRetriever).docs}
	web := memoryRetriever{source: "web", delay: 5 * time.Millisecond, docs: web0.(memoryRetriever).docs}
	deps, _, _ := testDeps(local, web)
	deps.localAspects = []flow.Aspect{flow.Timeout(30 * time.Millisecond)}

	_, _, err := runDAG("flow Requires", deps)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("want retrieve_local timeout, got %v", err)
	}
}

// TestYAMLIsomorphicFactAndChitchat：代码建图与 YAML 装图同构——同一工厂、
// 同一拓扑、同一结果（E2 拓扑 A 的验收形态）。
func TestYAMLIsomorphicFactAndChitchat(t *testing.T) {
	local, web := defaultRetrievers()

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"fact", "flow Requires AND", "事实回答"},
		{"chitchat", "你好", "闲聊"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codeDeps, _, _ := testDeps(local, web)
			codeRes, _, err := runDAG(tc.in, codeDeps)
			if err != nil {
				t.Fatal(err)
			}

			yamlFinal := new(string)
			reg, strTag, docTag := prepareYAMLReg(t, local, web, yamlFinal)
			g, plan, err := flowyaml.Load([]byte(yamlDoc(strTag, docTag, tc.in)), reg, flowyaml.LoadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := plan.Apply(g, nil); err != nil {
				t.Fatal(err)
			}
			if err := g.Run(); err != nil {
				t.Fatal(err)
			}
			if *yamlFinal != codeRes.Final {
				t.Fatalf("Final mismatch\ncode: %q\nyaml: %q", codeRes.Final, *yamlFinal)
			}
			if !strings.Contains(codeRes.Final, tc.want) {
				t.Fatalf("final=%q want contain %q", codeRes.Final, tc.want)
			}
		})
	}
}

func prepareYAMLReg(t *testing.T, local, web Retriever, final *string) (*flow.Registry, string, string) {
	t.Helper()
	reg := flow.NewRegistry()
	registerDemoKeys(reg)
	strTag, ok := reg.TypeTagOf("demo04.user_text")
	if !ok {
		t.Fatal("missing user_text type tag")
	}
	docTag, ok := reg.TypeTagOf("demo04.local_docs")
	if !ok {
		t.Fatal("missing local_docs type tag")
	}
	registerDAGFactories(reg, local, web, final)
	return reg, strTag, docTag
}

// ---- flaky 重试 Aspect（原 04-flow-dag）----

type flakyRetriever struct {
	inner    Retriever
	fails    int
	attempts int
}

func (f *flakyRetriever) Search(ctx context.Context, query string, limit int) ([]Document, error) {
	f.attempts++
	if f.attempts <= f.fails {
		return nil, errors.New("transient web error")
	}
	return f.inner.Search(ctx, query, limit)
}

func TestRetryAspectRecoversOnDAG(t *testing.T) {
	local, web0 := defaultRetrievers()
	web := &flakyRetriever{inner: web0, fails: 1}
	deps, _, _ := testDeps(local, web)
	deps.webAspects = []flow.Aspect{flow.Retry(2, time.Millisecond)}

	_, _, err := runDAG("flow yaml", deps)
	if err != nil {
		t.Fatalf("retry should recover transient failure: %v", err)
	}
	if web.attempts != 2 {
		t.Fatalf("attempts=%d, want 2", web.attempts)
	}
}
