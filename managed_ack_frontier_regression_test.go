package pulse

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestManagedAckRestoredSparseFrontierAndSequenceBoundary(t *testing.T) {
	for _, last := range []uint64{1 << 60, ^uint64(0)} {
		t.Run(fmt.Sprint(last), func(t *testing.T) {
			_, stream, _, rc := managedTestEndpoint[int](t, 3)
			// 巨大的已确认区间来自恢复记录，不应该逐个序号遍历空白。
			stream.state.installCheckpointRecord(CheckpointStreamRecord{
				Stream:       CheckpointStreamDescriptor{Terminal: StreamCompleted},
				LastSequence: last, AckedThrough: 0,
				Pending: []CheckpointStreamMessage{{Sequence: 1, Value: 11}, {Sequence: last - 1, Value: 22}, {Sequence: last, Value: 33}},
			})
			var deliveries []Delivery[int]
			for range 3 {
				delivery, err := stream.Receive(rc)
				managedTestOK(t, err)
				deliveries = append(deliveries, delivery)
			}
			managedTestOK(t, deliveries[2].Ack())
			if got := stream.Snapshot(); got.AckedThrough != 0 || got.InFlight != 2 {
				t.Fatalf("out-of-order Ack crossed a gap: %+v", got)
			}
			managedTestOK(t, deliveries[0].Nack())
			retry, err := stream.Receive(rc)
			managedTestOK(t, err)
			if retry.Sequence != 1 || retry.Value != 11 {
				t.Fatalf("Nack changed the pending identity: %+v", retry)
			}
			managedTestOK(t, retry.Ack())
			if got := stream.Snapshot(); got.AckedThrough != last-2 || got.InFlight != 1 {
				t.Fatalf("Ack did not jump over confirmed sparse history: %+v", got)
			}
			managedTestOK(t, deliveries[1].Ack())
			managedTestOK(t, deliveries[1].Ack())
			if got := stream.Snapshot(); got.AckedThrough != last || got.InFlight != 0 {
				t.Fatalf("closing the last gap lost or overflowed the frontier: %+v", got)
			}
			if _, err := stream.Receive(rc); err != io.EOF {
				t.Fatalf("completed sparse stream did not reach EOF: %v", err)
			}
		})
	}
}

// 测量真实 ProduceStream -> Receive/Ack，先等生产完成再确认，
// 使顺序确认期间 N 条消息都已在 pending，不把生产背压当成确认成本。
func BenchmarkManagedSequentialAck(b *testing.B) {
	for _, count := range []int{1024, 4096, 16384} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				ctx, cancel := context.WithTimeout(b.Context(), 30*time.Second)
				g, err := New("ack-benchmark", "")
				if err != nil {
					b.Fatal(err)
				}
				key := NewKey[Stream[int]]("input")
				err = ProduceStream(g, "source", "", key, func(_ *RunCtx, writer *StreamWriter[int]) error {
					for i := 0; i < count; i++ {
						if err := writer.Send(i); err != nil {
							return err
						}
					}
					return nil
				}, WithBuffer(count))
				if err != nil {
					b.Fatal(err)
				}
				err = g.Add(NewNode("reader", "", Requires(key), nil, func(rc *RunCtx) error {
					stream, err := Get(rc, key)
					if err != nil {
						return err
					}
					if err := rc.WaitStream(func(ctx context.Context) error {
						select {
						case <-stream.Done():
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}); err != nil {
						return err
					}
					if got := stream.Snapshot().InFlight; got != count {
						return fmt.Errorf("full pending set=%d, want %d", got, count)
					}
					for range count {
						delivery, err := stream.Receive(rc)
						if err != nil {
							return err
						}
						if err := delivery.Ack(); err != nil {
							return err
						}
					}
					return nil
				}))
				if err != nil {
					b.Fatal(err)
				}
				err = g.Run(ctx, Inputs{})
				cancel()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
