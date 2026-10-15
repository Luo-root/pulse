package pulse_test

import (
	"strings"
	"testing"

	p "github.com/Luo-root/pulse"
)

func TestHostStreamDefinitionCanBeValidatedBeforeSubscription(t *testing.T) {
	g, key, ctx := hostStreamGraph(t)
	streamAspectsMust(t, g.Add(p.NewNode("source", "", nil, p.Provides(key), func(rc *p.RunCtx) error {
		writer, err := p.OpenStream(rc, key)
		if err != nil {
			return err
		}
		return writer.Send(42)
	})))
	streamAspectsMust(t, g.Validate())
	execution, err := g.NewExecution(ctx, p.Inputs{})
	streamAspectsMust(t, err)
	defer execution.Cancel()
	if err := execution.Start(); err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
		t.Fatalf("declared outlet silently subscribed a reader: %v", err)
	}
	subscription, err := p.SubscribeStream(execution, key)
	streamAspectsMust(t, err)
	streamAspectsMust(t, execution.Start())
	message, err := subscription.Receive(ctx)
	streamAspectsMust(t, err)
	if message.Value != 42 {
		t.Fatalf("message=%d", message.Value)
	}
	streamAspectsMust(t, message.Ack())
	streamAspectsMust(t, execution.Wait())
}
