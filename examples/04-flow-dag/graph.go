package main

import (
	"context"
	"strings"
	"time"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/observability"
)

// 槽位：classify 用 FactGate/ChatGate 做分支（Set 一边 Skip 一边）。
// 不设 Intent Key：意图只用于分支门闩与摘要，门闩已编码分支，避免死槽。
// Final 不进双 Provide：answer / smalltalk 经共用闭包写出（见 README「闭包写 Final」）。
var (
	UserText   = flow.NewKey[string]("demo04.user_text")
	FactGate   = flow.NewKey[string]("demo04.fact_gate")
	ChatGate   = flow.NewKey[string]("demo04.chat_gate")
	LocalDocs  = flow.NewKey[[]Document]("demo04.local_docs")
	WebDocs    = flow.NewKey[[]Document]("demo04.web_docs")
	MergedDocs = flow.NewKey[[]Document]("demo04.merged_docs")
)

// Document 是内存假检索命中。
type Document struct {
	Source  string
	Title   string
	Content string
}

// Retriever 可注入延迟/错误，便于并行与失败测试。
type Retriever interface {
	Search(ctx context.Context, query string, limit int) ([]Document, error)
}

type memoryRetriever struct {
	source string
	docs   []Document
	delay  time.Duration
	err    error
}

func (r memoryRetriever) Search(ctx context.Context, query string, limit int) ([]Document, error) {
	if r.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.delay):
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}
	var out []Document
	for _, doc := range r.docs {
		hay := strings.ToLower(doc.Title + " " + doc.Content)
		if strings.Contains(hay, q) {
			d := doc
			if d.Source == "" {
				d.Source = r.source
			}
			out = append(out, d)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func classifyIntent(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "你好"), strings.Contains(t, "嗨"), strings.Contains(t, "hello"),
		strings.Contains(t, "晚饭"), strings.Contains(t, "天气怎么样"):
		return "chitchat"
	default:
		return "fact"
	}
}

// DAGResult 是一次图运行的对外结果。
type DAGResult struct {
	Intent string
	Final  string
	Peak   int32
}

type dagDeps struct {
	local Retriever
	web   Retriever
	sink  observability.Sink
	peak  *demoapp.FlowPeak
	host  string
	trace string
}

// buildDAG 构造并行双召回 + Skip 分支图；Run 与 YAML 工厂同源（newDAGRuns）。
func buildDAG(deps dagDeps) (*flow.Graph, *string, error) {
	var final string
	runs := newDAGRuns(deps.local, deps.web, &final)
	bridge := &demoapp.Bridge{Sink: deps.sink, HostID: deps.host, TraceID: deps.trace}
	g := flow.New(context.Background(),
		flow.WithObserver(bridge.FlowObserver(deps.peak)),
	)

	if err := g.Add(flow.NewNode("classify",
		flow.Requires(UserText),
		flow.Deps(flow.Provides(FactGate), flow.Provides(ChatGate)),
		runs.classify,
	)); err != nil {
		return nil, nil, err
	}
	if err := g.Add(flow.NewNode("retrieve_local",
		flow.Deps(flow.Requires(FactGate), flow.Requires(UserText)),
		flow.Provides(LocalDocs),
		runs.retrieveLocal,
	)); err != nil {
		return nil, nil, err
	}
	if err := g.Add(flow.NewNode("retrieve_web",
		flow.Deps(flow.Requires(FactGate), flow.Requires(UserText)),
		flow.Provides(WebDocs),
		runs.retrieveWeb,
	)); err != nil {
		return nil, nil, err
	}
	if err := g.Add(flow.NewNode("merge",
		flow.Deps(flow.Requires(LocalDocs), flow.Requires(WebDocs)),
		flow.Provides(MergedDocs),
		runs.merge,
	)); err != nil {
		return nil, nil, err
	}
	if err := g.Add(flow.NewNode("answer",
		flow.Deps(flow.Requires(FactGate), flow.Requires(UserText), flow.Requires(MergedDocs)),
		nil,
		runs.answer,
	)); err != nil {
		return nil, nil, err
	}
	if err := g.Add(flow.NewNode("smalltalk",
		flow.Deps(flow.Requires(ChatGate), flow.Requires(UserText)),
		nil,
		runs.smalltalk,
	)); err != nil {
		return nil, nil, err
	}
	return g, &final, nil
}

func runDAG(text string, deps dagDeps) (DAGResult, time.Duration, error) {
	g, finalPtr, err := buildDAG(deps)
	if err != nil {
		return DAGResult{}, 0, err
	}
	if err := flow.Seed(g, UserText, text); err != nil {
		return DAGResult{}, 0, err
	}
	started := time.Now()
	if err := g.Run(); err != nil {
		return DAGResult{}, time.Since(started), err
	}
	return DAGResult{
		Intent: classifyIntent(text),
		Final:  *finalPtr,
		Peak:   deps.peak.Peak(),
	}, time.Since(started), nil
}
