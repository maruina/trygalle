package runtime_test

import (
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// newHeartbeatCoordinator builds a coordinator with a tiny heartbeat interval
// for fast, generous-margin timing assertions.
func newHeartbeatCoordinator(t *testing.T, f *fakeClient, n *recordingNotifier) *runtime.Coordinator {
	t.Helper()
	cfg := runtime.DefaultConfig()
	cfg.HeartbeatInterval = 20 * time.Millisecond
	return runtime.NewCoordinator(f, n, cfg, testLogger(t))
}

func countNotices(n *recordingNotifier, kind runtime.NoticeKind, window time.Duration) int {
	deadline := time.After(window)
	count := 0
	for {
		select {
		case notice := <-n.notices:
			if notice.Kind == kind {
				count++
			}
		case <-deadline:
			return count
		}
	}
}

func drainNotices(n *recordingNotifier) {
	for {
		select {
		case <-n.notices:
		case <-n.responses:
		default:
			return
		}
	}
}

func TestHeartbeatsWhileRunning(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	c := newHeartbeatCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	f.activityMu.Lock()
	f.activity = rpc.Activity{Type: "message_update", At: time.Now().Add(-3 * time.Second)}
	f.activityMu.Unlock()

	// Exactly one notice per interval elapse: two ticks arrive within a
	// window of a little over one tick pair, and none follows the first tick
	// inside the same interval.
	deadline := time.After(200 * time.Millisecond)
	var hb runtime.Notice
	for tick := range 2 {
		select {
		case notice := <-n.notices:
			if notice.Kind != runtime.NoticeHeartbeat {
				t.Fatalf("notice kind = %q, want Heartbeat", notice.Kind)
			}
			if tick == 0 {
				hb = notice
			}
		case <-deadline:
			t.Fatalf("only %d heartbeats within 200ms; want 2", tick)
		}
		if tick == 0 {
			if extra := countNotices(n, runtime.NoticeHeartbeat, 5*time.Millisecond); extra != 0 {
				t.Fatalf("%d extra heartbeats right after the first tick; want 0", extra)
			}
		}
	}

	if hb.Elapsed <= 0 {
		t.Errorf("Heartbeat elapsed = %s, want > 0", hb.Elapsed)
	}
	f.activityMu.Lock()
	activity := f.activity
	f.activityMu.Unlock()
	if hb.LastActivity.Type != activity.Type {
		t.Errorf("Heartbeat last activity type = %q, want %q", hb.LastActivity.Type, activity.Type)
	}
	if d := hb.LastActivity.At.Sub(activity.At); d < -time.Second || d > time.Second {
		t.Errorf("Heartbeat last activity time off by %s from the fake's activity", d)
	}
}

func TestHeartbeatStopsAfterSettlement(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("done")
	c := newHeartbeatCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "done" {
		t.Fatalf("response = %q, want %q", got, "done")
	}

	// Ticking stops on the terminal state: drain whatever was queued before
	// settlement, then a window of several intervals produces no heartbeat.
	drainNotices(n)
	if count := countNotices(n, runtime.NoticeHeartbeat, 100*time.Millisecond); count != 0 {
		t.Errorf("%d heartbeats after settlement; want 0", count)
	}
}

func TestHeartbeatBeforeAnyEventCarriesNoActivity(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	c := newHeartbeatCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	// No event was ever observed, so the payload must report none.
	hb := <-n.notices
	if hb.Kind != runtime.NoticeHeartbeat {
		t.Fatalf("notice kind = %q, want Heartbeat", hb.Kind)
	}
	if hb.LastActivity.Type != "" {
		t.Errorf("LastActivity.Type = %q, want empty before any event", hb.LastActivity.Type)
	}
}
