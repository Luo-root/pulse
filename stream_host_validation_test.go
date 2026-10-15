package pulse

import (
	"strings"
	"testing"
)

func TestStreamHostReadersFollowActualAliases(t *testing.T) {
	for _, c := range []struct {
		name          string
		hosts         map[string]bool
		graphReader   bool
		sourceSkipped bool
		want          []string
	}{
		{name: "unconsumed", want: []string{"nothing consumes it"}},
		{name: "one-host", hosts: map[string]bool{"left": true}},
		{name: "ignored-false-registration", hosts: map[string]bool{"left": true, "right": false}},
		{name: "two-outlet-aliases", hosts: map[string]bool{"right": true, "left": true}, want: []string{"2 readers", "host output \"left\"", "host output \"right\"", "split between them"}},
		{name: "host-and-node", hosts: map[string]bool{"left": true}, graphReader: true, want: []string{"2 readers", "host output \"left\"", "node \"reader\"", "split between them"}},
		{name: "two-aliases-of-skipped-source", hosts: map[string]bool{"left": true, "right": true}, sourceSkipped: true, want: []string{"2 readers", "host output \"left\"", "host output \"right\""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
			inside := NewKey[Stream[int]]("inside")
			left, right := NewKey[Stream[int]]("left"), NewKey[Stream[int]]("right")
			streamTestMust(t, child.DeclareOutput(NewPort(inside)))
			binds := []SubBind{Out(inside, left), Out(inside, right)}
			var requires []keyRef
			if c.sourceSkipped {
				trigger := NewKey[bool]("trigger")
				streamTestMust(t, child.DeclareInput(NewPort(trigger)))
				requires = Requires(trigger)
				binds = append(binds, FixSkip(trigger))
			}
			streamTestMust(t, child.Add(NewNode("produce", "", requires, Provides(inside), func(*RunCtx) error { return nil })))
			streamTestMust(t, Sub(g, "child", "", binds, child))
			streamTestMust(t, g.DeclareOutput(NewPort(left), NewPort(right)))
			if c.graphReader {
				streamTestMust(t, g.Add(NewNode("reader", "", Requires(right), nil, func(*RunCtx) error { return nil })))
			}
			g.mu.Lock()
			err := g.validateRunLocked(0, streamKnownInputs{hostReads: c.hosts})
			g.mu.Unlock()
			if len(c.want) == 0 {
				if err != nil {
					t.Fatalf("one actual host reader was rejected: %v", err)
				}
				// 定义接受稍后订阅的可能；不能把它当成本次已登记的真实读者。
				if err := g.Validate(); err != nil {
					t.Fatalf("declared managed outlet could not be subscribed later: %v", err)
				}
				g.mu.Lock()
				withoutHost := g.validateRunLocked(0, streamKnownInputs{})
				g.mu.Unlock()
				if withoutHost == nil || !strings.Contains(withoutHost.Error(), "nothing consumes it") {
					t.Fatalf("definition outlet silently registered a host: %v", withoutHost)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid reader group was accepted")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("diagnostic %q omitted %q", err, want)
				}
			}
		})
	}
}

func TestStreamHostFactsDoNotSubscribeNativeChannel(t *testing.T) {
	g := streamTestGraph(t, "native")
	out := NewKey[<-chan int]("out")
	streamTestMust(t, g.DeclareOutput(NewPort(out)))
	streamTestProduce(t, g, "produce", out, 1, 0)
	g.mu.Lock()
	err := g.validateRunLocked(0, streamKnownInputs{hostReads: map[string]bool{"out": true}})
	g.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
		t.Fatalf("native channel acquired an unsupported host reader: %v", err)
	}
}
