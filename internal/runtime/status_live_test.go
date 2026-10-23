package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/harness"
	"github.com/maruina/trygalle/internal/runtime"
)

// TestLiveStatusDuringRun pins R11 against the real Pi: a /status query
// mid-run reports the session with IsStreaming true and never disturbs the
// active run, which still settles into its own response afterwards.
func TestLiveStatusDuringRun(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.Delay(3 * time.Second) // hold the run open past the /status query
	client := harness.Start(t, bin, harness.WithModel(model))
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), testLogger(t))
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "report slowly"); err != nil {
		t.Fatalf("HandleUserInput(prompt): %v", err)
	}
	if err := c.HandleUserInput(context.Background(), "/status"); err != nil {
		t.Fatalf("HandleUserInput(/status): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeStatus)
	if notice.State == nil {
		t.Fatal("Status notice carries no state")
	}
	if notice.State.SessionID == "" {
		t.Error("Status state has no session id")
	}
	if !notice.State.IsStreaming {
		t.Errorf("Status IsStreaming = false mid-run, want true")
	}

	// The active run still settles into its own response afterwards.
	select {
	case text := <-n.responses:
		if text == "" {
			t.Error("settlement response is empty")
		}
	case extra := <-n.notices:
		t.Fatalf("unexpected notice %q instead of the run response", extra.Kind)
	case <-time.After(60 * time.Second):
		t.Fatal("no settlement response within 60s")
	}
}
