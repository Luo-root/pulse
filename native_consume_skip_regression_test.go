package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeConsumeNormalSkipDrainsFiniteSource(t *testing.T) {
	for _, tee := range []bool{false, true} {
		for _, buffer := range []int{0, 2} {
			t.Run(fmt.Sprintf("tee=%t/buffer=%d", tee, buffer), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				var sent, skippedCalls, siblingCalls atomic.Int32
				var readerReason NodeFinishReason
				g, err := New("native-skip", "", WithObserver(ObserverFunc{Finished: func(_ Site, id string, reason NodeFinishReason, _ error) {
					if id == "skip-reader" {
						readerReason = reason
					}
				}}))
				managedTestOK(t, err)
				input := NewKey[<-chan int]("input")
				managedTestOK(t, Produce(g, "source", "", input, func(_ *RunCtx, send func(int) error) error {
					for i := 1; i <= 3; i++ {
						if err := send(i); err != nil {
							return err
						}
						sent.Add(1)
					}
					return nil
				}, WithBuffer(buffer)))
				readerInput := input
				if tee {
					readerInput = NewKey[<-chan int]("skip-branch")
					sibling := NewKey[<-chan int]("sibling-branch")
					managedTestOK(t, Tee(g, "tee", "", input, []Key[<-chan int]{readerInput, sibling}, WithBuffer(buffer)))
					managedTestOK(t, Consume(g, "sibling", "", sibling, func(*RunCtx, int) error {
						siblingCalls.Add(1)
						return nil
					}))
				}
				skip := NoValue("consumer has no result")
				managedTestOK(t, Consume(g, "skip-reader", "", readerInput, func(*RunCtx, int) error {
					skippedCalls.Add(1)
					return skip
				}))
				if err := g.Run(ctx, Inputs{}); err != nil {
					t.Fatalf("normal consumer skip became execution failure: %v (sent=%d callbacks=%d)", err, sent.Load(), skippedCalls.Load())
				}
				if sent.Load() != 3 || skippedCalls.Load() != 1 || readerReason != NodeSkipped {
					t.Fatalf("sent=%d callbacks=%d reader=%s", sent.Load(), skippedCalls.Load(), readerReason)
				}
				if tee && siblingCalls.Load() != 3 {
					t.Fatalf("skipped branch interrupted sibling delivery: %d", siblingCalls.Load())
				}
			})
		}
	}
}

// 已开始排空后遇到的真实生产失败/取消不能降成前面的正常缺失。
func TestNativeConsumeSkipDrainPreservesFailureAndCancel(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			g, err := New("native-skip-failure", "")
			managedTestOK(t, err)
			key := NewKey[<-chan int]("input")
			callback := make(chan struct{})
			failure := errors.New("producer failed after reader skip")
			managedTestOK(t, Produce(g, "source", "", key, func(rc *RunCtx, send func(int) error) error {
				if err := send(1); err != nil {
					return err
				}
				select {
				case <-callback:
				case <-rc.Context().Done():
					return rc.Context().Err()
				}
				if canceled {
					cancel()
					return rc.Context().Err()
				}
				return failure
			}))
			managedTestOK(t, Consume(g, "sink", "", key, func(*RunCtx, int) error {
				close(callback)
				return NoValue()
			}))
			err = g.Run(ctx, Inputs{})
			want := failure
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("post-skip failure/cancel lost: got %v, want %v", err, want)
			}
		})
	}
}
