package runtime_test

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// logCapture records every slog record so the extension UI policy tests can
// assert exactly which fields the coordinator logs: the method only, never
// dialog content.
type logCapture struct {
	mu      sync.Mutex
	records []logRecord
}

type logRecord struct {
	msg   string
	attrs map[string]string
}

func (h *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *logCapture) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{msg: r.Message, attrs: make(map[string]string)}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, rec)
	return nil
}

func (h *logCapture) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *logCapture) WithGroup(string) slog.Handler { return h }

func (h *logCapture) logger() *slog.Logger { return slog.New(h) }

// waitFor waits for the first record with msg and consumes it.
func (h *logCapture) waitFor(t *testing.T, msg string) logRecord {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		h.mu.Lock()
		for i, rec := range h.records {
			if rec.msg == msg {
				h.records = slices.Delete(h.records, i, i+1)
				h.mu.Unlock()
				return rec
			}
		}
		h.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("no log record %q within 2s", msg)
			return logRecord{}
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// waitForCall polls the fake client's recorded calls until want appears.
func waitForCall(t *testing.T, f *fakeClient, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !slices.Contains(f.getCalls(), want) {
		select {
		case <-deadline:
			t.Fatalf("call %q not recorded within 2s; calls = %v", want, f.getCalls())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestUIDialogMethodsAnsweredCancelled pins the dialog half of R14: every
// blocking dialog method is answered immediately with cancelled: true, and the
// log record carries the method only.
func TestUIDialogMethodsAnsweredCancelled(t *testing.T) {
	for _, method := range []string{"select", "confirm", "input", "editor"} {
		t.Run(method, func(t *testing.T) {
			f := newFakeClient()
			n := newRecordingNotifier()
			capture := &logCapture{}
			c := runtime.NewCoordinator(f, n, runtime.DefaultConfig(), capture.logger())
			defer c.Close()

			const id = "dialog-1"
			f.ui <- rpc.UIRequest{Type: rpc.UIRequestType, ID: id, Method: method}

			rec := capture.waitFor(t, "cancelling extension dialog")
			if got := rec.attrs["method"]; got != method {
				t.Errorf("logged method = %q, want %q", got, method)
			}
			if len(rec.attrs) != 1 {
				t.Errorf("dialog log carries fields beyond the method: %v", rec.attrs)
			}

			// The PiClient seam answers with cancelled: true by contract
			// (rpc.CancelledUIResponse), so the recorded id is the proof.
			waitForCall(t, f, "answer_ui_dialog:"+id)

			select {
			case text := <-n.responses:
				t.Errorf("dialog produced a response %q", text)
			case notice := <-n.notices:
				t.Errorf("dialog produced notice %q", notice.Kind)
			default:
			}
		})
	}
}

// TestUIFireAndForgetLoggedWithoutDelivery pins the fire-and-forget half of
// R14: notify and the TUI setters are logged as metadata, never answered, and
// never delivered to the user.
func TestUIFireAndForgetLoggedWithoutDelivery(t *testing.T) {
	for _, method := range []string{"notify", "setStatus", "setWidget", "setTitle", "set_editor_text"} {
		t.Run(method, func(t *testing.T) {
			f := newFakeClient()
			n := newRecordingNotifier()
			capture := &logCapture{}
			c := runtime.NewCoordinator(f, n, runtime.DefaultConfig(), capture.logger())
			defer c.Close()

			f.ui <- rpc.UIRequest{Type: rpc.UIRequestType, ID: "ui-1", Method: method}

			rec := capture.waitFor(t, "extension ui notification")
			if got := rec.attrs["method"]; got != method {
				t.Errorf("logged method = %q, want %q", got, method)
			}
			if len(rec.attrs) != 1 {
				t.Errorf("fire-and-forget log carries fields beyond the method: %v", rec.attrs)
			}

			// The log is written inside the UI-request branch, so anything the
			// loop had recorded by now is final for this request.
			calls := f.getCalls()
			if slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "answer_ui_dialog:") }) {
				t.Errorf("fire-and-forget method was answered; calls = %v", calls)
			}

			select {
			case text := <-n.responses:
				t.Errorf("fire-and-forget method produced a response %q", text)
			case notice := <-n.notices:
				t.Errorf("fire-and-forget method produced notice %q", notice.Kind)
			default:
			}
		})
	}
}
