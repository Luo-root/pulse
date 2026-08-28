package main

import (
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/kernel/flow"
)

// registerDAGFactories 按拓扑归属 A 只登记 Run；边在 YAML / buildDAG 里。
func registerDAGFactories(reg *flow.Registry, local, web Retriever, final *string) {
	reg.MustRegister("demo04.classify", func(rc *flow.RunCtx) error {
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
	})
	reg.MustRegister("demo04.retrieve_local", func(rc *flow.RunCtx) error {
		q, err := flow.Get(rc, UserText)
		if err != nil {
			return err
		}
		docs, err := local.Search(rc.Context(), q, 4)
		if err != nil {
			return err
		}
		return flow.Set(rc, LocalDocs, docs)
	})
	reg.MustRegister("demo04.retrieve_web", func(rc *flow.RunCtx) error {
		q, err := flow.Get(rc, UserText)
		if err != nil {
			return err
		}
		docs, err := web.Search(rc.Context(), q, 4)
		if err != nil {
			return err
		}
		return flow.Set(rc, WebDocs, docs)
	})
	reg.MustRegister("demo04.merge", func(rc *flow.RunCtx) error {
		a, err := flow.Get(rc, LocalDocs)
		if err != nil {
			return err
		}
		b, err := flow.Get(rc, WebDocs)
		if err != nil {
			return err
		}
		return flow.Set(rc, MergedDocs, append(append([]Document{}, a...), b...))
	})
	reg.MustRegister("demo04.answer", func(rc *flow.RunCtx) error {
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
	})
	reg.MustRegister("demo04.smalltalk", func(rc *flow.RunCtx) error {
		q, err := flow.Get(rc, UserText)
		if err != nil {
			return err
		}
		if final != nil {
			*final = fmt.Sprintf("闲聊：收到「%s」。（未走检索，retrieve_* 应为 Skip）\n", q)
		}
		return nil
	})
}
