package runtime_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// shutdownConfig bounds the shutdown sequence tightly enough for a unit test:
// the fake child exits as soon as its stdin closes.
func shutdownConfig() runtime.Config {
	cfg := runtime.DefaultConfig()
	cfg.ShutdownTimeout = 5 * time.Second
	return cfg
}

// TestShutdownAbortsRunningOperationInOrder pins the R17 sequence: clear_queue
// before abort before the stdin close, with the child exiting before the
// deadline so no kill is needed.
func TestShutdownAbortsRunningOperationInOrder(t *testing.T) {
	f := newFakeClient()
	f.dieOnCloseStdin = true
	c := runtime.NewCoordinator(f, newRecordingNotifier(), shutdownConfig(), testLogger(t))
	startRunningOperation(t, f, c)

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	calls := f.getCalls()
	queue, abort, stdin := slices.Index(calls, "clear_queue"), slices.Index(calls, "abort"), slices.Index(calls, "close_stdin")
	if queue < 0 || abort < 0 || stdin < 0 || !(queue < abort && abort < stdin) {
		t.Fatalf("shutdown calls = %v, want clear_queue before abort before close_stdin", calls)
	}
	if slices.Contains(calls, "kill") {
		t.Errorf("shutdown killed a child that exited within the deadline; calls = %v", calls)
	}
	if !slices.Contains(calls, "wait") {
		t.Errorf("shutdown did not wait for pi to exit; calls = %v", calls)
	}
}

// TestShutdownWhileIdleSkipsTheAbortSequence pins the idle path: no operation
// is aborted, and only the stdin close requests the exit.
func TestShutdownWhileIdleSkipsTheAbortSequence(t *testing.T) {
	f := newFakeClient()
	f.dieOnCloseStdin = true
	c := runtime.NewCoordinator(f, newRecordingNotifier(), shutdownConfig(), testLogger(t))

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	calls := f.getCalls()
	if slices.Contains(calls, "clear_queue") || slices.Contains(calls, "abort") {
		t.Errorf("idle shutdown sent abort commands; calls = %v", calls)
	}
	if !slices.Contains(calls, "close_stdin") {
		t.Errorf("idle shutdown did not close stdin; calls = %v", calls)
	}
}

// TestShutdownDeadlineKillsAChildThatIgnoresStdinClose pins the kill fallback:
// the fake never exits, so the sequence must return at the deadline instead of
// blocking, and the shutdown must still report success (exit 0).
func TestShutdownDeadlineKillsAChildThatIgnoresStdinClose(t *testing.T) {
	f := newFakeClient() // Wait blocks forever: this child ignores the close
	cfg := shutdownConfig()
	cfg.ShutdownTimeout = 50 * time.Millisecond
	c := runtime.NewCoordinator(f, newRecordingNotifier(), cfg, testLogger(t))

	start := time.Now()
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Shutdown blocked for %s past its 50ms deadline", elapsed)
	}
	if !slices.Contains(f.getCalls(), "kill") {
		t.Errorf("shutdown did not kill the child that missed the deadline; calls = %v", f.getCalls())
	}
}

// TestShutdownCleanExitIsNotFatal pins that a clean pi exit after the shutdown
// request is the expected end of the process: the coordinator logs it and
// never reports a fatal error. The child exits while the abort sequence runs,
// which is the order that reaches the loop's shutdown branch.
func TestShutdownCleanExitIsNotFatal(t *testing.T) {
	f := newFakeClient()
	capture := &logCapture{}
	cfg := shutdownConfig()
	c := runtime.NewCoordinator(f, newRecordingNotifier(), cfg, capture.logger())
	startRunningOperation(t, f, c)
	f.onClearQueue = f.die

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	capture.waitFor(t, "pi exited after the shutdown request")
	if slices.Contains(capture.messages(), "coordinator fatal") {
		t.Errorf("clean exit after the shutdown request was reported as fatal; logs = %v", capture.messages())
	}
}

// TestShutdownSettleDuringShutdownIsNotAttributed pins that a run which
// settles while the shutdown sequence aborts it produces no terminal outcome:
// the restart notice is the operation's outcome, and no final-text query runs
// against a dying child. The unbuffered event channel proves the loop received
// the settle before Shutdown returns.
func TestShutdownSettleDuringShutdownIsNotAttributed(t *testing.T) {
	f := newFakeClient()
	f.useUnbufferedEvents()
	f.dieOnCloseStdin = true
	capture := &logCapture{}
	c := runtime.NewCoordinator(f, newRecordingNotifier(), shutdownConfig(), capture.logger())
	startRunningOperation(t, f, c)
	f.onClearQueue = func() { f.events <- rpc.Event{Type: "agent_settled"} }

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	capture.waitFor(t, "operation settled during shutdown")
	if calls := f.getCalls(); slices.Contains(calls, "get_last_assistant_text") {
		t.Errorf("shutdown queried the final text of an aborted run; calls = %v", calls)
	}
}
