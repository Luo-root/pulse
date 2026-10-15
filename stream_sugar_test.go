package pulse

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStreamSugarMessageOptionCopies(t *testing.T) {
	caller := []MessageOption{WithMessageRetry(2, 0)}
	option := WithMessageHandling(caller...)
	caller[0] = WithMessageRetry(99, 0)
	base := streamConfig{messageOptions: make([]MessageOption, 1, 8)}
	base.messageOptions[0] = WithMessageRetry(3, 0)
	first, second := base, base
	option(&first)
	option(&second)
	first.messageOptions[0], first.messageOptions[1] = WithMessageRetry(99, 0), WithMessageRetry(99, 0)
	for _, cfg := range []streamConfig{second, newStreamConfig([]StreamOption{WithMessageHandling(), option})} {
		var message messageConfig
		for _, apply := range cfg.messageOptions {
			apply(&message)
		}
		if message.attempts != 2 {
			t.Fatalf("message option applications share caller storage: attempts=%d", message.attempts)
		}
	}
	open := newStreamConfig([]StreamOption{streamOpenOption(streamConfig{
		buf: 7, checkpointSource: CheckpointStreamSource{ID: "source", Resumable: true},
		aspects: []Aspect{Retry(2, 0)}, messageOptions: []MessageOption{WithMessageRetry(2, 0)},
	})})
	if len(open.aspects) != 0 || len(open.messageOptions) != 0 || open.buf != 7 || open.checkpointSource.ID != "source" || !open.checkpointSource.Resumable {
		t.Fatalf("OpenStream helper changed endpoint configuration: %+v", open)
	}
}

func TestStreamSugarCapturedConfigurationDoesNotChangeNode(t *testing.T) {
	g, err := New("sugar-captured-config", "", WithMaxRunning(1))
	if err != nil {
		t.Fatal(err)
	}
	input, output := NewKey[Stream[int]]("input"), NewKey[Stream[int]]("output")
	var saved []*streamConfig
	capture := func(cfg *streamConfig) { saved = append(saved, cfg) }
	if err := ProduceStream(g, "source", "", input, func(_ *RunCtx, writer *StreamWriter[int]) error { return writer.Send(7) }, WithStreamAspects(), capture); err != nil {
		t.Fatal(err)
	}
	option := WithMessageHandling(WithMessageRetry(2, 0))
	if err := TeeStream(g, "tee", "", input, Keys(output), option, capture); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := ConsumeStream(g, "sink", "", output, func(*RunCtx, int) error {
		calls++
		if calls == 1 {
			return errors.New("retry once")
		}
		return nil
	}, option, capture); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range saved {
		cfg.buf = 99
		for i := range cfg.messageOptions {
			cfg.messageOptions[i] = WithMessageRetry(1, 0)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := g.Run(ctx, Inputs{}); err != nil || calls != 2 {
		t.Fatalf("assembled configuration changed: err=%v calls=%d", err, calls)
	}
}
