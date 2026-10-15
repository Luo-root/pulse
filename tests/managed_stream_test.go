package pulse_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/Luo-root/pulse"
)

func managedPublicOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func managedPublicGraph(t *testing.T, id string, opts ...p.Option) *p.Graph {
	t.Helper()
	g, err := p.New(id, "", opts...)
	managedPublicOK(t, err)
	return g
}

func managedPublicContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestManagedNewNodePipeline(t *testing.T) {
	for _, limit := range []int{0, 1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			g := managedPublicGraph(t, "pipeline", p.WithMaxRunning(limit))
			input, output := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("output")
			var got []int
			managedPublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(input), func(rc *p.RunCtx) error {
				writer, err := p.OpenStream(rc, input)
				if err != nil {
					return err
				}
				for i := 1; i <= 8; i++ {
					if err := writer.Send(i); err != nil {
						return err
					}
				}
				return nil
			})))
			managedPublicOK(t, g.Add(p.NewNode("map", "", p.Requires(input), p.Provides(output), func(rc *p.RunCtx) error {
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
					if err := writer.Send(delivery.Value * 2); err != nil {
						return err
					}
					if err := delivery.Ack(); err != nil {
						return err
					}
				}
			})))
			managedPublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(output), nil, func(rc *p.RunCtx) error {
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
			managedPublicOK(t, g.Run(managedPublicContext(t), p.Inputs{}))
			if want := []int{2, 4, 6, 8, 10, 12, 14, 16}; !reflect.DeepEqual(got, want) {
				t.Fatalf("pipeline = %v, want %v", got, want)
			}
		})
	}
}

func TestManagedEmptyIsReadyAndUnopenedIsSkipped(t *testing.T) {
	for _, opened := range []bool{false, true} {
		t.Run(fmt.Sprint(opened), func(t *testing.T) {
			g := managedPublicGraph(t, "empty")
			key := p.NewKey[p.Stream[int]]("messages")
			var entered atomic.Int32
			managedPublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
				if opened {
					_, err := p.OpenStream(rc, key)
					return err
				}
				return nil
			})))
			managedPublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(key), nil, func(rc *p.RunCtx) error {
				entered.Add(1)
				stream, err := p.Get(rc, key)
				if err != nil {
					return err
				}
				if _, err := stream.Receive(rc); err != io.EOF {
					return fmt.Errorf("empty stream Receive = %v", err)
				}
				if got := stream.Snapshot(); got.Terminal != p.StreamCompleted || got.Err != nil || got.LastSequence != 0 {
					return fmt.Errorf("empty stream terminal = %+v", got)
				}
				return nil
			})))
			e, err := g.NewExecution(managedPublicContext(t), p.Inputs{})
			managedPublicOK(t, err)
			managedPublicOK(t, e.Start())
			managedPublicOK(t, e.Wait())
			want := int32(0)
			if opened {
				want = 1
			}
			if entered.Load() != want {
				t.Fatalf("sink entered %d times, want %d", entered.Load(), want)
			}
		})
	}
}

func TestManagedFullChainFailureNeverLooksLikeEOF(t *testing.T) {
	g := managedPublicGraph(t, "late-failure")
	key := p.NewKey[p.Stream[int]]("messages")
	bodyReturned, release := make(chan struct{}), make(chan struct{})
	observed := make(chan error, 1)
	endpoint := make(chan p.Stream[int], 1)
	late := errors.New("failure after source body returned")
	aspect := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
		if err := next(rc); err != nil {
			return err
		}
		close(bodyReturned)
		select {
		case <-release:
			return late
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	}
	managedPublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
		_, err := p.OpenStream(rc, key)
		return err
	}, aspect)))
	managedPublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(key), nil, func(rc *p.RunCtx) error {
		stream, err := p.Get(rc, key)
		if err != nil {
			return err
		}
		endpoint <- stream
		_, err = stream.Receive(rc)
		observed <- err
		return err
	})))
	ctx := managedPublicContext(t)
	e, err := g.NewExecution(ctx, p.Inputs{})
	managedPublicOK(t, err)
	managedPublicOK(t, e.Start())
	var stream p.Stream[int]
	select {
	case stream = <-endpoint:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-bodyReturned:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-stream.Done():
		t.Fatal("stream completed before the outer aspect returned")
	case err := <-observed:
		t.Fatalf("consumer saw a premature terminal: %v", err)
	default:
	}
	close(release)
	if err := e.Wait(); !errors.Is(err, late) {
		t.Fatalf("execution failure = %v", err)
	}
	if err := <-observed; !errors.Is(err, late) || errors.Is(err, io.EOF) {
		t.Fatalf("consumer saw %v, want producer failure", err)
	}
	if got := stream.Snapshot(); got.Terminal != p.StreamFailed || !errors.Is(got.Err, late) {
		t.Fatalf("stream terminal = %+v", got)
	}
}

