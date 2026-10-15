package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReaderLifecyclePendingAbsenceAndFailure(t *testing.T) {
	failure := errors.New("source failed before opening")
	for _, mode := range []string{"Skip", "implicit", "NoValue", "failure"} {
		for _, shortSkip := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shortSkip=%t", mode, shortSkip), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				key := NewKey[Stream[int]]("input")
				arrive := make(chan struct{})
				witness := &managedWaitWitness{Context: ctx, entered: arrive}
				g, err := New("absent-reader", "", WithMaxRunning(1))
				managedTestOK(t, err)
				managedTestOK(t, g.Add(NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
					if err := readerLifecycleWait(rc, arrive); err != nil {
						return err
					}
					switch mode {
					case "Skip":
						return Skip(rc, key)
					case "NoValue":
						return NoValue()
					case "failure":
						return failure
					default:
						return nil
					}
				})))
				managedTestOK(t, g.Add(NewNode("sink", "", Requires(key), nil, func(*RunCtx) error {
					return errors.New("short-circuited body ran")
				}, func(rc *RunCtx, _ func(*RunCtx) error) error {
					rc.ctx = witness
					if shortSkip {
						return NoValue()
					}
					return nil
				})))
				err = g.Run(ctx, Inputs{})
				if mode == "failure" {
					if !errors.Is(err, failure) {
						t.Fatalf("source failure swallowed: %v", err)
					}
				} else if err != nil {
					t.Fatalf("normal absence became failure: %v", err)
				}
			})
		}
	}
}

func TestReaderLifecyclePendingNestedAliases(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	root, err := New("root", "", WithMaxRunning(1))
	managedTestOK(t, err)
	producer, err := New("producer", "")
	managedTestOK(t, err)
	middle, err := New("middle", "")
	managedTestOK(t, err)
	consumer, err := New("consumer", "")
	managedTestOK(t, err)
	leafOut, middleOut, rootOut := NewKey[Stream[int]]("leaf"), NewKey[Stream[int]]("middle"), NewKey[Stream[int]]("root")
	left, right := NewKey[Stream[int]]("left"), NewKey[Stream[int]]("right")
	managedTestOK(t, producer.DeclareOutput(NewPort(leafOut)))
	managedTestOK(t, middle.DeclareOutput(NewPort(middleOut)))
	managedTestOK(t, consumer.DeclareInput(NewPort(left)))
	managedTestOK(t, consumer.DeclareInput(NewPort(right)))
	arrive := make(chan struct{})
	var endpoint Stream[int]
	managedTestOK(t, producer.Add(NewNode("source", "", nil, Provides(leafOut), func(rc *RunCtx) error {
		if err := readerLifecycleWait(rc, arrive); err != nil {
			return err
		}
		writer, err := OpenStream(rc, leafOut)
		if err != nil {
			return err
		}
		endpoint, err = Get(rc, leafOut)
		if err != nil {
			return err
		}
		return writer.Send(1)
	})))
	managedTestOK(t, consumer.Add(NewNode("sink", "", Requires(left, right), nil, nil,
		func(rc *RunCtx, _ func(*RunCtx) error) error {
			rc.ctx = &managedWaitWitness{Context: rc.ctx, entered: arrive}
			return NoValue()
		})))
	managedTestOK(t, Sub(middle, "producer", "", []SubBind{Out(leafOut, middleOut)}, producer))
	managedTestOK(t, Sub(root, "middle", "", []SubBind{Out(middleOut, rootOut)}, middle))
	managedTestOK(t, Sub(root, "consumer", "", []SubBind{In(rootOut, left), In(rootOut, right)}, consumer))
	err = root.Run(ctx, Inputs{})
	if err == nil || !strings.Contains(err.Error(), "reading obligations") {
		t.Fatalf("nested alias lost actual reader: %v (%+v)", err, endpoint.Snapshot())
	}
	if got := endpoint.Snapshot(); got.AckedThrough != 0 {
		t.Fatalf("alias cleanup confirmed data: %+v", got)
	}
}

func TestReaderLifecyclePendingFanOutLastExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	g, err := New("late-worker", "", WithMaxRunning(1))
	managedTestOK(t, err)
	key := NewKey[Stream[int]]("input")
	arrive := make(chan struct{})
	managedTestOK(t, g.Add(NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
		if err := readerLifecycleWait(rc, arrive); err != nil {
			return err
		}
		writer, err := OpenStream(rc, key)
		if err != nil {
			return err
		}
		return writer.Send(1)
	})))
	managedTestOK(t, FanOut(g, "workers", "", key, Keys(NewKey[int]("one"), NewKey[int]("two")),
		func(rc *RunCtx, _ int, stream Stream[int]) (int, error) {
			_, err := stream.Receive(rc)
			return 0, err // 最后一个真实 worker 未确认，先退出的 worker 不能成为幽灵读取者。
		}, func(rc *RunCtx, next func(*RunCtx) error) error {
			if rc.NodeID() == "workers-1" {
				rc.ctx = &managedWaitWitness{Context: rc.ctx, entered: arrive}
				return nil
			}
			return next(rc)
		}))
	err = g.Run(ctx, Inputs{})
	if err == nil || !strings.Contains(err.Error(), "reading obligations") {
		t.Fatalf("last actual worker was treated as still having peers: %v", err)
	}
}

func TestReaderLifecyclePendingWaitRestoresQuotaAndNeverAcksOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	key := NewKey[Stream[int]]("input")
	g, err := New("quota-reader", "", WithMaxRunning(1))
	managedTestOK(t, err)
	source, sink := NewNode("source", "", nil, Provides(key), nil), NewNode("sink", "", Requires(key), nil, nil)
	managedTestOK(t, g.Add(source))
	managedTestOK(t, g.Add(sink))
	e, err := g.NewExecution(ctx, Inputs{})
	managedTestOK(t, err)
	defer e.Cancel()
	writerRC, readerRC := newRunCtx(e, source, ctx), newRunCtx(e, sink, ctx)
	defer writerRC.Cancel()
	defer readerRC.Cancel()
	managedTestOK(t, readerRC.lease.acquire(ctx))
	defer readerRC.lease.release()
	witness := &managedResumeWitness{Context: readerRC.ctx, waiting: make(chan struct{}), resuming: make(chan struct{})}
	readerRC.ctx = witness
	result := make(chan error, 1)
	go func() { result <- readerRC.finishStreams(nil) }()
	select {
	case <-witness.waiting:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	managedTestOK(t, e.lim.acquire(ctx))
	var once sync.Once
	release := func() { once.Do(e.lim.release) }
	defer release()
	writer, err := OpenStream(writerRC, key)
	managedTestOK(t, err)
	managedTestOK(t, writer.Send(1))
	select {
	case <-witness.resuming:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	readerRC.Cancel()
	select {
	case err := <-result:
		t.Fatalf("cleanup returned without quota: %v", err)
	default:
	}
	release()
	if err := managedTestResult(t, ctx, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup = %v", err)
	}
	stream, err := Get(writerRC, key)
	managedTestOK(t, err)
	if got := stream.Snapshot(); got.AckedThrough != 0 || got.InFlight != 1 {
		t.Fatalf("cancel confirmed or dropped delivery: %+v", got)
	}
	readerRC.lease.mu.Lock()
	held := readerRC.lease.state == leaseHeld
	readerRC.lease.mu.Unlock()
	if !held {
		t.Fatal("cleanup did not restore quota")
	}
}
