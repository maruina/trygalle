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

// newLiveTimeout bounds every live new-session RPC and wait.
const newLiveTimeout = 60 * time.Second

// liveGetState returns the session Pi currently reports.
func liveGetState(t *testing.T, client *rpc.Client) rpc.State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), newLiveTimeout)
	defer cancel()
	state, err := client.GetState(ctx)
	if err != nil {
		t.Fatalf("get_state: %v", err)
	}
	return state
}

// liveClearQueue returns the steering and follow-up texts Pi currently holds.
func liveClearQueue(t *testing.T, client *rpc.Client) rpc.QueueContents {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), newLiveTimeout)
	defer cancel()
	queue, err := client.ClearQueue(ctx)
	if err != nil {
		t.Fatalf("clear_queue: %v", err)
	}
	return queue
}

// TestLiveNewSessionMidRun pins R13 against the real Pi: a mid-run /new
// supersedes the operation, reports the dropped steering, switches the
// session, and leaves an empty queue; the coordinator serves a fresh prompt
// afterwards with no misattributed trailing outcome (design contract test 4).
//
// Coverage honesty: a trailing old-run settle is timing-dependent (it needs
// the abandoned delay to resolve after the switch), so this test pins the
// deterministic effects. The trailing-event drop itself is pinned by the
// scripted unit test TestNewSessionMidRunSupersedes.
func TestLiveNewSessionMidRun(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.RespondText("answer after new")
	model.Delay(3 * time.Second) // hold the run open past /new
	client := harness.Start(t, bin, harness.WithModel(model))
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), testLogger(t))
	defer c.Close()

	before := liveGetState(t, client)
	if before.SessionID == "" {
		t.Fatal("get_state reports no session id before /new")
	}

	if err := c.HandleUserInput(context.Background(), "slow run for new session"); err != nil {
		t.Fatalf("HandleUserInput(prompt): %v", err)
	}
	if err := c.HandleUserInput(context.Background(), "steering to drop"); err != nil {
		t.Fatalf("HandleUserInput(steer): %v", err)
	}
	n.waitForNotice(t, runtime.NoticeSteerAcknowledged)

	if err := c.HandleUserInput(context.Background(), "/new"); err != nil {
		t.Fatalf("HandleUserInput(/new): %v", err)
	}
	// The new-session report is the superseded run's single terminal outcome:
	// no EmptyResponse or stale response precedes it (waitForNotice fails on
	// either).
	notice := n.waitForNotice(t, runtime.NoticeNewSessionReport)
	if !slices.Contains(notice.Dropped.Steering, "steering to drop") {
		t.Fatalf("NewSessionReport dropped = %+v, want the queued steering", notice.Dropped)
	}

	after := liveGetState(t, client)
	if after.SessionID == "" || after.SessionID == before.SessionID {
		t.Fatalf("session id before %q after %q, want a fresh session", before.SessionID, after.SessionID)
	}
	if again := liveClearQueue(t, client); len(again.Steering) != 0 || len(again.FollowUp) != 0 {
		t.Fatalf("queue after /new = %+v, want empty", again)
	}

	// The coordinator is usable again in the new session: a fresh prompt
	// settles into its own response with no trailing outcome leaking in.
	if err := c.HandleUserInput(context.Background(), "fresh prompt after new"); err != nil {
		t.Fatalf("HandleUserInput after /new: %v", err)
	}
	select {
	case text := <-n.responses:
		if text != "answer after new" {
			t.Errorf("post-/new response = %q, want the scripted answer", text)
		}
	case extra := <-n.notices:
		t.Fatalf("unexpected notice %q instead of the post-/new response", extra.Kind)
	case <-time.After(newLiveTimeout):
		t.Fatal("no post-/new response within 60s")
	}
}

// TestLiveNewSessionCancelledKeepsSession pins the cancellation path: with an
// extension whose session_before_switch handler cancels, /new reports the
// cancellation and the session id is unchanged (design contract test 5).
func TestLiveNewSessionCancelledKeepsSession(t *testing.T) {
	bin := harness.Pi(t)
	t.Setenv("MOCK_CANCEL_NEW_SESSION", "1")
	model := harness.StartMockModel(t)
	client := harness.Start(t, bin, harness.WithModel(model), harness.WithExtension("../harness/testdata/agent/extension.js"))
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), testLogger(t))
	defer c.Close()

	before := liveGetState(t, client)
	if before.SessionID == "" {
		t.Fatal("get_state reports no session id before /new")
	}

	if err := c.HandleUserInput(context.Background(), "/new"); err != nil {
		t.Fatalf("HandleUserInput(/new): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeNewSessionReport)
	if notice.Text != "session switch cancelled" {
		t.Fatalf("cancelled NewSessionReport text = %q, want the cancellation marker", notice.Text)
	}

	after := liveGetState(t, client)
	if after.SessionID != before.SessionID {
		t.Fatalf("session id changed across a cancelled /new: before %q after %q", before.SessionID, after.SessionID)
	}
}