func TestManagedMessagesSelectWithOneQuota(t *testing.T) {
	g := managedPublicGraph(t, "select", p.WithMaxRunning(1))
	key := p.NewKey[p.Stream[int]]("messages")
	managedPublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
		writer, err := p.OpenStream(rc, key)
		if err != nil {
			return err
		}
		for i := 1; i <= 5; i++ {
			if err := writer.Send(i); err != nil {
				return err
			}
		}
		return nil
	})))
	var values []int
	managedPublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(key), nil, func(rc *p.RunCtx) error {
		stream, err := p.Get(rc, key)
		if err != nil {
			return err
		}
		messages, err := stream.Messages(rc)
		if err != nil {
			return err
		}
		for {
			var delivery p.Delivery[int]
			var ok bool
			if err := rc.WaitStream(func(ctx context.Context) error {
				select {
				case delivery, ok = <-messages:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}); err != nil {
				return err
			}
			if !ok {
				return stream.Snapshot().Err
			}
			values = append(values, delivery.Value)
			if err := delivery.Ack(); err != nil {
				return err
			}
		}
	})))
	managedPublicOK(t, g.Run(managedPublicContext(t), p.Inputs{}))
	if want := []int{1, 2, 3, 4, 5}; !reflect.DeepEqual(values, want) {
		t.Fatalf("native select values = %v", values)
	}
}

func TestManagedExplicitStopIsNormal(t *testing.T) {
	g := managedPublicGraph(t, "stopped", p.WithMaxRunning(1))
	key := p.NewKey[p.Stream[int]]("messages")
	endpoint := make(chan p.Stream[int], 1)
	managedPublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
		writer, err := p.OpenStream(rc, key)
		if err != nil {
			return err
		}
		for i := 0; ; i++ {
			if err := writer.Send(i); errors.Is(err, p.ErrStreamStopped) {
				return nil
			} else if err != nil {
				return err
			}
		}
	})))
	managedPublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(key), nil, func(rc *p.RunCtx) error {
		stream, err := p.Get(rc, key)
		if err != nil {
			return err
		}
		if _, err := stream.Receive(rc); err != nil {
			return err
		}
		endpoint <- stream
		return stream.Stop(rc)
	})))
	managedPublicOK(t, g.Run(managedPublicContext(t), p.Inputs{}))
	if got := (<-endpoint).Snapshot(); got.Terminal != p.StreamStopped || got.Err != nil || got.AckedThrough != 0 || got.InFlight != 0 {
		t.Fatalf("explicit stop became failure or acknowledgement: %+v", got)
	}
}

func TestManagedReaderCannotSilentlyAbandonStream(t *testing.T) {
	for _, waitProduction := range []bool{false, true} {
		t.Run(fmt.Sprint(waitProduction), func(t *testing.T) {
			g := managedPublicGraph(t, "abandoned")
			key := p.NewKey[p.Stream[int]]("messages")
			managedPublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
				writer, err := p.OpenStream(rc, key)
				if err != nil {
					return err
				}
				if err := writer.Send(1); err != nil {
					return err
				}
				if !waitProduction {
					return writer.Send(2)
				}
				return nil
			})))
			managedPublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(key), nil, func(rc *p.RunCtx) error {
				if waitProduction {
					stream, err := p.Get(rc, key)
					if err != nil {
						return err
					}
					return rc.WaitStream(func(ctx context.Context) error {
						select {
						case <-stream.Done():
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					})
				}
				return nil // 连 Get 都没调用，也不能遗弃 Requires 的读取义务。
			})))
			if err := g.Run(managedPublicContext(t), p.Inputs{}); err == nil || !strings.Contains(err.Error(), "reading obligations") {
				t.Fatalf("abandoned stream = %v", err)
			}
		})
	}
}

