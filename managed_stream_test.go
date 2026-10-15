package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func managedTestOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func managedTestEndpoint[T any](t *testing.T, buffer int) (*StreamWriter[T], Stream[T], *RunCtx, *RunCtx) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	key := NewKey[Stream[T]]("messages")
	g, err := New("managed", "")
	managedTestOK(t, err)
	producer := NewNode("source", "", nil, Provides(key), nil)
	consumer := NewNode("sink", "", Requires(key), nil, nil)
	managedTestOK(t, g.Add(producer))
	managedTestOK(t, g.Add(consumer))
	e, err := g.NewExecution(ctx, Inputs{})
	managedTestOK(t, err)
	t.Cleanup(e.Cancel)
	writerRC, readerRC := newRunCtx(e, producer, e.Context()), newRunCtx(e, consumer, e.Context())
	t.Cleanup(writerRC.Cancel)
	t.Cleanup(readerRC.Cancel)
	writer, err := OpenStream(writerRC, key, WithBuffer(buffer))
	managedTestOK(t, err)
	stream, err := Get(readerRC, key)
	managedTestOK(t, err)
	return writer, stream, writerRC, readerRC
}

// Done 被 select 求值证明受管等待已进入；不靠 sleep 猜 Send 是否阻塞。
type managedWaitWitness struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *managedWaitWitness) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func managedTestBlockedSend(t *testing.T, rc *RunCtx, send func() error) <-chan error {
	t.Helper()
	witness := &managedWaitWitness{Context: rc.Context(), entered: make(chan struct{})}
	rc.ctx = witness
	result := make(chan error, 1)
	go func() { result <- send() }()
	select {
	case <-witness.entered:
	case err := <-result:
		t.Fatalf("Send returned before waiting for acknowledgement: %v", err)
	case <-witness.Context.Done():
		t.Fatal(witness.Context.Err())
	}
	return result
}

func managedTestResult(t *testing.T, ctx context.Context, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return ctx.Err()
	}
}

func TestManagedBackpressureWaitsForAck(t *testing.T) {
	for _, buffer := range []int{0, -1, 1} {
		t.Run(fmt.Sprint(buffer), func(t *testing.T) {
			writer, stream, writerRC, readerRC := managedTestEndpoint[int](t, buffer)
			managedTestOK(t, writer.Send(1))
			first, err := stream.Receive(readerRC)
			managedTestOK(t, err)
			result := managedTestBlockedSend(t, writerRC, func() error { return writer.Send(2) })
			if got := stream.Snapshot(); got.LastSequence != 1 || got.InFlight != 1 || got.AckedThrough != 0 {
				t.Fatalf("receive released capacity: %+v", got)
			}
			select {
			case err := <-result:
				t.Fatalf("Send returned before Ack: %v", err)
			default:
			}
			managedTestOK(t, first.Ack())
			managedTestOK(t, managedTestResult(t, readerRC.Context(), result))
			second, err := stream.Receive(readerRC)
			managedTestOK(t, err)
			if second.Value != 2 || second.Sequence != 2 {
				t.Fatalf("second delivery = %+v", second)
			}
			managedTestOK(t, second.Ack())
			managedTestOK(t, writerRC.finishStreams(nil))
			if _, err := stream.Receive(readerRC); err != io.EOF {
				t.Fatalf("completed Receive = %v", err)
			}
		})
	}
}

func TestManagedAckFrontierAndNackAfterProduction(t *testing.T) {
	writer, stream, writerRC, readerRC := managedTestEndpoint[int](t, 3)
	for i := 1; i <= 3; i++ {
		managedTestOK(t, writer.Send(i))
	}
	var deliveries [3]Delivery[int]
	for i := range deliveries {
		var err error
		deliveries[i], err = stream.Receive(readerRC)
		managedTestOK(t, err)
	}
	managedTestOK(t, deliveries[0].Ack())
	managedTestOK(t, deliveries[2].Ack())
	managedTestOK(t, deliveries[2].Ack())
	if got := stream.Snapshot(); got.AckedThrough != 1 || got.InFlight != 1 {
		t.Fatalf("Ack 3 crossed missing Ack 2: %+v", got)
	}
	managedTestOK(t, writer.Send(4))
	fourth, err := stream.Receive(readerRC)
	managedTestOK(t, err)
	managedTestOK(t, fourth.Ack())
	managedTestOK(t, writerRC.finishStreams(nil))
	writerRC.Cancel() // 引擎正常写端清理不能撤销已接纳消息。
	select {
	case <-stream.Done():
	default:
		t.Fatal("production terminal not published")
	}
	managedTestOK(t, deliveries[1].Nack())
	if err := deliveries[1].Ack(); !errors.Is(err, ErrDeliveryExpired) {
		t.Fatalf("old Ack after Nack = %v", err)
	}
	if got := stream.Snapshot(); got.AckedThrough != 1 || got.InFlight != 1 || got.Terminal != StreamCompleted {
		t.Fatalf("Nack changed frontier or production: %+v", got)
	}
	retry, err := stream.Receive(readerRC)
	managedTestOK(t, err)
	if retry.Value != 2 || retry.Sequence != 2 {
		t.Fatalf("redelivery = %+v", retry)
	}
	managedTestOK(t, retry.Ack())
	if got := stream.Snapshot(); got.AckedThrough != 4 || got.InFlight != 0 {
		t.Fatalf("closing gap did not advance frontier: %+v", got)
	}
	if _, err := stream.Receive(readerRC); err != io.EOF {
		t.Fatalf("drained Receive = %v", err)
	}
}

