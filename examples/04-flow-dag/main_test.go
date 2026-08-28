package main

import (
	"context"
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

func prepareYAMLReg(t *testing.T, local, web Retriever, final *string) (*flow.Registry, string, string) {
	t.Helper()
	reg := flow.NewRegistry()
	flow.MustRegisterKey(reg, UserText)
	flow.MustRegisterKey(reg, FactGate)
	flow.MustRegisterKey(reg, ChatGate)
	flow.MustRegisterKey(reg, LocalDocs)
	flow.MustRegisterKey(reg, WebDocs)
	flow.MustRegisterKey(reg, MergedDocs)
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

type flakyRetriever struct {
	inner   Retriever
	fails   int
	attempts int
}

func (f *flakyRetriever) Search(ctx context.Context, query string, limit int) ([]Document, error) {
	f.attempts++
	if f.attempts <= f.fails {
		return nil, errors.New("transient web error")
	}
	return f.inner.Search(ctx, query, limit)
}

func TestRetrieveWebRetryOnDAG(t *testing.T) {
	local, web0 := defaultRetrievers()
	flaky := &flakyRetriever{inner: web0, fails: 2}
	deps, sink, _ := testDeps(local, flaky)
	deps.webAspects = []flow.Aspect{flow.Retry(5, time.Millisecond)}

	res, _, err := runDAG("flow Requires", deps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Final, "事实回答") {
		t.Fatalf("final=%q", res.Final)
	}
	if flaky.attempts != 3 {
		t.Fatalf("attempts=%d, want 3 (2 fail + 1 ok)", flaky.attempts)
	}
	// E1：Retry 不重复打 Waiting/Running
	waits, runs := 0, 0
	for _, rec := range sink.Snapshot() {
		if rec.FiberName != "retrieve_web" {
			continue
		}
		switch rec.Event {
		case demoapp.EventFlowNodeWaitFinished:
			waits++
		case demoapp.EventFlowNodeRunFinished:
			runs++
		}
	}
	if waits != 1 || runs != 1 {
		t.Fatalf("retrieve_web waits=%d runs=%d (Retry must not double-fire)", waits, runs)
	}
}
