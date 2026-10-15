package pulse

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestCheckpointRestoredMessageValuesStayStaged(t *testing.T) {
	for _, failCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "commit-fails"}[failCommit], func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "restored-message-value")
			value := NewKey[int]("value")
			absent := NewKey[int]("absent")
			var readers atomic.Int32
			mustAdd(t, g, NewNode("processor", "", nil, Provides(value, absent), func(rc *RunCtx) error {
				got, err := Get(rc, value)
				if err != nil || got != 42 {
					return errors.New("confirmed message value was not restored before body")
				}
				if _, err := Get(rc, absent); !isSkipped(err) {
					return errors.New("confirmed missing value was not restored")
				}
				if state, _ := rc.exec.slotOf(value.asRef()).snapshot(); state != slotPending {
					return errors.New("message value published before complete-chain commit")
				}
				return nil
			}))
			mustAdd(t, g, NewNode("reader", "", Requires(value), nil, func(rc *RunCtx) error {
				readers.Add(1)
				return nil
			}))
			probe := &checkpointProbe{}
			hooks := probe.hooks()
			open := hooks.OpenAttempt
			hooks.OpenAttempt = func(ctx context.Context, request CheckpointAttemptRequest) (CheckpointAttempt, error) {
				attempt, err := open(ctx, request)
				if request.NodeID == "processor" {
					attempt.Values = []CheckpointOutput{
						{Key: checkpointKey(value.asRef()), State: SlotStateReady, Value: 42},
						{Key: checkpointKey(absent.asRef()), State: SlotStateSkipped},
					}
				}
				return attempt, err
			}
			cause := errors.New("store did not confirm full node")
			commit := hooks.Commit
			hooks.Commit = func(ctx context.Context, result CheckpointCommit) error {
				if failCommit && result.Node.NodeID == "processor" {
					return cause
				}
				return commit(ctx, result)
			}
			err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if failCommit {
				if !errors.Is(err, cause) || readers.Load() != 0 {
					t.Fatalf("unconfirmed value became visible: readers=%d err=%v", readers.Load(), err)
				}
			} else if err != nil || readers.Load() != 1 {
				t.Fatalf("restored value was lost: readers=%d err=%v", readers.Load(), err)
			}
		})
	}
}

func TestCheckpointAttemptRejectsInvalidMessageValueSubset(t *testing.T) {
	key := CheckpointKey{Name: "value", Type: reflect.TypeFor[int]()}
	valid := CheckpointOutput{Key: key, State: SlotStateReady, Value: 42}
	for _, test := range []struct {
		name   string
		values []CheckpointOutput
	}{
		{"duplicate", []CheckpointOutput{valid, valid}},
		{"undeclared", []CheckpointOutput{{Key: CheckpointKey{Name: "other", Type: key.Type}, State: SlotStateReady, Value: 42}}},
		{"mistyped", []CheckpointOutput{{Key: key, State: SlotStateReady, Value: "wrong"}}},
		{"pending", []CheckpointOutput{{Key: key, State: SlotStatePending}}},
		{"skipped-with-value", []CheckpointOutput{{Key: key, State: SlotStateSkipped, Value: 42}}},
		{"stream-descriptor", []CheckpointOutput{{Key: key, State: SlotStateReady, Value: 42, Stream: &CheckpointStreamDescriptor{}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, test.name)
			value := NewKey[int]("value")
			var called atomic.Int32
			mustAdd(t, g, NewNode("processor", "", nil, Provides(value), func(*RunCtx) error {
				called.Add(1)
				return nil
			}))
			hooks := (&checkpointProbe{}).hooks()
			hooks.OpenAttempt = func(context.Context, CheckpointAttemptRequest) (CheckpointAttempt, error) {
				return CheckpointAttempt{Scope: "attempt", Index: 1, Values: test.values}, nil
			}
			err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err == nil || !isCheckpointFailure(err) || called.Load() != 0 {
				t.Fatalf("invalid subset reached body: calls=%d err=%v", called.Load(), err)
			}
		})
	}
}
