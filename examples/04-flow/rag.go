package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

// RAG 线性图：extract_text → retrieve → answer，三个节点经槽位依赖
// 串成链。answer 节点内部跑 loop.Agent——这是 flow 与 loop 的集成形态：
// 确定性的检索管道（图）包住自主决策的模型回合（agent）。
var (
	RagUserInput   = flow.NewKey[*llm.Message]("demo04.rag_user_input")
	RagQueryText   = flow.NewKey[string]("demo04.rag_query_text")
	RagContextDocs = flow.NewKey[[]Document]("demo04.rag_context_docs")
	RagFinalText   = flow.NewKey[string]("demo04.rag_final_text")
)

// runRAGGraph 构造并运行 RAG 线性图，返回 agent 结果与耗时。
func runRAGGraph(host *demoapp.Host, agent *loop.Agent, retriever Retriever, history []*llm.Message, user *llm.Message, bridge *demoapp.Bridge) (*loop.Result, time.Duration, error) {
	g := flow.New(context.Background(),
		flow.WithObserver(bridge.FlowObserver(host.Peak)),
	)
	if err := flow.Seed(g, RagUserInput, user); err != nil {
		return nil, 0, err
	}
	if err := g.Add(flow.NewNode("extract_text", flow.Requires(RagUserInput), flow.Provides(RagQueryText), func(rc *flow.RunCtx) error {
		msg, err := flow.Get(rc, RagUserInput)
		if err != nil {
			return err
		}
		return flow.Set(rc, RagQueryText, strings.TrimSpace(msg.Text()))
	})); err != nil {
		return nil, 0, err
	}
	if err := g.Add(flow.NewNode("retrieve", flow.Requires(RagQueryText), flow.Provides(RagContextDocs), func(rc *flow.RunCtx) error {
		query, err := flow.Get(rc, RagQueryText)
		if err != nil {
			return err
		}
		docs, err := retriever.Search(rc.Context(), query, 4)
		if err != nil {
			return err
		}
		return flow.Set(rc, RagContextDocs, docs)
	})); err != nil {
		return nil, 0, err
	}
	var result *loop.Result
	if err := g.Add(flow.NewNode("answer",
		flow.Deps(flow.Requires(RagUserInput), flow.Requires(RagQueryText), flow.Requires(RagContextDocs)),
		flow.Provides(RagFinalText),
		func(rc *flow.RunCtx) error {
			msg, err := flow.Get(rc, RagUserInput)
			if err != nil {
				return err
			}
			query, err := flow.Get(rc, RagQueryText)
			if err != nil {
				return err
			}
			docs, err := flow.Get(rc, RagContextDocs)
			if err != nil {
				return err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "检索查询：%s\n", query)
			b.WriteString("检索上下文：\n")
			if len(docs) == 0 {
				b.WriteString("（无命中，空文档列表，不是 Skip）\n")
			}
			for _, doc := range docs {
				fmt.Fprintf(&b, "- %s: %s\n", doc.Title, doc.Content)
			}
			prompt := llm.User(append([]llm.Part{llm.Text(b.String())}, msg.Parts...)...)
			res, err := agent.Run(rc.Context(), history, prompt)
			if err != nil {
				return err
			}
			result = res
			return flow.Set(rc, RagFinalText, res.Final.Text())
		},
	)); err != nil {
		return nil, 0, err
	}
	started := time.Now()
	if err := g.Run(); err != nil {
		return nil, time.Since(started), err
	}
	if result == nil {
		return nil, time.Since(started), fmt.Errorf("demo: answer node produced no result")
	}
	return result, time.Since(started), nil
}
