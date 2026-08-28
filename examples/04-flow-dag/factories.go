package main

import (
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/kernel/flow"
)

// dagRuns 是代码建图与 YAML 装图共用的 Run 集合（E2 拓扑 A：工厂只给 Run）。
type dagRuns struct {
	classify       flow.RunFunc
	retrieveLocal flow.RunFunc
	retrieveWeb   flow.RunFunc
	merge         flow.RunFunc
	answer        flow.RunFunc
	smalltalk     flow.RunFunc
}

// newDAGRuns 构造与 buildDAG / Registry 共用的节点执行体。
// final 由 answer / smalltalk 写入：两叶子不能双 Provide 同一 Key（契约约束），
// 故 Final 不进槽位，见 04 README「闭包写 Final」。
func newDAGRuns(local, web Retriever, final *string) dagRuns {
	return dagRuns{
		classify: func(rc *flow.RunCtx) error {
			text, err := flow.Get(rc, UserText)
			if err != nil {
				return err
			}
			if classifyIntent(text) == "fact" {
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
		retrieveLocal: func(rc *flow.RunCtx) error {
			q, err := flow.Get(rc, UserText)
			if err != nil {
				return err
			}
			docs, err := local.Search(rc.Context(), q, 4)
			if err != nil {
				return err
			}
			return flow.Set(rc, LocalDocs, docs)
		},
		retrieveWeb: func(rc *flow.RunCtx) error {
			q, err := flow.Get(rc, UserText)
			if err != nil {
				return err
			}
			docs, err := web.Search(rc.Context(), q, 4)
			if err != nil {
				return err
			}
			return flow.Set(rc, WebDocs, docs)
		},
		merge: func(rc *flow.RunCtx) error {
			a, err := flow.Get(rc, LocalDocs)
			if err != nil {
				return err
			}
			b, err := flow.Get(rc, WebDocs)
			if err != nil {
				return err
			}
			return flow.Set(rc, MergedDocs, append(append([]Document{}, a...), b...))
		},
		answer: func(rc *flow.RunCtx) error {
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
			if final != nil {
				*final = b.String()
			}
			return nil
		},
		smalltalk: func(rc *flow.RunCtx) error {
			q, err := flow.Get(rc, UserText)
			if err != nil {
				return err
			}
			if final != nil {
				*final = fmt.Sprintf("闲聊：收到「%s」。（未走检索，retrieve_* 应为 Skip）\n", q)
			}
			return nil
		},
	}
}

// registerDAGFactories 把共用 Run 挂到 Registry（YAML uses 名）。
func registerDAGFactories(reg *flow.Registry, local, web Retriever, final *string) {
	runs := newDAGRuns(local, web, final)
	reg.MustRegister("demo04.classify", runs.classify)
	reg.MustRegister("demo04.retrieve_local", runs.retrieveLocal)
	reg.MustRegister("demo04.retrieve_web", runs.retrieveWeb)
	reg.MustRegister("demo04.merge", runs.merge)
	reg.MustRegister("demo04.answer", runs.answer)
	reg.MustRegister("demo04.smalltalk", runs.smalltalk)
}
