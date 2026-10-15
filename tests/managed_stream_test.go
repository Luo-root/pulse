package pulse_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
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
