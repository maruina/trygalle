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

// liveAbort sends one raw abort command and requires success.
func liveAbort(t *testing.T, client *rpc.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), abortLiveTimeout)
	defer cancel()
	resp, err := client.Send(ctx, rpc.AbortCommand())
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if !resp.Success {
		t.Fatalf("abort failed: %s", resp.Error)
	}
}

// TestLiveAbortAloneDoesNotRunQueuedSteering pins why /abort must clear the
// queue first: agent_start fires once per agent-loop invocation, so a queued
// steering message can never start a run after an abort. It either joins the
// aborted run's final turn and is aborted with it, or stays queued for the
// next prompt; either way it produces no answer. This is the Pi behavior that
// makes clear_queue-before-abort the Esc-parity sequence (design contract
// tests 1 and 3).
func TestLiveAbortAloneDoesNotRunQueuedSteering(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.RespondText("answer to the first run")
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
	liveAbort(t, client)

	// The aborted run settles without another agent_start: the queued message
	// never becomes its own run, and the session keeps no assistant answer.
	events := liveEventsUntil(t, client, func(e rpc.Event) bool { return e.Type == rpc.EventAgentSettled })
	if liveAgentStart(events) {
		t.Fatalf("agent_start after abort alone; the queued message became a run (saw %d events)", len(events))
	}
	ctx, cancel := context.WithTimeout(context.Background(), abortLiveTimeout)
	defer cancel()
	last, err := client.GetLastAssistantText(ctx)
	if err != nil {
		t.Fatalf("get_last_assistant_text: %v", err)
	}
	if last != nil {
		t.Fatalf("last assistant text = %q after abort alone, want no answer for the queued message", *last)
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

	liveAbort(t, client)

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
	model.RespondText("answer after abort")
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
	// The abort report is the aborted run's single terminal outcome: no
	// EmptyResponse or stale response precedes it (waitForNotice fails on
	// either), and it does not claim the session was idle.
	notice := n.waitForNotice(t, runtime.NoticeAbortReport)
	if notice.Text != "" {
		t.Errorf("mid-run AbortReport text = %q, want empty (an operation was aborted)", notice.Text)
	}
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
		if text != "answer after abort" {
			t.Errorf("post-abort response = %q, want the scripted answer", text)
		}
	case extra := <-n.notices:
		t.Fatalf("unexpected notice %q instead of the post-abort response", extra.Kind)
	case <-time.After(abortLiveTimeout):
		t.Fatal("no post-abort response within 60s")
	}
}
