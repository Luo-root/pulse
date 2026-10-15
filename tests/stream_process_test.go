package pulse_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/Luo-root/pulse"
)

func messagePublicOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func messagePublicContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestMessagePublicRetryPipelineOneQuotaAndOneNodeLifecycle(t *testing.T) {
	var mu sync.Mutex
	events := make(map[string][3]int)
	mark := func(node string, index int) {
		mu.Lock()
		counts := events[node]
		counts[index]++
		events[node] = counts
		mu.Unlock()
	}
	observer := p.ObserverFunc{
		Waiting:  func(_ p.Site, node string) { mark(node, 0) },
		Running:  func(_ p.Site, node string) { mark(node, 1) },
		Finished: func(_ p.Site, node string, _ p.NodeFinishReason, _ error) { mark(node, 2) },
	}
	g, err := p.New("message-pipeline", "", p.WithMaxRunning(1), p.WithObserver(observer))
	messagePublicOK(t, err)
	input, output := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("output")
	messagePublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(input), func(rc *p.RunCtx) error {
		writer, err := p.OpenStream(rc, input)
		if err != nil {
			return err
		}
		for _, value := range []int{1, 2, 3} {
			if err := writer.Send(value); err != nil {
				return err
			}
		}
		return nil
	})))
	var callbacks, got []int
	failed := false
	messagePublicOK(t, g.Add(p.NewNode("map", "", p.Requires(input), p.Provides(output), func(rc *p.RunCtx) error {
		stream, err := p.Get(rc, input)
		if err != nil {
			return err
		}
		writer, err := p.OpenStream(rc, output)
		if err != nil {
			return err
		}
		for {
			delivery, err := stream.Receive(rc)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if err := p.HandleMessage(rc, delivery, func(_ *p.RunCtx, value int) error {
				callbacks = append(callbacks, value)
				if value == 2 && !failed {
					failed = true
					if err := writer.Send(-2); err != nil {
						return err
					}
					return errors.New("retry only message two")
				}
				return writer.Send(value * 10)
			}, p.WithMessageRetry(2, time.Millisecond)); err != nil {
				return err
			}
		}
	}, p.Timeout(3*time.Second))))
	messagePublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(output), nil, func(rc *p.RunCtx) error {
		stream, err := p.Get(rc, output)
		if err != nil {
			return err
		}
		for {
			delivery, err := stream.Receive(rc)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			got = append(got, delivery.Value)
			if err := delivery.Ack(); err != nil {
				return err
			}
		}
	})))
	messagePublicOK(t, g.Run(messagePublicContext(t), p.Inputs{}))
	if want := []int{1, 2, 2, 3}; !reflect.DeepEqual(callbacks, want) {
		t.Fatalf("callbacks = %v, want %v", callbacks, want)
	}
	if want := []int{10, 20, 30}; !reflect.DeepEqual(got, want) {
		t.Fatalf("published outputs = %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, node := range []string{"source", "map", "sink"} {
		if events[node] != [3]int{1, 1, 1} {
			t.Fatalf("%s lifecycle = %v; message retries must not create extra node events", node, events[node])
		}
	}
}

func TestMessagePublicExhaustionCannotBecomeSuccessfulExecution(t *testing.T) {
	g, err := p.New("message-exhaustion", "", p.WithMaxRunning(1))
	messagePublicOK(t, err)
	input := p.NewKey[p.Stream[int]]("input")
	var endpoint p.Stream[int]
	messagePublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(input), func(rc *p.RunCtx) error {
		writer, err := p.OpenStream(rc, input)
		if err != nil {
			return err
		}
		return writer.Send(2)
	})))
	cause := errors.New("message exhausted")
	attempts := 0
	messagePublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(input), nil, func(rc *p.RunCtx) error {
		var err error
		endpoint, err = p.Get(rc, input)
		if err != nil {
			return err
		}
		delivery, err := endpoint.Receive(rc)
		if err != nil {
			return err
		}
		_ = p.HandleMessage(rc, delivery, func(*p.RunCtx, int) error {
			attempts++
			return cause
		}, p.WithMessageRetry(2, 0))
		return nil // 完整链的失败锁存必须挡住这个吞错。
	})))
	if err := g.Run(messagePublicContext(t), p.Inputs{}); !errors.Is(err, cause) {
		t.Fatalf("swallowed message failure became execution success: %v", err)
	}
	if got := endpoint.Snapshot(); attempts != 2 || got.AckedThrough != 0 || got.InFlight != 1 {
		t.Fatalf("exhaustion changed confirmation: attempts=%d input=%+v", attempts, got)
	}
}

