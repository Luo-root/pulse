package pulse_test

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	p "github.com/Luo-root/pulse"
)

func publicCheckpointStreamHooks() p.CheckpointHooks {
	var mu sync.Mutex
	sequence := 0
	attempts, failed := make(map[p.CheckpointAttemptRequest]p.CheckpointAttempt), make(map[string]bool)
	return p.CheckpointHooks{
		OpenAttempt: func(_ context.Context, request p.CheckpointAttemptRequest) (p.CheckpointAttempt, error) {
			mu.Lock()
			defer mu.Unlock()
			request.Site.Execution = ""
			attempt := attempts[request]
			if attempt.Scope != "" && !failed[attempt.Scope] {
				return attempt, nil
			}
			if attempt.Index == request.Limit {
				attempt.Exhausted = true
				return attempt, nil
			}
			sequence++
			attempt = p.CheckpointAttempt{Scope: fmt.Sprintf("public-attempt-%d", sequence), Index: attempt.Index + 1}
			attempts[request] = attempt
			return attempt, nil
		},
		FailAttempt: func(_ context.Context, scope string) error {
			mu.Lock()
			defer mu.Unlock()
			failed[scope] = true
			return nil
		},
		OpenChild: func(context.Context, p.CheckpointChild) ([]p.NodeCheckpoint, error) { return nil, nil },
		Commit:    func(context.Context, p.CheckpointCommit) error { return nil },
		StreamHooks: &p.CheckpointStreamHooks{
			Open: func(_ context.Context, request p.CheckpointStreamOpen) (p.CheckpointStreamRecord, error) {
				return p.CheckpointStreamRecord{Stream: p.CheckpointStreamDescriptor{
					ID:    p.CheckpointStreamID{Scope: request.Scope, NodeID: request.NodeID, Key: request.Key.Name, Group: request.Group, Generation: 1},
					Graph: request.Site.Graph, Path: request.Site.Path, Attempt: request.Attempt, Element: request.Key.Element,
					Source: request.Source, Capacity: request.Capacity,
				}}, nil
			},
			Append: func(context.Context, p.CheckpointStreamAppend) error { return nil },
			Commit: func(context.Context, p.CheckpointMessageCommit) error { return nil },
			OpenMessage: func(_ context.Context, request p.CheckpointMessageAttemptRequest) (p.CheckpointMessageAttempt, error) {
				return p.CheckpointMessageAttempt{Attempt: p.CheckpointAttempt{Scope: request.Attempt + "/message", Index: 1}}, nil
			},
			FailMessage: func(context.Context, string) error { return nil },
			Finish:      func(context.Context, p.CheckpointStreamFinish) error { return nil },
		},
	}
}

func TestCheckpointFourHooksStillSupportOrdinaryNodes(t *testing.T) {
	g := managedPublicGraph(t, "ordinary-checkpoint")
	key := p.NewKey[int]("result")
	managedPublicOK(t, g.Add(p.NewNode("source", "旧四钩子继续合法", nil, p.Provides(key), func(rc *p.RunCtx) error { return p.Set(rc, key, 42) })))
	managedPublicOK(t, g.DeclareOutput(p.NewPort(key)))
	hooks := publicCheckpointStreamHooks()
	hooks.StreamHooks = nil
	e, err := g.NewExecution(managedPublicContext(t), p.Inputs{}, p.WithExecCheckpoint("root", hooks))
	managedPublicOK(t, err)
	managedPublicOK(t, e.Start())
	managedPublicOK(t, e.Wait())
	value, state, err := p.ReadOutput(e, key)
	managedPublicOK(t, err)
	if value != 42 || state != p.SlotStateReady {
		t.Fatalf("ordinary checkpoint output=%d", value)
	}
}

func TestCheckpointActiveSourceContinuesFromPublicPosition(t *testing.T) {
	g := managedPublicGraph(t, "resume-source", p.WithMaxRunning(1))
	key := p.NewKey[p.Stream[int]]("stream")
	var values []int
	managedPublicOK(t, g.Add(p.NewNode("source", "按持久游标续读", nil, p.Provides(key), func(rc *p.RunCtx) error {
		writer, err := p.OpenStream(rc, key, p.WithBuffer(2), p.WithStreamSource("number-log", true))
		if err != nil {
			return err
		}
		position := writer.Position()
		if position.Sequence != 2 || position.Cursor != 2 {
			return fmt.Errorf("lost resume position: %+v", position)
		}
		return writer.SendAt(30, 3)
	})))
	managedPublicOK(t, g.Add(p.NewNode("sink", "跳过已确认前缀，收到尾部与新消息", p.Requires(key), nil, func(rc *p.RunCtx) error {
		stream, err := p.Get(rc, key)
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
			values = append(values, delivery.Value)
			if err := delivery.Ack(); err != nil {
				return err
			}
		}
	})))
	hooks := publicCheckpointStreamHooks()
	open := hooks.StreamHooks.Open
	hooks.StreamHooks.Open = func(ctx context.Context, request p.CheckpointStreamOpen) (p.CheckpointStreamRecord, error) {
		record, err := open(ctx, request)
		if err != nil {
			return record, err
		}
		record.Stream.ID.Generation = 7
		record.Resumed, record.LastSequence, record.AckedThrough, record.Cursor = true, 2, 1, 2
		record.Pending = []p.CheckpointStreamMessage{{Sequence: 2, Value: 20}}
		return record, nil
	}
	var mu sync.Mutex
	var appends []p.CheckpointStreamAppend
	hooks.StreamHooks.Append = func(_ context.Context, record p.CheckpointStreamAppend) error {
		mu.Lock()
		defer mu.Unlock()
		appends = append(appends, record)
		return nil
	}
	managedPublicOK(t, g.Run(managedPublicContext(t), p.Inputs{}, p.WithExecCheckpoint("stable-root", hooks)))
	if !reflect.DeepEqual(values, []int{20, 30}) || len(appends) != 1 || appends[0].Message.Sequence != 3 || appends[0].Cursor != 3 || appends[0].Stream.Generation != 7 {
		t.Fatalf("resume rewound stream: values=%v appends=%+v", values, appends)
	}
}

func TestCheckpointManagedIORejectsOrdinaryRetryFrame(t *testing.T) {
	g := managedPublicGraph(t, "retry-stream")
	key := p.NewKey[p.Stream[int]]("stream")
	managedPublicOK(t, g.Add(p.NewNode("source", "普通 Retry 不能换流", nil, p.Provides(key), func(rc *p.RunCtx) error {
		writer, err := p.OpenStream(rc, key, p.WithStreamSource("source", true))
		if err != nil {
			return err
		}
		return writer.Send(1)
	}, p.Retry(2, 0))))
	managedPublicOK(t, g.Add(p.NewNode("sink", "读取", p.Requires(key), nil, func(rc *p.RunCtx) error {
		stream, err := p.Get(rc, key)
		if err != nil {
			return err
		}
		_, err = stream.Receive(rc)
		return err
	})))
	hooks := publicCheckpointStreamHooks()
	var opens atomic.Int32
	open := hooks.StreamHooks.Open
	hooks.StreamHooks.Open = func(ctx context.Context, request p.CheckpointStreamOpen) (p.CheckpointStreamRecord, error) {
		opens.Add(1)
		return open(ctx, request)
	}
	err := g.Run(managedPublicContext(t), p.Inputs{}, p.WithExecCheckpoint("root", hooks))
	if err == nil || !strings.Contains(err.Error(), "Retry") || opens.Load() != 0 {
		t.Fatalf("ordinary retry published a stream: err=%v opens=%d", err, opens.Load())
	}
}
