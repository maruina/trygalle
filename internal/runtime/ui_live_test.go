package runtime_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/harness"
	"github.com/maruina/trygalle/internal/runtime"
)

// testExtension is the synthetic pi extension the harness installs. It defines
// /mock-dialog and /mock-notify (contract R14).
var testExtension = filepath.Join("..", "harness", "testdata", "agent", "extension.js")

// awaitRunResponse consumes notices until the run's final response arrives. A
// /mock-dialog handled mid-run legitimately produces its own outcome notice.
func awaitRunResponse(t *testing.T, n *recordingNotifier) string {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case text := <-n.responses:
			return text
		case notice := <-n.notices:
			if notice.Kind != runtime.NoticeExtensionHandled {
				t.Fatalf("unexpected notice %q before the run response", notice.Kind)
			}
		case <-deadline:
			t.Fatal("run did not settle within 60s")
			return ""
		}
	}
}

// TestLiveExtensionDialogCancelled pins R14 (design contract test 10) against
// the real Pi: a dialog opened by an extension command mid-run is answered
// cancelled immediately, so the handler resumes well inside its 60 s dialog
// timeout, and the active run still settles without hanging.
func TestLiveExtensionDialogCancelled(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.Delay(5 * time.Second) // hold the run open across the dialog
	client := harness.Start(t, bin, harness.WithModel(model), harness.WithExtension(testExtension))
	capture := &logCapture{}
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), capture.logger())
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "slow run for a dialog"); err != nil {
		t.Fatalf("HandleUserInput(prompt): %v", err)
	}

	// The dialog only blocks for 60 s if it is never answered, so a "handled"
	// response inside this bound proves the extension received the
	// cancellation and resumed.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.HandleUserInput(ctx, "/mock-dialog"); err != nil {
		t.Fatalf("HandleUserInput(/mock-dialog) = %v, want the cancelled dialog to resume the handler", err)
	}

	rec := capture.waitFor(t, "cancelling extension dialog")
	if got := rec.attrs["method"]; got != "select" {
		t.Errorf("logged dialog method = %q, want select", got)
	}
	if len(rec.attrs) != 1 {
		t.Errorf("dialog log carries fields beyond the method: %v", rec.attrs)
	}

	if text := awaitRunResponse(t, n); text == "" {
		t.Error("run response is empty after the cancelled dialog")
	}
}

// TestLiveExtensionNotifyLoggedOnly pins the fire-and-forget half of R14: a
// notify request is logged with its method and produces no Telegram-style
// delivery of the notification text.
func TestLiveExtensionNotifyLoggedOnly(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	client := harness.Start(t, bin, harness.WithModel(model), harness.WithExtension(testExtension))
	capture := &logCapture{}
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), capture.logger())
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "/mock-notify"); err != nil {
		t.Fatalf("HandleUserInput(/mock-notify): %v", err)
	}

	rec := capture.waitFor(t, "extension ui notification")
	if got := rec.attrs["method"]; got != "notify" {
		t.Errorf("logged method = %q, want notify", got)
	}
	if len(rec.attrs) != 1 {
		t.Errorf("fire-and-forget log carries fields beyond the method: %v", rec.attrs)
	}

	// /mock-notify starts no run, so its only outcome is the handled-no-run
	// notice; the notification body is never delivered.
	select {
	case text := <-n.responses:
		t.Fatalf("notification delivered a response %q", text)
	case notice := <-n.notices:
		if notice.Kind != runtime.NoticeEmptyResponse {
			t.Fatalf("unexpected notice %q after /mock-notify", notice.Kind)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no outcome for /mock-notify within 30s")
	}
}