func TestMessagePublicProgressiveSendVisibleBeforeSourceReturns(t *testing.T) {
	g, err := p.New("progressive-message-boundary", "", p.WithMaxRunning(1))
	messagePublicOK(t, err)
	key := p.NewKey[p.Stream[int]]("tokens")
	firstSeen, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	messagePublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
		defer close(returned)
		writer, err := p.OpenStream(rc, key)
		if err != nil {
			return err
		}
		if err := writer.Send(1); err != nil {
			return err
		}
		return writer.Send(2) // 等第一条确认，不能把整段来源隐式攒到末尾。
	})))
	messagePublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(key), nil, func(rc *p.RunCtx) error {
		stream, err := p.Get(rc, key)
		if err != nil {
			return err
		}
		first, err := stream.Receive(rc)
		if err != nil || first.Value != 1 {
			return fmt.Errorf("first progressive message: value=%d err=%v", first.Value, err)
		}
		close(firstSeen)
		if err := rc.WaitStream(func(ctx context.Context) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}); err != nil {
			return err
		}
		if err := first.Ack(); err != nil {
			return err
		}
		second, err := stream.Receive(rc)
		if err != nil || second.Value != 2 {
			return fmt.Errorf("second progressive message: value=%d err=%v", second.Value, err)
		}
		if err := second.Ack(); err != nil {
			return err
		}
		_, err = stream.Receive(rc)
		if err == io.EOF {
			return nil
		}
		return err
	})))
	ctx := messagePublicContext(t)
	e, err := g.NewExecution(ctx, p.Inputs{})
	messagePublicOK(t, err)
	defer e.Cancel()
	messagePublicOK(t, e.Start())
	select {
	case <-firstSeen:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-returned:
		t.Fatal("first message was delayed until progressive source completion")
	default:
	}
	close(release)
	messagePublicOK(t, e.Wait())
}

func TestMessagePublicWholeNodeRetryStillRejectsStreamIO(t *testing.T) {
	for _, operation := range []string{"open", "receive"} {
		t.Run(operation, func(t *testing.T) {
			g, err := p.New("whole-node-retry-"+operation, "")
			messagePublicOK(t, err)
			key := p.NewKey[p.Stream[int]]("messages")
			var sourceAspects, sinkAspects []p.Aspect
			if operation == "open" {
				sourceAspects = []p.Aspect{p.Retry(2, 0)}
			} else {
				sinkAspects = []p.Aspect{p.Retry(2, 0)}
			}
			messagePublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
				writer, err := p.OpenStream(rc, key)
				if err != nil {
					return err
				}
				return writer.Send(1)
			}, sourceAspects...)))
			messagePublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(key), nil, func(rc *p.RunCtx) error {
				stream, err := p.Get(rc, key)
				if err != nil {
					return err
				}
				delivery, err := stream.Receive(rc)
				if err != nil {
					return err
				}
				return delivery.Ack()
			}, sinkAspects...)))
			if err := g.Run(messagePublicContext(t), p.Inputs{}); err == nil || !strings.Contains(err.Error(), "write set") {
				t.Fatalf("whole-node Retry %s = %v", operation, err)
			}
		})
	}
}
