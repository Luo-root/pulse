// 04-flow：编排——RAG 线性链（agent 在图里）+ DAG 分支并行 + YAML 同构。
//
// 运行：go run ./examples/04-flow
// 每轮 REPL 输入跑两张图：① 线性 RAG 链（extract → retrieve → answer，
// answer 节点内嵌 loop.Agent）；② 分支并行 DAG（classify → 并行双检索 →
// merge → answer/smalltalk，Skip 是到达不是失败）。启动时先演示同一
// DAG 的代码建图与 YAML 装图同构（E2 拓扑 A：YAML 拥有边，Factory 只给 Run）。
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/kernel/flow"
	flowyaml "github.com/Luo-root/pulse/kernel/flow/yaml"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/observability"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "04-flow: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flags := demoapp.LoadFlagsFromEnv()
	host, err := demoapp.Open(flags, llm.Resp("根据检索到的文档：Pulse v2 用 kernel、llm、loop 和 flow 组成一次请求。"))
	if err != nil {
		return err
	}
	defer host.Close()

	local, web := defaultRetrievers()
	fmt.Printf("04-flow host=%s\n", host.HostID())

	// 启动演示：YAML 与代码建图同构——同一拓扑、同一工厂、同一结果。
	if err := demoYAMLParity(local, web); err != nil {
		return fmt.Errorf("yaml parity demo: %w", err)
	}
	fmt.Println()
	fmt.Println("REPL：每轮输入跑两张图。事实类输入（如 flow Requires）走 DAG 事实分支；闲聊（如 你好）触发 Skip。")

	var history []*llm.Message
	return demoapp.Loop(os.Stdin, os.Stdout, func(msg *llm.Message) ([]*llm.Message, error) {
		// 每轮独立 reqScope + Bridge + Agent（与 02/03 同模式）。
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
			loop.WithSystemPrompt("用检索上下文和对话历史回答。没有文档时也要基于用户输入作答。"),
			loop.WithEventScope(reqScope),
		)
		if err != nil {
			return nil, err
		}

		// 图 1：RAG 线性链——检索管道确定性执行，answer 节点把自主权交给 agent。
		res, ragDur, err := runRAGGraph(host, agent, local, history, msg, bridge)
		if err != nil {
			return nil, err
		}
		if res.Final != nil {
			fmt.Println(res.Final.Text())
		}

		// 图 2：DAG 分支并行图——纯数据流编排（classify 用 Skip 裁剪分支）。
		dagRes, dagDur, err := runDAG(msg.Text(), dagDeps{
			local: local, web: web,
			sink: host.Sink, peak: &demoapp.FlowPeak{},
			host: host.HostID(), trace: host.NewTraceID(),
		})
		if err != nil {
			return nil, err
		}
		fmt.Print(dagRes.Final)

		fmt.Fprintf(os.Stderr, "summary rag_ms=%d dag_ms=%d dag_peak=%d intent=%s history=%d\n",
			ragDur.Milliseconds(), dagDur.Milliseconds(), dagRes.Peak, dagRes.Intent, len(history)+1)
		bridge.Write("flow.summary", fmt.Sprintf(
			"rag_duration_ms=%d dag_duration_ms=%d dag_alive_nodes_peak=%d",
			ragDur.Milliseconds(), dagDur.Milliseconds(), dagRes.Peak))

		history = append(history, msg)
		history = append(history, res.Messages...)
		return res.Messages, nil
	}, func() int { return len(history) })
}