func TestManagedFanOutWorkerExitHandsDeliveryToRemainingWorker(t *testing.T) {
	g := managedPublicGraph(t, "handoff", p.WithMaxRunning(1))
	key := p.NewKey[p.Stream[int]]("messages")
	outs := p.Keys(p.NewKey[int]("first"), p.NewKey[int]("second"))
	managedPublicOK(t, g.DeclareOutput(p.NewPort(outs[1])))
	managedPublicOK(t, g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
		writer, err := p.OpenStream(rc, key, p.WithBuffer(2))
		if err != nil {
			return err
		}
		if err := writer.Send(1); err != nil {
			return err
		}
		return writer.Send(2)
	})))
	firstTaken := make(chan struct{})
	old := make(chan p.Delivery[int], 1)
	managedPublicOK(t, p.FanOut(g, "workers", "", key, outs, func(rc *p.RunCtx, shard int, stream p.Stream[int]) (int, error) {
		if shard == 1 {
			delivery, err := stream.Receive(rc)
			if err != nil {
				return 0, err
			}
			old <- delivery
			close(firstTaken)
			return 0, nil
		}
		if err := rc.WaitStream(func(ctx context.Context) error {
			select {
			case <-firstTaken:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}); err != nil {
			return 0, err
		}
		sum := 0
		for {
			delivery, err := stream.Receive(rc)
			if err == io.EOF {
				return sum, nil
			}
			if err != nil {
				return 0, err
			}
			sum += delivery.Value
			if err := delivery.Ack(); err != nil {
				return 0, err
			}
		}
	}))
	e, err := g.NewExecution(managedPublicContext(t), p.Inputs{})
	managedPublicOK(t, err)
	managedPublicOK(t, e.Start())
	managedPublicOK(t, e.Wait())
	if got, _, err := p.ReadOutput(e, outs[1]); err != nil || got != 3 {
		t.Fatalf("remaining worker result = (%d, %v)", got, err)
	}
	if err := (<-old).Ack(); !errors.Is(err, p.ErrDeliveryExpired) {
		t.Fatalf("exited worker still confirms handed-off message: %v", err)
	}
}

func TestManagedDefinitionRunsSeparately(t *testing.T) {
	g := managedPublicGraph(t, "reusable")
	seed := p.NewKey[int]("seed")
	key := p.NewKey[p.Stream[int]]("messages")
	result := p.NewKey[int]("sum")
	managedPublicOK(t, g.DeclareInput(p.NewPort(seed)))
	managedPublicOK(t, g.DeclareOutput(p.NewPort(result)))
	managedPublicOK(t, g.Add(p.NewNode("source", "", p.Requires(seed), p.Provides(key), func(rc *p.RunCtx) error {
		value, err := p.Get(rc, seed)
		if err != nil {
			return err
		}
		writer, err := p.OpenStream(rc, key)
		if err != nil {
			return err
		}
		for i := 0; i < 4; i++ {
			if err := writer.Send(value + i); err != nil {
				return err
			}
		}
		return nil
	})))
	managedPublicOK(t, g.Add(p.NewNode("sink", "", p.Requires(key), p.Provides(result), func(rc *p.RunCtx) error {
		stream, err := p.Get(rc, key)
		if err != nil {
			return err
		}
		sum := 0
		for {
			delivery, err := stream.Receive(rc)
			if err == io.EOF {
				return p.Set(rc, result, sum)
			}
			if err != nil {
				return err
			}
			sum += delivery.Value
			if err := delivery.Ack(); err != nil {
				return err
			}
		}
	})))
	ctx := managedPublicContext(t)
	run := func(value int) error {
		var inputs p.Inputs
		if err := p.Seed(&inputs, seed, value); err != nil {
			return err
		}
		e, err := g.NewExecution(ctx, inputs)
		if err != nil {
			return err
		}
		if err := e.Start(); err != nil {
			return err
		}
		if err := e.Wait(); err != nil {
			return err
		}
		got, state, err := p.ReadOutput(e, result)
		if err != nil {
			return err
		}
		if got != 4*value+6 || state != p.SlotStateReady {
			return fmt.Errorf("run %d got (%d, %v)", value, got, state)
		}
		return nil
	}
	managedPublicOK(t, run(1))
	managedPublicOK(t, run(10))
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for _, value := range []int{100, 200, 300, 400} {
		wg.Go(func() { errors <- run(value) })
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		managedPublicOK(t, err)
	}
}
