package runtime

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

type bufferedEventClient struct {
	PiClient
	events chan Event
}

func (c bufferedEventClient) Events() <-chan Event { return c.events }

type orderNotifier struct{ notices []Notice }

func (*orderNotifier) Response(context.Context, string) {}
func (n *orderNotifier) Notice(_ context.Context, notice Notice) {
	n.notices = append(n.notices, notice)
}

func TestHandledResultBeforeBufferedStartKeepsAttribution(t *testing.T) {
	events := make(chan Event, 1)
	events <- Event{Type: eventAgentStart}
	pending := &operation{}
	n := &orderNotifier{}
	c := &Coordinator{
		client:   bufferedEventClient{events: events},
		notifier: n,
		cfg:      DefaultConfig(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending:  pending,
	}
	defer c.stopHeartbeat()
	ack := make(chan error, 1)
	c.handleResult(coordCommand{op: pending, disposition: DispositionHandled, ack: ack})
	if err := <-ack; err != nil {
		t.Fatal(err)
	}
	if c.op != pending || !pending.hasRun {
		t.Fatal("buffered extension run lost its operation")
	}
	if c.pending != nil || len(n.notices) != 0 {
		t.Fatalf("pending = %v, notices = %v", c.pending, n.notices)
	}
}