// demoYAMLParity 用固定输入验证「代码建图 ≡ YAML 装图」：同一套工厂
//（registerDAGFactories）喂给两种拓扑来源，Final 必须一致。
// YAML 文本里的 type 标签来自 flow.Registry.TypeTagOf——类型标注是
// 注册表的运行时事实，不是手写常量。
func demoYAMLParity(local, web Retriever) error {
	// 代码建图基线（演示图不关心桥记录，观测进局部 Sink 即可）。
	codeDeps := dagDeps{local: local, web: web, sink: &observability.MemorySink{}, peak: &demoapp.FlowPeak{}, host: "yaml-demo", trace: "yaml-demo-1"}
	codeRes, codeDur, err := runDAG("flow Requires AND", codeDeps)
	if err != nil {
		return err
	}

	// YAML 装图：注册 Key 与工厂 → 取类型标签 → 拼拓扑 → Load/Apply/Run。
	final := new(string)
	reg := flow.NewRegistry()
	registerDemoKeys(reg)
	strTag, _ := reg.TypeTagOf("demo04.user_text")
	docTag, _ := reg.TypeTagOf("demo04.local_docs")
	registerDAGFactories(reg, local, web, final)
	yamlText := yamlDoc(strTag, docTag, "flow Requires AND")
	g, plan, err := flowyaml.Load([]byte(yamlText), reg, flowyaml.LoadOptions{})
	if err != nil {
		return err
	}
	if err := plan.Apply(g, nil); err != nil {
		return err
	}
	started := time.Now()
	if err := g.Run(); err != nil {
		return err
	}

	fmt.Println("=== YAML 同构演示（输入：flow Requires AND）===")
	fmt.Printf("代码建图 Final：\n%s", codeRes.Final)
	fmt.Printf("YAML 装图 Final：\n%s", *final)
	if *final != codeRes.Final {
		return fmt.Errorf("yaml parity broken:\ncode=%q\nyaml=%q", codeRes.Final, *final)
	}
	fmt.Printf("两图结果一致 ✓（code_ms=%d yaml_ms=%d）\n",
		codeDur.Milliseconds(), time.Since(started).Milliseconds())
	return nil
}

// registerDemoKeys 把 DAG 用的全部槽位 Key 注册进 flow.Registry
//（YAML 的 key 引用与类型标签都以注册表为准）。
func registerDemoKeys(reg *flow.Registry) {
	flow.MustRegisterKey(reg, UserText)
	flow.MustRegisterKey(reg, FactGate)
	flow.MustRegisterKey(reg, ChatGate)
	flow.MustRegisterKey(reg, LocalDocs)
	flow.MustRegisterKey(reg, WebDocs)
	flow.MustRegisterKey(reg, MergedDocs)
}

// yamlDoc 生成与 buildDAG 同构的 YAML 拓扑（节点/边完全一致；Factory
// 名与注册名一致）。strTag/docTag 是注册表的运行时类型标签。
func yamlDoc(strTag, docTag string, seed string) string {
	return fmt.Sprintf(`
version: 1
seeds:
  - key: { name: demo04.user_text, type: %q }
    from: { kind: literal, value: %q }
nodes:
  - id: classify
    uses: demo04.classify
    requires: [{ name: demo04.user_text, type: %q }]
    provides:
      - { name: demo04.fact_gate, type: %q }
      - { name: demo04.chat_gate, type: %q }
  - id: retrieve_local
    uses: demo04.retrieve_local
    requires:
      - { name: demo04.fact_gate, type: %q }
      - { name: demo04.user_text, type: %q }
    provides: [{ name: demo04.local_docs, type: %q }]
  - id: retrieve_web
    uses: demo04.retrieve_web
    requires:
      - { name: demo04.fact_gate, type: %q }
      - { name: demo04.user_text, type: %q }
    provides: [{ name: demo04.web_docs, type: %q }]
  - id: merge
    uses: demo04.merge
    requires:
      - { name: demo04.local_docs, type: %q }
      - { name: demo04.web_docs, type: %q }
    provides: [{ name: demo04.merged_docs, type: %q }]
  - id: answer
    uses: demo04.answer
    requires:
      - { name: demo04.fact_gate, type: %q }
      - { name: demo04.user_text, type: %q }
      - { name: demo04.merged_docs, type: %q }
    provides: []
  - id: smalltalk
    uses: demo04.smalltalk
    requires:
      - { name: demo04.chat_gate, type: %q }
      - { name: demo04.user_text, type: %q }
    provides: []
`, strTag, seed, strTag, strTag, strTag, strTag, strTag, docTag, strTag, strTag, docTag, docTag, docTag, docTag, strTag, strTag, docTag, strTag, strTag)
}
