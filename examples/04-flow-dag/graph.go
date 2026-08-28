package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/observability"
)

// 槽位：classify 用 FactGate/ChatGate 做分支（Set 一边 Skip 一边）。
// Final 不进双 Provide：answer / smalltalk 经闭包写出，避免 Add 期冲突。
var (
	UserText   = flow.NewKey[string]("demo04.user_text")
	Intent     = flow.NewKey[string]("demo04.intent") // fact | chitchat
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

// buildDAG 构造并行双召回 + Skip 分支图。final 由 answer/smalltalk 闭包写入。
func buildDAG(deps dagDeps) (*flow.Graph, *string, error) {
	var final string
	bridge := &demoapp.Bridge{Sink: deps.sink, HostID: deps.host, TraceID: deps.trace}
	g := flow.New(context.Background(),
		flow.WithObserver(bridge.FlowObserver(deps.peak)),
	)

	if err := g.Add(flow.NewNode("classify",
		flow.Requires(UserText),
		flow.Deps(flow.Provides(Intent), flow.Provides(FactGate), flow.Provides(ChatGate)),
		func(rc *flow.RunCtx) error {
			text, err := flow.Get(rc, UserText)
			if err != nil {
				return err
			}
			intent := classifyIntent(text)
			if err := flow.Set(rc, Intent, intent); err != nil {
				return err
			}
			if intent == "fact" {
				if err := flow.Set(rc, FactGate, "go"); err != nil {
					return err
				}
				return flow.Skip(rc, ChatGate)
			}
			if err := flow.Set(rc, ChatGate, "go"); err != nil {
				return err
			}
			return flow.Skip(rc, FactGate)
		},
	)); err != nil {
		return nil, nil, err
	}

	if err := g.Add(flow.NewNode("retrieve_local",
		flow.Deps(flow.Requires(FactGate), flow.Requires(UserText)),
		flow.Provides(LocalDocs),
		func(rc *flow.RunCtx) error {
			q, err := flow.Get(rc, UserText)
			if err != nil {
				return err
			}
			docs, err := deps.local.Search(rc.Context(), q, 4)
			if err != nil {
				return err
			}
			return flow.Set(rc, LocalDocs, docs)
		},
	)); err != nil {
		return nil, nil, err
	}

	if err := g.Add(flow.NewNode("retrieve_web",
		flow.Deps(flow.Requires(FactGate), flow.Requires(UserText)),
		flow.Provides(WebDocs),
		func(rc *flow.RunCtx) error {
			q, err := flow.Get(rc, UserText)
			if err != nil {
				return err
			}
			docs, err := deps.web.Search(rc.Context(), q, 4)
			if err != nil {
				return err
			}
			return flow.Set(rc, WebDocs, docs)
		},
	)); err != nil {
		return nil, nil, err
	}

	if err := g.Add(flow.NewNode("merge",
		flow.Deps(flow.Requires(LocalDocs), flow.Requires(WebDocs)),
		flow.Provides(MergedDocs),
		func(rc *flow.RunCtx) error {
			local, err := flow.Get(rc, LocalDocs)
			if err != nil {
				return err
			}
			web, err := flow.Get(rc, WebDocs)
			if err != nil {
				return err
			}
			merged := append(append([]Document{}, local...), web...)
			return flow.Set(rc, MergedDocs, merged)
		},
	)); err != nil {
		return nil, nil, err
	}

	if err := g.Add(flow.NewNode("answer",
		flow.Deps(flow.Requires(FactGate), flow.Requires(UserText), flow.Requires(MergedDocs)),
		nil,
		func(rc *flow.RunCtx) error {
			q, err := flow.Get(rc, UserText)
			if err != nil {
				return err
			}
			docs, err := flow.Get(rc, MergedDocs)
			if err != nil {
				return err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "事实回答（query=%s）：\n", q)
			if len(docs) == 0 {
				b.WriteString("（两路均无命中，空合并列表，不是 Skip）\n")
			}
			for _, d := range docs {
				fmt.Fprintf(&b, "- [%s] %s: %s\n", d.Source, d.Title, d.Content)
			}
			final = b.String()
			return nil
		},
	)); err != nil {
		return nil, nil, err
	}

	if err := g.Add(flow.NewNode("smalltalk",
		flow.Deps(flow.Requires(ChatGate), flow.Requires(UserText)),
		nil,
		func(rc *flow.RunCtx) error {
			q, err := flow.Get(rc, UserText)
			if err != nil {
				return err
			}
			final = fmt.Sprintf("闲聊：收到「%s」。（未走检索，retrieve_* 应为 Skip）\n", q)
			return nil
		},
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
	intent := classifyIntent(text)
	return DAGResult{Intent: intent, Final: *finalPtr, Peak: deps.peak.Peak()}, time.Since(started), nil
}
