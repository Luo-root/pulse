package experiment

// Adapted from pulse-durable runtime_test.go at b2e69374f8653c3cb2eb073d5d293ad05d043544.
// Only public APIs are used; no production implementation is copied.
import (
	"github.com/Luo-root/pulse"
	durable "github.com/Luo-root/pulse-durable"
	"testing"
)

func testGraph(t *testing.T, id string, options ...pulse.Option) *pulse.Graph {
	t.Helper()
	g, err := pulse.New(id, "", options...)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func testAdd(t *testing.T, g *pulse.Graph, n *pulse.Node) {
	t.Helper()
	if err := g.Add(n); err != nil {
		t.Fatal(err)
	}
}
func testRuntime(t *testing.T, store durable.Store, codecs *durable.Codecs) *durable.Runtime {
	t.Helper()
	r, err := durable.New(store, codecs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func testOptions(id string) durable.RunOptions {
	return durable.RunOptions{RunID: id, Version: "business/1"}
}