func TestManagedCancellationDoesNotAck(t *testing.T) {
	writer, stream, writerRC, readerRC := managedTestEndpoint[int](t, 1)
	managedTestOK(t, writer.Send(1))
	canceled := readerRC.Fork()
	canceled.Cancel()
	if _, err := stream.Receive(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Receive = %v", err)
	}
	if got := stream.Snapshot(); got.AckedThrough != 0 || got.InFlight != 1 || len(stream.state.queue) != 1 {
		t.Fatalf("cancellation consumed or acknowledged: %+v", got)
	}
	first, err := stream.Receive(readerRC)
	managedTestOK(t, err)
	result := managedTestBlockedSend(t, writerRC, func() error { return writer.Send(2) })
	writerRC.Cancel()
	if err := managedTestResult(t, readerRC.Context(), result); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Send = %v", err)
	}
	if err := writerRC.finishStreams(nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("swallowed canceled Send became success: %v", err)
	}
	if got := stream.Snapshot(); got.Terminal != StreamCanceled || got.InFlight != 1 || got.AckedThrough != 0 {
		t.Fatalf("canceled production = %+v", got)
	}
	managedTestOK(t, first.Ack())
	if _, err := stream.Receive(readerRC); !errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		t.Fatalf("canceled production drain = %v", err)
	}
}

// 第二次 Err 检查与真实接收/取消会合，使“接纳后改报取消”的变异必定失败。
type managedPostSendWitness struct {
	context.Context
	accepted <-chan struct{}
	calls    atomic.Int32
}

func (c *managedPostSendWitness) Err() error {
	if c.calls.Add(1) > 1 {
		<-c.accepted
	}
	return c.Context.Err()
}

func TestManagedAcceptedSendSurvivesConcurrentCancellation(t *testing.T) {
	writer, stream, writerRC, readerRC := managedTestEndpoint[int](t, 1)
	accepted := make(chan struct{})
	writerRC.ctx = &managedPostSendWitness{Context: writerRC.Context(), accepted: accepted}
	deliveries := make(chan Delivery[int], 1)
	go func() {
		delivery := <-stream.state.queue
		writerRC.Cancel()
		close(accepted)
		deliveries <- delivery
	}()
	if err := writer.Send(7); err != nil {
		t.Fatalf("accepted Send was rewritten as failure: %v", err)
	}
	var delivery Delivery[int]
	select {
	case delivery = <-deliveries:
	case <-readerRC.Context().Done():
		t.Fatal(readerRC.Context().Err())
	}
	if err := writerRC.Context().Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation rendezvous did not happen: %v", err)
	}
	if delivery.Value != 7 || delivery.Sequence != 1 || writer.state.failure() != nil {
		t.Fatalf("accepted delivery or failure latch changed: %+v, %v", delivery, writer.state.failure())
	}
	managedTestOK(t, writerRC.finishStreams(nil))
	managedTestOK(t, delivery.Ack())
}

type managedResumeWitness struct {
	context.Context
	waiting  chan struct{}
	resuming chan struct{}
	calls    atomic.Int32
}

func (c *managedResumeWitness) Done() <-chan struct{} {
	switch c.calls.Add(1) {
	case 1:
		close(c.waiting)
	case 2:
		close(c.resuming)
	}
	return c.Context.Done()
}

func TestManagedReceivedDeliverySurvivesCancellationWhileResumingQuota(t *testing.T) {
	writer, stream, writerRC, readerRC := managedTestEndpoint[int](t, 1)
	pool := newLimiter(1)
	readerRC.lease = &runLease{pool: pool}
	managedTestOK(t, readerRC.lease.acquire(readerRC.Context()))
	t.Cleanup(readerRC.lease.release)
	witness := &managedResumeWitness{Context: readerRC.Context(), waiting: make(chan struct{}), resuming: make(chan struct{})}
	readerRC.ctx = witness
	type outcome struct {
		delivery Delivery[int]
		err      error
	}
	result := make(chan outcome, 1)
	go func() {
		delivery, err := stream.Receive(readerRC)
		result <- outcome{delivery, err}
	}()
	select {
	case <-witness.waiting:
	case <-writerRC.Context().Done():
		t.Fatal(writerRC.Context().Err())
	}
	// Receive 已归还额度；用另一个持额者占住它，固定“取得后、恢复前”的窗口。
	managedTestOK(t, pool.acquire(writerRC.Context()))
	var releaseOnce sync.Once
	releaseExternal := func() { releaseOnce.Do(pool.release) }
	t.Cleanup(releaseExternal)
	managedTestOK(t, writer.Send(9))
	select {
	case <-witness.resuming:
	case <-writerRC.Context().Done():
		t.Fatal(writerRC.Context().Err())
	}
	readerRC.Cancel()
	releaseExternal()
	var got outcome
	select {
	case got = <-result:
	case <-writerRC.Context().Done():
		t.Fatal(writerRC.Context().Err())
	}
	if got.err != nil || got.delivery.Value != 9 || !got.delivery.active() {
		t.Fatalf("completed receive became cancellation or stale delivery: %+v, %v", got.delivery, got.err)
	}
	if snapshot := stream.Snapshot(); snapshot.InFlight != 1 || snapshot.AckedThrough != 0 || len(stream.state.queue) != 0 {
		t.Fatalf("cancellation automatically Acked/Nacked received delivery: %+v", snapshot)
	}
	managedTestOK(t, got.delivery.Ack())
}
