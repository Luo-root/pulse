package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/kernel/flow"
	flowyaml "github.com/Luo-root/pulse/kernel/flow/yaml"
	"github.com/Luo-root/pulse/observability"
)

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
	for _, rec := range sink.Snapshot() {
		if rec.Event != demoapp.EventFlowNodeRunFinished {
			continue
		}
		switch rec.FiberName {
		case "retrieve_local", "retrieve_web", "merge", "answer":
			t.Fatalf("chitchat must not run %s", rec.FiberName)
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

func TestYAMLIsomorphicFactPath(t *testing.T) {
	local, web := defaultRetrievers()
	reg := flow.NewRegistry()
	flow.MustRegisterKey(reg, UserText)
	flow.MustRegisterKey(reg, Intent)
	flow.MustRegisterKey(reg, FactGate)
	flow.MustRegisterKey(reg, ChatGate)
	flow.MustRegisterKey(reg, LocalDocs)
	flow.MustRegisterKey(reg, WebDocs)
	flow.MustRegisterKey(reg, MergedDocs)

	docTag, ok := reg.TypeTagOf("demo04.local_docs")
	if !ok {
		t.Fatal("missing local_docs type tag")
	}
	strTag, _ := reg.TypeTagOf("demo04.user_text")

	final := new(string)
	registerDAGFactories(reg, local, web, final)

	doc := fmt.Sprintf(`
version: 1
seeds:
  - key: { name: demo04.user_text, type: %q }
    from: { kind: literal, value: "flow Requires" }
nodes:
  - id: classify
    uses: demo04.classify
    requires: [{ name: demo04.user_text, type: %q }]
    provides:
      - { name: demo04.intent, type: %q }
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
`, strTag, strTag, strTag, strTag, strTag, strTag, strTag, docTag, strTag, strTag, docTag, docTag, docTag, docTag, strTag, strTag, docTag, strTag, strTag)

	g, plan, err := flowyaml.Load([]byte(doc), reg, flowyaml.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*final, "事实回答") {
		t.Fatalf("yaml final=%q", *final)
	}
}

func TestYAMLTimeoutOnSlowNode(t *testing.T) {
	reg := flow.NewRegistry()
	in := flow.NewKey[string]("t.in")
	out := flow.NewKey[string]("t.out")
	flow.MustRegisterKey(reg, in)
	flow.MustRegisterKey(reg, out)
	reg.MustRegister("slow", func(rc *flow.RunCtx) error {
		select {
		case <-rc.Context().Done():
			return rc.Context().Err()
		case <-time.After(time.Second):
			return flow.Set(rc, out, "late")
		}
	})
	inTag, _ := reg.TypeTagOf("t.in")
	outTag, _ := reg.TypeTagOf("t.out")
	doc := fmt.Sprintf(`
seeds:
  - key: { name: t.in, type: %q }
    from: { kind: literal, value: "x" }
nodes:
  - id: slow
    uses: slow
    requires: [{ name: t.in, type: %q }]
    provides: [{ name: t.out, type: %q }]
    timeout: 30ms
`, inTag, inTag, outTag)
	g, plan, err := flowyaml.Load([]byte(doc), reg, flowyaml.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	err = g.Run()
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("want timeout, got %v", err)
	}
}
