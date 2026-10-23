package runtime_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/harness"
	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// abortLiveTimeout bounds every live abort RPC and event wait.
const abortLiveTimeout = 60 * time.Second

// liveDisposition sends one prompt{steer} through the raw client and returns
// Pi's disposition for it.
func liveDisposition(t *testing.T, client *rpc.Client, message string) rpc.Disposition {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), abortLiveTimeout)
	defer cancel()
	resp, err := client.Send(ctx, rpc.PromptCommand(message, "steer"))
	if err != nil {
		t.Fatalf("prompt %q: %v", message, err)
	}
	if !resp.Success {
		t.Fatalf("prompt %q failed: %s", message, resp.Error)
	}
	var data rpc.PromptData
	if err := resp.Decode(&data); err != nil {
		t.Fatalf("decode prompt disposition: %v", err)
	}
	return data.Disposition
}

// liveCommand sends one raw command and returns its decoded data.
func liveCommand(t *testing.T, client *rpc.Client, cmd rpc.Command, data any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), abortLiveTimeout)
	defer cancel()
	resp, err := client.Send(ctx, cmd)
	if err != nil {
		t.Fatalf("%s: %v", cmd.Type, err)
	}
	if !resp.Success {
		t.Fatalf("%s failed: %s", cmd.Type, resp.Error)
	}
	if err := resp.Decode(data); err != nil {
		t.Fatalf("decode %s data: %v", cmd.Type, err)
	}
}

// liveEventsUntil collects session events until cond holds or the timeout
// elapses, returning everything observed.
func liveEventsUntil(t *testing.T, client *rpc.Client, cond func(rpc.Event) bool) []rpc.Event {
	t.Helper()
	var events []rpc.Event
	deadline := time.After(abortLiveTimeout)
	for {
		select {
		case event := <-client.Events():
			events = append(events, event)
			if cond(event) {
				return events
			}
		case <-deadline:
			t.Fatalf("timeout waiting for session event; saw %d events", len(events))
			return events
		}
	}
}

func liveAgentStart(events []rpc.Event) bool {
	return slices.ContainsFunc(events, func(e rpc.Event) bool {
		return e.Type == rpc.EventAgentStart
	})
}

// TestLiveAbortAloneLeavesQueueRunning pins that abort without a preceding
// clear_queue does not drain queued steering: the queued message still runs
// to its own agent_start after the abort (design contract test 1, second
// case). This is the Pi behavior that makes clear_queue-before-abort the
// Esc-parity sequence.
func TestLiveAbortAloneLeavesQueueRunning(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.Delay(2 * time.Second) // hold run 1 open past the queued steer
	client := harness.Start(t, bin, harness.WithModel(model))
	defer client.CloseStdin()

	if d := liveDisposition(t, client, "start a slow run"); d != rpc.DispositionStarted {
		t.Fatalf("idle prompt disposition = %q, want started", d)
	}
	liveEventsUntil(t, client, func(e rpc.Event) bool { return e.Type == rpc.EventAgentStart })
	if d := liveDisposition(t, client, "queued steering"); d != rpc.DispositionQueued {
		t.Fatalf("mid-run prompt disposition = %q, want queued", d)
	}

	// Abort alone: no clear_queue first.
	ctx, cancel := context.WithTimeout(context.Background(), abortLiveTimeout)
	defer cancel()
	resp, err := client.Send(ctx, rpc.AbortCommand())
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if !resp.Success {
		t.Fatalf("abort failed: %s", resp.Error)
	}

	// The queued message still runs: a new agent_start follows the abort,
	// then the run settles.
	events := liveEventsUntil(t, client, func(e rpc.Event) bool { return e.Type == rpc.EventAgentSettled })
	if !liveAgentStart(events) {
		t.Fatalf("no agent_start after abort alone; the queued message did not run (saw %d events)", len(events))
	}
}

// TestLiveClearQueueBeforeAbortDrainsQueue pins the Esc-parity sequence at
// the protocol level: clear_queue reports the queued steering, and after
// clear_queue + abort nothing remains queued (design contract test 3).
func TestLiveClearQueueBeforeAbortDrainsQueue(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.Delay(2 * time.Second) // hold the run open past the queued steer
	client := harness.Start(t, bin, harness.WithModel(model))
	defer client.CloseStdin()

	if d := liveDisposition(t, client, "start another slow run"); d != rpc.DispositionStarted {
		t.Fatalf("idle prompt disposition = %q, want started", d)
	}
	if d := liveDisposition(t, client, "steering to drop"); d != rpc.DispositionQueued {
		t.Fatalf("mid-run prompt disposition = %q, want queued", d)
	}

	var dropped rpc.QueueContents
	liveCommand(t, client, rpc.ClearQueueCommand(), &dropped)
	if !slices.Contains(dropped.Steering, "steering to drop") {
		t.Fatalf("clear_queue steering = %q, want the queued message", dropped.Steering)
	}

	ctx, cancel := context.WithTimeout(context.Background(), abortLiveTimeout)
	defer cancel()
	resp, err := client.Send(ctx, rpc.AbortCommand())
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if !resp.Success {
		t.Fatalf("abort failed: %s", resp.Error)
	}

	var again rpc.QueueContents
	liveCommand(t, client, rpc.ClearQueueCommand(), &again)
	if len(again.Steering) != 0 || len(again.FollowUp) != 0 {
		t.Fatalf("queue after clear_queue + abort = %+v, want empty", again)
	}
}

// TestLiveCoordinatorAbortMidRun pins R12 through the coordinator: a queued
// steering message is reported as dropped, the aborted run produces no
// further outcome, and the coordinator serves a fresh prompt afterwards.
func TestLiveCoordinatorAbortMidRun(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.Delay(3 * time.Second) // hold the run open past /abort
	client := harness.Start(t, bin, harness.WithModel(model))
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), testLogger(t))
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "slow run for abort"); err != nil {
		t.Fatalf("HandleUserInput(prompt): %v", err)
	}
	if err := c.HandleUserInput(context.Background(), "steered follow-up"); err != nil {
		t.Fatalf("HandleUserInput(steer): %v", err)
	}
	n.waitForNotice(t, runtime.NoticeSteerAcknowledged)

	if err := c.HandleUserInput(context.Background(), "/abort"); err != nil {
		t.Fatalf("HandleUserInput(/abort): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeAbortReport)
	if !slices.Contains(notice.Dropped.Steering, "steered follow-up") {
		t.Fatalf("AbortReport dropped = %+v, want the queued steering", notice.Dropped)
	}

	// The coordinator is idle and usable again: a fresh prompt settles into
	// its own response (the mock delay bounds this under the timeout).
	if err := c.HandleUserInput(context.Background(), "fresh prompt after abort"); err != nil {
		t.Fatalf("HandleUserInput after /abort: %v", err)
	}
	select {
	case text := <-n.responses:
		if text == "" {
			t.Error("post-abort response is empty")
		}
	case extra := <-n.notices:
		t.Fatalf("unexpected notice %q instead of the post-abort response", extra.Kind)
	case <-time.After(abortLiveTimeout):
		t.Fatal("no post-abort response within 60s")
	}
}
