package runtime_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/harness"
	"github.com/maruina/trygalle/internal/runtime"
)

// shutdownLiveDeadline is the live shutdown deadline: generous enough for a
// loaded machine, small enough that a hung child fails the test.
const shutdownLiveDeadline = 30 * time.Second

// assertLogOrder requires every message to appear in the captured log in the
// given order. The last asserted step is written before Shutdown returns, so
// the snapshot is complete.
func assertLogOrder(t *testing.T, capture *logCapture, msgs ...string) {
	t.Helper()
	logs := capture.messages()
	prev := -1
	for _, msg := range msgs {
		idx := slices.Index(logs, msg)
		if idx < 0 {
			t.Fatalf("log record %q missing; logs = %v", msg, logs)
		}
		if idx <= prev {
			t.Fatalf("log record %q out of order; logs = %v", msg, logs)
		}
		prev = idx
	}
}

// TestLiveShutdownMidRunAbortsInOrder pins R17 against the real Pi: SIGTERM
// during an active run runs clear_queue, then abort, then the stdin close, pi
// exits within the deadline, and no outcome of the aborted run reaches the
// user.
func TestLiveShutdownMidRunAbortsInOrder(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.RespondText("never delivered")
	model.Delay(5 * time.Second) // hold the run open past the shutdown
	client := harness.Start(t, bin, harness.WithModel(model))
	defer client.CloseStdin()

	capture := &logCapture{}
	n := newRecordingNotifier()
	cfg := runtime.DefaultConfig()
	cfg.ShutdownTimeout = shutdownLiveDeadline
	c := runtime.NewCoordinator(client, n, cfg, capture.logger())
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "start a slow run"); err != nil {
		t.Fatalf("HandleUserInput(prompt): %v", err)
	}

	start := time.Now()
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	elapsed := time.Since(start)

	assertLogOrder(t, capture,
		"shutdown requested",
		"shutting down during an operation",
		"shutdown aborted the active operation",
		"closed pi stdin",
		"pi exited after the shutdown request",
	)
	if elapsed >= shutdownLiveDeadline {
		t.Errorf("shutdown took %s, want it under the %s deadline", elapsed, shutdownLiveDeadline)
	}
	if err := client.Wait(); err != nil {
		t.Errorf("pi exit after shutdown = %v, want the orderly exit", err)
	}

	// The best-effort restart notice is the aborted operation's only outcome.
	n.waitForNotice(t, runtime.NoticeRestartNotice)
	select {
	case text := <-n.responses:
		t.Errorf("aborted run delivered a response %q", text)
	case notice := <-n.notices:
		t.Errorf("unexpected notice %q after the restart notice", notice.Kind)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestLiveShutdownWhileIdle pins the idle half of R17: no abort sequence runs,
// the stdin close alone gives pi an orderly exit, and shutdown stays inside
// the deadline.
func TestLiveShutdownWhileIdle(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	client := harness.Start(t, bin, harness.WithModel(model))
	defer client.CloseStdin()

	capture := &logCapture{}
	cfg := runtime.DefaultConfig()
	cfg.ShutdownTimeout = shutdownLiveDeadline
	c := runtime.NewCoordinator(client, newRecordingNotifier(), cfg, capture.logger())
	defer c.Close()

	start := time.Now()
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	elapsed := time.Since(start)

	assertLogOrder(t, capture,
		"shutdown requested",
		"closed pi stdin",
		"pi exited after the shutdown request",
	)
	if slices.Contains(capture.messages(), "shutting down during an operation") {
		t.Errorf("idle shutdown ran the abort sequence; logs = %v", capture.messages())
	}
	if elapsed >= shutdownLiveDeadline {
		t.Errorf("shutdown took %s, want it under the %s deadline", elapsed, shutdownLiveDeadline)
	}
	if err := client.Wait(); err != nil {
		t.Errorf("pi exit after shutdown = %v, want the orderly exit", err)
	}
}
