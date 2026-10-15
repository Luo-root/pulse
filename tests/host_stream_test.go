package pulse_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	p "github.com/Luo-root/pulse"
)

func hostStreamGraph(t *testing.T, opts ...p.Option) (*p.Graph, p.Key[p.Stream[int]], context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	g, err := p.New("host-stream", "", append([]p.Option{p.WithMaxRunning(1)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	key := p.NewKey[p.Stream[int]]("tokens")
	if err := g.DeclareOutput(p.NewPort(key)); err != nil {
		t.Fatal(err)
	}
	return g, key, ctx
}

func TestHostStreamReceivesBeforeWriterReturns(t *testing.T) {
	g, key, ctx := hostStreamGraph(t)
	release := make(chan struct{})
	returned := make(chan struct{})
	if err := g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
		defer close(returned)
		writer, err := p.OpenStream(rc, key)
		if err != nil {
			return err
		}
		if err := writer.Send(7); err != nil {
			return err
		}
		select {
		case <-release:
			return nil
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	})); err != nil {
		t.Fatal(err)
	}
	e, err := g.NewExecution(ctx, p.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	sub, err := p.SubscribeStream(e, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Receive(ctx); !errors.Is(err, p.ErrGraphNotStarted) {
		t.Fatalf("read before Start = %v", err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	message, err := sub.Receive(ctx)
	if err != nil || message.Value != 7 {
		t.Fatalf("first message=%+v err=%v", message, err)
	}
	select {
	case <-returned:
		t.Fatal("first message was delayed until source returned")
	default:
	}
	if _, _, err := p.ReadOutput(e, key); err == nil {
		t.Fatal("ReadOutput was allowed before Wait")
	}
	if err := message.Ack(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if _, err := sub.Receive(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal read = %v", err)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestHostStreamWaitRequiresCompletedTailConfirmation(t *testing.T) {
	sourceDone := make(chan struct{})
	g, key, ctx := hostStreamGraph(t, p.WithObserver(p.ObserverFunc{
		Finished: func(_ p.Site, id string, _ p.NodeFinishReason, _ error) {
			if id == "source" {
				close(sourceDone)
			}
		},
	}))
	if err := g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
		writer, err := p.OpenStream(rc, key, p.WithBuffer(2))
		if err != nil {
			return err
		}
		for _, v := range []int{4, 5} {
			if err := writer.Send(v); err != nil {
				return err
			}
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	e, err := g.NewExecution(ctx, p.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := p.SubscribeStream(e, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	waited := make(chan error, 1)
	go func() { waited <- e.Wait() }()
	select {
	case <-sourceDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// 实际写端已完成，宿主仍欠两条确认；不能提前把整轮报成成功。
	select {
	case err := <-waited:
		t.Fatalf("Wait finished before host confirmation: %v", err)
	default:
	}
	for _, want := range []int{4, 5} {
		message, err := sub.Receive(ctx)
		if err != nil || message.Value != want {
			t.Fatalf("completed source tail: value=%d err=%v want=%d", message.Value, err, want)
		}
		if err := message.Ack(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sub.Receive(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("tail terminal = %v", err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestHostStreamRegistrationAndSkip(t *testing.T) {
	g, key, ctx := hostStreamGraph(t)
	if err := g.Add(p.NewNode("source", "", nil, p.Provides(key), func(*p.RunCtx) error { return nil })); err != nil {
		t.Fatal(err)
	}
	e, err := g.NewExecution(ctx, p.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SubscribeStream(e, p.NewKey[p.Stream[int]]("internal")); err == nil {
		t.Fatal("undeclared output accepted")
	}
	sub, err := p.SubscribeStream(e, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SubscribeStream(e, key); err == nil {
		t.Fatal("duplicate subscription accepted")
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SubscribeStream(e, key); !errors.Is(err, p.ErrGraphStarted) {
		t.Fatalf("late registration = %v", err)
	}
	if _, err := sub.Receive(ctx); !errors.Is(err, p.ErrSkipped) {
		t.Fatalf("unopened stream must be skipped: %v", err)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestHostStreamSubscriptionMakesPassThroughInputRequired(t *testing.T) {
	g, key, ctx := hostStreamGraph(t)
	if err := g.DeclareInput(p.NewPort(key)); err != nil {
		t.Fatal(err)
	}
	e, err := g.NewExecution(ctx, p.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SubscribeStream(e, key); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err == nil {
		e.Cancel()
		t.Fatal("required host pass-through was allowed to wait forever for an unfed inlet")
	}
	var inputs p.Inputs
	if err := p.SkipSeed(&inputs, key); err != nil {
		t.Fatal(err)
	}
	e, err = g.NewExecution(ctx, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SubscribeStream(e, key); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		t.Fatalf("explicit normal skip = %v", err)
	}
}

func TestHostStreamCannotSplitAnInternalReader(t *testing.T) {
	g, key, ctx := hostStreamGraph(t)
	if err := g.Add(p.NewNode("source", "", nil, p.Provides(key), nil)); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(p.NewNode("sink", "", p.Requires(key), nil, nil)); err != nil {
		t.Fatal(err)
	}
	e, err := g.NewExecution(ctx, p.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SubscribeStream(e, key); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err == nil {
		t.Fatal("host and graph reader silently share one group")
	}
}
