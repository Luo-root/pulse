package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

var (
	UserInput   = flow.NewKey[*llm.Message]("demo.user_input")
	QueryText   = flow.NewKey[string]("demo.query_text")
	ContextDocs = flow.NewKey[[]Document]("demo.context_docs")
	FinalText   = flow.NewKey[string]("demo.final_text")
)

type Document struct {
	ID      string
	Title   string
	Content string
}

type Retriever interface {
	Search(ctx context.Context, query string, limit int) ([]Document, error)
}

type memoryRetriever struct{ docs []Document }

// Search 只做关键词匹配：不命中就返回空切片（数据），绝不用兜底规则
// 掩盖空命中路径——「无文档」本身就是要演示的合法结果。
func (r memoryRetriever) Search(_ context.Context, query string, limit int) ([]Document, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}
	var out []Document
	for _, doc := range r.docs {
		hay := strings.ToLower(doc.Title + " " + doc.Content)
		if strings.Contains(hay, q) {
			out = append(out, doc)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "03-flow-agent: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flags := demoapp.LoadFlagsFromEnv()
	scripted := []*llm.Response{
		llm.Resp("根据检索到的文档：Pulse v2 用 kernel、llm、loop 和 flow 组成一次请求。"),
	}
	host, err := demoapp.Open(flags, scripted...)
	if err != nil {
		return err
	}
	defer host.Close()

	retriever := memoryRetriever{docs: []Document{
		{ID: "k1", Title: "kernel", Content: "卸载即还原，依赖响应式装载。"},
		{ID: "f1", Title: "flow", Content: "一次运行一个世界，Requires 是 AND 前置，Skip 是到达。"},
	}}
	// lookup 只是 loop 层的演示工具（模型自愿调用），不是图的检索入口；
	// flow 的知识通路是 retrieve 节点 → ContextDocs。
	tools := loop.NewMemToolSet()
	if err := tools.Register(llm.ToolDef{
		Name:        "lookup",
		Description: "查找本地知识",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}}}`),
	}, func(_ context.Context, args json.RawMessage) (string, error) {
		return `{"ok":true}`, nil
	}); err != nil {
		return err
	}
	var history []*llm.Message
	fmt.Printf("03-flow-agent provider=%s model=%s scripted=%v host=%s\n",
		flags.Provider, flags.Model, flags.Scripted, host.HostID())
	return demoapp.Loop(os.Stdin, os.Stdout, func(msg *llm.Message) ([]*llm.Message, error) {
		// 每次请求：独立 reqScope + Bridge + Agent。
		// EmitLocal 后，只有本轮 Bridge 能听到 tool/turn/llm 事件。
		reqScope, err := host.Ctx.Derive()
		if err != nil {
			return nil, err
		}
		defer reqScope.Dispose()
		bridge, err := host.NewBridge(reqScope)
		if err != nil {
			return nil, err
		}
		agent, err := loop.NewAgent(host.Model,
			loop.WithToolSet(tools),
			loop.WithSystemPrompt("用检索上下文和对话历史回答。没有文档时也要基于用户输入作答。"),
			loop.WithEventScope(reqScope),
		)
		if err != nil {
			return nil, err
		}
		res, dur, err := runGraph(host, agent, retriever, history, msg, bridge)
		if err != nil {
			return nil, err
		}
		if res.Final != nil {
			fmt.Println(res.Final.Text())
		}
		history = append(history, msg)
		history = append(history, res.Messages...)
		bridge.Write("flow.summary", fmt.Sprintf(
			"duration_ms=%d alive_nodes_peak=%d history=%d",
			dur.Milliseconds(), host.Peak.Peak(), len(history)))
		return res.Messages, nil
	}, func() int { return len(history) })
}

func runGraph(host *demoapp.Host, agent *loop.Agent, retriever Retriever, history []*llm.Message, user *llm.Message, bridge *demoapp.Bridge) (*loop.Result, time.Duration, error) {
	// 不设 WithMaxRunning：本图是线性链，没有可并行执行的窗口；上限
	// 的真实效果（拿齐输入才占名额、等数据不占）留给扩图者验证。
	g := flow.New(context.Background(),
		flow.WithObserver(bridge.FlowObserver(host.Peak)),
	)
	if err := flow.Seed(g, UserInput, user); err != nil {
		return nil, 0, err
	}
	if err := g.Add(flow.NewNode("extract_text", flow.Requires(UserInput), flow.Provides(QueryText), func(rc *flow.RunCtx) error {
		msg, err := flow.Get(rc, UserInput)
		if err != nil {
			return err
		}
		return flow.Set(rc, QueryText, strings.TrimSpace(msg.Text()))
	})); err != nil {
		return nil, 0, err
	}
	if err := g.Add(flow.NewNode("retrieve", flow.Requires(QueryText), flow.Provides(ContextDocs), func(rc *flow.RunCtx) error {
		query, err := flow.Get(rc, QueryText)
		if err != nil {
			return err
		}
		docs, err := retriever.Search(rc.Context(), query, 4)
		if err != nil {
			return err
		}
		return flow.Set(rc, ContextDocs, docs)
	})); err != nil {
		return nil, 0, err
	}
	var result *loop.Result
	if err := g.Add(flow.NewNode("answer",
		flow.Deps(flow.Requires(UserInput), flow.Requires(QueryText), flow.Requires(ContextDocs)),
		flow.Provides(FinalText),
		func(rc *flow.RunCtx) error {
			msg, err := flow.Get(rc, UserInput)
			if err != nil {
				return err
			}
			query, err := flow.Get(rc, QueryText)
			if err != nil {
				return err
			}
			docs, err := flow.Get(rc, ContextDocs)
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
			return flow.Set(rc, FinalText, res.Final.Text())
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
